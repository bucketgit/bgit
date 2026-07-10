package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseIntent(t *testing.T) {
	intent := ParseIntent([]string{"board", "--help"})
	if intent.Command != "board" || !intent.Help || intent.Version {
		t.Fatalf("intent=%#v", intent)
	}
	intent = ParseIntent([]string{"status", "--version"})
	if intent.Command != "status" || !intent.Version || intent.Help {
		t.Fatalf("intent=%#v", intent)
	}
}

func TestUsageWithBanner(t *testing.T) {
	var output bytes.Buffer
	if err := UsageWithBanner(&output, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"bgit 1.2.3", "usage: bgit", "local web UI", "administer"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output does not contain %q", expected)
		}
	}
}
