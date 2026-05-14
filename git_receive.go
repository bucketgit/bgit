package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

type receivePackCommand struct {
	old  string
	new  string
	ref  string
	caps map[string]bool
}

type receivePackRequest struct {
	commands    []receivePackCommand
	caps        map[string]bool
	pushOptions []string
	pack        io.Reader
}

func readReceivePackRequest(stdin io.Reader) (receivePackRequest, error) {
	r := bufio.NewReader(stdin)
	var req receivePackRequest
	for {
		line, err := readPktLine(r)
		if err != nil {
			return req, err
		}
		switch line.kind {
		case pktLineFlush:
			if req.caps["push-options"] {
				options, err := readReceivePackPushOptions(r)
				if err != nil {
					return req, err
				}
				req.pushOptions = options
			}
			req.pack = r
			return req, nil
		case pktLineData:
		default:
			continue
		}
		cmd, caps, err := parseReceivePackCommandLine(string(line.data), len(req.commands) == 0)
		if err != nil {
			return req, err
		}
		req.commands = append(req.commands, cmd)
		if caps != nil {
			req.caps = caps
		}
	}
}

func readReceivePackPushOptions(r *bufio.Reader) ([]string, error) {
	var options []string
	for {
		line, err := readPktLine(r)
		if err != nil {
			return nil, err
		}
		switch line.kind {
		case pktLineFlush:
			return options, nil
		case pktLineData:
			options = append(options, strings.TrimRight(string(line.data), "\n"))
		default:
			return nil, fmt.Errorf("invalid push-options pkt-line")
		}
	}
}

func parseReceivePackCommandLine(line string, first bool) (receivePackCommand, map[string]bool, error) {
	line = strings.TrimRight(line, "\n")
	capText := ""
	if first {
		before, after, ok := strings.Cut(line, "\x00")
		if ok {
			line = before
			capText = after
		}
	}
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return receivePackCommand{}, nil, fmt.Errorf("invalid receive-pack command %q", line)
	}
	if !isHexHash(fields[0]) || !isHexHash(fields[1]) {
		return receivePackCommand{}, nil, fmt.Errorf("invalid receive-pack object id in %q", line)
	}
	if !strings.HasPrefix(fields[2], "refs/") {
		return receivePackCommand{}, nil, fmt.Errorf("invalid receive-pack ref %q", fields[2])
	}
	cmd := receivePackCommand{old: fields[0], new: fields[1], ref: fields[2]}
	caps := map[string]bool{}
	if capText != "" {
		for _, cap := range strings.Fields(capText) {
			caps[cap] = true
			if name, _, ok := strings.Cut(cap, "="); ok {
				caps[name] = true
			}
		}
		cmd.caps = caps
	}
	return cmd, caps, nil
}

func zeroObjectID() string {
	return "0000000000000000000000000000000000000000"
}

func (c receivePackCommand) action() string {
	switch {
	case c.old == zeroObjectID() && c.new != zeroObjectID():
		return "create"
	case c.old != zeroObjectID() && c.new == zeroObjectID():
		return "delete"
	case c.old != zeroObjectID() && c.new != zeroObjectID():
		return "update"
	default:
		return "noop"
	}
}

func writeReceivePackReportStatus(w io.Writer, commands []receivePackCommand, unpackErr error, commandErrs map[string]error) error {
	if unpackErr != nil {
		if err := writePktLineString(w, "unpack "+unpackErr.Error()+"\n"); err != nil {
			return err
		}
	} else if err := writePktLineString(w, "unpack ok\n"); err != nil {
		return err
	}
	for _, cmd := range commands {
		if err := commandErrs[cmd.ref]; err != nil {
			if err := writePktLineString(w, "ng "+cmd.ref+" "+err.Error()+"\n"); err != nil {
				return err
			}
			continue
		}
		if err := writePktLineString(w, "ok "+cmd.ref+"\n"); err != nil {
			return err
		}
	}
	return writePktFlush(w)
}

func serveReceivePack(ctx context.Context, repo *nativeGitRepo, stdin io.Reader, stdout io.Writer) error {
	req, err := readReceivePackRequest(stdin)
	if err != nil {
		return err
	}
	if len(req.commands) == 0 {
		return nil
	}
	store, ok := repo.store.(writableGitRemoteStore)
	if !ok {
		return errors.New("receive-pack requires a writable store")
	}

	var unpackErr error
	if receivePackNeedsPack(req.commands) {
		_, unpackErr = ingestReceivePack(ctx, repo, store, req.pack)
	}
	if unpackErr != nil {
		if req.caps["report-status"] || req.caps["report-status-v2"] {
			_ = writeReceivePackReport(stdout, req, unpackErr, map[string]error{})
		}
		return unpackErr
	}

	brokerURL := ""
	if repo.cfg.origin != "" {
		var err error
		brokerURL, err = brokerURLForSSHService(repo.cfg)
		if err != nil {
			return err
		}
	}
	commandErrs, applyErr := applyReceivePackCommands(ctx, repo, store, req.commands, req.caps["atomic"], brokerURL)
	if req.caps["report-status"] || req.caps["report-status-v2"] {
		if err := writeReceivePackReport(stdout, req, nil, commandErrs); err != nil && applyErr == nil {
			return err
		}
	}
	return applyErr
}

func writeReceivePackReport(stdout io.Writer, req receivePackRequest, unpackErr error, commandErrs map[string]error) error {
	if !req.caps["side-band-64k"] {
		return writeReceivePackReportStatus(stdout, req.commands, unpackErr, commandErrs)
	}
	var report strings.Builder
	if err := writeReceivePackReportStatus(&report, req.commands, unpackErr, commandErrs); err != nil {
		return err
	}
	if err := writeSideband(stdout, 1, []byte(report.String()), 65516); err != nil {
		return err
	}
	return writePktFlush(stdout)
}

func receivePackNeedsPack(commands []receivePackCommand) bool {
	for _, cmd := range commands {
		if cmd.new != zeroObjectID() {
			return true
		}
	}
	return false
}

func applyReceivePackCommands(ctx context.Context, repo *nativeGitRepo, store writableGitRemoteStore, commands []receivePackCommand, atomic bool, brokerURL string) (map[string]error, error) {
	refs, err := repo.refs(ctx)
	if err != nil {
		return nil, err
	}
	commandErrs := map[string]error{}
	var firstErr error
	for _, cmd := range commands {
		if err := validateReceivePackCommand(ctx, repo, refs, cmd); err != nil {
			commandErrs[cmd.ref] = err
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if atomic && firstErr != nil {
		for _, cmd := range commands {
			if commandErrs[cmd.ref] == nil {
				commandErrs[cmd.ref] = errors.New("atomic push failed")
			}
		}
		return commandErrs, firstErr
	}
	for _, cmd := range commands {
		if commandErrs[cmd.ref] != nil {
			continue
		}
		switch cmd.action() {
		case "create", "update":
			if brokerURL != "" {
				if err := brokerUpdateRef(brokerURL, repo.cfg, cmd.ref, cmd.old, cmd.new); err != nil {
					commandErrs[cmd.ref] = err
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
			}
			if err := store.write(ctx, cmd.ref, []byte(cmd.new+"\n")); err != nil {
				commandErrs[cmd.ref] = err
				if firstErr == nil {
					firstErr = err
				}
			} else {
				refs[cmd.ref] = cmd.new
			}
		case "delete":
			if brokerURL != "" {
				if err := brokerUpdateRef(brokerURL, repo.cfg, cmd.ref, cmd.old, zeroObjectID()); err != nil {
					commandErrs[cmd.ref] = err
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
			}
			if err := store.delete(ctx, cmd.ref); err != nil {
				commandErrs[cmd.ref] = err
				if firstErr == nil {
					firstErr = err
				}
			} else {
				delete(refs, cmd.ref)
			}
		case "noop":
		default:
			err := fmt.Errorf("unsupported ref update %s", cmd.action())
			commandErrs[cmd.ref] = err
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return commandErrs, firstErr
}

func validateReceivePackCommand(ctx context.Context, repo *nativeGitRepo, refs map[string]string, cmd receivePackCommand) error {
	current, exists := refs[cmd.ref]
	switch cmd.action() {
	case "create":
		if exists {
			return fmt.Errorf("ref already exists")
		}
	case "update":
		if !exists {
			return fmt.Errorf("ref does not exist")
		}
		if current != cmd.old {
			return fmt.Errorf("stale ref")
		}
	case "delete":
		if !exists {
			return fmt.Errorf("ref does not exist")
		}
		if current != cmd.old {
			return fmt.Errorf("stale ref")
		}
	case "noop":
		return nil
	default:
		return fmt.Errorf("unsupported ref update %s", cmd.action())
	}
	if cmd.new == zeroObjectID() {
		return nil
	}
	if _, err := repo.object(ctx, cmd.new); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("missing object %s", cmd.new)
		}
		return err
	}
	return nil
}
