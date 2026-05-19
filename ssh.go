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
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const defaultSSHHost = "git.bucketgit.com"

var brokerIdentityPreference string

func setBrokerIdentityPreference(value string) {
	brokerIdentityPreference = strings.TrimSpace(value)
}

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
		return errors.New("usage: bgit ssh git-upload-pack|git-receive-pack [args]")
	}
	switch args[0] {
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
	if localCfg, err := readLocalConfig("."); err == nil && localCfg.logicalRepo != "" {
		if strings.Trim(localCfg.logicalRepo, "/") == strings.Trim(repo, "/") {
			return mergeSSHRepoAuth(localCfg), nil
		}
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
				return opts, "", fmt.Errorf("unsupported ssh option %s", arg)
			}
			if repoArg != "" {
				return opts, "", errors.New("ssh commands accept at most one repository URI")
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
	if cfg.bucket == "" && cfg.logicalRepo == "" {
		localCfg, err := readLocalConfig(".")
		if err != nil {
			return config{}, errors.New("ssh command requires a repository URI or an existing bgit origin")
		}
		cfg = mergeConfig(cfg, localCfg)
	}
	if cfg.brokerURL != "" && cfg.logicalRepo != "" {
		if cfg.branch == "" {
			cfg.branch = defaultBranch
		}
		if cfg.origin == "" {
			cfg.origin = fmt.Sprintf("git@%s:%s", defaultSSHHost, strings.Trim(cfg.logicalRepo, "/"))
		}
		return cfg, nil
	}
	if cfg.bucket == "" || cfg.prefix == "" {
		return config{}, errors.New("ssh command requires a repository URI or an existing bgit origin")
	}
	if cfg.branch == "" {
		cfg.branch = defaultBranch
	}
	if cfg.origin == "" {
		cfg.origin = originForConfig(cfg)
	}
	return cfg, nil
}

func sshKeysCommand(base config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bgit admin keys list|add|remove|suspend [args]")
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
			return errors.New("admin keys add requires --key or a key loaded in ssh-agent")
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
		return fmt.Errorf("unknown admin keys command %q", action)
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
				return opts, "", fmt.Errorf("unsupported admin keys option %s", arg)
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
			return opts, "", errors.New("too many admin keys arguments")
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
	return "", errors.New("broker URL is required; run bgit setup/init or pass --broker URL")
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
	Logical  string `json:"logical,omitempty"`
}

type brokerKey struct {
	User      string `json:"user"`
	Role      string `json:"role"`
	PublicKey string `json:"public_key"`
	Source    string `json:"source,omitempty"`
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
	Source     string     `json:"source,omitempty"`
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
	Repo     brokerRepo `json:"repo"`
	Ref      string     `json:"ref"`
	Old      string     `json:"old"`
	New      string     `json:"new"`
	Override bool       `json:"override,omitempty"`
}

type brokerKeysResponse struct {
	Keys []brokerKey `json:"keys"`
}

func brokerUpsertLogicalRepo(brokerURL, provider, logicalRepo string) error {
	cfg := config{
		provider:    provider,
		prefix:      strings.Trim(logicalRepo, "/"),
		logicalRepo: strings.Trim(logicalRepo, "/"),
		origin:      fmt.Sprintf("git@%s:%s", defaultSSHHost, strings.Trim(logicalRepo, "/")),
	}
	req := brokerRepoRequest{Repo: repoForBroker(cfg)}
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
	return brokerAddKeysWithSource(brokerURL, cfg, user, role, "", publicKeys)
}

func brokerAddKeysWithSource(brokerURL string, cfg config, user, role, source string, publicKeys []string) error {
	role = normalizeBrokerRole(role)
	if !validBrokerRole(role) {
		return fmt.Errorf("invalid broker role %q", role)
	}
	req := brokerKeyRequest{
		Repo:       repoForBroker(cfg),
		User:       user,
		Role:       role,
		PublicKeys: publicKeys,
		Source:     source,
	}
	return brokerPost(brokerURL, "/keys/add", req, nil)
}

func brokerMutateKey(brokerURL, path string, cfg config, key string) error {
	return brokerPost(brokerURL, path, brokerKeyRequest{Repo: repoForBroker(cfg), Key: key}, nil)
}

func validBrokerRole(role string) bool {
	switch strings.TrimSpace(role) {
	case "owner", "admin", "maintainer", "developer", "triage", "read":
		return true
	default:
		return false
	}
}

func normalizeBrokerRole(role string) string {
	switch strings.TrimSpace(role) {
	case "write":
		return "developer"
	default:
		return strings.TrimSpace(role)
	}
}

func brokerUpdateRef(brokerURL string, cfg config, ref, oldHash, newHash string) error {
	return brokerUpdateRefWithOverride(brokerURL, cfg, ref, oldHash, newHash, false)
}

func brokerUpdateRefWithOverride(brokerURL string, cfg config, ref, oldHash, newHash string, override bool) error {
	req := brokerRefUpdateRequest{
		Repo:     repoForBroker(cfg),
		Ref:      ref,
		Old:      firstNonEmpty(strings.TrimSpace(oldHash), zeroObjectID()),
		New:      firstNonEmpty(strings.TrimSpace(newHash), zeroObjectID()),
		Override: override,
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
		return "", fmt.Errorf("broker URL is required for SSH Git access; run bgit init: %w", err)
	}
	return url, nil
}

func repoForBroker(cfg config) brokerRepo {
	if cfg.origin == "" {
		cfg.origin = originForConfig(cfg)
	}
	logical := strings.Trim(firstNonEmpty(cfg.logicalRepo, cfg.prefix), "/")
	return brokerRepo{
		Provider: firstNonEmpty(cfg.provider, "gcs"),
		Bucket:   cfg.bucket,
		Prefix:   strings.Trim(cfg.prefix, "/"),
		Origin:   cfg.origin,
		Logical:  logical,
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
	headerSets := brokerSignatureHeaderSetsForBroker(brokerURL, data)
	if len(headerSets) == 0 {
		headerSets = []map[string]string{{}}
	} else {
		headerSets = append(headerSets, map[string]string{})
	}
	var lastErr error
	for i, headers := range headerSets {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			return err
		}
		httpReq.Header.Set("content-type", "application/json")
		for key, value := range headers {
			httpReq.Header.Set(key, value)
		}
		httpResp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
			if fingerprint := headers["X-Bgit-Key-Fingerprint"]; fingerprint != "" {
				_ = writeRepoAuthCache(brokerURL, data, fingerprint)
			}
			if resp != nil && len(body) > 0 {
				if err := json.Unmarshal(body, resp); err != nil {
					return err
				}
			}
			return nil
		}
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = httpResp.Status
		}
		lastErr = fmt.Errorf("broker %s: %s", path, msg)
		if httpResp.StatusCode != http.StatusForbidden || i == len(headerSets)-1 || !brokerForbiddenAllowsSignatureRetry(msg) {
			return lastErr
		}
	}
	return lastErr
}

func brokerForbiddenAllowsSignatureRetry(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "ssh signature required")
}

func brokerSignatureHeaders(payload []byte) map[string]string {
	sets := brokerSignatureHeaderSetsForBroker("", payload)
	if len(sets) == 0 {
		return map[string]string{}
	}
	return sets[0]
}

func brokerSignatureHeaderSets(payload []byte) []map[string]string {
	return brokerSignatureHeaderSetsForBroker("", payload)
}

func brokerSignatureHeaderSetsForBroker(brokerURL string, payload []byte) []map[string]string {
	sock := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	if sock == "" {
		return nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil
	}
	defer conn.Close()
	signers, err := agent.NewClient(conn).Signers()
	if err != nil || len(signers) == 0 {
		return nil
	}
	message := brokerSignatureMessage(payload)
	preferred := preferredBrokerKeyFingerprints(brokerURL, payload)
	type signedHeaders struct {
		fingerprint string
		headers     map[string]string
	}
	var signed []signedHeaders
	for _, signer := range signers {
		sig, err := signer.Sign(nil, message)
		if err != nil {
			continue
		}
		fingerprint := ssh.FingerprintSHA256(signer.PublicKey())
		signed = append(signed, signedHeaders{fingerprint: fingerprint, headers: map[string]string{
			"X-Bgit-Key":               strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
			"X-Bgit-Key-Fingerprint":   fingerprint,
			"X-Bgit-Signature":         base64.StdEncoding.EncodeToString(ssh.Marshal(sig)),
			"X-Bgit-Signature-Message": base64.StdEncoding.EncodeToString(message),
		}})
	}
	sort.SliceStable(signed, func(i, j int) bool {
		return preferredBrokerKeyRank(signed[i].fingerprint, preferred) < preferredBrokerKeyRank(signed[j].fingerprint, preferred)
	})
	var sets []map[string]string
	for _, item := range signed {
		sets = append(sets, item.headers)
	}
	return sets
}

func preferredBrokerKeyRank(fingerprint string, preferred []string) int {
	for i, value := range preferred {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(fingerprint)) {
			return i
		}
	}
	return len(preferred) + 1
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
	if out, err := awsCommand(context.Background(), profile, args...).Output(); err == nil {
		if url := cleanBrokerURL(string(out)); url != "" {
			return url, nil
		}
	}
	args = []string{"ssm", "get-parameter", "--name", "/bgit/broker/default/url", "--region", region, "--query", "Parameter.Value", "--output", "text"}
	if out, err := awsCommand(context.Background(), profile, args...).Output(); err == nil {
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
	serviceAccount, err := ensureGCPBrokerServiceAccount(cfg, stdout)
	if err != nil {
		return "", err
	}
	if err := ensureGCPBrokerRuntimePermissions(cfg, serviceAccount, stdout); err != nil {
		return "", err
	}
	if err := ensureGCPBrokerDeployerPermission(cfg, serviceAccount, stdout); err != nil {
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
		"--service-account", serviceAccount,
		"--set-env-vars", "FIRESTORE_DATABASE="+gcpBrokerFirestoreDatabase(opts)+",BROKER_VERSION="+brokerVersion+",BGIT_SIGNING_SERVICE_ACCOUNT="+serviceAccount,
		"--quiet",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("deploy GCP bgit broker: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	if err := ensureGCPBrokerSigningPermission(cfg, serviceAccount, stdout); err != nil {
		return "", err
	}
	return discoverGCPBrokerURL(cfg, opts)
}

func ensureGCPBrokerServices(cfg config, stdout io.Writer) error {
	project := gcloudProject(cfg)
	if project == "" {
		return errors.New("GCP project is not configured")
	}
	services := []string{
		"serviceusage.googleapis.com",
		"cloudresourcemanager.googleapis.com",
		"cloudfunctions.googleapis.com",
		"run.googleapis.com",
		"cloudbuild.googleapis.com",
		"artifactregistry.googleapis.com",
		"firestore.googleapis.com",
		"iamcredentials.googleapis.com",
	}
	fmt.Fprintf(stdout, "ensuring GCP broker APIs are enabled\n")
	args := append([]string{"services", "enable"}, services...)
	args = append(args, "--project="+project, "--quiet")
	cmd := gcloudCommand(cfg.gcloudConfiguration, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if gcpBrokerServicesNeedBilling(string(out)) {
			return fmt.Errorf("enable GCP broker APIs: project %s does not have billing enabled; link a billing account with `gcloud billing projects link %s --billing-account BILLING_ACCOUNT` and rerun setup\n%s", project, project, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("enable GCP broker APIs: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	if err := waitForGCPBrokerServices(cfg, project, services, stdout); err != nil {
		return err
	}
	return nil
}

func gcpBrokerServicesNeedBilling(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "billing account") && strings.Contains(message, "not found") ||
		strings.Contains(message, "billing must be enabled") ||
		strings.Contains(message, "ureq_project_billing_not_found") ||
		strings.Contains(message, "billing-enabled")
}

func waitForGCPBrokerServices(cfg config, project string, services []string, stdout io.Writer) error {
	return waitForGCPServicesEnabled(cfg, project, services, stdout, "GCP broker APIs")
}

func waitForGCPServicesEnabled(cfg config, project string, services []string, stdout io.Writer, label string) error {
	want := map[string]struct{}{}
	for _, service := range services {
		want[service] = struct{}{}
	}
	var lastMissing []string
	for i := 0; i < 24; i++ {
		enabled, err := gcpEnabledServices(cfg, project)
		if err == nil {
			missing := missingGCPServices(want, enabled)
			if len(missing) == 0 {
				return nil
			}
			lastMissing = missing
		}
		if i == 0 {
			fmt.Fprintf(stdout, "waiting for %s to become enabled\n", label)
		}
		time.Sleep(5 * time.Second)
	}
	if len(lastMissing) == 0 {
		return fmt.Errorf("%s were not visible as enabled before timeout", label)
	}
	return fmt.Errorf("%s were not visible as enabled before timeout: %s", label, strings.Join(lastMissing, ", "))
}

func gcpEnabledServices(cfg config, project string) (map[string]struct{}, error) {
	cmd := gcloudCommand(cfg.gcloudConfiguration,
		"services", "list",
		"--enabled",
		"--project="+project,
		"--format=value(config.name)",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	enabled := map[string]struct{}{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		for _, service := range strings.Fields(scanner.Text()) {
			enabled[service] = struct{}{}
		}
	}
	return enabled, scanner.Err()
}

func missingGCPServices(want map[string]struct{}, enabled map[string]struct{}) []string {
	var missing []string
	for service := range want {
		if _, ok := enabled[service]; !ok {
			missing = append(missing, service)
		}
	}
	sort.Strings(missing)
	return missing
}

func ensureGCPBrokerServiceAccount(cfg config, stdout io.Writer) (string, error) {
	project := gcloudProject(cfg)
	if project == "" {
		return "", errors.New("GCP project is not configured")
	}
	email := gcpBrokerServiceAccountEmail(project)
	describe := gcloudCommand(cfg.gcloudConfiguration,
		"iam", "service-accounts", "describe", email,
		"--format=value(email)",
	)
	if out, err := describe.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		fmt.Fprintf(stdout, "using GCP broker service account %s\n", email)
		return email, nil
	}
	fmt.Fprintf(stdout, "creating GCP broker service account %s\n", email)
	create := gcloudCommand(cfg.gcloudConfiguration,
		"iam", "service-accounts", "create", "bgit-broker",
		"--display-name=BucketGit Broker",
		"--project="+project,
		"--quiet",
	)
	out, err := create.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("create GCP broker service account %s: %w\n%s", email, err, strings.TrimSpace(string(out)))
	}
	return email, nil
}

func gcpBrokerServiceAccountEmail(project string) string {
	return "bgit-broker@" + project + ".iam.gserviceaccount.com"
}

func ensureGCPBrokerRuntimePermissions(cfg config, serviceAccount string, stdout io.Writer) error {
	project := gcloudProject(cfg)
	if project == "" {
		return errors.New("GCP project is not configured")
	}
	for _, role := range []string{"roles/datastore.user", "roles/storage.admin"} {
		fmt.Fprintf(stdout, "granting GCP broker %s to %s\n", role, serviceAccount)
		cmd := gcloudCommand(cfg.gcloudConfiguration,
			"projects", "add-iam-policy-binding", project,
			"--member=serviceAccount:"+serviceAccount,
			"--role="+role,
			"--quiet",
		)
		out, err := runGcloudIAMBindingWithRetry(cmd)
		if err != nil {
			return fmt.Errorf("grant GCP broker %s: %w\n%s", role, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func ensureGCPBrokerDeployerPermission(cfg config, serviceAccount string, stdout io.Writer) error {
	account := gcloudAccount(cfg)
	if account == "" {
		return nil
	}
	member := "user:" + account
	if strings.HasSuffix(account, ".gserviceaccount.com") {
		member = "serviceAccount:" + account
	}
	fmt.Fprintf(stdout, "granting GCP broker deploy permission to %s\n", member)
	cmd := gcloudCommand(cfg.gcloudConfiguration,
		"iam", "service-accounts", "add-iam-policy-binding", serviceAccount,
		"--member="+member,
		"--role=roles/iam.serviceAccountUser",
		"--quiet",
	)
	out, err := runGcloudIAMBindingWithRetry(cmd)
	if err != nil {
		if fallbackErr := ensureGCPBrokerProjectDeployerPermission(cfg, member); fallbackErr != nil {
			return fmt.Errorf("grant GCP broker deploy permission: %w\n%s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func ensureGCPBrokerProjectDeployerPermission(cfg config, member string) error {
	project := gcloudProject(cfg)
	if project == "" {
		return errors.New("GCP project is not configured")
	}
	cmd := gcloudCommand(cfg.gcloudConfiguration,
		"projects", "add-iam-policy-binding", project,
		"--member="+member,
		"--role=roles/iam.serviceAccountUser",
		"--quiet",
	)
	out, err := runGcloudIAMBindingWithRetry(cmd)
	if err != nil {
		return fmt.Errorf("grant project-level deploy permission: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ensureGCPBrokerSigningPermission(cfg config, serviceAccount string, stdout io.Writer) error {
	fmt.Fprintf(stdout, "granting GCP broker signBlob permission to %s\n", serviceAccount)
	args := []string{
		"iam", "service-accounts", "add-iam-policy-binding", serviceAccount,
		"--member=serviceAccount:" + serviceAccount,
		"--role=roles/iam.serviceAccountTokenCreator",
		"--quiet",
	}
	if project := gcloudProject(cfg); project != "" {
		args = append(args, "--project="+project)
	}
	cmd := gcloudCommand(cfg.gcloudConfiguration, args...)
	bindOut, bindErr := runGcloudIAMBindingWithRetry(cmd)
	if bindErr != nil {
		if err := ensureGCPBrokerProjectSigningPermission(cfg, serviceAccount); err != nil {
			return fmt.Errorf("grant GCP broker signBlob permission: %w\n%s", bindErr, strings.TrimSpace(string(bindOut)))
		}
	}
	return nil
}

func ensureGCPBrokerProjectSigningPermission(cfg config, serviceAccount string) error {
	project := gcloudProject(cfg)
	if project == "" {
		return errors.New("GCP project is not configured")
	}
	cmd := gcloudCommand(cfg.gcloudConfiguration,
		"projects", "add-iam-policy-binding", project,
		"--member=serviceAccount:"+serviceAccount,
		"--role=roles/iam.serviceAccountTokenCreator",
		"--quiet",
	)
	out, err := runGcloudIAMBindingWithRetry(cmd)
	if err != nil {
		return fmt.Errorf("grant project-level signBlob permission: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runGcloudIAMBindingWithRetry(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.CombinedOutput()
	if err == nil || !gcloudIAMBindingRetryable(string(out), err) {
		return out, err
	}
	var lastOut []byte
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		time.Sleep(time.Duration(attempt+1) * time.Second)
		retry := exec.Command(cmd.Path, cmd.Args[1:]...)
		retry.Env = cmd.Env
		retry.Dir = cmd.Dir
		lastOut, lastErr = retry.CombinedOutput()
		if lastErr == nil || !gcloudIAMBindingRetryable(string(lastOut), lastErr) {
			return lastOut, lastErr
		}
	}
	return lastOut, lastErr
}

func gcloudIAMBindingRetryable(out string, err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(out + "\n" + err.Error())
	return strings.Contains(message, "service account") &&
		(strings.Contains(message, "does not exist") ||
			strings.Contains(message, "not found") ||
			strings.Contains(message, "principal") && strings.Contains(message, "not found"))
}

func gcloudAccount(cfg config) string {
	out, err := gcloudCommand(cfg.gcloudConfiguration, "config", "get-value", "account", "--quiet").Output()
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(out))
	if value == "(unset)" {
		return ""
	}
	return value
}

func gcloudProject(cfg config) string {
	out, err := gcloudCommand(cfg.gcloudConfiguration, "config", "get-value", "project", "--quiet").Output()
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(out))
	if value == "(unset)" {
		return ""
	}
	return value
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
	fmt.Fprintf(stdout, "deploying AWS CloudFormation stack bgit-broker in %s", region)
	if strings.TrimSpace(cfg.gcloudConfiguration) != "" {
		fmt.Fprintf(stdout, " with profile %s", strings.TrimSpace(cfg.gcloudConfiguration))
	}
	fmt.Fprintln(stdout)
	s3Bucket, err := ensureAWSBrokerDeploymentBucket(cfg, region, stdout)
	if err != nil {
		return "", err
	}
	args := []string{
		"cloudformation", "deploy",
		"--stack-name", "bgit-broker",
		"--template-file", templatePath,
		"--s3-bucket", s3Bucket,
		"--capabilities", "CAPABILITY_NAMED_IAM",
		"--region", region,
	}
	out, err := awsCommand(context.Background(), strings.TrimSpace(cfg.gcloudConfiguration), args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("deploy AWS bgit broker: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return discoverAWSBrokerURL(cfg, opts)
}

func ensureAWSBrokerDeploymentBucket(cfg config, region string, stdout io.Writer) (string, error) {
	accountID, _ := awsCallerIdentity(context.Background(), strings.TrimSpace(cfg.gcloudConfiguration))
	if accountID == "" {
		return "", errors.New("discover AWS account id for broker deployment bucket")
	}
	bucket := fmt.Sprintf("bgit-broker-artifacts-%s-%s", accountID, region)
	headArgs := []string{"s3api", "head-bucket", "--bucket", bucket, "--region", region}
	if err := awsCommand(context.Background(), strings.TrimSpace(cfg.gcloudConfiguration), headArgs...).Run(); err == nil {
		return bucket, nil
	}
	fmt.Fprintf(stdout, "creating AWS broker deployment bucket %s in %s\n", bucket, region)
	createArgs := []string{"s3api", "create-bucket", "--bucket", bucket, "--region", region}
	if region != "us-east-1" {
		createArgs = append(createArgs, "--create-bucket-configuration", "LocationConstraint="+region)
	}
	out, err := awsCommand(context.Background(), strings.TrimSpace(cfg.gcloudConfiguration), createArgs...).CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if strings.Contains(text, "BucketAlreadyOwnedByYou") || strings.Contains(text, "BucketAlreadyExists") {
			return bucket, nil
		}
		return "", fmt.Errorf("create AWS broker deployment bucket %s: %w\n%s", bucket, err, text)
	}
	return bucket, nil
}

func appendAWSProfile(args []string, profile string) []string {
	if strings.TrimSpace(profile) == "" {
		return args
	}
	return append(args, "--profile", strings.TrimSpace(profile))
}

func awsCommand(ctx context.Context, profile string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "aws", args...)
	if strings.TrimSpace(profile) != "" {
		cmd.Env = append(os.Environ(),
			"AWS_PROFILE="+strings.TrimSpace(profile),
			"AWS_SDK_LOAD_CONFIG=1",
		)
	}
	return cmd
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
