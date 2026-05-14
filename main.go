package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const defaultBranch = "main"
const defaultAuthMode = "gcloud"

var version = "dev"

type config struct {
	provider                    string
	bucket                      string
	prefix                      string
	branch                      string
	origin                      string
	auth                        string
	gcloudConfiguration         string
	authExplicit                bool
	gcloudConfigurationExplicit bool
	versionRequested            bool
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cfg, rest, err := parseGlobalFlags(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		if cfg.versionRequested {
			return versionCommand(stdout)
		}
		return usage(stderr)
	}
	if cfg.versionRequested {
		return versionCommand(stdout)
	}
	if rest[0] == "version" || rest[0] == "-v" || rest[0] == "--version" {
		return versionCommand(stdout)
	}
	if rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help" {
		return helpCommand(rest[1:], stdout)
	}

	cmd := rest[0]
	cmdArgs := rest[1:]
	if commandWantsVersion(cmdArgs) {
		return versionCommand(stdout)
	}
	if commandWantsHelp(cmdArgs) {
		return commandHelp(cmd, stdout)
	}
	explicitBucket := cfg.bucket != "" || cfg.prefix != ""
	if isUnsupportedCommand(cmd) && !(cmd == "show" && explicitBucket) {
		return unsupportedCommand(cmd)
	}
	if cmd == "origin" {
		return originCommand(cmdArgs, stdout)
	}
	if cmd == "remote" {
		return remoteCommand(cmdArgs, stdout)
	}
	if cmd == "admin" {
		return adminCommand(cfg, cmdArgs, stdout)
	}
	if cmd == "ssh" {
		return sshCommand(cfg, cmdArgs, stdout, stderr)
	}
	if cmd == "web" {
		return webCommand(context.Background(), cfg, cmdArgs, stdout)
	}
	if cmd == "create-gcloud-profile" {
		return createGcloudProfileCommand(cmdArgs, stdin, stdout)
	}
	if isLocalGitCommand(cmd) || (!explicitBucket && isPreferLocalGitCommand(cmd)) {
		return nativeLocalCommand(cmd, cmdArgs, stdout)
	}

	ctx := context.Background()
	if cmd == "clone" {
		return cloneCommand(ctx, cfg, cmdArgs, stdout)
	}
	if cmd == "init" && cfg.bucket == "" {
		return initEmptyWorktree(cmdArgs, stdout)
	}
	if cfg.bucket == "" {
		localCfg, err := readLocalConfig(".")
		if err == nil {
			cfg = mergeConfig(cfg, localCfg)
		}
	}
	if cfg.bucket == "" {
		if cmd == "push" {
			return missingOriginError()
		}
		return errors.New("--bucket is required outside a bucketgit checkout")
	}

	store, closeStore, err := newRemoteStore(ctx, cfg, isReadOnlyRemoteCommand(cmd))
	if err != nil {
		return fmt.Errorf("create remote store: %w", err)
	}
	defer closeStore()

	if isNativeRemoteCommand(cmd) {
		if commandCreatesBucket(cmd) || cmd == "push" {
			if err := ensureBucket(ctx, cfg); err != nil {
				return err
			}
		}
		repo := openNativeGitRepo(store, cfg)
		switch cmd {
		case "init":
			return repo.initWorktree(ctx, cmdArgs, stdout)
		case "fetch":
			return repo.fetch(ctx, cmdArgs, stdout)
		case "pull":
			return repo.pull(ctx, cmdArgs, stdout)
		case "push":
			return repo.push(ctx, cmdArgs, stdout)
		case "ls-remote":
			return repo.lsRemote(ctx, cmdArgs, stdout)
		case "ls", "list":
			return repo.listFiles(ctx, cmdArgs, stdout)
		case "cat", "show":
			return repo.catFile(ctx, cmdArgs, stdout)
		case "log":
			return repo.log(ctx, cmdArgs, stdout)
		case "put":
			return repo.putFile(ctx, cmdArgs, stdin, stdout)
		}
	}

	return fmt.Errorf("unknown command %q", cmd)
}

func isNativeRemoteCommand(cmd string) bool {
	switch cmd {
	case "init", "fetch", "pull", "push", "ls-remote", "ls", "list", "cat", "show", "log", "put":
		return true
	default:
		return false
	}
}

func parseGlobalFlags(args []string) (config, []string, error) {
	cfg := config{branch: defaultBranch}
	envAuth := strings.TrimSpace(os.Getenv("BGIT_AUTH"))
	envConfiguration := strings.TrimSpace(os.Getenv("BGIT_GCLOUD_CONFIGURATION"))
	cfg.auth = firstNonEmpty(envAuth, defaultAuthMode)
	cfg.gcloudConfiguration = envConfiguration
	rest, err := extractGlobalFlags(args, &cfg)
	if err != nil {
		return cfg, nil, err
	}
	cfg.authExplicit = envAuth != "" || cfg.authExplicit
	cfg.gcloudConfigurationExplicit = envConfiguration != "" || cfg.gcloudConfigurationExplicit
	cfg.prefix = strings.Trim(cfg.prefix, "/")
	cfg.auth = strings.ToLower(strings.TrimSpace(cfg.auth))
	if cfg.auth == "" {
		cfg.auth = defaultAuthMode
	}
	if cfg.auth != "gcloud" && cfg.auth != "adc" {
		return cfg, nil, fmt.Errorf("unsupported auth mode %q", cfg.auth)
	}
	return cfg, rest, nil
}

func extractGlobalFlags(args []string, cfg *config) ([]string, error) {
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--bucket":
			if !hasValue {
				i++
				if i >= len(args) {
					return nil, errors.New("--bucket requires a value")
				}
				value = args[i]
			}
			applyBucketFlag(cfg, value)
		case "--prefix":
			if !hasValue {
				i++
				if i >= len(args) {
					return nil, errors.New("--prefix requires a value")
				}
				value = args[i]
			}
			cfg.prefix = value
		case "--branch":
			if !hasValue {
				i++
				if i >= len(args) {
					return nil, errors.New("--branch requires a value")
				}
				value = args[i]
			}
			cfg.branch = value
		case "--auth":
			if !hasValue {
				i++
				if i >= len(args) {
					return nil, errors.New("--auth requires a value")
				}
				value = args[i]
			}
			cfg.auth = value
			cfg.authExplicit = true
		case "--configuration", "--profile":
			if !hasValue {
				i++
				if i >= len(args) {
					return nil, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			cfg.gcloudConfiguration = value
			cfg.gcloudConfigurationExplicit = true
		case "--version", "-v":
			cfg.versionRequested = true
		default:
			rest = append(rest, arg)
		}
	}
	return rest, nil
}

func applyBucketFlag(cfg *config, value string) {
	provider, bucket, prefix := normalizeAdminTarget(value)
	if provider != "" {
		cfg.provider = provider
	}
	if bucket != "" {
		cfg.bucket = bucket
	} else {
		cfg.bucket = value
	}
	if prefix != "" && cfg.prefix == "" {
		cfg.prefix = prefix
	}
}

func mergeConfig(primary, fallback config) config {
	if primary.provider == "" {
		primary.provider = fallback.provider
	}
	if primary.bucket == "" {
		primary.bucket = fallback.bucket
	}
	if primary.prefix == "" {
		primary.prefix = fallback.prefix
	}
	if primary.branch == "" || primary.branch == defaultBranch {
		primary.branch = fallback.branch
	}
	if primary.branch == "" {
		primary.branch = defaultBranch
	}
	if !primary.authExplicit && fallback.auth != "" {
		primary.auth = fallback.auth
	}
	if primary.auth == "" {
		primary.auth = defaultAuthMode
	}
	if !primary.gcloudConfigurationExplicit && fallback.gcloudConfiguration != "" {
		primary.gcloudConfiguration = fallback.gcloudConfiguration
	}
	return primary
}

func parseGCSRepoURI(raw string) (config, string, error) {
	return parseRepoURI(raw)
}

func parseRepoURI(raw string) (config, string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return config{}, "", err
	}
	provider := ""
	scheme := parsed.Scheme
	switch scheme {
	case "gs", "gcs":
		provider = "gcs"
		scheme = "gs"
	case "s3":
		provider = "s3"
	default:
		return config{}, "", errors.New("repository URI must start with gs:// or s3://")
	}
	if parsed.Host == "" {
		return config{}, "", errors.New("repository URI must include a bucket name")
	}
	prefix := strings.Trim(parsed.Path, "/")
	if prefix == "" {
		return config{}, "", errors.New("repository URI must include a repository prefix")
	}
	parts := strings.Split(prefix, "/")
	repoName := parts[len(parts)-1]
	origin := fmt.Sprintf("%s://%s/%s", scheme, parsed.Host, prefix)
	return config{provider: provider, bucket: parsed.Host, prefix: prefix, branch: defaultBranch, origin: origin}, repoName, nil
}

func readLocalConfig(dir string) (config, error) {
	branch := defaultBranch
	branchOut, branchErr := runGit(dir, "config", "--get", "bucketgit.branch")
	if branchErr == nil {
		branch = strings.TrimSpace(string(branchOut))
	}
	if branch == "" {
		branch = defaultBranch
	}
	localAuth := defaultBranchAuth(dir)

	originOut, originErr := runGit(dir, "config", "--get", "bucketgit.origin")
	if originErr == nil {
		origin := strings.TrimSpace(string(originOut))
		cfg, _, err := parseRepoURI(origin)
		if err == nil {
			cfg.branch = branch
			cfg.auth = localAuth.auth
			cfg.gcloudConfiguration = localAuth.gcloudConfiguration
			return cfg, nil
		}
	}

	bucketOut, err := runGit(dir, "config", "--get", "bucketgit.bucket")
	if err != nil {
		remoteOut, remoteErr := runGit(dir, "remote", "get-url", "origin")
		if remoteErr == nil {
			origin := strings.TrimSpace(string(remoteOut))
			cfg, _, parseErr := parseRepoURI(origin)
			if parseErr == nil {
				cfg.branch = branch
				cfg.auth = localAuth.auth
				cfg.gcloudConfiguration = localAuth.gcloudConfiguration
				return cfg, nil
			}
		}
		return config{}, err
	}
	prefixOut, err := runGit(dir, "config", "--get", "bucketgit.prefix")
	if err != nil {
		return config{}, err
	}
	bucket := strings.TrimSpace(string(bucketOut))
	prefix := strings.Trim(strings.TrimSpace(string(prefixOut)), "/")
	provider := "gcs"
	if providerOut, err := runGit(dir, "config", "--get", "bucketgit.provider"); err == nil {
		if value := strings.TrimSpace(string(providerOut)); value != "" {
			provider = value
		}
	}
	return config{
		provider:            provider,
		bucket:              bucket,
		prefix:              prefix,
		branch:              branch,
		origin:              originForConfig(config{provider: provider, bucket: bucket, prefix: prefix}),
		auth:                localAuth.auth,
		gcloudConfiguration: localAuth.gcloudConfiguration,
	}, nil
}

func defaultBranchAuth(dir string) config {
	cfg := config{auth: defaultAuthMode}
	if out, err := runGit(dir, "config", "--get", "bucketgit.auth"); err == nil {
		auth := strings.ToLower(strings.TrimSpace(string(out)))
		if auth == "gcloud" || auth == "adc" {
			cfg.auth = auth
		}
	}
	for _, key := range []string{"bucketgit.profile", "bucketgit.configuration", "bucketgit.gcloudConfiguration"} {
		if out, err := runGit(dir, "config", "--get", key); err == nil {
			if value := strings.TrimSpace(string(out)); value != "" {
				cfg.gcloudConfiguration = value
				break
			}
		}
	}
	return cfg
}

func missingOriginError() error {
	return errors.New(`No configured push destination.
Either specify the repository from the command-line:

    bgit --bucket bucket-name --prefix path/to/repo.git push

or configure a bgit origin:

    bgit origin gs://bucket-name/path/to/repo.git
    bgit origin s3://bucket-name/path/to/repo.git

and then push:

    bgit push`)
}

func newStorageClient(ctx context.Context, cfg config) (*storage.Client, error) {
	if cfg.auth == "" {
		cfg.auth = defaultAuthMode
	}
	switch cfg.auth {
	case "adc":
		return storage.NewClient(ctx)
	case "gcloud":
		token, err := gcloudAccessToken(cfg.gcloudConfiguration)
		if err != nil {
			return nil, err
		}
		source := oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: token,
			TokenType:   "Bearer",
		})
		return storage.NewClient(ctx, option.WithTokenSource(source))
	default:
		return nil, fmt.Errorf("unsupported auth mode %q", cfg.auth)
	}
}

func isReadOnlyRemoteCommand(cmd string) bool {
	switch cmd {
	case "clone", "fetch", "pull", "ls-remote", "ls", "list", "cat", "show", "log":
		return true
	default:
		return false
	}
}

func newRemoteStore(ctx context.Context, cfg config, publicFallback bool) (gitRemoteStore, func(), error) {
	provider := cfg.provider
	if provider == "" {
		provider = "gcs"
	}
	if publicFallback {
		publicStore, publicClose, publicErr := newAnonymousRemoteStore(ctx, cfg)
		authStore, authClose, authErr := newRemoteStore(ctx, cfg, false)
		if publicErr == nil && authErr == nil {
			return &fallbackGitRemoteStore{primary: publicStore, fallback: authStore}, func() {
				publicClose()
				authClose()
			}, nil
		}
		if publicErr == nil {
			return publicStore, publicClose, nil
		}
		if authErr == nil {
			return authStore, authClose, nil
		}
		return nil, func() {}, authErr
	}
	switch provider {
	case "gcs":
		client, err := newStorageClient(ctx, cfg)
		if err != nil {
			return nil, func() {}, err
		}
		return &gcsGitStore{
			client: client,
			bucket: cfg.bucket,
			prefix: strings.Trim(cfg.prefix, "/"),
		}, func() { _ = client.Close() }, nil
	case "s3":
		client, err := newS3Client(ctx, cfg, false)
		if err != nil {
			return nil, func() {}, err
		}
		return &s3GitStore{
			client: client,
			bucket: cfg.bucket,
			prefix: strings.Trim(cfg.prefix, "/"),
		}, func() {}, nil
	default:
		return nil, func() {}, fmt.Errorf("unsupported storage provider %q", provider)
	}
}

func newAnonymousRemoteStore(ctx context.Context, cfg config) (gitRemoteStore, func(), error) {
	provider := cfg.provider
	if provider == "" {
		provider = "gcs"
	}
	switch provider {
	case "gcs":
		client, err := storage.NewClient(ctx, option.WithoutAuthentication())
		if err != nil {
			return nil, func() {}, err
		}
		return &gcsGitStore{client: client, bucket: cfg.bucket, prefix: strings.Trim(cfg.prefix, "/")}, func() { _ = client.Close() }, nil
	case "s3":
		client, err := newS3Client(ctx, cfg, true)
		if err != nil {
			return nil, func() {}, err
		}
		return &s3GitStore{client: client, bucket: cfg.bucket, prefix: strings.Trim(cfg.prefix, "/")}, func() {}, nil
	default:
		return nil, func() {}, fmt.Errorf("unsupported storage provider %q", provider)
	}
}

func gcloudAccessToken(configuration string) (string, error) {
	cmd := gcloudCommand(configuration, "auth", "print-access-token")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gcloud auth print-access-token failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("gcloud auth print-access-token returned an empty token")
	}
	return token, nil
}

func gcloudCommand(configuration string, args ...string) *exec.Cmd {
	cmd := exec.Command("gcloud", args...)
	if strings.TrimSpace(configuration) != "" {
		cmd.Env = append(os.Environ(), "CLOUDSDK_ACTIVE_CONFIG_NAME="+strings.TrimSpace(configuration))
	}
	return cmd
}

func usage(w io.Writer) error {
	_, err := fmt.Fprint(w, `usage: bgit <command> [args]

common commands:
  clone gs://bucket/prefix.git [directory]
  clone s3://bucket/prefix.git [directory]
  init [directory]
  origin gs://bucket/prefix.git
  origin s3://bucket/prefix.git
  ssh setup [gs://bucket/prefix.git|s3://bucket/prefix.git]
  web [--addr 127.0.0.1] [--port 8042] [--local]
  admin grant-read|grant-write|grant-admin IDENTITY
  create-gcloud-profile NAME
  fetch | pull | push | ls-remote
  status | add | commit | checkout | branch | merge | tag
  diff | log | show | reset | restore | stash | revert
  grep | blame | cherry-pick | clean | describe
  ls-files | ls-tree | archive | config | rev-parse | rm | mv

direct GCS mode:
  bgit --bucket BUCKET --prefix PREFIX ls [prefix]
  bgit --bucket BUCKET --prefix PREFIX cat [--commit SHA] path
  bgit --bucket BUCKET --prefix PREFIX log [--limit N] [--skip N] [--path PATH]
  put path [--file FILE] -m MSG --author NAME --email EMAIL

global options:
  --profile NAME
  --auth gcloud|adc
  --version
`)
	return err
}

func helpCommand(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usage(stdout)
	}
	if len(args) > 1 {
		return errors.New("help accepts at most one command")
	}
	return commandHelp(args[0], stdout)
}

func commandWantsHelp(args []string) bool {
	return len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help")
}

func commandWantsVersion(args []string) bool {
	return len(args) == 1 && (args[0] == "version" || args[0] == "-v" || args[0] == "--version")
}

func versionCommand(stdout io.Writer) error {
	_, err := fmt.Fprintf(stdout, "bgit %s\n", version)
	return err
}

func commandHelp(cmd string, stdout io.Writer) error {
	if text, ok := helpPages()[cmd]; ok {
		_, err := fmt.Fprint(stdout, text)
		return err
	}
	if isLocalGitCommand(cmd) || isPreferLocalGitCommand(cmd) {
		_, err := fmt.Fprintf(stdout, "usage: bgit %s [args]\n", cmd)
		return err
	}
	if isUnsupportedCommand(cmd) {
		return unsupportedCommand(cmd)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func helpPages() map[string]string {
	return map[string]string{
		"clone": `usage: bgit clone gs://bucket/prefix.git [directory]
       bgit clone s3://bucket/prefix.git [directory]

Clone a bucketgit repository from object storage into a local worktree.
The origin is stored in .git/config so later bgit fetch, pull, and push
commands can infer it.

examples:
  bgit clone gs://my-bucket/repositories/app.git
  bgit clone s3://my-bucket/repositories/app.git --profile aws-profile
  bgit clone gs://my-bucket/repositories/app.git ./app
  bgit --branch develop clone gs://my-bucket/repositories/app.git
`,
		"init": `usage: bgit init [directory]

Create a local Git repository. This does not require an origin. Configure one
later with bgit origin before pushing.

examples:
  bgit init
  bgit init ./app
  bgit origin gs://my-bucket/repositories/app.git
  bgit push
`,
		"origin": `usage:
  bgit origin gs://bucket/prefix.git
  bgit origin s3://bucket/prefix.git

Set the bucketgit origin for the current local Git repository. This also
sets the regular Git remote named origin to the same URL for visibility.

examples:
  bgit origin gs://my-bucket/repositories/app.git
  bgit origin s3://my-bucket/repositories/app.git --profile aws-profile
  git remote -v
`,
		"remote": `usage:
  bgit remote add origin gs://bucket/prefix.git
  bgit remote add origin s3://bucket/prefix.git
  bgit remote set-url origin gs://bucket/prefix.git

Configure the bucketgit origin using Git remote syntax.
`,
		"admin": `usage:
  bgit admin grant-read IDENTITY
  bgit admin grant-write IDENTITY
  bgit admin grant-admin IDENTITY
  bgit admin make-public
  bgit admin make-private
  bgit admin --bucket BUCKET grant-write IDENTITY

Grant bucket access or toggle public read access for GCS or S3 repositories.
Run inside a bgit checkout to infer the bucket and prefix, or pass --bucket
explicitly.

For GCS, IDENTITY may be user@example.com, user:user@example.com,
serviceAccount:name@project.iam.gserviceaccount.com, group:team@example.com,
allUsers, or allAuthenticatedUsers.

For S3, IDENTITY must be an IAM/STS ARN, a 12 digit AWS account ID, or *.

grant-read grants object read access plus bucket/prefix listing.
grant-write grants object read/write/delete access plus bucket/prefix listing.
grant-admin grants storage admin access on the bucket or repository prefix.
make-public grants anonymous read access.
make-private removes bgit-managed anonymous read access.

examples:
  bgit admin grant-read user:dev@example.com
  bgit admin grant-write serviceAccount:ci@project.iam.gserviceaccount.com
  bgit admin --bucket my-bucket grant-admin admin@example.com
  bgit admin make-public
  bgit admin make-private
  bgit admin --bucket s3://my-bucket/repositories/app.git grant-read arn:aws:iam::123456789012:role/Developer
`,
		"ssh": `usage:
  bgit ssh setup [--broker URL] [--region REGION] [--firestore-database NAME] [--firestore-location LOCATION] [--key PATH] [--no-agent] [gs://bucket/prefix.git|s3://bucket/prefix.git]
  bgit ssh scaffold [--broker URL] [gs://bucket/prefix.git|s3://bucket/prefix.git]
  bgit ssh repo add [--broker URL] [--key PATH] [repo]
  bgit ssh keys list|add|remove|suspend [--broker URL] [--key PATH] [repo]

Configure the current repository so normal git fetch/push uses bgit as the SSH
transport command. The setup command also records public keys from ssh-agent or
--key for a future broker-backed authorization flow. When --broker is omitted,
setup looks for an existing bgit-broker endpoint in the selected cloud account
and region. Setup also upserts the repository into the broker with discovered
SSH identities under an admin user.

examples:
  bgit ssh setup gs://my-bucket/repositories/app.git
  bgit ssh setup s3://my-bucket/repositories/app.git --profile aws-profile --key ~/.ssh/id_ed25519.pub
  bgit ssh repo add --key ~/.ssh/id_ed25519.pub
  bgit ssh keys add --user ada --role write --key ~/.ssh/ada.pub
  bgit ssh keys list
  bgit ssh scaffold
`,
		"web": `usage: bgit web [--addr ADDR] [--port PORT] [--local]

Serve a small repository browser for the configured bucketgit repository.
By default this reads the configured remote using the same read-only store path
as bgit fetch and bgit ls-remote: anonymous public read first, then
authenticated GCS/S3 credentials if needed. Use --local to serve the local .git
object store instead.

examples:
  bgit web
  bgit web --port 8042
  bgit web --local
`,
		"create-gcloud-profile": `usage: bgit create-gcloud-profile [--yes] NAME

Create a gcloud configuration, run gcloud auth login for that configuration,
and save it as bucketgit.profile in the current checkout when run inside one.

examples:
  bgit create-gcloud-profile my-profile
  bgit create-gcloud-profile --yes my-profile
`,
		"fetch": `usage: bgit fetch

Download Git objects and refs from the configured remote into refs/remotes/bucketgit/*.
Tags are fetched by default.
`,
		"pull": `usage: bgit pull [branch]

Fetch from the configured remote and fast-forward the current branch when possible.

examples:
  bgit pull
  bgit pull main
`,
		"push": `usage: bgit push [--tags] [--force] [--skip-broker] [--delete ref] [refspec...]

Sync local Git objects and refs to the configured object-storage repository.
When a broker is configured, bgit uses broker compare-and-swap ref updates.
Use --skip-broker as an operator escape hatch for direct bucket ref writes.

examples:
  bgit push
  bgit push --tags
  bgit push HEAD:refs/heads/main
  bgit push --delete feature
`,
		"ls-remote": `usage: bgit ls-remote [--heads] [--tags]

List refs from the configured object-storage repository.
`,
		"ls": `usage: bgit --bucket BUCKET --prefix PREFIX ls [path-prefix]

Direct GCS mode: list files at the configured branch without a checkout.
`,
		"list": `usage: bgit --bucket BUCKET --prefix PREFIX list [path-prefix]

Direct GCS mode: list files at the configured branch without a checkout.
`,
		"cat": `usage: bgit --bucket BUCKET --prefix PREFIX cat [--commit SHA] path

Direct GCS mode: print one file from the configured branch or commit.
`,
		"put": `usage: bgit --bucket BUCKET --prefix PREFIX put path [--file FILE] -m MSG --author NAME --email EMAIL

Direct GCS mode: write one file and commit it to the GCS-backed repository.
Use --file - or omit --file to read content from stdin.
`,
	}
}

func originCommand(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("origin requires exactly one gs:// or s3:// repository URI")
	}
	cfg, _, err := parseRepoURI(args[0])
	if err != nil {
		return err
	}
	worktree, err := requireWorktree(".")
	if err != nil {
		return err
	}
	if err := writeBucketGitConfig(worktree, cfg); err != nil {
		return err
	}
	if err := setGitOrigin(worktree, cfg.origin); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "origin set to %s\n", cfg.origin)
	return nil
}

func remoteCommand(args []string, stdout io.Writer) error {
	if len(args) == 3 && args[0] == "add" && args[1] == "origin" {
		return originCommand([]string{args[2]}, stdout)
	}
	if len(args) == 3 && args[0] == "set-url" && args[1] == "origin" {
		return originCommand([]string{args[2]}, stdout)
	}
	return errors.New("supported remote commands: remote add origin gs://...|s3://... or remote set-url origin gs://...|s3://...")
}

func createGcloudProfileCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	yes := false
	var profile string
	for _, arg := range args {
		switch arg {
		case "-y", "--yes":
			yes = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unsupported create-gcloud-profile option %s", arg)
			}
			if profile != "" {
				return errors.New("create-gcloud-profile accepts exactly one profile name")
			}
			profile = arg
		}
	}
	if profile == "" {
		return errors.New("create-gcloud-profile requires a profile name")
	}
	if !yes {
		fmt.Fprintf(stdout, "Create gcloud configuration %q, run browser login, and save it in this checkout if possible? [y/N] ", profile)
		var answer string
		_, _ = fmt.Fscanln(stdin, &answer)
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			return errors.New("aborted")
		}
	}
	if err := runGcloudProfileCommand(stdout, "config", "configurations", "create", profile); err != nil {
		return err
	}
	if err := runGcloudProfileCommand(stdout, "auth", "login", "--configuration", profile); err != nil {
		return err
	}
	if repo, err := openLocalRepository("."); err == nil {
		if err := repo.config([]string{"bucketgit.profile", profile}, stdout); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "saved bucketgit.profile=%s\n", profile)
	} else {
		fmt.Fprintf(stdout, "created gcloud profile %s\n", profile)
	}
	return nil
}

func runGcloudProfileCommand(stdout io.Writer, args ...string) error {
	cmd := exec.Command("gcloud", args...)
	cmd.Stdin = os.Stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if out.Len() > 0 {
			_, _ = stdout.Write(out.Bytes())
		}
		return fmt.Errorf("gcloud %s: %w", strings.Join(args, " "), err)
	}
	if out.Len() > 0 {
		_, _ = stdout.Write(out.Bytes())
	}
	return nil
}

func isLocalGitCommand(cmd string) bool {
	switch cmd {
	case "status", "add", "rm", "delete", "mv", "move", "commit", "checkout", "switch",
		"branch", "tag", "rev-parse", "diff", "reset", "restore", "stash", "revert",
		"grep", "blame", "cherry-pick", "clean", "describe", "ls-files", "ls-tree",
		"archive", "show", "merge", "config":
		return true
	default:
		return false
	}
}

func isPreferLocalGitCommand(cmd string) bool {
	switch cmd {
	case "log":
		return true
	default:
		return false
	}
}

func isUnsupportedCommand(cmd string) bool {
	switch cmd {
	case "rebase",
		"daemon", "submodule", "lfs", "gc", "fsck", "repack", "prune",
		"worktree", "maintenance", "credential", "credential-cache",
		"credential-store", "filter-repo", "svn", "p4", "send-email",
		"request-pull", "upload-pack", "receive-pack", "upload-archive",
		"http-backend", "backfill":
		return true
	default:
		return false
	}
}

func unsupportedCommand(cmd string) error {
	return fmt.Errorf("Unsupported: '%s' is not supported by bgit", cmd)
}

func statusCommand(args []string, stdout io.Writer) error {
	return nativeLocalCommand("status", args, stdout)
}

func localGitCommand(cmd string, args []string, stdout io.Writer) error {
	return nativeLocalCommand(cmd, args, stdout)
}

func commandCreatesBucket(cmd string) bool {
	switch cmd {
	case "init", "put":
		return true
	default:
		return false
	}
}

func cloneCommand(ctx context.Context, base config, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("clone", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	branch := flags.String("branch", base.branch, "branch to check out")
	if err := flags.Parse(args); err != nil {
		return err
	}
	rest := flags.Args()
	if len(rest) < 1 || len(rest) > 2 {
		return errors.New("clone requires gs://bucket/prefix.git or s3://bucket/prefix.git and optional directory")
	}
	uriCfg, repoName, err := parseRepoURI(rest[0])
	if err != nil {
		return err
	}
	uriCfg.branch = *branch
	if base.branch != defaultBranch && *branch == defaultBranch {
		uriCfg.branch = base.branch
	}
	uriCfg.auth = base.auth
	uriCfg.authExplicit = base.authExplicit
	uriCfg.gcloudConfiguration = base.gcloudConfiguration
	uriCfg.gcloudConfigurationExplicit = base.gcloudConfigurationExplicit
	target := strings.TrimSuffix(repoName, ".git")
	if len(rest) == 2 {
		target = rest[1]
	}
	store, closeStore, err := newRemoteStore(ctx, uriCfg, true)
	if err != nil {
		return fmt.Errorf("create remote store: %w", err)
	}
	defer closeStore()
	repo := openNativeGitRepo(store, uriCfg)
	return repo.initWorktree(ctx, []string{target}, stdout)
}

func initEmptyWorktree(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	branch := flags.String("branch", defaultBranch, "initial branch")
	if err := flags.Parse(args); err != nil {
		return err
	}
	rest := flags.Args()
	if len(rest) > 1 {
		return errors.New("init accepts at most one directory")
	}
	target := "."
	if len(rest) == 1 {
		target = rest[0]
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absTarget, 0o755); err != nil {
		return err
	}
	if _, err := runGit(absTarget, "init", "--initial-branch", shortBranchName(*branch)); err != nil {
		if _, fallbackErr := runGit(absTarget, "init"); fallbackErr != nil {
			return err
		}
		if _, checkoutErr := runGit(absTarget, "checkout", "--quiet", "-B", shortBranchName(*branch)); checkoutErr != nil {
			return checkoutErr
		}
	}
	if _, err := runGit(absTarget, "config", "--local", "bucketgit.branch", *branch); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Initialized empty Git repository in %s/\n", filepath.Join(absTarget, ".git"))
	return nil
}

func writeBucketGitConfig(worktree string, cfg config) error {
	if cfg.origin == "" {
		cfg.origin = originForConfig(cfg)
	}
	pairs := [][]string{
		{"bucketgit.origin", cfg.origin},
		{"bucketgit.provider", firstNonEmpty(cfg.provider, "gcs")},
		{"bucketgit.bucket", cfg.bucket},
		{"bucketgit.prefix", cfg.prefix},
		{"bucketgit.branch", cfg.branch},
	}
	for _, pair := range pairs {
		if _, err := runGit(worktree, "config", "--local", pair[0], pair[1]); err != nil {
			return err
		}
	}
	return nil
}

func setGitOrigin(worktree string, origin string) error {
	if strings.TrimSpace(origin) == "" {
		return nil
	}
	if _, err := runGit(worktree, "remote", "get-url", "origin"); err == nil {
		_, err = runGit(worktree, "remote", "set-url", "origin", origin)
		return err
	}
	_, err := runGit(worktree, "remote", "add", "origin", origin)
	return err
}

func originForConfig(cfg config) string {
	if cfg.origin != "" {
		return cfg.origin
	}
	if cfg.bucket == "" || cfg.prefix == "" {
		return ""
	}
	scheme := "gs"
	if cfg.provider == "s3" {
		scheme = "s3"
	}
	return fmt.Sprintf("%s://%s/%s", scheme, cfg.bucket, strings.Trim(cfg.prefix, "/"))
}

func ensureBucket(ctx context.Context, cfg config) error {
	if cfg.provider == "s3" {
		return ensureS3Bucket(ctx, cfg)
	}
	client, err := newStorageClient(ctx, cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	bucketName := cfg.bucket
	bucket := client.Bucket(bucketName)
	if _, err := bucket.Attrs(ctx); err == nil {
		return nil
	} else if !errors.Is(err, storage.ErrBucketNotExist) {
		return fmt.Errorf("check bucket gs://%s: %w", bucketName, err)
	}
	projectID, err := defaultProjectID(cfg)
	if err != nil {
		return fmt.Errorf("bucket gs://%s does not exist and active gcloud project could not be detected: %w", bucketName, err)
	}
	if err := bucket.Create(ctx, projectID, nil); err != nil {
		return bucketCreateError(bucketName, projectID, err)
	}
	return nil
}

func bucketCreateError(bucket, projectID string, err error) error {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == 403 {
		return fmt.Errorf(`create bucket gs://%s in project %s: %w

The Google credentials used by bgit do not have storage.buckets.create on this project.
This is often because the selected gcloud configuration uses a different account or project than expected.

Fix credentials:
  gcloud auth print-access-token
  gcloud config set project %s

Or create the bucket manually with an account that has permission:
  gcloud storage buckets create gs://%s --project %s`, bucket, projectID, err, projectID, bucket, projectID)
	}
	return fmt.Errorf("create bucket gs://%s in project %s: %w", bucket, projectID, err)
}

func defaultProjectID(cfg config) (string, error) {
	for _, key := range []string{"GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT", "GCP_PROJECT"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value, nil
		}
	}
	if strings.TrimSpace(cfg.gcloudConfiguration) != "" {
		return gcloudProjectID(cfg.gcloudConfiguration)
	}
	return gcloudProjectID("")
}

func gcloudProjectID(configuration string) (string, error) {
	out, err := gcloudCommand(configuration, "config", "get-value", "project", "--quiet").Output()
	if err != nil {
		return "", err
	}
	projectID := strings.TrimSpace(string(out))
	if projectID == "" || projectID == "(unset)" {
		return "", errors.New("gcloud project is unset")
	}
	return projectID, nil
}

type pushOptions struct {
	tags       bool
	force      bool
	delete     bool
	skipBroker bool
	refs       []string
}

func parsePushArgs(args []string) (pushOptions, error) {
	var opts pushOptions
	for _, arg := range args {
		switch arg {
		case "--tags":
			opts.tags = true
		case "--force", "-f":
			opts.force = true
		case "--delete", "-d":
			opts.delete = true
		case "--skip-broker":
			opts.skipBroker = true
		default:
			if strings.HasPrefix(arg, "--force-with-lease") {
				opts.force = true
			} else if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unsupported push option %s", arg)
			} else {
				opts.refs = append(opts.refs, arg)
			}
		}
	}
	if opts.delete && len(opts.refs) == 0 {
		return opts, errors.New("push --delete requires at least one branch or ref")
	}
	return opts, nil
}

func normalizeDeleteRef(ref string) string {
	if strings.HasPrefix(ref, "refs/") {
		return ref
	}
	if strings.HasPrefix(ref, "tags/") {
		return "refs/" + ref
	}
	return branchRef(ref)
}

func runGit(dir string, args ...string) ([]byte, error) {
	return runGitEnv(dir, os.Environ(), args...)
}

func runGitEnv(dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = env
	if dir != "" {
		cmd.Dir = dir
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

func requireWorktree(dir string) (string, error) {
	worktree, _, err := findLocalRepository(dir)
	if err != nil {
		return "", errors.New("this command must be run inside a git worktree created by bgit clone/init")
	}
	return worktree, nil
}

func objectPrefix(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

func joinObjectName(prefix, rel string) string {
	prefix = strings.Trim(prefix, "/")
	rel = strings.TrimPrefix(rel, "/")
	if prefix == "" {
		return rel
	}
	return prefix + "/" + rel
}

func branchRef(branch string) string {
	if strings.HasPrefix(branch, "refs/") {
		return branch
	}
	return "refs/heads/" + branch
}

func shortBranchName(branch string) string {
	return strings.TrimPrefix(branch, "refs/heads/")
}

func shortRefName(ref string) string {
	for _, prefix := range []string{"refs/heads/", "refs/tags/", "refs/remotes/"} {
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix)
		}
	}
	return ref
}

func isMissingRef(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "Not a valid object name") ||
		strings.Contains(msg, "unknown revision") ||
		strings.Contains(msg, "Needed a single revision") ||
		strings.Contains(msg, "bad revision")
}

func isNoRefs(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "show-ref") && strings.Contains(msg, "exit status 1")
}
