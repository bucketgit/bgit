package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const (
	pktFlush       = "0000"
	pktDelim       = "0001"
	pktResponseEnd = "0002"

	gitUploadPackService  = "git-upload-pack"
	gitReceivePackService = "git-receive-pack"
)

type pktLineKind int

const (
	pktLineData pktLineKind = iota
	pktLineFlush
	pktLineDelim
	pktLineResponseEnd
)

type pktLine struct {
	kind pktLineKind
	data []byte
}

func writePktLine(w io.Writer, data []byte) error {
	if len(data) > 65516 {
		return errors.New("pkt-line payload too large")
	}
	_, err := fmt.Fprintf(w, "%04x%s", len(data)+4, data)
	return err
}

func writePktLineString(w io.Writer, data string) error {
	return writePktLine(w, []byte(data))
}

func writePktFlush(w io.Writer) error {
	_, err := io.WriteString(w, pktFlush)
	return err
}

func writePktDelim(w io.Writer) error {
	_, err := io.WriteString(w, pktDelim)
	return err
}

func writePktResponseEnd(w io.Writer) error {
	_, err := io.WriteString(w, pktResponseEnd)
	return err
}

func readPktLine(r *bufio.Reader) (pktLine, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return pktLine{}, err
	}
	switch string(hdr[:]) {
	case pktFlush:
		return pktLine{kind: pktLineFlush}, nil
	case pktDelim:
		return pktLine{kind: pktLineDelim}, nil
	case pktResponseEnd:
		return pktLine{kind: pktLineResponseEnd}, nil
	}
	n, err := strconv.ParseInt(string(hdr[:]), 16, 32)
	if err != nil {
		return pktLine{}, fmt.Errorf("invalid pkt-line length %q", string(hdr[:]))
	}
	if n < 4 {
		return pktLine{}, fmt.Errorf("invalid pkt-line length %d", n)
	}
	data := make([]byte, int(n)-4)
	if _, err := io.ReadFull(r, data); err != nil {
		return pktLine{}, err
	}
	return pktLine{kind: pktLineData, data: data}, nil
}

func pktLineDataString(line pktLine) (string, bool) {
	if line.kind != pktLineData {
		return "", false
	}
	return string(line.data), true
}

func writeSideband(w io.Writer, channel byte, data []byte, maxPayload int) error {
	if channel < 1 || channel > 3 {
		return fmt.Errorf("invalid sideband channel %d", channel)
	}
	if maxPayload <= 1 || maxPayload > 65516 {
		maxPayload = 65516
	}
	chunkSize := maxPayload - 1
	for len(data) > 0 {
		n := chunkSize
		if len(data) < n {
			n = len(data)
		}
		payload := append([]byte{channel}, data[:n]...)
		if err := writePktLine(w, payload); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

type gitCapabilities []string

func (c gitCapabilities) String() string {
	return strings.Join(c, " ")
}

func uploadPackCapabilities() gitCapabilities {
	return gitCapabilities{
		"multi_ack",
		"multi_ack_detailed",
		"no-done",
		"thin-pack",
		"side-band",
		"side-band-64k",
		"ofs-delta",
		"no-progress",
		"include-tag",
		"allow-tip-sha1-in-want",
		"allow-reachable-sha1-in-want",
		"symref=HEAD:refs/heads/main",
		"object-format=sha1",
		"agent=bgit",
	}
}

func receivePackCapabilities() gitCapabilities {
	return gitCapabilities{
		"report-status",
		"report-status-v2",
		"delete-refs",
		"side-band-64k",
		"quiet",
		"atomic",
		"ofs-delta",
		"push-options",
		"object-format=sha1",
		"agent=bgit",
	}
}

type gitCommandInvocation struct {
	service string
	repo    string
	host    string
}

func parseGitCommand(args []string) (string, string, error) {
	inv, err := parseGitCommandInvocation(args)
	return inv.service, inv.repo, err
}

func parseGitCommandInvocation(args []string) (gitCommandInvocation, error) {
	if len(args) == 0 {
		return gitCommandInvocation{}, errors.New("missing git service command")
	}
	for i, arg := range args {
		for _, service := range []string{gitUploadPackService, gitReceivePackService} {
			if arg == service {
				if len(args) <= i+1 {
					return gitCommandInvocation{}, errors.New("missing repository path")
				}
				return gitCommandInvocation{service: service, repo: cleanGitServiceRepo(args[i+1]), host: sshInvocationHost(args, i)}, nil
			}
			if strings.HasPrefix(arg, service+" ") {
				return gitCommandInvocation{service: service, repo: cleanGitServiceRepo(strings.TrimSpace(strings.TrimPrefix(arg, service))), host: sshInvocationHost(args, i)}, nil
			}
		}
	}
	joined := strings.Join(args, " ")
	for _, service := range []string{gitUploadPackService, gitReceivePackService} {
		marker := " " + service + " "
		if strings.Contains(joined, marker) {
			host, repo, _ := strings.Cut(joined, marker)
			return gitCommandInvocation{service: service, repo: cleanGitServiceRepo(repo), host: cleanSSHHost(lastField(host))}, nil
		}
	}
	return gitCommandInvocation{}, fmt.Errorf("unsupported git service command %q", joined)
}

func sshInvocationHost(args []string, serviceIndex int) string {
	if serviceIndex <= 0 {
		return ""
	}
	return cleanSSHHost(args[serviceIndex-1])
}

func cleanSSHHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.Trim(host, `"'`)
	if strings.Contains(host, "@") {
		_, host, _ = strings.Cut(host, "@")
	}
	return host
}

func lastField(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func cleanGitServiceRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	repo = strings.Trim(repo, `"'`)
	repo = strings.TrimPrefix(repo, "/")
	return repo
}

func writeAdvertisedRefs(w io.Writer, service string, refs map[string]string, caps gitCapabilities) error {
	if service != gitUploadPackService && service != gitReceivePackService {
		return fmt.Errorf("unsupported service %q", service)
	}
	names := make([]string, 0, len(refs))
	for name := range refs {
		if name == "HEAD" || strings.HasPrefix(name, "refs/") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if _, ok := refs["HEAD"]; ok {
		names = append([]string{"HEAD"}, removeString(names, "HEAD")...)
	}
	if len(names) == 0 {
		return writePktFlush(w)
	}
	first := true
	for _, name := range names {
		hash := strings.TrimSpace(refs[name])
		if !isHexHash(hash) {
			continue
		}
		line := hash + " " + name
		if first {
			line += "\x00" + caps.String()
			first = false
		}
		if err := writePktLineString(w, line+"\n"); err != nil {
			return err
		}
	}
	return writePktFlush(w)
}

func removeString(values []string, remove string) []string {
	out := values[:0]
	for _, value := range values {
		if value != remove {
			out = append(out, value)
		}
	}
	return out
}

func pktLinesForTest(buf []byte) ([]pktLine, error) {
	r := bufio.NewReader(bytes.NewReader(buf))
	var lines []pktLine
	for r.Buffered() > 0 || len(lines) == 0 {
		line, err := readPktLine(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return lines, nil
			}
			return nil, err
		}
		lines = append(lines, line)
		if r.Buffered() == 0 {
			return lines, nil
		}
	}
	return lines, nil
}
