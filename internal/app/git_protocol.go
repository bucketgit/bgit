package app

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	transportpkg "github.com/bucketgit/bgit/transport"
)

const (
	pktFlush       = "0000"
	pktDelim       = "0001"
	pktResponseEnd = "0002"

	gitUploadPackService  = transportpkg.UploadPackService
	gitReceivePackService = transportpkg.ReceivePackService
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

func writePktLineString(w io.Writer, data string) error {
	return transportpkg.WriteString(w, data)
}

func writePktFlush(w io.Writer) error {
	return transportpkg.WriteFlush(w)
}

func writePktDelim(w io.Writer) error {
	return transportpkg.WriteDelimiter(w)
}

func writePktResponseEnd(w io.Writer) error {
	return transportpkg.WriteResponseEnd(w)
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

func writeSideband(w io.Writer, channel byte, data []byte, maxPayload int) error {
	return transportpkg.WriteSideband(w, channel, data, maxPayload)
}

type gitCapabilities []string

func (c gitCapabilities) String() string {
	return strings.Join(c, " ")
}

func uploadPackCapabilities() gitCapabilities {
	return gitCapabilities(transportpkg.UploadPackCapabilities())
}

func receivePackCapabilities() gitCapabilities {
	return gitCapabilities(transportpkg.ReceivePackCapabilities())
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
	return transportpkg.WriteAdvertisedRefs(w, service, refs, transportpkg.Capabilities(caps))
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
