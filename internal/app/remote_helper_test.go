package app

import (
	"bytes"
	"strings"
	"testing"

	localbroker "github.com/bucketgit/bgit/broker/local"
	"github.com/bucketgit/bgit/protocol"
)

func TestRemoteHelperCapabilities(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := remoteHelperCommand([]string{"bgit://demo.git"}, strings.NewReader("capabilities\n\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("remoteHelperCommand: %v\nstderr: %s", err, stderr.String())
	}
	if got := stdout.String(); got != "connect\n\n" {
		t.Fatalf("capabilities output = %q", got)
	}
}

func TestRemoteHelperAddress(t *testing.T) {
	if got := remoteHelperAddress([]string{"origin", "bgit://demo.git"}); got != "bgit://demo.git" {
		t.Fatalf("address with url = %q", got)
	}
	if got := remoteHelperAddress([]string{"bgit::demo.git"}); got != "bgit::demo.git" {
		t.Fatalf("address without url = %q", got)
	}
}

func TestRemoteHelperBrokerURLConfig(t *testing.T) {
	cfg, err := configForRemoteHelperAddress("https://broker.example.com/demo.git")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.brokerURL != "https://broker.example.com" || cfg.logicalRepo != "demo.git" || cfg.prefix != "demo.git" {
		t.Fatalf("cfg = %#v", cfg)
	}
}

func TestRemoteHelperLogicalURLConfig(t *testing.T) {
	t.Setenv("BGIT_HOME", t.TempDir())
	_, err := configForRemoteHelperAddress("bgit://demo.git")
	if err == nil || !strings.Contains(err.Error(), "explicit bgit::gs://") {
		t.Fatalf("error = %v", err)
	}
}

func TestRemoteHelperFileShorthandRehydratesExistingRepository(t *testing.T) {
	t.Setenv("BGIT_HOME", t.TempDir())
	server, err := localBrokerServerForURL("local://default/default")
	if err != nil {
		t.Fatal(err)
	}
	repo := protocol.Repository{Provider: "file", Bucket: "file://demo", Logical: "demo.git", TeamID: coreTeamID}
	if err := server.saveRepo(localbroker.RepositoryState{Repo: repo}); err != nil {
		t.Fatal(err)
	}
	session, err := resolveRemoteHelperSession(t.Context(), "bgit::file://demo")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if session.config.provider != "local" || session.config.logicalRepo != "demo.git" || session.config.storageProvider != "file" {
		t.Fatalf("config = %#v", session.config)
	}
}

func TestRemoteHelperFileShorthandDoesNotCreateMissingRepository(t *testing.T) {
	t.Setenv("BGIT_HOME", t.TempDir())
	_, err := resolveRemoteHelperSession(t.Context(), "bgit::file://missing")
	if err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("error = %v", err)
	}
	server, serverErr := localBrokerServerForURL("local://default/default")
	if serverErr != nil {
		t.Fatal(serverErr)
	}
	if _, ok := server.indexedRepo("missing.git"); ok {
		t.Fatal("missing read-only helper target was added to the repository index")
	}
}
