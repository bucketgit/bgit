package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadReceivePackRequest(t *testing.T) {
	var input bytes.Buffer
	old := "0123456789abcdef0123456789abcdef01234567"
	new := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := writePktLineString(&input, old+" "+new+" refs/heads/main\x00report-status side-band-64k push-options object-format=sha1\n"); err != nil {
		t.Fatal(err)
	}
	if err := writePktFlush(&input); err != nil {
		t.Fatal(err)
	}
	if err := writePktLineString(&input, "ci.skip\n"); err != nil {
		t.Fatal(err)
	}
	if err := writePktFlush(&input); err != nil {
		t.Fatal(err)
	}
	input.WriteString("PACK")
	req, err := readReceivePackRequest(&input)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.commands) != 1 {
		t.Fatalf("commands = %#v", req.commands)
	}
	cmd := req.commands[0]
	if cmd.old != old || cmd.new != new || cmd.ref != "refs/heads/main" {
		t.Fatalf("cmd = %#v", cmd)
	}
	if !req.caps["report-status"] || !req.caps["side-band-64k"] || !req.caps["push-options"] || !req.caps["object-format"] {
		t.Fatalf("caps = %#v", req.caps)
	}
	if len(req.pushOptions) != 1 || req.pushOptions[0] != "ci.skip" {
		t.Fatalf("pushOptions = %#v", req.pushOptions)
	}
}

func TestReceivePackCommandAction(t *testing.T) {
	zero := zeroObjectID()
	hash := "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		cmd  receivePackCommand
		want string
	}{
		{receivePackCommand{old: zero, new: hash}, "create"},
		{receivePackCommand{old: hash, new: zero}, "delete"},
		{receivePackCommand{old: hash, new: hash}, "update"},
		{receivePackCommand{old: zero, new: zero}, "noop"},
	}
	for _, tc := range cases {
		if got := tc.cmd.action(); got != tc.want {
			t.Fatalf("action = %q, want %q", got, tc.want)
		}
	}
}

func TestWriteReceivePackReportStatus(t *testing.T) {
	cmds := []receivePackCommand{{ref: "refs/heads/main"}}
	var output bytes.Buffer
	if err := writeReceivePackReportStatus(&output, cmds, nil, map[string]error{}); err != nil {
		t.Fatal(err)
	}
	lines, err := pktLinesForTest(output.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("lines = %#v", lines)
	}
	if string(lines[0].data) != "unpack ok\n" || string(lines[1].data) != "ok refs/heads/main\n" || lines[2].kind != pktLineFlush {
		t.Fatalf("lines = %#v", lines)
	}
}

func TestDecodeReceivePackObjectsFromEncodedPack(t *testing.T) {
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
	pack, err := repo.encodePack(context.Background(), hashes)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := decodePackObjects(pack)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, obj := range objects {
		got[obj.hash] = true
		if obj.typ == "" || len(obj.data) == 0 {
			t.Fatalf("decoded object = %#v", obj)
		}
	}
	if !got[refs["refs/heads/main"]] {
		t.Fatalf("decoded hashes missing HEAD %s", refs["refs/heads/main"])
	}
}

func TestDecodeReceivePackObjectsFromNativeGitPack(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if _, err := runGit("", "init", "--initial-branch", "main", source); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(source, "config", "user.name", "Ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(source, "config", "user.email", "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte(strings.Repeat("alpha\n", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(source, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(source, "commit", "-m", "Initial"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte(strings.Repeat("alpha\n", 180)+strings.Repeat("beta\n", 20)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(source, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(source, "commit", "-m", "Update"); err != nil {
		t.Fatal(err)
	}
	headOut, err := runGit(source, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(headOut))
	cmd := exec.Command("git", "-C", source, "pack-objects", "--stdout", "--revs")
	cmd.Stdin = strings.NewReader("HEAD\n")
	pack, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	objects, err := decodePackObjects(pack)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, obj := range objects {
		got[obj.hash] = true
	}
	if !got[head] {
		t.Fatalf("decoded native pack missing HEAD %s", head)
	}
}

func TestServeReceivePackAcceptsThinPackWithExistingBase(t *testing.T) {
	ctx := context.Background()
	remoteRoot := createBareFixture(t)
	remoteRepo := newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: remoteRoot})
	refs, err := remoteRepo.refs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old := refs["refs/heads/main"]

	worktree := filepath.Join(t.TempDir(), "work")
	if _, err := runGit("", "clone", remoteRoot, worktree); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "config", "user.name", "Ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "config", "user.email", "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte(strings.Repeat("alpha\n", 180)+strings.Repeat("beta\n", 20)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "commit", "-m", "Update"); err != nil {
		t.Fatal(err)
	}
	newOut, err := runGit(worktree, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	newHash := strings.TrimSpace(string(newOut))

	cmd := exec.Command("git", "-C", worktree, "pack-objects", "--stdout", "--thin", "--revs")
	cmd.Stdin = strings.NewReader(newHash + "\n^" + old + "\n")
	pack, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	if err := writePktLineString(&input, old+" "+newHash+" refs/heads/main\x00report-status\n"); err != nil {
		t.Fatal(err)
	}
	if err := writePktFlush(&input); err != nil {
		t.Fatal(err)
	}
	input.Write(pack)
	var output bytes.Buffer
	if err := serveReceivePack(ctx, remoteRepo, &input, &output); err != nil {
		t.Fatal(err)
	}
	refData, err := os.ReadFile(filepath.Join(remoteRoot, "refs", "heads", "main"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(refData)) != newHash {
		t.Fatalf("ref = %q, want %s", refData, newHash)
	}
}

func TestServeReceivePackCreatesRefAndWritesObjects(t *testing.T) {
	ctx := context.Background()
	sourceBare := createBareFixture(t)
	sourceRepo := newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: sourceBare})
	refs, err := sourceRepo.refs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head := refs["refs/heads/main"]
	hashes, err := sourceRepo.objectsForUploadPack(ctx, []string{head}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pack, err := sourceRepo.encodePack(ctx, hashes)
	if err != nil {
		t.Fatal(err)
	}

	remoteRoot := filepath.Join(t.TempDir(), "remote.git")
	if _, err := runGit("", "init", "--bare", remoteRoot); err != nil {
		t.Fatal(err)
	}
	remoteRepo := newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: remoteRoot})
	var input bytes.Buffer
	if err := writePktLineString(&input, zeroObjectID()+" "+head+" refs/heads/main\x00report-status side-band-64k\n"); err != nil {
		t.Fatal(err)
	}
	if err := writePktFlush(&input); err != nil {
		t.Fatal(err)
	}
	input.Write(pack)

	var output bytes.Buffer
	if err := serveReceivePack(ctx, remoteRepo, &input, &output); err != nil {
		t.Fatal(err)
	}
	report := receivePackSidebandReportForTest(t, output.Bytes())
	if !strings.Contains(report, "unpack ok\n") || !strings.Contains(report, "ok refs/heads/main\n") {
		t.Fatalf("report = %q", report)
	}
	refData, err := os.ReadFile(filepath.Join(remoteRoot, "refs", "heads", "main"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(refData)) != head {
		t.Fatalf("ref = %q, want %s", refData, head)
	}
	if _, err := remoteRepo.object(ctx, head); err != nil {
		t.Fatalf("remote object: %v", err)
	}
}

func TestServeReceivePackDeletesRefWithoutPack(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "remote.git")
	if _, err := runGit("", "init", "--bare", root); err != nil {
		t.Fatal(err)
	}
	store := &localGitStore{root: root}
	old := "0123456789abcdef0123456789abcdef01234567"
	if err := store.write(ctx, "refs/heads/main", []byte(old+"\n")); err != nil {
		t.Fatal(err)
	}
	repo := newNativeGitRepoForStore(config{branch: "main"}, store)
	var input bytes.Buffer
	if err := writePktLineString(&input, old+" "+zeroObjectID()+" refs/heads/main\x00report-status\n"); err != nil {
		t.Fatal(err)
	}
	if err := writePktFlush(&input); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := serveReceivePack(ctx, repo, &input, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "refs", "heads", "main")); !os.IsNotExist(err) {
		t.Fatalf("deleted ref stat err = %v", err)
	}
	lines, err := pktLinesForTest(output.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) < 2 || string(lines[0].data) != "unpack ok\n" || string(lines[1].data) != "ok refs/heads/main\n" {
		t.Fatalf("lines = %#v", lines)
	}
}

func receivePackSidebandReportForTest(t *testing.T, data []byte) string {
	t.Helper()
	r := bufio.NewReader(bytes.NewReader(data))
	var report bytes.Buffer
	for {
		line, err := readPktLine(r)
		if err != nil {
			t.Fatal(err)
		}
		if line.kind == pktLineFlush {
			break
		}
		if len(line.data) == 0 || line.data[0] != 1 {
			t.Fatalf("sideband line = %#v", line)
		}
		report.Write(line.data[1:])
	}
	return report.String()
}
