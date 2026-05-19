package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAWSProfileFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	data := `[default]
region = us-east-1

[profile work]
region = eu-west-1
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	got := parseAWSProfileFile(path)
	if strings.Join(got, ",") != "default,work" {
		t.Fatalf("profiles = %#v", got)
	}
	if region := awsProfileFileValue(path, "work", "region"); region != "eu-west-1" {
		t.Fatalf("region = %q", region)
	}
}

func TestResolveAWSSetupRegionUsesConfiguredRegion(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "aws", []fakeCLIAction{})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := resolveAWSSetupRegion(context.Background(), setupProfile{Name: "prod", Region: "eu-west-1"}, "", false, strings.NewReader(""), ioDiscard{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "eu-west-1" {
		t.Fatalf("region = %q", got)
	}
}

func TestResolveAWSSetupRegionPromptsFromEnabledRegions(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "aws", []fakeCLIAction{})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	got, err := resolveAWSSetupRegion(context.Background(), setupProfile{Name: "prod"}, "", true, strings.NewReader("\x1b[B \x04"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if got != "eu-west-1" {
		t.Fatalf("region = %q", got)
	}
	if !strings.Contains(stdout.String(), "AWS profile prod regions") ||
		!strings.Contains(stdout.String(), "us-east-1") ||
		!strings.Contains(stdout.String(), "eu-west-1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestResolveAWSSetupRegionYesModeRequiresRegion(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "aws", []fakeCLIAction{})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	errRegion, err := resolveAWSSetupRegion(context.Background(), setupProfile{Name: "prod"}, "", false, strings.NewReader(""), ioDiscard{})
	if err == nil || errRegion != "" || !strings.Contains(err.Error(), "pass --region REGION") {
		t.Fatalf("region=%q err=%v", errRegion, err)
	}
}

func TestResolveAWSSetupRegionRequiresAWSCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	region, err := resolveAWSSetupRegion(context.Background(), setupProfile{Name: "prod", Region: "eu-west-1"}, "", true, strings.NewReader(""), ioDiscard{})
	if err == nil || region != "" || !strings.Contains(err.Error(), "AWS CLI is not installed") {
		t.Fatalf("region=%q err=%v", region, err)
	}
}

func TestResolveGCPSetupRegionUsesDialog(t *testing.T) {
	var stdout bytes.Buffer
	got, err := resolveGCPSetupRegion(setupProfile{Name: "work", Region: "us-central1"}, "", true, strings.NewReader("\x1b[B \x04"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if got != "us-east1" {
		t.Fatalf("region = %q", got)
	}
	if !strings.Contains(stdout.String(), "GCP profile work regions") ||
		!strings.Contains(stdout.String(), "us-central1") ||
		!strings.Contains(stdout.String(), "us-east1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestResolveGCPSetupRegionUsesExistingConfiguredRegion(t *testing.T) {
	var stdout bytes.Buffer
	got, err := resolveGCPSetupRegion(setupProfile{Name: "work", Region: "europe-west1", Existing: true}, "", true, strings.NewReader("\x04"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if got != "europe-west1" {
		t.Fatalf("region = %q", got)
	}
	if !strings.Contains(stdout.String(), "[x] europe-west1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestConfiguredSetupProfilesCarryConfiguredRegion(t *testing.T) {
	got := markConfiguredSetupProfiles([]setupProfile{{
		Provider: "gcs",
		Name:     "work",
		Region:   "us-central1",
	}}, globalConfig{GCPProfiles: []globalGCPProfile{{
		Name: "work",
		Regions: []globalProfileRegion{{
			Name:      "europe-west1",
			BrokerURL: "https://broker.example.test",
		}},
	}}})
	if len(got) != 1 {
		t.Fatalf("profiles = %#v", got)
	}
	if !got[0].Existing || got[0].Region != "europe-west1" {
		t.Fatalf("profile = %#v", got[0])
	}
}

func TestConfiguredSetupProfilesExpandConfiguredRegions(t *testing.T) {
	got := markConfiguredSetupProfiles([]setupProfile{{
		Provider: "gcs",
		Name:     "work",
	}}, globalConfig{GCPProfiles: []globalGCPProfile{{
		Name: "work",
		Regions: []globalProfileRegion{{
			Name:      "us-central1",
			BrokerURL: "https://us.example.test",
		}, {
			Name:      "europe-west1",
			BrokerURL: "https://eu.example.test",
		}},
	}}})
	if len(got) != 2 {
		t.Fatalf("profiles = %#v", got)
	}
	if got[0].Name != "work" || got[0].Region != "us-central1" || !got[0].Existing {
		t.Fatalf("first profile = %#v", got[0])
	}
	if got[1].Name != "work" || got[1].Region != "europe-west1" || !got[1].Existing {
		t.Fatalf("second profile = %#v", got[1])
	}
}

func TestSetupDialogRendersCheckboxesAndKeys(t *testing.T) {
	rendered := renderSetupDialog(setupDialogState{
		profiles: []setupProfile{{
			Provider:  "gcs",
			Name:      "work",
			ProjectID: "example-test-123456",
		}},
		keys: []setupSSHKey{{
			PublicKey: "ssh-ed25519 AAAATEST",
			Source:    "ssh-agent",
			Comment:   "dennis",
		}},
		selectedProfiles: []bool{true},
		selectedKeys:     []bool{true},
	})
	for _, want := range []string{"BUCKETGIT SETUP", "> [x] gcp:work", "Owner SSH keys", "[x] dennis", "[ OK ]", "[ Exit ]"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("dialog missing %q:\n%s", want, rendered)
		}
	}
}

func TestSetupDialogPaginatesProfilesPerProvider(t *testing.T) {
	var profiles []setupProfile
	for i := 0; i < 12; i++ {
		profiles = append(profiles, setupProfile{Provider: "gcs", Name: "gcp" + string(rune('a'+i))})
	}
	for i := 0; i < 11; i++ {
		profiles = append(profiles, setupProfile{Provider: "s3", Name: "aws" + string(rune('a'+i))})
	}
	rendered := renderSetupDialog(setupDialogState{
		profiles:         profiles,
		selectedProfiles: make([]bool, len(profiles)),
		providerPages:    map[string]int{},
	})
	if strings.Count(rendered, "gcp:") != setupDialogProfilesPerProvider {
		t.Fatalf("expected first GCP page only:\n%s", rendered)
	}
	if strings.Count(rendered, "aws:") != setupDialogProfilesPerProvider {
		t.Fatalf("expected first AWS page only:\n%s", rendered)
	}
	for _, want := range []string{"show next gcp profiles", "show next aws profiles"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("dialog missing %q:\n%s", want, rendered)
		}
	}
}

func TestSetupDialogTabJumpsBetweenProviders(t *testing.T) {
	state := setupDialogState{
		profiles: []setupProfile{
			{Provider: "gcs", Name: "work"},
			{Provider: "s3", Name: "prod"},
		},
		selectedProfiles: make([]bool, 2),
		providerPages:    map[string]int{},
	}
	state.tab()
	items := state.visibleItems()
	if state.cursor >= len(items) || items[state.cursor].Provider != "s3" {
		t.Fatalf("tab should jump to AWS provider, cursor=%d items=%#v", state.cursor, items)
	}
}

func TestSetupDialogHandlesKeyboardSelection(t *testing.T) {
	var stdout bytes.Buffer
	selected, err := runSetupDialog(strings.NewReader(" \x04"), &stdout, setupSelection{
		Profiles: []setupProfile{{
			Provider: "gcs",
			Name:     "work",
			Active:   true,
		}},
		Keys: []setupSSHKey{{
			PublicKey: "ssh-ed25519 AAAATEST",
			Comment:   "dennis",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Profiles) != 1 {
		t.Fatalf("profiles = %#v", selected.Profiles)
	}
	if len(selected.Keys) != 1 {
		t.Fatalf("keys = %#v", selected.Keys)
	}
}

func TestSetupDialogPreselectsSSHKeys(t *testing.T) {
	var stdout bytes.Buffer
	selected, err := runSetupDialog(strings.NewReader(" \x04"), &stdout, setupSelection{
		Profiles: []setupProfile{{
			Provider: "gcs",
			Name:     "work",
		}},
		Keys: []setupSSHKey{{
			PublicKey: "ssh-ed25519 AAAATEST",
			Comment:   "dennis",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Keys) != 1 {
		t.Fatalf("keys = %#v", selected.Keys)
	}
}

func TestSetupDialogCreatesAWSProfileInApp(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "aws", []fakeCLIAction{})
	t.Setenv("PATH", bin)
	var stdout bytes.Buffer
	input := " demo\n\x1b[B\nAKIA1234567890ABCDEF\n\x1b[B\nsecretkeyvalue1234567890\n\x1b[B\nus-east-1\n\x04"
	selected, err := runSetupDialog(strings.NewReader(input), &stdout, setupSelection{})
	if err != nil {
		t.Fatalf("%v\n%s", err, stdout.String())
	}
	if selected.Action != "create-profile" || selected.CreateProvider != "s3" {
		t.Fatalf("selection = %#v", selected)
	}
	if selected.CreateName != "demo" || selected.CreateAccessKey != "AKIA1234567890ABCDEF" || selected.CreateSecretKey != "secretkeyvalue1234567890" || selected.CreateRegion != "us-east-1" {
		t.Fatalf("selection = %#v", selected)
	}
	if !strings.Contains(stdout.String(), "Create AWS profile") || strings.Contains(stdout.String(), "AWS Access Key ID [None]") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestSetupCreateProfileDefaultsAvoidExistingDefault(t *testing.T) {
	defaults := setupCreateProfileDefaults([]setupProfile{{Provider: "s3", Name: "default"}}, setupOptions{})
	if defaults["s3"] != "" {
		t.Fatalf("aws default = %q", defaults["s3"])
	}
	if defaults["gcs"] != "default" {
		t.Fatalf("gcp default = %q", defaults["gcs"])
	}
}

func TestSetupCreateProfileValidationAndSingleLinePaste(t *testing.T) {
	state := setupDialogState{createProvider: "s3", createName: "default", createAccessKey: "bad", createSecretKey: "secretkeyvalue1234567890"}
	if _, ok := state.deployCreateProfile(); ok || !strings.Contains(state.message, "access key") {
		t.Fatalf("message = %q ok=%v", state.message, ok)
	}
	state = setupDialogState{createProvider: "s3", editingCreate: true}
	for _, b := range []byte("one\ntwo") {
		state.appendCreateByte(b)
	}
	if state.createName != "one" {
		t.Fatalf("createName = %q", state.createName)
	}
}

func TestCreateAWSProfileConfiguredUsesAWSConfigureSet(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "aws", []fakeCLIAction{
		{match: "configure set aws_access_key_id AKIA1234567890ABCDEF --profile demo"},
		{match: "configure set aws_secret_access_key secretkeyvalue1234567890 --profile demo"},
		{match: "configure set region eu-west-1 --profile demo"},
	})
	t.Setenv("PATH", bin)
	var stdout bytes.Buffer
	if err := createAWSProfileConfigured("demo", "AKIA1234567890ABCDEF", "secretkeyvalue1234567890", "eu-west-1", &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "created AWS profile demo") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestSetupDialogDoesNotDeployWithoutProfile(t *testing.T) {
	var stdout bytes.Buffer
	_, err := runSetupDialog(strings.NewReader("\x04\x03"), &stdout, setupSelection{
		Profiles: []setupProfile{{
			Provider: "gcs",
			Name:     "work",
			Active:   true,
		}},
		Keys: []setupSSHKey{{
			PublicKey: "ssh-ed25519 AAAATEST",
			Comment:   "dennis",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "setup canceled") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(stdout.String(), "Select a cloud profile or change identity") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestSetupDialogDeploysIdentityOnlyWhenChanged(t *testing.T) {
	state := setupDialogState{
		identityName:         "Dennis Example",
		identityEmail:        "dennis@example.com",
		initialIdentityName:  "BucketGit Client",
		initialIdentityEmail: "dennis@bucketgit.com",
	}
	selected, ok := state.deploy()
	if !ok {
		t.Fatalf("deploy rejected identity-only change: %q", state.message)
	}
	if len(selected.Profiles) != 0 {
		t.Fatalf("profiles = %#v", selected.Profiles)
	}
	if selected.IdentityName != "Dennis Example" || selected.IdentityEmail != "dennis@example.com" {
		t.Fatalf("identity = %q <%s>", selected.IdentityName, selected.IdentityEmail)
	}
}

func TestSetupDialogEOFCancels(t *testing.T) {
	var stdout bytes.Buffer
	_, err := runSetupDialog(strings.NewReader(""), &stdout, setupSelection{
		Profiles: []setupProfile{{
			Provider: "gcs",
			Name:     "work",
			Active:   true,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "setup canceled") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetupDialogCtrlCCancels(t *testing.T) {
	var stdout bytes.Buffer
	_, err := runSetupDialog(strings.NewReader("\x03"), &stdout, setupSelection{
		Profiles: []setupProfile{{
			Provider: "gcs",
			Name:     "work",
			Active:   true,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "setup canceled") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetupSSHKeyDiscoveryDedupesKeys(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	key := "ssh-ed25519 AAAATEST dennis@example"
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte(key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(home, "explicit.pub")
	if err := os.WriteFile(explicit, []byte(key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := discoverSetupSSHKeys(setupSSHKeyOptions{Paths: []string{explicit}, NoAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %#v", keys)
	}
	if keys[0].PublicKey != "ssh-ed25519 AAAATEST" || keys[0].Comment != "dennis@example" {
		t.Fatalf("key = %#v", keys[0])
	}
}

func TestSetupCommandProvisionsGCPAndWritesGlobalConfig(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	pubKey := filepath.Join(home, "owner.pub")
	if err := os.WriteFile(pubKey, []byte("ssh-ed25519 AAAAOWNER owner@example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var ownerReq brokerOwnerRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/owners/upsert" {
			t.Fatalf("unexpected broker path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&ownerReq); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "deployed")
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "config configurations list", stdout: "work True"},
		{match: "config get-value account", stdout: "dennis@example.com"},
		{match: "config get-value project", stdout: "example-test-123456"},
		{match: "auth print-access-token", stdout: "token"},
		{match: "billing projects describe example-test-123456", stdout: "True"},
		{match: "projects describe example-test-123456", stdout: "example-test-123456"},
		{match: "functions describe bgit-broker --gen2 --region europe-west1 --format=value(serviceConfig.uri)", stdout: server.URL, requireFile: marker, exitCode: 1},
		{match: "services enable"},
		{match: "services list --enabled", stdout: "serviceusage.googleapis.com cloudresourcemanager.googleapis.com cloudfunctions.googleapis.com run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com firestore.googleapis.com iamcredentials.googleapis.com"},
		{match: "firestore databases describe", exitCode: 1},
		{match: "firestore databases create"},
		{match: "iam service-accounts describe bgit-broker@example-test-123456.iam.gserviceaccount.com", exitCode: 1},
		{match: "iam service-accounts create bgit-broker"},
		{match: "projects add-iam-policy-binding example-test-123456 --member=serviceAccount:bgit-broker@example-test-123456.iam.gserviceaccount.com"},
		{match: "--service-account bgit-broker@example-test-123456.iam.gserviceaccount.com", touch: marker},
		{match: "iam service-accounts add-iam-policy-binding bgit-broker@example-test-123456.iam.gserviceaccount.com"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	configPath := filepath.Join(home, ".bgit", "config")
	var stdout bytes.Buffer
	err := setupCommand(nilContext{}, config{}, []string{"--yes", "--provider", "gcp", "--profile", "work", "--config", configPath, "--key", pubKey, "--no-agent", "--region", "europe-west1"}, strings.NewReader(""), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if len(ownerReq.PublicKeys) != 1 || ownerReq.Role != "owner" {
		t.Fatalf("owner request = %#v", ownerReq)
	}
	cfg, err := readGlobalConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.GCPProfiles) != 1 {
		t.Fatalf("cfg = %#v", cfg)
	}
	profile := cfg.GCPProfiles[0]
	if profile.Name != "work" || profile.ProjectID != "example-test-123456" ||
		len(profile.Regions) != 1 || profile.Regions[0].Name != "europe-west1" ||
		profile.Regions[0].BrokerURL != server.URL || profile.Regions[0].BrokerVersion != brokerVersion {
		t.Fatalf("profile = %#v", profile)
	}
	if !strings.Contains(stdout.String(), "Next steps:") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestSetupCommandOffersGCPProfileCreationWhenNoneExist(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	pubKey := filepath.Join(home, "owner.pub")
	if err := os.WriteFile(pubKey, []byte("ssh-ed25519 AAAAOWNER owner@example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	bin := t.TempDir()
	profileMarker := filepath.Join(t.TempDir(), "profile")
	deployMarker := filepath.Join(t.TempDir(), "deployed")
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "config configurations list", stdout: "new True", requireFile: profileMarker, exitCode: 1},
		{match: "config configurations create new", touch: profileMarker},
		{match: "auth login --configuration new"},
		{match: "config get-value account", stdout: "dennis@example.com"},
		{match: "config get-value project", stdout: "example-test-123456"},
		{match: "auth print-access-token", stdout: "token"},
		{match: "billing projects describe example-test-123456", stdout: "True"},
		{match: "projects describe example-test-123456", stdout: "example-test-123456"},
		{match: "functions describe bgit-broker --gen2 --region europe-west1 --format=value(serviceConfig.uri)", stdout: server.URL, requireFile: deployMarker, exitCode: 1},
		{match: "services enable"},
		{match: "services list --enabled", stdout: "serviceusage.googleapis.com cloudresourcemanager.googleapis.com cloudfunctions.googleapis.com run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com firestore.googleapis.com iamcredentials.googleapis.com"},
		{match: "firestore databases describe", exitCode: 1},
		{match: "firestore databases create"},
		{match: "iam service-accounts describe bgit-broker@example-test-123456.iam.gserviceaccount.com", exitCode: 1},
		{match: "iam service-accounts create bgit-broker"},
		{match: "projects add-iam-policy-binding example-test-123456 --member=serviceAccount:bgit-broker@example-test-123456.iam.gserviceaccount.com"},
		{match: "--service-account bgit-broker@example-test-123456.iam.gserviceaccount.com", touch: deployMarker},
		{match: "iam service-accounts add-iam-policy-binding bgit-broker@example-test-123456.iam.gserviceaccount.com"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	configPath := filepath.Join(home, ".bgit", "config")
	var stdout bytes.Buffer
	err := setupCommand(nilContext{}, config{}, []string{"--provider", "gcp", "--profile", "new", "--config", configPath, "--key", pubKey, "--no-agent", "--region", "europe-west1"}, strings.NewReader("\n\n\x04 \x04"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "create new gcp profile") || !strings.Contains(stdout.String(), "Profile name") || !strings.Contains(stdout.String(), "[new") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	cfg, err := readGlobalConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.GCPProfiles) != 1 || cfg.GCPProfiles[0].Name != "new" {
		t.Fatalf("cfg = %#v", cfg)
	}
}

func TestEnsureGcloudSetupAuthRunsLoginOnReauth(t *testing.T) {
	bin := t.TempDir()
	authMarker := filepath.Join(t.TempDir(), "authed")
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "auth print-access-token", stdout: "token", missingStdout: "ERROR: Reauthentication failed. Please run: gcloud auth login", requireFile: authMarker, exitCode: 1},
		{match: "auth login --configuration work --no-launch-browser", stdout: "https://example.test/oauth", touch: authMarker},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	err := ensureGcloudSetupAuth(context.Background(), config{gcloudConfiguration: "work"}, true, strings.NewReader("code\n"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Open the URL printed by gcloud") ||
		!strings.Contains(stdout.String(), "https://example.test/oauth") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestEnsureGcloudSetupAuthYesModeDoesNotLaunchLogin(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "auth print-access-token", stdout: "ERROR: Reauthentication failed. Please run: gcloud auth login", exitCode: 1},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := ensureGcloudSetupAuth(context.Background(), config{gcloudConfiguration: "work"}, false, strings.NewReader(""), ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "gcloud auth login --configuration work --no-launch-browser") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureGcloudSetupProjectAccessRunsLoginOnUserProjectDenied(t *testing.T) {
	bin := t.TempDir()
	authMarker := filepath.Join(t.TempDir(), "authed")
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "config get-value project", stdout: "hurozo"},
		{match: "projects describe hurozo", stdout: "hurozo", missingStdout: "ERROR: USER_PROJECT_DENIED Caller does not have required permission", requireFile: authMarker, exitCode: 1},
		{match: "auth login --configuration default --no-launch-browser", stdout: "https://example.test/oauth", touch: authMarker},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	err := ensureGcloudSetupProjectAccess(context.Background(), config{gcloudConfiguration: "default"}, true, strings.NewReader("code\n"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "needs authentication or a different account") ||
		!strings.Contains(stdout.String(), "https://example.test/oauth") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestEnsureGcloudSetupProjectAccessRepairsQuotaProject(t *testing.T) {
	bin := t.TempDir()
	quotaMarker := filepath.Join(t.TempDir(), "quota")
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "config get-value project", stdout: "hurozo"},
		{match: "projects describe hurozo", stdout: "hurozo", missingStdout: "ERROR: USER_PROJECT_DENIED Caller does not have required permission to use project aafje-490407", requireFile: quotaMarker, exitCode: 1},
		{match: "config get-value billing/quota_project", stdout: "aafje-490407"},
		{match: "config set billing/quota_project hurozo", touch: quotaMarker},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	err := ensureGcloudSetupProjectAccess(context.Background(), config{gcloudConfiguration: "default"}, true, strings.NewReader("\n"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "uses quota project aafje-490407") ||
		!strings.Contains(stdout.String(), "Set quota project to hurozo now?") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestEnsureGcloudSetupProjectAccessSelectsExistingProjectWhenUnset(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "config get-value project"},
		{match: "config get-value account", stdout: "dennis@example.com"},
		{match: "projects list", stdout: "hurozo Hurozo"},
		{match: "config set project hurozo"},
		{match: "config set billing/quota_project hurozo"},
		{match: "projects describe hurozo", stdout: "hurozo"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	err := ensureGcloudSetupProjectAccess(context.Background(), config{gcloudConfiguration: "dennis"}, true, strings.NewReader("1\n"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "has no project configured") ||
		!strings.Contains(stdout.String(), "1. hurozo - Hurozo") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestEnsureGcloudSetupProjectAccessCreatesProjectWhenUnset(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "config get-value project"},
		{match: "config get-value account", stdout: "dennis@example.com"},
		{match: "projects list"},
		{match: "projects create bgit-test"},
		{match: "config set project bgit-test"},
		{match: "config set billing/quota_project bgit-test"},
		{match: "projects describe bgit-test", stdout: "bgit-test"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	err := ensureGcloudSetupProjectAccess(context.Background(), config{gcloudConfiguration: "dennis"}, true, strings.NewReader("create\nbgit-test\nBucketGit Test\n"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "New project ID:") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestEnsureGcloudSetupProjectAccessEnablesAPIsForFreshProject(t *testing.T) {
	bin := t.TempDir()
	apiMarker := filepath.Join(t.TempDir(), "apis")
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "config get-value project"},
		{match: "config get-value account", stdout: "dennis@example.com"},
		{match: "projects list"},
		{match: "projects create bgittest"},
		{match: "config set project bgittest"},
		{match: "config set billing/quota_project bgittest"},
		{match: "projects describe bgittest", stdout: "bgittest", missingStdout: "ERROR: SERVICE_DISABLED Cloud Resource Manager API has not been used in project bgittest before or it is disabled.", requireFile: apiMarker, exitCode: 1},
		{match: "services enable serviceusage.googleapis.com cloudresourcemanager.googleapis.com --project bgittest", touch: apiMarker},
		{match: "services list --enabled --project=bgittest", stdout: "serviceusage.googleapis.com cloudresourcemanager.googleapis.com"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	err := ensureGcloudSetupProjectAccess(context.Background(), config{gcloudConfiguration: "dennis"}, true, strings.NewReader("create\nbgittest\n\n"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "enabling required GCP project APIs for bgittest") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestEnsureGcloudSetupProjectAccessCreatesProjectWithSuffixOnCollision(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "config get-value project"},
		{match: "config get-value account", stdout: "dennis@example.com"},
		{match: "projects list"},
		{match: "projects create bgit-test ", stdout: "ERROR: project ID already in use", exitCode: 1},
		{match: "projects create bgit-test-"},
		{match: "config set project bgit-test-"},
		{match: "config set billing/quota_project bgit-test-"},
		{match: "projects describe bgit-test-", stdout: "bgit-test"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	err := ensureGcloudSetupProjectAccess(context.Background(), config{gcloudConfiguration: "dennis"}, true, strings.NewReader("create\nbgit-test\n\n"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Project display name [bgit-test]:") ||
		!strings.Contains(stdout.String(), "Project ID bgit-test is already in use; trying bgit-test-") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestEnsureGcloudSetupProjectAccessCreatesShortProjectWithSuffix(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "config get-value project"},
		{match: "config get-value account", stdout: "dennis@example.com"},
		{match: "projects list"},
		{match: "projects create demo-"},
		{match: "config set project demo-"},
		{match: "config set billing/quota_project demo-"},
		{match: "projects describe demo-", stdout: "demo"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	err := ensureGcloudSetupProjectAccess(context.Background(), config{gcloudConfiguration: "dennis"}, true, strings.NewReader("create\ndemo\n\n"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Project display name [demo]:") ||
		!strings.Contains(stdout.String(), "Project ID demo is not a valid GCP project ID; trying demo-") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestEnsureGcloudSetupBillingLinksSelectedAccount(t *testing.T) {
	bin := t.TempDir()
	billingMarker := filepath.Join(t.TempDir(), "billing")
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "billing projects describe bgittest", stdout: "True", missingStdout: "False", requireFile: billingMarker, exitCode: 1},
		{match: "billing accounts list", stdout: "billingAccounts/123 Hurozo Billing true"},
		{match: "billing projects link bgittest --billing-account billingAccounts/123", touch: billingMarker},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	err := ensureGcloudSetupBilling(context.Background(), config{gcloudConfiguration: "dennis"}, "bgittest", true, strings.NewReader("1\n"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "does not have billing enabled") ||
		!strings.Contains(stdout.String(), "linking GCP project bgittest to billing account billingAccounts/123") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestEnsureGcloudSetupBillingEnablesCloudBillingAPI(t *testing.T) {
	bin := t.TempDir()
	apiMarker := filepath.Join(t.TempDir(), "billing-api")
	billingMarker := filepath.Join(t.TempDir(), "billing")
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "billing projects describe bgittest --configuration dennis --quiet --format=value(billingEnabled)", stdout: "True", onlyIfFile: billingMarker},
		{match: "billing projects describe bgittest --configuration dennis --quiet --format=value(billingEnabled)", stdout: "False", missingStdout: "ERROR: SERVICE_DISABLED Cloud Billing API has not been used in project bgittest before or it is disabled.", requireFile: apiMarker, exitCode: 1},
		{match: "services enable cloudbilling.googleapis.com --project bgittest", touch: apiMarker},
		{match: "services list --enabled --project=bgittest", stdout: "cloudbilling.googleapis.com"},
		{match: "billing accounts list --configuration dennis --quiet", stdout: "billingAccounts/123 Hurozo Billing true"},
		{match: "billing projects link bgittest --billing-account billingAccounts/123", touch: billingMarker},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	err := ensureGcloudSetupBilling(context.Background(), config{gcloudConfiguration: "dennis"}, "bgittest", true, strings.NewReader("1\n"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "enabling required GCP project APIs for bgittest") ||
		!strings.Contains(stdout.String(), "linking GCP project bgittest to billing account billingAccounts/123") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestEnsureGcloudSetupBillingYesModeReturnsLinkCommand(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "billing projects describe bgittest", stdout: "False"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := ensureGcloudSetupBilling(context.Background(), config{gcloudConfiguration: "dennis"}, "bgittest", false, strings.NewReader(""), ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "gcloud billing projects link bgittest --billing-account BILLING_ACCOUNT --configuration dennis") {
		t.Fatalf("err = %v", err)
	}
}

func TestLinkGcloudSetupBillingAccountReportsQuotaExceeded(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "billing projects link bgittest --billing-account billingAccounts/123", stdout: "ERROR: Cloud billing quota exceeded: https://support.google.com/code/contact/billing_quota_increase", exitCode: 1},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	err := linkGcloudSetupBillingAccount(context.Background(), config{gcloudConfiguration: "dennis"}, "bgittest", "billingAccounts/123", &stdout)
	if err == nil || !strings.Contains(err.Error(), "billing quota exceeded") ||
		!strings.Contains(err.Error(), "choose a different billing account") {
		t.Fatalf("err = %v", err)
	}
}

func TestGcloudSetupProjectIDWithSuffixTruncatesBase(t *testing.T) {
	got := gcloudSetupProjectIDWithSuffix("very-long-project-id-base", "1234567")
	if got != "very-long-project-id-b-1234567" {
		t.Fatalf("project ID = %q", got)
	}
}

func TestGcloudIAMBindingRetryableDetectsServiceAccountPropagation(t *testing.T) {
	if !gcloudIAMBindingRetryable("INVALID_ARGUMENT: Service account bgit-broker@project.iam.gserviceaccount.com does not exist.", errors.New("exit status 1")) {
		t.Fatal("service account propagation error should be retryable")
	}
	if gcloudIAMBindingRetryable("PERMISSION_DENIED: permission denied", errors.New("exit status 1")) {
		t.Fatal("permission denied should not be retryable")
	}
}

func TestBrokerDeleteAWSDeletesStackAndClearsConfig(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".bgit", "config")
	if err := writeGlobalConfig(configPath, globalConfig{
		Version: globalConfigVersion,
		AWSProfiles: []globalAWSProfile{{
			Name:      "prod",
			AccountID: "123456789012",
			Regions: []globalProfileRegion{{
				Name:          "eu-west-1",
				BrokerURL:     "https://broker.example.test",
				BrokerVersion: brokerVersion,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeFakeCLI(t, bin, "aws", []fakeCLIAction{
		{match: "cloudformation delete-stack --stack-name bgit-broker --region eu-west-1"},
		{match: "cloudformation wait stack-delete-complete --stack-name bgit-broker --region eu-west-1"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	if err := brokerCommand(nilContext{}, config{}, []string{"delete", "--provider", "aws", "--profile", "prod", "--region", "eu-west-1", "--config", configPath, "--yes"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	cfg, err := readGlobalConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AWSProfiles[0].Regions) != 0 {
		t.Fatalf("profile not cleared = %#v", cfg.AWSProfiles[0])
	}
	if !strings.Contains(stdout.String(), "deleted AWS bgit broker") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestBrokerDeleteGCPDeletesFunctionAndOptionalData(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	configPath := filepath.Join(home, ".bgit", "config")
	bin := t.TempDir()
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "functions delete bgit-broker --gen2 --region europe-west1 --quiet"},
		{match: "run services delete bgit-broker --region europe-west1 --quiet"},
		{match: "firestore databases delete --database=bgit --quiet"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	if err := brokerCommand(nilContext{}, config{}, []string{"delete", "--provider", "gcp", "--profile", "work", "--region", "europe-west1", "--data", "--config", configPath, "--yes"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "deleted GCP bgit broker") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}
