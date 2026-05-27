package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGlobalConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".bgit", "config.yaml")
	want := globalConfig{
		Version: globalConfigVersion,
		Identity: globalIdentityConfig{
			Name:  "Dennis Example",
			Email: "dennis@example.com",
		},
		GCPProfiles: []globalGCPProfile{{
			Name:           "work",
			ProjectID:      "example-test-123456",
			Account:        "dennis@example.com",
			ServiceAccount: "bgit-broker@example-test-123456.iam.gserviceaccount.com",
			Regions: []globalProfileRegion{{
				Name:          "europe-west1",
				BrokerURL:     "https://gcp.example.test",
				BrokerVersion: brokerVersion(),
				LastSetupAt:   "2026-05-16T10:00:00Z",
			}},
		}},
		AWSProfiles: []globalAWSProfile{{
			Name:      "work",
			AccountID: "123456789012",
			ARN:       "arn:aws:iam::123456789012:user/dennis",
			Regions: []globalProfileRegion{{
				Name:          "us-east-1",
				BrokerURL:     "https://aws.example.test",
				BrokerVersion: brokerVersion(),
				LastSetupAt:   "2026-05-16T10:00:00Z",
			}},
		}},
		Repos: []globalRepoConfig{{
			Name:      "app.git",
			Profile:   "gcp:work",
			BrokerURL: "https://gcp.example.test",
		}},
	}
	if err := writeGlobalConfig(path, want); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "gcp:") ||
		!strings.Contains(text, "identity:") ||
		!strings.Contains(text, "Dennis Example") ||
		!strings.Contains(text, "aws:") ||
		!strings.Contains(text, "profiles:") ||
		!strings.Contains(text, "work:") ||
		!strings.Contains(text, "regions:") ||
		!strings.Contains(text, "europe-west1:") ||
		!strings.Contains(text, "us-east-1:") {
		t.Fatalf("config format =\n%s", string(data))
	}
	got, err := readGlobalConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != want.Version ||
		len(got.GCPProfiles) != 1 ||
		len(got.AWSProfiles) != 1 ||
		len(got.Repos) != 1 ||
		got.Identity != want.Identity {
		t.Fatalf("cfg = %#v", got)
	}
	if !reflect.DeepEqual(got.GCPProfiles[0], want.GCPProfiles[0]) {
		t.Fatalf("gcp profile = %#v", got.GCPProfiles[0])
	}
	if !reflect.DeepEqual(got.AWSProfiles[0], want.AWSProfiles[0]) {
		t.Fatalf("aws profile = %#v", got.AWSProfiles[0])
	}
	if got.Repos[0] != want.Repos[0] {
		t.Fatalf("repo = %#v", got.Repos[0])
	}
}

func TestDefaultGlobalConfigPathUsesYAML(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	got, err := defaultGlobalConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".bgit", "config.yaml")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestReadGlobalConfigDoesNotFallBackToLegacyConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".bgit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(dir, "config")
	data := `version = 1

[[gcp.profiles]]
name = "work"
region = "europe-west1"
broker_url = "https://gcp.example.test"
`
	if err := os.WriteFile(legacyPath, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readGlobalConfig(filepath.Join(dir, "config.yaml"))
	if err == nil {
		t.Fatal("expected missing config.yaml error")
	}
}

func TestGlobalConfigRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "version: 1\ngcp:\n  unknown: value\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readGlobalConfig(path)
	if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("err = %v", err)
	}
}
