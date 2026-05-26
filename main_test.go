package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
)

func TestParseGlobalFlags(t *testing.T) {
	cfg, rest, err := parseGlobalFlags([]string{
		"--bucket", "bucket",
		"--prefix", "/repos/demo.git/",
		"--branch", "main",
		"--configuration", "test-profile",
		"ls",
		"docs/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.bucket != "bucket" {
		t.Fatalf("bucket = %q", cfg.bucket)
	}
	if cfg.prefix != "repos/demo.git" {
		t.Fatalf("prefix = %q", cfg.prefix)
	}
	if cfg.branch != "main" {
		t.Fatalf("branch = %q", cfg.branch)
	}
	if cfg.auth != "gcloud" {
		t.Fatalf("auth = %q", cfg.auth)
	}
	if cfg.gcloudConfiguration != "test-profile" {
		t.Fatalf("configuration = %q", cfg.gcloudConfiguration)
	}
	if len(rest) != 2 || rest[0] != "ls" || rest[1] != "docs/" {
		t.Fatalf("rest = %#v", rest)
	}
}

func setTestHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
		if volume := filepath.VolumeName(home); volume != "" {
			t.Setenv("HOMEDRIVE", volume)
			t.Setenv("HOMEPATH", strings.TrimPrefix(home, volume))
		}
	}
}

func TestParseGlobalBucketURLInfersProviderAndPrefix(t *testing.T) {
	cfg, rest, err := parseGlobalFlags([]string{
		"admin",
		"--bucket", "s3://bucket-name/path/repo.git",
		"grant-read",
		"123456789012",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rest, " ") != "admin grant-read 123456789012" {
		t.Fatalf("rest = %v", rest)
	}
	if cfg.provider != "s3" || cfg.bucket != "bucket-name" || cfg.prefix != "path/repo.git" {
		t.Fatalf("cfg = %#v", cfg)
	}
}

func TestParseGlobalFlagsAuthADCAndProfileAlias(t *testing.T) {
	cfg, rest, err := parseGlobalFlags([]string{
		"push",
		"--auth", "adc",
		"--profile", "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.auth != "adc" {
		t.Fatalf("auth = %q", cfg.auth)
	}
	if cfg.gcloudConfiguration != "default" {
		t.Fatalf("profile = %q", cfg.gcloudConfiguration)
	}
	if len(rest) != 1 || rest[0] != "push" {
		t.Fatalf("rest = %#v", rest)
	}
}

func TestParseGlobalFlagsAnywhere(t *testing.T) {
	cfg, rest, err := parseGlobalFlags([]string{
		"clone",
		"gs://bucket/repo.git",
		"--profile=test-profile",
		"--auth=adc",
		"--branch",
		"develop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.auth != "adc" || cfg.gcloudConfiguration != "test-profile" || cfg.branch != "develop" {
		t.Fatalf("cfg = %#v", cfg)
	}
	if strings.Join(rest, " ") != "clone gs://bucket/repo.git" {
		t.Fatalf("rest = %#v", rest)
	}
}

func TestParsePushArgsSkipBroker(t *testing.T) {
	opts, err := parsePushArgs([]string{"--skip-broker", "--tags", "HEAD:refs/heads/main"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.skipBroker || !opts.tags || len(opts.refs) != 1 || opts.refs[0] != "HEAD:refs/heads/main" {
		t.Fatalf("opts = %#v", opts)
	}
}

func TestHelpAcceptsGlobalFlagsAnywhere(t *testing.T) {
	for _, args := range [][]string{
		{"help", "--profile", "test-profile"},
		{"--help", "clone", "--profile=test-profile"},
		{"clone", "--help", "--auth", "adc"},
	} {
		var stdout bytes.Buffer
		if err := run(args, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(stdout.String(), "usage:") {
			t.Fatalf("%v output = %q", args, stdout.String())
		}
	}
}

func TestVersionCommand(t *testing.T) {
	oldVersion := version
	version = "9.8.7"
	defer func() { version = oldVersion }()

	for _, args := range [][]string{
		{"--version"},
		{"version"},
		{"help", "--version"},
	} {
		var stdout bytes.Buffer
		if err := run(args, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.TrimSpace(stdout.String()) != "bgit 9.8.7" {
			t.Fatalf("%v output = %q", args, stdout.String())
		}
	}
}

func TestGcloudCommandUsesCloudSDKActiveConfigName(t *testing.T) {
	cmd := gcloudCommand("test-profile", "auth", "print-access-token")
	if strings.Join(cmd.Args, " ") != "gcloud auth print-access-token" {
		t.Fatalf("args = %#v", cmd.Args)
	}
	found := false
	for _, value := range cmd.Env {
		if value == "CLOUDSDK_ACTIVE_CONFIG_NAME=test-profile" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing CLOUDSDK_ACTIVE_CONFIG_NAME in env")
	}

	cmd = gcloudCommand("", "auth", "print-access-token")
	for _, value := range cmd.Env {
		if strings.HasPrefix(value, "CLOUDSDK_ACTIVE_CONFIG_NAME=") {
			t.Fatalf("unexpected config env: %s", value)
		}
	}
}

func TestHelpCommandPages(t *testing.T) {
	var stdout bytes.Buffer
	if err := run([]string{"help", "clone"}, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "bgit clone <broker-repo> [directory]") {
		t.Fatalf("clone help = %q", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"clone", "help"}, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "bgit clone <broker-repo> [directory]") {
		t.Fatalf("clone help alias = %q", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"--help", "clone"}, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "bgit clone <broker-repo> [directory]") {
		t.Fatalf("--help clone = %q", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"clone", "--help"}, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "bgit clone <broker-repo> [directory]") {
		t.Fatalf("clone --help = %q", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"help", "setup"}, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "bgit setup profile create") {
		t.Fatalf("setup help = %q", stdout.String())
	}
}

func TestCreateGcloudProfileCommandRequiresConfirmation(t *testing.T) {
	var stdout bytes.Buffer
	err := createGcloudProfileCommand([]string{"test-profile"}, strings.NewReader("n\n"), &stdout)
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("expected aborted, got %v", err)
	}
	if !strings.Contains(stdout.String(), "Create gcloud configuration") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestCreateAWSProfileCommandRequiresConfirmation(t *testing.T) {
	var stdout bytes.Buffer
	err := createAWSProfileCommand([]string{"default"}, strings.NewReader("n\n"), &stdout)
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("expected aborted, got %v", err)
	}
	if !strings.Contains(stdout.String(), "Create or update AWS profile") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestConfigHelp(t *testing.T) {
	var stdout bytes.Buffer
	if err := run([]string{"help", "config"}, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "usage: bgit config") {
		t.Fatalf("config help = %q", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"config", "help"}, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "usage: bgit config") {
		t.Fatalf("config help alias = %q", stdout.String())
	}
}

func TestParseGCSRepoURI(t *testing.T) {
	cfg, repoName, err := parseGCSRepoURI("gs://bucket-name/some-repo-name/some-repo-name.git")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.bucket != "bucket-name" {
		t.Fatalf("bucket = %q", cfg.bucket)
	}
	if cfg.prefix != "some-repo-name/some-repo-name.git" {
		t.Fatalf("prefix = %q", cfg.prefix)
	}
	if cfg.branch != defaultBranch {
		t.Fatalf("branch = %q", cfg.branch)
	}
	if repoName != "some-repo-name.git" {
		t.Fatalf("repoName = %q", repoName)
	}
	if cfg.origin != "gs://bucket-name/some-repo-name/some-repo-name.git" {
		t.Fatalf("origin = %q", cfg.origin)
	}
}

func TestParseGCSRepoURIAcceptsLegacyGCSScheme(t *testing.T) {
	cfg, _, err := parseGCSRepoURI("gcs://bucket-name/path/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.origin != "gs://bucket-name/path/repo.git" {
		t.Fatalf("origin = %q", cfg.origin)
	}
}

func TestParseRepoURIAcceptsS3Scheme(t *testing.T) {
	cfg, repoName, err := parseRepoURI("s3://bucket-name/path/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.provider != "s3" {
		t.Fatalf("provider = %q", cfg.provider)
	}
	if cfg.bucket != "bucket-name" || cfg.prefix != "path/repo.git" {
		t.Fatalf("cfg = %#v", cfg)
	}
	if repoName != "repo.git" {
		t.Fatalf("repoName = %q", repoName)
	}
	if cfg.origin != "s3://bucket-name/path/repo.git" {
		t.Fatalf("origin = %q", cfg.origin)
	}
}

func TestObjectNameHelpers(t *testing.T) {
	if got := objectPrefix("repos/demo.git"); got != "repos/demo.git/" {
		t.Fatalf("objectPrefix = %q", got)
	}
	if got := objectPrefix(""); got != "" {
		t.Fatalf("empty objectPrefix = %q", got)
	}
	if got := joinObjectName("/repos/demo.git/", "/HEAD"); got != "repos/demo.git/HEAD" {
		t.Fatalf("joinObjectName = %q", got)
	}
}

func TestBranchHelpers(t *testing.T) {
	if got := branchRef("main"); got != "refs/heads/main" {
		t.Fatalf("branchRef = %q", got)
	}
	if got := branchRef("refs/heads/dev"); got != "refs/heads/dev" {
		t.Fatalf("full branchRef = %q", got)
	}
	if got := shortBranchName("refs/heads/dev"); got != "dev" {
		t.Fatalf("shortBranchName = %q", got)
	}
}

func TestCommandCreatesBucketForWriteSyncCommands(t *testing.T) {
	for _, cmd := range []string{"init", "put"} {
		if !commandCreatesBucket(cmd) {
			t.Fatalf("%s should create buckets", cmd)
		}
	}
	for _, cmd := range []string{"clone", "ls", "cat", "log"} {
		if commandCreatesBucket(cmd) {
			t.Fatalf("%s should not create buckets", cmd)
		}
	}
}

func TestAdminGrantHelpers(t *testing.T) {
	opts, err := parseAdminGrantArgs([]string{"--bucket", "demo-bucket", "grant_write_access", "ci@project.iam.gserviceaccount.com"})
	if err != nil {
		t.Fatalf("parseAdminGrantArgs: %v", err)
	}
	if opts.bucket != "demo-bucket" {
		t.Fatalf("bucket = %q", opts.bucket)
	}
	if opts.action != "write" {
		t.Fatalf("action = %q", opts.action)
	}
	if got := normalizeIAMMember(opts.member, opts.serviceAccount); got != "serviceAccount:ci@project.iam.gserviceaccount.com" {
		t.Fatalf("member = %q", got)
	}
	publicOpts, err := parseAdminGrantArgs([]string{"--bucket=s3://demo-bucket/repos/app.git", "make-public"})
	if err != nil {
		t.Fatalf("parse make-public: %v", err)
	}
	if publicOpts.provider != "s3" || publicOpts.bucket != "demo-bucket" || publicOpts.prefix != "repos/app.git" || publicOpts.action != "make-public" {
		t.Fatalf("make-public opts = %#v", publicOpts)
	}

	roles, label, err := adminGrantRoles("grant-read-access")
	if err != nil {
		t.Fatalf("adminGrantRoles: %v", err)
	}
	if label != "read" || len(roles) != 2 || roles[0] != storageObjectViewer || roles[1] != storageLegacyBucketReader {
		t.Fatalf("read roles = %v %q", roles, label)
	}

	roles, label, err = adminGrantRoles("grant-admin")
	if err != nil {
		t.Fatalf("adminGrantRoles admin: %v", err)
	}
	if label != "admin" || len(roles) != 1 || roles[0] != storageAdmin {
		t.Fatalf("admin roles = %v %q", roles, label)
	}

	if got := normalizeIAMMember("dev@example.com", false); got != "user:dev@example.com" {
		t.Fatalf("user member = %q", got)
	}
	if got := normalizeIAMMember("group:team@example.com", false); got != "group:team@example.com" {
		t.Fatalf("group member = %q", got)
	}
	if got := normalizeIAMMember("allUsers", false); got != "allUsers" {
		t.Fatalf("allUsers member = %q", got)
	}
	if got := normalizeAdminBucket("gs://demo-bucket/path/repo.git"); got != "demo-bucket" {
		t.Fatalf("admin bucket = %q", got)
	}
	provider, bucket, prefix := normalizeAdminTarget("s3://demo-bucket/path/repo.git")
	if provider != "s3" || bucket != "demo-bucket" || prefix != "path/repo.git" {
		t.Fatalf("admin target = %q %q %q", provider, bucket, prefix)
	}
}

func TestS3AdminPolicyGrantReadWriteAdmin(t *testing.T) {
	principal, err := normalizeAWSPrincipal("123456789012")
	if err != nil {
		t.Fatal(err)
	}
	if principal != "arn:aws:iam::123456789012:root" {
		t.Fatalf("principal = %q", principal)
	}
	rolePrincipal, err := normalizeAWSPrincipal("arn:aws:iam::123456789012:role/Developer")
	if err != nil {
		t.Fatal(err)
	}
	policy := s3BucketPolicy{Version: "2012-10-17"}
	policy.addBucketGitGrant("demo-bucket", "repos/app.git", "read", rolePrincipal)
	if len(policy.Statement) != 2 {
		t.Fatalf("read statements = %#v", policy.Statement)
	}
	list := policy.Statement[0]
	if list.Action != "s3:ListBucket" || list.Resource != "arn:aws:s3:::demo-bucket" {
		t.Fatalf("list statement = %#v", list)
	}
	if list.Condition == nil {
		t.Fatalf("missing prefix condition")
	}
	objects := policy.Statement[1]
	if objects.Resource != "arn:aws:s3:::demo-bucket/repos/app.git/*" {
		t.Fatalf("object resource = %#v", objects.Resource)
	}
	policy.addBucketGitGrant("demo-bucket", "repos/app.git", "write", rolePrincipal)
	if len(policy.Statement) != 4 {
		t.Fatalf("write should coexist with read grants by action, got %#v", policy.Statement)
	}
	policy.addBucketGitGrant("demo-bucket", "repos/app.git", "write", rolePrincipal)
	if len(policy.Statement) != 4 {
		t.Fatalf("duplicate write grant was not replaced: %#v", policy.Statement)
	}
	policy.addBucketGitGrant("demo-bucket", "repos/app.git", "admin", rolePrincipal)
	foundAdmin := false
	for _, statement := range policy.Statement {
		if strings.HasSuffix(statement.Sid, "Admin") {
			foundAdmin = true
			if statement.Action != "s3:*" {
				t.Fatalf("admin action = %#v", statement.Action)
			}
		}
	}
	if !foundAdmin {
		t.Fatalf("missing admin statement: %#v", policy.Statement)
	}
	if _, err := normalizeAWSPrincipal("dev@example.com"); err == nil {
		t.Fatal("expected invalid AWS principal")
	}
}

func TestS3AdminPolicyPublicPrivate(t *testing.T) {
	policy := s3BucketPolicy{Version: "2012-10-17"}
	policy.addBucketGitGrant("demo-bucket", "repos/app.git", "read", "*")
	if len(policy.Statement) != 2 {
		t.Fatalf("public read statements = %#v", policy.Statement)
	}
	if policy.Statement[0].Principal != "*" || policy.Statement[1].Principal != "*" {
		t.Fatalf("public principal = %#v %#v", policy.Statement[0].Principal, policy.Statement[1].Principal)
	}
	policy.addBucketGitGrant("demo-bucket", "repos/app.git", "write", "arn:aws:iam::123456789012:root")
	policy.removeBucketGitPublicGrants("repos/app.git")
	if len(policy.Statement) != 2 {
		t.Fatalf("private should only remove public statements: %#v", policy.Statement)
	}
	for _, statement := range policy.Statement {
		if statement.Principal == "*" {
			t.Fatalf("public statement remains: %#v", statement)
		}
	}
}

func TestReadOnlyRemoteCommandsUsePublicFallback(t *testing.T) {
	for _, cmd := range []string{"clone", "fetch", "pull", "ls-remote", "ls", "cat", "show", "log"} {
		if !isReadOnlyRemoteCommand(cmd) {
			t.Fatalf("%s should use public fallback", cmd)
		}
	}
	for _, cmd := range []string{"init", "push", "put"} {
		if isReadOnlyRemoteCommand(cmd) {
			t.Fatalf("%s should not use public fallback", cmd)
		}
	}
}

func TestUnsupportedCommand(t *testing.T) {
	err := unsupportedCommand("submodule")
	if err == nil || !strings.Contains(err.Error(), "Unsupported: 'submodule'") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isUnsupportedCommand("gc") {
		t.Fatal("gc should be unsupported")
	}
}

func TestFormerPassthroughCommandsAreUnsupported(t *testing.T) {
	for _, cmd := range []string{"rebase"} {
		err := run([]string{cmd}, strings.NewReader(""), ioDiscard{}, ioDiscard{})
		if err == nil || !strings.Contains(err.Error(), "Unsupported") {
			t.Fatalf("%s error = %v", cmd, err)
		}
	}
}

func TestParsePushArgs(t *testing.T) {
	opts, err := parsePushArgs([]string{"--tags", "--force", "HEAD:refs/heads/main"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.tags || !opts.force || opts.delete {
		t.Fatalf("opts = %#v", opts)
	}
	if len(opts.refs) != 1 || opts.refs[0] != "HEAD:refs/heads/main" {
		t.Fatalf("refs = %#v", opts.refs)
	}
	opts, err = parsePushArgs([]string{"--delete", "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.delete || normalizeDeleteRef(opts.refs[0]) != "refs/heads/feature" {
		t.Fatalf("delete opts = %#v", opts)
	}
}

func TestParsePushArgsAcceptsGitRemoteShape(t *testing.T) {
	opts, err := parsePushArgs([]string{"-u", "origin", "feature/protection-check"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.remote != "origin" {
		t.Fatalf("remote = %q", opts.remote)
	}
	if len(opts.refs) != 1 || opts.refs[0] != "feature/protection-check" {
		t.Fatalf("refs = %#v", opts.refs)
	}

	opts, err = parsePushArgs([]string{"--set-upstream", "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.remote != "origin" || len(opts.refs) != 0 {
		t.Fatalf("opts = %#v", opts)
	}
}

func TestNoRefsErrorDetection(t *testing.T) {
	err := errors.New("git --git-dir /tmp/repo.git show-ref --tags: exit status 1")
	if !isNoRefs(err) {
		t.Fatal("expected no refs error")
	}
}

func TestDefaultProjectIDUsesEnvironment(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "project-from-env")
	projectID, err := defaultProjectID(config{})
	if err != nil {
		t.Fatal(err)
	}
	if projectID != "project-from-env" {
		t.Fatalf("projectID = %q", projectID)
	}
}

func TestBucketCreateErrorExplainsGcloudAccountMismatch(t *testing.T) {
	err := bucketCreateError("bucket-name", "project-id", &googleapi.Error{Code: 403, Message: "forbidden"})
	text := err.Error()
	for _, want := range []string{
		"selected gcloud configuration",
		"gcloud auth print-access-token",
		"gcloud storage buckets create gs://bucket-name --project project-id",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestInitWorktreeCreatesGitCheckout(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "repo.git")
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "checkout")

	if _, err := runGit("", "init", "--bare", bare); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit("", "init", source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(source, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada",
		"GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada",
		"GIT_COMMITTER_EMAIL=ada@example.com",
	)
	if _, err := runGitEnv(source, env, "commit", "-m", "Initial commit"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(source, "push", bare, "HEAD:refs/heads/master"); err != nil {
		t.Fatal(err)
	}

	repo := newNativeGitRepoForStore(config{bucket: "bucket", prefix: "repos/demo.git", branch: "master"}, &localGitStore{root: bare})
	var stdout bytes.Buffer
	if err := repo.initWorktree(context.Background(), []string{target}, &stdout); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(data), "\r\n", "\n") != "# Demo\n" {
		t.Fatalf("README.md = %q", string(data))
	}
	logOut, err := runGit(target, "log", "--oneline")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logOut), "Initial commit") {
		t.Fatalf("git log output = %q", string(logOut))
	}
	configOut, err := runGit(target, "config", "--local", "bucketgit.prefix")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(configOut)) != "repos/demo.git" {
		t.Fatalf("bucketgit.prefix = %q", string(configOut))
	}
	originOut, err := runGit(target, "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(originOut)) != "gs://bucket/repos/demo.git" {
		t.Fatalf("origin = %q", string(originOut))
	}
	for key, want := range map[string]string{
		"branch.master.remote": "origin",
		"branch.master.merge":  "refs/heads/master",
	} {
		out, err := runGit(target, "config", "--local", "--get", key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestOriginCommandWritesLocalConfigAndGitRemote(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", target); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := originCommand([]string{"gs://bucket-name/path/repo.git"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("config", []string{"bucketgit.auth", "adc"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("config", []string{"bucketgit.profile", "test-profile"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := readLocalConfig(".")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.bucket != "bucket-name" || cfg.prefix != "path/repo.git" {
		t.Fatalf("cfg = %#v", cfg)
	}
	if cfg.auth != "adc" || cfg.gcloudConfiguration != "test-profile" {
		t.Fatalf("auth cfg = %#v", cfg)
	}
	remoteOut, err := runGit(target, "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(remoteOut)) != "gs://bucket-name/path/repo.git" {
		t.Fatalf("remote origin = %q", string(remoteOut))
	}
}

func TestOriginCommandWritesS3Provider(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", target); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := originCommand([]string{"s3://bucket-name/path/repo.git"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := readLocalConfig(".")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.provider != "s3" || cfg.bucket != "bucket-name" || cfg.prefix != "path/repo.git" {
		t.Fatalf("cfg = %#v", cfg)
	}
	if cfg.origin != "s3://bucket-name/path/repo.git" {
		t.Fatalf("origin = %q", cfg.origin)
	}
}

func TestSSHKeysCommandsUseBroker(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", target); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(keyPath, []byte("ssh-ed25519 AAAAADMIN admin@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/keys/add", "/keys/remove", "/keys/suspend":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/keys/list":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"keys":[{"user":"admin","role":"admin","public_key":"ssh-ed25519 AAAAADMIN admin@example.com"}]}`))
		default:
			t.Fatalf("unexpected broker path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := originCommand([]string{"gs://bucket-name/path/repo.git"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "config", "--local", "bucketgit.broker", server.URL); err != nil {
		t.Fatal(err)
	}
	if err := brokerAdminKeysCommand(config{auth: "gcloud", branch: defaultBranch}, []string{"add", "--no-agent", "--key", keyPath, "--user", "ada", "--role", "write"}, strings.NewReader(""), ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := brokerAdminKeysCommand(config{auth: "gcloud", branch: defaultBranch}, []string{"list"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "admin\tadmin\tactive\tssh-ed25519 AAAAADMIN admin@example.com") {
		t.Fatalf("keys list stdout = %q", stdout.String())
	}
	if err := brokerAdminKeysCommand(config{auth: "gcloud", branch: defaultBranch}, []string{"suspend", "AAAAADMIN"}, strings.NewReader(""), ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := brokerAdminKeysCommand(config{auth: "gcloud", branch: defaultBranch}, []string{"remove", "AAAAADMIN"}, strings.NewReader(""), ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/keys/add", "/keys/list", "/keys/suspend", "/keys/remove"}
	if strings.Join(requests, ",") != strings.Join(want, ",") {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestSSHTransportInvocationAdvertisesRefsBeforePackTransfer(t *testing.T) {
	err := sshCommand(config{}, []string{"git-upload-pack"}, ioDiscard{}, ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "missing repository path") {
		t.Fatalf("err = %v", err)
	}
}

func TestAuthorizeSSHGitServiceUsesBrokerRoles(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", target); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := originCommand([]string{"gs://bucket-name/path/repo.git"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	var operations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/check" {
			t.Fatalf("unexpected broker path %s", r.URL.Path)
		}
		var req brokerAuthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		operations = append(operations, req.Operation)
		w.Header().Set("content-type", "application/json")
		allowed := req.Operation == "read"
		_, _ = fmt.Fprintf(w, `{"allowed":%t,"user":"ada","role":"read"}`, allowed)
	}))
	defer server.Close()
	if _, err := runGit(target, "config", "--local", "bucketgit.broker", server.URL); err != nil {
		t.Fatal(err)
	}
	cfg := config{provider: "gcs", bucket: "bucket-name", prefix: "path/repo.git", branch: "main", origin: "gs://bucket-name/path/repo.git"}
	if err := authorizeSSHGitService(cfg, gitUploadPackService); err != nil {
		t.Fatal(err)
	}
	if err := authorizeSSHGitService(cfg, gitReceivePackService); err == nil || !strings.Contains(err.Error(), "broker denied write access") {
		t.Fatalf("write err = %v", err)
	}
	if strings.Join(operations, ",") != "read,write" {
		t.Fatalf("operations = %#v", operations)
	}
}

func TestConfigForSSHRepoDefaultsToGCSPath(t *testing.T) {
	cfg, err := configForSSHRepo("bucket-name/path/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.provider != "gcs" || cfg.bucket != "bucket-name" || cfg.prefix != "path/repo.git" {
		t.Fatalf("cfg = %#v", cfg)
	}
	if cfg.origin != "gs://bucket-name/path/repo.git" {
		t.Fatalf("origin = %q", cfg.origin)
	}
}

func TestConfigForSSHRepoInfersS3FromLocalConfig(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", target); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := originCommand([]string{"s3://bucket-name/path/repo.git"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := configForSSHRepo("bucket-name/path/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.provider != "s3" || cfg.origin != "s3://bucket-name/path/repo.git" {
		t.Fatalf("cfg = %#v", cfg)
	}
}

func TestConfigForSSHRepoDecodesProviderPrefixedPath(t *testing.T) {
	cfg, err := configForSSHRepo("s3/bucket-name/path/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.provider != "s3" || cfg.bucket != "bucket-name" || cfg.prefix != "path/repo.git" || cfg.origin != "s3://bucket-name/path/repo.git" {
		t.Fatalf("cfg = %#v", cfg)
	}
	cfg, err = configForSSHRepo("gs/bucket-name/path/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.provider != "gcs" || cfg.origin != "gs://bucket-name/path/repo.git" {
		t.Fatalf("cfg = %#v", cfg)
	}
}

func TestWriteAdvertisedRefsFromNativeRepo(t *testing.T) {
	bare := createBareFixture(t)
	repo := newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: bare})
	refs, err := repo.refs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	refs["HEAD"] = refs["refs/heads/main"]
	var stdout bytes.Buffer
	if err := writeAdvertisedRefs(&stdout, gitUploadPackService, refs, uploadPackCapabilities()); err != nil {
		t.Fatal(err)
	}
	lines, err := pktLinesForTest(stdout.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) < 2 {
		t.Fatalf("lines = %#v", lines)
	}
	first := string(lines[0].data)
	for _, want := range []string{"HEAD", "symref=HEAD:refs/heads/main", "side-band-64k"} {
		if !strings.Contains(first, want) {
			t.Fatalf("first advertised ref missing %q in %q", want, first)
		}
	}
}

func TestDiscoverGCPBrokerURLUsesCloudRunFunctionsURI(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "functions describe bgit-broker", stdout: "https://bgit-broker-functions.example.test"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := discoverGCPBrokerURL(config{provider: "gcs", gcloudConfiguration: "test-profile"}, sshSetupOptions{region: "europe-west1"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://bgit-broker-functions.example.test" {
		t.Fatalf("broker URL = %q", got)
	}
}

func TestDiscoverGCPBrokerURLFallsBackToCloudRunServiceURL(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "functions describe bgit-broker", exitCode: 1},
		{match: "run services describe bgit-broker", stdout: "https://bgit-broker-run.example.test"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := discoverGCPBrokerURL(config{provider: "gcs"}, sshSetupOptions{region: "europe-west1"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://bgit-broker-run.example.test" {
		t.Fatalf("broker URL = %q", got)
	}
}

func TestDiscoverAWSBrokerURLUsesCloudFormationOutput(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "aws", []fakeCLIAction{
		{match: "cloudformation describe-stacks", stdout: "https://bgit-broker-aws.example.test"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := discoverAWSBrokerURL(config{provider: "s3", gcloudConfiguration: "aws-profile"}, sshSetupOptions{region: "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://bgit-broker-aws.example.test" {
		t.Fatalf("broker URL = %q", got)
	}
}

func TestDiscoverAWSBrokerURLFallsBackToSSMParameter(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "aws", []fakeCLIAction{
		{match: "cloudformation describe-stacks", exitCode: 1},
		{match: "ssm get-parameter", stdout: "https://bgit-broker-ssm.example.test"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := discoverAWSBrokerURL(config{provider: "s3"}, sshSetupOptions{region: "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://bgit-broker-ssm.example.test" {
		t.Fatalf("broker URL = %q", got)
	}
}

func TestProvisionGCPBrokerURLDeploysThenDiscoversFunction(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "deployed")
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "functions describe bgit-broker --gen2 --region europe-west1 --format=value(serviceConfig.uri)", stdout: "https://bgit-broker-provisioned.example.test", requireFile: marker, exitCode: 1},
		{match: "services enable"},
		{match: "services list --enabled", stdout: "serviceusage.googleapis.com cloudresourcemanager.googleapis.com cloudfunctions.googleapis.com run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com firestore.googleapis.com iamcredentials.googleapis.com secretmanager.googleapis.com"},
		{match: "firestore databases describe", exitCode: 1},
		{match: "firestore databases create"},
		{match: "config get-value project", stdout: "project-id"},
		{match: "config get-value account", stdout: "ada@example.com"},
		{match: "iam service-accounts describe bgit-broker@project-id.iam.gserviceaccount.com", exitCode: 1},
		{match: "iam service-accounts create bgit-broker"},
		{match: "projects add-iam-policy-binding project-id --member=serviceAccount:bgit-broker@project-id.iam.gserviceaccount.com"},
		{match: "secrets describe bgit-ci-materializer-token", exitCode: 1},
		{match: "secrets create bgit-ci-materializer-token"},
		{match: "secrets versions add bgit-ci-materializer-token"},
		{match: "secrets add-iam-policy-binding bgit-ci-materializer-token"},
		{match: "run services describe bgit-ci-materializer", exitCode: 1},
		{match: "--service-account bgit-broker@project-id.iam.gserviceaccount.com", touch: marker},
		{match: "iam service-accounts add-iam-policy-binding bgit-broker@project-id.iam.gserviceaccount.com"},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	got, err := provisionGCPBrokerURL(config{provider: "gcs"}, sshSetupOptions{region: "europe-west1"}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://bgit-broker-provisioned.example.test" {
		t.Fatalf("broker URL = %q", got)
	}
	if !strings.Contains(stdout.String(), "deploying GCP Cloud Run function bgit-broker in europe-west1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "ensuring GCP broker APIs are enabled") ||
		!strings.Contains(stdout.String(), "creating Firestore database bgit in europe-west1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestEnsureGCPBrokerFirestoreDatabaseSkipsExistingDatabase(t *testing.T) {
	bin := t.TempDir()
	writeFakeCLI(t, bin, "gcloud", []fakeCLIAction{
		{match: "firestore databases describe --database=custom", stdout: "projects/demo/databases/custom"},
		{match: "firestore databases create", exitCode: 9},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	err := ensureGCPBrokerFirestoreDatabase(config{provider: "gcs"}, sshSetupOptions{firestoreDatabase: "custom", region: "europe-west1"}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestProvisionAWSBrokerURLDeploysThenDiscoversStackOutput(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "deployed")
	writeFakeCLI(t, bin, "aws", []fakeCLIAction{
		{match: "sts get-caller-identity", stdout: `{"Account":"123456789012","Arn":"arn:aws:iam::123456789012:user/dennis"}`},
		{match: "s3api head-bucket --bucket bgit-broker-artifacts-123456789012-eu-west-1", exitCode: 1},
		{match: "s3api create-bucket --bucket bgit-broker-artifacts-123456789012-eu-west-1"},
		{match: "cloudformation describe-stacks", stdout: "https://bgit-broker-provisioned-aws.example.test", requireFile: marker, exitCode: 1},
		{match: "cloudformation deploy", touch: marker},
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout bytes.Buffer
	got, err := provisionAWSBrokerURL(config{provider: "s3", gcloudConfiguration: "aws-profile"}, sshSetupOptions{region: "eu-west-1"}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://bgit-broker-provisioned-aws.example.test" {
		t.Fatalf("broker URL = %q", got)
	}
	if !strings.Contains(stdout.String(), "deploying AWS CloudFormation stack bgit-broker in eu-west-1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

type fakeCLIAction struct {
	match         string
	stdout        string
	missingStdout string
	exitCode      int
	touch         string
	requireFile   string
	onlyIfFile    string
}

type fakeCLIActionJSON struct {
	Match         string `json:"match,omitempty"`
	Stdout        string `json:"stdout,omitempty"`
	MissingStdout string `json:"missing_stdout,omitempty"`
	ExitCode      int    `json:"exit_code,omitempty"`
	Touch         string `json:"touch,omitempty"`
	RequireFile   string `json:"require_file,omitempty"`
	OnlyIfFile    string `json:"only_if_file,omitempty"`
}

func (a fakeCLIAction) MarshalJSON() ([]byte, error) {
	return json.Marshal(fakeCLIActionJSON{
		Match:         a.match,
		Stdout:        a.stdout,
		MissingStdout: a.missingStdout,
		ExitCode:      a.exitCode,
		Touch:         a.touch,
		RequireFile:   a.requireFile,
		OnlyIfFile:    a.onlyIfFile,
	})
}

func (a *fakeCLIAction) UnmarshalJSON(data []byte) error {
	var raw fakeCLIActionJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*a = fakeCLIAction{
		match:         raw.Match,
		stdout:        raw.Stdout,
		missingStdout: raw.MissingStdout,
		exitCode:      raw.ExitCode,
		touch:         raw.Touch,
		requireFile:   raw.RequireFile,
		onlyIfFile:    raw.OnlyIfFile,
	}
	return nil
}

func writeFakeCLI(t *testing.T, dir, name string, actions []fakeCLIAction) {
	t.Helper()
	path := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	actionsData, err := json.Marshal(actions)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".json", actionsData, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMain(m *testing.M) {
	if path, ok := fakeCLIActionPath(); ok {
		os.Exit(runFakeCLI(path, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeCLIActionPath() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	path := exe + ".json"
	if _, err := os.Stat(path); err == nil {
		return path, true
	}
	return "", false
}

func runFakeCLI(path string, args []string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var actions []fakeCLIAction
	if err := json.Unmarshal(data, &actions); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	joined := strings.Join(args, " ")
	for _, action := range actions {
		if !strings.Contains(joined, action.match) {
			continue
		}
		if action.onlyIfFile != "" {
			if _, err := os.Stat(action.onlyIfFile); err != nil {
				continue
			}
		}
		if action.requireFile != "" {
			if _, err := os.Stat(action.requireFile); err != nil {
				if action.missingStdout != "" {
					fmt.Fprintln(os.Stdout, action.missingStdout)
				}
				return firstNonZeroInt(action.exitCode, 1)
			}
		}
		if action.touch != "" {
			if err := os.WriteFile(action.touch, nil, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		if action.stdout != "" {
			fmt.Fprintln(os.Stdout, action.stdout)
		}
		return fakeCLIFinalExitCode(action)
	}
	return 1
}

func fakeCLIFinalExitCode(action fakeCLIAction) int {
	if action.requireFile != "" && action.stdout != "" {
		return 0
	}
	return action.exitCode
}

func firstNonZeroInt(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func TestAWSBrokerCloudFormationTemplateHasBrokerOutput(t *testing.T) {
	template := awsBrokerCloudFormationTemplate()
	for _, want := range []string{
		"Type: AWS::DynamoDB::Table",
		"Type: AWS::Lambda::Function",
		"Type: AWS::Lambda::Url",
		"dynamodb:GetItem",
		"dynamodb:PutItem",
		"BrokerUrl:",
		"nodejs22.x",
		"/auth/check",
		"/objects/read",
		"s3:GetObject",
		"InvokedViaFunctionUrl",
		"/refs/update",
		"roleAllows",
		"normalizeLogicalRepo",
		"logical repo names must be flat",
		"ConditionalCheckFailedException",
		"BROKER_VERSION: " + brokerVersion,
		`version: brokerVersion`,
	} {
		if !strings.Contains(template, want) {
			t.Fatalf("template missing %q:\n%s", want, template)
		}
	}
}

func TestGCPBrokerSourceUsesFirestoreAndSignatureHeaders(t *testing.T) {
	dir := t.TempDir()
	if err := writeGCPBrokerSource(dir); err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"@google-cloud/firestore",
		"@google-cloud/storage",
		"databaseId: process.env.FIRESTORE_DATABASE || 'bgit'",
		"x-bgit-key",
		"x-bgit-signature",
		"admin SSH signature required",
		"bgit_broker_repos",
		"/auth/check",
		"/objects/read",
		"/refs/update",
		"roleAllows",
		"normalizeLogicalRepo",
		"logical repo names must be flat",
		"runTransaction",
		"process.env.BROKER_VERSION",
		"version: brokerVersion",
	} {
		if !strings.Contains(string(index), want) {
			t.Fatalf("GCP broker source missing %q:\n%s", want, string(index))
		}
	}
}

func TestBrokerObjectCapabilityPathPolicy(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for broker JavaScript policy tests")
	}
	gcpSourceBytes, err := brokerAssets.ReadFile("broker/gcp/index.js")
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{
		"gcp": string(gcpSourceBytes),
		"aws": awsBrokerCloudFormationTemplate(),
	}
	for name, source := range sources {
		t.Run(name, func(t *testing.T) {
			script, err := brokerCapabilityPolicyNodeScript(source)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("node", "-e", script)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("policy test failed: %v\n%s", err, string(out))
			}
		})
	}
}

func TestBrokerReplayStoresRejectDuplicateNonces(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for broker JavaScript replay tests")
	}
	gcpSourceBytes, err := brokerAssets.ReadFile("broker/gcp/index.js")
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"gcp": brokerReplayNodeScript(string(gcpSourceBytes), true),
		"aws": brokerReplayNodeScript(awsBrokerCloudFormationTemplate(), false),
	}
	for name, script := range tests {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("node", "-e", script)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("replay test failed: %v\n%s", err, string(out))
			}
		})
	}
}

func TestBrokerLocalMaterializerHarness(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for broker JavaScript materializer tests")
	}
	gcpSourceBytes, err := brokerAssets.ReadFile("broker/gcp/index.js")
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"gcp": brokerGCPMaterializerNodeScript(string(gcpSourceBytes)),
		"aws": brokerAWSMaterializerNodeScript(awsBrokerCloudFormationTemplate()),
	}
	for name, script := range tests {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("node", "-e", script)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("materializer test failed: %v\n%s", err, string(out))
			}
		})
	}
}

func brokerCapabilityPolicyNodeScript(source string) (string, error) {
	var functions []string
	for _, name := range []string{"cleanObjectPath", "isGitObjectDatabasePath", "validateCapabilityPath"} {
		fn, err := extractJavaScriptFunction(source, name)
		if err != nil {
			return "", err
		}
		functions = append(functions, fn)
	}
	cases := []struct {
		Operation string `json:"operation"`
		Path      string `json:"path"`
		Want      string `json:"want,omitempty"`
		OK        bool   `json:"ok"`
	}{
		{Operation: "read", Path: "HEAD", Want: "HEAD", OK: true},
		{Operation: "read", Path: "packed-refs", Want: "packed-refs", OK: true},
		{Operation: "read", Path: "refs/heads/main", Want: "refs/heads/main", OK: true},
		{Operation: "read", Path: "objects/aa/bb", Want: "objects/aa/bb", OK: true},
		{Operation: "read", Path: "objects/pack/pack-demo.pack", Want: "objects/pack/pack-demo.pack", OK: true},
		{Operation: "write", Path: "objects/aa/bb", Want: "objects/aa/bb", OK: true},
		{Operation: "write", Path: "/objects/aa/bb", Want: "objects/aa/bb", OK: true},
		{Operation: "write", Path: "refs/heads/main", OK: false},
		{Operation: "write", Path: "HEAD", OK: false},
		{Operation: "read", Path: "config", OK: false},
		{Operation: "delete", Path: "objects/aa/bb", OK: false},
		{Operation: "write", Path: "objects/../refs/heads/main", OK: false},
		{Operation: "read", Path: "", OK: false},
		{Operation: "read", Path: "objects//aa", OK: false},
	}
	data, err := json.Marshal(cases)
	if err != nil {
		return "", err
	}
	return strings.Join(functions, "\n") + `
const cases = ` + string(data) + `;
for (const tc of cases) {
  let ok = true;
  let got = "";
  try {
    got = validateCapabilityPath(tc.operation, tc.path);
  } catch (err) {
    ok = false;
  }
  if (ok !== tc.ok || (tc.ok && got !== tc.want)) {
    console.error(JSON.stringify({case: tc, ok, got}));
    process.exit(1);
  }
}
`, nil
}

func brokerReplayNodeScript(source string, gcp bool) string {
	fn, err := extractJavaScriptFunction(source, "consumeSignatureNonce")
	if err != nil {
		return "throw new Error(" + strconv.Quote(err.Error()) + ");"
	}
	if gcp {
		return `
const seen = new Map();
const nonces = {doc(id) { return {id}; }};
const db = {async runTransaction(fn) {
  const tx = {
    async get(ref) { return {exists: seen.has(ref.id), data() { return seen.get(ref.id); }}; },
    set(ref, data) { seen.set(ref.id, data); },
  };
  await fn(tx);
}};
` + fn + `
(async () => {
  await consumeSignatureNonce("SHA256:test", "nonce", String(Math.floor(Date.now() / 1000)));
  let replayed = false;
  try {
    await consumeSignatureNonce("SHA256:test", "nonce", String(Math.floor(Date.now() / 1000)));
  } catch (err) {
    replayed = /replay/.test(String(err.message || err));
  }
  if (!replayed) throw new Error("duplicate nonce was accepted");
})();
`
	}
	return `
class PutItemCommand { constructor(input) { this.input = input; } }
const seen = new Map();
const db = {async send(cmd) {
  const id = cmd.input.Item.id.S;
  const exists = seen.has(id);
  const now = Number(cmd.input.ExpressionAttributeValues[":now"].N);
  const old = exists ? seen.get(id) : null;
  if (exists && Number(old.expires_at.N) >= now) {
    const err = new Error("conditional failed");
    err.name = "ConditionalCheckFailedException";
    throw err;
  }
  seen.set(id, cmd.input.Item);
}};
const table = "broker";
` + fn + `
(async () => {
  await consumeSignatureNonce("SHA256:test", "nonce", String(Math.floor(Date.now() / 1000)));
  let replayed = false;
  try {
    await consumeSignatureNonce("SHA256:test", "nonce", String(Math.floor(Date.now() / 1000)));
  } catch (err) {
    replayed = /replay/.test(String(err.message || err));
  }
  if (!replayed) throw new Error("duplicate nonce was accepted");
})();
`
}

func brokerGCPMaterializerNodeScript(source string) string {
	trigger, err := extractJavaScriptFunction(source, "triggerCIRun")
	if err != nil {
		return "throw new Error(" + strconv.Quote(err.Error()) + ");"
	}
	rotate, err := extractJavaScriptFunction(source, "rotateCIMaterializerToken")
	if err != nil {
		return "throw new Error(" + strconv.Quote(err.Error()) + ");"
	}
	return `
process.env.BGIT_CI_MATERIALIZER_URL = "https://materializer.example/run";
process.env.BGIT_CI_MATERIALIZER_SECRET = "projects/p/secrets/s";
const brokerVersion = "test";
let currentSecret = "old-token";
let idTokenRequest = null;
const auth = {async getIdTokenClient(url) {
  idTokenRequest = url;
  return {async request(req) {
    globalThis.materializerRequest = req;
    return {data: {status: "queued", url: "https://build.example/1", message: "ok"}};
  }};
}};
async function ciMaterializerToken() { return currentSecret; }
function randomCIToken() { return "new-token"; }
async function secretManagerRequest(path, opts = {}) {
  globalThis.secretRequest = {path, opts};
  currentSecret = Buffer.from(opts.body.payload.data, "base64").toString("utf8");
  return {ok: true};
}
` + trigger + "\n" + rotate + `
(async () => {
  const entry = {data: {repo: {provider: "gcs", bucket: "bucket", prefix: "repo.git"}}};
  const run = {id: 1, provider: "gcp", ref: "refs/heads/main", commit: "abc", config: "cloudbuild.yaml"};
  await triggerCIRun(entry, run);
  if (idTokenRequest !== process.env.BGIT_CI_MATERIALIZER_URL) throw new Error("ID-token client was not created for materializer URL");
  if (globalThis.materializerRequest.headers["x-bgit-ci-token"] !== "old-token") throw new Error("materializer token header missing");
  if (run.url !== "https://build.example/1") throw new Error("materializer response was not applied");
  await rotateCIMaterializerToken();
  if (currentSecret !== "new-token") throw new Error("rotation did not update secret");
})();
`
}

func brokerAWSMaterializerNodeScript(source string) string {
	trigger, err := extractJavaScriptFunction(source, "triggerCIRun")
	if err != nil {
		return "throw new Error(" + strconv.Quote(err.Error()) + ");"
	}
	rotate, err := extractJavaScriptFunction(source, "rotateCIMaterializerToken")
	if err != nil {
		return "throw new Error(" + strconv.Quote(err.Error()) + ");"
	}
	return `
class InvokeCommand { constructor(input) { this.input = input; } }
class GetSecretValueCommand { constructor(input) { this.input = input; } }
class PutSecretValueCommand { constructor(input) { this.input = input; } }
const brokerVersion = "test";
const ciMaterializerFunctionName = "bgit-ci-materializer";
const ciMaterializerSecretArn = "arn:secret";
let secret = "old-token";
const secrets = {async send(cmd) {
  if (cmd instanceof GetSecretValueCommand) return {SecretString: secret};
  if (cmd instanceof PutSecretValueCommand) { secret = cmd.input.SecretString; return {}; }
  throw new Error("unexpected secrets command");
}};
const lambda = {async send(cmd) {
  globalThis.invokeInput = cmd.input;
  const payload = JSON.parse(Buffer.from(cmd.input.Payload).toString("utf8"));
  globalThis.materializerPayload = payload;
  return {Payload: Buffer.from(JSON.stringify({status: "queued", url: "https://build.example/1", message: "ok"}))};
}};
function randomCIToken() { return "new-token"; }
` + extractMust(source, "ciMaterializerToken") + "\n" + trigger + "\n" + rotate + `
(async () => {
  const entry = {data: {repo: {provider: "s3", bucket: "bucket", prefix: "repo.git"}}};
  const run = {id: 1, provider: "aws", ref: "refs/heads/main", commit: "abc", config: "buildspec.yml"};
  await triggerCIRun(entry, run);
  if (globalThis.invokeInput.FunctionName !== ciMaterializerFunctionName) throw new Error("Lambda function name was not used");
  if (globalThis.materializerPayload.headers["x-bgit-ci-token"] !== "old-token") throw new Error("materializer token header missing");
  if (run.url !== "https://build.example/1") throw new Error("materializer response was not applied");
  await rotateCIMaterializerToken();
  if (secret !== "new-token") throw new Error("rotation did not update secret");
})();
`
}

func extractMust(source, name string) string {
	fn, err := extractJavaScriptFunction(source, name)
	if err != nil {
		return "throw new Error(" + strconv.Quote(err.Error()) + ");"
	}
	return fn
}

func extractJavaScriptFunction(source, name string) (string, error) {
	start := strings.Index(source, "function "+name+"(")
	if asyncStart := strings.Index(source, "async function "+name+"("); asyncStart >= 0 && (start < 0 || asyncStart < start) {
		start = asyncStart
	}
	if start < 0 {
		return "", fmt.Errorf("missing JavaScript function %s", name)
	}
	open := strings.Index(source[start:], "{")
	if open < 0 {
		return "", fmt.Errorf("missing function body for %s", name)
	}
	open += start
	depth := 0
	inString := byte(0)
	escape := false
	for i := open; i < len(source); i++ {
		ch := source[i]
		if inString != 0 {
			if escape {
				escape = false
				continue
			}
			if ch == '\\' {
				escape = true
				continue
			}
			if ch == inString {
				inString = 0
			}
			continue
		}
		switch ch {
		case '\'', '"', '`':
			inString = ch
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unterminated JavaScript function %s", name)
}

func TestBrokerSignatureMessageIsStable(t *testing.T) {
	a := brokerSignatureMessage(http.MethodPost, "/repos/mine", "broker.example.com", "1770000000", "nonce", []byte(`{"repo":"demo"}`))
	b := brokerSignatureMessage(http.MethodPost, "/repos/mine", "broker.example.com", "1770000000", "nonce", []byte(`{"repo":"demo"}`))
	c := brokerSignatureMessage(http.MethodPost, "/repos/mine", "broker.example.com", "1770000000", "nonce", []byte(`{"repo":"other"}`))
	if !bytes.Equal(a, b) {
		t.Fatalf("signature message is not stable")
	}
	if bytes.Equal(a, c) {
		t.Fatalf("signature message should depend on payload")
	}
	if !strings.HasPrefix(string(a), "bgit-broker-v2\nPOST\n/repos/mine\nbroker.example.com\n1770000000\nnonce\n") {
		t.Fatalf("signature message = %q", string(a))
	}
}

func TestBrokerForbiddenAllowsSignatureRetryOnlyForAuthFailures(t *testing.T) {
	for _, msg := range []string{
		`{"error":"write SSH signature required"}`,
		`{"error":"owner SSH signature required"}`,
		`admin SSH signature required`,
	} {
		if !brokerForbiddenAllowsSignatureRetry(msg) {
			t.Fatalf("expected auth retry for %q", msg)
		}
	}
	for _, msg := range []string{
		`{"error":"protected branch refs/heads/main requires a pull request"}`,
		`{"error":"repository is read-only"}`,
		`{"error":"owners cannot be removed or suspended"}`,
		`forbidden`,
		``,
	} {
		if brokerForbiddenAllowsSignatureRetry(msg) {
			t.Fatalf("did not expect auth retry for %q", msg)
		}
	}
}

func TestBrokerHTTPErrorHintsAtIncompatibleBrokerOnSignatureFailure(t *testing.T) {
	err := brokerHTTPError("/auth/status", `{"error":"SSH signature required"}`)
	if err == nil || !strings.Contains(err.Error(), "incompatible with this bgit version") || !strings.Contains(err.Error(), "bgit admin broker upgrade") {
		t.Fatalf("error = %v", err)
	}
	err = brokerHTTPError("/refs/update", `{"error":"stale ref"}`)
	if strings.Contains(err.Error(), "incompatible with this bgit version") {
		t.Fatalf("unexpected compatibility hint: %v", err)
	}
}

func TestMergeConfigUsesRepoAuthUnlessExplicit(t *testing.T) {
	local := config{auth: "adc", gcloudConfiguration: "test-profile"}
	merged := mergeConfig(config{auth: "gcloud"}, local)
	if merged.auth != "adc" || merged.gcloudConfiguration != "test-profile" {
		t.Fatalf("merged = %#v", merged)
	}
	merged = mergeConfig(config{auth: "gcloud", authExplicit: true, gcloudConfiguration: "default", gcloudConfigurationExplicit: true}, local)
	if merged.auth != "gcloud" || merged.gcloudConfiguration != "default" {
		t.Fatalf("explicit merged = %#v", merged)
	}
}

func TestMergeConfigUsesRepoRegion(t *testing.T) {
	local := config{region: "eu-west-1"}
	merged := mergeConfig(config{}, local)
	if merged.region != "eu-west-1" {
		t.Fatalf("merged region = %q", merged.region)
	}
	merged = mergeConfig(config{region: "us-west-2"}, local)
	if merged.region != "us-west-2" {
		t.Fatalf("explicit region = %q", merged.region)
	}
}

func TestDefaultAWSRegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	if got := defaultAWSRegion(); got != "us-east-1" {
		t.Fatalf("default region = %q", got)
	}
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")
	if got := defaultAWSRegion(); got != "eu-west-1" {
		t.Fatalf("default region fallback = %q", got)
	}
	t.Setenv("AWS_REGION", "eu-central-1")
	if got := defaultAWSRegion(); got != "eu-central-1" {
		t.Fatalf("default region env = %q", got)
	}
}

func TestAWSRegionPrefersExplicitConfig(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-central-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")
	if got := awsRegion(config{region: "ap-southeast-2"}); got != "ap-southeast-2" {
		t.Fatalf("explicit region = %q", got)
	}
	if got := awsRegion(config{}); got != "eu-central-1" {
		t.Fatalf("fallback region = %q", got)
	}
}

func TestAnonymousS3ClientUsesExplicitRegion(t *testing.T) {
	client, err := newS3Client(context.Background(), config{region: "ap-southeast-2"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.Options().Region; got != "ap-southeast-2" {
		t.Fatalf("client region = %q", got)
	}
}

func TestInitEmptyWorktreeDoesNotRequireOrigin(t *testing.T) {
	target := filepath.Join(t.TempDir(), "repo")
	var stdout bytes.Buffer
	if err := initEmptyWorktree([]string{target}, &stdout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		t.Fatal(err)
	}
	branchOut, err := runGit(target, "branch", "--show-current")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(branchOut)) != defaultBranch {
		t.Fatalf("branch = %q", string(branchOut))
	}
	_, err = runGit(target, "remote", "get-url", "origin")
	if err == nil {
		t.Fatal("did not expect origin remote")
	}
}

func TestReadLocalConfigFallsBackToGCSRemoteOrigin(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", target); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "remote", "add", "origin", "gs://bucket-name/path/repo.git"); err != nil {
		t.Fatal(err)
	}
	cfg, err := readLocalConfig(target)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.bucket != "bucket-name" || cfg.prefix != "path/repo.git" {
		t.Fatalf("cfg = %#v", cfg)
	}
}

func TestWriteBucketGitConfigPersistsSelectedAuthDefaults(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", target); err != nil {
		t.Fatal(err)
	}
	cfg := config{
		provider:            "gcs",
		bucket:              "bucket-name",
		prefix:              "path/repo.git",
		branch:              defaultBranch,
		auth:                "adc",
		gcloudConfiguration: "work",
	}
	if err := writeBucketGitConfig(target, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := readLocalConfig(target)
	if err != nil {
		t.Fatal(err)
	}
	if got.gcloudConfiguration != "work" || got.auth != "adc" {
		t.Fatalf("cfg = %#v", got)
	}
}

func TestMissingOriginErrorIncludesCopyPasteCommands(t *testing.T) {
	err := missingOriginError()
	if err == nil {
		t.Fatal("expected error")
	}
	text := err.Error()
	for _, want := range []string{
		"No configured push destination.",
		"bgit direct origin gs://bucket-name/path/to/repo.git",
		"bgit direct push",
		"bgit --bucket bucket-name --prefix path/to/repo.git direct push",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestPushWithoutOriginReportsSetupBeforeGCSClient(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", target); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"direct", "push"}, strings.NewReader(""), ioDiscard{}, ioDiscard{})
	if err == nil {
		t.Fatal("expected missing origin error")
	}
	if !errors.Is(err, missingOriginError()) && !strings.Contains(err.Error(), "No configured push destination.") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStatusCommandPrintsNativeStatus(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := statusCommand([]string{"--short"}, &stdout); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	for _, want := range []string{"?? README.md"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestStatusCommandPrintsLongStatusByDefault(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "tracked.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "Ada"},
		{"config", "user.email", "ada@example.com"},
		{"add", "tracked.txt"},
		{"commit", "-m", "Initial"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, "tracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := statusCommand(nil, &stdout); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	for _, want := range []string{
		"On branch main",
		"Changes not staged for commit:",
		"modified:   tracked.txt",
		"Untracked files:",
		"README.md",
		"no changes added to commit",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, " M tracked.txt") || strings.Contains(text, "?? README.md") {
		t.Fatalf("long status should not use short status lines:\n%s", text)
	}
}

func TestStatusCommandRespectsGitignore(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".gitignore"), []byte("*.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := statusCommand([]string{"--short"}, &stdout); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	if strings.Contains(text, "README.md") {
		t.Fatalf("ignored file should not be listed:\n%s", text)
	}
	for _, want := range []string{"?? .gitignore", "?? main.go"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestStatusCommandReportsTrackedIgnoredFiles(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "Ada"},
		{"config", "user.email", "ada@example.com"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	ignoredDir := filepath.Join(target, "vendor")
	if err := os.MkdirAll(ignoredDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ignoredDir, "keep.go"), []byte("package keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-m", "Initial"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".gitignore"), []byte("vendor/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ignoredDir, "keep.go"), []byte("package keep\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ignoredDir, "temp.go"), []byte("package temp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := statusCommand([]string{"--short"}, &stdout); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	if !strings.Contains(text, " M vendor/keep.go") {
		t.Fatalf("tracked ignored file modification should be listed:\n%s", text)
	}
	if strings.Contains(text, "vendor/temp.go") {
		t.Fatalf("untracked ignored file should not be listed:\n%s", text)
	}
}

func TestStatusCommandPrintsPathsRelativeToCurrentDirectory(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "Ada"},
		{"config", "user.email", "ada@example.com"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	subdir := filepath.Join(target, "terraform")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "README.md"), []byte("# Terraform\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-m", "Initial"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("# Demo\nchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "README.md"), []byte("# Terraform\nchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(subdir); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := statusCommand([]string{"--short"}, &stdout); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	for _, want := range []string{" M ../README.md", " M README.md"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "terraform/README.md") {
		t.Fatalf("status path should be relative to cwd:\n%s", text)
	}
}

func TestResetPathRestoresIndexFromHead(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "Ada"},
		{"config", "user.email", "ada@example.com"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, ".gitignore"), []byte("*.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", ".gitignore"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-m", "Initial"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(target, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", ".gitignore"); err != nil {
		t.Fatal(err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("reset", []string{".gitignore"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := statusCommand([]string{"--short"}, &stdout); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	if !strings.Contains(text, " D .gitignore") {
		t.Fatalf("worktree deletion should remain unstaged:\n%s", text)
	}
	if strings.Contains(text, "D  .gitignore") {
		t.Fatalf("deletion should not remain staged:\n%s", text)
	}
	if err := localGitCommand("reset", []string{".gitignore"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckoutPathRestoresFileFromHead(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "Ada"},
		{"config", "user.email", "ada@example.com"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, ".gitignore"), []byte("*.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", ".gitignore"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-m", "Initial"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(target, ".gitignore")); err != nil {
		t.Fatal(err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("checkout", []string{".gitignore"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "*.md\n" {
		t.Fatalf(".gitignore = %q", string(data))
	}
	var stdout bytes.Buffer
	if err := statusCommand([]string{"--short"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "" {
		t.Fatalf("expected clean status after checkout path, got:\n%s", stdout.String())
	}
}

func TestCheckoutPathWithSeparatorRestoresFileFromHead(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "Ada"},
		{"config", "user.email", "ada@example.com"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, ".gitignore"), []byte("*.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", ".gitignore"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-m", "Initial"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(target, ".gitignore")); err != nil {
		t.Fatal(err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("checkout", []string{"--", ".gitignore"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "*.md\n" {
		t.Fatalf(".gitignore = %q", string(data))
	}
}

func TestCheckoutPathCanUseSourceRevision(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "Ada"},
		{"config", "user.email", "ada@example.com"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(target, "README.md")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-m", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-m", "v2"); err != nil {
		t.Fatal(err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("checkout", []string{"HEAD~1", "--", "README.md"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "v1\n" {
		t.Fatalf("README.md = %q", string(data))
	}
	var stdout bytes.Buffer
	if err := statusCommand([]string{"--short"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "M  README.md") {
		t.Fatalf("expected checkout path to stage source revision, got:\n%s", stdout.String())
	}
}

func TestAddCommandStagesLocalFilesWithoutOrigin(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("add", []string{"README.md"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	out, err := runGit(target, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "A  README.md") {
		t.Fatalf("status = %q", string(out))
	}
}

func TestRmMvAndTagWorkWithoutOrigin(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old.md"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("add", []string{"old.md"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada",
		"GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada",
		"GIT_COMMITTER_EMAIL=ada@example.com",
	)
	if _, err := runGitEnv(target, env, "commit", "-m", "Add old"); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("mv", []string{"old.md", "new.md"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if _, err := runGitEnv(target, env, "commit", "-m", "Rename old"); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("tag", []string{"v1"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("rm", []string{"new.md"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	out, err := runGit(target, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "D  new.md") {
		t.Fatalf("status = %q", string(out))
	}
	tagOut, err := runGit(target, "tag", "--list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(tagOut)) != "v1" {
		t.Fatalf("tags = %q", string(tagOut))
	}
}

func TestMoveReportsRenameLikeGit(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "Ada"},
		{"config", "user.email", "ada@example.com"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("add", []string{"old.txt"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("commit", []string{"-m", "Add old"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("mv", []string{"old.txt", "new.txt"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	var status bytes.Buffer
	if err := statusCommand([]string{"--short"}, &status); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.String(), "R  old.txt -> new.txt") {
		t.Fatalf("status = %q", status.String())
	}
	var commit bytes.Buffer
	if err := localGitCommand("commit", []string{"-am", "Move old"}, &commit); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 file changed, 0 insertions(+), 0 deletions(-)", "rename old.txt => new.txt (100%)"} {
		if !strings.Contains(commit.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, commit.String())
		}
	}
}

func TestCommitCheckoutBranchAndLogWorkWithoutOrigin(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "config", "user.name", "Ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "config", "user.email", "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("add", []string{"README.md"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("commit", []string{"-m", "Initial"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("checkout", []string{"-b", "feature"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("add", []string{"feature.txt"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("commit", []string{"-m", "Feature"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "feature.txt")); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := localGitCommand("branch", nil, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "* feature") {
		t.Fatalf("branches = %q", stdout.String())
	}
	stdout.Reset()
	if err := localGitCommand("log", []string{"--oneline", "-n", "2"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Feature") {
		t.Fatalf("log = %q", stdout.String())
	}
}

func TestLocalBranchLifecycleMaintainsOriginTracking(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "Ada"},
		{"config", "user.email", "ada@example.com"},
		{"remote", "add", "origin", "git@git.bucketgit.com:app.git"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("add", []string{"README.md"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("commit", []string{"-m", "Initial"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("checkout", []string{"-b", "feature/web"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"branch.feature/web.remote": "origin",
		"branch.feature/web.merge":  "refs/heads/feature/web",
	} {
		out, err := runGit(target, "config", "--local", "--get", key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if err := localGitCommand("checkout", []string{"main"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("branch", []string{"-D", "feature/web"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "config", "--local", "--get", "branch.feature/web.remote"); err == nil {
		t.Fatal("branch.feature/web.remote still configured after branch delete")
	}
	if _, err := runGit(target, "config", "--local", "--get", "branch.feature/web.merge"); err == nil {
		t.Fatal("branch.feature/web.merge still configured after branch delete")
	}
}

func TestCheckoutCarriesCompatibleLocalChanges(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.name", "Ada"}, {"config", "user.email", "ada@example.com"}} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("TEST\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-m", "Initial"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "checkout", "-b", "barfoo"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("TEST\nTEST2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := localGitCommand("checkout", []string{"main"}, &stdout); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "TEST\nTEST2\n" {
		t.Fatalf("README.md = %q", string(data))
	}
	if !strings.Contains(stdout.String(), "Switched to branch 'main'") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestCheckoutRejectsOverwrittenLocalChanges(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.name", "Ada"}, {"config", "user.email", "ada@example.com"}} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-m", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "checkout", "-b", "barfoo"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-am", "branch"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	err = localGitCommand("checkout", []string{"main"}, ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "would be overwritten by checkout") {
		t.Fatalf("err = %v", err)
	}
	if branch, _ := runGit(target, "branch", "--show-current"); strings.TrimSpace(string(branch)) != "barfoo" {
		t.Fatalf("branch = %q", string(branch))
	}
	data, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "dirty\n" {
		t.Fatalf("README.md = %q", string(data))
	}
}

func TestExpandedNativePorcelainCommands(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "Ada"},
		{"config", "user.email", "ada@example.com"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("add", []string{"README.md"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("commit", []string{"-m", "Initial"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("tag", []string{"v1.0.0"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		cmd  string
		args []string
		want string
	}{
		{"diff", nil, "+world"},
		{"grep", []string{"world"}, "README.md:2:world"},
		{"ls-files", nil, "README.md"},
		{"ls-tree", []string{"-r", "HEAD"}, "README.md"},
		{"show", []string{"HEAD:README.md"}, "hello"},
		{"describe", nil, "v1.0.0"},
		{"blame", []string{"README.md"}, "Ada"},
	}
	for _, check := range checks {
		var stdout bytes.Buffer
		if err := localGitCommand(check.cmd, check.args, &stdout); err != nil {
			t.Fatalf("%s: %v", check.cmd, err)
		}
		if !strings.Contains(stdout.String(), check.want) {
			t.Fatalf("%s output missing %q:\n%s", check.cmd, check.want, stdout.String())
		}
	}

	if err := localGitCommand("restore", []string{"README.md"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("restore data = %q", string(data))
	}
	if err := os.WriteFile(filepath.Join(target, "temp.txt"), []byte("temp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := localGitCommand("clean", []string{"-f"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "temp.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("temp.txt still exists: %v", err)
	}
}

func TestCherryPickRevertBranchTagAndStashOutput(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "Ada"},
		{"config", "user.email", "ada@example.com"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("add", []string{"base.txt"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("commit", []string{"-m", "Base"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("checkout", []string{"-b", "feature"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("add", []string{"feature.txt"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("commit", []string{"-m", "Add feature"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := localGitCommand("tag", []string{"v-delete"}, &stdout); err != nil {
		t.Fatal(err)
	}
	featureHashOut, err := runGit(target, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	featureHash := strings.TrimSpace(string(featureHashOut))
	stdout.Reset()
	if err := localGitCommand("checkout", []string{"main"}, &stdout); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := localGitCommand("cherry-pick", []string{featureHash}, &stdout); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[main ", "Add feature", "1 file changed, 1 insertion(+)", "create mode 100644 feature.txt"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("cherry-pick output missing %q:\n%s", want, stdout.String())
		}
	}
	stdout.Reset()
	if err := localGitCommand("revert", []string{"HEAD"}, &stdout); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[main ", "Revert \"Add feature\"", "1 file changed, 1 deletion(-)", "delete mode 100644 feature.txt"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("revert output missing %q:\n%s", want, stdout.String())
		}
	}
	stdout.Reset()
	if err := localGitCommand("branch", []string{"-D", "feature"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Deleted branch feature (was ") {
		t.Fatalf("branch delete output = %q", stdout.String())
	}
	stdout.Reset()
	if err := localGitCommand("tag", []string{"-d", "v-delete"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != "Deleted tag 'v-delete'" {
		t.Fatalf("tag delete output = %q", stdout.String())
	}
	if err := os.WriteFile(filepath.Join(target, "base.txt"), []byte("base\nstashed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := localGitCommand("stash", []string{"push"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Saved working directory and index state WIP on main:") {
		t.Fatalf("stash push output = %q", stdout.String())
	}
	stdout.Reset()
	if err := localGitCommand("stash", []string{"drop"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Dropped refs/stash@{0}") {
		t.Fatalf("stash drop output = %q", stdout.String())
	}
}

func TestNativeConfigCommand(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("config", []string{"user.name", "Ada"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("config", []string{"remote.origin.url", "gs://bucket/repo.git"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := localGitCommand("config", []string{"--get", "user.name"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != "Ada" {
		t.Fatalf("user.name = %q", stdout.String())
	}
	stdout.Reset()
	if err := localGitCommand("config", []string{"--list"}, &stdout); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"user.name=Ada", "remote.origin.url=gs://bucket/repo.git"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, stdout.String())
		}
	}
	if err := localGitCommand("config", []string{"--unset", "user.name"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	err = localGitCommand("config", []string{"user.name"}, &stdout)
	if err != nil {
		t.Fatalf("missing key should not error: %v", err)
	}
	if stdout.String() != "" {
		t.Fatalf("missing key output = %q", stdout.String())
	}
}

func TestGlobalIdentityConfigCommand(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	if err := globalConfigCommand([]string{"--global", "user.name", "Dennis Example"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := globalConfigCommand([]string{"--global", "user.email", "dennis@example.com"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := globalConfigCommand([]string{"--global", "--get", "user.name"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != "Dennis Example" {
		t.Fatalf("user.name = %q", stdout.String())
	}
	cfg, err := readGlobalConfig(filepath.Join(home, ".bgit", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Identity.Name != "Dennis Example" || cfg.Identity.Email != "dennis@example.com" {
		t.Fatalf("identity = %#v", cfg.Identity)
	}
}

func TestPushUpdatesBareRepo(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "repo.git")
	target := filepath.Join(root, "checkout")

	if _, err := runGit("", "init", "--bare", bare); err != nil {
		t.Fatal(err)
	}
	repo := newNativeGitRepoForStore(config{bucket: "bucket", prefix: "repos/demo.git", branch: "main"}, &localGitStore{root: bare})
	if err := repo.initWorktree(context.Background(), []string{target}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada",
		"GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada",
		"GIT_COMMITTER_EMAIL=ada@example.com",
	)
	if _, err := runGitEnv(target, env, "commit", "-m", "Initial commit"); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := repo.push(context.Background(), nil, &stdout); err != nil {
		t.Fatal(err)
	}
	out, err := runGit("", "--git-dir", bare, "log", "--oneline", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Initial commit") {
		t.Fatalf("bare log = %q", string(out))
	}
}

func TestNativeDiffSupportsRevisionOperands(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "config", "user.name", "Ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "config", "user.email", "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-m", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "checkout", "-b", "barfoo"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("main\nbarfoo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-am", "barfoo"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "checkout", "main"); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := localGitCommand("diff", []string{"barfoo"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "-barfoo") {
		t.Fatalf("diff barfoo = %q", stdout.String())
	}
	stdout.Reset()
	err = localGitCommand("diff", []string{"foobar"}, &stdout)
	if err == nil || !strings.Contains(err.Error(), `unknown revision "foobar"`) {
		t.Fatalf("err = %v stdout=%q", err, stdout.String())
	}
}

func TestNativeResetHardSupportsHeadAncestorAndRemovesFiles(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "config", "user.name", "Ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "config", "user.email", "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-m", "Initial"); err != nil {
		t.Fatal(err)
	}
	initial, err := runGit(target, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "reset.txt"), []byte("reset\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "add", "reset.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(target, "commit", "-m", "Reset target"); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	if err := localGitCommand("reset", []string{"--hard", "HEAD~1"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	head, err := runGit(target, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(head)) != strings.TrimSpace(string(initial)) {
		t.Fatalf("HEAD = %q, want %q", head, initial)
	}
	if _, err := os.Stat(filepath.Join(target, "reset.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reset.txt stat err = %v", err)
	}
}

func TestNativeGitRepoReadsLooseObjects(t *testing.T) {
	bare := createBareFixture(t)
	repo := newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: bare})

	var stdout bytes.Buffer
	if err := repo.listFiles(context.Background(), nil, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "README.md") || !strings.Contains(stdout.String(), "docs/guide.md") {
		t.Fatalf("ls = %q", stdout.String())
	}

	stdout.Reset()
	if err := repo.catFile(context.Background(), []string{"docs/guide.md"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(stdout.String(), "\r\n", "\n") != "guide\n" {
		t.Fatalf("cat = %q", stdout.String())
	}

	stdout.Reset()
	if err := repo.log(context.Background(), []string{"--limit", "1"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Initial commit") {
		t.Fatalf("log = %q", stdout.String())
	}

	stdout.Reset()
	if err := repo.lsRemote(context.Background(), []string{"--heads"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "refs/heads/main") {
		t.Fatalf("ls-remote = %q", stdout.String())
	}
}

func TestNativeGitRepoReadsPackedObjects(t *testing.T) {
	bare := createBareFixture(t)
	if _, err := runGit("", "--git-dir", bare, "repack", "-ad"); err != nil {
		t.Fatal(err)
	}
	repo := newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: bare})

	var stdout bytes.Buffer
	if err := repo.catFile(context.Background(), []string{"README.md"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(stdout.String(), "\r\n", "\n") != "# Demo\n" {
		t.Fatalf("packed cat = %q", stdout.String())
	}

	stdout.Reset()
	if err := repo.listFiles(context.Background(), nil, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "docs/guide.md") {
		t.Fatalf("packed ls = %q", stdout.String())
	}
}

func TestNativeGitRepoPushWritesObjectsAndRefsWithoutBareSync(t *testing.T) {
	root := t.TempDir()
	remoteRoot := filepath.Join(root, "remote.git")
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(remoteRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit("", "init", "--initial-branch", "main", worktree); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "config", "user.name", "Ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "config", "user.email", "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "commit", "-m", "Initial commit"); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(worktree); err != nil {
		t.Fatal(err)
	}

	repo := newNativeGitRepoForStore(config{branch: "main", origin: "gs://bucket/repo.git"}, &localGitStore{root: remoteRoot})
	var stdout bytes.Buffer
	if err := repo.push(context.Background(), nil, &stdout); err != nil {
		t.Fatal(err)
	}
	refData, err := os.ReadFile(filepath.Join(remoteRoot, "refs", "heads", "main"))
	if err != nil {
		t.Fatal(err)
	}
	if !isHexHash(strings.TrimSpace(string(refData))) {
		t.Fatalf("remote ref = %q", string(refData))
	}

	stdout.Reset()
	readRepo := newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: remoteRoot})
	if err := readRepo.catFile(context.Background(), []string{"README.md"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(stdout.String(), "\r\n", "\n") != "# Demo\n" {
		t.Fatalf("remote cat = %q", stdout.String())
	}

	stdout.Reset()
	if err := repo.push(context.Background(), nil, &stdout); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != "Everything up-to-date" {
		t.Fatalf("second push stdout = %q", stdout.String())
	}
}

func TestNativeGitRepoPushTagsDoesNotMoveConfiguredBranch(t *testing.T) {
	root := t.TempDir()
	remoteRoot := filepath.Join(root, "remote.git")
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(remoteRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit("", "init", "--initial-branch", "main", worktree); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "config", "user.name", "Ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "config", "user.email", "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "commit", "-m", "Initial commit"); err != nil {
		t.Fatal(err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(worktree); err != nil {
		t.Fatal(err)
	}
	repo := newNativeGitRepoForStore(config{branch: "main", origin: "gs://bucket/repo.git"}, &localGitStore{root: remoteRoot})
	if err := repo.push(context.Background(), nil, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	mainBefore := strings.TrimSpace(string(mustReadFile(t, filepath.Join(remoteRoot, "refs", "heads", "main"))))

	if _, err := runGit(worktree, "checkout", "-b", "feature"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "add", "feature.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "commit", "-m", "Feature commit"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "tag", "v-test"); err != nil {
		t.Fatal(err)
	}
	if err := repo.push(context.Background(), []string{"--tags"}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}

	mainAfter := strings.TrimSpace(string(mustReadFile(t, filepath.Join(remoteRoot, "refs", "heads", "main"))))
	if mainAfter != mainBefore {
		t.Fatalf("push --tags moved main: before=%s after=%s", mainBefore, mainAfter)
	}
	tagData := strings.TrimSpace(string(mustReadFile(t, filepath.Join(remoteRoot, "refs", "tags", "v-test"))))
	if !isHexHash(tagData) || tagData == mainBefore {
		t.Fatalf("tag ref = %q, main = %q", tagData, mainBefore)
	}
}

func TestNativeGitRepoFetchCopiesObjectsAndRemoteRefs(t *testing.T) {
	remoteRoot := createBareFixture(t)
	worktree := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", worktree); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(worktree); err != nil {
		t.Fatal(err)
	}

	repo := newNativeGitRepoForStore(config{branch: "main", origin: "gs://bucket/repo.git"}, &localGitStore{root: remoteRoot})
	var stdout bytes.Buffer
	if err := repo.fetch(context.Background(), nil, &stdout); err != nil {
		t.Fatal(err)
	}
	out, err := runGit(worktree, "rev-parse", "--verify", "refs/remotes/bucketgit/main")
	if err != nil {
		t.Fatal(err)
	}
	if !isHexHash(strings.TrimSpace(string(out))) {
		t.Fatalf("remote tracking ref = %q", string(out))
	}
}

func TestNativeGitRepoPullDefaultsToCurrentBranch(t *testing.T) {
	remoteRoot := createBareFixture(t)
	worktree := t.TempDir()
	if _, err := runGit("", "clone", remoteRoot, worktree); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "config", "user.name", "Ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "config", "user.email", "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "checkout", "-b", "feature/dennis"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "add", "feature.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "commit", "-m", "Feature commit"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "push", remoteRoot, "HEAD:refs/heads/feature/dennis"); err != nil {
		t.Fatal(err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(worktree); err != nil {
		t.Fatal(err)
	}
	repo := newNativeGitRepoForStore(config{branch: "main", origin: "gs://bucket/repo.git"}, &localGitStore{root: remoteRoot})
	var stdout bytes.Buffer
	if err := repo.pull(context.Background(), nil, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Already up to date.") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestNativeGitRepoPushDefaultsToCurrentBranch(t *testing.T) {
	root := t.TempDir()
	remoteRoot := filepath.Join(root, "remote.git")
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(remoteRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit("", "init", "--initial-branch", "main", worktree); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "config", "user.name", "Ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "config", "user.email", "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "commit", "-m", "Initial commit"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "checkout", "-b", "barfoo"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("# Demo\n\nbarfoo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "commit", "-am", "Barfoo"); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(worktree); err != nil {
		t.Fatal(err)
	}
	repo := newNativeGitRepoForStore(config{branch: "main", origin: "gs://bucket/repo.git"}, &localGitStore{root: remoteRoot})
	var stdout bytes.Buffer
	if err := repo.push(context.Background(), nil, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "barfoo -> barfoo") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "refs", "heads", "barfoo")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "refs", "heads", "main")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("main ref err = %v", err)
	}
}

func TestNativeGitRepoPutFileCommitsAndPushesWithoutBareSync(t *testing.T) {
	root := t.TempDir()
	remoteRoot := filepath.Join(root, "remote.git")
	if _, err := runGit("", "init", "--bare", remoteRoot); err != nil {
		t.Fatal(err)
	}
	repo := newNativeGitRepoForStore(
		config{bucket: "bucket", prefix: "repo.git", branch: "main", origin: "gs://bucket/repo.git"},
		&localGitStore{root: remoteRoot},
	)
	var stdout bytes.Buffer
	if err := repo.putFile(
		context.Background(),
		[]string{"README.md", "-m", "Add README", "--author", "Ada", "--email", "ada@example.com"},
		strings.NewReader("# Demo\n"),
		&stdout,
	); err != nil {
		t.Fatal(err)
	}
	if !isHexHash(strings.TrimSpace(stdout.String())) {
		t.Fatalf("put stdout = %q", stdout.String())
	}

	stdout.Reset()
	readRepo := newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: remoteRoot})
	if err := readRepo.catFile(context.Background(), []string{"README.md"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(stdout.String(), "\r\n", "\n") != "# Demo\n" {
		t.Fatalf("remote cat = %q", stdout.String())
	}
}

func createBareFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "repo.git")
	source := filepath.Join(root, "source")
	if _, err := runGit("", "init", "--bare", bare); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit("", "init", "--initial-branch", "main", source); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "docs", "guide.md"), []byte("guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(source, "add", "."); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada",
		"GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada",
		"GIT_COMMITTER_EMAIL=ada@example.com",
	)
	if _, err := runGitEnv(source, env, "commit", "-m", "Initial commit"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(source, "push", bare, "HEAD:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	return bare
}

func TestWebHandlerServesTreeBlobAndRaw(t *testing.T) {
	bare := createBareFixture(t)
	repo := newNativeGitRepoForStore(config{branch: "main", origin: "gs://bucket/repo.git"}, &localGitStore{root: bare})
	handler := newWebHandler(repo, config{branch: "main", origin: "gs://bucket/repo.git"})

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/", want: "README.md"},
		{path: "/", want: "Initial commit"},
		{path: "/tree/docs", want: "guide.md"},
		{path: "/blob/README.md", want: "# Demo"},
		{path: "/raw/README.md", want: "# Demo\n"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", tc.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s body does not contain %q:\n%s", tc.path, tc.want, rec.Body.String())
		}
	}
}

func TestWebHandlerRendersBranchSelector(t *testing.T) {
	bare := createBareFixture(t)
	worktree := t.TempDir()
	if _, err := runGit("", "clone", bare, worktree); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "checkout", "-b", "feature/web-ui"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("# Feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada",
		"GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada",
		"GIT_COMMITTER_EMAIL=ada@example.com",
	)
	if _, err := runGitEnv(worktree, env, "commit", "-m", "Feature branch"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(worktree, "push", bare, "HEAD:refs/heads/feature/web-ui"); err != nil {
		t.Fatal(err)
	}

	repo := newNativeGitRepoForStore(config{branch: "main", origin: "gs://bucket/repo.git"}, &localGitStore{root: bare})
	handler := newWebHandler(repo, config{branch: "main", origin: "gs://bucket/repo.git"})
	req := httptest.NewRequest(http.MethodGet, "/?ref=refs/heads/feature/web-ui", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`data-ref-selector`, `refs/heads/main`, `refs/heads/feature/web-ui`, `# Feature`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestWebHandlerServesJSONAPI(t *testing.T) {
	bare := createBareFixture(t)
	repo := newNativeGitRepoForStore(config{branch: "main", origin: "gs://bucket/repo.git"}, &localGitStore{root: bare})
	handler := newWebHandler(repo, config{branch: "main", origin: "gs://bucket/repo.git"})

	for _, tc := range []struct {
		path string
		want []string
	}{
		{path: "/api/refs", want: []string{`"full_name":"refs/heads/main"`}},
		{path: "/api/tree?path=docs", want: []string{`"path":"docs/guide.md"`, `"kind":"file"`}},
		{path: "/api/blob?path=README.md", want: []string{`"encoding":"utf-8"`, `"content":"# Demo\n"`}},
		{path: "/api/commits", want: []string{`"subject":"Initial commit"`}},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", tc.path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
			t.Fatalf("%s content-type = %q", tc.path, got)
		}
		for _, want := range tc.want {
			if !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("%s body missing %q:\n%s", tc.path, want, rec.Body.String())
			}
		}
	}
}

func TestOpenWebRepositoryUsesBrokerFromRepoConfig(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "bucketgit.broker", "https://broker.example.test"},
		{"config", "bucketgit.logicalRepo", "app.git"},
		{"config", "bucketgit.provider", "gcs"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	repo, apiRepo, closeStore, cfg, err := openWebRepository(context.Background(), config{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	if _, ok := repo.store.(*localGitStore); !ok {
		t.Fatalf("seed store = %T, want *localGitStore", repo.store)
	}
	store, ok := apiRepo.store.(*brokerGitStore)
	if !ok {
		t.Fatalf("api store = %T, want *brokerGitStore", apiRepo.store)
	}
	if store.brokerURL != "https://broker.example.test" || cfg.brokerURL != "https://broker.example.test" || cfg.logicalRepo != "app.git" {
		t.Fatalf("store=%#v cfg=%#v", store, cfg)
	}
}

func TestOpenWebRepositoryLocalBypassesBroker(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", "--initial-branch", "main", target); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "bucketgit.broker", "https://broker.example.test"},
		{"config", "bucketgit.logicalRepo", "app.git"},
	} {
		if _, err := runGit(target, args...); err != nil {
			t.Fatal(err)
		}
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(target); err != nil {
		t.Fatal(err)
	}
	repo, apiRepo, closeStore, _, err := openWebRepository(context.Background(), config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	if _, ok := repo.store.(*localGitStore); !ok {
		t.Fatalf("store = %T, want *localGitStore", repo.store)
	}
	if apiRepo != repo {
		t.Fatalf("api repo should be local repo in --local mode")
	}
}

func TestWebClonePanelShowsBrokerCloneCommand(t *testing.T) {
	server := &webServer{cfg: config{
		brokerURL:   "https://broker.example.test/",
		logicalRepo: "app.git",
		teamID:      "t_marketing",
		origin:      "git@git.bucketgit.com:app.git",
	}}
	html := server.clonePanelHTML()
	if !strings.Contains(html, "app.git") {
		t.Fatalf("clone panel missing repo: %s", html)
	}
	if !strings.Contains(html, "bgit clone https://broker.example.test/t_marketing/app.git") {
		t.Fatalf("clone panel missing broker clone command: %s", html)
	}
	if !strings.Contains(html, "git@git.bucketgit.com:app.git") {
		t.Fatalf("clone panel missing ssh origin: %s", html)
	}
}

func TestWebRepoHeaderUsesShortTitleAndBrokerLocationBadge(t *testing.T) {
	cfg := config{
		brokerURL:   "https://broker.example.test/",
		logicalRepo: "app.git",
		teamID:      coreTeamID,
	}
	title := webRepoTitle(cfg)
	if title != "app.git" {
		t.Fatalf("title = %q", title)
	}
	server := &webServer{cfg: cfg, title: title}
	if badge := server.repoLocationBadge(); badge != "broker.example.test/core/app.git" {
		t.Fatalf("badge = %q", badge)
	}
	header := server.headerHTML("refs/heads/main", "")
	if strings.Contains(header, "bucketgit repository") {
		t.Fatalf("header should not include repository label: %s", header)
	}
	if !strings.Contains(header, `data-theme-toggle`) {
		t.Fatalf("header missing theme toggle: %s", header)
	}
	if strings.Contains(header, `href="/admin"`) {
		t.Fatalf("header should not include separate repo admin tab: %s", header)
	}
	if strings.Contains(header, `href="/broker-admin"`) || strings.Contains(header, "Broker Admin") {
		t.Fatalf("header should not include broker admin tab: %s", header)
	}
}

func TestWebBrokerAdminRouteIsGone(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "repo.git")
	if _, err := runGit("", "init", "--bare", bare); err != nil {
		t.Fatal(err)
	}
	handler := newWebHandlerWithAPI(newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: bare}), nil, config{branch: "main"})
	req := httptest.NewRequest(http.MethodGet, "/broker-admin", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWebHandlerCanRenderSeedThenRemote(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	for _, root := range []string{localRoot, remoteRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	localSource := filepath.Join(localRoot, "source")
	remoteSource := filepath.Join(remoteRoot, "source")
	localBare := filepath.Join(localRoot, "repo.git")
	remoteBare := filepath.Join(remoteRoot, "repo.git")
	for _, bare := range []string{localBare, remoteBare} {
		if _, err := runGit("", "init", "--bare", bare); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		worktree string
		bare     string
		text     string
		message  string
	}{
		{localSource, localBare, "# Local\n", "Local commit"},
		{remoteSource, remoteBare, "# Remote\n", "Remote commit"},
	} {
		if _, err := runGit("", "init", "--initial-branch", "main", item.worktree); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(item.worktree, "README.md"), []byte(item.text), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(item.worktree, "add", "README.md"); err != nil {
			t.Fatal(err)
		}
		env := append(os.Environ(), "GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com")
		if _, err := runGitEnv(item.worktree, env, "commit", "-m", item.message); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(item.worktree, "push", item.bare, "HEAD:refs/heads/main"); err != nil {
			t.Fatal(err)
		}
	}
	seed := newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: localBare})
	remote := newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: remoteBare})
	handler := newWebHandlerWithAPI(seed, remote, config{branch: "main", origin: "gs://bucket/repo.git"})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "# Local") || strings.Contains(rec.Body.String(), "# Remote") {
		t.Fatalf("seed body = %s", rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/?_remote=1", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "# Remote") || strings.Contains(rec.Body.String(), "# Local") {
		t.Fatalf("remote body = %s", rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/tree", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"subject":"Remote commit"`) {
		t.Fatalf("api body = %s", rec.Body.String())
	}
}

func TestWebMutationCSRFAndOriginValidation(t *testing.T) {
	handler := &webServer{csrfToken: "token"}
	req := httptest.NewRequest(http.MethodPost, "/api/actions/settings", strings.NewReader(`{}`))
	if err := handler.validateWebMutation(req); err == nil {
		t.Fatalf("missing CSRF token should fail")
	}
	req = httptest.NewRequest(http.MethodPost, "/api/actions/settings", strings.NewReader(`{}`))
	req.Header.Set("X-Bgit-CSRF", "wrong")
	if err := handler.validateWebMutation(req); err == nil {
		t.Fatalf("wrong CSRF token should fail")
	}
	req = httptest.NewRequest(http.MethodPost, "/api/actions/settings", strings.NewReader(`{}`))
	req.Header.Set("X-Bgit-CSRF", "token")
	req.Header.Set("Origin", "https://evil.example")
	if err := handler.validateWebMutation(req); err == nil {
		t.Fatalf("foreign origin should fail")
	}
	req = httptest.NewRequest(http.MethodPost, "/api/actions/settings", strings.NewReader(`{}`))
	req.Header.Set("X-Bgit-CSRF", "token")
	req.Header.Set("Origin", "http://localhost:8080")
	if err := handler.validateWebMutation(req); err != nil {
		t.Fatalf("local origin should pass: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/actions/settings", strings.NewReader(`{}`))
	req.Header.Set("X-Bgit-CSRF", "token")
	if err := handler.validateWebMutation(req); err != nil {
		t.Fatalf("absent origin with valid CSRF should pass: %v", err)
	}
}

func TestWebPullRequestCacheRendersPRTabAndPage(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "repo.git")
	if _, err := runGit("", "init", "--bare", bare); err != nil {
		t.Fatal(err)
	}
	handler := newWebHandlerWithAPI(
		newNativeGitRepoForStore(config{branch: "main"}, &localGitStore{root: bare}),
		nil,
		config{branch: "main", brokerURL: "https://broker.example.test", logicalRepo: "app.git", provider: "gcs"},
	)
	if err := handler.writePullRequestCache([]brokerPullRequest{{
		ID:        7,
		Title:     "Add docs",
		Source:    "refs/heads/docs",
		Target:    "refs/heads/main",
		Status:    "open",
		Approvals: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/prs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Add docs") || !strings.Contains(rec.Body.String(), `data-pr-tab`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(bare, "bucketgit", "cache", "prs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"title": "Add docs"`) {
		t.Fatalf("cache = %s", string(data))
	}
}

func TestParseWebArgs(t *testing.T) {
	opts, err := parseWebArgs([]string{"--local", "--addr", "0.0.0.0", "--port", "9000"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.local || opts.addr != "0.0.0.0" || opts.port != 9000 {
		t.Fatalf("opts = %#v", opts)
	}
	defaults, err := parseWebArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.addr != defaultWebAddr || defaults.port != defaultWebPort || defaults.local {
		t.Fatalf("defaults = %#v", defaults)
	}
}

func TestListenWebIncrementsWhenPortIsBusy(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	_, portText, err := net.SplitHostPort(occupied.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	if port >= 65535 {
		t.Skip("ephemeral port has no room to increment")
	}
	ln, err := listenWeb("127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ln.Addr().String() == occupied.Addr().String() {
		t.Fatalf("listenWeb reused occupied address %s", ln.Addr().String())
	}
	_, gotPortText, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	gotPort, err := strconv.Atoi(gotPortText)
	if err != nil {
		t.Fatal(err)
	}
	if gotPort != port+1 {
		t.Fatalf("port = %d, want %d", gotPort, port+1)
	}
}

func TestBrokerGitStoreReadsAndListsThroughBroker(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/objects/capability":
			var req brokerObjectCapabilityRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.Repo.Bucket != "bucket" || req.Path != "objects/aa/bb" || req.Operation != "read" {
				t.Fatalf("read req = %#v", req)
			}
			_, _ = fmt.Fprintf(w, `{"provider":"gcs","mode":"signed_url","method":"GET","url":%q}`, "http://"+r.Host+"/object")
		case "/object":
			_, _ = w.Write([]byte("object data"))
		case "/objects/list":
			var req brokerObjectRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.Repo.Prefix != "repos/demo.git" || req.Prefix != "refs/" {
				t.Fatalf("list req = %#v", req)
			}
			_, _ = w.Write([]byte(`{"paths":["refs/heads/main"]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	store := &brokerGitStore{brokerURL: server.URL, cfg: config{provider: "gcs", bucket: "bucket", prefix: "repos/demo.git", origin: "gs://bucket/repos/demo.git"}}
	data, err := store.read(context.Background(), "objects/aa/bb")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "object data" {
		t.Fatalf("data = %q", string(data))
	}
	listed, err := store.list(context.Background(), "refs/")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(listed, ",") != "refs/heads/main" {
		t.Fatalf("listed = %#v", listed)
	}
	if strings.Join(paths, ",") != "/objects/capability,/object,/objects/list" {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestBrokerGitStoreMapsMissingObjectToNotExist(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"No such object: bucket/repo.git/packed-refs"}`))
	}))
	defer server.Close()

	store := &brokerGitStore{brokerURL: server.URL, cfg: config{provider: "gcs", bucket: "bucket", prefix: "repo.git"}}
	_, err := store.read(context.Background(), "packed-refs")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
	if !isBrokerNotFoundError(errors.New(`broker /objects/read: {"error":"The specified key does not exist."}`)) {
		t.Fatalf("AWS missing key error was not recognized")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}
