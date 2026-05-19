package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWhoamiCommandWritesGlobalCache(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/status" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(brokerAuthStatus{
			BrokerVersion: "1.0.0-dev",
			Repo:          brokerRepo{Provider: "gcs", Logical: "foo.git"},
			Identity:      brokerIdentity{User: "dennis", KeyFingerprint: "SHA256:test"},
			User:          "dennis",
			Role:          "admin",
			Capabilities:  map[string]bool{"read": true, "push": true, "admin_keys": true},
			ResolvedAt:    "2026-05-18T12:00:00Z",
		})
	}))
	defer server.Close()

	var stdout bytes.Buffer
	cfg := config{provider: "gcs", brokerURL: server.URL, logicalRepo: "foo.git", prefix: "foo.git"}
	if err := whoamiCommand(nilContext{}, cfg, []string{"--refresh"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "user: dennis") || !strings.Contains(stdout.String(), "role: admin") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	path, err := whoamiCachePath(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"role": "admin"`)) {
		t.Fatalf("cache = %s", data)
	}
	if !strings.HasPrefix(path, filepath.Join(home, ".bgit", "cache")) {
		t.Fatalf("cache path = %s", path)
	}
}

func TestPreferredBrokerKeyRankUsesConfiguredThenCachedKeys(t *testing.T) {
	preferred := []string{"SHA256:configured", "SHA256:cached"}
	if preferredBrokerKeyRank("SHA256:configured", preferred) >= preferredBrokerKeyRank("SHA256:cached", preferred) {
		t.Fatal("configured key should rank before cached key")
	}
	if preferredBrokerKeyRank("SHA256:cached", preferred) >= preferredBrokerKeyRank("SHA256:other", preferred) {
		t.Fatal("cached key should rank before unrelated agent keys")
	}
}

func TestRepoMembershipWarningsShowAmbiguousKeys(t *testing.T) {
	warnings := repoMembershipWarnings([]brokerRepoMembership{
		{Logical: "foo.git", User: "dennis", Role: "admin", KeyFingerprint: "SHA256:a"},
		{Logical: "foo.git", User: "dennis", Role: "read", KeyFingerprint: "SHA256:b"},
		{Logical: "bar.git", User: "work", Role: "read", KeyFingerprint: "SHA256:c"},
		{Logical: "bar.git", User: "personal", Role: "read", KeyFingerprint: "SHA256:d"},
	})
	got := strings.Join(warnings, "\n")
	if !strings.Contains(got, "foo.git is available through multiple SSH keys with different roles") {
		t.Fatalf("warnings = %#v", warnings)
	}
	if !strings.Contains(got, "bar.git is available through multiple SSH keys with different user labels") {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestExplicitProfileSelectionAppliesToRepositoryDiscovery(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	path := filepath.Join(home, ".bgit", "config.yaml")
	if err := writeGlobalConfig(path, globalConfig{
		Version: globalConfigVersion,
		GCPProfiles: []globalGCPProfile{{
			Name:      "work",
			ProjectID: "example-test-123456",
			Regions: []globalProfileRegion{{
				Name:      "europe-west1",
				BrokerURL: "https://gcp.example.test",
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config{gcloudConfiguration: "work.europe-west1", gcloudConfigurationExplicit: true}
	if err := applyExplicitBrokerProfileSelection(&cfg, "repos"); err != nil {
		t.Fatal(err)
	}
	if cfg.brokerURL != "https://gcp.example.test" || cfg.provider != "gcs" || cfg.region != "europe-west1" {
		t.Fatalf("cfg = %#v", cfg)
	}
}
