package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type brokerAuthStatusRequest struct {
	Repo brokerRepo `json:"repo"`
}

type repoAuthCache struct {
	BrokerURL      string `json:"broker_url,omitempty"`
	Repo           string `json:"repo,omitempty"`
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

type brokerAuthStatus struct {
	BrokerURL     string          `json:"broker_url,omitempty"`
	BrokerVersion string          `json:"broker_version,omitempty"`
	Repo          brokerRepo      `json:"repo,omitempty"`
	Identity      brokerIdentity  `json:"identity,omitempty"`
	User          string          `json:"user,omitempty"`
	Role          string          `json:"role,omitempty"`
	Capabilities  map[string]bool `json:"capabilities,omitempty"`
	ResolvedAt    string          `json:"resolved_at,omitempty"`
	CachedAt      string          `json:"cached_at,omitempty"`
	Stale         bool            `json:"stale,omitempty"`
	Error         string          `json:"error,omitempty"`
}

type brokerIdentity struct {
	User           string `json:"user,omitempty"`
	Source         string `json:"source,omitempty"`
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	PublicKey      string `json:"public_key,omitempty"`
}

func whoamiCommand(ctx context.Context, cfg config, args []string, stdout io.Writer) error {
	jsonOut := false
	refresh := false
	all := false
	for _, arg := range args {
		switch arg {
		case "--json":
			jsonOut = true
		case "--refresh":
			refresh = true
		case "--all":
			all = true
		default:
			return errors.New("usage: bgit whoami [--json] [--refresh] [--all]")
		}
	}
	if all {
		return whoamiAllCommand(ctx, cfg, jsonOut, stdout)
	}
	if cfg.brokerURL == "" || cfg.logicalRepo == "" {
		return errors.New("whoami requires a broker-backed BucketGit repository")
	}
	status, err := brokerWhoami(ctx, cfg, refresh)
	if err != nil {
		return err
	}
	if jsonOut {
		data, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(data))
		return nil
	}
	fmt.Fprintf(stdout, "broker: %s\n", status.BrokerURL)
	fmt.Fprintf(stdout, "repo: %s\n", firstNonEmpty(status.Repo.Logical, status.Repo.Prefix))
	fmt.Fprintf(stdout, "user: %s\n", firstNonEmpty(status.User, status.Identity.User, "unknown"))
	fmt.Fprintf(stdout, "role: %s\n", firstNonEmpty(status.Role, "none"))
	if status.Identity.KeyFingerprint != "" {
		fmt.Fprintf(stdout, "key: %s\n", status.Identity.KeyFingerprint)
		if cfg.identity != "" {
			fmt.Fprintf(stdout, "selected identity: %s\n", cfg.identity)
		}
	}
	if status.BrokerVersion != "" {
		fmt.Fprintf(stdout, "broker version: %s\n", status.BrokerVersion)
	}
	var caps []string
	for name, ok := range status.Capabilities {
		if ok {
			caps = append(caps, name)
		}
	}
	sort.Strings(caps)
	if len(caps) > 0 {
		fmt.Fprintf(stdout, "capabilities: %s\n", strings.Join(caps, ", "))
	}
	return nil
}

func whoamiAllCommand(ctx context.Context, cfg config, jsonOut bool, stdout io.Writer) error {
	if cfg.brokerURL == "" {
		return errors.New("whoami --all requires a broker-backed repository or --profile selection")
	}
	repos, err := brokerReposMineAllKeys(ctx, cfg.brokerURL)
	if err != nil {
		return err
	}
	resp := brokerReposMineResponse{Repos: repos}
	if jsonOut {
		data, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(data))
		return nil
	}
	if len(resp.Repos) == 0 {
		fmt.Fprintln(stdout, "No repositories found for the available SSH keys.")
		return nil
	}
	for _, repo := range resp.Repos {
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", repo.Logical, repo.Role, repo.User, repo.KeyFingerprint)
	}
	for _, warning := range repoMembershipWarnings(resp.Repos) {
		fmt.Fprintf(stdout, "warning: %s\n", warning)
	}
	return nil
}

func reposCommand(ctx context.Context, cfg config, args []string, stdout io.Writer) error {
	jsonOut := false
	if len(args) == 0 || args[0] != "mine" {
		return errors.New("usage: bgit repos mine [--json]")
	}
	for _, arg := range args[1:] {
		switch arg {
		case "--json":
			jsonOut = true
		default:
			return errors.New("usage: bgit repos mine [--json]")
		}
	}
	return whoamiAllCommand(ctx, cfg, jsonOut, stdout)
}

func brokerReposMineAllKeys(ctx context.Context, brokerURL string) ([]brokerRepoMembership, error) {
	data := []byte(`{}`)
	headerSets := brokerSignatureHeaderSetsForBroker(brokerURL, "/repos/mine", data)
	if len(headerSets) == 0 {
		return nil, errors.New("no SSH agent keys available")
	}
	merged := map[string]brokerRepoMembership{}
	var lastErr error
	for _, headers := range headerSets {
		var resp brokerReposMineResponse
		if err := brokerPostContextWithHeaders(ctx, brokerURL, "/repos/mine", data, headers, &resp); err != nil {
			lastErr = err
			continue
		}
		for _, repo := range resp.Repos {
			key := repo.KeyFingerprint + "\x00" + firstNonEmpty(repo.Logical, repo.RepoID, repo.Repo.Logical)
			merged[key] = repo
		}
	}
	if len(merged) == 0 && lastErr != nil {
		return nil, lastErr
	}
	var repos []brokerRepoMembership
	for _, repo := range merged {
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool {
		a := firstNonEmpty(repos[i].Logical, repos[i].RepoID)
		b := firstNonEmpty(repos[j].Logical, repos[j].RepoID)
		if a == b {
			return repos[i].KeyFingerprint < repos[j].KeyFingerprint
		}
		return a < b
	})
	return repos, nil
}

func repoMembershipWarnings(repos []brokerRepoMembership) []string {
	byRepo := map[string][]brokerRepoMembership{}
	for _, repo := range repos {
		name := firstNonEmpty(repo.Logical, repo.RepoID, repo.Repo.Logical)
		if name == "" {
			continue
		}
		byRepo[name] = append(byRepo[name], repo)
	}
	var warnings []string
	for name, memberships := range byRepo {
		if len(memberships) < 2 {
			continue
		}
		users := map[string]struct{}{}
		roles := map[string]struct{}{}
		for _, membership := range memberships {
			users[membership.User] = struct{}{}
			roles[membership.Role] = struct{}{}
		}
		if len(users) > 1 {
			warnings = append(warnings, fmt.Sprintf("%s is available through multiple SSH keys with different user labels", name))
		} else if len(roles) > 1 {
			warnings = append(warnings, fmt.Sprintf("%s is available through multiple SSH keys with different roles", name))
		}
	}
	sort.Strings(warnings)
	return warnings
}

func brokerPostContextWithHeaders(ctx context.Context, brokerURL, path string, data []byte, headers map[string]string, resp any) error {
	endpoint := strings.TrimRight(brokerURL, "/") + path
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
		return brokerHTTPError(path, msg)
	}
	if resp != nil && len(body) > 0 {
		if err := json.Unmarshal(body, resp); err != nil {
			return err
		}
	}
	return nil
}

type brokerReposMineResponse struct {
	Repos []brokerRepoMembership `json:"repos"`
}

type brokerRepoMembership struct {
	RepoID         string     `json:"repo_id,omitempty"`
	Logical        string     `json:"logical,omitempty"`
	Repo           brokerRepo `json:"repo,omitempty"`
	User           string     `json:"user,omitempty"`
	Role           string     `json:"role,omitempty"`
	Source         string     `json:"source,omitempty"`
	KeyFingerprint string     `json:"key_fingerprint,omitempty"`
	Suspended      bool       `json:"suspended,omitempty"`
	UpdatedAt      string     `json:"updated_at,omitempty"`
}

func brokerWhoami(ctx context.Context, cfg config, refresh bool) (brokerAuthStatus, error) {
	if !refresh {
		if cached, err := readWhoamiCache(cfg.brokerURL); err == nil && cached.BrokerURL != "" {
			if firstNonEmpty(cached.Repo.Logical, cached.Repo.Prefix) == firstNonEmpty(cfg.logicalRepo, cfg.prefix) {
				return cached, nil
			}
		}
	}
	var status brokerAuthStatus
	if err := brokerPostContext(ctx, cfg.brokerURL, "/auth/status", brokerAuthStatusRequest{Repo: repoForBroker(cfg)}, &status); err != nil {
		return brokerAuthStatus{}, err
	}
	status.BrokerURL = cfg.brokerURL
	if status.Repo.Logical == "" && status.Repo.Prefix == "" {
		status.Repo = repoForBroker(cfg)
	}
	if status.User == "" {
		status.User = status.Identity.User
	}
	if status.CachedAt == "" {
		status.CachedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_ = writeWhoamiCache(cfg.brokerURL, status)
	return status, nil
}

func readWhoamiCache(brokerURL string) (brokerAuthStatus, error) {
	path, err := whoamiCachePath(brokerURL)
	if err != nil {
		return brokerAuthStatus{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return brokerAuthStatus{}, err
	}
	var status brokerAuthStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return brokerAuthStatus{}, err
	}
	return status, nil
}

func writeWhoamiCache(brokerURL string, status brokerAuthStatus) error {
	path, err := whoamiCachePath(brokerURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func preferredBrokerKeyFingerprints(brokerURL string, payload []byte) []string {
	var preferred []string
	if fp := fingerprintForIdentityPreference(brokerIdentityPreference); fp != "" {
		preferred = append(preferred, fp)
	}
	if cache, err := readRepoAuthCache(brokerURL, payload); err == nil && cache.KeyFingerprint != "" {
		preferred = append(preferred, cache.KeyFingerprint)
	}
	return uniqueNonEmptyStrings(preferred)
}

func fingerprintForIdentityPreference(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "SHA256:") {
		return value
	}
	data, err := os.ReadFile(expandHome(value))
	if err != nil {
		return ""
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(pub)
}

func readRepoAuthCache(brokerURL string, payload []byte) (repoAuthCache, error) {
	path, err := repoAuthCachePath()
	if err != nil {
		return repoAuthCache{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return repoAuthCache{}, err
	}
	var cache repoAuthCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return repoAuthCache{}, err
	}
	if cache.BrokerURL != "" && brokerURL != "" && cache.BrokerURL != brokerURL {
		return repoAuthCache{}, errors.New("repo auth cache is for a different broker")
	}
	if repo := repoNameFromBrokerPayload(payload); repo != "" && cache.Repo != "" && cache.Repo != repo {
		return repoAuthCache{}, errors.New("repo auth cache is for a different repo")
	}
	return cache, nil
}

func writeRepoAuthCache(brokerURL string, payload []byte, fingerprint string) error {
	if strings.TrimSpace(fingerprint) == "" {
		return nil
	}
	path, err := repoAuthCachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	cache := repoAuthCache{
		BrokerURL:      brokerURL,
		Repo:           repoNameFromBrokerPayload(payload),
		KeyFingerprint: fingerprint,
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func repoAuthCachePath() (string, error) {
	out, err := runGit(".", "rev-parse", "--git-path", "bgit/cache/auth.json")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func repoNameFromBrokerPayload(payload []byte) string {
	var raw struct {
		Repo brokerRepo `json:"repo"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return ""
	}
	return firstNonEmpty(raw.Repo.Logical, raw.Repo.Prefix, raw.Repo.Origin)
}

func whoamiCachePath(brokerURL string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bgit", "cache", safeBrokerCacheName(brokerURL), "whoami.json"), nil
}

func safeBrokerCacheName(brokerURL string) string {
	value := strings.TrimSpace(brokerURL)
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		value = parsed.Host
	}
	value = strings.ToLower(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}
