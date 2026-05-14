package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestPktLineRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writePktLineString(&buf, "want abc\n"); err != nil {
		t.Fatal(err)
	}
	if err := writePktFlush(&buf); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(&buf)
	line, err := readPktLine(r)
	if err != nil {
		t.Fatal(err)
	}
	if line.kind != pktLineData || string(line.data) != "want abc\n" {
		t.Fatalf("line = %#v", line)
	}
	line, err = readPktLine(r)
	if err != nil {
		t.Fatal(err)
	}
	if line.kind != pktLineFlush {
		t.Fatalf("line = %#v", line)
	}
}

func TestPktLineSpecialPackets(t *testing.T) {
	var buf bytes.Buffer
	if err := writePktDelim(&buf); err != nil {
		t.Fatal(err)
	}
	if err := writePktResponseEnd(&buf); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(&buf)
	line, err := readPktLine(r)
	if err != nil {
		t.Fatal(err)
	}
	if line.kind != pktLineDelim {
		t.Fatalf("line = %#v", line)
	}
	line, err = readPktLine(r)
	if err != nil {
		t.Fatal(err)
	}
	if line.kind != pktLineResponseEnd {
		t.Fatalf("line = %#v", line)
	}
}

func TestWriteSidebandSplitsPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSideband(&buf, 1, []byte("abcdef"), 4); err != nil {
		t.Fatal(err)
	}
	lines, err := pktLinesForTest(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %#v", lines)
	}
	if string(lines[0].data) != "\x01abc" || string(lines[1].data) != "\x01def" {
		t.Fatalf("sideband data = %q %q", lines[0].data, lines[1].data)
	}
}

func TestParseGitCommand(t *testing.T) {
	service, repo, err := parseGitCommand([]string{"git-upload-pack", "'bucket/path/repo.git'"})
	if err != nil {
		t.Fatal(err)
	}
	if service != gitUploadPackService || repo != "bucket/path/repo.git" {
		t.Fatalf("service=%q repo=%q", service, repo)
	}
	service, repo, err = parseGitCommand([]string{"git-receive-pack '/bucket/path/repo.git'"})
	if err != nil {
		t.Fatal(err)
	}
	if service != gitReceivePackService || repo != "bucket/path/repo.git" {
		t.Fatalf("service=%q repo=%q", service, repo)
	}
	service, repo, err = parseGitCommand([]string{"git@git.bucketgit.com", "git-receive-pack", "'bucket/path/repo.git'"})
	if err != nil {
		t.Fatal(err)
	}
	if service != gitReceivePackService || repo != "bucket/path/repo.git" {
		t.Fatalf("service=%q repo=%q", service, repo)
	}
	service, repo, err = parseGitCommand([]string{"git@git.bucketgit.com", "git-upload-pack 'bucket/path/repo.git'"})
	if err != nil {
		t.Fatal(err)
	}
	if service != gitUploadPackService || repo != "bucket/path/repo.git" {
		t.Fatalf("service=%q repo=%q", service, repo)
	}
}

func TestAdvertisedRefsIncludeCapabilitiesOnFirstRef(t *testing.T) {
	refs := map[string]string{
		"refs/heads/main": "0123456789abcdef0123456789abcdef01234567",
		"refs/tags/v1":    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	var buf bytes.Buffer
	if err := writeAdvertisedRefs(&buf, gitUploadPackService, refs, uploadPackCapabilities()); err != nil {
		t.Fatal(err)
	}
	lines, err := pktLinesForTest(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("lines = %#v", lines)
	}
	first := string(lines[0].data)
	for _, want := range []string{"refs/heads/main", "\x00", "multi_ack_detailed", "side-band-64k", "object-format=sha1"} {
		if !strings.Contains(first, want) {
			t.Fatalf("first adv ref missing %q in %q", want, first)
		}
	}
	for _, unsupported := range []string{"shallow", "deepen-since", "deepen-not", "filter"} {
		if strings.Contains(first, unsupported) {
			t.Fatalf("first adv ref includes unsupported capability %q in %q", unsupported, first)
		}
	}
	if lines[2].kind != pktLineFlush {
		t.Fatalf("last line = %#v", lines[2])
	}
}

func TestReceivePackCapabilitiesAreBroad(t *testing.T) {
	caps := receivePackCapabilities().String()
	for _, want := range []string{"report-status", "report-status-v2", "delete-refs", "side-band-64k", "atomic", "ofs-delta", "push-options"} {
		if !strings.Contains(caps, want) {
			t.Fatalf("capabilities missing %q in %q", want, caps)
		}
	}
}
