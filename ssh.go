package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const defaultSSHHost = "git.bucketgit.com"

type sshSetupOptions struct {
	broker            string
	region            string
	firestoreDatabase string
	firestoreLocation string
	keys              []string
	noAgent           bool
}

func sshCommand(base config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bgit ssh setup|scaffold [args]")
	}
	switch args[0] {
	case "setup":
		return sshSetupCommand(base, args[1:], stdout, true)
	case "scaffold":
		return sshSetupCommand(base, args[1:], stdout, false)
	case "repo":
		return sshRepoCommand(base, args[1:], stdout)
	case "keys":
		return sshKeysCommand(base, args[1:], stdout)
	case "git-upload-pack", "git-receive-pack", "git-upload-archive":
		return sshGitServiceCommand(args, stdout)
	default:
		if looksLikeSSHInvocation(args) {
			return sshGitServiceCommand(args, stdout)
		}
		return fmt.Errorf("unknown ssh command %q", args[0])
	}
}

func sshGitServiceCommand(args []string, stdout io.Writer) error {
	inv, err := parseGitCommandInvocation(args)
	if err != nil {
		return err
	}
	switch inv.service {
	case gitUploadPackService, gitReceivePackService:
		return sshServeGitService(inv.service, inv.repo, inv.host, os.Stdin, stdout)
	default:
		return fmt.Errorf("unsupported git service %q", inv.service)
	}
}

func sshServeGitService(service, repo, host string, stdin io.Reader, stdout io.Writer) error {
	ctx := context.Background()
	cfg, err := configForSSHRepoForService(ctx, repo, host, service == gitUploadPackService)
	if err != nil {
		return err
	}
	if err := authorizeSSHGitService(cfg, service); err != nil {
		return err
	}
	store, closeStore, err := newRemoteStore(ctx, cfg, service == gitUploadPackService)
	if err != nil {
		return fmt.Errorf("create remote store: %w", err)
	}
	defer closeStore()
	refs, err := openNativeGitRepo(store, cfg).refs(ctx)
	if err != nil {
		return err
	}
	if head, ok := refs[branchRef(cfg.branch)]; ok {
		refs["HEAD"] = head
	}
	caps := uploadPackCapabilities()
	if service == gitReceivePackService {
		caps = receivePackCapabilities()
	}
	if err := writeAdvertisedRefs(stdout, service, refs, caps); err != nil {
		return err
	}
	if service == gitUploadPackService {
		return serveUploadPack(ctx, openNativeGitRepo(store, cfg), stdin, stdout)
	}
	return serveReceivePack(ctx, openNativeGitRepo(store, cfg), stdin, stdout)
}

func configForSSHRepo(repo string) (config, error) {
	repo = cleanGitServiceRepo(repo)
	if repo == "" {
		return config{}, errors.New("missing repository path")
	}
	if strings.Contains(repo, "://") {
		cfg, _, err := parseRepoURI(repo)
		if err != nil {
			return config{}, err
		}
		return mergeSSHRepoAuth(cfg), nil
	}
	if provider, rest, ok := strings.Cut(repo, "/"); ok && (provider == "s3" || provider == "gs" || provider == "gcs") {
		scheme := provider
		if scheme == "gcs" {
			scheme = "gs"
		}
		cfg, _, err := parseRepoURI(scheme + "://" + rest)
		if err != nil {
			return config{}, err
		}
		return mergeSSHRepoAuth(cfg), nil
	}
	_, bucket, prefix := normalizeAdminTarget(repo)
	if bucket == "" || prefix == "" {
		return config{}, fmt.Errorf("repository path must be bucket/prefix.git, got %q", repo)
	}
	cfg := config{provider: "gcs", bucket: bucket, prefix: prefix, branch: defaultBranch, auth: defaultAuthMode}
	if localCfg, err := readLocalConfig("."); err == nil {
		if localCfg.bucket == bucket && strings.Trim(localCfg.prefix, "/") == strings.Trim(prefix, "/") {
			localCfg.authExplicit = false
			localCfg.gcloudConfigurationExplicit = false
			cfg = mergeConfig(localCfg, cfg)
		}
	}
	if cfg.origin == "" {
		cfg.origin = originForConfig(cfg)
	}
	return mergeSSHRepoAuth(cfg), nil
}

func configForSSHRepoForService(ctx context.Context, repo, host string, publicFallback bool) (config, error) {
	_ = host
	cfg, err := configForSSHRepo(repo)
	if err != nil {
		return config{}, err
	}
	if cfg.provider != "gcs" || strings.Contains(cleanGitServiceRepo(repo), "://") {
		persistDiscoveredSSHRepoConfig(cfg)
		return cfg, nil
	}
	if localCfg, err := readLocalConfig("."); err == nil {
		if localCfg.bucket == cfg.bucket && strings.Trim(localCfg.prefix, "/") == strings.Trim(cfg.prefix, "/") && strings.TrimSpace(localCfg.provider) != "" {
			return cfg, nil
		}
	}
	provider, err := autodiscoverSSHRepoProvider(ctx, cfg, publicFallback)
	if err != nil {
		return config{}, err
	}
	cfg.provider = provider
	cfg.origin = ""
	cfg.origin = originForConfig(cfg)
	persistDiscoveredSSHRepoConfig(cfg)
	return cfg, nil
}

func autodiscoverSSHRepoProvider(ctx context.Context, cfg config, publicFallback bool) (string, error) {
	var misses []string
	for _, provider := range []string{"s3", "gcs"} {
		probe := cfg
		probe.provider = provider
		probe.origin = originForConfig(probe)
		store, closeStore, err := newRemoteStore(ctx, probe, publicFallback)
		if err != nil {
			misses = append(misses, provider+": "+err.Error())
			continue
		}
		refs, err := openNativeGitRepo(store, probe).refs(ctx)
		closeStore()
		if err != nil {
			misses = append(misses, provider+": "+err.Error())
			continue
		}
		if len(refs) > 0 {
			return provider, nil
		}
		misses = append(misses, provider+": no refs found")
	}
	return "", fmt.Errorf("could not autodiscover provider for %s/%s (%s)", cfg.bucket, strings.Trim(cfg.prefix, "/"), strings.Join(misses, "; "))
}

func persistDiscoveredSSHRepoConfig(cfg config) {
	worktree, err := requireWorktree(".")
	if err != nil {
		return
	}
	_ = writeBucketGitConfig(worktree, cfg)
}

func mergeSSHRepoAuth(cfg config) config {
	cfg.branch = firstNonEmpty(cfg.branch, defaultBranch)
	cfg.auth = firstNonEmpty(cfg.auth, defaultAuthMode)
	if localCfg, err := readLocalConfig("."); err == nil {
		cfg = mergeConfig(cfg, localCfg)
	}
	if cfg.origin == "" {
		cfg.origin = originForConfig(cfg)
	}
	return cfg
}

func sshSetupCommand(base config, args []string, stdout io.Writer, includeKeys bool) error {
	opts, repoArg, err := parseSSHSetupArgs(args)
	if err != nil {
		return err
	}
	cfg, err := sshSetupConfig(base, repoArg)
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
	sshURL := sshRemoteURL(cfg)
	if err := setGitOrigin(worktree, sshURL); err != nil {
		return err
	}
	for _, pair := range [][]string{
		{"core.sshCommand", "bgit ssh"},
		{"bucketgit.sshHost", defaultSSHHost},
		{"bucketgit.sshRemote", sshURL},
	} {
		if _, err := runGit(worktree, "config", "--local", pair[0], pair[1]); err != nil {
			return err
		}
	}
	brokerURL := ""
	if strings.TrimSpace(opts.broker) != "" {
		brokerURL = strings.TrimSpace(opts.broker)
		if err := writeBrokerConfig(worktree, strings.TrimSpace(opts.broker), stdout); err != nil {
			return err
		}
	} else if includeKeys {
		discovered, err := discoverBrokerURL(cfg, opts)
		if err != nil {
			fmt.Fprintf(stdout, "broker not found; provisioning bgit-broker\n")
			discovered, err = provisionBrokerURL(cfg, opts, stdout)
			if err != nil {
				return err
			}
		}
		brokerURL = discovered
		if err := writeBrokerConfig(worktree, brokerURL, stdout); err != nil {
			return err
		}
	}

	fmt.Fprintf(stdout, "configured SSH origin %s\n", sshURL)
	fmt.Fprintf(stdout, "configured core.sshCommand=bgit ssh\n")
	if includeKeys {
		keys, err := collectSSHPublicKeys(opts)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			fmt.Fprintf(stdout, "no public keys found; add one later with bgit ssh setup --key PATH\n")
			return nil
		}
		if err := writeSSHKeyDefaults(worktree, keys); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "recorded %d SSH public key default(s) for broker setup\n", len(keys))
		if brokerURL != "" {
			if firstNonEmpty(cfg.provider, "gcs") == "gcs" && strings.TrimSpace(opts.broker) == "" {
				if err := ensureGCPBrokerServices(cfg, stdout); err != nil {
					return err
				}
				if err := ensureGCPBrokerFirestoreDatabase(cfg, opts, stdout); err != nil {
					return err
				}
			}
			if err := brokerUpsertRepo(brokerURL, cfg, "admin", keys); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "upserted repo %s with admin user admin\n", cfg.origin)
		}
	}
	return nil
}

func parseSSHSetupArgs(args []string) (sshSetupOptions, string, error) {
	var opts sshSetupOptions
	var repoArg string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--broker":
			if !hasValue {
				i++
				if i >= len(args) {
					return opts, "", errors.New("--broker requires a value")
				}
				value = args[i]
			}
			opts.broker = value
		case "--key":
			if !hasValue {
				i++
				if i >= len(args) {
					return opts, "", errors.New("--key requires a value")
				}
				value = args[i]
			}
			opts.keys = append(opts.keys, value)
		case "--region":
			if !hasValue {
				i++
				if i >= len(args) {
					return opts, "", errors.New("--region requires a value")
				}
				value = args[i]
			}
			opts.region = value
		case "--firestore-database":
			if !hasValue {
				i++
				if i >= len(args) {
					return opts, "", errors.New("--firestore-database requires a value")
				}
				value = args[i]
			}
			opts.firestoreDatabase = value
		case "--firestore-location":
			if !hasValue {
				i++
				if i >= len(args) {
					return opts, "", errors.New("--firestore-location requires a value")
				}
				value = args[i]
			}
			opts.firestoreLocation = value
		case "--no-agent":
			opts.noAgent = true
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, "", fmt.Errorf("unsupported ssh setup option %s", arg)
			}
			if repoArg != "" {
				return opts, "", errors.New("ssh setup accepts at most one repository URI")
			}
			repoArg = arg
		}
	}
	return opts, repoArg, nil
}

func sshSetupConfig(base config, repoArg string) (config, error) {
	if strings.TrimSpace(repoArg) != "" {
		cfg, _, err := parseRepoURI(repoArg)
		if err != nil {
			return config{}, err
		}
		cfg.auth = base.auth
		cfg.authExplicit = base.authExplicit
		cfg.gcloudConfiguration = base.gcloudConfiguration
		cfg.gcloudConfigurationExplicit = base.gcloudConfigurationExplicit
		return cfg, nil
	}
	cfg := base
	if cfg.bucket == "" {
		localCfg, err := readLocalConfig(".")
		if err != nil {
			return config{}, errors.New("ssh setup requires a repository URI or an existing bgit origin")
		}
		cfg = mergeConfig(cfg, localCfg)
	}
	if cfg.bucket == "" || cfg.prefix == "" {
		return config{}, errors.New("ssh setup requires a repository URI or an existing bgit origin")
	}
	if cfg.branch == "" {
		cfg.branch = defaultBranch
	}
	if cfg.origin == "" {
		cfg.origin = originForConfig(cfg)
	}
	return cfg, nil
}

func sshRepoCommand(base config, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New("usage: bgit ssh repo add [--broker URL] [--key PATH] [--no-agent] [repo]")
	}
	opts, repoArg, err := parseSSHSetupArgs(args[1:])
	if err != nil {
		return err
	}
	cfg, err := sshSetupConfig(base, repoArg)
	if err != nil {
		return err
	}
	brokerURL, err := brokerURLForCommand(opts)
	if err != nil {
		return err
	}
	keys, err := collectSSHPublicKeys(opts)
	if err != nil {
		return err
	}
	if err := brokerUpsertRepo(brokerURL, cfg, "admin", keys); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "upserted repo %s in broker %s\n", cfg.origin, brokerURL)
	if len(keys) > 0 {
		fmt.Fprintf(stdout, "added %d admin key(s) for user admin\n", len(keys))
	}
	return nil
}

func sshKeysCommand(base config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bgit ssh keys list|add|remove|suspend [args]")
	}
	action := args[0]
	opts, repoArg, err := parseSSHKeyArgs(args[1:])
	if err != nil {
		return err
	}
	cfg, err := sshSetupConfig(base, repoArg)
	if err != nil {
		return err
	}
	brokerURL, err := brokerURLForCommand(opts.setup)
	if err != nil {
		return err
	}
	switch action {
	case "list":
		keys, err := brokerListKeys(brokerURL, cfg)
		if err != nil {
			return err
		}
		for _, key := range keys {
			state := "active"
			if key.Suspended {
				state = "suspended"
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", key.User, key.Role, state, key.PublicKey)
		}
		return nil
	case "add":
		keys, err := collectSSHPublicKeys(opts.setup)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			return errors.New("ssh keys add requires --key or a key loaded in ssh-agent")
		}
		if err := brokerAddKeys(brokerURL, cfg, opts.user, opts.role, keys); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "added %d key(s) for user %s with role %s\n", len(keys), opts.user, opts.role)
		return nil
	case "remove":
		identity, err := keyIdentityForMutation(opts)
		if err != nil {
			return err
		}
		if err := brokerMutateKey(brokerURL, "/keys/remove", cfg, identity); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed key %s\n", identity)
		return nil
	case "suspend":
		identity, err := keyIdentityForMutation(opts)
		if err != nil {
			return err
		}
		if err := brokerMutateKey(brokerURL, "/keys/suspend", cfg, identity); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "suspended key %s\n", identity)
		return nil
	default:
		return fmt.Errorf("unknown ssh keys command %q", action)
	}
}

type sshKeyOptions struct {
	setup       sshSetupOptions
	user        string
	role        string
	keyID       string
	fingerprint string
}

func parseSSHKeyArgs(args []string) (sshKeyOptions, string, error) {
	opts := sshKeyOptions{user: "admin", role: "read"}
	var repoArg string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--broker":
			value, next, err := optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, "", err
			}
			i = next
			opts.setup.broker = value
		case "--key":
			value, next, err := optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, "", err
			}
			i = next
			opts.setup.keys = append(opts.setup.keys, value)
		case "--region":
			value, next, err := optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, "", err
			}
			i = next
			opts.setup.region = value
		case "--no-agent":
			opts.setup.noAgent = true
		case "--user":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, "", err
			}
			opts.user = value
		case "--role":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, "", err
			}
			opts.role = value
		case "--fingerprint":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, "", err
			}
			opts.fingerprint = value
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, "", fmt.Errorf("unsupported ssh keys option %s", arg)
			}
			if repoArg == "" && strings.Contains(arg, "://") {
				repoArg = arg
				continue
			}
			if opts.keyID == "" {
				opts.keyID = arg
				continue
			}
			if repoArg == "" {
				repoArg = arg
				continue
			}
			return opts, "", errors.New("too many ssh keys arguments")
		}
	}
	return opts, repoArg, nil
}

func optionValue(args []string, i int, hasValue bool, value, name string) (string, int, error) {
	if hasValue {
		return value, i, nil
	}
	i++
	if i >= len(args) {
		return "", i, fmt.Errorf("%s requires a value", name)
	}
	return args[i], i, nil
}

func keyIdentityForMutation(opts sshKeyOptions) (string, error) {
	if strings.TrimSpace(opts.fingerprint) != "" {
		return strings.TrimSpace(opts.fingerprint), nil
	}
	if strings.TrimSpace(opts.keyID) != "" {
		return strings.TrimSpace(opts.keyID), nil
	}
	keys, err := collectSSHPublicKeys(opts.setup)
	if err != nil {
		return "", err
	}
	if len(keys) != 1 {
		return "", errors.New("key mutation requires exactly one --key, --fingerprint, or key argument")
	}
	return keys[0], nil
}

func brokerURLForCommand(opts sshSetupOptions) (string, error) {
	if strings.TrimSpace(opts.broker) != "" {
		return strings.TrimSpace(opts.broker), nil
	}
	if out, err := runGit(".", "config", "--get", "bucketgit.broker"); err == nil {
		if value := strings.TrimSpace(string(out)); value != "" {
			return value, nil
		}
	}
	return "", errors.New("broker URL is required; run bgit ssh setup or pass --broker URL")
}

func sshRemoteURL(cfg config) string {
	repo := fmt.Sprintf("%s/%s", cfg.bucket, strings.Trim(cfg.prefix, "/"))
	return fmt.Sprintf("git@%s:%s", defaultSSHHost, repo)
}

func writeBrokerConfig(worktree, brokerURL string, stdout io.Writer) error {
	if strings.TrimSpace(brokerURL) == "" {
		return nil
	}
	if _, err := runGit(worktree, "config", "--local", "bucketgit.broker", strings.TrimSpace(brokerURL)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "configured broker %s\n", strings.TrimSpace(brokerURL))
	return nil
}

type brokerRepo struct {
	Provider string `json:"provider"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
	Origin   string `json:"origin"`
}

type brokerKey struct {
	User      string `json:"user"`
	Role      string `json:"role"`
	PublicKey string `json:"public_key"`
	Suspended bool   `json:"suspended,omitempty"`
}

type brokerRepoRequest struct {
	Repo       brokerRepo `json:"repo"`
	AdminUser  string     `json:"admin_user,omitempty"`
	PublicKeys []string   `json:"public_keys,omitempty"`
	Role       string     `json:"role,omitempty"`
}

type brokerKeyRequest struct {
	Repo       brokerRepo `json:"repo"`
	User       string     `json:"user,omitempty"`
	Role       string     `json:"role,omitempty"`
	PublicKeys []string   `json:"public_keys,omitempty"`
	Key        string     `json:"key,omitempty"`
}

type brokerAuthRequest struct {
	Repo      brokerRepo `json:"repo"`
	Operation string     `json:"operation"`
}

type brokerAuthResponse struct {
	Allowed bool   `json:"allowed"`
	User    string `json:"user,omitempty"`
	Role    string `json:"role,omitempty"`
}

type brokerRefUpdateRequest struct {
	Repo brokerRepo `json:"repo"`
	Ref  string     `json:"ref"`
	Old  string     `json:"old"`
	New  string     `json:"new"`
}

type brokerKeysResponse struct {
	Keys []brokerKey `json:"keys"`
}

func brokerUpsertRepo(brokerURL string, cfg config, adminUser string, publicKeys []string) error {
	req := brokerRepoRequest{
		Repo:       repoForBroker(cfg),
		AdminUser:  adminUser,
		PublicKeys: publicKeys,
		Role:       "admin",
	}
	return brokerPost(brokerURL, "/repos/upsert", req, nil)
}

func brokerListKeys(brokerURL string, cfg config) ([]brokerKey, error) {
	var resp brokerKeysResponse
	if err := brokerPost(brokerURL, "/keys/list", brokerKeyRequest{Repo: repoForBroker(cfg)}, &resp); err != nil {
		return nil, err
	}
	return resp.Keys, nil
}

func brokerAddKeys(brokerURL string, cfg config, user, role string, publicKeys []string) error {
	req := brokerKeyRequest{
		Repo:       repoForBroker(cfg),
		User:       user,
		Role:       role,
		PublicKeys: publicKeys,
	}
	return brokerPost(brokerURL, "/keys/add", req, nil)
}

func brokerMutateKey(brokerURL, path string, cfg config, key string) error {
	return brokerPost(brokerURL, path, brokerKeyRequest{Repo: repoForBroker(cfg), Key: key}, nil)
}

func brokerUpdateRef(brokerURL string, cfg config, ref, oldHash, newHash string) error {
	req := brokerRefUpdateRequest{
		Repo: repoForBroker(cfg),
		Ref:  ref,
		Old:  firstNonEmpty(strings.TrimSpace(oldHash), zeroObjectID()),
		New:  firstNonEmpty(strings.TrimSpace(newHash), zeroObjectID()),
	}
	return brokerPost(brokerURL, "/refs/update", req, nil)
}

func optionalBrokerURLForPush() string {
	out, err := runGit(".", "config", "--get", "bucketgit.broker")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func brokerPushError(err error) error {
	return fmt.Errorf("%w\n\nBroker ref coordination failed. Retry after fetching, or use --skip-broker for a direct bucket push if this is an operator recovery action.", err)
}

func authorizeSSHGitService(cfg config, service string) error {
	operation, err := brokerOperationForGitService(service)
	if err != nil {
		return err
	}
	brokerURL, err := brokerURLForSSHService(cfg)
	if err != nil {
		return err
	}
	var resp brokerAuthResponse
	req := brokerAuthRequest{Repo: repoForBroker(cfg), Operation: operation}
	if err := brokerPost(brokerURL, "/auth/check", req, &resp); err != nil {
		return err
	}
	if !resp.Allowed {
		return fmt.Errorf("broker denied %s access", operation)
	}
	return nil
}

func brokerOperationForGitService(service string) (string, error) {
	switch service {
	case gitUploadPackService:
		return "read", nil
	case gitReceivePackService:
		return "write", nil
	default:
		return "", fmt.Errorf("unsupported git service %q", service)
	}
}

func brokerURLForSSHService(cfg config) (string, error) {
	if out, err := runGit(".", "config", "--get", "bucketgit.broker"); err == nil {
		if value := strings.TrimSpace(string(out)); value != "" {
			return value, nil
		}
	}
	url, err := discoverBrokerURL(cfg, sshSetupOptions{})
	if err != nil {
		return "", fmt.Errorf("broker URL is required for SSH Git access; run bgit ssh setup: %w", err)
	}
	return url, nil
}

func repoForBroker(cfg config) brokerRepo {
	if cfg.origin == "" {
		cfg.origin = originForConfig(cfg)
	}
	return brokerRepo{
		Provider: firstNonEmpty(cfg.provider, "gcs"),
		Bucket:   cfg.bucket,
		Prefix:   strings.Trim(cfg.prefix, "/"),
		Origin:   cfg.origin,
	}
}

func brokerPost(brokerURL, path string, req any, resp any) error {
	return brokerPostContext(context.Background(), brokerURL, path, req, resp)
}

func brokerPostContext(ctx context.Context, brokerURL, path string, req any, resp any) error {
	endpoint := strings.TrimRight(brokerURL, "/") + path
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	httpReq.Header.Set("content-type", "application/json")
	for key, value := range brokerSignatureHeaders(data) {
		httpReq.Header.Set(key, value)
	}
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	body, readErr := io.ReadAll(httpResp.Body)
	if readErr != nil {
		return readErr
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = httpResp.Status
		}
		return fmt.Errorf("broker %s: %s", path, msg)
	}
	if resp != nil && len(body) > 0 {
		if err := json.Unmarshal(body, resp); err != nil {
			return err
		}
	}
	return nil
}

func brokerSignatureHeaders(payload []byte) map[string]string {
	headers := map[string]string{}
	sock := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	if sock == "" {
		return headers
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return headers
	}
	defer conn.Close()
	signers, err := agent.NewClient(conn).Signers()
	if err != nil || len(signers) == 0 {
		return headers
	}
	message := brokerSignatureMessage(payload)
	sig, err := signers[0].Sign(nil, message)
	if err != nil {
		return headers
	}
	headers["X-Bgit-Key"] = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signers[0].PublicKey())))
	headers["X-Bgit-Signature"] = base64.StdEncoding.EncodeToString(ssh.Marshal(sig))
	headers["X-Bgit-Signature-Message"] = base64.StdEncoding.EncodeToString(message)
	return headers
}

func brokerSignatureMessage(payload []byte) []byte {
	sum := sha256.Sum256(payload)
	return []byte("bgit-broker-v1\n" + base64.StdEncoding.EncodeToString(sum[:]))
}

func discoverBrokerURL(cfg config, opts sshSetupOptions) (string, error) {
	switch firstNonEmpty(cfg.provider, "gcs") {
	case "gcs":
		return discoverGCPBrokerURL(cfg, opts)
	case "s3":
		return discoverAWSBrokerURL(cfg, opts)
	default:
		return "", fmt.Errorf("unsupported storage provider %q", cfg.provider)
	}
}

func discoverGCPBrokerURL(cfg config, opts sshSetupOptions) (string, error) {
	region := firstNonEmpty(strings.TrimSpace(opts.region), defaultGCPRegion(cfg))
	args := []string{"functions", "describe", "bgit-broker", "--gen2", "--region", region, "--format=value(serviceConfig.uri)"}
	if out, err := gcloudCommand(cfg.gcloudConfiguration, args...).Output(); err == nil {
		if url := cleanBrokerURL(string(out)); url != "" {
			return url, nil
		}
	}
	args = []string{"run", "services", "describe", "bgit-broker", "--region", region, "--format=value(status.url)"}
	if out, err := gcloudCommand(cfg.gcloudConfiguration, args...).Output(); err == nil {
		if url := cleanBrokerURL(string(out)); url != "" {
			return url, nil
		}
	}
	return "", fmt.Errorf("bgit broker was not found in GCP region %s", region)
}

func defaultGCPRegion(cfg config) string {
	for _, key := range []string{"CLOUD_RUN_REGION", "GOOGLE_CLOUD_REGION", "FUNCTION_REGION"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	for _, key := range []string{"run/region", "functions/region", "compute/region"} {
		out, err := gcloudCommand(cfg.gcloudConfiguration, "config", "get-value", key, "--quiet").Output()
		if err == nil {
			if value := strings.TrimSpace(string(out)); value != "" && value != "(unset)" {
				return value
			}
		}
	}
	return "us-central1"
}

func discoverAWSBrokerURL(cfg config, opts sshSetupOptions) (string, error) {
	region := firstNonEmpty(strings.TrimSpace(opts.region), defaultAWSRegion())
	profile := strings.TrimSpace(cfg.gcloudConfiguration)
	args := []string{"cloudformation", "describe-stacks", "--stack-name", "bgit-broker", "--region", region, "--query", "Stacks[0].Outputs[?OutputKey=='BrokerUrl'].OutputValue | [0]", "--output", "text"}
	args = appendAWSProfile(args, profile)
	if out, err := exec.Command("aws", args...).Output(); err == nil {
		if url := cleanBrokerURL(string(out)); url != "" {
			return url, nil
		}
	}
	args = []string{"ssm", "get-parameter", "--name", "/bgit/broker/default/url", "--region", region, "--query", "Parameter.Value", "--output", "text"}
	args = appendAWSProfile(args, profile)
	if out, err := exec.Command("aws", args...).Output(); err == nil {
		if url := cleanBrokerURL(string(out)); url != "" {
			return url, nil
		}
	}
	return "", fmt.Errorf("bgit broker was not found in AWS region %s", region)
}

func provisionBrokerURL(cfg config, opts sshSetupOptions, stdout io.Writer) (string, error) {
	switch firstNonEmpty(cfg.provider, "gcs") {
	case "gcs":
		return provisionGCPBrokerURL(cfg, opts, stdout)
	case "s3":
		return provisionAWSBrokerURL(cfg, opts, stdout)
	default:
		return "", fmt.Errorf("unsupported storage provider %q", cfg.provider)
	}
}

func provisionGCPBrokerURL(cfg config, opts sshSetupOptions, stdout io.Writer) (string, error) {
	region := firstNonEmpty(strings.TrimSpace(opts.region), defaultGCPRegion(cfg))
	if err := ensureGCPBrokerServices(cfg, stdout); err != nil {
		return "", err
	}
	if err := ensureGCPBrokerFirestoreDatabase(cfg, opts, stdout); err != nil {
		return "", err
	}
	sourceDir, err := os.MkdirTemp("", "bgit-gcp-broker-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(sourceDir)
	if err := writeGCPBrokerSource(sourceDir); err != nil {
		return "", err
	}
	fmt.Fprintf(stdout, "deploying GCP Cloud Run function bgit-broker in %s\n", region)
	cmd := gcloudCommand(cfg.gcloudConfiguration,
		"functions", "deploy", "bgit-broker",
		"--gen2",
		"--runtime", "nodejs22",
		"--region", region,
		"--source", sourceDir,
		"--entry-point", "broker",
		"--trigger-http",
		"--allow-unauthenticated",
		"--set-env-vars", "FIRESTORE_DATABASE="+gcpBrokerFirestoreDatabase(opts),
		"--quiet",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("deploy GCP bgit broker: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return discoverGCPBrokerURL(cfg, opts)
}

func ensureGCPBrokerServices(cfg config, stdout io.Writer) error {
	services := []string{
		"cloudfunctions.googleapis.com",
		"run.googleapis.com",
		"cloudbuild.googleapis.com",
		"artifactregistry.googleapis.com",
		"firestore.googleapis.com",
	}
	fmt.Fprintf(stdout, "ensuring GCP broker APIs are enabled\n")
	args := append([]string{"services", "enable"}, services...)
	cmd := gcloudCommand(cfg.gcloudConfiguration, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("enable GCP broker APIs: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ensureGCPBrokerFirestoreDatabase(cfg config, opts sshSetupOptions, stdout io.Writer) error {
	database := gcpBrokerFirestoreDatabase(opts)
	region := firstNonEmpty(strings.TrimSpace(opts.region), defaultGCPRegion(cfg))
	location := firstNonEmpty(strings.TrimSpace(opts.firestoreLocation), os.Getenv("BGIT_FIRESTORE_LOCATION"), region)
	describe := gcloudCommand(cfg.gcloudConfiguration,
		"firestore", "databases", "describe",
		"--database="+database,
		"--format=value(name)",
	)
	if out, err := describe.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return nil
	}
	fmt.Fprintf(stdout, "creating Firestore database %s in %s\n", database, location)
	create := gcloudCommand(cfg.gcloudConfiguration,
		"firestore", "databases", "create",
		"--database="+database,
		"--location="+location,
		"--type=firestore-native",
		"--quiet",
	)
	out, err := create.CombinedOutput()
	if err != nil {
		return fmt.Errorf("create GCP Firestore database %s in %s: %w\n%s", database, location, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gcpBrokerFirestoreDatabase(opts sshSetupOptions) string {
	return firstNonEmpty(strings.TrimSpace(opts.firestoreDatabase), os.Getenv("BGIT_FIRESTORE_DATABASE"), "bgit")
}

func writeGCPBrokerSource(dir string) error {
	files := map[string]string{
		"package.json": `{"scripts":{"start":"functions-framework --target=broker"},"dependencies":{"@google-cloud/functions-framework":"^3.4.0","@google-cloud/firestore":"^7.10.0","@google-cloud/storage":"^7.16.0"}}
`,
		"index.js": `'use strict';

const crypto = require('crypto');
const {Firestore} = require('@google-cloud/firestore');
const {Storage} = require('@google-cloud/storage');
const db = new Firestore({databaseId: process.env.FIRESTORE_DATABASE || 'bgit'});
const repos = db.collection('bgit_broker_repos');
const storage = new Storage();

function repoID(repo) {
  return [repo.provider || 'gcs', repo.bucket, repo.prefix].join(':');
}

function docID(repo) {
  return Buffer.from(repoID(repo)).toString('base64url');
}

async function loadRepo(repo) {
  const ref = repos.doc(docID(repo));
  const snap = await ref.get();
  if (!snap.exists) return {ref, data: {repo, keys: []}};
  const data = snap.data() || {};
  data.repo = data.repo || repo;
  data.keys = data.keys || [];
  return {ref, data};
}

async function saveRepo(entry) {
  await entry.ref.set(entry.data, {merge: true});
}

function readSSHString(buf, offset) {
  const len = buf.readUInt32BE(offset);
  const start = offset + 4;
  return {value: buf.subarray(start, start + len), offset: start + len};
}

function rawBody(req) {
  if (req.rawBody) return Buffer.from(req.rawBody);
  return Buffer.from(JSON.stringify(req.body || {}));
}

function expectedMessage(req) {
  const digest = crypto.createHash('sha256').update(rawBody(req)).digest('base64');
  return Buffer.from('bgit-broker-v1\n' + digest).toString('base64');
}

function normalizeKey(key) {
  return String(key || '').trim().split(/\s+/).slice(0, 2).join(' ');
}

function publicKeyObject(publicKey) {
  const parts = normalizeKey(publicKey).split(/\s+/);
  if (parts[0] !== 'ssh-ed25519') return crypto.createPublicKey(publicKey);
  const blob = Buffer.from(parts[1], 'base64');
  let parsed = readSSHString(blob, 0);
  const alg = parsed.value.toString();
  if (alg !== 'ssh-ed25519') throw new Error('unsupported SSH key algorithm');
  parsed = readSSHString(blob, parsed.offset);
  const derPrefix = Buffer.from('302a300506032b6570032100', 'hex');
  return crypto.createPublicKey({key: Buffer.concat([derPrefix, parsed.value]), format: 'der', type: 'spki'});
}

function verifySignature(req, entry) {
  const adminKeys = (entry.data.keys || []).filter((k) => k.role === 'admin' && !k.suspended);
  if (adminKeys.length === 0) return true;
  const key = signedKey(req, entry);
  return !!key && key.role === 'admin';
}

function signedKey(req, entry) {
  const keys = (entry.data.keys || []).filter((k) => !k.suspended);
  const publicKey = normalizeKey(req.get('x-bgit-key'));
  const message = String(req.get('x-bgit-signature-message') || '');
  const signature = String(req.get('x-bgit-signature') || '');
  if (!publicKey || !message || !signature || message !== expectedMessage(req)) return null;
  const key = keys.find((k) => normalizeKey(k.public_key) === publicKey);
  if (!key) return null;
  const parsed = readSSHString(Buffer.from(signature, 'base64'), 0);
  const alg = parsed.value.toString();
  const sig = readSSHString(Buffer.from(signature, 'base64'), parsed.offset).value;
  const verifyAlg = alg === 'ssh-ed25519' ? null : 'sha256';
  if (!crypto.verify(verifyAlg, Buffer.from(message, 'base64'), publicKeyObject(publicKey), sig)) return null;
  return key;
}

function roleAllows(role, operation) {
  if (role === 'admin') return true;
  if (operation === 'read') return role === 'read' || role === 'write';
  if (operation === 'write') return role === 'write';
  return false;
}

function cleanObjectPath(value) {
  const path = String(value || '').replace(/^\/+/, '');
  if (path.includes('\0')) throw new Error('invalid object path');
  return path;
}

function objectName(repo, objectPath) {
  const prefix = String(repo.prefix || '').replace(/^\/+|\/+$/g, '');
  const path = cleanObjectPath(objectPath);
  return prefix ? prefix + '/' + path : path;
}

function requireRead(req, entry) {
  const key = signedKey(req, entry);
  if (!key || !roleAllows(key.role, 'read')) {
    const err = new Error('read SSH signature required');
    err.status = 403;
    throw err;
  }
}

async function readObject(repo, objectPath) {
  const [data] = await storage.bucket(repo.bucket).file(objectName(repo, objectPath)).download();
  return data.toString('base64');
}

async function listObjects(repo, prefix) {
  const repoPrefix = String(repo.prefix || '').replace(/^\/+|\/+$/g, '');
  const queryPrefix = objectName(repo, prefix);
  const [files] = await storage.bucket(repo.bucket).getFiles({prefix: queryPrefix});
  const strip = repoPrefix ? repoPrefix + '/' : '';
  return files.map((file) => file.name.startsWith(strip) ? file.name.slice(strip.length) : file.name);
}

async function updateRefCAS(repo, ref, oldHash, newHash) {
  const id = docID(repo);
  const refDoc = repos.doc(id);
  await db.runTransaction(async (tx) => {
    const snap = await tx.get(refDoc);
    const data = snap.exists ? (snap.data() || {}) : {repo, keys: [], refs: {}};
    data.repo = data.repo || repo;
    data.keys = data.keys || [];
    data.refs = data.refs || {};
    const zero = '0000000000000000000000000000000000000000';
    const current = Object.prototype.hasOwnProperty.call(data.refs, ref) ? data.refs[ref] : oldHash;
    if (current !== oldHash) {
      const err = new Error('stale ref');
      err.status = 409;
      throw err;
    }
    if (newHash === zero) {
      delete data.refs[ref];
    } else {
      data.refs[ref] = newHash;
    }
    tx.set(refDoc, data, {merge: true});
  });
}

async function ensureRepo(repo) {
  const id = repoID(repo);
  if (!repo || !repo.bucket || !repo.prefix) throw new Error('repo is required');
  return loadRepo(repo);
}

function requireAdmin(req, entry) {
  if (!verifySignature(req, entry)) {
    const err = new Error('admin SSH signature required');
    err.status = 403;
    throw err;
  }
}

exports.broker = async (req, res) => {
  res.set('content-type', 'application/json');
  if (req.path === '/health' || req.path === '/') {
    res.status(200).send(JSON.stringify({ok: true, service: 'bgit-broker'}));
    return;
  }
  try {
    const body = req.body || {};
    if (req.path === '/repos/upsert' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      const user = body.admin_user || 'admin';
      const role = body.role || 'admin';
      for (const publicKey of body.public_keys || []) {
        if (!entry.data.keys.find((k) => normalizeKey(k.public_key) === normalizeKey(publicKey))) {
          entry.data.keys.push({user, role, public_key: publicKey, suspended: false});
        }
      }
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/keys/list' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      res.status(200).send(JSON.stringify({keys: entry.data.keys}));
      return;
    }
    if (req.path === '/keys/add' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      const user = body.user || 'admin';
      const role = body.role || 'read';
      for (const publicKey of body.public_keys || []) {
        if (!entry.data.keys.find((k) => normalizeKey(k.public_key) === normalizeKey(publicKey))) {
          entry.data.keys.push({user, role, public_key: publicKey, suspended: false});
        }
      }
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if ((req.path === '/keys/remove' || req.path === '/keys/suspend') && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      const key = String(body.key || '').trim();
      const normalized = normalizeKey(key);
      const match = (k) => normalizeKey(k.public_key) === normalized || k.public_key.includes(key);
      if (req.path === '/keys/remove') {
        entry.data.keys = entry.data.keys.filter((k) => !match(k));
      } else {
        for (const item of entry.data.keys) if (match(item)) item.suspended = true;
      }
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/auth/check' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = signedKey(req, entry);
      const operation = body.operation || '';
      const allowed = !!key && roleAllows(key.role, operation);
      res.status(200).send(JSON.stringify({allowed, user: key && key.user, role: key && key.role}));
      return;
    }
    if (req.path === '/objects/read' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireRead(req, entry);
      const data = await readObject(body.repo, body.path);
      res.status(200).send(JSON.stringify({data}));
      return;
    }
    if (req.path === '/objects/list' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireRead(req, entry);
      const paths = await listObjects(body.repo, body.prefix);
      res.status(200).send(JSON.stringify({paths}));
      return;
    }
    if (req.path === '/refs/update' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = signedKey(req, entry);
      if (!key || !roleAllows(key.role, 'write')) {
        res.status(403).send(JSON.stringify({error: 'write SSH signature required'}));
        return;
      }
      await updateRefCAS(body.repo, body.ref, body.old, body.new);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    res.status(404).send(JSON.stringify({error: 'unknown broker endpoint'}));
  } catch (err) {
    res.status(err.status || 500).send(JSON.stringify({error: err.message || String(err)}));
  }
};
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func provisionAWSBrokerURL(cfg config, opts sshSetupOptions, stdout io.Writer) (string, error) {
	region := firstNonEmpty(strings.TrimSpace(opts.region), defaultAWSRegion())
	template, err := os.CreateTemp("", "bgit-aws-broker-*.yaml")
	if err != nil {
		return "", err
	}
	templatePath := template.Name()
	defer os.Remove(templatePath)
	if _, err := template.WriteString(awsBrokerCloudFormationTemplate()); err != nil {
		template.Close()
		return "", err
	}
	if err := template.Close(); err != nil {
		return "", err
	}
	fmt.Fprintf(stdout, "deploying AWS CloudFormation stack bgit-broker in %s\n", region)
	args := []string{
		"cloudformation", "deploy",
		"--stack-name", "bgit-broker",
		"--template-file", templatePath,
		"--capabilities", "CAPABILITY_NAMED_IAM",
		"--region", region,
	}
	args = appendAWSProfile(args, strings.TrimSpace(cfg.gcloudConfiguration))
	out, err := exec.Command("aws", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("deploy AWS bgit broker: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return discoverAWSBrokerURL(cfg, opts)
}

func awsBrokerCloudFormationTemplate() string {
	return `AWSTemplateFormatVersion: '2010-09-09'
Description: Minimal bgit SSH broker control-plane endpoint.
Resources:
  BrokerRole:
    Type: AWS::IAM::Role
    Properties:
      RoleName: !Sub bgit-broker-${AWS::Region}
      AssumeRolePolicyDocument:
        Version: '2012-10-17'
        Statement:
          - Effect: Allow
            Principal:
              Service: lambda.amazonaws.com
            Action: sts:AssumeRole
      ManagedPolicyArns:
        - arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole
      Policies:
        - PolicyName: bgit-broker-table
          PolicyDocument:
            Version: '2012-10-17'
            Statement:
              - Effect: Allow
                Action:
                  - dynamodb:GetItem
                  - dynamodb:PutItem
                Resource: !GetAtt BrokerTable.Arn
              - Effect: Allow
                Action:
                  - s3:GetObject
                Resource: arn:aws:s3:::*/*
              - Effect: Allow
                Action:
                  - s3:ListBucket
                Resource: arn:aws:s3:::*
  BrokerTable:
    Type: AWS::DynamoDB::Table
    Properties:
      TableName: bgit-broker-repos
      BillingMode: PAY_PER_REQUEST
      AttributeDefinitions:
        - AttributeName: id
          AttributeType: S
      KeySchema:
        - AttributeName: id
          KeyType: HASH
  BrokerFunction:
    Type: AWS::Lambda::Function
    Properties:
      FunctionName: bgit-broker
      Runtime: nodejs22.x
      Handler: index.handler
      Role: !GetAtt BrokerRole.Arn
      Environment:
        Variables:
          TABLE_NAME: !Ref BrokerTable
      Code:
        ZipFile: |
          const crypto = require("crypto");
          const {DynamoDBClient, GetItemCommand, PutItemCommand} = require("@aws-sdk/client-dynamodb");
          const {S3Client, GetObjectCommand, ListObjectsV2Command} = require("@aws-sdk/client-s3");
          const db = new DynamoDBClient({});
          const s3 = new S3Client({});
          const table = process.env.TABLE_NAME;
          function repoID(repo) {
            return [repo.provider || "s3", repo.bucket, repo.prefix].join(":");
          }
          function docID(repo) {
            return Buffer.from(repoID(repo)).toString("base64url");
          }
          async function loadRepo(repo) {
            if (!repo || !repo.bucket || !repo.prefix) throw new Error("repo is required");
            const id = docID(repo);
            const out = await db.send(new GetItemCommand({TableName: table, Key: {id: {S: id}}}));
            if (!out.Item) return {id, data: {repo, keys: []}};
            const data = JSON.parse(out.Item.data.S || "{}");
            data.repo = data.repo || repo;
            data.keys = data.keys || [];
            return {id, data};
          }
          async function saveRepo(entry) {
            await db.send(new PutItemCommand({TableName: table, Item: {id: {S: entry.id}, data: {S: JSON.stringify(entry.data)}}}));
          }
          function readSSHString(buf, offset) {
            const len = buf.readUInt32BE(offset);
            const start = offset + 4;
            return {value: buf.subarray(start, start + len), offset: start + len};
          }
          function expectedMessage(rawBody) {
            const digest = crypto.createHash("sha256").update(Buffer.from(rawBody || "{}")).digest("base64");
            return Buffer.from("bgit-broker-v1\n" + digest).toString("base64");
          }
          function normalizeKey(key) {
            return String(key || "").trim().split(/\s+/).slice(0, 2).join(" ");
          }
          function publicKeyObject(publicKey) {
            const parts = normalizeKey(publicKey).split(/\s+/);
            if (parts[0] !== "ssh-ed25519") return crypto.createPublicKey(publicKey);
            const blob = Buffer.from(parts[1], "base64");
            let parsed = readSSHString(blob, 0);
            const alg = parsed.value.toString();
            if (alg !== "ssh-ed25519") throw new Error("unsupported SSH key algorithm");
            parsed = readSSHString(blob, parsed.offset);
            const derPrefix = Buffer.from("302a300506032b6570032100", "hex");
            return crypto.createPublicKey({key: Buffer.concat([derPrefix, parsed.value]), format: "der", type: "spki"});
          }
          function header(event, name) {
            const headers = event.headers || {};
            return headers[name] || headers[name.toLowerCase()] || "";
          }
          function verifySignature(event, entry) {
            const adminKeys = (entry.data.keys || []).filter((k) => k.role === "admin" && !k.suspended);
            if (adminKeys.length === 0) return true;
            const key = signedKey(event, entry);
            return !!key && key.role === "admin";
          }
          function signedKey(event, entry) {
            const keys = (entry.data.keys || []).filter((k) => !k.suspended);
            const publicKey = normalizeKey(header(event, "x-bgit-key"));
            const message = String(header(event, "x-bgit-signature-message"));
            const signature = String(header(event, "x-bgit-signature"));
            if (!publicKey || !message || !signature || message !== expectedMessage(event.body)) return null;
            const key = keys.find((k) => normalizeKey(k.public_key) === publicKey);
            if (!key) return null;
            const parsed = readSSHString(Buffer.from(signature, "base64"), 0);
            const alg = parsed.value.toString();
            const sig = readSSHString(Buffer.from(signature, "base64"), parsed.offset).value;
            const verifyAlg = alg === "ssh-ed25519" ? null : "sha256";
            if (!crypto.verify(verifyAlg, Buffer.from(message, "base64"), publicKeyObject(publicKey), sig)) return null;
            return key;
          }
          function roleAllows(role, operation) {
            if (role === "admin") return true;
            if (operation === "read") return role === "read" || role === "write";
            if (operation === "write") return role === "write";
            return false;
          }
          function cleanObjectPath(value) {
            const path = String(value || "").replace(/^\/+/, "");
            if (path.includes("\0")) throw new Error("invalid object path");
            return path;
          }
          function objectName(repo, objectPath) {
            const prefix = String(repo.prefix || "").replace(/^\/+|\/+$/g, "");
            const path = cleanObjectPath(objectPath);
            return prefix ? prefix + "/" + path : path;
          }
          function requireRead(event, entry) {
            const key = signedKey(event, entry);
            if (!key || !roleAllows(key.role, "read")) {
              const err = new Error("read SSH signature required");
              err.statusCode = 403;
              throw err;
            }
          }
          async function streamToBuffer(stream) {
            const chunks = [];
            for await (const chunk of stream) chunks.push(Buffer.from(chunk));
            return Buffer.concat(chunks);
          }
          async function readObject(repo, objectPath) {
            const out = await s3.send(new GetObjectCommand({Bucket: repo.bucket, Key: objectName(repo, objectPath)}));
            const data = await streamToBuffer(out.Body);
            return data.toString("base64");
          }
          async function listObjects(repo, prefix) {
            const repoPrefix = String(repo.prefix || "").replace(/^\/+|\/+$/g, "");
            const queryPrefix = objectName(repo, prefix);
            const paths = [];
            let token = undefined;
            do {
              const out = await s3.send(new ListObjectsV2Command({Bucket: repo.bucket, Prefix: queryPrefix, ContinuationToken: token}));
              for (const item of out.Contents || []) {
                const strip = repoPrefix ? repoPrefix + "/" : "";
                paths.push(item.Key.startsWith(strip) ? item.Key.slice(strip.length) : item.Key);
              }
              token = out.NextContinuationToken;
            } while (token);
            return paths;
          }
          async function updateRefCAS(repo, ref, oldHash, newHash) {
            const id = docID(repo);
            const out = await db.send(new GetItemCommand({TableName: table, Key: {id: {S: id}}}));
            const oldData = out.Item && out.Item.data ? out.Item.data.S : "";
            const data = oldData ? JSON.parse(oldData || "{}") : {repo, keys: [], refs: {}};
            data.repo = data.repo || repo;
            data.keys = data.keys || [];
            data.refs = data.refs || {};
            const zero = "0000000000000000000000000000000000000000";
            const current = Object.prototype.hasOwnProperty.call(data.refs, ref) ? data.refs[ref] : oldHash;
            if (current !== oldHash) {
              const err = new Error("stale ref");
              err.statusCode = 409;
              throw err;
            }
            if (newHash === zero) {
              delete data.refs[ref];
            } else {
              data.refs[ref] = newHash;
            }
            const item = {id: {S: id}, data: {S: JSON.stringify(data)}};
            const input = {TableName: table, Item: item};
            if (oldData) {
              input.ConditionExpression = "#data = :old";
              input.ExpressionAttributeNames = {"#data": "data"};
              input.ExpressionAttributeValues = {":old": {S: oldData}};
            } else {
              input.ConditionExpression = "attribute_not_exists(id)";
            }
            try {
              await db.send(new PutItemCommand(input));
            } catch (err) {
              if (err.name === "ConditionalCheckFailedException") {
                const stale = new Error("stale ref");
                stale.statusCode = 409;
                throw stale;
              }
              throw err;
            }
          }
          function requireAdmin(event, entry) {
            if (!verifySignature(event, entry)) {
              const err = new Error("admin SSH signature required");
              err.statusCode = 403;
              throw err;
            }
          }
          exports.handler = async (event) => {
            const path = event.rawPath || "/";
            const method = event.requestContext && event.requestContext.http ? event.requestContext.http.method : "GET";
            const body = event.body ? JSON.parse(event.body) : {};
            try {
              if (path === "/" || path === "/health") {
                return { statusCode: 200, headers: {"content-type": "application/json"}, body: JSON.stringify({ok: true, service: "bgit-broker"}) };
              }
              if (path === "/repos/upsert" && method === "POST") {
                const entry = await loadRepo(body.repo);
                requireAdmin(event, entry);
                const user = body.admin_user || "admin";
                const role = body.role || "admin";
                for (const publicKey of body.public_keys || []) {
                  if (!entry.data.keys.find((k) => normalizeKey(k.public_key) === normalizeKey(publicKey))) entry.data.keys.push({user, role, public_key: publicKey, suspended: false});
                }
                await saveRepo(entry);
                return { statusCode: 200, headers: {"content-type": "application/json"}, body: JSON.stringify({ok: true}) };
              }
              if (path === "/keys/list" && method === "POST") {
                const entry = await loadRepo(body.repo);
                requireAdmin(event, entry);
                return { statusCode: 200, headers: {"content-type": "application/json"}, body: JSON.stringify({keys: entry.data.keys}) };
              }
              if (path === "/keys/add" && method === "POST") {
                const entry = await loadRepo(body.repo);
                requireAdmin(event, entry);
                const user = body.user || "admin";
                const role = body.role || "read";
                for (const publicKey of body.public_keys || []) {
                  if (!entry.data.keys.find((k) => normalizeKey(k.public_key) === normalizeKey(publicKey))) entry.data.keys.push({user, role, public_key: publicKey, suspended: false});
                }
                await saveRepo(entry);
                return { statusCode: 200, headers: {"content-type": "application/json"}, body: JSON.stringify({ok: true}) };
              }
              if ((path === "/keys/remove" || path === "/keys/suspend") && method === "POST") {
                const entry = await loadRepo(body.repo);
                requireAdmin(event, entry);
                const key = String(body.key || "").trim();
                const normalized = normalizeKey(key);
                const match = (k) => normalizeKey(k.public_key) === normalized || k.public_key.includes(key);
                if (path === "/keys/remove") {
                  entry.data.keys = entry.data.keys.filter((k) => !match(k));
                } else {
                  for (const item of entry.data.keys) if (match(item)) item.suspended = true;
                }
                await saveRepo(entry);
                return { statusCode: 200, headers: {"content-type": "application/json"}, body: JSON.stringify({ok: true}) };
              }
              if (path === "/auth/check" && method === "POST") {
                const entry = await loadRepo(body.repo);
                const key = signedKey(event, entry);
                const operation = body.operation || "";
                const allowed = !!key && roleAllows(key.role, operation);
                return { statusCode: 200, headers: {"content-type": "application/json"}, body: JSON.stringify({allowed, user: key && key.user, role: key && key.role}) };
              }
              if (path === "/objects/read" && method === "POST") {
                const entry = await loadRepo(body.repo);
                requireRead(event, entry);
                const data = await readObject(body.repo, body.path);
                return { statusCode: 200, headers: {"content-type": "application/json"}, body: JSON.stringify({data}) };
              }
              if (path === "/objects/list" && method === "POST") {
                const entry = await loadRepo(body.repo);
                requireRead(event, entry);
                const paths = await listObjects(body.repo, body.prefix);
                return { statusCode: 200, headers: {"content-type": "application/json"}, body: JSON.stringify({paths}) };
              }
              if (path === "/refs/update" && method === "POST") {
                const entry = await loadRepo(body.repo);
                const key = signedKey(event, entry);
                if (!key || !roleAllows(key.role, "write")) {
                  return { statusCode: 403, headers: {"content-type": "application/json"}, body: JSON.stringify({error: "write SSH signature required"}) };
                }
                await updateRefCAS(body.repo, body.ref, body.old, body.new);
                return { statusCode: 200, headers: {"content-type": "application/json"}, body: JSON.stringify({ok: true}) };
              }
              return { statusCode: 404, headers: {"content-type": "application/json"}, body: JSON.stringify({error: "unknown broker endpoint"}) };
            } catch (err) {
              return { statusCode: err.statusCode || 500, headers: {"content-type": "application/json"}, body: JSON.stringify({error: err.message || String(err)}) };
            }
          };
  BrokerFunctionUrl:
    Type: AWS::Lambda::Url
    Properties:
      TargetFunctionArn: !Ref BrokerFunction
      AuthType: NONE
  BrokerFunctionUrlPermission:
    Type: AWS::Lambda::Permission
    Properties:
      FunctionName: !Ref BrokerFunction
      Action: lambda:InvokeFunctionUrl
      Principal: '*'
      FunctionUrlAuthType: NONE
  BrokerFunctionInvokePermission:
    Type: AWS::Lambda::Permission
    Properties:
      FunctionName: !Ref BrokerFunction
      Action: lambda:InvokeFunction
      Principal: '*'
      InvokedViaFunctionUrl: true
Outputs:
  BrokerUrl:
    Value: !GetAtt BrokerFunctionUrl.FunctionUrl
`
}

func appendAWSProfile(args []string, profile string) []string {
	if strings.TrimSpace(profile) == "" {
		return args
	}
	return append(args, "--profile", strings.TrimSpace(profile))
}

func cleanBrokerURL(out string) string {
	value := strings.TrimSpace(out)
	if value == "" || value == "None" || value == "null" {
		return ""
	}
	return value
}

func looksLikeSSHInvocation(args []string) bool {
	if len(args) < 2 {
		return false
	}
	last := args[len(args)-1]
	return strings.Contains(last, "git-upload-pack") || strings.Contains(last, "git-receive-pack") || strings.Contains(last, "git-upload-archive")
}

func collectSSHPublicKeys(opts sshSetupOptions) ([]string, error) {
	var keys []string
	for _, path := range opts.keys {
		data, err := os.ReadFile(expandHome(path))
		if err != nil {
			return nil, err
		}
		for _, line := range splitPublicKeyLines(string(data)) {
			keys = append(keys, line)
		}
	}
	if !opts.noAgent {
		agentKeys, err := sshAgentPublicKeys()
		if err == nil {
			keys = append(keys, agentKeys...)
		}
	}
	return uniqueStrings(keys), nil
}

func splitPublicKeyLines(data string) []string {
	var keys []string
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	return keys
}

func sshAgentPublicKeys() ([]string, error) {
	cmd := exec.Command("ssh-add", "-L")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return splitPublicKeyLines(string(out)), nil
}

func writeSSHKeyDefaults(worktree string, keys []string) error {
	for i, key := range keys {
		name := fmt.Sprintf("bucketgit.sshkey%d", i+1)
		if _, err := runGit(worktree, "config", "--local", name, key); err != nil {
			return err
		}
	}
	return nil
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
