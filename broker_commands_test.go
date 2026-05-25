package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrokerInitWritesBrokerGitConfig(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/get" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var req brokerRepoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Repo.Logical != "app.git" {
			t.Fatalf("logical repo = %q", req.Repo.Logical)
		}
		if req.Repo.TeamID != coreTeamID {
			t.Fatalf("team = %q", req.Repo.TeamID)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	configPath := filepath.Join(root, ".bgit", "config")
	if err := writeGlobalConfig(configPath, globalConfig{
		Version: globalConfigVersion,
		GCPProfiles: []globalGCPProfile{{
			Name:      "work",
			ProjectID: "project-id",
			Regions: []globalProfileRegion{{
				Name:      "europe-west1",
				BrokerURL: server.URL,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "app")
	var stdout bytes.Buffer
	err := brokerInitCommand([]string{"--noninteractive", "--repo", "app", target, "--profile", "gcp:work/europe-west1", "--team", "core", "--config", configPath}, strings.NewReader(""), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"bucketgit.broker":      server.URL,
		"bucketgit.profile":     "gcp:work/europe-west1",
		"bucketgit.region":      "europe-west1",
		"bucketgit.team":        coreTeamID,
		"bucketgit.logicalRepo": "app.git",
		"branch.main.remote":    "origin",
		"branch.main.merge":     "refs/heads/main",
		"core.autocrlf":         "false",
		"core.eol":              "lf",
	} {
		out, err := runGit(target, "config", "--get", key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if strings.TrimSpace(string(out)) != want {
			t.Fatalf("%s = %q, want %q", key, strings.TrimSpace(string(out)), want)
		}
	}
	out, err := runGit(target, "config", "--get", "core.sshCommand")
	if err != nil {
		t.Fatalf("core.sshCommand: %v", err)
	}
	if got := strings.TrimSpace(string(out)); !strings.HasSuffix(got, " ssh") {
		t.Fatalf("core.sshCommand = %q", got)
	}
	if got := strings.TrimSpace(string(out)); !strings.HasPrefix(got, "'") {
		t.Fatalf("core.sshCommand should quote executable path, got %q", got)
	}
	remote, err := runGit(target, "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(remote)) != "git@git.bucketgit.com:app.git" {
		t.Fatalf("origin = %q", strings.TrimSpace(string(remote)))
	}
}

func TestShellQuoteForGitSSHCommand(t *testing.T) {
	got := shellQuoteForGitSSHCommand(`D:\a\bgit\bgit\bgit.exe`)
	if got != `'D:\a\bgit\bgit\bgit.exe'` {
		t.Fatalf("quoted windows path = %q", got)
	}
	got = shellQuoteForGitSSHCommand(`/tmp/BucketGit Test/bin/bgit`)
	if got != `'/tmp/BucketGit Test/bin/bgit'` {
		t.Fatalf("quoted unix path = %q", got)
	}
	got = shellQuoteForGitSSHCommand(`/tmp/dennis' test/bgit`)
	if got != `'/tmp/dennis'\'' test/bgit'` {
		t.Fatalf("quoted apostrophe path = %q", got)
	}
}

func TestInitBrokerWorktreeOmitsIdentityWhenUnset(t *testing.T) {
	target := filepath.Join(t.TempDir(), "app")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/get" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	err := initBrokerWorktree(target, "app", brokerProfile{
		Provider:      "gcs",
		QualifiedName: "broker:https://broker.example.test",
		BrokerURL:     server.URL,
		TeamID:        coreTeamID,
	}, "", "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runGit(target, "config", "--local", "--get", "user.name"); err == nil {
		t.Fatalf("user.name should not be set, got %q", strings.TrimSpace(string(out)))
	}
	if out, err := runGit(target, "config", "--local", "--get", "user.email"); err == nil {
		t.Fatalf("user.email should not be set, got %q", strings.TrimSpace(string(out)))
	}
}

func TestBoardCommandCreatesAndMovesStories(t *testing.T) {
	var createReq brokerIssueRequest
	var moveReq brokerIssueRequest
	target, server, requests := setupBrokerCommandTestRepo(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/issues/create":
			if err := json.NewDecoder(r.Body).Decode(&createReq); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"issue":{"id":7,"type":"story","title":"Ship board","lane":"backlog"}}`))
		case "/issues/move":
			if err := json.NewDecoder(r.Body).Decode(&moveReq); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})
	defer server.Close()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := boardCommand([]string{"create", "As", "a", "maintainer,", "I", "want", "to", "track", "stories,", "so", "that", "work", "is", "visible."}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	wantStory := "As a maintainer, I want to track stories, so that work is visible."
	if createReq.Type != "story" || createReq.Title != wantStory || createReq.Body != wantStory || createReq.Lane != "backlog" {
		t.Fatalf("create req = %#v", createReq)
	}
	if !strings.Contains(stdout.String(), "created story AP-7") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	stdout.Reset()
	if err := boardCommand([]string{"move", "AP-7", "review"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	if moveReq.ID != 7 || moveReq.Lane != "review" {
		t.Fatalf("move req = %#v", moveReq)
	}
	if got := strings.Join(*requests, ","); got != "/issues/create,/issues/move" {
		t.Fatalf("requests = %s", got)
	}
}

func TestBoardCommandListsStoriesByLane(t *testing.T) {
	target, server, _ := setupBrokerCommandTestRepo(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/issues/list" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"issues":[{"id":1,"type":"story","title":"Backlog item","lane":"backlog"},{"id":2,"type":"story","title":"Needs review","lane":"review","assignee":"ada"},{"id":3,"type":"issue","title":"Bug","lane":"doing"}]}`))
	})
	defer server.Close()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := boardCommand([]string{"list"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{"backlog\n", "AP-1\tBacklog item", "review\n", "AP-2\tNeeds review\t@ada"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Bug") {
		t.Fatalf("issue leaked into board list:\n%s", out)
	}
}

func TestSortedBoardAssigneesDeduplicatesCaseInsensitive(t *testing.T) {
	got := sortedBoardAssignees([]string{"zoe", "Ada", "ada", "", "mike"})
	want := []string{"Ada", "mike", "zoe"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("assignees = %#v, want %#v", got, want)
	}
}

func TestRepoMonogramAndStoryDisplayID(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "app.git", want: "AP"},
		{name: "bucket-git", want: "BG"},
		{name: "team/bucketgit.git", want: "BU"},
		{name: "bucket_git_web", want: "BGW"},
	} {
		if got := repoMonogram(tc.name); got != tc.want {
			t.Fatalf("repoMonogram(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := storyDisplayID("BG", 42); got != "BG-42" {
		t.Fatalf("storyDisplayID = %q", got)
	}
}

func TestUniqueUserMonogramsDisambiguateCollisions(t *testing.T) {
	got := uniqueUserMonograms([]string{"dennis", "denise", "deborah", "derek", "derek1", "derek2"})
	want := map[string]string{
		"deborah": "DEB",
		"denise":  "DE3",
		"dennis":  "DE4",
		"derek":   "DER",
		"derek1":  "DE1",
		"derek2":  "DE2",
	}
	for user, wantLabel := range want {
		if got[user] != wantLabel {
			t.Fatalf("monogram for %s = %q, want %q (all: %#v)", user, got[user], wantLabel, got)
		}
	}
}

func TestUniqueUserMonogramsPreferNumericUsernameSuffix(t *testing.T) {
	users := make([]string, 0, 15)
	for i := 1; i <= 15; i++ {
		users = append(users, fmt.Sprintf("prosus-user-%d", i))
	}
	got := uniqueUserMonograms(users)
	for i := 1; i <= 9; i++ {
		user := fmt.Sprintf("prosus-user-%d", i)
		want := fmt.Sprintf("PR%d", i)
		if got[user] != want {
			t.Fatalf("monogram for %s = %q, want %q (all: %#v)", user, got[user], want, got)
		}
	}
	for i := 10; i <= 15; i++ {
		user := fmt.Sprintf("prosus-user-%d", i)
		want := fmt.Sprintf("P%d", i)
		if got[user] != want {
			t.Fatalf("monogram for %s = %q, want %q (all: %#v)", user, got[user], want, got)
		}
	}
}

func TestUniqueUserMonogramsCounterFallbackStartsAtOne(t *testing.T) {
	got := uniqueUserMonograms([]string{"aaab", "aaac", "aaad"})
	want := map[string]string{
		"aaab": "AA1",
		"aaac": "AA2",
		"aaad": "AA3",
	}
	for user, wantLabel := range want {
		if got[user] != wantLabel {
			t.Fatalf("monogram for %s = %q, want %q (all: %#v)", user, got[user], wantLabel, got)
		}
	}
}

func TestParseBoardStoryIDArgRequiresRepoPrefix(t *testing.T) {
	got, err := parseBoardStoryIDArg([]string{"move", "AP-7"}, "AP")
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("id = %d, want 7", got)
	}
	if _, err := parseBoardStoryIDArg([]string{"move", "7"}, "AP"); err == nil {
		t.Fatal("expected bare board story id to be rejected")
	}
}

func TestBrokerUpgradeTargetUsesCurrentRepoProfile(t *testing.T) {
	global := globalConfig{GCPProfiles: []globalGCPProfile{{
		Name: "work",
		Regions: []globalProfileRegion{{
			Name:      "europe-west1",
			BrokerURL: "https://broker.example.test",
		}},
	}}}
	got, err := brokerUpgradeTargetForCurrentRepo(config{
		brokerURL:           "https://broker.example.test",
		gcloudConfiguration: "gcp:work/europe-west1",
		region:              "europe-west1",
	}, global)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "gcs" || got.Name != "work" || got.Region != "europe-west1" || got.BrokerURL != "https://broker.example.test" {
		t.Fatalf("target = %#v", got)
	}
}

func TestBrokerUpgradeTargetFallsBackToBrokerURL(t *testing.T) {
	global := globalConfig{AWSProfiles: []globalAWSProfile{{
		Name: "prod",
		Regions: []globalProfileRegion{{
			Name:      "us-east-1",
			BrokerURL: "https://abc.lambda-url.us-east-1.on.aws/",
		}},
	}}}
	got, err := brokerUpgradeTargetForCurrentRepo(config{brokerURL: "https://abc.lambda-url.us-east-1.on.aws"}, global)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "s3" || got.Name != "prod" || got.Region != "us-east-1" {
		t.Fatalf("target = %#v", got)
	}
}

func TestBrokerInitNoninteractiveRequiresProfileAndRepo(t *testing.T) {
	err := brokerInitCommand([]string{"--noninteractive", "--repo", "app"}, strings.NewReader(""), ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "requires --profile") {
		t.Fatalf("err = %v", err)
	}
	err = brokerInitCommand([]string{"--noninteractive", "--profile", "work"}, strings.NewReader(""), ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "requires --repo") {
		t.Fatalf("err = %v", err)
	}
	err = brokerInitCommand([]string{"--noninteractive", "--profile", "work", "--repo", "app"}, strings.NewReader(""), ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "requires --team") {
		t.Fatalf("err = %v", err)
	}
}

func TestAdminKeysListUsesLogicalBrokerRepo(t *testing.T) {
	target, server, _ := setupBrokerCommandTestRepo(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys/list" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var req brokerRepoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Repo.Logical != "app.git" {
			t.Fatalf("logical repo = %q", req.Repo.Logical)
		}
		_, _ = w.Write([]byte(`{"keys":[{"user":"owner","role":"owner","public_key":"ssh-ed25519 AAAA owner"}]}`))
	})
	defer server.Close()

	var stdout bytes.Buffer
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(previous)

	if err := brokerAdminKeysCommand(config{}, []string{"list"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "owner\towner\tactive") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestInviteUserPreservesTeamScopedRepo(t *testing.T) {
	var got brokerOwnerTransferRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys/invite/create" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"accept_command":"bgit admin accept-invite bgitinv_test"}`))
	}))
	defer server.Close()

	var stdout bytes.Buffer
	if err := brokerInviteUserCommand(config{provider: "s3"}, []string{"--broker", server.URL, "--team", "t_marketing", "--user", "owner", "--role", "read", "mkt"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if got.Repo.Logical != "mkt.git" || got.Repo.TeamID != "t_marketing" || got.Repo.Provider != "s3" {
		t.Fatalf("repo = %#v", got.Repo)
	}
	if got.User != "owner" || got.Role != "read" {
		t.Fatalf("request = %#v", got)
	}
}

func TestPrintBrokerUsersUsesReadableColumns(t *testing.T) {
	var stdout bytes.Buffer
	printBrokerUsers(&stdout, []brokerUserInfo{{
		ID:         "u_owner",
		Username:   "owner",
		BrokerRole: "owner",
		Keys:       []brokerKey{{PublicKey: "ssh-ed25519 AAAA owner"}},
	}, {
		ID:         "u_pending",
		Username:   "pending",
		BrokerRole: "user",
		Pending:    true,
	}})
	out := stdout.String()
	for _, want := range []string{"ID", "Username", "Role", "Status", "u_owner", "owner", "active", "u_pending", "pending"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "\t") {
		t.Fatalf("output should not contain tabs: %q", out)
	}
}

func TestTopLevelBrokerInitForwardsGlobalProfile(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/get" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	configPath := filepath.Join(root, ".bgit", "config")
	if err := writeGlobalConfig(configPath, globalConfig{
		Version: globalConfigVersion,
		GCPProfiles: []globalGCPProfile{{
			Name:      "work",
			ProjectID: "project-id",
			Regions: []globalProfileRegion{{
				Name:      "europe-west1",
				BrokerURL: server.URL,
			}},
		}},
		AWSProfiles: []globalAWSProfile{{
			Name:      "prod",
			AccountID: "123456789012",
			Regions: []globalProfileRegion{{
				Name:      "eu-west-1",
				BrokerURL: "https://aws-broker.example.test",
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "app")
	var stdout bytes.Buffer
	err := run([]string{"init", "--noninteractive", "--repo", "app", target, "--config", configPath, "--profile", "gcp:work/europe-west1", "--team", "core"}, strings.NewReader(""), &stdout, ioDiscard{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runGit(target, "config", "--get", "bucketgit.profile")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "gcp:work/europe-west1" {
		t.Fatalf("profile = %q", strings.TrimSpace(string(out)))
	}
}

func TestBrokerProfileAmbiguousBareNameSuggestsRegionQualifiedNames(t *testing.T) {
	profiles := []brokerProfile{{
		Provider:      "gcs",
		Name:          "work",
		Region:        "us-central1",
		QualifiedName: "gcp:work/us-central1",
		BrokerURL:     "https://us.example.test",
	}, {
		Provider:      "gcs",
		Name:          "work",
		Region:        "europe-west1",
		QualifiedName: "gcp:work/europe-west1",
		BrokerURL:     "https://eu.example.test",
	}}
	_, err := selectBrokerProfileForCommand(profiles, "work", "", "bgit push")
	if err == nil {
		t.Fatal("expected ambiguous profile error")
	}
	if !strings.Contains(err.Error(), `broker profile "work" is ambiguous`) ||
		!strings.Contains(err.Error(), "bgit push --profile work.us-central1") ||
		!strings.Contains(err.Error(), "bgit push --profile work.europe-west1") {
		t.Fatalf("err = %v", err)
	}
}

func TestBrokerProfileBareNameWithRegionSelectsProfile(t *testing.T) {
	profiles := []brokerProfile{{
		Provider:      "gcs",
		Name:          "work",
		Region:        "us-central1",
		QualifiedName: "gcp:work/us-central1",
		BrokerURL:     "https://us.example.test",
	}, {
		Provider:      "gcs",
		Name:          "work",
		Region:        "europe-west1",
		QualifiedName: "gcp:work/europe-west1",
		BrokerURL:     "https://eu.example.test",
	}}
	got, err := selectBrokerProfileForCommand(profiles, "work", "europe-west1", "bgit push")
	if err != nil {
		t.Fatal(err)
	}
	if got.Region != "europe-west1" || got.BrokerURL != "https://eu.example.test" {
		t.Fatalf("profile = %#v", got)
	}
}

func TestExplicitBrokerProfileSelectionUsesRegionForDataPlaneCommand(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	configPath := filepath.Join(home, ".bgit", "config.yaml")
	if err := writeGlobalConfig(configPath, globalConfig{
		Version: globalConfigVersion,
		GCPProfiles: []globalGCPProfile{{
			Name: "work",
			Regions: []globalProfileRegion{{
				Name:      "us-central1",
				BrokerURL: "https://us.example.test",
			}, {
				Name:      "europe-west1",
				BrokerURL: "https://eu.example.test",
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config{gcloudConfiguration: "work", gcloudConfigurationExplicit: true, region: "europe-west1"}
	if err := applyExplicitBrokerProfileSelection(&cfg, "push"); err != nil {
		t.Fatal(err)
	}
	if cfg.brokerURL != "https://eu.example.test" || cfg.gcloudConfiguration != "gcp:work/europe-west1" {
		t.Fatalf("cfg = %#v", cfg)
	}
}

func TestExplicitBrokerProfileSelectionRejectsAmbiguousDataPlaneProfile(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	configPath := filepath.Join(home, ".bgit", "config.yaml")
	if err := writeGlobalConfig(configPath, globalConfig{
		Version: globalConfigVersion,
		GCPProfiles: []globalGCPProfile{{
			Name: "work",
			Regions: []globalProfileRegion{{
				Name:      "us-central1",
				BrokerURL: "https://us.example.test",
			}, {
				Name:      "europe-west1",
				BrokerURL: "https://eu.example.test",
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config{gcloudConfiguration: "work", gcloudConfigurationExplicit: true}
	err := applyExplicitBrokerProfileSelection(&cfg, "push")
	if err == nil {
		t.Fatal("expected ambiguous profile error")
	}
	if !strings.Contains(err.Error(), "bgit push --profile work.us-central1") ||
		!strings.Contains(err.Error(), "bgit push --profile work.europe-west1") {
		t.Fatalf("err = %v", err)
	}
}

func TestBrokerProfileDotRegionSelectsProfile(t *testing.T) {
	profiles := []brokerProfile{{
		Provider:      "s3",
		Name:          "prod",
		Region:        "us-east-1",
		QualifiedName: "aws:prod/us-east-1",
		BrokerURL:     "https://us.example.test",
	}, {
		Provider:      "s3",
		Name:          "prod",
		Region:        "eu-west-1",
		QualifiedName: "aws:prod/eu-west-1",
		BrokerURL:     "https://eu.example.test",
	}}
	got, err := selectBrokerProfileForCommand(profiles, "prod.eu-west-1", "", "bgit push")
	if err != nil {
		t.Fatal(err)
	}
	if got.Region != "eu-west-1" {
		t.Fatalf("profile = %#v", got)
	}
}

func TestParseBrokerCloneURL(t *testing.T) {
	brokerURL, repo, teamName, ok, err := parseBrokerCloneURL("https://broker.example.test/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || brokerURL != "https://broker.example.test" || repo != "app.git" || teamName != "" {
		t.Fatalf("brokerURL=%q repo=%q team=%q ok=%v", brokerURL, repo, teamName, ok)
	}
	brokerURL, repo, teamName, ok, err = parseBrokerCloneURL("https://broker.example.test/core/app/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || brokerURL != "https://broker.example.test" || repo != "app.git" || teamName != "core" {
		t.Fatalf("brokerURL=%q repo=%q team=%q ok=%v", brokerURL, repo, teamName, ok)
	}
}

func TestLogicalRepoNamesMustBeFlat(t *testing.T) {
	for _, name := range []string{"team/app", "team/app.git", `team\app`} {
		if _, err := normalizeLogicalRepoName(name); err == nil {
			t.Fatalf("normalizeLogicalRepoName(%q) succeeded", name)
		}
	}
	if _, _, _, _, err := parseBrokerCloneURL("https://broker.example.test/team/other/app.git"); err == nil {
		t.Fatal("parseBrokerCloneURL accepted mismatched team repo path")
	}
}

func TestDiscoverBrokerCloneURLUsesTXTTeamName(t *testing.T) {
	oldLookup := lookupTXT
	lookupTXT = func(name string) ([]string, error) {
		if name != "_bgit.git.example.com" {
			t.Fatalf("lookup name = %q", name)
		}
		return []string{`v=bgit1 broker=https://broker.example.test team=t_abc123 name=teamfoobar`}, nil
	}
	defer func() { lookupTXT = oldLookup }()

	brokerURL, repo, teamID, ok, err := discoverBrokerCloneURL("https://git.example.com/teamfoobar/repo/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || brokerURL != "https://broker.example.test" || repo != "repo.git" || teamID != "t_abc123" {
		t.Fatalf("brokerURL=%q repo=%q teamID=%q ok=%v", brokerURL, repo, teamID, ok)
	}

	_, _, _, ok, err = discoverBrokerCloneURL("https://git.example.com/teamfoobar/other/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("discovered broker for mismatched GitHub-style repo path")
	}
}

func TestDiscoverBrokerCloneURLSkipsDirectBrokerHosts(t *testing.T) {
	oldLookup := lookupTXT
	lookupTXT = func(name string) ([]string, error) {
		t.Fatalf("unexpected TXT lookup %q", name)
		return nil, nil
	}
	defer func() { lookupTXT = oldLookup }()

	_, _, _, ok, err := discoverBrokerCloneURL("https://service.run.app/teamfoobar/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("discovered broker for direct Cloud Run host")
	}
}

func TestBrokerProfileForCloneURL(t *testing.T) {
	profile, err := brokerProfileForCloneURL("https://broker.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	if profile.BrokerURL != "https://broker.example.test" ||
		profile.QualifiedName != "broker:https://broker.example.test" ||
		profile.Provider != "gcs" {
		t.Fatalf("profile = %#v", profile)
	}
}

func TestBrokerInitInteractivePromptsForRepoAndProfile(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/get" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	configPath := filepath.Join(root, ".bgit", "config")
	if err := writeGlobalConfig(configPath, globalConfig{
		Version: globalConfigVersion,
		AWSProfiles: []globalAWSProfile{{
			Name:      "prod",
			AccountID: "123456789012",
			Regions: []globalProfileRegion{{
				Name:      "eu-west-1",
				BrokerURL: server.URL,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "repo")
	var stdout bytes.Buffer
	err := brokerInitCommand([]string{"--config", configPath, "--profile", "aws:prod/eu-west-1", "--team", "core", "ignored", target}, strings.NewReader("\x04"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	out, err := runGit(target, "config", "--get", "bucketgit.profile")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "aws:prod/eu-west-1" {
		t.Fatalf("profile = %q", strings.TrimSpace(string(out)))
	}
}

func TestBrokerInitInteractiveIdentityOnlyUpdatesRepoConfig(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".bgit", "config")
	if err := writeGlobalConfig(configPath, globalConfig{
		Version:  globalConfigVersion,
		Identity: globalIdentityConfig{Name: "Global User", Email: "global@example.com"},
		GCPProfiles: []globalGCPProfile{{
			Name: "work",
			Regions: []globalProfileRegion{{
				Name:      "europe-west1",
				BrokerURL: "https://broker.example.test",
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "repo")
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "bucketgit.logicalRepo", "app.git"},
		{"config", "bucketgit.profile", "gcp:work/europe-west1"},
		{"config", "user.name", "Repo User"},
		{"config", "user.email", "old@example.com"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	var stdout bytes.Buffer
	input := strings.NewReader("\x1b[B\x1b[B\n" + strings.Repeat("\x7f", len("old@example.com")) + "new@example.com\n\x04")
	err := brokerInitCommand([]string{"--config", configPath, "--repo", "app.git", target}, input, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	out, err := runGit(target, "config", "--get", "user.email")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "new@example.com" {
		t.Fatalf("user.email = %q", strings.TrimSpace(string(out)))
	}
	if !strings.Contains(stdout.String(), "Updated repository identity.") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestInitDialogRendersRepoInputAndProfiles(t *testing.T) {
	rendered := renderInitDialogWithStyle(initDialogState{
		repoName: "app.git",
		profiles: []brokerProfile{{
			Provider:      "gcs",
			Name:          "work",
			Region:        "europe-west1",
			QualifiedName: "gcp:work/europe-west1",
			BrokerURL:     "https://broker.example.test",
		}},
		selectedProfile: 0,
	}, false)
	for _, want := range []string{"BUCKETGIT INIT", "Repository [app.git", "[x] gcp:work/europe-west1", "[ OK ]"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered missing %q:\n%s", want, rendered)
		}
	}
}

func TestInitDialogRendersIdentityAndDefaultWarning(t *testing.T) {
	rendered := renderInitDialogWithStyle(initDialogState{
		repoName:             "app.git",
		identityName:         defaultBucketGitIdentityName,
		identityEmail:        defaultBucketGitIdentityEmail(),
		initialIdentityName:  defaultBucketGitIdentityName,
		initialIdentityEmail: defaultBucketGitIdentityEmail(),
		profiles:             nil,
		selectedProfile:      -1,
		initialProfile:       -1,
		editingField:         -1,
	}, false)
	for _, want := range []string{"Identity", "Name  [BucketGit Client", "Email [", "Configure name/email with bgit setup or bgit config."} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered missing %q:\n%s", want, rendered)
		}
	}
}

func TestInitDialogExistingNoChangesReturnsUnchanged(t *testing.T) {
	state := initDialogState{
		repoName:             "app.git",
		initialRepoName:      "app.git",
		profileName:          "gcp:work/europe-west1",
		initialProfileName:   "gcp:work/europe-west1",
		identityName:         "Dennis Example",
		initialIdentityName:  "Dennis Example",
		identityEmail:        "dennis@example.com",
		initialIdentityEmail: "dennis@example.com",
		existing:             true,
		profiles:             []brokerProfile{{QualifiedName: "gcp:work/europe-west1"}},
		selectedProfile:      0,
		initialProfile:       0,
	}
	result, ok := state.deploy()
	if !ok {
		t.Fatal("deploy rejected unchanged existing config")
	}
	if result.Changed {
		t.Fatalf("changed = true: %#v", result)
	}
}

func TestInitDialogFreshNoProfileNoChangesReturnsUnchanged(t *testing.T) {
	state := initDialogState{
		repoName:             "foo.git",
		initialRepoName:      "foo.git",
		identityName:         "Dennis Vink",
		initialIdentityName:  "Dennis Vink",
		identityEmail:        "hi@bucketgit.com",
		initialIdentityEmail: "hi@bucketgit.com",
		existing:             false,
		profiles:             []brokerProfile{{QualifiedName: "gcp:work/europe-west1"}},
		selectedProfile:      -1,
		initialProfile:       -1,
	}
	result, ok := state.deploy()
	if !ok {
		t.Fatalf("deploy rejected unchanged fresh dialog: %q", state.message)
	}
	if result.Changed {
		t.Fatalf("changed = true: %#v", result)
	}
}

func TestInitDialogExistingIdentityOnlyChangeDoesNotRequireProfile(t *testing.T) {
	state := initDialogState{
		repoName:             "app.git",
		initialRepoName:      "app.git",
		identityName:         "Dennis Example",
		initialIdentityName:  "Dennis Example",
		identityEmail:        "new@example.com",
		initialIdentityEmail: "old@example.com",
		existing:             true,
		selectedProfile:      -1,
		initialProfile:       -1,
	}
	result, ok := state.deploy()
	if !ok {
		t.Fatal("deploy rejected identity-only change")
	}
	if !result.IdentityChanged || result.ProfileChanged || result.RepoChanged {
		t.Fatalf("result = %#v", result)
	}
}

func TestInitDialogInitialStateUsesRepoThenGlobalIdentity(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "repo")
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "bucketgit.logicalRepo", "app.git"},
		{"config", "bucketgit.profile", "gcp:work/europe-west1"},
		{"config", "user.name", "Repo User"},
		{"config", "user.email", "repo@example.com"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	initial := initDialogInitialState(target, globalConfig{Identity: globalIdentityConfig{Name: "Global User", Email: "global@example.com"}}, "", "")
	if !initial.Existing || initial.RepoName != "app.git" || initial.ProfileName != "gcp:work/europe-west1" ||
		initial.IdentityName != "Repo User" || initial.IdentityEmail != "repo@example.com" {
		t.Fatalf("initial = %#v", initial)
	}
	fresh := initDialogInitialState(filepath.Join(root, "fresh"), globalConfig{Identity: globalIdentityConfig{Name: "Global User", Email: "global@example.com"}}, "new.git", "")
	if fresh.Existing || fresh.IdentityName != "Global User" || fresh.IdentityEmail != "global@example.com" {
		t.Fatalf("fresh = %#v", fresh)
	}
}

func TestInitDialogEditsRepoAndSelectsProfile(t *testing.T) {
	var stdout bytes.Buffer
	result, err := runInitDialogWithRaw(
		bufio.NewReader(strings.NewReader("\nbar\n\x1b[B\x1b[B\x1b[B \x04")),
		strings.NewReader(""),
		&stdout,
		initDialogConfig{RepoName: "foo"},
		[]brokerProfile{{
			Provider:      "gcs",
			Name:          "work",
			Region:        "europe-west1",
			QualifiedName: "gcp:work/europe-west1",
			BrokerURL:     "https://broker.example.test",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.RepoName != "foobar" || result.ProfileName != "gcp:work/europe-west1" {
		t.Fatalf("repo=%q profile=%q", result.RepoName, result.ProfileName)
	}
	if !strings.Contains(stdout.String(), "BUCKETGIT INIT") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestInitDialogEscapeRevertsRepoEdit(t *testing.T) {
	var stdout bytes.Buffer
	result, err := runInitDialogWithRaw(
		bufio.NewReader(strings.NewReader("\nbar\x1b\x1b[B\x1b[B\x1b[B \x04")),
		strings.NewReader(""),
		&stdout,
		initDialogConfig{RepoName: "foo"},
		[]brokerProfile{{
			Provider:      "gcs",
			Name:          "work",
			Region:        "europe-west1",
			QualifiedName: "gcp:work/europe-west1",
			BrokerURL:     "https://broker.example.test",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.RepoName != "foo" || result.ProfileName != "gcp:work/europe-west1" {
		t.Fatalf("repo=%q profile=%q", result.RepoName, result.ProfileName)
	}
}

func TestTopLevelPushWithoutBrokerOrBucketShowsOriginHint(t *testing.T) {
	err := run([]string{"push"}, strings.NewReader(""), &bytes.Buffer{}, ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "No configured push destination") {
		t.Fatalf("err = %v", err)
	}
}

func TestAdminCloudIAMMovedToDirect(t *testing.T) {
	err := brokerAdminCommand(config{}, []string{"grant-read", "user@example.com"}, ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "bgit direct admin") {
		t.Fatalf("err = %v", err)
	}
}

func TestAdminRepoCreateUsesCreateEndpointAndTeam(t *testing.T) {
	var got brokerRepoAdminRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/teams/resolve":
			_, _ = w.Write([]byte(`{"team":{"id":"t_marketing","name":"marketing"}}`))
		case "/repos/create":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	var stdout bytes.Buffer
	err := brokerAdminCommand(config{provider: "gcs", brokerURL: server.URL}, []string{"repo", "create", "--team", "marketing", "--role", "developer", "demo"}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo.Logical != "demo.git" || got.Repo.TeamID != "t_marketing" || got.Role != "developer" {
		t.Fatalf("request = %#v", got)
	}
	if !strings.Contains(stdout.String(), "created repository demo in team marketing") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestAdminRepoCreateAllowsOwnerTeamGrant(t *testing.T) {
	var got brokerRepoAdminRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/teams/resolve":
			_, _ = w.Write([]byte(`{"team":{"id":"t_core","name":"core"}}`))
		case "/repos/create":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	if err := brokerAdminCommand(config{provider: "gcs", brokerURL: server.URL}, []string{"repo", "create", "--team", "core", "--role", "owner", "demo"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if got.Role != "owner" || got.Repo.TeamID != "t_core" {
		t.Fatalf("request = %#v", got)
	}
}

func TestAdminTeamsRepoAddAllowsOwnerRole(t *testing.T) {
	var got brokerRepoAdminRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.URL.Path != "/repo/teams/upsert" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	cfg := config{brokerURL: server.URL, logicalRepo: "demo.git", provider: "gcs"}
	if err := brokerTeamsCommand(cfg, []string{"repo", "add", "t_core", "owner"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if got.TeamID != "t_core" || got.Role != "owner" || got.Repo.Logical != "demo.git" {
		t.Fatalf("request = %#v", got)
	}
}

func TestBrokerAdminProtectAndPRCommandsUseBroker(t *testing.T) {
	target, server, requests := setupBrokerCommandTestRepo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/protection/upsert", "/prs/merge", "/prs/close":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/protection/list":
			_, _ = w.Write([]byte(`{"protections":[{"ref":"refs/heads/main","require_pr":true,"allow_overrides":true}]}`))
		case "/prs/create":
			var req brokerPullRequestRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.PR.Source != "refs/heads/main" || req.PR.Target != "refs/heads/main" {
				t.Fatalf("create PR req = %#v", req.PR)
			}
			_, _ = w.Write([]byte(`{"pr":{"id":7,"title":"demo","source":"refs/heads/main","target":"refs/heads/main","status":"open"}}`))
		case "/prs/list":
			_, _ = w.Write([]byte(`{"prs":[{"id":7,"title":"demo","source":"refs/heads/main","target":"refs/heads/main","status":"open"}]}`))
		case "/prs/view":
			_, _ = w.Write([]byte(`{"pr":{"id":7,"title":"demo","source":"refs/heads/main","target":"refs/heads/main","status":"open","approvals":0}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})
	defer server.Close()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := brokerAdminCommand(config{}, []string{"protect", "add", "main", "--allow-owner-admin-override"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if err := brokerAdminCommand(config{}, []string{"protect", "list"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if err := prCommand([]string{"create", "--title", "demo"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	if err := prCommand([]string{"list"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	if err := prCommand([]string{"view", "7"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	if err := prCommand([]string{"merge", "7"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	if err := prCommand([]string{"close", "7"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	want := "/protection/upsert,/protection/list,/prs/create,/prs/list,/prs/view,/prs/merge,/prs/close"
	if strings.Join(*requests, ",") != want {
		t.Fatalf("requests = %#v", *requests)
	}
	if !strings.Contains(stdout.String(), "created PR #7") || !strings.Contains(stdout.String(), "refs/heads/main") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestImportGitHubKeysConfirmsAndStoresSource(t *testing.T) {
	var addReq brokerKeyRequest
	target, server, _ := setupBrokerCommandTestRepo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.URL.Path != "/keys/add" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&addReq); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	defer server.Close()

	oldTransport := http.DefaultClient.Transport
	http.DefaultClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "github.com" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ssh-ed25519 AAAAGH octocat@github\n")),
				Request:    req,
			}, nil
		}
		return http.DefaultTransport.RoundTrip(req)
	})
	defer func() { http.DefaultClient.Transport = oldTransport }()

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := brokerAdminKeysCommand(config{}, []string{"import-github", "octocat", "--role", "triage"}, strings.NewReader("y\n"), &stdout); err != nil {
		t.Fatal(err)
	}
	if addReq.User != "octocat" || addReq.Role != "triage" || addReq.Source != "github:octocat" || len(addReq.PublicKeys) != 1 {
		t.Fatalf("add req = %#v", addReq)
	}
}

func setupBrokerCommandTestRepo(t *testing.T, handler http.HandlerFunc) (string, *httptest.Server, *[]string) {
	t.Helper()
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		handler(w, r)
	}))
	for key, value := range map[string]string{
		"bucketgit.broker":      server.URL,
		"bucketgit.logicalRepo": "app.git",
		"bucketgit.provider":    "gcs",
		"bucketgit.branch":      "main",
	} {
		if _, err := runGit(target, "config", "--local", key, value); err != nil {
			server.Close()
			t.Fatal(err)
		}
	}
	return target, server, &requests
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
