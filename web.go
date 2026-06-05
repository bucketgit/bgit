package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

//go:embed www/*
var webAssets embed.FS

const (
	defaultWebAddr      = "127.0.0.1"
	defaultWebPort      = 8042
	webPageTemplatePath = "www/page.html"
	webCSSPath          = "www/app.css"
	webJSPath           = "www/app.js"
	webLogoPath         = "www/bgit-mark.png"
	webFaviconPath      = "www/favicon.ico"
)

type webOptions struct {
	addr  string
	port  int
	local bool
}

type webServer struct {
	repo        *nativeGitRepo
	apiRepo     *nativeGitRepo
	cfg         config
	title       string
	events      *webEventHub
	localGitDir string
	csrfToken   string
}

type brokerGitStore struct {
	brokerURL string
	cfg       config
}

type brokerObjectRequest struct {
	Repo   brokerRepo `json:"repo"`
	Path   string     `json:"path,omitempty"`
	Prefix string     `json:"prefix,omitempty"`
}

type brokerObjectResponse struct {
	Data  string   `json:"data,omitempty"`
	Paths []string `json:"paths,omitempty"`
}

type webTreeFile struct {
	path string
	hash string
}

type webFileIndexEntry struct {
	Path string `json:"path"`
	URL  string `json:"url"`
	Kind string `json:"kind"`
}

type webChangedFile struct {
	path      string
	oldHash   string
	newHash   string
	additions int
	deletions int
	diff      []webDiffLine
	visual    []webVisualDiffRow
	binary    bool
}

type webDiffRenderOptions struct {
	Review bool
	PRID   int
}

type webRefOption struct {
	name     string
	fullName string
	kind     string
}

type webDiffLine struct {
	kind string
	text string
}

type webAPIRef struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Kind     string `json:"kind"`
}

type webAPICommit struct {
	Hash      string   `json:"hash"`
	ShortHash string   `json:"short_hash"`
	Subject   string   `json:"subject"`
	Body      string   `json:"body,omitempty"`
	Author    string   `json:"author"`
	Email     string   `json:"email"`
	Timestamp int64    `json:"timestamp"`
	Parents   []string `json:"parents,omitempty"`
	Tree      string   `json:"tree,omitempty"`
}

type webAPITreeEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
	Hash string `json:"hash"`
	URL  string `json:"url"`
}

type webAPIState struct {
	Branch          string         `json:"branch"`
	LocalHead       string         `json:"local_head,omitempty"`
	RemoteHead      string         `json:"remote_head,omitempty"`
	Ahead           int            `json:"ahead"`
	Behind          int            `json:"behind"`
	Dirty           bool           `json:"dirty"`
	DirtyFiles      []string       `json:"dirty_files"`
	StagedFiles     []string       `json:"staged_files"`
	UnstagedFiles   []string       `json:"unstaged_files"`
	UntrackedFiles  []string       `json:"untracked_files"`
	UnpushedFiles   []string       `json:"unpushed_files"`
	UnpulledFiles   []string       `json:"unpulled_files"`
	UnpushedCommits []webAPICommit `json:"unpushed_commits"`
	UnpulledCommits []webAPICommit `json:"unpulled_commits"`
	FetchError      string         `json:"fetch_error,omitempty"`
}

type webPullRequestCache struct {
	UpdatedAt int64               `json:"updated_at"`
	PRs       []brokerPullRequest `json:"prs"`
}

type brokerIssue struct {
	ID        int                `json:"id,omitempty"`
	Type      string             `json:"type,omitempty"`
	Title     string             `json:"title,omitempty"`
	Body      string             `json:"body,omitempty"`
	Status    string             `json:"status,omitempty"`
	Lane      string             `json:"lane,omitempty"`
	Assignee  string             `json:"assignee,omitempty"`
	Position  float64            `json:"position,omitempty"`
	Archived  bool               `json:"archived,omitempty"`
	Author    string             `json:"author,omitempty"`
	CreatedAt string             `json:"created_at,omitempty"`
	UpdatedAt string             `json:"updated_at,omitempty"`
	Comments  []brokerIssueReply `json:"comments,omitempty"`
	History   []brokerIssueEvent `json:"history,omitempty"`
}

type brokerIssueReply struct {
	User string `json:"user,omitempty"`
	Body string `json:"body,omitempty"`
	At   string `json:"at,omitempty"`
}

type brokerIssueEvent struct {
	User     string `json:"user,omitempty"`
	Action   string `json:"action,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	At       string `json:"at,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Position string `json:"position,omitempty"`
}

type brokerIssueRequest struct {
	Repo            brokerRepo `json:"repo"`
	ID              int        `json:"id,omitempty"`
	Type            string     `json:"type,omitempty"`
	Title           string     `json:"title,omitempty"`
	Body            string     `json:"body,omitempty"`
	Lane            string     `json:"lane,omitempty"`
	Assignee        string     `json:"assignee,omitempty"`
	Comment         string     `json:"comment,omitempty"`
	AfterID         *int       `json:"after_id,omitempty"`
	Order           int        `json:"order,omitempty"`
	Archived        bool       `json:"archived,omitempty"`
	IncludeArchived bool       `json:"include_archived,omitempty"`
}

type boardRenderContext struct {
	Assignees []string
	Monogram  string
}

func webCommand(ctx context.Context, cfg config, args []string, stdout io.Writer) error {
	opts, err := parseWebArgs(args)
	if err != nil {
		return err
	}
	repo, apiRepo, closeStore, cfg, err := openWebRepository(ctx, cfg, opts.local)
	if err != nil {
		return err
	}
	defer closeStore()

	handler := newWebHandlerWithAPI(repo, apiRepo, cfg)
	liveCtx, cancelLive := context.WithCancel(ctx)
	defer cancelLive()
	handler.startLiveMonitors(liveCtx)
	ln, err := listenWeb(opts.addr, opts.port)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "serving %s at http://%s/\n", webRepoTitle(cfg), ln.Addr().String())
	return http.Serve(ln, handler)
}

func listenWeb(addr string, startPort int) (net.Listener, error) {
	var lastErr error
	for offset := 0; offset <= 100; offset++ {
		port := startPort + offset
		if port > 65535 {
			break
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
		if err == nil {
			return ln, nil
		}
		lastErr = err
		if !isAddrInUse(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("listen tcp %s:%d-%d: no available port: %w", addr, startPort, min(startPort+100, 65535), lastErr)
}

func isAddrInUse(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		err = opErr.Err
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "only one usage of each socket address") ||
		strings.Contains(msg, "address is already in use")
}

func parseWebArgs(args []string) (webOptions, error) {
	opts := webOptions{addr: defaultWebAddr, port: defaultWebPort}
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.addr, "addr", defaultWebAddr, "address to bind")
	fs.IntVar(&opts.port, "port", defaultWebPort, "port to bind")
	fs.BoolVar(&opts.local, "local", false, "serve the local checkout instead of the configured remote")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, errors.New("web does not accept positional arguments")
	}
	if opts.addr == "" {
		return opts, errors.New("web --addr cannot be empty")
	}
	if opts.port < 1 || opts.port > 65535 {
		return opts, errors.New("web --port must be between 1 and 65535")
	}
	return opts, nil
}

func openWebRepository(ctx context.Context, cfg config, local bool) (*nativeGitRepo, *nativeGitRepo, func(), config, error) {
	if local {
		localRepo, err := openLocalRepository(".")
		if err != nil {
			return nil, nil, nil, cfg, err
		}
		if localCfg, err := readLocalConfig(localRepo.worktree); err == nil {
			cfg = mergeConfig(cfg, localCfg)
		}
		if branch := localRepo.currentBranch(); branch != "" {
			cfg.branch = branch
		}
		repo := newNativeGitRepoForStore(cfg, localRepo.store)
		return repo, repo, func() {}, cfg, nil
	}

	var seedRepo *nativeGitRepo
	if localRepo, err := openLocalRepository("."); err == nil {
		if localCfg, err := readLocalConfig(localRepo.worktree); err == nil {
			cfg = mergeConfig(cfg, localCfg)
		}
		if branch := localRepo.currentBranch(); branch != "" {
			cfg.branch = branch
		}
		seedRepo = newNativeGitRepoForStore(cfg, localRepo.store)
	}
	if cfg.bucket == "" && cfg.brokerURL == "" {
		localCfg, err := readLocalConfig(".")
		if err == nil {
			cfg = mergeConfig(cfg, localCfg)
		}
	}
	if cfg.bucket == "" && cfg.brokerURL == "" {
		return nil, nil, nil, cfg, errors.New("--bucket is required outside a bucketgit checkout")
	}
	localBroker, err := ensureLocalBrokerForCommand(ctx, &cfg)
	if err != nil {
		return nil, nil, nil, cfg, err
	}
	store, closeStore, err := newRemoteStore(ctx, cfg, true)
	if err != nil {
		if localBroker != nil {
			localBroker.Close()
		}
		return nil, nil, nil, cfg, fmt.Errorf("create remote store: %w", err)
	}
	if localBroker != nil {
		originalClose := closeStore
		closeStore = func() {
			originalClose()
			localBroker.Close()
		}
	}
	remoteRepo := openNativeGitRepo(store, cfg)
	if seedRepo == nil {
		seedRepo = remoteRepo
	}
	return seedRepo, remoteRepo, closeStore, cfg, nil
}

func (s *brokerGitStore) read(ctx context.Context, objectPath string) ([]byte, error) {
	if data, ok, err := s.readWithCapability(ctx, objectPath); ok || err != nil {
		return data, err
	}
	var resp brokerObjectResponse
	err := brokerPostContext(ctx, s.brokerURL, "/objects/read", brokerObjectRequest{
		Repo: repoForBroker(s.cfg),
		Path: strings.TrimPrefix(objectPath, "/"),
	}, &resp)
	if err != nil {
		if isBrokerNotFoundError(err) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(resp.Data)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *brokerGitStore) list(ctx context.Context, prefix string) ([]string, error) {
	var resp brokerObjectResponse
	err := brokerPostContext(ctx, s.brokerURL, "/objects/list", brokerObjectRequest{
		Repo:   repoForBroker(s.cfg),
		Prefix: strings.TrimPrefix(prefix, "/"),
	}, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Paths, nil
}

func isBrokerNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "No such object:") ||
		strings.Contains(message, "NoSuchKey") ||
		strings.Contains(message, "specified key does not exist") ||
		strings.Contains(message, "NotFound") ||
		strings.Contains(message, "not found")
}

func newWebHandler(repo *nativeGitRepo, cfg config) *webServer {
	return newWebHandlerWithAPI(repo, repo, cfg)
}

func newWebHandlerWithAPI(repo, apiRepo *nativeGitRepo, cfg config) *webServer {
	if apiRepo == nil {
		apiRepo = repo
	}
	localGitDir := ""
	if repo != nil {
		if store, ok := repo.store.(*localGitStore); ok {
			localGitDir = store.root
		}
	}
	csrfToken, err := randomWebCSRFToken()
	if err != nil {
		csrfToken = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return &webServer{repo: repo, apiRepo: apiRepo, cfg: cfg, title: webRepoTitle(cfg), events: newWebEventHub(), localGitDir: localGitDir, csrfToken: csrfToken}
}

func webRepoTitle(cfg config) string {
	logicalRepo := strings.Trim(cfg.logicalRepo, "/")
	if logicalRepo != "" {
		return logicalRepo
	}
	if cfg.origin != "" {
		return cfg.origin
	}
	if cfg.bucket != "" {
		return strings.Trim(strings.Join([]string{cfg.provider, cfg.bucket, cfg.prefix}, " "), " ")
	}
	return "local repository"
}

func (s *webServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	route := strings.TrimPrefix(r.URL.Path, "/")
	srv := s.serverForRequest(r, strings.HasPrefix(route, "api/"))
	if s.isMutationRoute(route, r.Method) {
		if err := s.validateWebMutation(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	switch {
	case r.Method != http.MethodGet && r.Method != http.MethodHead && !strings.HasPrefix(route, "api/actions/") && route != "api/user/profile":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case route == "assets/bgit-mark.png":
		s.handleWebAsset(w, webLogoPath)
	case route == "favicon.ico":
		s.handleWebAsset(w, webFaviconPath)
	case route == "events":
		s.handleEvents(w, r)
	case route == "api/state":
		s.handleAPIState(ctx, w, r)
	case route == "api/me":
		s.handleAPIMe(ctx, w, r)
	case route == "api/actions/commit":
		s.handleAPIActionCommit(ctx, w, r)
	case route == "api/actions/stage":
		s.handleAPIActionStage(ctx, w, r)
	case route == "api/actions/unstage":
		s.handleAPIActionUnstage(ctx, w, r)
	case route == "api/actions/discard":
		s.handleAPIActionDiscard(ctx, w, r)
	case route == "api/actions/uncommit":
		s.handleAPIActionUncommit(ctx, w, r)
	case route == "api/actions/push":
		s.handleAPIActionPush(ctx, w, r)
	case route == "api/actions/pull":
		s.handleAPIActionPull(ctx, w, r)
	case route == "api/actions/pr":
		s.handleAPIActionPullRequest(ctx, w, r)
	case route == "api/actions/issues":
		s.handleAPIActionIssue(ctx, w, r)
	case route == "api/actions/board":
		s.handleAPIActionBoard(ctx, w, r)
	case route == "api/actions/ci":
		s.handleAPIActionCI(ctx, w, r)
	case route == "api/actions/settings":
		s.handleAPIActionSettings(ctx, w, r)
	case route == "api/user/profile":
		s.handleAPIUserProfile(ctx, w, r)
	case route == "api/diff":
		s.handleAPIDiff(ctx, w, r)
	case route == "api/refs":
		srv.handleAPIRefs(ctx, w, r)
	case route == "api/tree":
		srv.handleAPITree(ctx, w, r)
	case route == "api/commits":
		srv.handleAPICommits(ctx, w, r)
	case route == "api/prs":
		s.handleAPIPullRequests(ctx, w, r)
	case route == "api/issues":
		s.handleAPIIssues(ctx, w, r)
	case route == "api/settings":
		s.handleAPISettings(ctx, w, r)
	case route == "api/settings-fragment":
		s.handleAPISettingsFragment(ctx, w, r)
	case route == "api/blob":
		srv.handleAPIBlob(ctx, w, r)
	case strings.HasPrefix(route, "api/commit/"):
		srv.handleAPICommit(ctx, w, r, strings.TrimPrefix(route, "api/commit/"))
	case r.URL.Path == "/":
		srv.handleTree(ctx, w, r, "")
	case route == "commits":
		srv.handleCommits(ctx, w, r)
	case route == "prs":
		s.handlePullRequests(ctx, w, r)
	case route == "prs/new":
		s.handleNewPullRequest(ctx, w, r)
	case strings.HasPrefix(route, "prs/"):
		s.handlePullRequest(ctx, w, r, strings.TrimPrefix(route, "prs/"))
	case route == "issues":
		s.handleIssues(ctx, w, r)
	case strings.HasPrefix(route, "issues/"):
		s.handleIssue(ctx, w, r, strings.TrimPrefix(route, "issues/"))
	case route == "board":
		s.handleBoard(ctx, w, r)
	case strings.HasPrefix(route, "board/"):
		s.handleStory(ctx, w, r, strings.TrimPrefix(route, "board/"))
	case route == "ci":
		s.handleCI(ctx, w, r)
	case route == "settings":
		s.handleSettings(ctx, w, r)
	case route == "user/settings" || route == "user/settings/":
		s.handleUserSettings(ctx, w, r)
	case route == "admin":
		http.Redirect(w, r, "/settings", http.StatusFound)
	case route == "archive.zip":
		srv.handleArchiveZip(ctx, w, r)
	case strings.HasPrefix(route, "commit/"):
		srv.handleCommit(ctx, w, r, strings.TrimPrefix(route, "commit/"))
	case strings.HasPrefix(route, "tree/"):
		srv.handleTree(ctx, w, r, strings.TrimPrefix(route, "tree/"))
	case strings.HasPrefix(route, "blob/"):
		srv.handleBlob(ctx, w, r, strings.TrimPrefix(route, "blob/"), false)
	case strings.HasPrefix(route, "raw/"):
		srv.handleBlob(ctx, w, r, strings.TrimPrefix(route, "raw/"), true)
	default:
		http.NotFound(w, r)
	}
}

func randomWebCSRFToken() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func (s *webServer) isMutationRoute(route, method string) bool {
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
		return false
	}
	return strings.HasPrefix(route, "api/actions/") || route == "api/user/profile"
}

func (s *webServer) validateWebMutation(r *http.Request) error {
	if r.Header.Get("X-Bgit-CSRF") != s.csrfToken {
		return errors.New("invalid CSRF token")
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return errors.New("invalid origin")
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = u.Hostname()
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if u.Scheme != "http" || (host != "localhost" && host != "127.0.0.1" && host != "::1") {
		return errors.New("foreign origin rejected")
	}
	return nil
}

func (s *webServer) handleWebAsset(w http.ResponseWriter, path string) {
	data, err := webAssetBytes(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if typ := mime.TypeByExtension(filepath.Ext(path)); typ != "" {
		w.Header().Set("Content-Type", typ)
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *webServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	client := s.events.subscribe()
	defer s.events.unsubscribe(client)
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-client:
			fmt.Fprint(w, event)
			flusher.Flush()
		}
	}
}

func (s *webServer) startLiveMonitors(ctx context.Context) {
	if s.events == nil {
		return
	}
	if s.cfg.brokerURL != "" && s.cfg.logicalRepo != "" {
		go s.refreshWhoamiForWeb(ctx)
	}
	if repo, err := openLocalRepository("."); err == nil {
		go monitorWebPath(ctx, repo.gitDir, "git", s.events)
	}
	if dir := webAssetDir(); dir != "" {
		go monitorWebPath(ctx, dir, "assets", s.events)
	}
}

func (s *webServer) handleAPIMe(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	status, err := s.webWhoami(ctx, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	s.renderJSON(w, status)
}

func (s *webServer) cachedWhoamiJSON() string {
	if s.cfg.brokerURL == "" {
		return "null"
	}
	status, err := readWhoamiCache(s.cfg.brokerURL)
	if err != nil {
		return "null"
	}
	data, err := json.Marshal(status)
	if err != nil {
		return "null"
	}
	return string(data)
}

func (s *webServer) webWhoami(ctx context.Context, refresh bool) (brokerAuthStatus, error) {
	if s.cfg.brokerURL == "" || s.cfg.logicalRepo == "" {
		return brokerAuthStatus{}, errors.New("whoami requires a broker-backed repository")
	}
	return brokerWhoami(ctx, s.cfg, refresh)
}

func (s *webServer) refreshWhoamiForWeb(ctx context.Context) {
	status, err := s.webWhoami(ctx, true)
	if err != nil {
		return
	}
	if s.events != nil {
		s.events.broadcastJSON("whoami", status)
	}
}

func (s *webServer) serverForRequest(r *http.Request, api bool) *webServer {
	if s.apiRepo == nil || s.apiRepo == s.repo {
		return s
	}
	if api || r.URL.Query().Get("_remote") == "1" {
		next := *s
		next.repo = s.apiRepo
		return &next
	}
	return s
}

func (s *webServer) headCommit(ctx context.Context, r *http.Request) (string, commitObject, string, error) {
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	}
	hash, err := s.repo.resolveRevision(ctx, ref)
	if err != nil {
		return "", commitObject{}, ref, err
	}
	commit, err := s.repo.commit(ctx, hash)
	if err != nil {
		return "", commitObject{}, ref, err
	}
	return hash, commit, ref, nil
}

func (s *webServer) handleAPIRefs(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	options, err := s.refOptions(ctx)
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	refs := make([]webAPIRef, 0, len(options))
	for _, option := range options {
		refs = append(refs, webAPIRef{Name: option.name, FullName: option.fullName, Kind: option.kind})
	}
	s.renderJSON(w, map[string]any{
		"refs":        refs,
		"default_ref": branchRef(firstNonEmpty(s.cfg.branch, defaultBranch)),
	})
}

func (s *webServer) handleAPITree(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	_, commit, ref, err := s.headCommit(ctx, r)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.renderJSON(w, map[string]any{
				"ref":            ref,
				"path":           cleanWebPath(r.URL.Query().Get("path")),
				"entries":        []webAPITreeEntry{},
				"recent_commits": []webAPICommit{},
				"empty":          true,
			})
			return
		}
		s.renderJSONError(w, http.StatusNotFound, err)
		return
	}
	repoPath := cleanWebPath(r.URL.Query().Get("path"))
	treeHash := commit.tree
	if repoPath != "" && repoPath != "commits" && repoPath != "prs" {
		hash, err := s.repo.findPath(ctx, commit.tree, repoPath)
		if err != nil {
			s.renderJSONError(w, http.StatusNotFound, err)
			return
		}
		obj, err := s.repo.object(ctx, hash)
		if err != nil {
			s.renderJSONError(w, http.StatusInternalServerError, err)
			return
		}
		if obj.typ == gitObjectBlob {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("path is a blob"))
			return
		}
		treeHash = hash
	}
	entries, err := s.repo.treeEntries(ctx, treeHash)
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].typ != entries[j].typ {
			return entries[i].typ == gitObjectTree
		}
		return entries[i].name < entries[j].name
	})
	apiEntries := make([]webAPITreeEntry, 0, len(entries))
	for _, entry := range entries {
		kind := "file"
		route := "blob"
		if entry.typ == gitObjectTree {
			kind = "dir"
			route = "tree"
		}
		targetPath := pathpkg.Join(repoPath, entry.name)
		apiEntries = append(apiEntries, webAPITreeEntry{
			Name: entry.name,
			Path: targetPath,
			Kind: kind,
			Hash: entry.hash,
			URL:  webURL(route, targetPath, ref),
		})
	}
	commits, _ := s.repo.walkCommits(ctx, commit.hash, 10, 0, repoPath)
	s.renderJSON(w, map[string]any{
		"ref":            ref,
		"path":           repoPath,
		"commit":         webAPICommitFromCommit(commit),
		"entries":        apiEntries,
		"recent_commits": webAPICommitsFromCommits(commits),
	})
}

func (s *webServer) handleAPICommits(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	_, commit, ref, err := s.headCommit(ctx, r)
	if err != nil {
		s.renderJSONError(w, http.StatusNotFound, err)
		return
	}
	repoPath := cleanWebPath(r.URL.Query().Get("path"))
	commits, err := s.repo.walkCommits(ctx, commit.hash, 100, 0, repoPath)
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	s.renderJSON(w, map[string]any{"ref": ref, "path": repoPath, "head": webAPICommitFromCommit(commit), "commits": webAPICommitsFromCommits(commits)})
}

func (s *webServer) handleAPIPullRequests(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	prs := []brokerPullRequest{}
	source := "cache"
	stale := false
	if !refresh {
		if cached, err := s.readPullRequestCache(); err == nil {
			s.renderJSON(w, map[string]any{
				"prs":    webAPIPullRequests(cached.PRs),
				"source": source,
				"stale":  true,
			})
			return
		}
	}
	refreshed, err := s.refreshPullRequestCache(ctx)
	if err != nil {
		cached, cacheErr := s.readPullRequestCache()
		if cacheErr != nil {
			s.renderJSONError(w, http.StatusForbidden, err)
			return
		}
		prs = cached.PRs
		stale = true
	} else {
		prs = refreshed
		source = "broker"
	}
	s.renderJSON(w, map[string]any{
		"prs":    webAPIPullRequests(prs),
		"source": source,
		"stale":  stale,
	})
}

type webSettingsInfo struct {
	Repo          brokerRepo                `json:"repo"`
	Title         string                    `json:"title"`
	BrokerURL     string                    `json:"broker_url,omitempty"`
	Provider      string                    `json:"provider,omitempty"`
	Region        string                    `json:"region,omitempty"`
	Description   string                    `json:"description,omitempty"`
	DefaultBranch string                    `json:"default_branch,omitempty"`
	Visibility    string                    `json:"visibility,omitempty"`
	ReadOnly      bool                      `json:"read_only,omitempty"`
	IssuesEnabled bool                      `json:"issues_enabled"`
	Keys          []brokerKey               `json:"keys,omitempty"`
	UserGrants    []brokerRepoUserGrant     `json:"user_grants,omitempty"`
	Protections   []brokerProtectionRequest `json:"protections,omitempty"`
	Errors        map[string]string         `json:"errors,omitempty"`
}

type webSettingsCache struct {
	UpdatedAt int64           `json:"updated_at"`
	Info      webSettingsInfo `json:"info"`
}

type brokerRepoTeamGrant struct {
	ID     string `json:"id,omitempty"`
	TeamID string `json:"team_id,omitempty"`
	Role   string `json:"role,omitempty"`
}

type brokerRepoInfoRequest struct {
	Repo          brokerRepo `json:"repo"`
	Description   string     `json:"description,omitempty"`
	DefaultBranch string     `json:"default_branch,omitempty"`
	Visibility    string     `json:"visibility,omitempty"`
	ReadOnly      bool       `json:"read_only,omitempty"`
	IssuesEnabled bool       `json:"issues_enabled"`
	Logical       string     `json:"logical,omitempty"`
	TeamID        string     `json:"team_id,omitempty"`
	Name          string     `json:"name,omitempty"`
	UserID        string     `json:"user_id,omitempty"`
	User          string     `json:"user,omitempty"`
	Role          string     `json:"role,omitempty"`
	BrokerRole    string     `json:"broker_role,omitempty"`
	PublicKeys    []string   `json:"public_keys,omitempty"`
}

type brokerRepoInfoResponse struct {
	Repo          brokerRepo `json:"repo"`
	Description   string     `json:"description"`
	DefaultBranch string     `json:"default_branch"`
	Visibility    string     `json:"visibility"`
	ReadOnly      bool       `json:"read_only"`
	IssuesEnabled bool       `json:"issues_enabled"`
}

type brokerUserProfileRequest struct {
	Repo   brokerRepo `json:"repo"`
	Bio    string     `json:"bio,omitempty"`
	Avatar string     `json:"avatar,omitempty"`
}

type brokerUserProfileResponse struct {
	User    string                 `json:"user"`
	Profile brokerUserProfileData  `json:"profile"`
	Keys    []brokerUserProfileKey `json:"keys"`
}

type brokerUserProfileData struct {
	Bio    string `json:"bio,omitempty"`
	Avatar string `json:"avatar,omitempty"`
}

type brokerUserProfileKey struct {
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Source      string `json:"source,omitempty"`
}

func (s *webServer) handleAPISettings(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	info, _, err := s.cachedSettingsInfo(ctx, refresh)
	if err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	s.renderJSON(w, info)
}

func (s *webServer) handleAPISettingsFragment(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	view := strings.TrimSpace(r.URL.Query().Get("view"))
	if view == "" {
		view = "settings"
	}
	info, _, err := s.cachedSettingsInfo(ctx, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	s.renderJSON(w, map[string]any{"html": s.settingsSectionsHTML(info, view), "settings": info})
}

func (s *webServer) handleAPIIssues(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	issues, err := s.listIssues(ctx)
	if err != nil {
		s.renderJSONError(w, http.StatusForbidden, err)
		return
	}
	s.renderJSON(w, map[string]any{"issues": issues})
}

func (s *webServer) listIssues(ctx context.Context) ([]brokerIssue, error) {
	return s.listIssuesWithOptions(ctx, brokerIssueRequest{Repo: repoForBroker(s.cfg)})
}

func (s *webServer) listIssuesWithOptions(ctx context.Context, req brokerIssueRequest) ([]brokerIssue, error) {
	if strings.TrimSpace(s.cfg.brokerURL) == "" || strings.TrimSpace(s.cfg.logicalRepo) == "" {
		return nil, errors.New("broker issues unavailable")
	}
	if strings.TrimSpace(req.Repo.Logical) == "" {
		req.Repo = repoForBroker(s.cfg)
	}
	var resp struct {
		Issues []brokerIssue `json:"issues"`
	}
	if err := brokerPostContext(ctx, s.cfg.brokerURL, "/issues/list", req, &resp); err != nil {
		return nil, err
	}
	return resp.Issues, nil
}

func (s *webServer) getIssue(ctx context.Context, id int) (brokerIssue, error) {
	var resp struct {
		Issue brokerIssue `json:"issue"`
	}
	if err := brokerPostContext(ctx, s.cfg.brokerURL, "/issues/view", brokerIssueRequest{Repo: repoForBroker(s.cfg), ID: id}, &resp); err != nil {
		return brokerIssue{}, err
	}
	return resp.Issue, nil
}

func (s *webServer) settingsInfo(ctx context.Context) webSettingsInfo {
	info := webSettingsInfo{
		Repo:          repoForBroker(s.cfg),
		Title:         s.title,
		BrokerURL:     s.cfg.brokerURL,
		Provider:      s.cfg.provider,
		Region:        firstNonEmpty(s.cfg.region, globalConfigRegionForBrokerURL(s.cfg.brokerURL)),
		DefaultBranch: defaultBranch,
		Visibility:    "private",
		IssuesEnabled: true,
		Errors:        map[string]string{},
	}
	if strings.TrimSpace(s.cfg.brokerURL) == "" || strings.TrimSpace(s.cfg.logicalRepo) == "" {
		return info
	}
	if s.cfg.provider == "local" {
		info.Errors = nil
		return info
	}
	var repoInfo brokerRepoInfoResponse
	if err := brokerPostContext(ctx, s.cfg.brokerURL, "/repo/info", brokerRepoInfoRequest{Repo: repoForBroker(s.cfg)}, &repoInfo); err != nil {
		info.Errors["repo"] = err.Error()
	} else {
		if repoInfo.Repo.Logical != "" || repoInfo.Repo.Bucket != "" {
			info.Repo = repoInfo.Repo
		}
		info.Description = repoInfo.Description
		info.DefaultBranch = firstNonEmpty(repoInfo.DefaultBranch, defaultBranch)
		info.Visibility = firstNonEmpty(repoInfo.Visibility, "private")
		info.ReadOnly = repoInfo.ReadOnly
		info.IssuesEnabled = repoInfo.IssuesEnabled
	}
	if keys, err := brokerListKeys(s.cfg.brokerURL, s.cfg); err != nil {
		info.Errors["members"] = err.Error()
	} else {
		info.Keys = keys
	}
	var repoUsers brokerRepoUsersResponse
	if err := brokerPostContext(ctx, s.cfg.brokerURL, "/repo/users/list", brokerRepoAdminRequest{Repo: repoForBroker(s.cfg)}, &repoUsers); err != nil {
		info.Errors["repo users"] = err.Error()
	} else {
		info.UserGrants = repoUsers.Users
	}
	var protections struct {
		Protections []brokerProtectionRequest `json:"protections"`
	}
	if err := brokerPostContext(ctx, s.cfg.brokerURL, "/protection/list", brokerProtectionRequest{Repo: repoForBroker(s.cfg)}, &protections); err != nil {
		info.Errors["protections"] = err.Error()
	} else {
		info.Protections = protections.Protections
	}
	if len(info.Errors) == 0 {
		info.Errors = nil
	}
	return info
}

func (s *webServer) settingsSeedInfo() webSettingsInfo {
	return webSettingsInfo{
		Repo:          repoForBroker(s.cfg),
		Title:         s.title,
		BrokerURL:     s.cfg.brokerURL,
		Provider:      s.cfg.provider,
		Region:        firstNonEmpty(s.cfg.region, globalConfigRegionForBrokerURL(s.cfg.brokerURL)),
		DefaultBranch: defaultBranch,
		Visibility:    "private",
		IssuesEnabled: true,
	}
}

func (s *webServer) settingsCachePath() string {
	if s.localGitDir == "" {
		return ""
	}
	return filepath.Join(s.localGitDir, "bucketgit", "cache", "settings.json")
}

func (s *webServer) readSettingsCache() (webSettingsInfo, error) {
	path := s.settingsCachePath()
	if path == "" {
		return webSettingsInfo{}, fs.ErrNotExist
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return webSettingsInfo{}, err
	}
	var cache webSettingsCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return webSettingsInfo{}, err
	}
	return cache.Info, nil
}

func (s *webServer) writeSettingsCache(info webSettingsInfo) error {
	path := s.settingsCachePath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(webSettingsCache{UpdatedAt: time.Now().Unix(), Info: info}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	return os.WriteFile(path, data, 0o644)
}

func (s *webServer) cachedSettingsInfo(ctx context.Context, refresh bool) (webSettingsInfo, bool, error) {
	if !refresh {
		if info, err := s.readSettingsCache(); err == nil {
			return info, true, nil
		}
		return s.settingsSeedInfo(), false, nil
	}
	info := s.settingsInfo(ctx)
	_ = s.writeSettingsCache(info)
	return info, false, nil
}

func globalConfigRegionForBrokerURL(brokerURL string) string {
	want := normalizeBrokerURLForCompare(brokerURL)
	if want == "" {
		return ""
	}
	path, err := defaultGlobalConfigPath()
	if err != nil {
		return ""
	}
	global, err := readGlobalConfig(path)
	if err != nil {
		return ""
	}
	for _, profile := range global.GCPProfiles {
		for _, region := range profile.Regions {
			if normalizeBrokerURLForCompare(region.BrokerURL) == want {
				return region.Name
			}
		}
	}
	for _, profile := range global.AWSProfiles {
		for _, region := range profile.Regions {
			if normalizeBrokerURLForCompare(region.BrokerURL) == want {
				return region.Name
			}
		}
	}
	return ""
}

func normalizeBrokerURLForCompare(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

func (s *webServer) handleAPICommit(ctx context.Context, w http.ResponseWriter, r *http.Request, hash string) {
	hash = strings.TrimSpace(strings.Trim(hash, "/"))
	if hash == "" {
		s.renderJSONError(w, http.StatusNotFound, fs.ErrNotExist)
		return
	}
	commitHash, err := s.repo.resolveRevision(ctx, hash)
	if err != nil {
		commitHash = hash
	}
	commit, err := s.repo.commit(ctx, commitHash)
	if err != nil {
		s.renderJSONError(w, http.StatusNotFound, err)
		return
	}
	files, additions, deletions, err := s.changedFiles(ctx, commit)
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	s.renderJSON(w, map[string]any{
		"commit":    webAPICommitFromCommit(commit),
		"files":     webAPIChangedFiles(files),
		"additions": additions,
		"deletions": deletions,
	})
}

func (s *webServer) handleAPIBlob(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	_, commit, ref, err := s.headCommit(ctx, r)
	if err != nil {
		s.renderJSONError(w, http.StatusNotFound, err)
		return
	}
	repoPath := cleanWebPath(r.URL.Query().Get("path"))
	if repoPath == "" {
		s.renderJSONError(w, http.StatusNotFound, fs.ErrNotExist)
		return
	}
	hash, err := s.repo.findPath(ctx, commit.tree, repoPath)
	if err != nil {
		s.renderJSONError(w, http.StatusNotFound, err)
		return
	}
	obj, err := s.repo.object(ctx, hash)
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if obj.typ == gitObjectTree {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("path is a tree"))
		return
	}
	content := ""
	encoding := "base64"
	if isTextBlob(obj.data) {
		content = string(obj.data)
		encoding = "utf-8"
	} else {
		content = base64.StdEncoding.EncodeToString(obj.data)
	}
	s.renderJSON(w, map[string]any{
		"ref":      ref,
		"path":     repoPath,
		"commit":   webAPICommitFromCommit(commit),
		"hash":     hash,
		"size":     len(obj.data),
		"text":     isTextBlob(obj.data),
		"encoding": encoding,
		"content":  content,
		"raw_url":  webURL("raw", repoPath, ref),
	})
}

func (s *webServer) handleAPIState(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	state, err := s.webRepositoryState(ctx, true, r.URL.Query().Get("ref"))
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	s.renderJSON(w, state)
}

func (s *webServer) handleAPIDiff(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if commit := strings.TrimSpace(r.URL.Query().Get("commit")); commit != "" {
		files, additions, deletions, err := s.commitChangedFiles(ctx, commit)
		if err == nil {
			s.renderJSON(w, map[string]any{"commit": commit, "diff": changedFilesUnifiedDiff(files), "html": diffFilesPanelHTML(files, additions, deletions)})
			return
		}
		diffHTML, htmlErr := localCommitVisualDiffHTML(commit)
		diff, diffErr := localCommitDiff(commit)
		if diffErr != nil && htmlErr != nil {
			s.renderJSONError(w, http.StatusBadRequest, err)
			return
		}
		resp := map[string]any{"commit": commit, "diff": diff}
		if htmlErr == nil {
			resp["html"] = diffHTML
		}
		s.renderJSON(w, resp)
		return
	}
	if prID := strings.TrimSpace(r.URL.Query().Get("pr")); prID != "" {
		id, err := strconv.Atoi(prID)
		if err != nil || id <= 0 {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("invalid pull request id"))
			return
		}
		diff, err := s.pullRequestUnifiedDiff(ctx, id)
		if err != nil {
			s.renderJSONError(w, http.StatusBadRequest, err)
			return
		}
		resp := map[string]any{"pr": id, "diff": diff}
		if pr, prErr := s.pullRequestByID(ctx, id); prErr == nil {
			resp["html"] = s.pullRequestFilesHTML(ctx, pr)
		}
		s.renderJSON(w, resp)
		return
	}
	if source := strings.TrimSpace(r.URL.Query().Get("source")); source != "" {
		target := strings.TrimSpace(r.URL.Query().Get("target"))
		if target == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("target branch is required"))
			return
		}
		html, mergeable, conflict, err := s.pullRequestPreviewDiffHTML(ctx, normalizeDestinationRef(target), normalizeDestinationRef(source))
		if err != nil {
			s.renderJSONError(w, http.StatusBadRequest, err)
			return
		}
		s.renderJSON(w, map[string]any{"html": html, "mergeable": mergeable, "conflict": conflict})
		return
	}
	repoPath := cleanWebPath(r.URL.Query().Get("path"))
	if repoPath == "" || repoPath == "." {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("diff requires a path"))
		return
	}
	mode := strings.TrimSpace(r.URL.Query().Get("mode"))
	if mode == "" {
		mode = "worktree"
	}
	diff, err := localFileDiff(repoPath, mode)
	if err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	diffHTML, htmlErr := localFileVisualDiffHTML(repoPath, mode)
	resp := map[string]any{
		"path": repoPath,
		"mode": mode,
		"diff": diff,
	}
	if htmlErr == nil {
		resp["html"] = diffHTML
	}
	s.renderJSON(w, resp)
	_ = ctx
}

func (s *webServer) handleAPIActionCommit(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("commit message is required"))
		return
	}
	repo, err := openLocalRepository(".")
	if err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	var out bytes.Buffer
	if err := repo.commit([]string{"-m", req.Message}, &out); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	state, err := s.webRepositoryState(ctx, false, "")
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	s.renderJSON(w, map[string]any{"ok": true, "output": strings.TrimSpace(out.String()), "state": state})
}

func (s *webServer) handleAPIActionStage(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	repoPath := cleanWebPath(req.Path)
	if repoPath == "" || repoPath == "." {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("stage requires a path"))
		return
	}
	repo, err := openLocalRepository(".")
	if err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	repoPath = canonicalWorktreePath(repo, repoPath)
	if err := repo.add([]string{repoPath}); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	state, err := s.webRepositoryState(ctx, false, "")
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	s.renderJSON(w, map[string]any{"ok": true, "state": state})
}

func (s *webServer) handleAPIActionUnstage(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	repoPath := cleanWebPath(req.Path)
	if repoPath == "" || repoPath == "." {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("unstage requires a path"))
		return
	}
	repo, err := openLocalRepository(".")
	if err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	if err := repo.reset([]string{"--", repoPath}); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	state, err := s.webRepositoryState(ctx, false, "")
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	s.renderJSON(w, map[string]any{"ok": true, "state": state})
}

func (s *webServer) handleAPIActionDiscard(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	repoPath := cleanWebPath(req.Path)
	if repoPath == "" || repoPath == "." {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("checkout requires a path"))
		return
	}
	repo, err := openLocalRepository(".")
	if err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	repoPath = canonicalWorktreePath(repo, repoPath)
	state, err := s.webRepositoryState(ctx, false, "")
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	source := firstNonEmpty(state.RemoteHead, state.LocalHead, "HEAD")
	if err := repo.checkoutPaths(source, []string{repoPath}); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	state, err = s.webRepositoryState(ctx, false, "")
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	s.renderJSON(w, map[string]any{"ok": true, "state": state})
}

func (s *webServer) handleAPIActionUncommit(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	state, err := s.webRepositoryState(ctx, false, "")
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if state.RemoteHead == "" || state.Ahead == 0 {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("no unpushed commits to uncommit"))
		return
	}
	repo, err := openLocalRepository(".")
	if err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	if err := repo.reset([]string{"--soft", state.RemoteHead}); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	state, err = s.webRepositoryState(ctx, false, "")
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	s.renderJSON(w, map[string]any{"ok": true, "state": state})
}

func canonicalWorktreePath(repo *localRepository, repoPath string) string {
	files, err := repo.allWorktreeFiles()
	if err != nil {
		return repoPath
	}
	for _, file := range files {
		if file == repoPath {
			return file
		}
	}
	for _, file := range files {
		if strings.EqualFold(file, repoPath) {
			return file
		}
	}
	return repoPath
}

func (s *webServer) handleAPIActionPush(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var out bytes.Buffer
	if err := run([]string{"push"}, strings.NewReader("n\n"), &out, &out); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	state, err := s.webRepositoryState(ctx, true, "")
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	s.renderJSON(w, map[string]any{"ok": true, "output": strings.TrimSpace(out.String()), "state": state})
}

func (s *webServer) handleAPIActionPull(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var out bytes.Buffer
	if err := run([]string{"pull"}, strings.NewReader(""), &out, &out); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	state, err := s.webRepositoryState(ctx, true, "")
	if err != nil {
		s.renderJSONError(w, http.StatusInternalServerError, err)
		return
	}
	s.renderJSON(w, map[string]any{"ok": true, "output": strings.TrimSpace(out.String()), "state": state})
}

func (s *webServer) handleAPIActionPullRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.TrimSpace(s.cfg.brokerURL) == "" {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("pull request actions require a broker-backed repository"))
		return
	}
	var req struct {
		ID              int                        `json:"id"`
		Action          string                     `json:"action"`
		Title           string                     `json:"title"`
		Body            string                     `json:"body"`
		Source          string                     `json:"source"`
		Target          string                     `json:"target"`
		Comment         string                     `json:"comment"`
		DeleteBranch    bool                       `json:"delete_branch"`
		Comments        []brokerPullRequestComment `json:"comments"`
		TargetNoteID    int                        `json:"target_note_id"`
		TargetCommentID int                        `json:"target_comment_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Action) != "create" && req.ID <= 0 {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("pull request id is required"))
		return
	}
	var resp struct {
		PR brokerPullRequest `json:"pr"`
	}
	brokerReq := brokerPullRequestRequest{
		Repo:            repoForBroker(s.cfg),
		ID:              req.ID,
		Comment:         strings.TrimSpace(req.Comment),
		DeleteBranch:    req.DeleteBranch,
		Comments:        req.Comments,
		TargetNoteID:    req.TargetNoteID,
		TargetCommentID: req.TargetCommentID,
	}
	endpoint := ""
	switch strings.TrimSpace(req.Action) {
	case "create":
		source := normalizeDestinationRef(req.Source)
		target := normalizeDestinationRef(req.Target)
		if source == "" || target == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("source and target branches are required"))
			return
		}
		if source == target {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("source and target branches must be different"))
			return
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			title = shortRefName(source) + " into " + shortRefName(target)
		}
		endpoint = "/prs/create"
		brokerReq.PR = brokerPullRequest{Title: title, Body: strings.TrimSpace(req.Body), Source: source, Target: target}
	case "comment":
		if brokerReq.Comment == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("comment is required"))
			return
		}
		endpoint = "/prs/comment"
	case "approve":
		endpoint = "/prs/review"
		brokerReq.Review = "approved"
	case "reject":
		endpoint = "/prs/review"
		brokerReq.Review = "changes_requested"
	case "review-comment":
		endpoint = "/prs/review"
		brokerReq.Review = "commented"
	case "reply":
		if brokerReq.Comment == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("comment is required"))
			return
		}
		endpoint = "/prs/reply"
	case "merge":
		endpoint = "/prs/merge"
		brokerReq.Merge = true
	case "close":
		endpoint = "/prs/close"
	case "reopen":
		endpoint = "/prs/reopen"
	default:
		s.renderJSONError(w, http.StatusBadRequest, fmt.Errorf("unsupported pull request action %q", req.Action))
		return
	}
	if err := brokerPostContext(ctx, s.cfg.brokerURL, endpoint, brokerReq, &resp); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	prs := s.upsertPullRequestCache(resp.PR)
	s.renderJSON(w, map[string]any{"ok": true, "pr": resp.PR, "prs": webAPIPullRequests(prs)})
}

func (s *webServer) handleAPIActionSettings(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.TrimSpace(s.cfg.brokerURL) == "" || strings.TrimSpace(s.cfg.logicalRepo) == "" {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("settings require a broker-backed repository"))
		return
	}
	var req struct {
		Action         string `json:"action"`
		Description    string `json:"description"`
		DefaultBranch  string `json:"default_branch"`
		Visibility     string `json:"visibility"`
		ReadOnly       bool   `json:"read_only"`
		IssuesEnabled  bool   `json:"issues_enabled"`
		Logical        string `json:"logical"`
		UserID         string `json:"user_id"`
		User           string `json:"user"`
		Role           string `json:"role"`
		Key            string `json:"key"`
		Ref            string `json:"ref"`
		RequirePR      bool   `json:"require_pr"`
		AllowOverrides bool   `json:"allow_overrides"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	endpoint := ""
	var payload any
	switch strings.TrimSpace(req.Action) {
	case "update-repo":
		endpoint = "/repo/update"
		payload = brokerRepoInfoRequest{
			Repo:          repoForBroker(s.cfg),
			Description:   req.Description,
			DefaultBranch: req.DefaultBranch,
			Visibility:    req.Visibility,
			ReadOnly:      req.ReadOnly,
			IssuesEnabled: req.IssuesEnabled,
		}
	case "add-member":
		user := strings.TrimSpace(req.User)
		if user == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("user is required"))
			return
		}
		role := normalizeBrokerRole(req.Role)
		if !validBrokerRole(role) || role == "owner" {
			s.renderJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid role %q", req.Role))
			return
		}
		endpoint = "/keys/invite/create"
		payload = brokerOwnerTransferRequest{Repo: repoForBroker(s.cfg), User: user, Role: role, BrokerURL: s.cfg.brokerURL}
	case "remove-member":
		endpoint = "/keys/remove"
		payload = brokerKeyRequest{Repo: repoForBroker(s.cfg), Key: strings.TrimSpace(req.Key)}
	case "remove-repo-user":
		user := strings.TrimSpace(req.User)
		userID := strings.TrimSpace(req.UserID)
		if user == "" && userID == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("user is required"))
			return
		}
		endpoint = "/repo/users/remove"
		payload = brokerRepoAdminRequest{Repo: repoForBroker(s.cfg), UserID: userID, User: user}
	case "suspend-member":
		endpoint = "/keys/suspend"
		payload = brokerKeyRequest{Repo: repoForBroker(s.cfg), Key: strings.TrimSpace(req.Key)}
	case "unsuspend-member":
		endpoint = "/keys/unsuspend"
		payload = brokerKeyRequest{Repo: repoForBroker(s.cfg), Key: strings.TrimSpace(req.Key)}
	case "transfer-owner":
		endpoint = "/owners/transfer/confirm"
		payload = brokerOwnerTransferRequest{Repo: repoForBroker(s.cfg), BrokerURL: s.cfg.brokerURL}
	case "repo-rename":
		logical, err := normalizeLogicalRepoName(req.Logical)
		if err != nil {
			s.renderJSONError(w, http.StatusBadRequest, err)
			return
		}
		endpoint = "/repo/rename"
		payload = brokerRepoInfoRequest{Repo: repoForBroker(s.cfg), Logical: logical}
	case "repo-delete":
		endpoint = "/repo/delete"
		payload = brokerRepoInfoRequest{Repo: repoForBroker(s.cfg)}
	case "protect-upsert":
		ref := normalizeDestinationRef(firstNonEmpty(strings.TrimSpace(req.Ref), defaultBranch))
		endpoint = "/protection/upsert"
		payload = brokerProtectionRequest{Repo: repoForBroker(s.cfg), Ref: ref, RequirePR: req.RequirePR, AllowOverrides: req.AllowOverrides}
	case "protect-remove":
		endpoint = "/protection/remove"
		payload = brokerProtectionRequest{Repo: repoForBroker(s.cfg), Ref: normalizeDestinationRef(req.Ref)}
	default:
		s.renderJSONError(w, http.StatusBadRequest, fmt.Errorf("unsupported settings action %q", req.Action))
		return
	}
	if endpoint != "/repo/update" {
		switch p := payload.(type) {
		case brokerKeyRequest:
			if strings.TrimSpace(p.Key) == "" && len(p.PublicKeys) == 0 {
				s.renderJSONError(w, http.StatusBadRequest, errors.New("member key is required"))
				return
			}
		case brokerOwnerTransferRequest:
			if endpoint == "/keys/invite/create" && strings.TrimSpace(p.User) == "" {
				s.renderJSONError(w, http.StatusBadRequest, errors.New("member invite requires user"))
				return
			}
		case brokerProtectionRequest:
			if strings.TrimSpace(p.Ref) == "" {
				s.renderJSONError(w, http.StatusBadRequest, errors.New("branch protection ref is required"))
				return
			}
		case brokerRepoInfoRequest:
			if endpoint == "/repo/rename" && strings.TrimSpace(p.Logical) == "" {
				s.renderJSONError(w, http.StatusBadRequest, errors.New("logical repo name is required"))
				return
			}
		}
	}
	var brokerResp map[string]any
	if err := brokerPostContext(ctx, s.cfg.brokerURL, endpoint, payload, &brokerResp); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	if endpoint == "/repo/rename" && strings.TrimSpace(req.Logical) != "" {
		logical, err := normalizeLogicalRepoName(req.Logical)
		if err != nil {
			s.renderJSONError(w, http.StatusBadRequest, err)
			return
		}
		_, _ = runGit(".", "config", "--local", "bucketgit.logicalRepo", logical)
		_, _ = runGit(".", "remote", "set-url", "origin", "git@"+defaultSSHHost+":"+logical)
		s.cfg.logicalRepo = logical
	}
	s.renderJSON(w, map[string]any{"ok": true, "settings": s.settingsInfo(ctx), "broker": brokerResp})
}

func (s *webServer) handleAPIActionIssue(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.TrimSpace(s.cfg.brokerURL) == "" || strings.TrimSpace(s.cfg.logicalRepo) == "" {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("issues require a broker-backed repository"))
		return
	}
	var req struct {
		Action  string `json:"action"`
		ID      int    `json:"id"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	endpoint := ""
	payload := brokerIssueRequest{Repo: repoForBroker(s.cfg), ID: req.ID, Title: strings.TrimSpace(req.Title), Body: strings.TrimSpace(req.Body), Comment: strings.TrimSpace(req.Comment)}
	switch strings.TrimSpace(req.Action) {
	case "create":
		endpoint = "/issues/create"
		if payload.Title == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("issue title is required"))
			return
		}
	case "comment":
		endpoint = "/issues/comment"
		if payload.ID <= 0 || payload.Comment == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("issue comment requires an issue and comment"))
			return
		}
	case "close":
		endpoint = "/issues/close"
	case "reopen":
		endpoint = "/issues/reopen"
	default:
		s.renderJSONError(w, http.StatusBadRequest, fmt.Errorf("unsupported issue action %q", req.Action))
		return
	}
	var resp map[string]any
	if err := brokerPostContext(ctx, s.cfg.brokerURL, endpoint, payload, &resp); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	s.renderJSON(w, map[string]any{"ok": true})
}

func (s *webServer) handleAPIActionBoard(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.TrimSpace(s.cfg.brokerURL) == "" || strings.TrimSpace(s.cfg.logicalRepo) == "" {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("board requires a broker-backed repository"))
		return
	}
	var req struct {
		Action   string `json:"action"`
		ID       int    `json:"id"`
		Title    string `json:"title"`
		Body     string `json:"body"`
		Lane     string `json:"lane"`
		Assignee string `json:"assignee"`
		Comment  string `json:"comment"`
		AfterID  *int   `json:"after_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	payload := brokerIssueRequest{Repo: repoForBroker(s.cfg), ID: req.ID, Type: "story", Title: strings.TrimSpace(req.Title), Body: strings.TrimSpace(req.Body), Lane: strings.TrimSpace(req.Lane), Assignee: strings.TrimSpace(req.Assignee), Comment: strings.TrimSpace(req.Comment), AfterID: req.AfterID}
	endpoint := ""
	switch strings.TrimSpace(req.Action) {
	case "create":
		endpoint = "/issues/create"
		payload.Lane = firstNonEmpty(payload.Lane, "backlog")
		if payload.Body == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("story is required"))
			return
		}
		payload.Title = firstNonEmpty(payload.Title, storySummary(payload.Body))
	case "move":
		endpoint = "/issues/move"
		if payload.ID <= 0 || payload.Lane == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("story move requires an id and lane"))
			return
		}
	case "reorder":
		endpoint = "/issues/reorder"
		if payload.ID <= 0 || payload.Lane == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("story reorder requires an id and lane"))
			return
		}
	case "edit":
		endpoint = "/issues/update"
		if payload.ID <= 0 || payload.Body == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("story edit requires an id and story"))
			return
		}
		payload.Title = firstNonEmpty(payload.Title, storySummary(payload.Body))
	case "archive", "unarchive":
		endpoint = "/issues/archive"
		payload.Archived = strings.TrimSpace(req.Action) == "archive"
		if payload.ID <= 0 {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("story archive requires an id"))
			return
		}
	case "take":
		endpoint = "/issues/take"
		if payload.ID <= 0 {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("story id is required"))
			return
		}
	case "assign":
		endpoint = "/issues/assign"
		if payload.ID <= 0 {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("story assignment requires an id"))
			return
		}
	case "comment":
		endpoint = "/issues/comment"
		if payload.ID <= 0 || payload.Comment == "" {
			s.renderJSONError(w, http.StatusBadRequest, errors.New("story comment requires an id and comment"))
			return
		}
	default:
		s.renderJSONError(w, http.StatusBadRequest, fmt.Errorf("unsupported board action %q", req.Action))
		return
	}
	var resp map[string]any
	if err := brokerPostContext(ctx, s.cfg.brokerURL, endpoint, payload, &resp); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, boardUpgradeError(err))
		return
	}
	s.renderJSON(w, map[string]any{"ok": true})
}

func (s *webServer) handleAPIActionCI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.TrimSpace(s.cfg.brokerURL) == "" || strings.TrimSpace(s.cfg.logicalRepo) == "" {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("CI actions require a broker-backed repository"))
		return
	}
	var req struct {
		Action   string `json:"action"`
		ID       int    `json:"id"`
		Provider string `json:"provider"`
		Ref      string `json:"ref"`
		Config   string `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	switch strings.TrimSpace(req.Action) {
	case "list":
		runs, err := s.listCIRuns(ctx)
		if err != nil {
			s.renderJSONError(w, http.StatusBadRequest, err)
			return
		}
		s.renderJSON(w, map[string]any{"ok": true, "runs": runs})
		return
	case "logs":
		var resp struct {
			Run  brokerCIRun `json:"run"`
			Logs string      `json:"logs"`
		}
		if err := brokerPostContext(ctx, s.cfg.brokerURL, "/ci/logs", brokerCIRequest{Repo: repoForBroker(s.cfg), ID: req.ID}, &resp); err != nil {
			s.renderJSONError(w, http.StatusBadRequest, err)
			return
		}
		s.renderJSON(w, map[string]any{"ok": true, "run": resp.Run, "logs": resp.Logs})
		return
	case "run":
	default:
		s.renderJSONError(w, http.StatusBadRequest, fmt.Errorf("unsupported CI action %q", req.Action))
		return
	}
	ref := normalizeDestinationRef(firstNonEmpty(strings.TrimSpace(req.Ref), s.cfg.branch, defaultBranch))
	commit := ""
	if head, err := runGit(".", "rev-parse", shortRefName(ref)); err == nil {
		commit = strings.TrimSpace(string(head))
	} else if head, err := runGit(".", "rev-parse", "HEAD"); err == nil {
		commit = strings.TrimSpace(string(head))
	}
	config := strings.TrimSpace(req.Config)
	if config == "" {
		if detected, err := detectCIConfig(req.Provider, commit); err == nil {
			config = detected
		}
	}
	var resp struct {
		Run brokerCIRun `json:"run"`
	}
	if err := brokerPostContext(ctx, s.cfg.brokerURL, "/ci/run", brokerCIRequest{Repo: repoForBroker(s.cfg), Provider: normalizeCIProvider(req.Provider), Ref: ref, Commit: commit, Config: config}, &resp); err != nil {
		s.renderJSONError(w, http.StatusBadRequest, err)
		return
	}
	runs, _ := s.listCIRuns(ctx)
	s.renderJSON(w, map[string]any{"ok": true, "run": resp.Run, "runs": runs})
}

func (s *webServer) webRepositoryState(ctx context.Context, refreshRemote bool, selectedRef string) (webAPIState, error) {
	localRepo, err := openLocalRepository(".")
	if err != nil {
		return webAPIState{}, err
	}
	currentBranch := localRepo.currentBranch()
	ref := normalizeWebRef(selectedRef)
	if ref == "" {
		ref = branchRef(firstNonEmpty(currentBranch, s.cfg.branch, defaultBranch))
	}
	branch := shortRefName(ref)
	state := webAPIState{Branch: branch}
	isBranchRef := strings.HasPrefix(ref, "refs/heads/")
	if refreshRemote && isBranchRef && (s.cfg.brokerURL != "" || s.cfg.bucket != "" || s.cfg.origin != "") {
		if err := s.fetchWebRemoteTracking(ctx, ref); err != nil {
			state.FetchError = err.Error()
		}
	}
	if head, err := localRepo.resolveRevision(ref); err == nil {
		state.LocalHead = head
	}
	if isBranchRef {
		remoteRef := "refs/remotes/bucketgit/" + shortBranchName(ref)
		if remoteHead, err := localRepo.resolveRevision(remoteRef); err == nil {
			state.RemoteHead = remoteHead
		} else if remoteHead, err := localRepo.resolveRevision("refs/remotes/origin/" + shortBranchName(ref)); err == nil {
			state.RemoteHead = remoteHead
		}
	}

	if currentBranch != "" && ref == branchRef(currentBranch) {
		status := localWorkingTreeStatus()
		state.StagedFiles = status.staged
		state.UnstagedFiles = status.unstaged
		state.UntrackedFiles = status.untracked
		state.DirtyFiles = append(state.DirtyFiles, state.StagedFiles...)
		state.DirtyFiles = append(state.DirtyFiles, state.UnstagedFiles...)
		state.DirtyFiles = append(state.DirtyFiles, state.UntrackedFiles...)
		state.DirtyFiles = uniqueSortedStrings(state.DirtyFiles)
		state.StagedFiles = uniqueSortedStrings(state.StagedFiles)
		state.UnstagedFiles = uniqueSortedStrings(state.UnstagedFiles)
		state.UntrackedFiles = uniqueSortedStrings(state.UntrackedFiles)
		sort.Strings(state.DirtyFiles)
		state.Dirty = len(state.DirtyFiles) > 0
	}
	state.UnpushedFiles = localChangedFilesBetween(localRepo, state.RemoteHead, state.LocalHead)
	state.UnpulledFiles = localChangedFilesBetween(localRepo, state.LocalHead, state.RemoteHead)

	if state.LocalHead != "" {
		commits, err := localCommitRange(localRepo, state.RemoteHead, state.LocalHead, 25)
		if err != nil {
			return state, err
		}
		state.UnpushedCommits = webAPICommitsFromCommits(commits)
		state.Ahead = len(commits)
	}
	if state.RemoteHead != "" {
		commits, err := localCommitRange(localRepo, state.LocalHead, state.RemoteHead, 25)
		if err != nil {
			return state, err
		}
		state.UnpulledCommits = webAPICommitsFromCommits(commits)
		state.Behind = len(commits)
	}
	return state, nil
}

func (s *webServer) fetchWebRemoteTracking(ctx context.Context, branch string) error {
	worktree, err := requireWorktree(".")
	if err != nil {
		return err
	}
	localCfg, err := readLocalConfig(worktree)
	if err != nil {
		return err
	}
	cfg := mergeConfig(localCfg, s.cfg)
	cfg.branch = firstNonEmpty(branch, cfg.branch, defaultBranch)
	store, closeStore, err := newRemoteStore(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer closeStore()
	repo := openNativeGitRepo(store, cfg)
	return repo.fetchRefsIntoWorktree(ctx, worktree, true, io.Discard)
}

type webWorkingTreeStatus struct {
	staged    []string
	unstaged  []string
	untracked []string
}

func localWorkingTreeStatus() webWorkingTreeStatus {
	out, err := runGit(".", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return webWorkingTreeStatus{}
	}
	var status webWorkingTreeStatus
	for _, line := range strings.Split(strings.TrimRight(string(out), "\r\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		indexStatus := line[0]
		worktreeStatus := line[1]
		path := strings.TrimSpace(line[3:])
		if before, after, ok := strings.Cut(path, " -> "); ok && before != "" {
			path = strings.TrimSpace(after)
		}
		path = strings.Trim(path, `"`)
		if path == "" {
			continue
		}
		if indexStatus == '?' && worktreeStatus == '?' {
			status.untracked = append(status.untracked, path)
			continue
		}
		if indexStatus != ' ' && indexStatus != '?' {
			status.staged = append(status.staged, path)
		}
		if worktreeStatus != ' ' && worktreeStatus != '?' {
			status.unstaged = append(status.unstaged, path)
		}
	}
	return status
}

func localFileDiff(repoPath, mode string) (string, error) {
	repo, err := openLocalRepository(".")
	if err != nil {
		return "", err
	}
	repoPath = canonicalWorktreePath(repo, repoPath)
	switch mode {
	case "staged", "cached":
		out, err := runGit(".", "diff", "--cached", "--", repoPath)
		if err != nil {
			return "", err
		}
		return string(out), nil
	case "worktree", "unstaged", "":
		out, err := runGit(".", "diff", "--", repoPath)
		if err != nil {
			return "", err
		}
		if len(out) > 0 {
			return string(out), nil
		}
		status := localWorkingTreeStatus()
		for _, path := range status.untracked {
			if path == repoPath {
				return localUntrackedFileDiff(repoPath)
			}
		}
		return "", nil
	default:
		return "", fmt.Errorf("unsupported diff mode %q", mode)
	}
}

func (s *webServer) commitChangedFiles(ctx context.Context, hash string) ([]webChangedFile, int, int, error) {
	commitHash, err := s.repo.resolveRevision(ctx, hash)
	if err != nil {
		commitHash = hash
	}
	commit, err := s.repo.commit(ctx, commitHash)
	if err != nil {
		return nil, 0, 0, err
	}
	return s.changedFiles(ctx, commit)
}

func localFileVisualDiffHTML(repoPath, mode string) (string, error) {
	repo, err := openLocalRepository(".")
	if err != nil {
		return "", err
	}
	repoPath = canonicalWorktreePath(repo, repoPath)
	var oldData, newData []byte
	switch mode {
	case "staged", "cached":
		oldData = gitBlobData("HEAD:" + repoPath)
		newData = gitBlobData(":" + repoPath)
	case "worktree", "unstaged", "":
		oldData = gitBlobData(":" + repoPath)
		if oldData == nil {
			oldData = gitBlobData("HEAD:" + repoPath)
		}
		if data, err := readLocalWorktreeRegularFile(repo.worktree, repoPath); err == nil {
			newData = data
		}
	default:
		return "", fmt.Errorf("unsupported diff mode %q", mode)
	}
	file := webChangedFileFromData(repoPath, "", "", oldData, newData)
	return diffFileHTML(file), nil
}

func gitBlobData(revisionPath string) []byte {
	out, err := runGit(".", "show", revisionPath)
	if err != nil {
		return nil
	}
	return out
}

func localUntrackedFileDiff(repoPath string) (string, error) {
	repo, err := openLocalRepository(".")
	if err != nil {
		return "", err
	}
	data, err := readLocalWorktreeRegularFile(repo.worktree, repoPath)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n", repoPath, repoPath)
	fmt.Fprintln(&b, "new file mode 100644")
	fmt.Fprintln(&b, "index 0000000..0000000 100644")
	fmt.Fprintln(&b, "--- /dev/null")
	fmt.Fprintf(&b, "+++ b/%s\n", repoPath)
	if !isTextBlob(data) {
		fmt.Fprintln(&b, "Binary file changed")
		return b.String(), nil
	}
	for _, line := range splitLines(string(data)) {
		b.WriteString("+")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String(), nil
}

func readLocalWorktreeRegularFile(worktree, repoPath string) ([]byte, error) {
	repoPath = cleanWebPath(repoPath)
	if repoPath == "" {
		return nil, errors.New("file path is required")
	}
	target := filepath.Join(worktree, filepath.FromSlash(repoPath))
	rel, err := filepath.Rel(worktree, target)
	if err != nil {
		return nil, err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("path escapes worktree: %s", repoPath)
	}
	info, err := os.Lstat(target)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to read symlink: %s", repoPath)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", repoPath)
	}
	return os.ReadFile(target)
}

func localCommitDiff(hash string) (string, error) {
	repo, err := openLocalRepository(".")
	if err != nil {
		return "", err
	}
	hash, err = repo.resolveRevision(hash)
	if err != nil {
		return "", err
	}
	commit, err := repo.commitObject(hash)
	if err != nil {
		return "", err
	}
	if len(commit.parents) == 0 {
		out, err := runGit(".", "show", "--format=", "--patch", hash)
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
	out, err := runGit(".", "diff", commit.parents[0], hash)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func localCommitVisualDiffHTML(hash string) (string, error) {
	fullHash, err := gitOutputLine("rev-parse", hash)
	if err != nil {
		return "", err
	}
	parentLine, _ := gitOutputLine("show", "-s", "--format=%P", fullHash)
	parent := ""
	if fields := strings.Fields(parentLine); len(fields) > 0 {
		parent = fields[0]
	}
	var namesOut []byte
	if parent != "" {
		namesOut, err = runGit(".", "diff", "--name-only", parent, fullHash)
	} else {
		namesOut, err = runGit(".", "show", "--format=", "--name-only", fullHash)
	}
	if err != nil {
		return "", err
	}
	var files []webChangedFile
	totalAdditions := 0
	totalDeletions := 0
	for _, name := range strings.Split(strings.TrimSpace(string(namesOut)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		var oldData []byte
		if parent != "" {
			oldData = gitBlobData(parent + ":" + name)
		}
		newData := gitBlobData(fullHash + ":" + name)
		file := webChangedFileFromData(name, "", "", oldData, newData)
		totalAdditions += file.additions
		totalDeletions += file.deletions
		files = append(files, file)
	}
	return diffFilesPanelHTML(files, totalAdditions, totalDeletions), nil
}

func gitOutputLine(args ...string) (string, error) {
	out, err := runGit(".", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	files := make([]string, 0, len(seen))
	for value := range seen {
		files = append(files, value)
	}
	sort.Strings(files)
	return files
}

func localCommitRange(repo *localRepository, base, head string, limit int) ([]commitObject, error) {
	if head == "" || head == base {
		return nil, nil
	}
	excluded := map[string]struct{}{}
	if base != "" {
		if err := markCommitAncestors(repo, base, excluded); err != nil {
			return nil, err
		}
	}
	seen := map[string]struct{}{}
	var commits []commitObject
	stack := []string{head}
	for len(stack) > 0 {
		hash := stack[0]
		stack = stack[1:]
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		if _, ok := excluded[hash]; ok {
			continue
		}
		commit, err := repo.commitObject(hash)
		if err != nil {
			return nil, err
		}
		commits = append(commits, commit)
		stack = append(stack, commit.parents...)
	}
	sort.SliceStable(commits, func(i, j int) bool {
		return commits[i].timestamp > commits[j].timestamp
	})
	if limit > 0 && len(commits) > limit {
		commits = commits[:limit]
	}
	return commits, nil
}

func markCommitAncestors(repo *localRepository, head string, out map[string]struct{}) error {
	stack := []string{head}
	for len(stack) > 0 {
		hash := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := out[hash]; ok {
			continue
		}
		out[hash] = struct{}{}
		commit, err := repo.commitObject(hash)
		if err != nil {
			return err
		}
		stack = append(stack, commit.parents...)
	}
	return nil
}

func localChangedFilesBetween(repo *localRepository, base, head string) []string {
	if base == "" || head == "" || base == head {
		return nil
	}
	before, err := repo.treeFilesForCommit(base)
	if err != nil {
		return nil
	}
	after, err := repo.treeFilesForCommit(head)
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	for path, afterFile := range after {
		if beforeFile, ok := before[path]; !ok || beforeFile.hash != afterFile.hash || beforeFile.mode != afterFile.mode {
			seen[path] = struct{}{}
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			seen[path] = struct{}{}
		}
	}
	files := make([]string, 0, len(seen))
	for path := range seen {
		files = append(files, path)
	}
	sort.Strings(files)
	return files
}

func (s *webServer) handleTree(ctx context.Context, w http.ResponseWriter, r *http.Request, repoPath string) {
	_, commit, ref, err := s.headCommit(ctx, r)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.renderEmptyRepository(w, ref, cleanWebPath(repoPath))
			return
		}
		s.renderError(w, http.StatusNotFound, err)
		return
	}
	repoPath = cleanWebPath(repoPath)
	treeHash := commit.tree
	if repoPath != "" && repoPath != "commits" && repoPath != "prs" {
		hash, err := s.repo.findPath(ctx, commit.tree, repoPath)
		if err != nil {
			s.renderError(w, http.StatusNotFound, err)
			return
		}
		obj, err := s.repo.object(ctx, hash)
		if err != nil {
			s.renderError(w, http.StatusInternalServerError, err)
			return
		}
		if obj.typ == gitObjectBlob {
			http.Redirect(w, r, webURL("blob", repoPath, ref), http.StatusFound)
			return
		}
		treeHash = hash
	}
	entries, err := s.repo.treeEntries(ctx, treeHash)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err)
		return
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].typ != entries[j].typ {
			return entries[i].typ == gitObjectTree
		}
		return entries[i].name < entries[j].name
	})
	repoCommits, _ := s.repo.walkCommits(ctx, commit.hash, 200, 0, "")
	readme := s.readmeHTML(ctx, commit.tree)

	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-head="` + html.EscapeString(commit.hash) + `" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, repoPath))
	body.WriteString(s.repoToolbarHTML(ref, true))
	body.WriteString(s.fileIndexHTML(ctx, commit.tree, ref))
	body.WriteString(`<div class="repo-content"><div class="repo-primary">`)
	body.WriteString(`<section class="panel files-panel"><div class="commit-strip"><span class="commit-author">` + html.EscapeString(commit.author) + `</span><a class="commit-subject" href="` + html.EscapeString(webCommitURL(commit.hash, ref)) + `">` + html.EscapeString(displayCommitSubject(commit)) + `</a><a class="commit-hash-link" href="` + html.EscapeString(webCommitURL(commit.hash, ref)) + `"><code>` + html.EscapeString(shortHash(commit.hash)) + `</code></a><span class="commit-when">` + html.EscapeString(relativeTime(commit.timestamp)) + `</span></div><table class="files" data-file-list>`)
	if repoPath != "" && repoPath != "commits" && repoPath != "prs" {
		parent := pathpkg.Dir(repoPath)
		if parent == "." {
			parent = ""
		}
		body.WriteString(`<tr data-file-row data-file-name=".." data-file-path=".."><td class="kind">dir</td><td><a href="` + html.EscapeString(webURL("tree", parent, ref)) + `">..</a></td><td></td></tr>`)
	}
	for _, entry := range entries {
		targetPath := pathpkg.Join(repoPath, entry.name)
		kind := "file"
		route := "blob"
		name := entry.name
		if entry.typ == gitObjectTree {
			kind = "dir"
			route = "tree"
			name += "/"
		}
		body.WriteString(`<tr data-file-row data-file-name="` + html.EscapeString(strings.ToLower(name)) + `" data-file-path="` + html.EscapeString(targetPath) + `"><td class="kind">` + kind + `</td><td><a href="` + html.EscapeString(webURL(route, targetPath, ref)) + `">` + html.EscapeString(name) + `</a><span class="state-actions" data-file-state></span></td><td class="hash">` + html.EscapeString(shortHash(entry.hash)) + `</td></tr>`)
	}
	body.WriteString(`</table></section>`)
	body.WriteString(`<section class="panel readme-panel"><div class="panel-title">README</div><article class="readme">`)
	if readme != "" && repoPath == "" {
		body.WriteString(readme)
	}
	body.WriteString(`</article></section>`)
	body.WriteString(`</div>`)
	body.WriteString(s.repoSidePanelHTML(contributorsFromCommits(repoCommits)))
	body.WriteString(`</div></main>`)
	s.renderPage(w, webPageTitle(s.title, repoPath), body.String())
}

func (s *webServer) renderEmptyRepository(w http.ResponseWriter, ref, repoPath string) {
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-head="" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, repoPath))
	body.WriteString(s.emptyRepositoryBootstrapHTML(ref))
	body.WriteString(`</main>`)
	s.renderPage(w, webPageTitle(s.title, repoPath), body.String())
}

func (s *webServer) emptyRepositoryBootstrapHTML(ref string) string {
	repoName := strings.TrimSuffix(strings.Trim(s.title, "/"), ".git")
	if repoName == "" {
		repoName = "repository"
	}
	repoTarget := firstNonEmpty(s.primaryRepoTarget(), repoName+".git")
	cloneCommand := "bgit clone " + shellSingleToken(repoTarget)
	firstCommit := strings.Join([]string{
		`echo "# ` + shellDoubleQuoteText(repoName) + `" >> README.md`,
		"bgit init " + shellSingleToken(repoTarget),
		"bgit add README.md",
		`bgit commit -m "first commit"`,
		"bgit push -u origin main",
	}, "\n")
	existingRepo := strings.Join([]string{
		"bgit init " + shellSingleToken(repoTarget),
		"bgit push -u origin main",
	}, "\n")
	var b strings.Builder
	b.WriteString(`<div class="empty-repo-wrap">`)
	b.WriteString(`<div class="empty-repo-cards">`)
	b.WriteString(`<section class="empty-repo-card"><div class="empty-repo-icon">` + repoIconHTML() + `</div><h2>Create the first commit</h2><p>Add a README or push existing source to make this repository browsable.</p></section>`)
	b.WriteString(`<section class="empty-repo-card"><div class="empty-repo-icon">` + userPlusIconHTML() + `</div><h2>Give access to collaborators</h2><p>Use Settings to grant users and teams access to this repository.</p><a class="button-link" href="/settings">Manage access</a></section>`)
	b.WriteString(`</div>`)
	b.WriteString(`<section class="empty-repo-setup">`)
	b.WriteString(`<div class="empty-repo-quick"><h2>Quick setup if you have done this kind of thing before</h2><div class="empty-repo-clone"><code id="empty-clone-command">` + html.EscapeString(cloneCommand) + `</code><button class="copy-button copy-icon-button" data-copy-icon data-copy-target="empty-clone-command" aria-label="Copy clone command" title="Copy clone command"><span aria-hidden="true">📋</span></button></div><p>Get started by creating a README, LICENSE, or .gitignore, then push your first commit.</p></div>`)
	b.WriteString(emptyRepoCommandBlockHTML("...or create a new repository on the command line", "empty-first-commit", firstCommit))
	b.WriteString(emptyRepoCommandBlockHTML("...or push an existing repository from the command line", "empty-existing-repo", existingRepo))
	b.WriteString(`</section></div>`)
	return b.String()
}

func (s *webServer) primaryRepoTarget() string {
	logicalRepo := strings.Trim(s.cfg.logicalRepo, "/")
	if s.cfg.provider == "local" && logicalRepo != "" {
		return localBrokerBootstrapTarget(s.cfg, logicalRepo)
	}
	if s.cfg.brokerURL != "" && logicalRepo != "" {
		return brokerCloneCommandURL(s.cfg.brokerURL, s.cfg.teamID, logicalRepo)
	}
	if s.cfg.origin != "" {
		return s.cfg.origin
	}
	return ""
}

func localBrokerBootstrapTarget(cfg config, logicalRepo string) string {
	logicalRepo = strings.Trim(logicalRepo, "/")
	if logicalRepo == "" {
		return ""
	}
	provider := firstNonEmpty(strings.TrimSpace(cfg.storageProvider), strings.TrimSpace(cfg.provider))
	if parsed, ok, _ := localBrokerCloudConfig(cfg.bucket); ok {
		provider = parsed.provider
	}
	switch provider {
	case "s3":
		return "s3://" + logicalRepo
	case "gcs", "gs":
		return "gs://" + logicalRepo
	case "file", "local":
		if strings.HasPrefix(cfg.bucket, "file://") {
			return cfg.bucket
		}
		return "file://" + logicalRepo
	default:
		return "file://" + logicalRepo
	}
}

func emptyRepoCommandBlockHTML(title, id, command string) string {
	return `<div class="empty-repo-command"><h2>` + html.EscapeString(title) + `</h2><div class="empty-repo-code"><pre id="` + html.EscapeString(id) + `">` + html.EscapeString(command) + `</pre><button class="copy-button copy-icon-button" data-copy-icon data-copy-target="` + html.EscapeString(id) + `" aria-label="Copy commands" title="Copy commands"><span aria-hidden="true">📋</span></button></div></div>`
}

func shellSingleToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "origin"
	}
	if strings.ContainsAny(value, " \t\n'\"\\$`") {
		return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
	}
	return value
}

func shellDoubleQuoteText(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(value)
}

func (s *webServer) handleCommits(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	_, commit, ref, err := s.headCommit(ctx, r)
	if err != nil {
		s.renderError(w, http.StatusNotFound, err)
		return
	}
	selectedHash := strings.TrimSpace(r.URL.Query().Get("commit"))
	if selectedHash == "" {
		selectedHash = strings.TrimSpace(r.URL.Query().Get("selected"))
	}
	if selectedHash != "" {
		if resolved, err := s.repo.resolveRevision(ctx, selectedHash); err == nil {
			selectedHash = resolved
		}
	}
	commits, err := s.repo.walkCommits(ctx, commit.hash, 100, 0, "")
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err)
		return
	}
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-head="` + html.EscapeString(commit.hash) + `" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, "commits"))
	body.WriteString(`<div class="repo-content repo-content-single"><div class="repo-primary"><section class="panel"><div class="panel-title">Commits</div>`)
	body.WriteString(s.commitListHTML(ctx, commits, ref, false, selectedHash))
	body.WriteString(`</section></div></div></main>`)
	s.renderPage(w, webPageTitle(s.title, "commits"), body.String())
}

func (s *webServer) handlePullRequests(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	prs := []brokerPullRequest{}
	source := "cache"
	stale := false
	if cached, err := s.readPullRequestCache(); err == nil {
		prs = cached.PRs
		stale = true
	} else if refreshed, err := s.refreshPullRequestCache(ctx); err == nil {
		prs = refreshed
		source = "broker"
	}
	ref := branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, "prs"))
	body.WriteString(`<div class="repo-content repo-content-single"><div class="repo-primary"><section class="panel"><div class="panel-title pr-list-title"><span>Pull requests</span><a class="button-link" href="/prs/new" data-capability="push">New pull request</a></div><div data-pr-list data-pr-source="` + html.EscapeString(source) + `"` + boolDataAttr("stale", stale) + `>`)
	body.WriteString(pullRequestListHTML(prs))
	body.WriteString(`</div></section></div></div></main>`)
	s.renderPage(w, webPageTitle(s.title, "pull requests"), body.String())
}

func (s *webServer) handleNewPullRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	ref := branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, "prs"))
	body.WriteString(`<div class="repo-content repo-content-single"><div class="repo-primary"><section class="panel pr-create-panel"><div class="panel-title">Open a pull request</div>`)
	if strings.TrimSpace(s.cfg.brokerURL) == "" {
		body.WriteString(`<div class="empty">Pull requests are available for broker-backed repositories.</div></section></div></div></main>`)
		s.renderPage(w, webPageTitle(s.title, "new pull request"), body.String())
		return
	}
	body.WriteString(s.pullRequestCreateHTML(ctx, ref))
	body.WriteString(`</section></div></div></main>`)
	s.renderPage(w, webPageTitle(s.title, "new pull request"), body.String())
}

func (s *webServer) pullRequestCreateHTML(ctx context.Context, currentRef string) string {
	options, err := s.refOptions(ctx)
	if err != nil {
		return `<div class="settings-error">` + html.EscapeString(err.Error()) + `</div>`
	}
	var branches []webRefOption
	for _, option := range options {
		if option.kind == "Branches" {
			branches = append(branches, option)
		}
	}
	if len(branches) == 0 {
		return `<div class="empty">No branches found.</div>`
	}
	target := branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	source := normalizeWebRef(currentRef)
	if source == "" || source == target {
		for _, branch := range branches {
			if branch.fullName != target {
				source = branch.fullName
				break
			}
		}
	}
	if source == "" {
		source = branches[0].fullName
	}
	var b strings.Builder
	b.WriteString(`<form class="pr-create-form" data-pr-create-form data-capability="push">`)
	b.WriteString(`<div class="pr-compare-box"><label>base<select name="target" data-pr-create-target>`)
	for _, branch := range branches {
		selected := ""
		if branch.fullName == target {
			selected = ` selected`
		}
		b.WriteString(`<option value="` + html.EscapeString(branch.fullName) + `"` + selected + `>` + html.EscapeString(branch.name) + `</option>`)
	}
	b.WriteString(`</select></label><span aria-hidden="true">←</span><label>compare<select name="source" data-pr-create-source>`)
	for _, branch := range branches {
		selected := ""
		if branch.fullName == source {
			selected = ` selected`
		}
		b.WriteString(`<option value="` + html.EscapeString(branch.fullName) + `"` + selected + `>` + html.EscapeString(branch.name) + `</option>`)
	}
	b.WriteString(`</select></label></div>`)
	b.WriteString(`<div class="pr-create-summary" data-pr-create-summary></div>`)
	b.WriteString(`<div class="pr-create-diff" data-pr-create-diff><div class="empty">Select branches to preview the diff.</div></div>`)
	b.WriteString(`<label>Title<input name="title" autocomplete="off" placeholder="Briefly describe these changes"></label>`)
	b.WriteString(`<label>Description<textarea name="body" rows="8" placeholder="Describe what changed and why"></textarea></label>`)
	b.WriteString(`<div class="settings-actions"><a class="button-link" href="/prs">Cancel</a><button class="button-link primary" type="submit">Create pull request</button></div>`)
	b.WriteString(`</form>`)
	return b.String()
}

func (s *webServer) handleIssues(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	issues, err := s.listIssues(ctx)
	if err != nil {
		issues = nil
	}
	ref := branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, "issues"))
	body.WriteString(`<div class="repo-content repo-content-single"><div class="repo-primary"><section class="panel issues-panel"><div class="panel-title">Issues</div>`)
	body.WriteString(issueListHTML(issues))
	body.WriteString(`<form class="issue-form" data-issue-form="create"><h3>New issue</h3><label>Title<input name="title" autocomplete="off" required></label><label>Description<textarea name="body" rows="4"></textarea></label><div class="settings-actions"><button class="button-link primary" type="submit">Create issue</button></div></form>`)
	if err != nil {
		body.WriteString(`<div class="settings-error">` + html.EscapeString(err.Error()) + `</div>`)
	}
	body.WriteString(`</section></div></div></main>`)
	s.renderPage(w, webPageTitle(s.title, "issues"), body.String())
}

func (s *webServer) handleBoard(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	archived := r.URL.Query().Get("view") == "archived" || r.URL.Query().Get("archived") == "1"
	issues, err := s.listIssuesWithOptions(ctx, brokerIssueRequest{Repo: repoForBroker(s.cfg), Type: "story", IncludeArchived: archived})
	if err != nil {
		issues = nil
	}
	boardCtx := s.boardRenderContext(ctx)
	ref := branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, "board"))
	body.WriteString(`<div class="repo-content repo-content-single"><div class="repo-primary"><section class="panel board-panel" data-board-panel><div class="panel-title board-title"><span>Task board</span><span class="story-tabs"><a class="button-link story-chip`)
	if !archived {
		body.WriteString(` active`)
	}
	body.WriteString(`" href="/board">Active</a><a class="button-link story-chip`)
	if archived {
		body.WriteString(` active`)
	}
	body.WriteString(`" href="/board?view=archived">Archived</a></span><button class="button-link story-chip" type="button" data-board-only-me aria-pressed="false">Only me</button></div>`)
	body.WriteString(boardHTML(issues, boardCtx, archived))
	if err != nil {
		body.WriteString(`<div class="settings-error">` + html.EscapeString(err.Error()) + `</div>`)
	}
	body.WriteString(`</section></div></div></main>`)
	s.renderPage(w, webPageTitle(s.title, "board"), body.String())
}

func (s *webServer) handleCI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	runs, err := s.listCIRuns(ctx)
	ref := branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, "ci"))
	body.WriteString(`<div class="repo-content repo-content-single"><div class="repo-primary"><section class="panel ci-panel"><div class="panel-title">CI/CD</div>`)
	if strings.TrimSpace(s.cfg.brokerURL) == "" {
		body.WriteString(`<div class="empty">CI is available for broker-backed repositories.</div>`)
	} else {
		body.WriteString(ciRunFormHTML(s.cfg))
		body.WriteString(ciRunListHTML(runs))
	}
	if err != nil {
		body.WriteString(`<div class="settings-error">` + html.EscapeString(err.Error()) + `</div>`)
	}
	body.WriteString(`</section></div></div></main>`)
	s.renderPage(w, webPageTitle(s.title, "ci"), body.String())
}

func (s *webServer) listCIRuns(ctx context.Context) ([]brokerCIRun, error) {
	if strings.TrimSpace(s.cfg.brokerURL) == "" || strings.TrimSpace(s.cfg.logicalRepo) == "" {
		return nil, errors.New("CI requires a broker-backed repository")
	}
	var resp struct {
		Runs []brokerCIRun `json:"runs"`
	}
	if err := brokerPostContext(ctx, s.cfg.brokerURL, "/ci/list", brokerCIRequest{Repo: repoForBroker(s.cfg)}, &resp); err != nil {
		return nil, err
	}
	return resp.Runs, nil
}

func (s *webServer) handleIssue(ctx context.Context, w http.ResponseWriter, r *http.Request, idPart string) {
	id, err := strconv.Atoi(strings.Trim(idPart, "/"))
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	issue, err := s.getIssue(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if issue.Type == "story" {
		monogram := repoMonogram(firstNonEmpty(s.cfg.logicalRepo, s.title))
		http.Redirect(w, r, "/board/"+url.PathEscape(storyDisplayID(monogram, issue.ID)), http.StatusFound)
		return
	}
	ref := branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, "issues"))
	body.WriteString(`<div class="repo-content repo-content-single"><div class="repo-primary"><section class="panel issue-detail" data-issue-id="` + strconv.Itoa(issue.ID) + `">`)
	body.WriteString(`<div class="issue-heading"><span class="issue-state ` + html.EscapeString(issue.Status) + `">` + html.EscapeString(strings.ToUpper(firstNonEmpty(issue.Status, "open"))) + `</span><h1>` + html.EscapeString(issue.Title) + `</h1></div>`)
	body.WriteString(`<div class="issue-comment"><strong>` + html.EscapeString(firstNonEmpty(issue.Author, "anonymous")) + ` opened ` + html.EscapeString(relativeTime(parseTime(issue.CreatedAt))) + `</strong><p>` + html.EscapeString(issue.Body) + `</p></div>`)
	for _, comment := range issue.Comments {
		body.WriteString(`<div class="issue-comment"><strong>` + html.EscapeString(firstNonEmpty(comment.User, "anonymous")) + ` commented ` + html.EscapeString(relativeTime(parseTime(comment.At))) + `</strong><p>` + html.EscapeString(comment.Body) + `</p></div>`)
	}
	formAttrs := `class="issue-form" data-issue-form="comment"`
	if issue.Type == "story" {
		formAttrs += ` data-capability="push"`
	}
	body.WriteString(`<form ` + formAttrs + `><label>Comment<textarea name="comment" rows="4" required></textarea></label><div class="settings-actions"><button class="button-link primary" type="submit">Comment</button>`)
	if issue.Status == "closed" {
		body.WriteString(`<button class="button-link" type="button" data-issue-action="reopen">Reopen</button>`)
	} else {
		body.WriteString(`<button class="button-link" type="button" data-issue-action="close">Close</button>`)
	}
	body.WriteString(`</div></form></section></div></div></main>`)
	s.renderPage(w, webPageTitle(s.title, "issue"), body.String())
}

func (s *webServer) handleStory(ctx context.Context, w http.ResponseWriter, r *http.Request, idPart string) {
	boardCtx := s.boardRenderContext(ctx)
	id, err := parseStoryDisplayID(strings.Trim(idPart, "/"), boardCtx.Monogram)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	story, err := s.getIssue(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if story.Type != "story" {
		http.NotFound(w, r)
		return
	}
	ref := branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	lane := normalizeKanbanLane(story.Lane)
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, "board"))
	body.WriteString(`<div class="repo-content repo-content-single"><div class="repo-primary"><section class="panel story-detail" data-story-id="` + strconv.Itoa(story.ID) + `">`)
	body.WriteString(`<div class="issue-heading story-heading"><span class="issue-state">` + html.EscapeString(kanbanLaneLabel(lane)) + `</span><h1>Story ` + html.EscapeString(storyDisplayID(boardCtx.Monogram, story.ID)) + `</h1></div>`)
	body.WriteString(`<div class="story-meta"><span>Created by ` + html.EscapeString(firstNonEmpty(story.Author, "anonymous")) + `</span><span>` + html.EscapeString(relativeTime(parseTime(story.CreatedAt))) + `</span>`)
	if story.UpdatedAt != "" {
		body.WriteString(`<span>Updated ` + html.EscapeString(relativeTime(parseTime(story.UpdatedAt))) + `</span>`)
	}
	if story.Archived {
		body.WriteString(`<span>Archived</span>`)
	}
	body.WriteString(`</div>`)
	body.WriteString(`<div class="story-body">` + html.EscapeString(storyText(story)) + `</div>`)
	body.WriteString(`<div class="story-detail-actions" data-capability="push">`)
	body.WriteString(storyAssignmentControlsHTML(story, boardCtx))
	body.WriteString(`<button class="button-link story-chip" type="button" data-board-action="edit">Edit</button>`)
	body.WriteString(`<select data-board-lane aria-label="Move story">`)
	for _, option := range kanbanLanes() {
		selected := ""
		if option == lane {
			selected = ` selected`
		}
		body.WriteString(`<option value="` + option + `"` + selected + `>` + html.EscapeString(kanbanLaneLabel(option)) + `</option>`)
	}
	body.WriteString(`</select>`)
	if story.Assignee != "" {
		body.WriteString(`<span class="muted">Assigned to ` + html.EscapeString(story.Assignee) + `</span>`)
	}
	if story.Archived {
		body.WriteString(`<button class="button-link story-chip" type="button" data-board-action="unarchive">Unarchive</button>`)
	} else {
		body.WriteString(`<button class="button-link story-chip" type="button" data-board-action="archive">Archive</button>`)
	}
	body.WriteString(`</div>`)
	if len(story.History) > 0 {
		body.WriteString(`<div class="story-history"><h2>Activity</h2>`)
		for _, event := range story.History {
			body.WriteString(`<div><strong>` + html.EscapeString(firstNonEmpty(event.User, "anonymous")) + `</strong> ` + html.EscapeString(storyEventText(event)) + ` <span>` + html.EscapeString(relativeTime(parseTime(event.At))) + `</span></div>`)
		}
		body.WriteString(`</div>`)
	}
	for _, comment := range story.Comments {
		body.WriteString(`<div class="issue-comment"><strong>` + html.EscapeString(firstNonEmpty(comment.User, "anonymous")) + ` commented ` + html.EscapeString(relativeTime(parseTime(comment.At))) + `</strong><p>` + html.EscapeString(comment.Body) + `</p></div>`)
	}
	body.WriteString(`<form class="issue-form" data-board-form="comment" data-capability="push"><label>Comment<textarea name="comment" rows="4" required></textarea></label><div class="settings-actions"><button class="button-link primary" type="submit">Comment</button></div></form>`)
	body.WriteString(`</section></div></div></main>`)
	s.renderPage(w, webPageTitle(s.title, "story"), body.String())
}

func storyEventText(event brokerIssueEvent) string {
	switch event.Action {
	case "created":
		return "created the story"
	case "edited":
		return "edited the story"
	case "commented":
		return "commented"
	case "archived":
		return "archived the story"
	case "unarchived":
		return "unarchived the story"
	case "moved":
		if event.From != "" || event.To != "" {
			return "moved from " + kanbanLaneLabel(normalizeKanbanLane(event.From)) + " to " + kanbanLaneLabel(normalizeKanbanLane(event.To))
		}
		return "moved the story"
	case "reordered":
		return "reordered the story"
	case "assigned":
		if event.To == "" {
			return "unassigned the story"
		}
		return "assigned the story to " + event.To
	default:
		return firstNonEmpty(event.Action, "updated the story")
	}
}

func (s *webServer) boardRenderContext(ctx context.Context) boardRenderContext {
	var out boardRenderContext
	out.Monogram = repoMonogram(firstNonEmpty(s.cfg.logicalRepo, s.title))
	out.Assignees, _ = s.listBoardAssignees(ctx)
	return out
}

func (s *webServer) listBoardAssignees(ctx context.Context) ([]string, error) {
	if strings.TrimSpace(s.cfg.brokerURL) == "" || strings.TrimSpace(s.cfg.logicalRepo) == "" {
		return nil, errors.New("board requires a broker-backed repository")
	}
	var resp struct {
		Users []string `json:"users"`
	}
	if err := brokerPostContext(ctx, s.cfg.brokerURL, "/issues/assignees", brokerIssueRequest{Repo: repoForBroker(s.cfg)}, &resp); err != nil {
		return nil, err
	}
	return resp.Users, nil
}

func (s *webServer) handleSettings(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	info, cached, _ := s.cachedSettingsInfo(ctx, false)
	ref := branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, "settings"))
	body.WriteString(`<div class="repo-content repo-content-single"><div class="repo-primary settings-primary" data-settings-root>`)
	body.WriteString(`<section class="panel settings-panel"><div class="panel-title">Repository settings</div>`)
	if strings.TrimSpace(s.cfg.brokerURL) == "" {
		body.WriteString(`<div class="empty">Settings are available for broker-backed repositories.</div></section>`)
		body.WriteString(`</div></div></main>`)
		s.renderPage(w, webPageTitle(s.title, "settings"), body.String())
		return
	}
	body.WriteString(`<div data-settings-sections data-settings-view="settings">`)
	body.WriteString(s.settingsInitialSectionsHTML(info, "settings", cached))
	body.WriteString(`</div>`)
	body.WriteString(`</section></div></div></main>`)
	s.renderPage(w, webPageTitle(s.title, "settings"), body.String())
}

func (s *webServer) handleUserSettings(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	ref := branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, "user-settings"))
	body.WriteString(`<div class="repo-content repo-content-single"><div class="repo-primary"><section class="panel user-settings-panel" data-user-settings>`)
	body.WriteString(`<div class="panel-title">User profile</div>`)
	if strings.TrimSpace(s.cfg.brokerURL) == "" {
		body.WriteString(`<div class="empty">User profiles are available for broker-backed repositories.</div></section></div></div></main>`)
		s.renderPage(w, webPageTitle(s.title, "user settings"), body.String())
		return
	}
	body.WriteString(`<div class="user-profile-grid"><div class="user-profile-avatar-block"><div class="user-avatar-preview" data-user-avatar-preview>` + userIconHTML() + `</div><label class="button-link user-avatar-upload">Upload avatar<input type="file" accept="image/*" data-user-avatar-file hidden></label></div>`)
	body.WriteString(`<form class="user-profile-form" data-user-profile-form><label>Bio<textarea name="bio" rows="6" maxlength="2000" placeholder="Short bio"></textarea></label><div class="settings-actions"><button class="button-link primary" type="submit">Save profile</button></div></form></div>`)
	body.WriteString(`<div class="avatar-cropper" data-avatar-cropper hidden><div class="avatar-crop-frame" data-avatar-crop-frame><img alt="" draggable="false" data-avatar-crop-image></div><div class="avatar-zoom-controls"><button class="button-link story-chip" type="button" data-avatar-zoom-out aria-label="Zoom out" title="Zoom out">-</button><button class="button-link story-chip" type="button" data-avatar-zoom-in aria-label="Zoom in" title="Zoom in">+</button></div><div class="settings-actions"><button class="button-link primary" type="button" data-avatar-apply>Use image</button><button class="button-link" type="button" data-avatar-cancel>Cancel</button></div></div>`)
	body.WriteString(`<section class="settings-section"><h2>SSH keys</h2><div data-user-keys><div class="empty">Loading keys...</div></div></section>`)
	body.WriteString(`</section></div></div></main>`)
	s.renderPage(w, webPageTitle(s.title, "user settings"), body.String())
}

func (s *webServer) handleAPIUserProfile(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.cfg.brokerURL) == "" || strings.TrimSpace(s.cfg.logicalRepo) == "" {
		s.renderJSONError(w, http.StatusBadRequest, errors.New("user profile requires a broker-backed repository"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		var resp brokerUserProfileResponse
		if err := brokerPostContext(ctx, s.cfg.brokerURL, "/profile/get", brokerUserProfileRequest{Repo: repoForBroker(s.cfg)}, &resp); err != nil {
			s.renderJSONError(w, http.StatusBadRequest, err)
			return
		}
		s.renderJSON(w, resp)
	case http.MethodPost:
		var req struct {
			Bio    string `json:"bio"`
			Avatar string `json:"avatar"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.renderJSONError(w, http.StatusBadRequest, err)
			return
		}
		var resp brokerUserProfileResponse
		if err := brokerPostContext(ctx, s.cfg.brokerURL, "/profile/update", brokerUserProfileRequest{Repo: repoForBroker(s.cfg), Bio: strings.TrimSpace(req.Bio), Avatar: strings.TrimSpace(req.Avatar)}, &resp); err != nil {
			s.renderJSONError(w, http.StatusBadRequest, err)
			return
		}
		s.renderJSON(w, resp)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *webServer) settingsInitialSectionsHTML(info webSettingsInfo, view string, cached bool) string {
	if cached {
		return s.settingsSectionsHTML(info, view)
	}
	return `<section class="settings-section settings-loading"><h2>Loading broker data</h2><p>Repository administration data is loading in the background.</p></section>`
}

func (s *webServer) settingsSectionsHTML(info webSettingsInfo, view string) string {
	var b strings.Builder
	b.WriteString(s.settingsAboutHTML(info))
	b.WriteString(s.settingsAccessHTML(info))
	b.WriteString(s.settingsBranchesHTML(info))
	b.WriteString(s.settingsPullRequestsHTML(info))
	b.WriteString(s.settingsDangerHTML(info))
	if len(info.Errors) > 0 {
		b.WriteString(`<section class="settings-section"><h2>Unavailable sections</h2>`)
		for name, message := range info.Errors {
			b.WriteString(`<div class="settings-error"><strong>` + html.EscapeString(name) + `</strong> ` + html.EscapeString(message) + `</div>`)
		}
		b.WriteString(`</section>`)
	}
	return b.String()
}

func (s *webServer) settingsAboutHTML(info webSettingsInfo) string {
	var b strings.Builder
	b.WriteString(`<section class="settings-section"><h2>About</h2>`)
	b.WriteString(`<form class="settings-form" data-settings-form="update-repo" data-capability="manage_protection">`)
	b.WriteString(`<label>Repository description<textarea name="description" rows="3" placeholder="Describe this repository">` + html.EscapeString(info.Description) + `</textarea></label>`)
	b.WriteString(`<div class="settings-form-grid"><label>Default branch<input name="default_branch" value="` + html.EscapeString(firstNonEmpty(info.DefaultBranch, defaultBranch)) + `" autocomplete="off"></label><label>Visibility<select name="visibility">`)
	for _, option := range []string{"private", "public"} {
		selected := ""
		if firstNonEmpty(info.Visibility, "private") == option {
			selected = ` selected`
		}
		b.WriteString(`<option value="` + option + `"` + selected + `>` + option + `</option>`)
	}
	readOnlyChecked := ""
	if info.ReadOnly {
		readOnlyChecked = ` checked`
	}
	issuesChecked := ""
	if info.IssuesEnabled {
		issuesChecked = ` checked`
	}
	b.WriteString(`</select></label><label class="settings-check"><input type="checkbox" name="issues_enabled"` + issuesChecked + `> Enable issues</label><label class="settings-check"><input type="checkbox" name="read_only"` + readOnlyChecked + `> Make repository read-only</label></div>`)
	b.WriteString(`<div class="settings-actions"><button class="button-link primary" type="submit">Save about</button></div></form>`)
	b.WriteString(`<div class="settings-meta-grid">`)
	b.WriteString(settingsMetaItem("Repository", logicalRepoDisplayName(firstNonEmpty(info.Repo.Logical, info.Title))))
	b.WriteString(settingsMetaItem("Provider", firstNonEmpty(info.Provider, info.Repo.Provider)))
	b.WriteString(settingsMetaItem("Region", info.Region))
	b.WriteString(settingsMetaItem("Broker", strings.TrimPrefix(strings.TrimPrefix(info.BrokerURL, "https://"), "http://")))
	b.WriteString(`</div></section>`)
	return b.String()
}

func (s *webServer) settingsAccessHTML(info webSettingsInfo) string {
	var b strings.Builder
	b.WriteString(`<section class="settings-section"><h2>Access</h2>`)
	b.WriteString(`<div class="settings-table" data-settings-members>`)
	if len(info.Keys) == 0 && len(info.UserGrants) == 0 {
		b.WriteString(`<div class="empty">No members found.</div>`)
	} else {
		for _, grant := range info.UserGrants {
			user := firstNonEmpty(grant.User, grant.Username, grant.UserID, "unknown")
			role := firstNonEmpty(grant.Role, "read")
			b.WriteString(`<div class="settings-row" data-repo-user="` + html.EscapeString(user) + `" data-repo-user-id="` + html.EscapeString(grant.UserID) + `">`)
			b.WriteString(`<div><strong>` + html.EscapeString(user) + `</strong><span>` + html.EscapeString(role) + ` · broker user grant</span>`)
			if grant.UserID != "" {
				b.WriteString(`<small>` + html.EscapeString(grant.UserID) + `</small>`)
			}
			b.WriteString(`</div><div class="settings-row-actions" data-capability="admin_keys">`)
			b.WriteString(`<button class="button-link" type="button" data-settings-action="remove-repo-user">Remove</button>`)
			b.WriteString(`</div></div>`)
		}
		for _, key := range info.Keys {
			fingerprint := publicKeyFingerprint(key.PublicKey)
			if fingerprint == "" {
				fingerprint = key.PublicKey
			}
			status := "active"
			if key.Suspended {
				status = "suspended"
			}
			b.WriteString(`<div class="settings-row" data-member-key="` + html.EscapeString(fingerprint) + `">`)
			b.WriteString(`<div><strong>` + html.EscapeString(firstNonEmpty(key.User, "unknown")) + `</strong><span>` + html.EscapeString(key.Role) + ` · ` + html.EscapeString(status) + `</span><small>` + html.EscapeString(fingerprint) + `</small>`)
			if key.Source != "" {
				b.WriteString(`<small>` + html.EscapeString(key.Source) + `</small>`)
			}
			b.WriteString(`</div><div class="settings-row-actions" data-capability="admin_keys">`)
			if key.Role == "owner" {
				b.WriteString(`<span class="settings-note">Owner key</span>`)
			} else {
				if key.Suspended {
					b.WriteString(`<button class="button-link" type="button" data-settings-action="unsuspend-member">Unsuspend</button>`)
				} else {
					b.WriteString(`<button class="button-link" type="button" data-settings-action="suspend-member">Suspend</button>`)
				}
				b.WriteString(`<button class="button-link" type="button" data-settings-action="remove-member">Remove</button>`)
			}
			b.WriteString(`</div></div>`)
		}
	}
	b.WriteString(`</div>`)
	b.WriteString(`<form class="settings-form settings-member-form" data-settings-form="add-member" data-capability="admin_keys">`)
	b.WriteString(`<h3>Invite member</h3><div class="settings-form-grid"><label>Username<input name="user" autocomplete="off" required></label><label>Role<select name="role">`)
	for _, role := range []string{"read", "triage", "developer", "maintainer", "admin", "owner"} {
		b.WriteString(`<option value="` + role + `">` + role + `</option>`)
	}
	b.WriteString(`</select></label></div>`)
	b.WriteString(`<div class="settings-actions"><button class="button-link primary" type="submit">Create invite</button></div></form>`)
	b.WriteString(`</section>`)
	return b.String()
}

func (s *webServer) settingsBranchesHTML(info webSettingsInfo) string {
	var b strings.Builder
	b.WriteString(`<section class="settings-section"><h2>Branches</h2><div class="settings-table" data-settings-protections>`)
	if len(info.Protections) == 0 {
		b.WriteString(`<div class="empty">No protected branches.</div>`)
	} else {
		for _, protection := range info.Protections {
			mode := "PR required"
			if protection.AllowOverrides {
				mode += " · owner/admin override"
			}
			b.WriteString(`<div class="settings-row" data-protection-ref="` + html.EscapeString(protection.Ref) + `"><div><strong>` + html.EscapeString(shortRefName(protection.Ref)) + `</strong><span>` + html.EscapeString(mode) + `</span></div><div class="settings-row-actions" data-capability="manage_protection"><button class="button-link" type="button" data-settings-action="protect-remove">Remove</button></div></div>`)
		}
	}
	b.WriteString(`</div><form class="settings-form settings-protection-form" data-settings-form="protect-upsert" data-capability="manage_protection">`)
	b.WriteString(`<h3>Protect branch</h3><div class="settings-form-grid"><label>Branch or ref<input name="ref" value="main" autocomplete="off" required></label><label class="settings-check"><input type="checkbox" name="require_pr" checked> Require pull request</label><label class="settings-check"><input type="checkbox" name="allow_overrides"> Owner/admin override</label></div>`)
	b.WriteString(`<div class="settings-actions"><button class="button-link primary" type="submit">Protect branch</button></div></form></section>`)
	return b.String()
}

func (s *webServer) settingsPullRequestsHTML(info webSettingsInfo) string {
	return `<section class="settings-section"><h2>Pull requests</h2><div class="settings-info-list"><div><strong>Protected branches</strong><span>Branches can require pull requests before updates land.</span></div><div><strong>Review metadata</strong><span>Approvals, requested changes, comments, and inline review threads are stored by the broker.</span></div></div></section>`
}

func (s *webServer) settingsDangerHTML(info webSettingsInfo) string {
	logical := firstNonEmpty(info.Repo.Logical, info.Title)
	var b strings.Builder
	b.WriteString(`<section class="settings-section settings-danger" data-capability="owner_transfer"><h2>Danger Zone</h2><div class="settings-warning">Owner-only repository actions live here. These actions can permanently change or delete repository state.</div>`)
	b.WriteString(`<form class="settings-form settings-member-form" data-settings-form="transfer-owner" data-capability="owner_transfer">`)
	b.WriteString(`<h3>Transfer ownership</h3><p class="settings-note">Creates a one-time accept command for the new owner. Their SSH signature becomes the new owner key.</p>`)
	b.WriteString(`<div class="settings-actions"><button class="button-link" type="submit">Transfer ownership</button></div></form>`)
	b.WriteString(`<form class="settings-form settings-member-form" data-settings-form="repo-rename" data-capability="owner_transfer">`)
	b.WriteString(`<h3>Rename repository</h3><div class="settings-form-grid"><label>New repository name<input name="logical" value="` + html.EscapeString(logicalRepoDisplayName(logical)) + `" autocomplete="off" required></label></div>`)
	b.WriteString(`<div class="settings-actions"><button class="button-link" type="submit">Rename repository</button></div></form>`)
	b.WriteString(`<form class="settings-form settings-member-form" data-settings-form="repo-delete" data-capability="owner_transfer">`)
	b.WriteString(`<h3>Delete repository</h3><p class="settings-note">Deletes broker metadata, bucket contents, and the physical bucket.</p><div class="settings-form-grid"><label>Type the repository name to confirm<input name="confirm" autocomplete="off" required></label></div>`)
	b.WriteString(`<div class="settings-actions"><button class="button-link danger" type="submit" data-confirm-repo="` + html.EscapeString(logicalRepoDisplayName(logical)) + `">Delete repository</button></div></form></section>`)
	return b.String()
}

func settingsMetaItem(label, value string) string {
	if strings.TrimSpace(value) == "" {
		value = "not configured"
	}
	return `<div><span>` + html.EscapeString(label) + `</span><strong>` + html.EscapeString(value) + `</strong></div>`
}

func publicKeyFingerprint(value string) string {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(value)))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(pub)
}

func (s *webServer) handlePullRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, value string) {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		s.renderError(w, http.StatusNotFound, fs.ErrNotExist)
		return
	}
	id, err := strconv.Atoi(parts[0])
	if err != nil || id <= 0 {
		s.renderError(w, http.StatusNotFound, fs.ErrNotExist)
		return
	}
	tab := "conversation"
	if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
		tab = strings.TrimSpace(parts[1])
	}
	pr, err := s.pullRequestByID(ctx, id)
	if err != nil {
		s.renderError(w, http.StatusNotFound, err)
		return
	}
	ref := branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, "prs"))
	body.WriteString(`<div class="repo-content repo-content-single"><div class="repo-primary">`)
	body.WriteString(prHeaderHTML(pr, tab))
	switch tab {
	case "files", "files-changed", "diff":
		body.WriteString(s.pullRequestFilesHTML(ctx, pr))
	case "commits":
		body.WriteString(s.pullRequestCommitsHTML(ctx, pr))
	case "review":
		body.WriteString(s.pullRequestReviewHTML(ctx, pr))
	default:
		body.WriteString(s.pullRequestConversationHTML(ctx, pr))
	}
	body.WriteString(`</div></div></main>`)
	s.renderPage(w, webPageTitle(s.title, fmt.Sprintf("PR #%d", pr.ID)), body.String())
}

func (s *webServer) pullRequestByID(ctx context.Context, id int) (brokerPullRequest, error) {
	if cached, err := s.readPullRequestCache(); err == nil {
		for _, pr := range cached.PRs {
			if pr.ID == id {
				return pr, nil
			}
		}
	}
	if refreshed, err := s.refreshPullRequestCache(ctx); err == nil {
		for _, pr := range refreshed {
			if pr.ID == id {
				return pr, nil
			}
		}
	}
	return brokerPullRequest{}, errors.New("pull request not found")
}

func (s *webServer) pullRequestFilesHTML(ctx context.Context, pr brokerPullRequest) string {
	files, additions, deletions, err := s.pullRequestChangedFiles(ctx, pr)
	if err != nil {
		return `<section class="panel"><div class="empty">` + html.EscapeString(err.Error()) + `</div></section>`
	}
	return diffFilesPanelHTML(files, additions, deletions)
}

func (s *webServer) pullRequestReviewHTML(ctx context.Context, pr brokerPullRequest) string {
	files, additions, deletions, err := s.pullRequestChangedFiles(ctx, pr)
	if err != nil {
		return `<section class="panel"><div class="empty">` + html.EscapeString(err.Error()) + `</div></section>`
	}
	var b strings.Builder
	commentJSON, _ := json.Marshal(prReviewComments(pr))
	b.WriteString(`<script type="application/json" id="pr-review-comments">` + html.EscapeString(string(commentJSON)) + `</script>`)
	b.WriteString(`<section class="panel pr-review-workspace" data-pr-review-workspace data-pr-id="` + strconv.Itoa(pr.ID) + `"><div class="panel-title">Review changes</div><div class="muted review-help">Hover a new-side line or file header to add review comments. Submit them together at the bottom.</div></section>`)
	b.WriteString(`<div class="pr-review-diff" data-pr-review-diff data-pr-id="` + strconv.Itoa(pr.ID) + `">`)
	b.WriteString(diffFilesPanelHTMLWithOptions(files, additions, deletions, webDiffRenderOptions{Review: true, PRID: pr.ID}))
	b.WriteString(`</div><form class="pr-review-submit" data-pr-review-submit data-pr-id="` + strconv.Itoa(pr.ID) + `"><label for="pr-review-note">Review note</label><textarea id="pr-review-note" data-pr-review-note rows="3" placeholder="Leave an optional review note"></textarea><div class="pr-review-actions"><a class="button-link" href="/prs/` + strconv.Itoa(pr.ID) + `" data-review-cancel>Cancel review</a><button type="button" class="button-link" data-pr-review-action="comment" data-capability="comment">Finish review</button><button type="button" class="button-link" data-pr-review-action="approve" data-capability="approve">Approve</button><button type="button" class="button-link" data-pr-review-action="reject" data-capability="review">Request changes</button></div></form>`)
	return b.String()
}

func prReviewComments(pr brokerPullRequest) []brokerPullRequestComment {
	var comments []brokerPullRequestComment
	for _, review := range pr.Reviews {
		comments = append(comments, review.Comments...)
	}
	return comments
}

func (s *webServer) pullRequestChangedFiles(ctx context.Context, pr brokerPullRequest) ([]webChangedFile, int, int, error) {
	targetRef := firstNonEmpty(pr.Target, branchRef(defaultBranch))
	sourceRef := firstNonEmpty(pr.Source, pr.Head)
	return s.pullRequestChangedFilesForRefs(ctx, targetRef, sourceRef, pr.Head)
}

func (s *webServer) pullRequestChangedFilesForRefs(ctx context.Context, targetRef, sourceRef, sourceFallback string) ([]webChangedFile, int, int, error) {
	repo := s
	targetHash, targetErr := repo.resolvePullRequestRevision(ctx, targetRef, "")
	sourceHash, sourceErr := repo.resolvePullRequestRevision(ctx, sourceRef, sourceFallback)
	if (targetErr != nil || sourceErr != nil) && s.apiRepo != nil && s.apiRepo != s.repo {
		remote := *s
		remote.repo = s.apiRepo
		repo = &remote
		targetHash, targetErr = repo.resolvePullRequestRevision(ctx, targetRef, "")
		sourceHash, sourceErr = repo.resolvePullRequestRevision(ctx, sourceRef, sourceFallback)
	}
	if targetErr != nil || sourceErr != nil {
		return nil, 0, 0, errors.New("pull request refs are not available locally yet. Fetch the source and target branches, then refresh this page")
	}
	targetCommit, err := repo.repo.commit(ctx, targetHash)
	if err != nil {
		return nil, 0, 0, err
	}
	sourceCommit, err := repo.repo.commit(ctx, sourceHash)
	if err != nil {
		return nil, 0, 0, err
	}
	return repo.changedFilesBetweenTrees(ctx, targetCommit.tree, sourceCommit.tree)
}

func (s *webServer) pullRequestPreviewDiffHTML(ctx context.Context, targetRef, sourceRef string) (string, bool, string, error) {
	files, additions, deletions, err := s.pullRequestChangedFilesForRefs(ctx, targetRef, sourceRef, "")
	if err != nil {
		return "", false, "", err
	}
	mergeable, conflict := s.pullRequestMergeability(ctx, targetRef, sourceRef)
	return diffFilesPanelHTML(files, additions, deletions), mergeable, conflict, nil
}

func (s *webServer) pullRequestMergeability(ctx context.Context, targetRef, sourceRef string) (bool, string) {
	repo := s
	targetHash, targetErr := repo.resolvePullRequestRevision(ctx, targetRef, "")
	sourceHash, sourceErr := repo.resolvePullRequestRevision(ctx, sourceRef, "")
	if (targetErr != nil || sourceErr != nil) && s.apiRepo != nil && s.apiRepo != s.repo {
		remote := *s
		remote.repo = s.apiRepo
		repo = &remote
		targetHash, targetErr = repo.resolvePullRequestRevision(ctx, targetRef, "")
		sourceHash, sourceErr = repo.resolvePullRequestRevision(ctx, sourceRef, "")
	}
	if targetErr != nil || sourceErr != nil {
		return false, "refs are not available"
	}
	baseHash, err := repo.mergeBase(ctx, targetHash, sourceHash)
	if err != nil {
		return false, err.Error()
	}
	conflict, err := repo.hasMergeConflict(ctx, baseHash, targetHash, sourceHash)
	if err != nil {
		return false, err.Error()
	}
	if conflict != "" {
		return false, conflict
	}
	return true, ""
}

func (s *webServer) pullRequestUnifiedDiff(ctx context.Context, id int) (string, error) {
	pr, err := s.pullRequestByID(ctx, id)
	if err != nil {
		return "", err
	}
	repo := s
	targetRef := firstNonEmpty(pr.Target, branchRef(defaultBranch))
	sourceRef := firstNonEmpty(pr.Source, pr.Head)
	targetHash, targetErr := repo.resolvePullRequestRevision(ctx, targetRef, "")
	sourceHash, sourceErr := repo.resolvePullRequestRevision(ctx, sourceRef, pr.Head)
	if (targetErr != nil || sourceErr != nil) && s.apiRepo != nil && s.apiRepo != s.repo {
		remote := *s
		remote.repo = s.apiRepo
		repo = &remote
		targetHash, targetErr = repo.resolvePullRequestRevision(ctx, targetRef, "")
		sourceHash, sourceErr = repo.resolvePullRequestRevision(ctx, sourceRef, pr.Head)
	}
	if targetErr != nil || sourceErr != nil {
		return "", errors.New("pull request refs are not available")
	}
	targetCommit, err := repo.repo.commit(ctx, targetHash)
	if err != nil {
		return "", err
	}
	sourceCommit, err := repo.repo.commit(ctx, sourceHash)
	if err != nil {
		return "", err
	}
	files, _, _, err := repo.changedFilesBetweenTrees(ctx, targetCommit.tree, sourceCommit.tree)
	if err != nil {
		return "", err
	}
	return changedFilesUnifiedDiff(files), nil
}

func changedFilesUnifiedDiff(files []webChangedFile) string {
	var b strings.Builder
	for _, file := range files {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", file.path, file.path)
		leftShort := "0000000"
		rightShort := "0000000"
		if file.oldHash != "" {
			leftShort = shortHash(file.oldHash)
		}
		if file.newHash != "" {
			rightShort = shortHash(file.newHash)
		}
		fmt.Fprintf(&b, "index %s..%s 100644\n", leftShort, rightShort)
		if file.oldHash == "" {
			fmt.Fprintln(&b, "new file mode 100644")
			fmt.Fprintln(&b, "--- /dev/null")
			fmt.Fprintf(&b, "+++ b/%s\n", file.path)
		} else if file.newHash == "" {
			fmt.Fprintln(&b, "deleted file mode 100644")
			fmt.Fprintf(&b, "--- a/%s\n", file.path)
			fmt.Fprintln(&b, "+++ /dev/null")
		} else {
			fmt.Fprintf(&b, "--- a/%s\n", file.path)
			fmt.Fprintf(&b, "+++ b/%s\n", file.path)
		}
		if file.binary {
			fmt.Fprintln(&b, "Binary file changed")
			continue
		}
		for _, line := range file.diff {
			fmt.Fprintln(&b, line.text)
		}
	}
	return b.String()
}

func (s *webServer) pullRequestCommitsHTML(ctx context.Context, pr brokerPullRequest) string {
	repo := s
	targetRef := firstNonEmpty(pr.Target, branchRef(defaultBranch))
	sourceRef := firstNonEmpty(pr.Source, pr.Head)
	targetHash, targetErr := repo.resolvePullRequestRevision(ctx, targetRef, "")
	sourceHash, sourceErr := repo.resolvePullRequestRevision(ctx, sourceRef, pr.Head)
	if (targetErr != nil || sourceErr != nil) && s.apiRepo != nil && s.apiRepo != s.repo {
		remote := *s
		remote.repo = s.apiRepo
		repo = &remote
		targetHash, targetErr = repo.resolvePullRequestRevision(ctx, targetRef, "")
		sourceHash, sourceErr = repo.resolvePullRequestRevision(ctx, sourceRef, pr.Head)
	}
	if sourceErr != nil {
		return `<section class="panel"><div class="empty">Pull request source branch is not available.</div></section>`
	}
	commits, err := repo.commitRange(ctx, targetHash, sourceHash, 50)
	if err != nil {
		return `<section class="panel"><div class="empty">` + html.EscapeString(err.Error()) + `</div></section>`
	}
	return `<section class="panel"><div class="panel-title">Commits</div>` + s.commitListHTML(ctx, commits, firstNonEmpty(pr.Source, pr.Head), false, "") + `</section>`
}

func (s *webServer) resolvePullRequestRevision(ctx context.Context, ref, fallbackHash string) (string, error) {
	ref = strings.TrimSpace(ref)
	var candidates []string
	if ref != "" {
		candidates = append(candidates, ref)
		short := shortRefName(ref)
		if short != "" && short != ref {
			candidates = append(candidates,
				short,
				"refs/remotes/bucketgit/"+short,
				"refs/remotes/origin/"+short,
			)
		}
	}
	if fallbackHash != "" {
		candidates = append(candidates, fallbackHash)
	}
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		if hash, err := s.repo.resolveRevision(ctx, candidate); err == nil {
			return hash, nil
		}
	}
	return "", fs.ErrNotExist
}

func (s *webServer) mergeBase(ctx context.Context, a, b string) (string, error) {
	ancestors := map[string]struct{}{}
	stack := []string{a}
	for len(stack) > 0 {
		hash := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := ancestors[hash]; ok {
			continue
		}
		ancestors[hash] = struct{}{}
		commit, err := s.repo.commit(ctx, hash)
		if err != nil {
			return "", err
		}
		stack = append(stack, commit.parents...)
	}
	stack = []string{b}
	seen := map[string]struct{}{}
	for len(stack) > 0 {
		hash := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := ancestors[hash]; ok {
			return hash, nil
		}
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		commit, err := s.repo.commit(ctx, hash)
		if err != nil {
			return "", err
		}
		stack = append(stack, commit.parents...)
	}
	return "", errors.New("no merge base found")
}

func (s *webServer) hasMergeConflict(ctx context.Context, baseHash, targetHash, sourceHash string) (string, error) {
	baseCommit, err := s.repo.commit(ctx, baseHash)
	if err != nil {
		return "", err
	}
	targetCommit, err := s.repo.commit(ctx, targetHash)
	if err != nil {
		return "", err
	}
	sourceCommit, err := s.repo.commit(ctx, sourceHash)
	if err != nil {
		return "", err
	}
	base := map[string]webTreeFile{}
	target := map[string]webTreeFile{}
	source := map[string]webTreeFile{}
	if err := s.collectTreeFiles(ctx, baseCommit.tree, "", base); err != nil {
		return "", err
	}
	if err := s.collectTreeFiles(ctx, targetCommit.tree, "", target); err != nil {
		return "", err
	}
	if err := s.collectTreeFiles(ctx, sourceCommit.tree, "", source); err != nil {
		return "", err
	}
	paths := map[string]struct{}{}
	for path := range base {
		paths[path] = struct{}{}
	}
	for path := range target {
		paths[path] = struct{}{}
	}
	for path := range source {
		paths[path] = struct{}{}
	}
	for path := range paths {
		b := base[path]
		t := target[path]
		src := source[path]
		targetChanged := t.hash != b.hash
		sourceChanged := src.hash != b.hash
		if targetChanged && sourceChanged && t.hash != src.hash {
			return "merge conflict in " + path, nil
		}
	}
	return "", nil
}

func (s *webServer) commitRange(ctx context.Context, base, head string, limit int) ([]commitObject, error) {
	if head == "" || head == base {
		return nil, nil
	}
	excluded := map[string]struct{}{}
	if base != "" {
		if err := s.markCommitAncestors(ctx, base, excluded); err != nil {
			return nil, err
		}
	}
	seen := map[string]struct{}{}
	var commits []commitObject
	stack := []string{head}
	for len(stack) > 0 {
		hash := stack[0]
		stack = stack[1:]
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		if _, ok := excluded[hash]; ok {
			continue
		}
		commit, err := s.repo.commit(ctx, hash)
		if err != nil {
			return nil, err
		}
		commits = append(commits, commit)
		stack = append(stack, commit.parents...)
	}
	sort.SliceStable(commits, func(i, j int) bool {
		return commits[i].timestamp > commits[j].timestamp
	})
	if limit > 0 && len(commits) > limit {
		commits = commits[:limit]
	}
	return commits, nil
}

func (s *webServer) markCommitAncestors(ctx context.Context, head string, out map[string]struct{}) error {
	stack := []string{head}
	for len(stack) > 0 {
		hash := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := out[hash]; ok {
			continue
		}
		out[hash] = struct{}{}
		commit, err := s.repo.commit(ctx, hash)
		if err != nil {
			return err
		}
		stack = append(stack, commit.parents...)
	}
	return nil
}

func (s *webServer) handleCommit(ctx context.Context, w http.ResponseWriter, r *http.Request, hash string) {
	hash = strings.TrimSpace(strings.Trim(hash, "/"))
	if hash == "" {
		s.renderError(w, http.StatusNotFound, fs.ErrNotExist)
		return
	}
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	http.Redirect(w, r, webCommitURL(hash, ref), http.StatusFound)
}

func (s *webServer) handleBlob(ctx context.Context, w http.ResponseWriter, r *http.Request, repoPath string, raw bool) {
	_, commit, ref, err := s.headCommit(ctx, r)
	if err != nil {
		s.renderError(w, http.StatusNotFound, err)
		return
	}
	repoPath = cleanWebPath(repoPath)
	if repoPath == "" {
		s.renderError(w, http.StatusNotFound, fs.ErrNotExist)
		return
	}
	hash, err := s.repo.findPath(ctx, commit.tree, repoPath)
	if err != nil {
		s.renderError(w, http.StatusNotFound, err)
		return
	}
	obj, err := s.repo.object(ctx, hash)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err)
		return
	}
	if obj.typ == gitObjectTree {
		http.Redirect(w, r, webURL("tree", repoPath, ref), http.StatusFound)
		return
	}
	if raw {
		contentType := mime.TypeByExtension(pathpkg.Ext(repoPath))
		if contentType == "" {
			contentType = http.DetectContentType(obj.data)
		}
		w.Header().Set("Content-Type", contentType)
		w.Write(obj.data)
		return
	}
	var body strings.Builder
	body.WriteString(`<main class="layout" data-bgit-head="` + html.EscapeString(commit.hash) + `" data-bgit-source="seed">`)
	body.WriteString(s.headerHTML(ref, repoPath))
	body.WriteString(`<section class="panel"><div class="blob-toolbar"><div><div class="panel-title">` + html.EscapeString(repoPath) + `</div><div class="muted">` + strconv.Itoa(len(obj.data)) + ` bytes</div></div><div class="actions"><button class="copy-button" data-copy-target="raw-url">Copy raw URL</button><a class="button-link" href="` + html.EscapeString(webURL("raw", repoPath, ref)) + `">Raw</a></div></div>`)
	body.WriteString(`<input id="raw-url" class="sr-only" value="` + html.EscapeString(webURL("raw", repoPath, ref)) + `">`)
	if isTextBlob(obj.data) {
		body.WriteString(`<pre class="blob">` + html.EscapeString(string(obj.data)) + `</pre>`)
	} else {
		body.WriteString(`<div class="empty">Binary file. Use Raw to download the contents.</div>`)
	}
	body.WriteString(`</section></main>`)
	s.renderPage(w, webPageTitle(s.title, repoPath), body.String())
}

func (s *webServer) handleArchiveZip(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	_, commit, ref, err := s.headCommit(ctx, r)
	if err != nil {
		s.renderError(w, http.StatusNotFound, err)
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	rootName := archiveRootName(s.title, ref)
	if err := s.writeZipTree(ctx, zw, commit.tree, rootName); err != nil {
		_ = zw.Close()
		s.renderError(w, http.StatusInternalServerError, err)
		return
	}
	if err := zw.Close(); err != nil {
		s.renderError(w, http.StatusInternalServerError, err)
		return
	}
	filename := anchorID(rootName)
	if filename == "" {
		filename = "bucketgit"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`.zip"`)
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (s *webServer) writeZipTree(ctx context.Context, zw *zip.Writer, treeHash, prefix string) error {
	entries, err := s.repo.treeEntries(ctx, treeHash)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		target := pathpkg.Join(prefix, entry.name)
		if entry.typ == gitObjectTree {
			if _, err := zw.Create(target + "/"); err != nil {
				return err
			}
			if err := s.writeZipTree(ctx, zw, entry.hash, target); err != nil {
				return err
			}
			continue
		}
		obj, err := s.repo.object(ctx, entry.hash)
		if err != nil {
			return err
		}
		writer, err := zw.Create(target)
		if err != nil {
			return err
		}
		if _, err := writer.Write(obj.data); err != nil {
			return err
		}
	}
	return nil
}

func archiveRootName(title, ref string) string {
	name := strings.Trim(strings.TrimSuffix(pathpkg.Base(strings.Trim(title, "/")), ".git"), ".")
	if name == "" || name == "." {
		name = "bucketgit"
	}
	refName := displayRef(ref)
	if refName == "" {
		return name
	}
	return name + "-" + refName
}

func (s *webServer) readmeHTML(ctx context.Context, treeHash string) string {
	entries, err := s.repo.treeEntries(ctx, treeHash)
	if err != nil {
		return ""
	}
	for _, name := range []string{"README.md", "README", "readme.md", "readme"} {
		for _, entry := range entries {
			if entry.typ != gitObjectBlob || entry.name != name {
				continue
			}
			obj, err := s.repo.object(ctx, entry.hash)
			if err != nil || !isTextBlob(obj.data) {
				return ""
			}
			return `<pre>` + html.EscapeString(string(obj.data)) + `</pre>`
		}
	}
	return ""
}

func (s *webServer) headerHTML(ref, repoPath string) string {
	var b strings.Builder
	b.WriteString(topUserControlsHTML())
	b.WriteString(`<header class="repo-header">`)
	codeActive := ` class="active"`
	commitsActive := ""
	prsActive := ""
	boardActive := ""
	ciActive := ""
	issuesActive := ""
	settingsActive := ""
	if repoPath == "commits" {
		codeActive = ""
		commitsActive = ` class="active"`
	} else if repoPath == "prs" {
		codeActive = ""
		prsActive = ` class="active"`
	} else if repoPath == "settings" {
		codeActive = ""
		settingsActive = ` class="active"`
	} else if repoPath == "board" {
		codeActive = ""
		boardActive = ` class="active"`
	} else if repoPath == "ci" {
		codeActive = ""
		ciActive = ` class="active"`
	} else if repoPath == "issues" {
		codeActive = ""
		issuesActive = ` class="active"`
	}
	b.WriteString(`<div class="tabs-row"><nav class="tabs"><a` + codeActive + ` href="` + html.EscapeString(webURL("tree", "", ref)) + `">Code</a><a` + commitsActive + ` href="` + html.EscapeString("/commits?ref="+urlQueryEscape(ref)) + `">Commits</a>`)
	if s.pullRequestsAvailable() {
		hidden := ""
		count := 0
		if cached, err := s.readPullRequestCache(); err == nil && len(cached.PRs) > 0 {
			count = openPullRequestCount(cached.PRs)
		} else {
			hidden = ` hidden`
		}
		b.WriteString(`<a` + prsActive + ` href="/prs" data-pr-tab` + hidden + `>Pull requests (<span data-pr-tab-count>` + strconv.Itoa(count) + `</span>)</a>`)
	}
	b.WriteString(`<a` + boardActive + ` href="/board">Board</a>`)
	b.WriteString(`<a` + ciActive + ` href="/ci">CI</a>`)
	b.WriteString(`<a` + issuesActive + ` href="/issues">Issues</a>`)
	b.WriteString(`<a` + settingsActive + ` href="/settings" data-capability="read">Settings</a>`)
	b.WriteString(`</nav>`)
	codeActions := ""
	if codeActive != "" {
		codeActions = ` data-code-actions="true"`
	}
	b.WriteString(`<div class="repo-action-control"` + codeActions + `><button class="repo-action-button pull" type="button" data-web-action="pull" data-repo-pull data-capability="read" hidden>PULL</button><button class="repo-action-button uncommit" type="button" data-web-action="uncommit" data-repo-uncommit hidden>UNCOMMIT</button><button class="repo-action-button push" type="button" data-web-action="push" data-repo-push data-capability="push" hidden>PUSH</button><button class="repo-action-button commit" type="button" data-web-action="commit" data-repo-commit hidden>COMMIT</button></div>`)
	if location := s.repoLocationBadge(); location != "" {
		b.WriteString(`<div class="repo-controls"><div class="repo-header-location">` + html.EscapeString(location) + `</div></div>`)
	}
	b.WriteString(`</div>`)
	if banner := s.repoPolicyBannerHTML(); banner != "" {
		b.WriteString(banner)
	}
	b.WriteString(`</header>`)
	return b.String()
}

func (s *webServer) repoPolicyBannerHTML() string {
	if strings.TrimSpace(s.cfg.brokerURL) == "" || strings.TrimSpace(s.cfg.logicalRepo) == "" {
		return ""
	}
	var repoInfo brokerRepoInfoResponse
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := brokerPostContext(ctx, s.cfg.brokerURL, "/repo/info", brokerRepoInfoRequest{Repo: repoForBroker(s.cfg)}, &repoInfo); err != nil {
		return ""
	}
	if repoInfo.ReadOnly {
		return `<div class="repo-policy-banner">This repository has been set to read-only.</div>`
	}
	return ""
}

func (s *webServer) repoToolbarHTML(ref string, includeSearch bool) string {
	branchCount, tagCount := s.refCounts(context.Background())
	var b strings.Builder
	b.WriteString(`<div class="repo-toolbar">`)
	b.WriteString(`<div class="repo-toolbar-left">`)
	b.WriteString(s.refSelectorHTML(ref))
	b.WriteString(`<a class="ref-count" href="#branches" data-focus-ref-selector>` + strconv.Itoa(branchCount) + ` Branch`)
	if branchCount != 1 {
		b.WriteString(`es`)
	}
	b.WriteString(`</a>`)
	b.WriteString(`<a class="ref-count" href="#tags" data-focus-ref-selector>` + strconv.Itoa(tagCount) + ` Tag`)
	if tagCount != 1 {
		b.WriteString(`s`)
	}
	b.WriteString(`</a>`)
	b.WriteString(`</div><div class="repo-toolbar-right">`)
	b.WriteString(remoteSyncHTML())
	if includeSearch {
		b.WriteString(`<div class="file-search"><label><span class="sr-only">Go to file</span><input type="search" data-file-search autocomplete="off" placeholder="Go to file" aria-expanded="false" aria-controls="file-search-results"></label><div class="file-search-results" id="file-search-results" data-file-search-results hidden></div></div>`)
	}
	b.WriteString(s.codeDropdownHTML(ref))
	b.WriteString(`</div></div>`)
	return b.String()
}

type webContributor struct {
	Name string
}

func (s *webServer) repoSidePanelHTML(contributors []webContributor) string {
	repoName := strings.TrimSuffix(strings.Trim(s.title, "/"), ".git")
	if repoName == "" {
		repoName = "this"
	}
	var b strings.Builder
	b.WriteString(`<aside class="repo-side-panel"><section class="side-panel-section repo-identity"><h2>Repository</h2><div class="repo-identity-name">` + html.EscapeString(s.title) + `</div></section><section class="side-panel-section"><h2>About</h2><p>Welcome to the BucketGit ` + html.EscapeString(repoName) + ` repository. BucketGit stores Git repositories directly in cloud buckets, with brokered access, pull requests, branch protection and a lightweight web view for browsing code.</p><p>BucketGit was created by <a href="https://drvink.com/" target="_blank" rel="noopener noreferrer">Dennis Vink</a>.</p><div class="side-links"><a class="icon-link" href="https://github.com/bucketgit/bgit" target="_blank" rel="noopener noreferrer" aria-label="BucketGit on GitHub">` + gitHubIconHTML() + `<span>GitHub</span></a><a class="icon-link" href="https://www.linkedin.com/in/drvink/" target="_blank" rel="noopener noreferrer" aria-label="Dennis Vink on LinkedIn">` + linkedInIconHTML() + `<span>LinkedIn</span></a></div></section><section class="side-panel-section contributors-line"><h2>Contributors <span class="count-badge">` + strconv.Itoa(len(contributors)) + `</span></h2>`)
	if len(contributors) > 0 {
		b.WriteString(`<ol class="contributors-list">`)
		for _, contributor := range contributors {
			b.WriteString(`<li><span>` + html.EscapeString(contributor.Name) + `</span></li>`)
		}
		b.WriteString(`</ol>`)
	}
	b.WriteString(`</section></aside>`)
	return b.String()
}

func linkedInIconHTML() string {
	return `<svg aria-hidden="true" focusable="false" viewBox="0 0 24 24"><path d="M20.45 20.45h-3.56v-5.57c0-1.33-.02-3.04-1.85-3.04-1.86 0-2.14 1.45-2.14 2.95v5.66H9.34V9h3.42v1.56h.05c.48-.9 1.64-1.85 3.37-1.85 3.6 0 4.27 2.37 4.27 5.46v6.28ZM5.32 7.43a2.06 2.06 0 1 1 0-4.12 2.06 2.06 0 0 1 0 4.12Zm1.78 13.02H3.54V9H7.1v11.45ZM22.22 0H1.77C.79 0 0 .77 0 1.72v20.56C0 23.23.79 24 1.77 24h20.45c.98 0 1.78-.77 1.78-1.72V1.72C24 .77 23.2 0 22.22 0Z"></path></svg>`
}

func gitHubIconHTML() string {
	return `<svg aria-hidden="true" focusable="false" viewBox="0 0 24 24"><path d="M12 .3a12 12 0 0 0-3.79 23.39c.6.11.82-.26.82-.58v-2.04c-3.34.73-4.04-1.61-4.04-1.61-.55-1.39-1.34-1.76-1.34-1.76-1.09-.75.08-.73.08-.73 1.2.08 1.84 1.24 1.84 1.24 1.07 1.83 2.81 1.3 3.5.99.11-.78.42-1.3.76-1.6-2.67-.3-5.47-1.33-5.47-5.93 0-1.31.47-2.38 1.24-3.22-.12-.3-.54-1.52.12-3.18 0 0 1.01-.32 3.3 1.23a11.45 11.45 0 0 1 6 0c2.29-1.55 3.3-1.23 3.3-1.23.66 1.66.24 2.88.12 3.18.77.84 1.24 1.91 1.24 3.22 0 4.61-2.81 5.63-5.49 5.93.43.37.81 1.1.81 2.22v3.29c0 .32.22.7.83.58A12 12 0 0 0 12 .3Z"></path></svg>`
}

func diffIconSVGHTML() string {
	return `<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false"><path opacity="0.1" d="M9 6C9 7.65685 7.65685 9 6 9C4.34315 9 3 7.65685 3 6C3 4.34315 4.34315 3 6 3C7.65685 3 9 4.34315 9 6Z" fill="currentColor"/><path opacity="0.1" d="M21 18C21 19.6569 19.6569 21 18 21C16.3431 21 15 19.6569 15 18C15 16.3431 16.3431 15 18 15C19.6569 15 21 16.3431 21 18Z" fill="currentColor"/><path d="M9 6C9 7.65685 7.65685 9 6 9C4.34315 9 3 7.65685 3 6C3 4.34315 4.34315 3 6 3C7.65685 3 9 4.34315 9 6Z" stroke="currentColor" stroke-width="2"/><path d="M21 18C21 19.6569 19.6569 21 18 21C16.3431 21 15 19.6569 15 18C15 16.3431 16.3431 15 18 15C19.6569 15 21 16.3431 21 18Z" stroke="currentColor" stroke-width="2"/><path d="M15 3L12.0605 5.93945C12.0271 5.97289 12.0271 6.02711 12.0605 6.06055L15 9" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/><path d="M9 21L11.9473 18.0527C11.9764 18.0236 11.9764 17.9764 11.9473 17.9473L9 15" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/><path d="M12 6C14.8284 6 16.2426 6 17.1213 6.87868C18 7.75736 18 9.17157 18 12V15" stroke="currentColor" stroke-width="2"/><path d="M12 18C9.17157 18 7.75736 18 6.87868 17.1213C6 16.2426 6 14.8284 6 12L6 9" stroke="currentColor" stroke-width="2"/></svg>`
}

func contributorsFromCommits(commits []commitObject) []webContributor {
	indexes := map[string]int{}
	contributors := []webContributor{}
	for _, commit := range commits {
		key := strings.TrimSpace(strings.ToLower(commit.email))
		if key == "" {
			key = strings.TrimSpace(strings.ToLower(commit.author))
		}
		if key == "" {
			continue
		}
		if _, ok := indexes[key]; ok {
			continue
		}
		name := strings.TrimSpace(commit.author)
		if name == "" {
			name = strings.TrimSpace(commit.email)
		}
		indexes[key] = len(contributors)
		contributors = append(contributors, webContributor{Name: name})
	}
	return contributors
}

func (s *webServer) fileIndexHTML(ctx context.Context, treeHash, ref string) string {
	files := map[string]webTreeFile{}
	if err := s.collectTreeFiles(ctx, treeHash, "", files); err != nil {
		return ""
	}
	dirs := map[string]struct{}{}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
		for dir := pathpkg.Dir(path); dir != "." && dir != ""; dir = pathpkg.Dir(dir) {
			dirs[dir] = struct{}{}
		}
	}
	sort.Strings(paths)
	dirPaths := make([]string, 0, len(dirs))
	for dir := range dirs {
		dirPaths = append(dirPaths, dir)
	}
	sort.Strings(dirPaths)
	maxInt := int(^uint(0) >> 1)
	if len(paths) > maxInt-len(dirPaths) {
		return ""
	}
	indexLen := len(paths) + len(dirPaths)
	index := make([]webFileIndexEntry, 0, indexLen)
	for _, dir := range dirPaths {
		index = append(index, webFileIndexEntry{Path: dir, URL: webURL("tree", dir, ref), Kind: "dir"})
	}
	for _, path := range paths {
		index = append(index, webFileIndexEntry{Path: path, URL: webURL("blob", path, ref), Kind: "file"})
	}
	data, err := json.Marshal(index)
	if err != nil {
		return ""
	}
	return `<script type="application/json" id="bgit-file-index">` + string(data) + `</script>`
}

func themeToggleHTML() string {
	return `<button class="theme-toggle" type="button" data-theme-toggle aria-label="Toggle dark or light theme" title="Toggle theme. Long press for auto."><svg class="theme-symbol" aria-hidden="true" viewBox="0 0 80 80" focusable="false"><circle cx="40" cy="40" r="17"/><path d="M40 0l5.8 19.7H34.2L40 0zM40 80l-5.8-19.7h11.6L40 80zM0 40l19.7-5.8v11.6L0 40zM80 40l-19.7 5.8V34.2L80 40zM11.7 11.7l18 9.8-8.2 8.2-9.8-18zM68.3 68.3l-18-9.8 8.2-8.2 9.8 18zM68.3 11.7l-9.8 18-8.2-8.2 18-9.8zM11.7 68.3l9.8-18 8.2 8.2-18 9.8z"/></svg><span class="theme-auto" aria-hidden="true">A</span></button>`
}

func topUserControlsHTML() string {
	return `<div class="top-user-controls">` + userMenuHTML() + themeToggleHTML() + `</div>`
}

func userMenuHTML() string {
	return `<div class="user-menu"><button class="user-menu-button" type="button" data-user-menu-toggle aria-label="User menu" title="User menu" aria-expanded="false">` + userIconHTML() + `</button><div class="user-menu-popover" data-user-menu hidden><a href="/user/settings/">Settings</a></div></div>`
}

func userIconHTML() string {
	return `<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false"><path d="M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z" stroke="currentColor" stroke-width="2"/><path d="M4 21a8 8 0 0 1 16 0" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>`
}

func userPlusIconHTML() string {
	return `<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false"><path d="M15 19a6 6 0 0 0-12 0" stroke="currentColor" stroke-width="2" stroke-linecap="round"/><path d="M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z" stroke="currentColor" stroke-width="2"/><path d="M19 8v6M16 11h6" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>`
}

func repoIconHTML() string {
	return `<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false"><path d="M6 3h9l3 3v15H6a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2Z" stroke="currentColor" stroke-width="2" stroke-linejoin="round"/><path d="M14 3v4h4M8 12h6M8 16h8" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>`
}

func remoteSyncHTML() string {
	return `<div class="remote-sync" data-remote-sync><button class="remote-refresh is-spinning" type="button" data-remote-refresh disabled aria-label="Refresh remote status" title="Refresh remote status"><svg aria-hidden="true" viewBox="0 0 65 65" focusable="false"><path d="m32.5 4.999c-5.405 0-10.444 1.577-14.699 4.282l-5.75-5.75v16.11h16.11l-6.395-6.395c3.18-1.787 6.834-2.82 10.734-2.82 12.171 0 22.073 9.902 22.073 22.074 0 2.899-0.577 5.664-1.599 8.202l4.738 2.762c1.47-3.363 2.288-7.068 2.288-10.964 0-15.164-12.337-27.501-27.5-27.501z"/><path d="m43.227 51.746c-3.179 1.786-6.826 2.827-10.726 2.827-12.171 0-22.073-9.902-22.073-22.073 0-2.739 0.524-5.35 1.439-7.771l-4.731-2.851c-1.375 3.271-2.136 6.858-2.136 10.622 0 15.164 12.336 27.5 27.5 27.5 5.406 0 10.434-1.584 14.691-4.289l5.758 5.759v-16.112h-16.111l6.389 6.388z"/></svg></button><span class="remote-badge is-syncing" data-remote-sync-badge>Synchronising</span></div>`
}

func boolDataAttr(name string, value bool) string {
	if !value {
		return ""
	}
	return ` data-` + name + `="true"`
}

func (s *webServer) repoLocationBadge() string {
	brokerURL := strings.TrimSpace(s.cfg.brokerURL)
	logicalRepo := strings.Trim(s.cfg.logicalRepo, "/")
	if brokerURL == "" || logicalRepo == "" {
		return ""
	}
	if parsed, err := url.Parse(brokerURL); err == nil && parsed.Host != "" {
		return parsed.Host + brokerClonePath(s.cfg.teamID, logicalRepo)
	}
	return strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(brokerURL, "/"), "https://"), "http://") + brokerClonePath(s.cfg.teamID, logicalRepo)
}

func (s *webServer) refSelectorHTML(ref string) string {
	if s.repo == nil {
		return `<div class="ref-pill">` + html.EscapeString(displayRef(ref)) + `</div>`
	}
	options, err := s.refOptions(context.Background())
	if err != nil || len(options) == 0 {
		return `<div class="ref-pill">` + html.EscapeString(displayRef(ref)) + `</div>`
	}
	selected := normalizeWebRef(ref)
	if selected == "" {
		selected = branchRef(firstNonEmpty(s.cfg.branch, defaultBranch))
	}
	var b strings.Builder
	b.WriteString(`<label class="ref-selector"><span>Branch or tag</span><select data-ref-selector>`)
	matched := false
	currentGroup := ""
	for _, option := range options {
		if option.kind != currentGroup {
			if currentGroup != "" {
				b.WriteString(`</optgroup>`)
			}
			currentGroup = option.kind
			b.WriteString(`<optgroup label="` + html.EscapeString(option.kind) + `">`)
		}
		isSelected := option.fullName == selected
		if isSelected {
			matched = true
		}
		b.WriteString(`<option value="` + html.EscapeString(option.fullName) + `"`)
		if isSelected {
			b.WriteString(` selected`)
		}
		b.WriteString(`>` + html.EscapeString(option.name) + `</option>`)
	}
	if currentGroup != "" {
		b.WriteString(`</optgroup>`)
	}
	if !matched && selected != "" {
		b.WriteString(`<option value="` + html.EscapeString(selected) + `" selected>` + html.EscapeString(displayRef(selected)) + `</option>`)
	}
	b.WriteString(`</select></label>`)
	return b.String()
}

func (s *webServer) refOptions(ctx context.Context) ([]webRefOption, error) {
	refs, err := s.repo.refs(ctx)
	if err != nil {
		return nil, err
	}
	var options []webRefOption
	for ref := range refs {
		switch {
		case strings.HasPrefix(ref, "refs/heads/"):
			options = append(options, webRefOption{name: strings.TrimPrefix(ref, "refs/heads/"), fullName: ref, kind: "Branches"})
		case strings.HasPrefix(ref, "refs/tags/"):
			options = append(options, webRefOption{name: strings.TrimPrefix(ref, "refs/tags/"), fullName: ref, kind: "Tags"})
		}
	}
	sort.SliceStable(options, func(i, j int) bool {
		if options[i].kind != options[j].kind {
			return options[i].kind < options[j].kind
		}
		return options[i].name < options[j].name
	})
	return options, nil
}

func (s *webServer) refCounts(ctx context.Context) (int, int) {
	options, err := s.refOptions(ctx)
	if err != nil {
		return 0, 0
	}
	branches := 0
	tags := 0
	for _, option := range options {
		switch option.kind {
		case "Branches":
			branches++
		case "Tags":
			tags++
		}
	}
	return branches, tags
}

func (s *webServer) codeDropdownHTML(ref string) string {
	widget := s.cloneWidgetHTML(ref)
	if widget == "" {
		return ""
	}
	return `<div class="code-menu"><button class="code-menu-button" type="button" data-code-menu-toggle aria-expanded="false"><span aria-hidden="true">&lt;&gt;</span> Code <span aria-hidden="true">▾</span></button><div class="code-menu-popover" data-code-menu hidden>` + widget + `</div></div>`
}

func (s *webServer) clonePanelHTML() string {
	widget := s.cloneWidgetHTML("")
	if widget == "" {
		return ""
	}
	return `<section class="clone-panel">` + widget + `</section>`
}

func (s *webServer) cloneWidgetHTML(ref string) string {
	origin := firstNonEmpty(s.cfg.origin, originForConfig(s.cfg))
	sshURL := ""
	logicalRepo := strings.Trim(s.cfg.logicalRepo, "/")
	if logicalRepo != "" {
		sshURL = fmt.Sprintf("git@%s:%s", defaultSSHHost, logicalRepo)
	} else if s.cfg.bucket != "" && s.cfg.prefix != "" {
		sshURL = sshRemoteURL(s.cfg)
	}
	options := []cloneOption{}
	if s.cfg.brokerURL != "" && logicalRepo != "" {
		options = append(options, cloneOption{Label: "BGIT", Value: "bgit clone " + brokerCloneCommandURL(s.cfg.brokerURL, s.cfg.teamID, logicalRepo)})
	} else if origin != "" {
		options = append(options, cloneOption{Label: "BGIT", Value: "bgit clone " + origin})
	}
	if sshURL != "" {
		options = append(options, cloneOption{Label: "SSH", Value: sshURL})
	}
	if origin != "" && origin != sshURL {
		options = append(options, cloneOption{Label: "Origin", Value: origin})
	}
	if len(options) == 0 {
		return ""
	}
	return cloneWidgetHTML(options, ref)
}

type cloneOption struct {
	Label string
	Value string
}

func cloneWidgetHTML(options []cloneOption, ref string) string {
	var b strings.Builder
	b.WriteString(`<div class="clone-widget"><div class="clone-head"><div class="panel-title">Clone</div>`)
	if len(options) > 1 {
		b.WriteString(`<div class="clone-tabs" role="tablist">`)
		for i, option := range options {
			active := ""
			selected := "false"
			if i == 0 {
				active = ` class="active"`
				selected = "true"
			}
			id := anchorID("clone-" + option.Label)
			b.WriteString(`<button type="button"` + active + ` data-clone-tab="` + html.EscapeString(id) + `" aria-selected="` + selected + `">` + html.EscapeString(option.Label) + `</button>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div><div class="clone-panes">`)
	for i, option := range options {
		id := anchorID("clone-" + option.Label)
		copyID := "copy-" + anchorID(option.Label+"-"+option.Value)
		hidden := ""
		if i != 0 {
			hidden = ` hidden`
		}
		b.WriteString(`<div class="clone-pane" data-clone-pane="` + html.EscapeString(id) + `"` + hidden + `><code id="` + html.EscapeString(copyID) + `">` + html.EscapeString(option.Value) + `</code><button class="copy-button copy-icon-button" data-copy-icon data-copy-target="` + html.EscapeString(copyID) + `" aria-label="Copy clone command" title="Copy clone command"><span aria-hidden="true">📋</span></button></div>`)
	}
	b.WriteString(`</div>`)
	if ref != "" {
		b.WriteString(`<div class="clone-download"><a href="` + html.EscapeString("/archive.zip?ref="+urlQueryEscape(ref)) + `">Download ZIP</a></div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

func brokerCloneCommandURL(brokerURL, teamID, logicalRepo string) string {
	return strings.TrimRight(strings.TrimSpace(brokerURL), "/") + brokerClonePath(teamID, logicalRepo)
}

func brokerClonePath(teamID, logicalRepo string) string {
	logical := strings.Trim(strings.TrimSpace(logicalRepo), "/")
	team := strings.Trim(strings.TrimSpace(teamID), "/")
	if team == "" {
		return "/" + logical
	}
	if team == coreTeamID {
		team = coreTeamName
	}
	return "/" + team + "/" + logical
}

func (s *webServer) renderPage(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := webAssetString(webPageTemplatePath)
	source := "seed"
	if s.apiRepo == nil || s.apiRepo == s.repo {
		source = "remote"
	}
	page = strings.ReplaceAll(page, "{{TITLE}}", html.EscapeString(title))
	page = strings.ReplaceAll(page, "{{CSS}}", webAssetString(webCSSPath))
	page = strings.ReplaceAll(page, "{{SOURCE}}", source)
	page = strings.ReplaceAll(page, "{{CSRF}}", html.EscapeString(s.csrfToken))
	page = strings.ReplaceAll(page, "{{BODY}}", body)
	page = strings.ReplaceAll(page, "{{WHOAMI}}", s.cachedWhoamiJSON())
	page = strings.ReplaceAll(page, "{{JS}}", webAssetString(webJSPath))
	fmt.Fprint(w, page)
}

func (s *webServer) renderError(w http.ResponseWriter, status int, err error) {
	w.WriteHeader(status)
	s.renderPage(w, fmt.Sprintf("%d", status), `<main class="layout"><section><div class="empty">`+html.EscapeString(err.Error())+`</div></section></main>`)
}

func (s *webServer) renderJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}

func (s *webServer) renderJSONError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func webAPICommitFromCommit(commit commitObject) webAPICommit {
	return webAPICommit{
		Hash:      commit.hash,
		ShortHash: shortHash(commit.hash),
		Subject:   firstNonEmpty(commit.subject, shortHash(commit.hash)),
		Body:      commit.body,
		Author:    commit.author,
		Email:     commit.email,
		Timestamp: commit.timestamp,
		Parents:   commit.parents,
		Tree:      commit.tree,
	}
}

func webAPICommitsFromCommits(commits []commitObject) []webAPICommit {
	out := make([]webAPICommit, 0, len(commits))
	for _, commit := range commits {
		out = append(out, webAPICommitFromCommit(commit))
	}
	return out
}

func webAPIChangedFiles(files []webChangedFile) []map[string]any {
	out := make([]map[string]any, 0, len(files))
	for _, file := range files {
		lines := make([]map[string]string, 0, len(file.diff))
		for _, line := range file.diff {
			lines = append(lines, map[string]string{"kind": line.kind, "text": line.text})
		}
		out = append(out, map[string]any{
			"path":      file.path,
			"old_hash":  file.oldHash,
			"new_hash":  file.newHash,
			"additions": file.additions,
			"deletions": file.deletions,
			"binary":    file.binary,
			"diff":      lines,
		})
	}
	return out
}

func (s *webServer) pullRequestsAvailable() bool {
	if strings.TrimSpace(s.cfg.brokerURL) != "" && strings.TrimSpace(s.cfg.logicalRepo) != "" {
		return true
	}
	if cached, err := s.readPullRequestCache(); err == nil && len(cached.PRs) > 0 {
		return true
	}
	return false
}

func (s *webServer) pullRequestCachePath() string {
	if s.localGitDir == "" {
		return ""
	}
	return filepath.Join(s.localGitDir, "bucketgit", "cache", "prs.json")
}

func (s *webServer) readPullRequestCache() (webPullRequestCache, error) {
	path := s.pullRequestCachePath()
	if path == "" {
		return webPullRequestCache{}, fs.ErrNotExist
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return webPullRequestCache{}, err
	}
	var cache webPullRequestCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return webPullRequestCache{}, err
	}
	if cache.PRs == nil {
		cache.PRs = []brokerPullRequest{}
	}
	return cache, nil
}

func (s *webServer) writePullRequestCache(prs []brokerPullRequest) error {
	path := s.pullRequestCachePath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(webPullRequestCache{UpdatedAt: time.Now().Unix(), PRs: prs}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	return os.WriteFile(path, data, 0o644)
}

func (s *webServer) upsertPullRequestCache(pr brokerPullRequest) []brokerPullRequest {
	if pr.ID <= 0 {
		if cached, err := s.readPullRequestCache(); err == nil {
			return cached.PRs
		}
		return nil
	}
	cache, err := s.readPullRequestCache()
	if err != nil {
		cache.PRs = []brokerPullRequest{}
	}
	found := false
	for i := range cache.PRs {
		if cache.PRs[i].ID == pr.ID {
			cache.PRs[i] = pr
			found = true
			break
		}
	}
	if !found {
		cache.PRs = append(cache.PRs, pr)
	}
	sort.SliceStable(cache.PRs, func(i, j int) bool {
		return cache.PRs[i].ID > cache.PRs[j].ID
	})
	_ = s.writePullRequestCache(cache.PRs)
	return cache.PRs
}

func (s *webServer) refreshPullRequestCache(ctx context.Context) ([]brokerPullRequest, error) {
	if strings.TrimSpace(s.cfg.brokerURL) == "" || strings.TrimSpace(s.cfg.logicalRepo) == "" {
		return nil, errors.New("broker pull requests unavailable")
	}
	known := map[string]string{}
	cached, cacheErr := s.readPullRequestCache()
	if cacheErr == nil {
		for _, pr := range cached.PRs {
			if pr.ID > 0 && strings.TrimSpace(pr.Version) != "" {
				known[strconv.Itoa(pr.ID)] = pr.Version
			}
		}
	}
	var resp struct {
		PRs     []brokerPullRequest `json:"prs"`
		Deleted []int               `json:"deleted"`
	}
	if err := brokerPostContext(ctx, s.cfg.brokerURL, "/prs/sync", brokerPullRequestRequest{Repo: repoForBroker(s.cfg), Known: known}, &resp); err != nil {
		if !strings.Contains(err.Error(), "unknown broker endpoint") {
			return nil, err
		}
		if listErr := brokerPostContext(ctx, s.cfg.brokerURL, "/prs/list", brokerPullRequestRequest{Repo: repoForBroker(s.cfg)}, &resp); listErr != nil {
			return nil, err
		}
	}
	if resp.PRs == nil {
		resp.PRs = []brokerPullRequest{}
	}
	prs := mergePullRequestCache(cached.PRs, resp.PRs, resp.Deleted)
	_ = s.writePullRequestCache(prs)
	return prs, nil
}

func mergePullRequestCache(cached, changed []brokerPullRequest, deleted []int) []brokerPullRequest {
	byID := map[int]brokerPullRequest{}
	for _, pr := range cached {
		if pr.ID > 0 {
			byID[pr.ID] = pr
		}
	}
	for _, id := range deleted {
		delete(byID, id)
	}
	for _, pr := range changed {
		if pr.ID > 0 {
			byID[pr.ID] = pr
		}
	}
	prs := make([]brokerPullRequest, 0, len(byID))
	for _, pr := range byID {
		prs = append(prs, pr)
	}
	sort.SliceStable(prs, func(i, j int) bool {
		return prs[i].ID > prs[j].ID
	})
	return prs
}

func webAPIPullRequests(prs []brokerPullRequest) []map[string]any {
	out := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		out = append(out, map[string]any{
			"id":        pr.ID,
			"title":     pr.Title,
			"body":      pr.Body,
			"source":    pr.Source,
			"target":    pr.Target,
			"status":    pr.Status,
			"author":    pr.Author,
			"approvals": pr.Approvals,
			"checks":    pr.Checks,
			"head":      pr.Head,
			"comments":  pr.Comments,
			"reviews":   pr.Reviews,
		})
	}
	return out
}

func openPullRequestCount(prs []brokerPullRequest) int {
	count := 0
	for _, pr := range prs {
		if pullRequestIsOpen(pr) {
			count++
		}
	}
	return count
}

func pullRequestIsOpen(pr brokerPullRequest) bool {
	return strings.EqualFold(firstNonEmpty(pr.Status, "open"), "open")
}

func pullRequestListHTML(prs []brokerPullRequest) string {
	if len(prs) == 0 {
		return `<div class="empty">No pull requests found.</div>`
	}
	var b strings.Builder
	b.WriteString(`<ul class="pr-list">`)
	for _, pr := range prs {
		status := firstNonEmpty(pr.Status, "open")
		title := firstNonEmpty(pr.Title, "Untitled pull request")
		prURL := `/prs/` + strconv.Itoa(pr.ID)
		b.WriteString(`<li class="pr-item" data-pr-href="` + prURL + `"><div class="pr-main"><div><a class="pr-title" href="` + prURL + `"><span class="pr-id">#` + strconv.Itoa(pr.ID) + `</span> <strong>` + html.EscapeString(title) + `</strong></a></div><div class="muted">` + html.EscapeString(shortRefName(pr.Source)) + ` → ` + html.EscapeString(shortRefName(pr.Target)) + `</div></div><div class="pr-meta"><span class="pr-status">` + html.EscapeString(status) + `</span>`)
		if pr.Approvals > 0 {
			b.WriteString(`<span class="pr-approvals">` + strconv.Itoa(pr.Approvals) + ` approval`)
			if pr.Approvals != 1 {
				b.WriteString(`s`)
			}
			b.WriteString(`</span>`)
		}
		b.WriteString(`</div></li>`)
	}
	b.WriteString(`</ul>`)
	return b.String()
}

func issueListHTML(issues []brokerIssue) string {
	var b strings.Builder
	b.WriteString(`<div class="issue-list">`)
	count := 0
	for _, issue := range issues {
		if issue.Type == "story" {
			continue
		}
		count++
		status := firstNonEmpty(issue.Status, "open")
		b.WriteString(`<a class="issue-row" href="/issues/` + strconv.Itoa(issue.ID) + `"><span class="issue-state ` + html.EscapeString(status) + `">` + html.EscapeString(strings.ToUpper(status)) + `</span><span><strong>` + html.EscapeString(firstNonEmpty(issue.Title, "Untitled issue")) + `</strong><small>#` + strconv.Itoa(issue.ID) + ` opened by ` + html.EscapeString(firstNonEmpty(issue.Author, "anonymous")) + `</small></span></a>`)
	}
	if count == 0 {
		return `<div class="empty">No issues found.</div>`
	}
	b.WriteString(`</div>`)
	return b.String()
}

func ciRunFormHTML(cfg config) string {
	provider := defaultCIProvider(cfg)
	ref := shortRefName(firstNonEmpty(cfg.branch, defaultBranch))
	config := ""
	if detected, err := detectCIConfig(provider, "HEAD"); err == nil {
		config = detected
	}
	var b strings.Builder
	b.WriteString(`<form class="ci-run-form settings-form" data-ci-form data-capability="push">`)
	b.WriteString(`<h3>Run CI</h3><div class="settings-form-grid">`)
	b.WriteString(`<label>Provider<select name="provider"><option value="gcp"`)
	if provider == "gcp" {
		b.WriteString(` selected`)
	}
	b.WriteString(`>GCP Cloud Build</option><option value="aws"`)
	if provider == "aws" {
		b.WriteString(` selected`)
	}
	b.WriteString(`>AWS CodeBuild</option></select></label>`)
	b.WriteString(`<label>Branch or ref<input name="ref" value="` + html.EscapeString(ref) + `" autocomplete="off" required></label>`)
	b.WriteString(`<label>Config<input name="config" value="` + html.EscapeString(config) + `" placeholder="cloudbuild.yaml or buildspec.yml" autocomplete="off"></label>`)
	b.WriteString(`</div><div class="settings-actions"><button class="button-link primary" type="submit">Run CI</button></div></form>`)
	return b.String()
}

func ciRunListHTML(runs []brokerCIRun) string {
	if len(runs) == 0 {
		return `<div class="empty">No CI runs yet.</div>`
	}
	var b strings.Builder
	b.WriteString(`<div class="ci-run-list" data-ci-runs>`)
	for _, run := range runs {
		status := firstNonEmpty(run.Status, "queued")
		b.WriteString(`<div class="ci-run-row" data-ci-run-id="` + strconv.Itoa(run.ID) + `" data-ci-status="` + html.EscapeString(status) + `"><div><strong>#` + strconv.Itoa(run.ID) + ` ` + html.EscapeString(status) + `</strong><span>` + html.EscapeString(strings.ToUpper(firstNonEmpty(run.Provider, "ci"))) + ` · ` + html.EscapeString(shortRefName(run.Ref)) + ` · ` + html.EscapeString(shortHash(run.Commit)) + ` · ` + html.EscapeString(run.Config) + `</span>`)
		if run.Message != "" {
			b.WriteString(`<small>` + html.EscapeString(run.Message) + `</small>`)
		}
		b.WriteString(`<pre class="ci-run-log" data-ci-log hidden></pre>`)
		b.WriteString(`</div>`)
		if run.URL != "" {
			b.WriteString(`<a class="button-link" href="` + html.EscapeString(run.URL) + `" target="_blank" rel="noreferrer">Open</a>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

func boardHTML(issues []brokerIssue, ctx boardRenderContext, archived bool) string {
	var stories []brokerIssue
	for _, issue := range issues {
		if issue.Type == "story" && issue.Archived == archived {
			stories = append(stories, issue)
		}
	}
	sortBoardStories(stories)
	assigneeNames := append([]string{}, ctx.Assignees...)
	for _, story := range stories {
		if strings.TrimSpace(story.Assignee) != "" {
			assigneeNames = append(assigneeNames, story.Assignee)
		}
	}
	assigneeMonograms := uniqueUserMonograms(assigneeNames)
	var b strings.Builder
	if archived {
		b.WriteString(`<div class="archived-stories">`)
		if len(stories) == 0 {
			b.WriteString(`<div class="empty">No archived stories.</div>`)
		}
		for _, story := range stories {
			storyKey := storyDisplayID(ctx.Monogram, story.ID)
			b.WriteString(`<article class="story-card archived-story" data-story-id="` + strconv.Itoa(story.ID) + `" data-story-lane="` + html.EscapeString(normalizeKanbanLane(story.Lane)) + `" data-story-assignee="` + html.EscapeString(story.Assignee) + `"><div class="story-card-title"><a href="/board/` + url.PathEscape(storyKey) + `">` + html.EscapeString(storyKey) + `</a><div class="story-actions" data-capability="push">`)
			if story.Assignee != "" {
				b.WriteString(`<span class="story-assignee-mark" title="Assigned to ` + html.EscapeString(story.Assignee) + `" aria-label="Assigned to ` + html.EscapeString(story.Assignee) + `">` + html.EscapeString(assigneeMonogram(story.Assignee, assigneeMonograms)) + `</span>`)
			}
			b.WriteString(`<button class="button-link story-chip" type="button" data-board-action="unarchive">Unarchive</button></div></div><p>` + html.EscapeString(storyText(story)) + `</p></article>`)
		}
		b.WriteString(`</div>`)
		return b.String()
	}
	b.WriteString(`<div class="kanban-board">`)
	for _, lane := range kanbanLanes() {
		b.WriteString(`<section class="kanban-lane" data-board-drop-lane="` + html.EscapeString(lane) + `"><h3>` + html.EscapeString(kanbanLaneLabel(lane)) + `</h3>`)
		if lane == "backlog" {
			b.WriteString(`<button class="story-card story-card-add" type="button" data-board-action="new" data-capability="push" aria-label="Add story"><span aria-hidden="true">+</span></button>`)
		}
		for _, story := range stories {
			if normalizeKanbanLane(story.Lane) != lane {
				continue
			}
			storyKey := storyDisplayID(ctx.Monogram, story.ID)
			b.WriteString(`<article class="story-card" data-story-id="` + strconv.Itoa(story.ID) + `" data-story-lane="` + html.EscapeString(lane) + `" data-story-assignee="` + html.EscapeString(story.Assignee) + `" draggable="true"><div class="story-card-title"><a href="/board/` + url.PathEscape(storyKey) + `">` + html.EscapeString(storyKey) + `</a><div class="story-actions" data-capability="push">`)
			if story.Assignee != "" {
				b.WriteString(`<span class="story-assignee-mark" title="Assigned to ` + html.EscapeString(story.Assignee) + `" aria-label="Assigned to ` + html.EscapeString(story.Assignee) + `">` + html.EscapeString(assigneeMonogram(story.Assignee, assigneeMonograms)) + `</span>`)
			}
			b.WriteString(storyAssignmentControlsHTML(story, ctx))
			b.WriteString(`<button class="button-link story-chip story-icon-chip" type="button" data-board-action="edit" aria-label="Edit story" title="Edit story">` + editStoryIconHTML() + `</button>`)
			b.WriteString(`<button class="button-link story-chip story-icon-chip" type="button" data-board-action="archive" aria-label="Archive story" title="Archive story">` + archiveStoryIconHTML() + `</button>`)
			b.WriteString(`<button class="button-link story-chip story-icon-chip" type="button" data-board-move-toggle aria-label="Move story" title="Move story">` + moveStoryIconHTML() + `</button>`)
			b.WriteString(`<select data-board-lane aria-label="Move story" hidden>`)
			for _, option := range kanbanLanes() {
				selected := ""
				if option == lane {
					selected = ` selected`
				}
				b.WriteString(`<option value="` + option + `"` + selected + `>` + html.EscapeString(kanbanLaneLabel(option)) + `</option>`)
			}
			b.WriteString(`</select></div></div><p>` + html.EscapeString(storyText(story)) + `</p></article>`)
		}
		b.WriteString(`</section>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

func editStoryIconHTML() string {
	return `<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false"><path d="M4 20h4l11-11a2.5 2.5 0 0 0-4-4L4 16v4Z" stroke="currentColor" stroke-width="2" stroke-linejoin="round"/><path d="m13 6 5 5" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>`
}

func archiveStoryIconHTML() string {
	return `<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false"><path d="M4 7h16v13H4V7Z" stroke="currentColor" stroke-width="2" stroke-linejoin="round"/><path d="M3 4h18v3H3V4Z" stroke="currentColor" stroke-width="2" stroke-linejoin="round"/><path d="M9 11h6" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>`
}

func storyAssignmentControlsHTML(story brokerIssue, ctx boardRenderContext) string {
	assignee := strings.TrimSpace(story.Assignee)
	if assignee != "" {
		var b strings.Builder
		b.WriteString(`<button class="button-link story-chip story-icon-chip" type="button" data-board-reassign-toggle aria-label="Reassign story" title="Reassign story">` + reassignStoryIconHTML() + `</button>`)
		b.WriteString(`<select data-board-reassign aria-label="Reassign story" hidden>`)
		b.WriteString(`<option value="__unassigned__">Unassigned</option>`)
		for _, user := range sortedBoardAssignees(ctx.Assignees) {
			user = strings.TrimSpace(user)
			if user == "" {
				continue
			}
			selected := ""
			if strings.EqualFold(user, assignee) {
				selected = ` selected`
			}
			b.WriteString(`<option value="` + html.EscapeString(user) + `"` + selected + `>` + html.EscapeString(user) + `</option>`)
		}
		b.WriteString(`</select>`)
		return b.String()
	}
	return `<button class="button-link story-chip" type="button" data-board-action="take">Take</button>`
}

func moveStoryIconHTML() string {
	return `<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false"><path d="M5 12h14" stroke="currentColor" stroke-width="2" stroke-linecap="round"/><path d="m14 7 5 5-5 5" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>`
}

func reassignStoryIconHTML() string {
	return `<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false"><path d="M8 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z" stroke="currentColor" stroke-width="2"/><path d="M3 21a5 5 0 0 1 10 0" stroke="currentColor" stroke-width="2" stroke-linecap="round"/><path d="M16 8h5" stroke="currentColor" stroke-width="2" stroke-linecap="round"/><path d="m19 5 3 3-3 3" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/><path d="M21 16h-5" stroke="currentColor" stroke-width="2" stroke-linecap="round"/><path d="m18 13-3 3 3 3" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>`
}

func sortedBoardAssignees(users []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, user := range users {
		user = strings.TrimSpace(user)
		key := strings.ToLower(user)
		if user == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, user)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

func uniqueUserMonograms(users []string) map[string]string {
	sorted := sortedBoardAssignees(users)
	baseGroups := map[string][]string{}
	for _, user := range sorted {
		base := userMonogram(user)
		if base == "" {
			continue
		}
		baseGroups[base] = append(baseGroups[base], user)
	}
	out := map[string]string{}
	for base, group := range baseGroups {
		if len(group) == 1 {
			out[strings.ToLower(group[0])] = base
			continue
		}
		used := map[string]bool{}
		assigned := map[string]bool{}
		pending := make([]string, 0, len(group))
		for _, user := range group {
			label := uniqueNumericSuffixMonogram(user, group)
			if label != "" && !used[label] {
				used[label] = true
				out[strings.ToLower(user)] = label
				assigned[strings.ToLower(user)] = true
				continue
			}
			if label != "" {
				pending = append(pending, user)
			}
		}
		var remaining []string
		for _, user := range group {
			if !assigned[strings.ToLower(user)] {
				remaining = append(remaining, user)
			}
		}
		for _, user := range remaining {
			if containsStringFold(pending, user) {
				continue
			}
			label := uniqueUserPrefix(user, remaining)
			if label == "" || used[label] {
				pending = append(pending, user)
				continue
			}
			used[label] = true
			out[strings.ToLower(user)] = label
			assigned[strings.ToLower(user)] = true
		}
		counter := 1
		for _, user := range pending {
			if assigned[strings.ToLower(user)] {
				continue
			}
			label := numberedUserMonogram(user, counter)
			for used[label] {
				counter++
				label = numberedUserMonogram(user, counter)
			}
			counter++
			used[label] = true
			out[strings.ToLower(user)] = label
		}
	}
	return out
}

func containsStringFold(values []string, value string) bool {
	for _, candidate := range values {
		if strings.EqualFold(candidate, value) {
			return true
		}
	}
	return false
}

func uniqueNumericSuffixMonogram(user string, group []string) string {
	suffix, ok := trailingDigits(user)
	if !ok {
		return ""
	}
	for _, other := range group {
		if strings.EqualFold(user, other) {
			continue
		}
		otherSuffix, otherOK := trailingDigits(other)
		if otherOK && otherSuffix == suffix {
			return ""
		}
	}
	return numberedUserMonogramSuffix(user, suffix)
}

func assigneeMonogram(user string, labels map[string]string) string {
	key := strings.ToLower(strings.TrimSpace(user))
	if label := labels[key]; label != "" {
		return label
	}
	return userMonogram(user)
}

func uniqueUserPrefix(user string, group []string) string {
	runes := userIdentifierRunes(user)
	for size := 1; size <= 3 && size <= len(runes); size++ {
		prefix := string(runes[:size])
		unique := true
		for _, other := range group {
			if strings.EqualFold(user, other) {
				continue
			}
			otherRunes := userIdentifierRunes(other)
			if len(otherRunes) >= size && string(otherRunes[:size]) == prefix {
				unique = false
				break
			}
		}
		if unique {
			return prefix
		}
	}
	return ""
}

func numberedUserMonogram(user string, n int) string {
	return numberedUserMonogramSuffix(user, strconv.Itoa(n))
}

func numberedUserMonogramSuffix(user, suffix string) string {
	runes := userIdentifierRunes(user)
	limit := 3 - utf8.RuneCountInString(suffix)
	if limit < 1 {
		limit = 1
	}
	if len(runes) > limit {
		runes = runes[:limit]
	}
	if len(runes) == 0 {
		return suffix
	}
	return string(runes) + suffix
}

func trailingDigits(user string) (string, bool) {
	var digits []rune
	for _, r := range strings.TrimSpace(user) {
		if unicode.IsDigit(r) {
			digits = append(digits, r)
			continue
		}
		digits = nil
	}
	if len(digits) == 0 {
		return "", false
	}
	return string(digits), true
}

func userIdentifierRunes(user string) []rune {
	var out []rune
	for _, r := range strings.TrimSpace(user) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out = append(out, unicode.ToUpper(r))
		}
	}
	return out
}

func storyDisplayID(monogram string, id int) string {
	monogram = strings.TrimSpace(monogram)
	if monogram == "" {
		return strconv.Itoa(id)
	}
	return monogram + "-" + strconv.Itoa(id)
}

func parseStoryDisplayID(value, monogram string) (int, error) {
	monogram = strings.TrimSpace(monogram)
	if monogram == "" {
		return 0, errors.New("story id prefix is required")
	}
	wantPrefix := strings.ToUpper(monogram) + "-"
	value = strings.ToUpper(strings.TrimSpace(value))
	if !strings.HasPrefix(value, wantPrefix) {
		return 0, fmt.Errorf("story id must use the repository prefix, for example %s", storyDisplayID(monogram, 1))
	}
	id, err := strconv.Atoi(strings.TrimPrefix(value, wantPrefix))
	if err != nil || id <= 0 {
		return 0, errors.New("story id is required")
	}
	return id, nil
}

func repoMonogram(name string) string {
	name = strings.TrimSpace(strings.Trim(name, "/"))
	if name == "" {
		return ""
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, ".git")
	var parts []string
	var current []rune
	flush := func() {
		if len(current) == 0 {
			return
		}
		parts = append(parts, string(current))
		current = nil
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current = append(current, r)
			continue
		}
		flush()
	}
	flush()
	if len(parts) == 0 {
		return ""
	}
	var out []rune
	if len(parts) == 1 {
		for _, r := range parts[0] {
			out = append(out, unicode.ToUpper(r))
			if len(out) == 2 {
				break
			}
		}
		return string(out)
	}
	for _, part := range parts {
		for _, r := range part {
			out = append(out, unicode.ToUpper(r))
			break
		}
		if len(out) == 3 {
			break
		}
	}
	return string(out)
}

func userMonogram(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r))
	})
	var out []rune
	if len(parts) == 1 {
		for _, r := range parts[0] {
			out = append(out, unicode.ToUpper(r))
			if len(out) == 2 {
				break
			}
		}
		return string(out)
	}
	for _, part := range parts {
		if part == "" {
			continue
		}
		for _, r := range part {
			out = append(out, unicode.ToUpper(r))
			break
		}
		if len(out) == 2 {
			break
		}
	}
	return string(out)
}

func kanbanLaneLabel(lane string) string {
	switch lane {
	case "ready":
		return "Ready"
	case "doing":
		return "Doing"
	case "review":
		return "Review"
	case "done":
		return "Done"
	default:
		return "Backlog"
	}
}

func storyText(story brokerIssue) string {
	return firstNonEmpty(strings.TrimSpace(story.Body), strings.TrimSpace(story.Title), "Untitled story")
}

func storySummary(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return ""
	}
	const max = 80
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return strings.TrimSpace(string(runes[:max-1])) + "..."
}

func prHeaderHTML(pr brokerPullRequest, active string) string {
	status := firstNonEmpty(pr.Status, "open")
	title := firstNonEmpty(pr.Title, "Untitled pull request")
	filesActive := ""
	conversationActive := ` class="active"`
	commitsActive := ""
	reviewActive := ""
	if active == "files" || active == "files-changed" || active == "diff" {
		conversationActive = ""
		filesActive = ` class="active"`
	} else if active == "commits" {
		conversationActive = ""
		commitsActive = ` class="active"`
	} else if active == "review" {
		conversationActive = ""
		reviewActive = ` class="active"`
	}
	id := strconv.Itoa(pr.ID)
	var b strings.Builder
	b.WriteString(`<section class="panel pr-detail-header">`)
	b.WriteString(`<div class="pr-detail-title"><span class="pr-status">` + html.EscapeString(status) + `</span><h2>#` + id + ` ` + html.EscapeString(title) + `</h2></div>`)
	b.WriteString(`<div class="muted">` + html.EscapeString(shortRefName(pr.Source)) + ` → ` + html.EscapeString(shortRefName(pr.Target)))
	if pr.Author != "" {
		b.WriteString(` by ` + html.EscapeString(pr.Author))
	}
	b.WriteString(`</div>`)
	b.WriteString(`<div class="pr-nav-row"><nav class="pr-subtabs"><a` + conversationActive + ` href="/prs/` + id + `">Conversation</a><a` + filesActive + ` href="/prs/` + id + `/files">Files changed</a><a` + commitsActive + ` href="/prs/` + id + `/commits">Commits</a><a` + reviewActive + ` href="/prs/` + id + `/review">Review</a></nav></div>`)
	b.WriteString(`</section>`)
	return b.String()
}

func (s *webServer) pullRequestConversationHTML(ctx context.Context, pr brokerPullRequest) string {
	contexts := s.prInlineCommentContexts(ctx, pr)
	var b strings.Builder
	b.WriteString(`<section class="panel pr-conversation" data-pr-id="` + strconv.Itoa(pr.ID) + `"><div class="panel-title">Conversation</div>`)
	if strings.TrimSpace(pr.Body) != "" {
		b.WriteString(`<div class="pr-body">` + html.EscapeString(pr.Body) + `</div>`)
	} else {
		b.WriteString(`<div class="empty">No description.</div>`)
	}
	b.WriteString(`<div class="pr-timeline">`)
	for _, comment := range pr.Comments {
		b.WriteString(prNoteHTML(comment, "commented", contexts))
	}
	for _, review := range pr.Reviews {
		label := "reviewed"
		if review.State == "approved" {
			label = "approved"
		} else if review.State == "changes_requested" {
			label = "requested changes"
		}
		b.WriteString(prNoteHTML(review, label, contexts))
	}
	if len(pr.Comments) == 0 && len(pr.Reviews) == 0 {
		b.WriteString(`<div class="empty">No comments or reviews yet.</div>`)
	}
	b.WriteString(`</div>`)
	if firstNonEmpty(pr.Status, "open") == "open" {
		b.WriteString(`<div class="actions"><a class="button-link primary" href="/prs/` + strconv.Itoa(pr.ID) + `/review">Start review</a></div>`)
		b.WriteString(`<form class="pr-review-form" data-pr-form><label for="pr-comment">Comment</label><textarea id="pr-comment" data-pr-comment rows="4" placeholder="Leave a comment or review note"></textarea><div class="pr-review-actions"><button type="button" class="button-link" data-pr-action="comment" data-capability="comment">Comment</button><button type="button" class="button-link" data-pr-action="approve" data-capability="approve">Approve</button><button type="button" class="button-link" data-pr-action="reject" data-capability="review">Request changes</button><button type="button" class="button-link" data-pr-action="close" data-capability="push">Close PR</button><label class="pr-delete-branch"><input type="checkbox" data-pr-delete-branch data-capability="merge"> Delete source branch after merge</label><button type="button" class="button-link primary" data-pr-action="merge" data-capability="merge">Merge</button></div></form>`)
	} else {
		b.WriteString(`<div class="pr-closed-note">This pull request is ` + html.EscapeString(firstNonEmpty(pr.Status, "closed")) + `.<button type="button" class="button-link" data-pr-action="reopen" data-capability="reopen_pr">Reopen PR</button></div>`)
	}
	b.WriteString(`</section>`)
	return b.String()
}

func (s *webServer) prInlineCommentContexts(ctx context.Context, pr brokerPullRequest) map[string][]webVisualDiffRow {
	contexts := map[string][]webVisualDiffRow{}
	if len(pr.Comments) == 0 && len(pr.Reviews) == 0 {
		return contexts
	}
	files, _, _, err := s.pullRequestChangedFiles(ctx, pr)
	if err != nil {
		return contexts
	}
	filesByPath := map[string]webChangedFile{}
	for _, file := range files {
		filesByPath[file.path] = file
	}
	collect := func(note brokerPullRequestNote) {
		for _, comment := range note.Comments {
			if comment.Kind != "line" || comment.File == "" || comment.Line <= 0 {
				continue
			}
			file, ok := filesByPath[comment.File]
			if !ok {
				continue
			}
			rows := file.visual
			if len(rows) == 0 {
				rows = webVisualDiffRows(file.diff)
			}
			context := prCommentAfterContextRows(rows, comment)
			if len(context) > 0 {
				contexts[prInlineCommentKey(comment)] = context
			}
		}
	}
	for _, note := range pr.Comments {
		collect(note)
	}
	for _, note := range pr.Reviews {
		collect(note)
	}
	return contexts
}

func prCommentAfterContextRows(rows []webVisualDiffRow, comment brokerPullRequestComment) []webVisualDiffRow {
	target := -1
	targetLine := strconv.Itoa(comment.Line)
	for i, row := range rows {
		if row.newLine == targetLine {
			target = i
			break
		}
		if target < 0 && comment.HunkIndex >= 0 && row.hunkIndex == comment.HunkIndex && row.offset == comment.Offset && row.newLine != "" {
			target = i
		}
	}
	if target < 0 {
		return nil
	}
	hunkIndex := rows[target].hunkIndex
	var context []webVisualDiffRow
	for _, row := range rows {
		if row.hidden || row.newLine == "" {
			continue
		}
		if hunkIndex >= 0 && row.hunkIndex != hunkIndex {
			continue
		}
		if row.kind == "hunk" || row.kind == "hunk-top" || row.kind == "hunk-bottom" || row.kind == "note" {
			continue
		}
		context = append(context, row)
	}
	if len(context) == 0 {
		context = append(context, rows[target])
	}
	return context
}

func prNoteHTML(note brokerPullRequestNote, action string, contexts map[string][]webVisualDiffRow) string {
	user := firstNonEmpty(note.User, "unknown")
	when := note.At
	if parsed, err := time.Parse(time.RFC3339, note.At); err == nil {
		when = relativeTime(parsed.Unix())
	}
	var b strings.Builder
	b.WriteString(`<div class="pr-note" id="pr-note-` + strconv.Itoa(note.ID) + `" data-pr-note-id="` + strconv.Itoa(note.ID) + `"><div class="pr-note-meta"><strong>` + html.EscapeString(user) + `</strong> ` + html.EscapeString(action))
	if when != "" {
		b.WriteString(` <span>` + html.EscapeString(when) + `</span>`)
	}
	b.WriteString(`<button type="button" class="review-comment-button pr-reply-button" data-pr-reply data-target-note-id="` + strconv.Itoa(note.ID) + `" aria-label="Reply" title="Reply">💬</button></div>`)
	if strings.TrimSpace(note.Body) != "" {
		b.WriteString(`<div class="pr-note-body">` + html.EscapeString(note.Body) + `</div>`)
	}
	if len(note.Replies) > 0 {
		b.WriteString(prReplyThreadHTML(note.Replies, 1))
	}
	if len(note.Comments) > 0 {
		b.WriteString(`<div class="pr-inline-comments">`)
		for _, comment := range note.Comments {
			b.WriteString(prInlineCommentHTML(note.ID, comment, contexts[prInlineCommentKey(comment)]))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

func prInlineCommentKey(comment brokerPullRequestComment) string {
	return strings.Join([]string{
		comment.File,
		comment.Kind,
		strconv.Itoa(comment.Line),
		strconv.Itoa(comment.HunkIndex),
		strconv.Itoa(comment.Offset),
		comment.Head,
		comment.Body,
	}, "\x00")
}

func prInlineCommentHTML(noteID int, comment brokerPullRequestComment, context []webVisualDiffRow) string {
	file := firstNonEmpty(comment.File, "Changed file")
	line := ""
	if comment.Kind == "line" && comment.Line > 0 {
		line = strconv.Itoa(comment.Line)
	}
	var b strings.Builder
	b.WriteString(`<div class="pr-inline-comment" id="pr-comment-` + strconv.Itoa(comment.ID) + `" data-pr-comment-id="` + strconv.Itoa(comment.ID) + `">`)
	b.WriteString(`<div class="pr-inline-context-header"><span>` + html.EscapeString(file) + `</span>`)
	if line != "" {
		b.WriteString(`<span class="pr-inline-context-line">line ` + html.EscapeString(line) + `</span>`)
	}
	if comment.Outdated {
		b.WriteString(`<span class="pr-inline-outdated">outdated</span>`)
	}
	b.WriteString(`<button type="button" class="review-comment-button pr-reply-button" data-pr-reply data-target-note-id="` + strconv.Itoa(noteID) + `" data-target-comment-id="` + strconv.Itoa(comment.ID) + `" aria-label="Reply" title="Reply">💬</button>`)
	b.WriteString(`</div>`)
	if comment.Kind == "line" {
		if len(context) > 0 {
			b.WriteString(`<div class="pr-inline-after-context">`)
			targetRendered := false
			for _, row := range context {
				target := row.newLine == line
				if target {
					targetRendered = true
				}
				b.WriteString(prInlineAfterRowHTML(row, target))
				if target {
					b.WriteString(`<div></div>` + prInlineCommentBodyHTML(comment, prReplyAttrs(noteID, comment.ID)))
					if len(comment.Replies) > 0 {
						b.WriteString(`<div></div>` + prReplyThreadHTML(comment.Replies, 1))
					}
				}
			}
			if !targetRendered {
				b.WriteString(`<div></div>` + prInlineCommentBodyHTML(comment, prReplyAttrs(noteID, comment.ID)))
				if len(comment.Replies) > 0 {
					b.WriteString(`<div></div>` + prReplyThreadHTML(comment.Replies, 1))
				}
			}
			b.WriteString(`</div>`)
		} else {
			b.WriteString(`<div class="pr-inline-after-context">`)
			b.WriteString(`<div class="pr-inline-line-number">` + html.EscapeString(line) + `</div>`)
			lineText := comment.LineText
			if strings.TrimSpace(lineText) == "" {
				lineText = "(line context unavailable)"
			}
			b.WriteString(`<pre class="pr-inline-line-code pr-inline-target-line"><span class="diff-change added">` + html.EscapeString(lineText) + `</span></pre>`)
			b.WriteString(`<div></div>` + prInlineCommentBodyHTML(comment, prReplyAttrs(noteID, comment.ID)))
			if len(comment.Replies) > 0 {
				b.WriteString(`<div></div>` + prReplyThreadHTML(comment.Replies, 1))
			}
			b.WriteString(`</div>`)
		}
	} else {
		b.WriteString(`<div class="pr-inline-file-comment"><div class="pr-inline-file-target">File comment</div>` + prInlineCommentBodyHTML(comment, prReplyAttrs(noteID, comment.ID)) + `</div>`)
		if len(comment.Replies) > 0 {
			b.WriteString(prReplyThreadHTML(comment.Replies, 1))
		}
	}
	b.WriteString(`</div>`)
	return b.String()
}

func prReplyThreadHTML(replies []brokerPullRequestComment, depth int) string {
	if len(replies) == 0 {
		return ""
	}
	if depth > 5 {
		depth = 5
	}
	var b strings.Builder
	b.WriteString(`<div class="pr-reply-thread depth-` + strconv.Itoa(depth) + `">`)
	for _, reply := range replies {
		user := firstNonEmpty(reply.User, "unknown")
		when := reply.At
		if parsed, err := time.Parse(time.RFC3339, reply.At); err == nil {
			when = relativeTime(parsed.Unix())
		}
		b.WriteString(`<div class="pr-reply" id="pr-comment-` + strconv.Itoa(reply.ID) + `" data-pr-comment-id="` + strconv.Itoa(reply.ID) + `"><div class="pr-reply-meta"><strong>` + html.EscapeString(user) + `</strong> commented`)
		if when != "" {
			b.WriteString(` <span>` + html.EscapeString(when) + `</span>`)
		}
		b.WriteString(`<button type="button" class="review-comment-button pr-reply-button" data-pr-reply data-target-comment-id="` + strconv.Itoa(reply.ID) + `" aria-label="Reply" title="Reply">💬</button></div>`)
		b.WriteString(`<div>` + html.EscapeString(reply.Body) + `</div>`)
		b.WriteString(`</div>`)
		if len(reply.Replies) > 0 {
			b.WriteString(prReplyThreadHTML(reply.Replies, depth+1))
		}
	}
	b.WriteString(`</div>`)
	return b.String()
}

func prReplyAttrs(noteID, commentID int) string {
	attrs := ` data-pr-reply`
	if noteID > 0 {
		attrs += ` data-target-note-id="` + strconv.Itoa(noteID) + `"`
	}
	if commentID > 0 {
		attrs += ` data-target-comment-id="` + strconv.Itoa(commentID) + `"`
	}
	return attrs
}

func prInlineCommentBodyHTML(comment brokerPullRequestComment, replyAttrs string) string {
	reply := ""
	if replyAttrs != "" {
		reply = `<button type="button" class="review-comment-button pr-reply-button pr-inline-body-reply"` + replyAttrs + ` aria-label="Reply" title="Reply">💬</button>`
	}
	user := firstNonEmpty(comment.User, "unknown")
	when := comment.At
	if parsed, err := time.Parse(time.RFC3339, comment.At); err == nil {
		when = relativeTime(parsed.Unix())
	}
	meta := `<div class="pr-reply-meta"><strong>` + html.EscapeString(user) + `</strong> commented`
	if when != "" {
		meta += ` <span>` + html.EscapeString(when) + `</span>`
	}
	meta += `</div>`
	return `<div class="pr-inline-comment-body"><div>` + meta + `<div>` + html.EscapeString(comment.Body) + `</div></div>` + reply + `</div>`
}

func prInlineAfterRowHTML(row webVisualDiffRow, target bool) string {
	right := webDiffCellHTML(row.right, false, row.kind == "add")
	if row.kind == "change" {
		_, right = webInlineChangedHTML(row.left, row.right)
	}
	targetClass := ""
	if target {
		targetClass = " pr-inline-target-line"
	}
	return `<div class="pr-inline-line-number">` + html.EscapeString(row.newLine) + `</div><pre class="pr-inline-line-code` + targetClass + `">` + right + `</pre>`
}

func (s *webServer) commitListHTML(ctx context.Context, commits []commitObject, ref string, compact bool, selectedHash string) string {
	if len(commits) == 0 {
		return `<div class="empty">No commits.</div>`
	}
	var b strings.Builder
	b.WriteString(`<ol class="commits">`)
	for _, commit := range commits {
		when := ""
		if commit.timestamp > 0 {
			when = time.Unix(commit.timestamp, 0).Format("2006-01-02 15:04")
		}
		selected := selectedHash != "" && (commit.hash == selectedHash || strings.HasPrefix(commit.hash, selectedHash) || strings.HasPrefix(selectedHash, commit.hash))
		selectedClass := ""
		if selected {
			selectedClass = ` class="is-selected-commit"`
		}
		commitURL := webCommitURL(commit.hash, ref)
		b.WriteString(`<li` + selectedClass + ` data-commit-row data-commit-hash="` + html.EscapeString(commit.hash) + `" data-commit-href="` + html.EscapeString(commitURL) + `" id="commit-` + html.EscapeString(shortHash(commit.hash)) + `"><div class="commit-row-main"><a class="commit-subject" href="` + html.EscapeString(commitURL) + `">` + html.EscapeString(displayCommitSubject(commit)) + `</a><span class="state-actions" data-commit-state></span><div class="meta">` + html.EscapeString(commit.author) + ` authored ` + html.EscapeString(when))
		if !compact && commit.committer != "" && (commit.committer != commit.author || commit.committerEmail != commit.email) {
			b.WriteString(` · committed by ` + html.EscapeString(commit.committer))
		}
		b.WriteString(`</div></div><div class="commit-row-meta"><a class="commit-hash-link" href="` + html.EscapeString(commitURL) + `"><code>` + html.EscapeString(shortHash(commit.hash)) + `</code></a></div>`)
		if selected {
			b.WriteString(s.commitInlineDetailHTML(ctx, commit, ref))
		}
		b.WriteString(`</li>`)
	}
	if compact {
		b.WriteString(`</ol><div class="actions"><a class="button-link" href="` + html.EscapeString("/commits?ref="+urlQueryEscape(ref)) + `">View all commits</a></div>`)
	} else {
		b.WriteString(`</ol>`)
	}
	return b.String()
}

func (s *webServer) commitInlineDetailHTML(ctx context.Context, commit commitObject, ref string) string {
	files, additions, deletions, err := s.changedFiles(ctx, commit)
	if err != nil {
		return `<div class="commit-inline-detail"><div class="empty">` + html.EscapeString(err.Error()) + `</div></div>`
	}
	var b strings.Builder
	b.WriteString(`<div class="commit-inline-detail">`)
	b.WriteString(`<section class="commit-detail">`)
	b.WriteString(`<h2>` + html.EscapeString(firstNonEmpty(commit.subject, shortHash(commit.hash))) + `</h2>`)
	if commit.body != "" {
		b.WriteString(`<pre class="commit-message">` + html.EscapeString(commit.body) + `</pre>`)
	}
	b.WriteString(`<div class="metadata-grid">`)
	b.WriteString(metadataItemHTML("Author", commit.author, commit.email, commit.timestamp))
	b.WriteString(metadataItemHTML("Committer", firstNonEmpty(commit.committer, commit.author), firstNonEmpty(commit.committerEmail, commit.email), firstNonZero(commit.committerTimestamp, commit.timestamp)))
	b.WriteString(`<div><span>Commit</span><code>` + html.EscapeString(commit.hash) + `</code></div>`)
	b.WriteString(`<div><span>Tree</span><code>` + html.EscapeString(commit.tree) + `</code></div>`)
	if len(commit.parents) > 0 {
		var parentLinks []string
		for _, parent := range commit.parents {
			parentLinks = append(parentLinks, `<a href="`+html.EscapeString(webCommitURL(parent, ref))+`"><code>`+html.EscapeString(shortHash(parent))+`</code></a>`)
		}
		b.WriteString(`<div><span>Parents</span>` + strings.Join(parentLinks, " ") + `</div>`)
	}
	b.WriteString(`</div></section>`)
	b.WriteString(diffFilesPanelHTML(files, additions, deletions))
	b.WriteString(`</div>`)
	return b.String()
}

func displayCommitSubject(commit commitObject) string {
	const maxSubjectRunes = 80
	subject := firstNonBlankLine(commit.subject)
	if subject == "" {
		subject = firstNonBlankLine(commit.body)
	}
	if subject == "" {
		return shortHash(commit.hash)
	}
	runes := []rune(subject)
	if len(runes) <= maxSubjectRunes {
		return subject
	}
	return strings.TrimSpace(string(runes[:maxSubjectRunes-1])) + "…"
}

func firstNonBlankLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func cleanWebPath(value string) string {
	value = strings.TrimSpace(strings.Trim(value, "/"))
	if value == "" {
		return ""
	}
	cleaned := pathpkg.Clean("/" + value)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "." {
		return ""
	}
	return cleaned
}

func webURL(route, repoPath, ref string) string {
	repoPath = cleanWebPath(repoPath)
	value := "/"
	if !(route == "tree" && repoPath == "") {
		value = "/" + route
		if repoPath != "" {
			value += "/" + repoPath
		}
	}
	if route == "" {
		value = "/"
	}
	if strings.TrimSpace(ref) != "" {
		value += "?ref=" + urlQueryEscape(ref)
	}
	return value
}

func normalizeWebRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if isHexHash(ref) || strings.HasPrefix(ref, "refs/") {
		return ref
	}
	return branchRef(ref)
}

func urlQueryEscape(value string) string {
	return url.QueryEscape(value)
}

func webCommitURL(hash, ref string) string {
	value := "/commits?commit=" + urlQueryEscape(hash)
	if strings.TrimSpace(ref) != "" {
		value += "&ref=" + urlQueryEscape(ref)
	}
	return value
}

func webPageTitle(title, repoPath string) string {
	if repoPath == "" {
		return title
	}
	return repoPath + " - " + title
}

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func isTextBlob(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	return utf8.Valid(data)
}

func (s *webServer) changedFiles(ctx context.Context, commit commitObject) ([]webChangedFile, int, int, error) {
	before := map[string]webTreeFile{}
	if len(commit.parents) > 0 {
		parent, err := s.repo.commit(ctx, commit.parents[0])
		if err != nil {
			return nil, 0, 0, err
		}
		if err := s.collectTreeFiles(ctx, parent.tree, "", before); err != nil {
			return nil, 0, 0, err
		}
	}
	after := map[string]webTreeFile{}
	if err := s.collectTreeFiles(ctx, commit.tree, "", after); err != nil {
		return nil, 0, 0, err
	}
	seen := map[string]struct{}{}
	for path := range before {
		seen[path] = struct{}{}
	}
	for path := range after {
		seen[path] = struct{}{}
	}
	var paths []string
	for path := range seen {
		if before[path].hash != after[path].hash {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	var files []webChangedFile
	totalAdditions := 0
	totalDeletions := 0
	for _, path := range paths {
		file, err := s.changedFile(ctx, path, before[path].hash, after[path].hash)
		if err != nil {
			return nil, 0, 0, err
		}
		totalAdditions += file.additions
		totalDeletions += file.deletions
		files = append(files, file)
	}
	return files, totalAdditions, totalDeletions, nil
}

func (s *webServer) changedFilesBetweenTrees(ctx context.Context, beforeTree, afterTree string) ([]webChangedFile, int, int, error) {
	before := map[string]webTreeFile{}
	if beforeTree != "" {
		if err := s.collectTreeFiles(ctx, beforeTree, "", before); err != nil {
			return nil, 0, 0, err
		}
	}
	after := map[string]webTreeFile{}
	if afterTree != "" {
		if err := s.collectTreeFiles(ctx, afterTree, "", after); err != nil {
			return nil, 0, 0, err
		}
	}
	return s.changedFilesBetweenMaps(ctx, before, after)
}

func (s *webServer) changedFilesBetweenMaps(ctx context.Context, before, after map[string]webTreeFile) ([]webChangedFile, int, int, error) {
	seen := map[string]struct{}{}
	for path := range before {
		seen[path] = struct{}{}
	}
	for path := range after {
		seen[path] = struct{}{}
	}
	var paths []string
	for path := range seen {
		if before[path].hash != after[path].hash {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	var files []webChangedFile
	totalAdditions := 0
	totalDeletions := 0
	for _, path := range paths {
		file, err := s.changedFile(ctx, path, before[path].hash, after[path].hash)
		if err != nil {
			return nil, 0, 0, err
		}
		totalAdditions += file.additions
		totalDeletions += file.deletions
		files = append(files, file)
	}
	return files, totalAdditions, totalDeletions, nil
}

func (s *webServer) collectTreeFiles(ctx context.Context, treeHash, prefix string, out map[string]webTreeFile) error {
	entries, err := s.repo.treeEntries(ctx, treeHash)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		entryPath := pathpkg.Join(prefix, entry.name)
		if entry.typ == gitObjectTree {
			if err := s.collectTreeFiles(ctx, entry.hash, entryPath, out); err != nil {
				return err
			}
			continue
		}
		out[entryPath] = webTreeFile{path: entryPath, hash: entry.hash}
	}
	return nil
}

func (s *webServer) changedFile(ctx context.Context, path, oldHash, newHash string) (webChangedFile, error) {
	var oldData, newData []byte
	if oldHash != "" {
		obj, err := s.repo.object(ctx, oldHash)
		if err != nil {
			return webChangedFile{path: path, oldHash: oldHash, newHash: newHash}, err
		}
		oldData = obj.data
	}
	if newHash != "" {
		obj, err := s.repo.object(ctx, newHash)
		if err != nil {
			return webChangedFile{path: path, oldHash: oldHash, newHash: newHash}, err
		}
		newData = obj.data
	}
	return webChangedFileFromData(path, oldHash, newHash, oldData, newData), nil
}

func webChangedFileFromData(path, oldHash, newHash string, oldData, newData []byte) webChangedFile {
	file := webChangedFile{path: path, oldHash: oldHash, newHash: newHash}
	if !isTextBlob(oldData) || !isTextBlob(newData) {
		file.binary = true
		return file
	}
	file.visual = webVisualRowsFromText(string(oldData), string(newData), 3)
	for _, line := range simpleLineDiff(string(oldData), string(newData)) {
		diffLine := webDiffLine{text: line}
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			diffLine.kind = "add"
			file.additions++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			diffLine.kind = "del"
			file.deletions++
		case strings.HasPrefix(line, "@@"):
			diffLine.kind = "hunk"
		default:
			diffLine.kind = "ctx"
		}
		file.diff = append(file.diff, diffLine)
	}
	return file
}

func diffFileHTML(file webChangedFile) string {
	return diffFileHTMLWithOptions(file, webDiffRenderOptions{})
}

func diffFileHTMLWithOptions(file webChangedFile, opts webDiffRenderOptions) string {
	var b strings.Builder
	reviewAttrs := ""
	reviewButton := ""
	if opts.Review {
		reviewAttrs = ` data-review-file="` + html.EscapeString(file.path) + `"`
		reviewButton = `<button type="button" class="review-comment-button file-comment" data-review-comment-file="` + html.EscapeString(file.path) + `" aria-label="Comment on file" title="Comment on file">💬</button>`
	}
	b.WriteString(`<section class="panel diff-file" id="diff-` + html.EscapeString(anchorID(file.path)) + `"` + reviewAttrs + `><div class="diff-header"><div><strong>` + html.EscapeString(file.path) + `</strong><div class="muted">`)
	if file.oldHash == "" && file.newHash == "" {
		b.WriteString(`local changes`)
	} else if file.oldHash == "" {
		b.WriteString(`added`)
	} else if file.newHash == "" {
		b.WriteString(`deleted`)
	} else {
		b.WriteString(shortHash(file.oldHash) + ` -> ` + shortHash(file.newHash))
	}
	b.WriteString(`</div></div><div class="diff-header-actions">` + reviewButton + `<span class="additions">+` + strconv.Itoa(file.additions) + `</span> <span class="deletions">-` + strconv.Itoa(file.deletions) + `</span></div></div>`)
	if file.binary {
		b.WriteString(`<div class="empty">Binary file changed.</div>`)
	} else if len(file.visual) > 0 {
		b.WriteString(webVisualDiffGridRowsHTML(file.visual))
	} else {
		b.WriteString(webVisualDiffGridHTML(file.diff))
	}
	b.WriteString(`</section>`)
	return b.String()
}

type webVisualDiffRow struct {
	kind      string
	left      string
	right     string
	oldLine   string
	newLine   string
	control   string
	hunk      string
	hunkIndex int
	oldStart  int
	newStart  int
	offset    int
	hidden    bool
}

type webPendingDelete struct {
	text string
	line int
}

func webVisualDiffGridHTML(lines []webDiffLine) string {
	return webVisualDiffGridRowsHTML(webVisualDiffRows(lines))
}

func webVisualDiffGridRowsHTML(rows []webVisualDiffRow) string {
	var b strings.Builder
	b.WriteString(`<div class="visual-diff"><section class="visual-diff-file"><div class="visual-diff-grid"><div class="visual-diff-heading visual-diff-line-heading"></div><div class="visual-diff-heading">Before</div><div class="visual-diff-heading visual-diff-line-heading"></div><div class="visual-diff-heading">After</div>`)
	for _, row := range rows {
		b.WriteString(webVisualDiffRowHTML(row))
	}
	b.WriteString(`</div></section></div>`)
	return b.String()
}

func webVisualRowsFromText(left, right string, context int) []webVisualDiffRow {
	a := splitLines(left)
	b := splitLines(right)
	ops := simpleLineDiffOps(a, b)
	hunks := simpleLineDiffHunks(ops, context)
	if len(hunks) == 0 {
		return nil
	}
	hunkStarts := map[int]int{}
	hunkEnds := map[int]int{}
	visible := map[int]struct{}{}
	for i, hunk := range hunks {
		hunkStarts[hunk.start] = i
		hunkEnds[hunk.end] = i
		for i := hunk.start; i < hunk.end; i++ {
			visible[i] = struct{}{}
		}
	}
	var rows []webVisualDiffRow
	var pending []webPendingDelete
	flushDeletes := func(hidden bool) {
		for _, deleted := range pending {
			rows = append(rows, webVisualDiffRow{kind: "del", left: deleted.text, oldLine: strconv.Itoa(deleted.line), hidden: hidden})
		}
		pending = nil
	}
	for i, op := range ops {
		if hunkIndex, ok := hunkStarts[i]; ok {
			hunk := hunks[hunkIndex]
			flushDeletes(false)
			oldStart, oldCount, newStart, newCount := simpleDiffHunkRange(ops[hunk.start:hunk.end])
			control := ""
			if hunkIndex > 0 && hunks[hunkIndex-1].end < hunk.start {
				control = "up"
			}
			hunkLabel := "Lines " + webLineRangeLabel(oldStart, oldCount) + " -> " + webLineRangeLabel(newStart, newCount)
			rows = append(rows, webVisualDiffRow{kind: "hunk-top", left: hunkLabel, control: control, hunk: hunkLabel, hunkIndex: hunkIndex, oldStart: oldStart, newStart: newStart})
		}
		_, isVisible := visible[i]
		hidden := !isVisible
		hunkIndex := webHunkIndexForOp(hunks, i)
		hunkLabel := ""
		oldStart, newStart := 0, 0
		if hunkIndex >= 0 {
			oldStart, _, newStart, _ = simpleDiffHunkRange(ops[hunks[hunkIndex].start:hunks[hunkIndex].end])
			hunkLabel = "Lines " + webLineRangeLabel(oldStart, 1) + " -> " + webLineRangeLabel(newStart, 1)
		}
		switch op.kind {
		case '-':
			pending = append(pending, webPendingDelete{text: op.text, line: op.oldLine})
		case '+':
			if len(pending) > 0 {
				deleted := pending[0]
				pending = pending[1:]
				rows = append(rows, webVisualDiffRow{kind: "change", left: deleted.text, right: op.text, oldLine: strconv.Itoa(deleted.line), newLine: strconv.Itoa(op.newLine), hidden: hidden, hunk: hunkLabel, hunkIndex: hunkIndex, oldStart: oldStart, newStart: newStart, offset: op.newLine - newStart})
			} else {
				rows = append(rows, webVisualDiffRow{kind: "add", right: op.text, newLine: strconv.Itoa(op.newLine), hidden: hidden, hunk: hunkLabel, hunkIndex: hunkIndex, oldStart: oldStart, newStart: newStart, offset: op.newLine - newStart})
			}
		default:
			flushDeletes(hidden)
			rows = append(rows, webVisualDiffRow{kind: "same", left: op.text, right: op.text, oldLine: strconv.Itoa(op.oldLine), newLine: strconv.Itoa(op.newLine), hidden: hidden, hunk: hunkLabel, hunkIndex: hunkIndex, oldStart: oldStart, newStart: newStart, offset: op.newLine - newStart})
		}
		if hunkIndex, ok := hunkEnds[i+1]; ok {
			hunk := hunks[hunkIndex]
			flushDeletes(false)
			control := ""
			if hunkIndex < len(hunks)-1 && hunk.end < hunks[hunkIndex+1].start {
				control = "down"
			}
			if control != "" {
				rows = append(rows, webVisualDiffRow{kind: "hunk-bottom", control: control})
			}
		}
	}
	flushDeletes(true)
	return rows
}

func webHunkIndexForOp(hunks []simpleDiffHunk, opIndex int) int {
	for i, hunk := range hunks {
		if opIndex >= hunk.start && opIndex < hunk.end {
			return i
		}
	}
	return -1
}

func webVisualDiffRows(lines []webDiffLine) []webVisualDiffRow {
	oldLine, newLine := 0, 0
	var rows []webVisualDiffRow
	var pending []webPendingDelete
	flushDeletes := func() {
		for _, deleted := range pending {
			rows = append(rows, webVisualDiffRow{kind: "del", left: deleted.text, oldLine: strconv.Itoa(deleted.line)})
		}
		pending = nil
	}
	for _, line := range lines {
		text := line.text
		switch line.kind {
		case "hunk":
			flushDeletes()
			oldLine, newLine = webDiffHunkStarts(text)
			rows = append(rows, webVisualDiffRow{kind: "hunk", left: webDiffDividerLabel(text)})
		case "del":
			pending = append(pending, webPendingDelete{text: strings.TrimPrefix(text, "-"), line: oldLine})
			oldLine++
		case "add":
			added := strings.TrimPrefix(text, "+")
			if len(pending) > 0 {
				deleted := pending[0]
				pending = pending[1:]
				rows = append(rows, webVisualDiffRow{kind: "change", left: deleted.text, right: added, oldLine: strconv.Itoa(deleted.line), newLine: strconv.Itoa(newLine)})
			} else {
				rows = append(rows, webVisualDiffRow{kind: "add", right: added, newLine: strconv.Itoa(newLine)})
			}
			newLine++
		default:
			flushDeletes()
			value := strings.TrimPrefix(text, " ")
			rows = append(rows, webVisualDiffRow{kind: "same", left: value, right: value, oldLine: strconv.Itoa(oldLine), newLine: strconv.Itoa(newLine)})
			oldLine++
			newLine++
		}
	}
	flushDeletes()
	return rows
}

func webVisualDiffRowHTML(row webVisualDiffRow) string {
	if row.kind == "hunk" || row.kind == "hunk-top" || row.kind == "hunk-bottom" || row.kind == "note" {
		controls := webDiffContextControlHTML(row.control)
		label := html.EscapeString(row.left)
		if row.kind == "hunk" {
			label = html.EscapeString(webDiffDividerLabel(row.left))
		}
		if row.kind == "hunk-bottom" && label == "" {
			label = `<span class="sr-only">More context</span>`
		} else {
			label = `<span>` + label + `</span>`
		}
		return `<div class="visual-diff-divider visual-diff-` + html.EscapeString(row.kind) + `">` + controls + label + `</div>`
	}
	left := webDiffCellHTML(row.left, row.kind == "del", false)
	right := webDiffCellHTML(row.right, false, row.kind == "add")
	if row.kind == "change" {
		left, right = webInlineChangedHTML(row.left, row.right)
	}
	hidden := ""
	if row.hidden {
		hidden = ` data-hidden-context="true" hidden`
	}
	attrs := ` data-hunk-index="` + strconv.Itoa(row.hunkIndex) + `" data-hunk="` + html.EscapeString(row.hunk) + `" data-old-start="` + strconv.Itoa(row.oldStart) + `" data-new-start="` + strconv.Itoa(row.newStart) + `" data-offset="` + strconv.Itoa(row.offset) + `"`
	return `<div class="visual-diff-row visual-diff-` + html.EscapeString(row.kind) + `"` + attrs + hidden + `><div class="visual-diff-line-number">` + html.EscapeString(row.oldLine) + `</div><pre>` + left + `</pre><div class="visual-diff-line-number">` + html.EscapeString(row.newLine) + `</div><pre data-new-line="` + html.EscapeString(row.newLine) + `">` + right + `</pre></div>`
}

func webDiffContextControlHTML(control string) string {
	switch control {
	case "up":
		return `<span class="diff-context-controls"><button type="button" data-diff-context="up" title="Show 20 more lines above" aria-label="Show 20 more lines above">↑</button></span>`
	case "down":
		return `<span class="diff-context-controls"><button type="button" data-diff-context="down" title="Show 20 more lines below" aria-label="Show 20 more lines below">↓</button></span>`
	default:
		return ""
	}
}

func webDiffCellHTML(text string, deleted, added bool) string {
	if text == "" {
		return ""
	}
	value := html.EscapeString(text)
	switch {
	case deleted:
		return `<span class="diff-change deleted">` + value + `</span>`
	case added:
		return `<span class="diff-change added">` + value + `</span>`
	default:
		return value
	}
}

func webInlineChangedHTML(left, right string) (string, string) {
	prefix := 0
	for prefix < len(left) && prefix < len(right) && left[prefix] == right[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(left)-prefix && suffix < len(right)-prefix && left[len(left)-1-suffix] == right[len(right)-1-suffix] {
		suffix++
	}
	oldEnd := len(left) - suffix
	newEnd := len(right) - suffix
	oldChanged := left[prefix:oldEnd]
	newChanged := right[prefix:newEnd]
	if oldChanged == "" {
		oldChanged = " "
	}
	if newChanged == "" {
		newChanged = " "
	}
	oldHTML := html.EscapeString(left[:prefix]) + `<span class="diff-change deleted">` + html.EscapeString(oldChanged) + `</span>` + html.EscapeString(left[oldEnd:])
	newHTML := html.EscapeString(right[:prefix]) + `<span class="diff-change added">` + html.EscapeString(newChanged) + `</span>` + html.EscapeString(right[newEnd:])
	return oldHTML, newHTML
}

func webDiffHunkStarts(line string) (int, int) {
	oldStart, _, newStart, _, ok := webDiffHunkRange(line)
	if ok {
		return oldStart, newStart
	}
	return 0, 0
}

func webDiffDividerLabel(line string) string {
	oldStart, oldCount, newStart, newCount, ok := webDiffHunkRange(line)
	if ok {
		return "Lines " + webLineRangeLabel(oldStart, oldCount) + " -> " + webLineRangeLabel(newStart, newCount)
	}
	return line
}

func webDiffHunkRange(line string) (int, int, int, int, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "@@" {
		return 0, 0, 0, 0, false
	}
	oldStart, oldCount, ok := webParseHunkPart(fields[1], "-")
	if !ok {
		return 0, 0, 0, 0, false
	}
	newStart, newCount, ok := webParseHunkPart(fields[2], "+")
	if !ok {
		return 0, 0, 0, 0, false
	}
	return oldStart, oldCount, newStart, newCount, true
}

func webParseHunkPart(part, prefix string) (int, int, bool) {
	part = strings.TrimPrefix(part, prefix)
	if part == "" {
		return 0, 0, false
	}
	startText, countText, hasCount := strings.Cut(part, ",")
	start, err := strconv.Atoi(startText)
	if err != nil {
		return 0, 0, false
	}
	count := 1
	if hasCount {
		count, err = strconv.Atoi(countText)
		if err != nil {
			return 0, 0, false
		}
	}
	return start, count, true
}

func webLineRangeLabel(start, count int) string {
	if count <= 1 {
		return strconv.Itoa(start)
	}
	return strconv.Itoa(start) + "-" + strconv.Itoa(start+count-1)
}

func diffFilesPanelHTML(files []webChangedFile, additions, deletions int) string {
	return diffFilesPanelHTMLWithOptions(files, additions, deletions, webDiffRenderOptions{})
}

func diffFilesPanelHTMLWithOptions(files []webChangedFile, additions, deletions int, opts webDiffRenderOptions) string {
	var b strings.Builder
	b.WriteString(`<section class="panel"><div class="diff-summary"><strong>` + strconv.Itoa(len(files)) + ` changed file` + pluralSuffix(len(files)) + `</strong><span class="additions">+` + strconv.Itoa(additions) + `</span><span class="deletions">-` + strconv.Itoa(deletions) + `</span></div>`)
	if len(files) == 0 {
		b.WriteString(`<div class="empty">No file changes.</div></section>`)
		return b.String()
	}
	b.WriteString(`<table class="files changed-files">`)
	for _, file := range files {
		b.WriteString(`<tr><td><a href="#diff-` + html.EscapeString(anchorID(file.path)) + `">` + html.EscapeString(file.path) + `</a></td><td class="diff-stat"><span class="additions">+` + strconv.Itoa(file.additions) + `</span> <span class="deletions">-` + strconv.Itoa(file.deletions) + `</span></td></tr>`)
	}
	b.WriteString(`</table></section>`)
	for _, file := range files {
		b.WriteString(diffFileHTMLWithOptions(file, opts))
	}
	return b.String()
}

func metadataItemHTML(label, name, email string, ts int64) string {
	var b strings.Builder
	b.WriteString(`<div><span>` + html.EscapeString(label) + `</span><strong>` + html.EscapeString(name) + `</strong>`)
	if email != "" {
		b.WriteString(`<small>` + html.EscapeString(email) + `</small>`)
	}
	if ts > 0 {
		b.WriteString(`<small>` + html.EscapeString(time.Unix(ts, 0).Format(time.RFC1123)) + `</small>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

func displayRef(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	return firstNonEmpty(ref, defaultBranch)
}

func relativeTime(ts int64) string {
	if ts == 0 {
		return "at an unknown time"
	}
	then := time.Unix(ts, 0)
	diff := time.Since(then)
	suffix := "ago"
	if diff < 0 {
		diff = -diff
		suffix = "from now"
	}
	minute := time.Minute
	hour := time.Hour
	day := 24 * hour
	week := 7 * day
	month := 30 * day
	year := 365 * day
	switch {
	case diff < minute:
		return "just now"
	case diff < hour:
		return relativeTimeUnit(int(diff/minute), "minute", suffix)
	case diff < day:
		return relativeTimeUnit(int(diff/hour), "hour", suffix)
	case diff < week:
		return relativeTimeUnit(int(diff/day), "day", suffix)
	case diff < month:
		return relativeTimeUnit(int(diff/week), "week", suffix)
	case diff < year:
		return relativeTimeUnit(int(diff/month), "month", suffix)
	default:
		return relativeTimeUnit(int(diff/year), "year", suffix)
	}
}

func parseTime(value string) int64 {
	if value == "" {
		return 0
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.Unix()
	}
	return 0
}

func relativeTimeUnit(count int, unit, suffix string) string {
	if count < 1 {
		count = 1
	}
	if count != 1 {
		unit += "s"
	}
	return strconv.Itoa(count) + " " + unit + " " + suffix
}

func firstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func anchorID(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

type webEventHub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

func newWebEventHub() *webEventHub {
	return &webEventHub{clients: map[chan string]struct{}{}}
}

func (h *webEventHub) subscribe() chan string {
	ch := make(chan string, 8)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *webEventHub) unsubscribe(ch chan string) {
	h.mu.Lock()
	delete(h.clients, ch)
	close(ch)
	h.mu.Unlock()
}

func (h *webEventHub) broadcast(name string) {
	payload := fmt.Sprintf("event: %s\ndata: {\"time\":%d}\n\n", name, time.Now().UnixMilli())
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- payload:
		default:
		}
	}
}

func (h *webEventHub) broadcastJSON(name string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	payload := fmt.Sprintf("event: %s\ndata: %s\n\n", name, data)
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- payload:
		default:
		}
	}
}

func monitorWebPath(ctx context.Context, root, eventName string, hub *webEventHub) {
	if root == "" || hub == nil {
		return
	}
	last := webPathFingerprint(root)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			next := webPathFingerprint(root)
			if next != "" && next != last {
				last = next
				hub.broadcast(eventName)
			}
		}
	}
}

func webPathFingerprint(root string) string {
	var newest int64
	var count int
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if rel, relErr := filepath.Rel(root, path); relErr == nil {
			slashRel := filepath.ToSlash(rel)
			if strings.HasPrefix(slashRel, "refs/remotes/bucketgit/") || strings.HasPrefix(slashRel, "refs/tags/") {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		name := entry.Name()
		if entry.IsDir() {
			if name == "objects" || name == "tmp" || name == "bucketgit" || name == "bgit" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".lock") || name == "index" || name == "FETCH_HEAD" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		count++
		if mod := info.ModTime().UnixNano(); mod > newest {
			newest = mod
		}
		return nil
	})
	return fmt.Sprintf("%d:%d", newest, count)
}

func webAssetString(path string) string {
	data, err := webAssetBytes(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func webAssetBytes(path string) ([]byte, error) {
	if diskPath := webAssetDiskPath(path); diskPath != "" {
		if data, err := os.ReadFile(diskPath); err == nil {
			return data, nil
		}
	}
	return webAssets.ReadFile(path)
}

func webAssetDiskPath(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), path)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
	}
	return ""
}

func webAssetDir() string {
	diskPath := webAssetDiskPath(webJSPath)
	if diskPath == "" {
		return ""
	}
	return filepath.Dir(diskPath)
}
