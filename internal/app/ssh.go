package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	brokerclient "github.com/bucketgit/bgit/broker/client"
	internalidentity "github.com/bucketgit/bgit/internal/identity"
	internalsetup "github.com/bucketgit/bgit/internal/setup"
	"github.com/bucketgit/bgit/protocol"
	"golang.org/x/crypto/ssh"
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

type provisionedBroker struct {
	URL            string
	BootstrapToken string
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
	return serveGitServiceWithConfig(ctx, service, cfg, stdin, stdout)
}

func serveGitServiceWithConfig(ctx context.Context, service string, cfg config, stdin io.Reader, stdout io.Writer) error {
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
	if cfg.provider == "local" {
		if _, err := ensureLocalBrokerForCommand(ctx, &cfg); err != nil {
			return config{}, err
		}
		return cfg, nil
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

const coreTeamID = "t_core"
const coreTeamName = "core"

func brokerListKeys(brokerURL string, cfg config) ([]protocol.Key, error) {
	endpoints, err := brokerEndpointClient(brokerURL)
	if err != nil {
		return nil, err
	}
	return endpoints.ListKeys(context.Background(), protocol.KeyRequest{Repo: repoForBroker(cfg)})
}

func brokerAddKeys(brokerURL string, cfg config, user, role string, publicKeys []string) error {
	return brokerAddKeysWithSource(brokerURL, cfg, user, role, "", publicKeys)
}

func brokerAddKeysWithSource(brokerURL string, cfg config, user, role, source string, publicKeys []string) error {
	role = normalizeBrokerRole(role)
	if !validBrokerRole(role) {
		return fmt.Errorf("invalid broker role %q", role)
	}
	req := protocol.KeyRequest{
		Repo:       repoForBroker(cfg),
		User:       user,
		Role:       role,
		PublicKeys: publicKeys,
		Source:     source,
	}
	endpoints, err := brokerEndpointClient(brokerURL)
	if err != nil {
		return err
	}
	return endpoints.AddKey(context.Background(), req)
}

func brokerMutateKey(brokerURL, path string, cfg config, key string) error {
	endpoints, err := brokerEndpointClient(brokerURL)
	if err != nil {
		return err
	}
	request := protocol.KeyRequest{Repo: repoForBroker(cfg), Key: key}
	switch path {
	case "/keys/remove":
		return endpoints.RemoveKey(context.Background(), request)
	case "/keys/suspend":
		return endpoints.SuspendKey(context.Background(), request, true)
	case "/keys/unsuspend":
		return endpoints.SuspendKey(context.Background(), request, false)
	default:
		return fmt.Errorf("unsupported key mutation endpoint %q", path)
	}
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
	req := protocol.RefUpdateRequest{
		Repo:     repoForBroker(cfg),
		Ref:      ref,
		Old:      firstNonEmpty(strings.TrimSpace(oldHash), zeroObjectID()),
		New:      firstNonEmpty(strings.TrimSpace(newHash), zeroObjectID()),
		Override: override,
	}
	endpoints, err := brokerEndpointClient(brokerURL)
	if err != nil {
		return err
	}
	return endpoints.UpdateRef(context.Background(), req)
}

func optionalBrokerURLForPush() string {
	out, err := runGit(".", "config", "--get", "bucketgit.broker")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func brokerPushError(err error) error {
	//lint:ignore ST1005 Multiline CLI remediation is intentionally formatted and capitalized.
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
	req := protocol.AuthRequest{Repo: repoForBroker(cfg), Operation: operation}
	endpoints, err := brokerEndpointClient(brokerURL)
	if err != nil {
		return err
	}
	resp, err := endpoints.Authorize(context.Background(), req)
	if err != nil {
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
	if cfg.provider == "local" {
		if cfg.brokerURL != "" {
			return cfg.brokerURL, nil
		}
		profileName, regionName := localProfileSelection(cfg.gcloudConfiguration)
		return localBrokerURL(profileName, regionName), nil
	}
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

func repoForBroker(cfg config) protocol.Repository {
	if cfg.origin == "" {
		cfg.origin = originForConfig(cfg)
	}
	logical := strings.Trim(firstNonEmpty(cfg.logicalRepo, cfg.prefix), "/")
	if normalized, err := normalizeLogicalRepoName(logical); err == nil {
		logical = normalized
	}
	return protocol.Repository{
		Provider: firstNonEmpty(cfg.storageProvider, cfg.provider, "gcs"),
		Bucket:   cfg.bucket,
		Prefix:   strings.Trim(cfg.prefix, "/"),
		Origin:   cfg.origin,
		Logical:  logical,
		Profile:  cfg.storageProfile,
		Region:   cfg.storageRegion,
		TeamID:   brokerTeamIDForConfig(cfg),
	}
}

func brokerTeamIDForConfig(cfg config) string {
	teamID := strings.TrimSpace(cfg.teamID)
	if teamID == "" && strings.TrimSpace(cfg.logicalRepo) != "" {
		teamID = coreTeamID
	}
	return teamID
}

func brokerPostJSONContextWithHeaders(ctx context.Context, brokerURL, path string, req any, resp any, extraHeaders map[string]string) error {
	if isLocalBrokerURL(brokerURL) {
		return localBrokerPostContext(ctx, brokerURL, path, req, resp)
	}
	c, err := newBrokerHTTPClient(brokerURL)
	if err != nil {
		return err
	}
	headers := http.Header{}
	for key, value := range extraHeaders {
		headers.Set(key, value)
	}
	return c.PostJSON(ctx, path, req, resp, headers)
}

func newBrokerHTTPClient(brokerURL string) (*brokerclient.Client, error) {
	return brokerclient.New(brokerURL, brokerclient.Options{
		Signatures: brokerclient.SignatureProviderFunc(func(_ context.Context, baseURL, endpoint string, payload []byte) ([]http.Header, error) {
			sets := brokerSignatureHeaderSetsForBroker(baseURL, endpoint, payload)
			headers := make([]http.Header, 0, len(sets))
			for _, set := range sets {
				header := http.Header{}
				for key, value := range set {
					header.Set(key, value)
				}
				headers = append(headers, header)
			}
			return headers, nil
		}),
		AllowUnsignedFallback: true,
		DecodeError: func(endpoint string, status int, message string) error {
			return brokerHTTPStatusError(endpoint, status, message)
		},
		Retry: func(status int, message string) bool {
			return status == http.StatusForbidden && brokerForbiddenAllowsSignatureRetry(message)
		},
		ObserveSuccess: func(baseURL string, payload []byte, headers http.Header) {
			if fingerprint := headers.Get(protocol.HeaderKeyFingerprint); fingerprint != "" {
				_ = writeRepoAuthCache(baseURL, payload, fingerprint)
			}
		},
	})
}

type localBrokerCaller struct{ brokerURL string }

func (c localBrokerCaller) PostJSON(ctx context.Context, endpoint string, request, response any, _ http.Header) error {
	return localBrokerPostContext(ctx, c.brokerURL, endpoint, request, response)
}

func brokerEndpointClient(brokerURL string) (*brokerclient.Endpoints, error) {
	if isLocalBrokerURL(brokerURL) {
		return brokerclient.NewEndpoints(localBrokerCaller{brokerURL: brokerURL}), nil
	}
	client, err := newBrokerHTTPClient(brokerURL)
	if err != nil {
		return nil, err
	}
	return brokerclient.NewEndpoints(client), nil
}

func brokerHTTPError(path, msg string) error {
	return brokerHTTPStatusError(path, 0, msg)
}

func brokerHTTPStatusError(path string, status int, msg string) error {
	kind := error(nil)
	lower := strings.ToLower(msg)
	switch {
	case status == http.StatusConflict:
		kind = protocol.ErrConflict
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = protocol.ErrUnauthorized
	case status == http.StatusNotImplemented || strings.Contains(lower, "unknown broker endpoint"):
		kind = protocol.ErrUnsupported
	}
	if brokerLooksIncompatibleWithV2Signatures(msg) {
		msg += "\nbroker is incompatible with this bgit version and must be upgraded for v2 request signatures; run `bgit admin broker upgrade` with a bgit version that can administer this broker"
	}
	return &protocol.BrokerError{Endpoint: path, Status: status, Message: msg, Kind: kind}
}

func brokerLooksIncompatibleWithV2Signatures(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if !strings.Contains(msg, "ssh signature required") {
		return false
	}
	for _, scoped := range []string{"read ssh signature required", "write ssh signature required", "admin ssh signature required", "owner ssh signature required", "merge ssh signature required", "broker admin ssh signature required"} {
		if strings.Contains(msg, scoped) {
			return false
		}
	}
	return true
}

func brokerForbiddenAllowsSignatureRetry(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "ssh signature required")
}

func brokerSignatureHeaderSetsForBroker(brokerURL, requestPath string, payload []byte) []map[string]string {
	signers := explicitBrokerSigners()
	agentSigners, cleanup, err := sshAgentSigners()
	if err == nil && len(agentSigners) > 0 {
		signers = append(signers, agentSigners...)
	}
	if len(signers) == 0 {
		return nil
	}
	if cleanup != nil {
		defer cleanup()
	}
	preferred := preferredBrokerKeyFingerprints(brokerURL, payload)
	provider := brokerclient.NewV2Signatures(brokerclient.V2SignatureOptions{
		Signers: signers,
		Rank: func(fingerprint string) int {
			return preferredBrokerKeyRank(fingerprint, preferred)
		},
	})
	headers, err := provider.HeaderSets(context.Background(), brokerURL, requestPath, payload)
	if err != nil {
		return nil
	}
	sets := make([]map[string]string, 0, len(headers))
	for _, header := range headers {
		item := map[string]string{}
		for key, values := range header {
			if len(values) > 0 {
				item[key] = values[0]
			}
		}
		sets = append(sets, item)
	}
	return sets
}

func explicitBrokerSigners() []ssh.Signer {
	return internalidentity.ExplicitSigners(brokerIdentityPreference)
}

func preferredBrokerKeyRank(fingerprint string, preferred []string) int {
	return internalidentity.FingerprintRank(fingerprint, preferred)
}

func brokerSignatureMessage(method, requestPath, host, timestamp, nonce string, payload []byte) []byte {
	return protocol.SignatureMessage(protocol.RequestMetadata{
		Method:    method,
		Path:      requestPath,
		Host:      host,
		Timestamp: timestamp,
		Nonce:     nonce,
	}, payload)
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

func provisionBroker(cfg config, opts sshSetupOptions, stdout io.Writer) (provisionedBroker, error) {
	token, err := randomBrokerSecret()
	if err != nil {
		return provisionedBroker{}, err
	}
	hash := brokerSecretHash(token)
	switch firstNonEmpty(cfg.provider, "gcs") {
	case "gcs":
		url, err := provisionGCPBrokerURLWithBootstrap(cfg, opts, stdout, hash)
		return provisionedBroker{URL: url, BootstrapToken: token}, err
	case "s3":
		url, err := provisionAWSBrokerURLWithBootstrap(cfg, opts, stdout, hash)
		return provisionedBroker{URL: url, BootstrapToken: token}, err
	case "local":
		url := firstNonEmpty(strings.TrimSpace(os.Getenv("BGIT_LOCAL_BROKER_URL")), localBrokerURL("default", "default"))
		fmt.Fprintf(stdout, "using local broker %s\n", url)
		if token := strings.TrimSpace(os.Getenv("BGIT_LOCAL_BROKER_BOOTSTRAP_TOKEN")); token != "" {
			return provisionedBroker{URL: strings.TrimRight(url, "/"), BootstrapToken: token}, nil
		}
		return provisionedBroker{URL: strings.TrimRight(url, "/"), BootstrapToken: token}, nil
	default:
		return provisionedBroker{}, fmt.Errorf("unsupported storage provider %q", cfg.provider)
	}
}

func provisionGCPBrokerURL(cfg config, opts sshSetupOptions, stdout io.Writer) (string, error) {
	return provisionGCPBrokerURLWithBootstrap(cfg, opts, stdout, "")
}

func provisionGCPBrokerURLWithBootstrap(cfg config, opts sshSetupOptions, stdout io.Writer, bootstrapHash string) (string, error) {
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
	ciSecret, err := ensureGCPMaterializerSecret(cfg, serviceAccount, stdout)
	if err != nil {
		return "", err
	}
	ciMaterializerURL, err := provisionGCPMaterializerURL(cfg, region, serviceAccount, ciSecret, stdout)
	if err != nil {
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
	cmd := internalsetup.GCPFunctionDeployCommand(context.Background(), internalsetup.GCPFunctionDeployment{
		Configuration: cfg.gcloudConfiguration, Name: "bgit-broker", Region: region,
		Source: sourceDir, EntryPoint: "broker", ServiceAccount: serviceAccount,
		Environment: gcpBrokerEnvVars(cfg, opts, serviceAccount, ciSecret, ciMaterializerURL, bootstrapHash), Public: true,
	})
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("deploy GCP bgit broker: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	if err := ensureGCPBrokerSigningPermission(cfg, serviceAccount, stdout); err != nil {
		return "", err
	}
	return discoverGCPBrokerURL(cfg, opts)
}

func provisionGCPMaterializerURL(cfg config, region, serviceAccount, ciSecret string, stdout io.Writer) (string, error) {
	sourceDir, err := os.MkdirTemp("", "bgit-gcp-materializer-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(sourceDir)
	if err := writeGCPMaterializerSource(sourceDir); err != nil {
		return "", err
	}
	fmt.Fprintf(stdout, "deploying GCP Cloud Run function bgit-ci-materializer in %s\n", region)
	cmd := internalsetup.GCPFunctionDeployCommand(context.Background(), internalsetup.GCPFunctionDeployment{
		Configuration: cfg.gcloudConfiguration, Name: "bgit-ci-materializer", Region: region,
		Source: sourceDir, EntryPoint: "materializer", ServiceAccount: serviceAccount,
		Environment: gcpMaterializerEnvVars(cfg, serviceAccount, ciSecret),
	})
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("deploy GCP bgit CI materializer: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	url := discoverGCPMaterializerURL(cfg, region)
	if strings.TrimSpace(url) == "" {
		return "", errors.New("discover GCP bgit CI materializer URL")
	}
	return url, nil
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
		"secretmanager.googleapis.com",
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
	for _, role := range []string{"roles/datastore.user", "roles/storage.admin", "roles/cloudbuild.builds.editor", "roles/run.invoker", "roles/iam.serviceAccountUser", "roles/logging.logWriter", "roles/logging.viewer"} {
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

func gcpBrokerEnvVars(cfg config, opts sshSetupOptions, serviceAccount, ciSecret, ciMaterializerURL, bootstrapHash string) string {
	values := []string{
		"FIRESTORE_DATABASE=" + gcpBrokerFirestoreDatabase(opts),
		"BROKER_VERSION=" + brokerVersion(),
		"BGIT_GCP_PROJECT=" + gcloudProject(cfg),
		"BGIT_SIGNING_SERVICE_ACCOUNT=" + serviceAccount,
		"BGIT_CI_MATERIALIZER_SECRET=" + ciSecret,
		"BGIT_OWNER_BOOTSTRAP_HASH=" + bootstrapHash,
	}
	if strings.TrimSpace(ciMaterializerURL) != "" {
		values = append(values, "BGIT_CI_MATERIALIZER_URL="+strings.TrimSpace(ciMaterializerURL))
	}
	return strings.Join(values, ",")
}

func gcpMaterializerEnvVars(cfg config, serviceAccount, ciSecret string) string {
	return strings.Join([]string{
		"BROKER_VERSION=" + brokerVersion(),
		"BGIT_GCP_PROJECT=" + gcloudProject(cfg),
		"BGIT_CI_MATERIALIZER_SECRET=" + ciSecret,
		"BGIT_CI_BUILD_SERVICE_ACCOUNT=" + serviceAccount,
	}, ",")
}

func ensureGCPMaterializerSecret(cfg config, serviceAccount string, stdout io.Writer) (string, error) {
	project := gcloudProject(cfg)
	if project == "" {
		return "", errors.New("GCP project is not configured")
	}
	secret := "bgit-ci-materializer-token"
	fullName := "projects/" + project + "/secrets/" + secret
	describe := gcloudCommand(cfg.gcloudConfiguration, "secrets", "describe", secret, "--project="+project, "--format=value(name)")
	if out, err := describe.Output(); err != nil || strings.TrimSpace(string(out)) == "" {
		fmt.Fprintf(stdout, "creating GCP CI materializer secret %s\n", secret)
		create := gcloudCommand(cfg.gcloudConfiguration,
			"secrets", "create", secret,
			"--replication-policy=automatic",
			"--project="+project,
			"--quiet",
		)
		if createOut, createErr := create.CombinedOutput(); createErr != nil {
			return "", fmt.Errorf("create GCP CI materializer secret: %w\n%s", createErr, strings.TrimSpace(string(createOut)))
		}
		token, err := randomBrokerSecret()
		if err != nil {
			return "", err
		}
		add := gcloudCommand(cfg.gcloudConfiguration,
			"secrets", "versions", "add", secret,
			"--data-file=-",
			"--project="+project,
			"--quiet",
		)
		add.Stdin = strings.NewReader(token)
		if addOut, addErr := add.CombinedOutput(); addErr != nil {
			return "", fmt.Errorf("seed GCP CI materializer secret: %w\n%s", addErr, strings.TrimSpace(string(addOut)))
		}
	}
	for _, role := range []string{"roles/secretmanager.secretAccessor", "roles/secretmanager.admin"} {
		fmt.Fprintf(stdout, "granting GCP broker %s on %s\n", role, secret)
		cmd := gcloudCommand(cfg.gcloudConfiguration,
			"secrets", "add-iam-policy-binding", secret,
			"--member=serviceAccount:"+serviceAccount,
			"--role="+role,
			"--project="+project,
			"--quiet",
		)
		out, err := runGcloudIAMBindingWithRetry(cmd)
		if err != nil {
			return "", fmt.Errorf("grant GCP broker %s on CI materializer secret: %w\n%s", role, err, strings.TrimSpace(string(out)))
		}
	}
	return fullName, nil
}

func randomBrokerSecret() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func brokerSecretHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum[:])
}

func discoverGCPMaterializerURL(cfg config, region string) string {
	project := gcloudProject(cfg)
	if project == "" {
		return ""
	}
	cmd := gcloudCommand(cfg.gcloudConfiguration,
		"run", "services", "describe", "bgit-ci-materializer",
		"--region", region,
		"--project", project,
		"--format=value(status.url)",
	)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func provisionAWSBrokerURL(cfg config, opts sshSetupOptions, stdout io.Writer) (string, error) {
	return provisionAWSBrokerURLWithBootstrap(cfg, opts, stdout, "")
}

func provisionAWSBrokerURLWithBootstrap(cfg config, opts sshSetupOptions, stdout io.Writer, bootstrapHash string) (string, error) {
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
	cmd := internalsetup.AWSStackDeployCommand(context.Background(), internalsetup.AWSStackDeployment{
		Profile: strings.TrimSpace(cfg.gcloudConfiguration), Region: region, StackName: "bgit-broker",
		TemplatePath: templatePath, ArtifactBucket: s3Bucket, Capability: "CAPABILITY_NAMED_IAM",
		ParameterOverrides: []string{"OwnerBootstrapHash=" + bootstrapHash},
	})
	out, err := cmd.CombinedOutput()
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

func awsCommand(ctx context.Context, profile string, args ...string) *exec.Cmd {
	return internalsetup.AWSCommand(ctx, profile, args...)
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
		keys = append(keys, splitPublicKeyLines(string(data))...)
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
