package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"os/exec"
	"testing"
)

func TestReadUploadPackRequest(t *testing.T) {
	var input bytes.Buffer
	if err := writePktLineString(&input, "want 0123456789abcdef0123456789abcdef01234567 multi_ack side-band-64k ofs-delta\n"); err != nil {
		t.Fatal(err)
	}
	if err := writePktLineString(&input, "have aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"); err != nil {
		t.Fatal(err)
	}
	if err := writePktLineString(&input, "done\n"); err != nil {
		t.Fatal(err)
	}
	req, err := readUploadPackRequest(&input)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.wants) != 1 || req.wants[0] != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("wants = %#v", req.wants)
	}
	if len(req.haves) != 1 || req.haves[0] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("haves = %#v", req.haves)
	}
	if !req.done || !req.sideband64 {
		t.Fatalf("req = %#v", req)
	}
}

func TestReadUploadPackRequestRespondsToNegotiationFlush(t *testing.T) {
	var input bytes.Buffer
	if err := writePktLineString(&input, "want 0123456789abcdef0123456789abcdef01234567 multi_ack side-band-64k ofs-delta\n"); err != nil {
		t.Fatal(err)
	}
	if err := writePktFlush(&input); err != nil {
		t.Fatal(err)
	}
	if err := writePktLineString(&input, "have aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"); err != nil {
		t.Fatal(err)
	}
	if err := writePktFlush(&input); err != nil {
		t.Fatal(err)
	}
	if err := writePktLineString(&input, "done\n"); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	req, err := readUploadPackRequestWithResponses(&input, &output)
	if err != nil {
		t.Fatal(err)
	}
	if !req.responded {
		t.Fatalf("expected negotiation response")
	}
	lines, err := pktLinesForTest(output.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || string(lines[0].data) != "NAK\n" || string(lines[1].data) != "NAK\n" {
		t.Fatalf("response lines = %#v", lines)
	}
}

func TestEncodePackWritesValidHeaderAndChecksum(t *testing.T) {
	bare := createBareFixture(t)
	repo := newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: bare})
	refs, err := repo.refs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hashes, err := repo.objectsForUploadPack(context.Background(), []string{refs["refs/heads/main"]}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) == 0 {
		t.Fatal("expected objects")
	}
	pack, err := repo.encodePack(context.Background(), hashes)
	if err != nil {
		t.Fatal(err)
	}
	if string(pack[:4]) != "PACK" {
		t.Fatalf("pack magic = %q", pack[:4])
	}
	if version := binary.BigEndian.Uint32(pack[4:8]); version != 2 {
		t.Fatalf("pack version = %d", version)
	}
	if count := int(binary.BigEndian.Uint32(pack[8:12])); count != len(hashes) {
		t.Fatalf("pack count = %d, want %d", count, len(hashes))
	}
	got := pack[len(pack)-20:]
	sum := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(got, sum[:]) {
		t.Fatal("pack checksum mismatch")
	}
	cmd := exec.Command("git", "index-pack", "--stdin")
	cmd.Stdin = bytes.NewReader(pack)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git index-pack failed: %v\n%s", err, out)
	}
}

func TestServeUploadPackWritesNAKAndSidebandPack(t *testing.T) {
	bare := createBareFixture(t)
	repo := newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: bare})
	refs, err := repo.refs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	if err := writePktLineString(&input, "want "+refs["refs/heads/main"]+" side-band-64k\n"); err != nil {
		t.Fatal(err)
	}
	if err := writePktLineString(&input, "done\n"); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := serveUploadPack(context.Background(), repo, &input, &output); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(&output)
	line, err := readPktLine(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(line.data) != "NAK\n" {
		t.Fatalf("first line = %q", line.data)
	}
	line, err = readPktLine(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(line.data) < 5 || line.data[0] != 1 || string(line.data[1:5]) != "PACK" {
		t.Fatalf("sideband pack line = %q", line.data[:min(len(line.data), 16)])
	}
}
