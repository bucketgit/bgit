package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	if !strings.Contains(stdout.String(), "usage: bgit clone gs://bucket/prefix.git") {
		t.Fatalf("clone help = %q", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"clone", "help"}, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "usage: bgit clone gs://bucket/prefix.git") {
		t.Fatalf("clone help alias = %q", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"--help", "clone"}, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "usage: bgit clone gs://bucket/prefix.git") {
		t.Fatalf("--help clone = %q", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"clone", "--help"}, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "usage: bgit clone gs://bucket/prefix.git") {
		t.Fatalf("clone --help = %q", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"help", "create-gcloud-profile"}, strings.NewReader(""), &stdout, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "usage: bgit create-gcloud-profile") {
		t.Fatalf("create-gcloud-profile help = %q", stdout.String())
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

func TestSSHSetupWritesLocalConfigAndGitRemote(t *testing.T) {
	target := t.TempDir()
	if _, err := runGit("", "init", target); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(keyPath, []byte("ssh-ed25519 AAAATESTKEY ada@example.com\n"), 0o644); err != nil {
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
	var upsert brokerRepoRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/upsert" {
			t.Fatalf("unexpected broker path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&upsert); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	var stdout bytes.Buffer
	err = sshCommand(config{auth: "gcloud", branch: defaultBranch}, []string{
		"setup",
		"gs://bucket-name/path/repo.git",
		"--no-agent",
		"--key", keyPath,
		"--broker", server.URL,
	}, &stdout, ioDiscard{})
	if err != nil {
		t.Fatal(err)
	}
	remoteOut, err := runGit(target, "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(remoteOut)); got != "git@git.bucketgit.com:bucket-name/path/repo.git" {
		t.Fatalf("remote origin = %q", got)
	}
	for key, want := range map[string]string{
		"core.sshCommand":   "bgit ssh",
		"bucketgit.origin":  "gs://bucket-name/path/repo.git",
		"bucketgit.sshHost": "git.bucketgit.com",
		"bucketgit.broker":  server.URL,
		"bucketgit.sshkey1": "ssh-ed25519 AAAATESTKEY ada@example.com",
	} {
		out, err := runGit(target, "config", "--local", key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if !strings.Contains(stdout.String(), "configured SSH origin git@git.bucketgit.com:bucket-name/path/repo.git") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if upsert.Repo.Origin != "gs://bucket-name/path/repo.git" || upsert.AdminUser != "admin" || upsert.Role != "admin" {
		t.Fatalf("upsert = %#v", upsert)
	}
	if len(upsert.PublicKeys) != 1 || upsert.PublicKeys[0] != "ssh-ed25519 AAAATESTKEY ada@example.com" {
		t.Fatalf("upsert keys = %#v", upsert.PublicKeys)
	}
}

func TestSSHScaffoldInfersExistingOrigin(t *testing.T) {
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
	if err := sshCommand(config{auth: "gcloud", branch: defaultBranch}, []string{"scaffold"}, ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	remoteOut, err := runGit(target, "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(remoteOut)); got != "git@git.bucketgit.com:bucket-name/path/repo.git" {
		t.Fatalf("remote origin = %q", got)
	}
	providerOut, err := runGit(target, "config", "--local", "bucketgit.provider")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(providerOut)); got != "s3" {
		t.Fatalf("provider = %q", got)
	}
}

func TestSSHRepoAddAndKeysCommandsUseBroker(t *testing.T) {
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
		case "/repos/upsert", "/keys/add", "/keys/remove", "/keys/suspend":
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
	if err := sshCommand(config{auth: "gcloud", branch: defaultBranch}, []string{"repo", "add", "--no-agent", "--key", keyPath}, ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := sshCommand(config{auth: "gcloud", branch: defaultBranch}, []string{"keys", "add", "--no-agent", "--key", keyPath, "--user", "ada", "--role", "write"}, ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := sshCommand(config{auth: "gcloud", branch: defaultBranch}, []string{"keys", "list"}, &stdout, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "admin\tadmin\tactive\tssh-ed25519 AAAAADMIN admin@example.com") {
		t.Fatalf("keys list stdout = %q", stdout.String())
	}
	if err := sshCommand(config{auth: "gcloud", branch: defaultBranch}, []string{"keys", "suspend", "AAAAADMIN"}, ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err := sshCommand(config{auth: "gcloud", branch: defaultBranch}, []string{"keys", "remove", "AAAAADMIN"}, ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/repos/upsert", "/keys/add", "/keys/list", "/keys/suspend", "/keys/remove"}
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
		{match: "functions describe bgit-broker", stdout: "https://bgit-broker-provisioned.example.test", requireFile: marker, exitCode: 1},
		{match: "services enable"},
		{match: "firestore databases describe", exitCode: 1},
		{match: "firestore databases create"},
		{match: "functions deploy bgit-broker", touch: marker},
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
	match       string
	stdout      string
	exitCode    int
	touch       string
	requireFile string
}

func writeFakeCLI(t *testing.T, dir, name string, actions []fakeCLIAction) {
	t.Helper()
	path := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		path += ".bat"
	}
	var script strings.Builder
	if runtime.GOOS == "windows" {
		script.WriteString("@echo off\r\n")
		script.WriteString("set ARGS=%*\r\n")
		for _, action := range actions {
			finalExitCode := fakeCLIFinalExitCode(action)
			script.WriteString("echo %ARGS% | findstr /C:\"")
			script.WriteString(escapeBatch(action.match))
			script.WriteString("\" >nul\r\n")
			script.WriteString("if not errorlevel 1 (\r\n")
			if action.requireFile != "" {
				script.WriteString("  if not exist \"")
				script.WriteString(escapeBatch(action.requireFile))
				script.WriteString("\" exit /b ")
				script.WriteString(strconv.Itoa(firstNonZeroInt(action.exitCode, 1)))
				script.WriteString("\r\n")
			}
			if action.touch != "" {
				script.WriteString("  type nul > \"")
				script.WriteString(escapeBatch(action.touch))
				script.WriteString("\"\r\n")
			}
			if action.stdout != "" {
				script.WriteString("  echo ")
				script.WriteString(action.stdout)
				script.WriteString("\r\n")
			}
			script.WriteString("  exit /b ")
			script.WriteString(strconv.Itoa(finalExitCode))
			script.WriteString("\r\n)\r\n")
		}
		script.WriteString("exit /b 1\r\n")
	} else {
		script.WriteString("#!/bin/sh\n")
		script.WriteString("case \"$*\" in\n")
		for _, action := range actions {
			finalExitCode := fakeCLIFinalExitCode(action)
			script.WriteString("  *\"")
			script.WriteString(strings.ReplaceAll(action.match, `"`, `\"`))
			script.WriteString("\"*) ")
			if action.requireFile != "" {
				script.WriteString("[ -f '")
				script.WriteString(strings.ReplaceAll(action.requireFile, `'`, `'\''`))
				script.WriteString("' ] || exit ")
				script.WriteString(strconv.Itoa(firstNonZeroInt(action.exitCode, 1)))
				script.WriteString(" ; ")
			}
			if action.touch != "" {
				script.WriteString("touch '")
				script.WriteString(strings.ReplaceAll(action.touch, `'`, `'\''`))
				script.WriteString("' ; ")
			}
			if action.stdout != "" {
				script.WriteString("echo ")
				script.WriteString(action.stdout)
				script.WriteString(" ; ")
			}
			script.WriteString("exit ")
			script.WriteString(strconv.Itoa(finalExitCode))
			script.WriteString(" ;;\n")
		}
		script.WriteString("  *) exit 1 ;;\n")
		script.WriteString("esac\n")
	}
	if err := os.WriteFile(path, []byte(script.String()), 0o755); err != nil {
		t.Fatal(err)
	}
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

func escapeBatch(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
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
		"ConditionalCheckFailedException",
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
		"runTransaction",
	} {
		if !strings.Contains(string(index), want) {
			t.Fatalf("GCP broker source missing %q:\n%s", want, string(index))
		}
	}
}

func TestBrokerSignatureMessageIsStable(t *testing.T) {
	a := brokerSignatureMessage([]byte(`{"repo":"demo"}`))
	b := brokerSignatureMessage([]byte(`{"repo":"demo"}`))
	c := brokerSignatureMessage([]byte(`{"repo":"other"}`))
	if !bytes.Equal(a, b) {
		t.Fatalf("signature message is not stable")
	}
	if bytes.Equal(a, c) {
		t.Fatalf("signature message should depend on payload")
	}
	if !strings.HasPrefix(string(a), "bgit-broker-v1\n") {
		t.Fatalf("signature message = %q", string(a))
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

func TestMissingOriginErrorIncludesCopyPasteCommands(t *testing.T) {
	err := missingOriginError()
	if err == nil {
		t.Fatal("expected error")
	}
	text := err.Error()
	for _, want := range []string{
		"No configured push destination.",
		"bgit origin gs://bucket-name/path/to/repo.git",
		"bgit push",
		"bgit --bucket bucket-name --prefix path/to/repo.git push",
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
	err = run([]string{"push"}, strings.NewReader(""), ioDiscard{}, ioDiscard{})
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
		case "/objects/read":
			var req brokerObjectRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.Repo.Bucket != "bucket" || req.Path != "objects/aa/bb" {
				t.Fatalf("read req = %#v", req)
			}
			_, _ = fmt.Fprintf(w, `{"data":%q}`, base64.StdEncoding.EncodeToString([]byte("object data")))
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
	if strings.Join(paths, ",") != "/objects/read,/objects/list" {
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
