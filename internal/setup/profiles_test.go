package setup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseAWSProfileFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("[default]\nregion=us-east-1\n[profile work]\nregion=eu-west-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ParseAWSProfileFile(path); !reflect.DeepEqual(got, []string{"default", "work"}) {
		t.Fatalf("profiles = %#v", got)
	}
	if got := AWSProfileFileValue(path, "work", "region"); got != "eu-west-1" {
		t.Fatalf("region = %q", got)
	}
}

func TestDiscoverGCPProfilesUsesInjectedCommands(t *testing.T) {
	command := func(_ context.Context, _ string, args ...string) *exec.Cmd {
		output := ""
		switch {
		case len(args) > 1 && args[0] == "config" && args[1] == "configurations":
			output = "default true\n"
		case len(args) > 4 && args[0] == "--configuration" && args[3] == "get-value":
			switch args[4] {
			case "account":
				output = "owner@example.com\n"
			case "project":
				output = "demo-project\n"
			case "run/region":
				output = "europe-west1\n"
			}
		}
		return exec.Command(os.Args[0], "-test.run=TestProfileCommandHelper", "--", output)
	}
	profiles, err := DiscoverGCPProfiles(context.Background(), ProfileDiscoveryOptions{
		LookPath: func(string) (string, error) { return "gcloud", nil },
		Command:  command,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].ProjectID != "demo-project" || profiles[0].Region != "europe-west1" {
		t.Fatalf("profiles = %#v", profiles)
	}
}

func TestProfileCommandHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			_, _ = os.Stdout.WriteString(os.Args[i+1])
			os.Exit(0)
		}
	}
}
