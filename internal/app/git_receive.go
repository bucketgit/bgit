package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	transportpkg "github.com/bucketgit/bgit/transport"
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

type nativeReceiveStore struct {
	repo  *nativeGitRepo
	store writableGitRemoteStore
}

func (s nativeReceiveStore) Read(ctx context.Context, path string) ([]byte, error) {
	return s.store.read(ctx, path)
}
func (s nativeReceiveStore) List(ctx context.Context, prefix string) ([]string, error) {
	return s.store.list(ctx, prefix)
}
func (s nativeReceiveStore) Write(ctx context.Context, path string, data []byte) error {
	return s.store.write(ctx, path, data)
}
func (s nativeReceiveStore) Delete(ctx context.Context, path string) error {
	return s.store.delete(ctx, path)
}
func (s nativeReceiveStore) ListRefs(ctx context.Context) (map[string]string, error) {
	return s.repo.refs(ctx)
}
func (s nativeReceiveStore) CompareAndSwapRef(ctx context.Context, ref, oldOID, newOID string) error {
	if s.repo.cfg.origin != "" {
		brokerURL, err := brokerURLForSSHService(s.repo.cfg)
		if err != nil {
			return err
		}
		if err := brokerUpdateRef(brokerURL, s.repo.cfg, ref, oldOID, firstNonEmpty(newOID, zeroObjectID())); err != nil {
			return err
		}
		if newOID == "" && strings.HasPrefix(ref, "refs/heads/") {
			_ = unsetGitBranchTracking(".", strings.TrimPrefix(ref, "refs/heads/"))
		}
		return nil
	}
	if refs, ok := s.store.(refCASGitRemoteStore); ok {
		if err := refs.compareAndSwapRef(ctx, ref, oldOID, newOID); err != nil {
			return err
		}
		if newOID == "" && strings.HasPrefix(ref, "refs/heads/") {
			_ = unsetGitBranchTracking(".", strings.TrimPrefix(ref, "refs/heads/"))
		}
		return nil
	}
	refs, err := s.repo.refs(ctx)
	if err != nil {
		return err
	}
	current, exists := refs[ref]
	if (oldOID == "" && exists) || (oldOID != "" && current != oldOID) {
		return errors.New("stale ref")
	}
	if newOID == "" {
		if err := s.store.delete(ctx, ref); err != nil {
			return err
		}
		if strings.HasPrefix(ref, "refs/heads/") {
			_ = unsetGitBranchTracking(".", strings.TrimPrefix(ref, "refs/heads/"))
		}
		return nil
	}
	return s.store.write(ctx, ref, []byte(newOID+"\n"))
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
	store, ok := repo.store.(writableGitRemoteStore)
	if !ok {
		return errors.New("receive-pack requires a writable store")
	}
	return transportpkg.ServeReceivePack(ctx, repo.engine, nativeReceiveStore{repo: repo, store: store}, stdin, stdout)
}
