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
const brokerVersion = "1.1.0"

var version = "dev"

type config struct {
	provider                    string
	bucket                      string
	prefix                      string
	branch                      string
	origin                      string
	brokerURL                   string
	logicalRepo                 string
	teamID                      string
	region                      string
	auth                        string
	gcloudConfiguration         string
	identity                    string
	direct                      bool
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
	setBrokerIdentityPreference(cfg.identity)
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
	if cmd == "origin" || cmd == "remote" {
		return fmt.Errorf("bgit %s is direct bucket configuration; use bgit direct %s", cmd, strings.Join(append([]string{cmd}, cmdArgs...), " "))
	}
	if cmd == "direct" {
		cfg.direct = true
		return directCommand(context.Background(), cfg, cmdArgs, stdin, stdout)
	}
	if cmd == "admin" {
		if localCfg, err := readLocalConfig("."); err == nil {
			cfg = mergeConfig(cfg, localCfg)
			setBrokerIdentityPreference(cfg.identity)
		}
		if cfg.gcloudConfigurationExplicit {
			if err := applyExplicitBrokerProfileSelection(&cfg, cmd); err != nil {
				return err
			}
		}
		return brokerAdminCommandWithInput(cfg, cmdArgs, stdin, stdout)
	}
	if cmd == "janitor" {
		if localCfg, err := readLocalConfig("."); err == nil {
			cfg = mergeConfig(cfg, localCfg)
			setBrokerIdentityPreference(cfg.identity)
		}
		if cfg.gcloudConfigurationExplicit {
			if err := applyExplicitBrokerProfileSelection(&cfg, cmd); err != nil {
				return err
			}
		}
		return janitorCommand(cfg, cmdArgs, stdout)
	}
	if cmd == "ssh" {
		return sshCommand(cfg, cmdArgs, stdout, stderr)
	}
	if cmd == "import-gh-user" || cmd == "create-gcloud-profile" || cmd == "create-aws-profile" {
		return fmt.Errorf("unknown command %q", cmd)
	}
	if cmd == "setup" {
		return setupCommand(context.Background(), cfg, cmdArgs, stdin, stdout)
	}
	if cmd == "broker" {
		return brokerCommand(context.Background(), cfg, cmdArgs, stdin, stdout)
	}
	if cmd == "repos" {
		if localCfg, err := readLocalConfig("."); err == nil {
			cfg = mergeConfig(cfg, localCfg)
			setBrokerIdentityPreference(cfg.identity)
		}
		if cfg.gcloudConfigurationExplicit {
			if err := applyExplicitBrokerProfileSelection(&cfg, cmd); err != nil {
				return err
			}
		}
		return reposCommand(context.Background(), cfg, cmdArgs, stdout)
	}
	if cmd == "pr" {
		if localCfg, err := readLocalConfig("."); err == nil {
			cfg = mergeConfig(cfg, localCfg)
			setBrokerIdentityPreference(cfg.identity)
		}
		return prCommand(cmdArgs, stdin, stdout)
	}
	if cmd == "ci" {
		if localCfg, err := readLocalConfig("."); err == nil {
			cfg = mergeConfig(cfg, localCfg)
			setBrokerIdentityPreference(cfg.identity)
		}
		return ciCommand(cmdArgs, stdin, stdout)
	}
	if cmd == "board" || cmd == "kanban" {
		if localCfg, err := readLocalConfig("."); err == nil {
			cfg = mergeConfig(cfg, localCfg)
			setBrokerIdentityPreference(cfg.identity)
		}
		return boardCommand(cmdArgs, stdin, stdout)
	}
	if cmd == "issue" || cmd == "issues" {
		if localCfg, err := readLocalConfig("."); err == nil {
			cfg = mergeConfig(cfg, localCfg)
			setBrokerIdentityPreference(cfg.identity)
		}
		return issueCommand(cmdArgs, stdin, stdout)
	}
	if cmd == "web" {
		return webCommand(context.Background(), cfg, cmdArgs, stdout)
	}
	if cmd == "config" && configArgsAreGlobal(cmdArgs) {
		return globalConfigCommand(cmdArgs, stdout)
	}
	if isLocalGitCommand(cmd) || (!explicitBucket && isPreferLocalGitCommand(cmd)) {
		return nativeLocalCommand(cmd, cmdArgs, stdout)
	}
	if cfg.authExplicit {
		return errors.New("--auth is only supported with bgit direct")
	}
	if explicitBucket {
		return errors.New("direct bucket operations require bgit direct; run bgit direct help")
	}

	if cmd == "clone" {
		cmdArgs = mergeBrokerSelectionArgs(cmdArgs, cfg)
		return brokerCloneCommand(cmdArgs, stdin, stdout)
	}
	if cmd == "init" {
		cmdArgs = mergeBrokerSelectionArgs(cmdArgs, cfg)
		return brokerInitCommand(cmdArgs, stdin, stdout)
	}
	if cfg.bucket == "" {
		localCfg, err := readLocalConfig(".")
		if err == nil {
			cfg = mergeConfig(cfg, localCfg)
			setBrokerIdentityPreference(cfg.identity)
		}
	}
	if cmd == "whoami" {
		if cfg.gcloudConfigurationExplicit {
			if err := applyExplicitBrokerProfileSelection(&cfg, cmd); err != nil {
				return err
			}
		}
		return whoamiCommand(context.Background(), cfg, cmdArgs, stdout)
	}
	if !cfg.direct && cfg.gcloudConfigurationExplicit && isNativeRemoteCommand(cmd) {
		if err := applyExplicitBrokerProfileSelection(&cfg, cmd); err != nil {
			return err
		}
	}
	if cfg.bucket == "" && cfg.brokerURL == "" {
		if cmd == "push" {
			return missingOriginError()
		}
		return errors.New("--bucket is required outside a bucketgit checkout")
	}

	ctx := context.Background()
	store, closeStore, err := newRemoteStore(ctx, cfg, isReadOnlyRemoteCommand(cmd))
	if err != nil {
		return fmt.Errorf("create remote store: %w", err)
	}
	defer closeStore()

	if isNativeRemoteCommand(cmd) {
		if cfg.brokerURL == "" && (commandCreatesBucket(cmd) || cmd == "push") {
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
			if err := maybeConfigureIdentityBeforePush(stdin, stdout); err != nil {
				return err
			}
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

func applyExplicitBrokerProfileSelection(cfg *config, cmd string) error {
	path, err := defaultGlobalConfigPath()
	if err != nil {
		return err
	}
	global, err := readGlobalConfig(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	profiles := brokerProfilesFromGlobalConfig(global)
	if len(profiles) == 0 {
		return nil
	}
	profile, err := selectBrokerProfileForCommand(profiles, cfg.gcloudConfiguration, cfg.region, "bgit "+cmd)
	if err != nil {
		return err
	}
	cfg.provider = profile.Provider
	cfg.brokerURL = profile.BrokerURL
	cfg.region = profile.Region
	cfg.gcloudConfiguration = profile.QualifiedName
	if cfg.logicalRepo == "" && cfg.prefix != "" {
		cfg.logicalRepo = strings.Trim(cfg.prefix, "/")
	}
	return nil
}

func mergeBrokerProfileArg(args []string, cfg config) []string {
	return mergeBrokerSelectionArgs(args, cfg)
}

func mergeBrokerSelectionArgs(args []string, cfg config) []string {
	merged := append([]string{}, args...)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--profile":
			cfg.gcloudConfigurationExplicit = false
		case "--region":
			cfg.region = ""
		}
	}
	if cfg.gcloudConfigurationExplicit && strings.TrimSpace(cfg.gcloudConfiguration) != "" {
		merged = append(merged, "--profile", cfg.gcloudConfiguration)
	}
	if strings.TrimSpace(cfg.region) != "" {
		merged = append(merged, "--region", cfg.region)
	}
	return merged
}

func directCommand(ctx context.Context, cfg config, args []string, stdin io.Reader, stdout io.Writer) error {
	cfg.direct = true
	if len(args) == 0 {
		return errors.New("usage: bgit direct clone|init|fetch|pull|push|ls-remote|ls|cat|show|log|put|admin [args]")
	}
	cmd := args[0]
	cmdArgs := args[1:]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		return commandHelp("direct", stdout)
	}
	if cmd == "admin" {
		return adminCommand(cfg, cmdArgs, stdout)
	}
	if cmd == "origin" {
		return originCommand(cmdArgs, stdout)
	}
	if cmd == "remote" {
		return remoteCommand(cmdArgs, stdout)
	}
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
	if !isNativeRemoteCommand(cmd) {
		return fmt.Errorf("unknown direct command %q", cmd)
	}
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
	default:
		return fmt.Errorf("unknown direct command %q", cmd)
	}
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
		case "--region":
			if !hasValue {
				i++
				if i >= len(args) {
					return nil, errors.New("--region requires a value")
				}
				value = args[i]
			}
			cfg.region = value
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
		case "--identity":
			if !hasValue {
				i++
				if i >= len(args) {
					return nil, errors.New("--identity requires a value")
				}
				value = args[i]
			}
			cfg.identity = value
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
	if primary.brokerURL == "" {
		primary.brokerURL = fallback.brokerURL
	}
	if primary.logicalRepo == "" {
		primary.logicalRepo = fallback.logicalRepo
	}
	if primary.teamID == "" {
		primary.teamID = fallback.teamID
	}
	if primary.region == "" {
		primary.region = fallback.region
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
	if primary.identity == "" {
		primary.identity = fallback.identity
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
	brokerURL := ""
	if brokerOut, brokerErr := runGit(dir, "config", "--get", "bucketgit.broker"); brokerErr == nil {
		brokerURL = strings.TrimSpace(string(brokerOut))
	}
	logicalRepo := ""
	if logicalOut, logicalErr := runGit(dir, "config", "--get", "bucketgit.logicalRepo"); logicalErr == nil {
		var err error
		logicalRepo, err = normalizeLogicalRepoName(string(logicalOut))
		if err != nil {
			return config{}, err
		}
	}
	teamID := ""
	if teamOut, teamErr := runGit(dir, "config", "--get", "bucketgit.team"); teamErr == nil {
		teamID = strings.TrimSpace(string(teamOut))
	}
	localRegion := ""
	if regionOut, regionErr := runGit(dir, "config", "--get", "bucketgit.region"); regionErr == nil {
		localRegion = strings.TrimSpace(string(regionOut))
	}
	localProvider := ""
	if providerOut, providerErr := runGit(dir, "config", "--get", "bucketgit.provider"); providerErr == nil {
		localProvider = strings.TrimSpace(string(providerOut))
	}
	if brokerURL != "" && logicalRepo != "" {
		identity := localIdentityPreference(dir)
		provider := firstNonEmpty(localProvider, "gcs")
		return config{
			provider:            provider,
			prefix:              logicalRepo,
			branch:              branch,
			origin:              fmt.Sprintf("git@%s:%s", defaultSSHHost, logicalRepo),
			brokerURL:           brokerURL,
			logicalRepo:         logicalRepo,
			teamID:              teamID,
			region:              localRegion,
			identity:            identity,
			auth:                localAuth.auth,
			gcloudConfiguration: localAuth.gcloudConfiguration,
		}, nil
	}

	originOut, originErr := runGit(dir, "config", "--get", "bucketgit.origin")
	if originErr == nil {
		origin := strings.TrimSpace(string(originOut))
		cfg, _, err := parseRepoURI(origin)
		if err == nil {
			cfg.branch = branch
			cfg.region = localRegion
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
				cfg.region = localRegion
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
	provider := firstNonEmpty(localProvider, "gcs")
	return config{
		provider:            provider,
		bucket:              bucket,
		prefix:              prefix,
		branch:              branch,
		origin:              originForConfig(config{provider: provider, bucket: bucket, prefix: prefix}),
		brokerURL:           brokerURL,
		logicalRepo:         logicalRepo,
		region:              localRegion,
		auth:                localAuth.auth,
		gcloudConfiguration: localAuth.gcloudConfiguration,
	}, nil
}

func localIdentityPreference(dir string) string {
	for _, key := range []string{"bucketgit.sshKeyFingerprint", "bucketgit.sshKey", "bucketgit.identity"} {
		out, err := runGit(dir, "config", "--get", key)
		if err == nil {
			if value := strings.TrimSpace(string(out)); value != "" {
				return value
			}
		}
	}
	return ""
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

    bgit --bucket bucket-name --prefix path/to/repo.git direct push

or configure a direct bgit origin:

    bgit direct origin gs://bucket-name/path/to/repo.git
    bgit direct origin s3://bucket-name/path/to/repo.git

and then push:

    bgit direct push`)
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
	if !cfg.direct {
		if cfg.brokerURL == "" {
			if out, err := runGit(".", "config", "--get", "bucketgit.broker"); err == nil {
				cfg.brokerURL = strings.TrimSpace(string(out))
			}
		}
		if cfg.logicalRepo == "" {
			if out, err := runGit(".", "config", "--get", "bucketgit.logicalRepo"); err == nil {
				logical, normalizeErr := normalizeLogicalRepoName(string(out))
				if normalizeErr != nil {
					return nil, nil, normalizeErr
				}
				cfg.logicalRepo = logical
			}
		}
		if cfg.brokerURL != "" {
			return &brokerGitStore{brokerURL: cfg.brokerURL, cfg: cfg}, func() {}, nil
		}
	}
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

These are common BucketGit commands:

start a repository
   setup      Connect a cloud account and deploy or update BucketGit
   init       Create a local Git repository backed by BucketGit
   clone      Clone a BucketGit repository into a new directory

work on the current change
   add        Add file contents to the index
   mv         Move or rename a file, directory, or symlink
   restore    Restore working tree files
   rm         Remove files from the working tree and index

examine history and state
   diff       Show changes between commits, commit and working tree, etc
   grep       Print lines matching a pattern
   log        Show commit logs
   show       Show objects
   status     Show the working tree status

grow, mark, and tweak history
   branch     List, create, or delete branches
   checkout   Switch branches or restore paths
   commit     Record changes to the repository
   merge      Join development histories together
   reset      Reset HEAD, index, or working tree state
   tag        Create, list, delete, or verify tags

collaborate
   fetch      Download objects and refs from BucketGit
   pull       Fetch and integrate with the current branch
   push       Update remote refs and upload objects
   ls-remote  List remote refs
   pr         Create, review, merge, and close pull requests
   ci         Run and inspect broker CI builds
   board      Manage the repository task board
   issue      Create, comment on, close, and reopen issues

administer
   whoami     Show broker identity, role, and capabilities for this repo
   repos      List repositories visible to local SSH keys
   admin      Manage broker-backed users, keys, owners, and protection
   janitor    Run broker maintenance and repair tasks
   broker     Delete or decommission deployed broker infrastructure
   web        Browse a repository locally

global options:
  --profile NAME
  --identity KEY_OR_FINGERPRINT
  --version

Legacy direct bucket operations are under "bgit direct".
Run "bgit help <command>" or "bgit direct help" for details.
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
		"clone": `usage:
  bgit clone <broker-repo> [directory]
  bgit clone https://broker.example.com/app.git [directory]
  bgit clone https://broker.example.com/team/app.git [directory]
  bgit clone https://broker.example.com/team/app/app.git [directory]
  bgit clone --broker https://broker.example.com app.git [directory]

Clone a BucketGit repository by logical repo name. Flat broker URLs use the
default core team. Team URLs can use /team/repo.git or /team/repo/repo.git.
Passing a broker URL makes the checkout self-contained and does not require a
local profile. Direct object-storage clone moved to bgit direct clone.

examples:
  bgit clone app.git
  bgit clone https://bgit-broker.example.com/app.git
  bgit clone https://git.example.com/platform/app.git
  bgit clone https://git.example.com/platform/app/app.git
  bgit direct clone gs://my-bucket/repositories/app.git
  bgit direct clone s3://my-bucket/repositories/app.git --profile aws-profile
`,
		"init": `usage:
  bgit init
  bgit init --noninteractive --repo NAME --profile PROFILE[.REGION] --team TEAM [--region REGION] [directory]

Create a local Git repository and attach it to an existing BucketGit repository from
~/.bgit/config.yaml. Without --noninteractive, init prompts for missing repo,
profile, region, and team choices.

examples:
  bgit init
  bgit init --noninteractive --repo app --profile gcp:work.europe-west1 --team core
  bgit init --noninteractive --repo app --profile work --region europe-west1 --team core
`,
		"setup": `usage:
  bgit setup
  bgit setup --yes [--provider gcp|aws] [--profile NAME] [--key PATH] [--region REGION]
  bgit setup profile create --provider gcp|aws NAME

Discover cloud profiles, deploy or update a bgit broker, import owner SSH keys,
and write the global BucketGit config at ~/.bgit/config.yaml.

GCP profiles are discovered from gcloud configurations. AWS profiles are
discovered from AWS config/credentials files and aws configure list-profiles
when the AWS CLI is available.

examples:
  bgit setup
  bgit setup profile create --provider gcp work
  bgit setup --yes --provider gcp --profile work --key ~/.ssh/id_ed25519.pub
  bgit setup --yes --provider aws --profile production --region us-east-1
`,
		"broker": `usage:
  bgit broker delete --provider gcp --profile NAME [--region REGION] [--data] --yes
  bgit broker delete --provider aws --profile NAME [--region REGION] --yes

Delete deployed bgit broker infrastructure for a selected cloud profile.
AWS deletes the CloudFormation stack and waits for deletion. GCP deletes the
Gen2 function/Cloud Run service; pass --data to also delete the Firestore
broker database.
`,
		"origin": `usage:
  bgit direct origin gs://bucket/prefix.git
  bgit direct origin s3://bucket/prefix.git

Set a direct bucketgit origin for the current local Git repository. This also
sets the regular Git remote named origin to the same URL for visibility.

examples:
  bgit direct origin gs://my-bucket/repositories/app.git
  bgit direct origin s3://my-bucket/repositories/app.git --profile aws-profile
  git remote -v
`,
		"remote": `usage:
  bgit direct remote add origin gs://bucket/prefix.git
  bgit direct remote add origin s3://bucket/prefix.git
  bgit direct remote set-url origin gs://bucket/prefix.git

Configure a direct bucketgit origin using Git remote syntax.
`,
		"admin": `usage:
  bgit admin keys list|add|remove|suspend|import-github [args]
  bgit admin broker upgrade
  bgit admin broker owner-bootstrap reset
  bgit admin broker-users list|upsert USER [--role admin|user] [--key PATH_OR_PUBLIC_KEY]|delete USER
  bgit admin teams list|create NAME|delete TEAM|member add TEAM USER [--role ROLE]|member remove TEAM USER
  bgit admin teams repo list|repo add TEAM ROLE|repo remove TEAM
  bgit admin repo list
  bgit admin repo info
  bgit admin repo create --team TEAM [--role ROLE] REPO
  bgit admin invite-user --broker URL [--team TEAM] --user USER [--role ROLE] REPO
  bgit admin accept-invite CODE
  bgit admin cancel-invite --broker URL [--team TEAM] --user USER REPO
  bgit admin confirm-ownership-transfer --broker URL REPO
  bgit admin accept-ownership-transfer CODE
  bgit admin cancel-ownership-transfer [--broker URL REPO]
  bgit admin protect add|list|remove [ref]
  bgit admin repo visibility public|private
  bgit admin repo readonly on|off
  bgit admin repo issues on|off
  bgit admin repo rename NEW_LOGICAL_NAME
  bgit admin repo delete --yes

Broker-backed repository administration. Cloud IAM and bucket-policy
administration moved to bgit direct admin.

examples:
  bgit admin broker upgrade
  bgit admin broker owner-bootstrap reset
  bgit admin keys list
  bgit admin keys add --user ada --role developer --key ~/.ssh/ada.pub
  bgit admin keys import-github octocat --role read
  bgit admin broker-users upsert ada --role user --key ~/.ssh/ada.pub
  bgit admin teams create platform
  bgit admin teams delete TEAM_ID
  bgit admin teams member add TEAM_ID ada --role developer
  bgit admin teams repo list
  bgit admin teams repo add TEAM_ID developer
  bgit admin repo list
  bgit admin repo info
  bgit admin repo create --team platform app
  bgit admin invite-user --broker https://broker.example.com --user ada --role developer app
  bgit admin protect add main
  bgit admin ci rotate-secret
  bgit admin repo visibility public
  bgit direct admin grant-read user:dev@example.com
`,
		"issue": `usage:
  bgit issue list
  bgit issue create TITLE [--body BODY]
  bgit issue view ID
  bgit issue comment ID COMMENT
  bgit issue close ID
  bgit issue reopen ID

Broker-backed repository issues. Public repositories allow anonymous issue
creation; private repositories require membership.
`,
		"board": `usage:
  bgit board list
  bgit board create STORY
  bgit board move STORY_ID backlog|ready|doing|review|done
  bgit board take STORY_ID
  bgit board comment STORY_ID COMMENT

Broker-backed repository task board. The board is available immediately for
broker-backed repositories and stores stories in repository metadata. Viewers
can read the board; developers and higher can create, take, move, and comment.
`,
		"ci": `usage:
  bgit ci list
  bgit ci run [--ref REF] [--config FILE] [--provider gcp|aws]
  bgit ci view ID

Broker-backed CI records and provider build handoff. CI runs are requested for
a broker ref and commit; the broker verifies repository state before queuing the
trusted provider/materializer path.
`,
		"pr": `usage:
  bgit pr create [--title TITLE] [--body BODY] [--source BRANCH] [--target BRANCH]
  bgit pr list
  bgit pr view ID
  bgit pr checkout ID
  bgit pr diff ID
  bgit pr merge ID [--delete-branch]
  bgit pr close ID
  bgit pr comment ID COMMENT
  bgit pr approve ID [COMMENT]
  bgit pr reject ID [COMMENT]

Broker-backed pull request metadata and merge/ref protection workflow.
Pull requests are stored in the broker control plane, not in Git itself.
`,
		"whoami": `usage: bgit whoami [--json] [--refresh] [--all]

Show the SSH identity, repo role, and broker capabilities for the current
broker-backed repository. Results are cached under ~/.bgit/cache/<broker>/.
Use --all to list repositories visible to the SSH keys currently loaded in
ssh-agent.
`,
		"repos": `usage: bgit repos mine [--json]

List repositories visible to the SSH keys currently loaded in ssh-agent using
the broker membership index.
`,
		"janitor": `usage: bgit janitor members reindex

Broker maintenance and repair commands. These commands rebuild derived broker
metadata from authoritative repo state and are not needed for normal use.
`,
		"direct": `usage:
  bgit direct help
  bgit direct clone gs://bucket/prefix.git [directory]
  bgit direct clone s3://bucket/prefix.git [directory]
  bgit direct origin gs://bucket/prefix.git
  bgit direct remote add origin s3://bucket/prefix.git
  bgit direct fetch|pull|push|ls-remote
  bgit direct ls|cat|show|log|put [args]
  bgit direct admin grant-read|grant-write|grant-admin IDENTITY

Low-level object-storage and cloud IAM escape hatch for legacy direct bucket
operations, recovery, migration, and debugging. Normal BucketGit workflows
should use setup, init, git transport, and admin commands.

Direct mode also owns --bucket/--prefix and --auth gcloud|adc.
`,
		"ssh": `usage:
  bgit ssh git-upload-pack <repo>
  bgit ssh git-receive-pack <repo>

Internal SSH transport used by Git for BucketGit remotes. Most users should not
run this command directly; bgit init writes the required core.sshCommand config.

examples:
  git fetch origin
  git push origin main
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
		"ls": `usage: bgit --bucket BUCKET --prefix PREFIX direct ls [path-prefix]

Direct bucket mode: list files at the configured branch without a checkout.
`,
		"list": `usage: bgit --bucket BUCKET --prefix PREFIX direct list [path-prefix]

Direct bucket mode: list files at the configured branch without a checkout.
`,
		"cat": `usage: bgit --bucket BUCKET --prefix PREFIX direct cat [--commit SHA] path

Direct bucket mode: print one file from the configured branch or commit.
`,
		"put": `usage: bgit --bucket BUCKET --prefix PREFIX direct put path [--file FILE] -m MSG --author NAME --email EMAIL

Direct bucket mode: write one file and commit it to the bucket-backed repository.
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
				return fmt.Errorf("unsupported setup profile create option %s", arg)
			}
			if profile != "" {
				return errors.New("setup profile create accepts exactly one profile name")
			}
			profile = arg
		}
	}
	if profile == "" {
		return errors.New("setup profile create requires a profile name")
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

func createAWSProfileCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	yes := false
	var profile string
	for _, arg := range args {
		switch arg {
		case "-y", "--yes":
			yes = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unsupported setup profile create option %s", arg)
			}
			if profile != "" {
				return errors.New("setup profile create accepts exactly one profile name")
			}
			profile = arg
		}
	}
	if profile == "" {
		return errors.New("setup profile create requires a profile name")
	}
	if !yes {
		fmt.Fprintf(stdout, "Create or update AWS profile %q with aws configure? [y/N] ", profile)
		var answer string
		_, _ = fmt.Fscanln(stdin, &answer)
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			return errors.New("aborted")
		}
	}
	if err := runAWSProfileCommand(stdout, "configure", "--profile", profile); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "created AWS profile %s\n", profile)
	return nil
}

func createAWSProfileConfigured(profile, accessKey, secretKey, region string, stdout io.Writer) error {
	profile = strings.TrimSpace(profile)
	accessKey = strings.TrimSpace(accessKey)
	secretKey = strings.TrimSpace(secretKey)
	region = strings.TrimSpace(region)
	if profile == "" {
		return errors.New("setup profile create requires a profile name")
	}
	if accessKey == "" {
		return errors.New("AWS access key ID is required")
	}
	if secretKey == "" {
		return errors.New("AWS secret access key is required")
	}
	fmt.Fprintf(stdout, "configuring AWS profile %s\n", profile)
	if err := runAWSProfileCommand(stdout, "configure", "set", "aws_access_key_id", accessKey, "--profile", profile); err != nil {
		return err
	}
	if err := runAWSProfileCommand(stdout, "configure", "set", "aws_secret_access_key", secretKey, "--profile", profile); err != nil {
		return err
	}
	if region != "" {
		if err := runAWSProfileCommand(stdout, "configure", "set", "region", region, "--profile", profile); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "created AWS profile %s\n", profile)
	return nil
}

func runGcloudProfileCommand(stdout io.Writer, args ...string) error {
	cmd := exec.Command("gcloud", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gcloud %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func runAWSProfileCommand(stdout io.Writer, args ...string) error {
	cmd := exec.Command("aws", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("aws %s failed: %w", strings.Join(args, " "), err)
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
	if err := configureBucketGitLineEndings(absTarget); err != nil {
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
	if strings.TrimSpace(cfg.auth) != "" && cfg.auth != defaultAuthMode {
		pairs = append(pairs, []string{"bucketgit.auth", cfg.auth})
	}
	if strings.TrimSpace(cfg.gcloudConfiguration) != "" {
		pairs = append(pairs, []string{"bucketgit.profile", cfg.gcloudConfiguration})
	}
	for _, pair := range pairs {
		if _, err := runGit(worktree, "config", "--local", pair[0], pair[1]); err != nil {
			return err
		}
	}
	if err := configureBucketGitLineEndings(worktree); err != nil {
		return err
	}
	return nil
}

func configureBucketGitLineEndings(worktree string) error {
	pairs := [][]string{
		{"core.autocrlf", "false"},
		{"core.eol", "lf"},
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

func setGitBranchTracking(worktree, branch, remote string) error {
	branch = shortBranchName(firstNonEmpty(branch, defaultBranch))
	remote = firstNonEmpty(strings.TrimSpace(remote), "origin")
	pairs := [][]string{
		{"branch." + branch + ".remote", remote},
		{"branch." + branch + ".merge", branchRef(branch)},
	}
	for _, pair := range pairs {
		if _, err := runGit(worktree, "config", "--local", pair[0], pair[1]); err != nil {
			return err
		}
	}
	return nil
}

func setGitBranchTrackingIfOrigin(worktree, branch string) error {
	if _, err := runGit(worktree, "remote", "get-url", "origin"); err != nil {
		return nil
	}
	return setGitBranchTracking(worktree, branch, "origin")
}

func unsetGitBranchTracking(worktree, branch string) error {
	branch = shortBranchName(strings.TrimSpace(branch))
	if branch == "" {
		return nil
	}
	for _, key := range []string{"branch." + branch + ".remote", "branch." + branch + ".merge"} {
		if _, err := runGit(worktree, "config", "--local", "--unset-all", key); err != nil {
			continue
		}
	}
	return nil
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
	remote     string
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
		case "--set-upstream", "-u":
			// bgit records the configured remote in the worktree. Accept Git's
			// common upstream flag for CLI compatibility, but there is nothing
			// extra to persist in the object-store ref update path.
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
	if len(opts.refs) > 0 && opts.refs[0] == "origin" {
		opts.remote = opts.refs[0]
		opts.refs = opts.refs[1:]
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
