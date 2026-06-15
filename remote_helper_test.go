package main

import (
	"bytes"
	"strings"
	"testing"
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
	cfg, err := configForRemoteHelperAddress("bgit://demo.git")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.logicalRepo != "demo.git" || cfg.prefix != "demo.git" {
		t.Fatalf("cfg = %#v", cfg)
	}
}
