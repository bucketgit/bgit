package main

import (
	"bytes"
	"context"
	"encoding/base64"
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
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultWebAddr = "127.0.0.1"
	defaultWebPort = 8042
)

type webOptions struct {
	addr  string
	port  int
	local bool
}

type webServer struct {
	repo  *nativeGitRepo
	cfg   config
	title string
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

type webChangedFile struct {
	path      string
	oldHash   string
	newHash   string
	additions int
	deletions int
	diff      []webDiffLine
	binary    bool
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

func webCommand(ctx context.Context, cfg config, args []string, stdout io.Writer) error {
	opts, err := parseWebArgs(args)
	if err != nil {
		return err
	}
	repo, closeStore, cfg, err := openWebRepository(ctx, cfg, opts.local)
	if err != nil {
		return err
	}
	defer closeStore()

	handler := newWebHandler(repo, cfg)
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
	return strings.Contains(strings.ToLower(err.Error()), "address already in use")
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

func openWebRepository(ctx context.Context, cfg config, local bool) (*nativeGitRepo, func(), config, error) {
	if local {
		localRepo, err := openLocalRepository(".")
		if err != nil {
			return nil, nil, cfg, err
		}
		if localCfg, err := readLocalConfig(localRepo.worktree); err == nil {
			cfg = mergeConfig(cfg, localCfg)
		}
		if branch := localRepo.currentBranch(); branch != "" {
			cfg.branch = branch
		}
		return newNativeGitRepoForStore(cfg, localRepo.store), func() {}, cfg, nil
	}

	if cfg.bucket == "" {
		localCfg, err := readLocalConfig(".")
		if err == nil {
			cfg = mergeConfig(cfg, localCfg)
		}
	}
	if cfg.bucket == "" {
		return nil, nil, cfg, errors.New("--bucket is required outside a bucketgit checkout")
	}
	brokerURL := webBrokerURL()
	store, closeStore, err := newRemoteStore(ctx, cfg, true)
	if err != nil {
		if brokerURL == "" {
			return nil, nil, cfg, fmt.Errorf("create remote store: %w", err)
		}
		return openNativeGitRepo(&brokerGitStore{brokerURL: brokerURL, cfg: cfg}, cfg), func() {}, cfg, nil
	}
	if brokerURL != "" {
		store = &fallbackGitRemoteStore{
			primary:  store,
			fallback: &brokerGitStore{brokerURL: brokerURL, cfg: cfg},
		}
	}
	return openNativeGitRepo(store, cfg), closeStore, cfg, nil
}

func webBrokerURL() string {
	if out, err := runGit(".", "config", "--get", "bucketgit.broker"); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

func (s *brokerGitStore) read(ctx context.Context, objectPath string) ([]byte, error) {
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

func newWebHandler(repo *nativeGitRepo, cfg config) http.Handler {
	return &webServer{repo: repo, cfg: cfg, title: webRepoTitle(cfg)}
}

func webRepoTitle(cfg config) string {
	if cfg.origin != "" {
		return cfg.origin
	}
	if cfg.bucket != "" {
		return strings.Trim(strings.Join([]string{cfg.provider, cfg.bucket, cfg.prefix}, " "), " ")
	}
	return "local repository"
}

func (s *webServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	route := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	case r.URL.Path == "/":
		s.handleTree(ctx, w, r, "")
	case route == "commits":
		s.handleCommits(ctx, w, r)
	case strings.HasPrefix(route, "commit/"):
		s.handleCommit(ctx, w, r, strings.TrimPrefix(route, "commit/"))
	case strings.HasPrefix(route, "tree/"):
		s.handleTree(ctx, w, r, strings.TrimPrefix(route, "tree/"))
	case strings.HasPrefix(route, "blob/"):
		s.handleBlob(ctx, w, r, strings.TrimPrefix(route, "blob/"), false)
	case strings.HasPrefix(route, "raw/"):
		s.handleBlob(ctx, w, r, strings.TrimPrefix(route, "raw/"), true)
	default:
		http.NotFound(w, r)
	}
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

func (s *webServer) handleTree(ctx context.Context, w http.ResponseWriter, r *http.Request, repoPath string) {
	_, commit, ref, err := s.headCommit(ctx, r)
	if err != nil {
		s.renderError(w, http.StatusNotFound, err)
		return
	}
	repoPath = cleanWebPath(repoPath)
	treeHash := commit.tree
	if repoPath != "" {
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
	commits, _ := s.repo.walkCommits(ctx, commit.hash, 10, 0, repoPath)
	readme := s.readmeHTML(ctx, commit.tree)

	var body strings.Builder
	body.WriteString(`<main class="layout">`)
	body.WriteString(s.headerHTML(ref, repoPath))
	body.WriteString(s.clonePanelHTML())
	body.WriteString(`<section class="panel"><div class="commit-strip"><div><a class="commit-subject" href="` + html.EscapeString(webCommitURL(commit.hash, ref)) + `">` + html.EscapeString(firstNonEmpty(commit.subject, shortHash(commit.hash))) + `</a><div class="muted">` + html.EscapeString(commit.author) + ` committed ` + html.EscapeString(relativeTime(commit.timestamp)) + `</div></div><code>` + html.EscapeString(shortHash(commit.hash)) + `</code></div></section>`)
	body.WriteString(`<section class="panel"><div class="panel-title">Files</div><table class="files">`)
	if repoPath != "" {
		parent := pathpkg.Dir(repoPath)
		if parent == "." {
			parent = ""
		}
		body.WriteString(`<tr><td class="kind">dir</td><td><a href="` + html.EscapeString(webURL("tree", parent, ref)) + `">..</a></td><td></td></tr>`)
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
		body.WriteString(`<tr><td class="kind">` + kind + `</td><td><a href="` + html.EscapeString(webURL(route, targetPath, ref)) + `">` + html.EscapeString(name) + `</a></td><td class="hash">` + html.EscapeString(shortHash(entry.hash)) + `</td></tr>`)
	}
	body.WriteString(`</table></section>`)
	if readme != "" && repoPath == "" {
		body.WriteString(`<section class="panel readme-panel"><div class="panel-title">README</div><article class="readme">` + readme + `</article></section>`)
	}
	body.WriteString(`<section class="panel"><div class="panel-title">Recent commits</div>`)
	body.WriteString(commitListHTML(commits, ref, true))
	body.WriteString(`</section></main>`)
	s.renderPage(w, webPageTitle(s.title, repoPath), body.String())
}

func (s *webServer) handleCommits(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	_, commit, ref, err := s.headCommit(ctx, r)
	if err != nil {
		s.renderError(w, http.StatusNotFound, err)
		return
	}
	commits, err := s.repo.walkCommits(ctx, commit.hash, 100, 0, "")
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err)
		return
	}
	var body strings.Builder
	body.WriteString(`<main class="layout">`)
	body.WriteString(s.headerHTML(ref, "commits"))
	body.WriteString(`<section class="panel"><div class="panel-title">Commits</div>`)
	body.WriteString(commitListHTML(commits, ref, false))
	body.WriteString(`</section></main>`)
	s.renderPage(w, webPageTitle(s.title, "commits"), body.String())
}

func (s *webServer) handleCommit(ctx context.Context, w http.ResponseWriter, r *http.Request, hash string) {
	hash = strings.TrimSpace(strings.Trim(hash, "/"))
	if hash == "" {
		s.renderError(w, http.StatusNotFound, fs.ErrNotExist)
		return
	}
	commitHash, err := s.repo.resolveRevision(ctx, hash)
	if err != nil {
		commitHash = hash
	}
	commit, err := s.repo.commit(ctx, commitHash)
	if err != nil {
		s.renderError(w, http.StatusNotFound, err)
		return
	}
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	files, additions, deletions, err := s.changedFiles(ctx, commit)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err)
		return
	}
	var body strings.Builder
	body.WriteString(`<main class="layout">`)
	body.WriteString(s.headerHTML(firstNonEmpty(ref, commit.hash), "commit/"+shortHash(commit.hash)))
	body.WriteString(`<section class="panel commit-detail">`)
	body.WriteString(`<h2>` + html.EscapeString(firstNonEmpty(commit.subject, shortHash(commit.hash))) + `</h2>`)
	if commit.body != "" {
		body.WriteString(`<pre class="commit-message">` + html.EscapeString(commit.body) + `</pre>`)
	}
	body.WriteString(`<div class="metadata-grid">`)
	body.WriteString(metadataItemHTML("Author", commit.author, commit.email, commit.timestamp))
	body.WriteString(metadataItemHTML("Committer", firstNonEmpty(commit.committer, commit.author), firstNonEmpty(commit.committerEmail, commit.email), firstNonZero(commit.committerTimestamp, commit.timestamp)))
	body.WriteString(`<div><span>Commit</span><code>` + html.EscapeString(commit.hash) + `</code></div>`)
	body.WriteString(`<div><span>Tree</span><code>` + html.EscapeString(commit.tree) + `</code></div>`)
	if len(commit.parents) > 0 {
		var parentLinks []string
		for _, parent := range commit.parents {
			parentLinks = append(parentLinks, `<a href="`+html.EscapeString(webCommitURL(parent, ref))+`"><code>`+html.EscapeString(shortHash(parent))+`</code></a>`)
		}
		body.WriteString(`<div><span>Parents</span>` + strings.Join(parentLinks, " ") + `</div>`)
	}
	body.WriteString(`</div></section>`)
	body.WriteString(`<section class="panel"><div class="diff-summary"><strong>` + strconv.Itoa(len(files)) + ` changed file` + pluralSuffix(len(files)) + `</strong><span class="additions">+` + strconv.Itoa(additions) + `</span><span class="deletions">-` + strconv.Itoa(deletions) + `</span></div>`)
	body.WriteString(`<table class="files changed-files">`)
	for _, file := range files {
		body.WriteString(`<tr><td><a href="#diff-` + html.EscapeString(anchorID(file.path)) + `">` + html.EscapeString(file.path) + `</a></td><td class="diff-stat"><span class="additions">+` + strconv.Itoa(file.additions) + `</span> <span class="deletions">-` + strconv.Itoa(file.deletions) + `</span></td></tr>`)
	}
	body.WriteString(`</table></section>`)
	for _, file := range files {
		body.WriteString(diffFileHTML(file))
	}
	body.WriteString(`</main>`)
	s.renderPage(w, webPageTitle(s.title, shortHash(commit.hash)), body.String())
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
	body.WriteString(`<main class="layout">`)
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
	b.WriteString(`<header class="repo-header"><div class="repo-topline"><div><div class="repo-kicker">bucketgit repository</div><h1>` + html.EscapeString(s.title) + `</h1></div>`)
	b.WriteString(s.refSelectorHTML(ref))
	b.WriteString(`</div>`)
	b.WriteString(`<nav class="tabs"><a href="` + html.EscapeString(webURL("tree", "", ref)) + `">Code</a><a href="` + html.EscapeString("/commits?ref="+urlQueryEscape(ref)) + `">Commits</a></nav>`)
	if repoPath != "" {
		b.WriteString(`<div class="crumbs">`)
		b.WriteString(`<a href="` + html.EscapeString(webURL("tree", "", ref)) + `">root</a>`)
		current := ""
		for _, part := range strings.Split(repoPath, "/") {
			if part == "" {
				continue
			}
			current = pathpkg.Join(current, part)
			b.WriteString(` / <a href="` + html.EscapeString(webURL("tree", current, ref)) + `">` + html.EscapeString(part) + `</a>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</header>`)
	return b.String()
}

func (s *webServer) refSelectorHTML(ref string) string {
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

func (s *webServer) clonePanelHTML() string {
	origin := firstNonEmpty(s.cfg.origin, originForConfig(s.cfg))
	sshURL := ""
	if s.cfg.bucket != "" && s.cfg.prefix != "" {
		sshURL = sshRemoteURL(s.cfg)
	}
	var b strings.Builder
	b.WriteString(`<section class="clone-panel"><div><div class="panel-title">Clone</div><div class="muted">Use the bucket URL with bgit, or SSH after running bgit ssh setup.</div></div><div class="clone-grid">`)
	if origin != "" {
		b.WriteString(cloneRowHTML("bgit", "bgit clone "+origin))
		b.WriteString(cloneRowHTML("origin", origin))
	}
	if sshURL != "" {
		b.WriteString(cloneRowHTML("ssh", sshURL))
	}
	b.WriteString(`</div></section>`)
	return b.String()
}

func cloneRowHTML(label, value string) string {
	id := "copy-" + anchorID(label+"-"+value)
	return `<div class="clone-row"><span>` + html.EscapeString(label) + `</span><code id="` + html.EscapeString(id) + `">` + html.EscapeString(value) + `</code><button class="copy-button" data-copy-target="` + html.EscapeString(id) + `">Copy</button></div>`
}

func (s *webServer) renderPage(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>`)
	fmt.Fprint(w, html.EscapeString(title))
	fmt.Fprint(w, `</title><style>`)
	fmt.Fprint(w, webCSS)
	fmt.Fprint(w, `</style></head><body>`)
	fmt.Fprint(w, body)
	fmt.Fprint(w, webJS)
	fmt.Fprint(w, `</body></html>`)
}

func (s *webServer) renderError(w http.ResponseWriter, status int, err error) {
	w.WriteHeader(status)
	s.renderPage(w, fmt.Sprintf("%d", status), `<main class="layout"><section><div class="empty">`+html.EscapeString(err.Error())+`</div></section></main>`)
}

func commitListHTML(commits []commitObject, ref string, compact bool) string {
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
		b.WriteString(`<li><div><a class="commit-subject" href="` + html.EscapeString(webCommitURL(commit.hash, ref)) + `">` + html.EscapeString(firstNonEmpty(commit.subject, shortHash(commit.hash))) + `</a><div class="meta">` + html.EscapeString(commit.author) + ` authored ` + html.EscapeString(when))
		if !compact && commit.committer != "" && (commit.committer != commit.author || commit.committerEmail != commit.email) {
			b.WriteString(` · committed by ` + html.EscapeString(commit.committer))
		}
		b.WriteString(`</div></div><code>` + html.EscapeString(shortHash(commit.hash)) + `</code></li>`)
	}
	if compact {
		b.WriteString(`</ol><div class="actions"><a class="button-link" href="` + html.EscapeString("/commits?ref="+urlQueryEscape(ref)) + `">View all commits</a></div>`)
	} else {
		b.WriteString(`</ol>`)
	}
	return b.String()
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
	value := "/commit/" + hash
	if strings.TrimSpace(ref) != "" {
		value += "?ref=" + urlQueryEscape(ref)
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
	file := webChangedFile{path: path, oldHash: oldHash, newHash: newHash}
	var oldData, newData []byte
	if oldHash != "" {
		obj, err := s.repo.object(ctx, oldHash)
		if err != nil {
			return file, err
		}
		oldData = obj.data
	}
	if newHash != "" {
		obj, err := s.repo.object(ctx, newHash)
		if err != nil {
			return file, err
		}
		newData = obj.data
	}
	if !isTextBlob(oldData) || !isTextBlob(newData) {
		file.binary = true
		return file, nil
	}
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
	return file, nil
}

func diffFileHTML(file webChangedFile) string {
	var b strings.Builder
	b.WriteString(`<section class="panel diff-file" id="diff-` + html.EscapeString(anchorID(file.path)) + `"><div class="diff-header"><div><strong>` + html.EscapeString(file.path) + `</strong><div class="muted">`)
	if file.oldHash == "" {
		b.WriteString(`added`)
	} else if file.newHash == "" {
		b.WriteString(`deleted`)
	} else {
		b.WriteString(shortHash(file.oldHash) + ` -> ` + shortHash(file.newHash))
	}
	b.WriteString(`</div></div><div><span class="additions">+` + strconv.Itoa(file.additions) + `</span> <span class="deletions">-` + strconv.Itoa(file.deletions) + `</span></div></div>`)
	if file.binary {
		b.WriteString(`<div class="empty">Binary file changed.</div>`)
	} else {
		b.WriteString(`<pre class="diff">`)
		for _, line := range file.diff {
			b.WriteString(`<span class="diff-line ` + html.EscapeString(line.kind) + `">` + html.EscapeString(line.text) + `</span>`)
		}
		b.WriteString(`</pre>`)
	}
	b.WriteString(`</section>`)
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
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
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

const webCSS = `
:root { color-scheme: light dark; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; --border: color-mix(in srgb, CanvasText 16%, transparent); --muted: color-mix(in srgb, CanvasText 62%, transparent); --panel: color-mix(in srgb, Canvas 94%, CanvasText 6%); }
* { box-sizing: border-box; }
body { margin: 0; background: color-mix(in srgb, Canvas 96%, CanvasText 4%); color: CanvasText; }
a { color: #0969da; text-decoration: none; }
a:hover { text-decoration: underline; }
code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.layout { max-width: 1180px; margin: 0 auto; padding: 20px 24px 36px; }
.repo-header { border-bottom: 1px solid var(--border); margin-bottom: 16px; padding: 8px 0 0; }
.repo-topline { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.repo-kicker { color: var(--muted); font-size: 12px; font-weight: 700; text-transform: uppercase; }
h1 { font-size: 22px; line-height: 1.25; margin: 2px 0 12px; overflow-wrap: anywhere; }
h2 { font-size: 21px; line-height: 1.3; margin: 0 0 12px; }
.ref-pill { border: 1px solid var(--border); border-radius: 999px; padding: 5px 10px; font-size: 13px; background: Canvas; max-width: 280px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ref-selector { display: grid; gap: 4px; min-width: 210px; max-width: 320px; }
.ref-selector span { color: var(--muted); font-size: 12px; font-weight: 700; text-transform: uppercase; }
.ref-selector select { min-height: 34px; border: 1px solid var(--border); border-radius: 6px; background: Canvas; color: CanvasText; padding: 0 32px 0 10px; font: inherit; font-size: 13px; max-width: 100%; }
.tabs { display: flex; gap: 6px; margin-top: 4px; }
.tabs a { display: inline-flex; align-items: center; min-height: 36px; padding: 0 12px; border-bottom: 2px solid transparent; color: CanvasText; font-weight: 600; }
.tabs a:first-child { border-bottom-color: #fd8c73; }
.crumbs { margin: 10px 0 12px; color: var(--muted); font-size: 13px; overflow-wrap: anywhere; }
.panel, .clone-panel { background: Canvas; border: 1px solid var(--border); border-radius: 8px; margin: 14px 0; overflow: hidden; }
.panel-title { font-size: 14px; font-weight: 700; padding: 12px 14px; border-bottom: 1px solid var(--border); background: var(--panel); }
.clone-panel { display: grid; grid-template-columns: minmax(180px, 270px) 1fr; gap: 14px; padding: 14px; align-items: start; }
.clone-panel .panel-title { padding: 0; border: 0; background: transparent; }
.clone-grid { display: grid; gap: 8px; min-width: 0; }
.clone-row { display: grid; grid-template-columns: 64px minmax(0, 1fr) auto; gap: 8px; align-items: center; }
.clone-row span { color: var(--muted); font-size: 12px; font-weight: 700; text-transform: uppercase; }
.clone-row code { border: 1px solid var(--border); background: var(--panel); min-height: 34px; padding: 8px 10px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.copy-button, .button-link { border: 1px solid var(--border); background: Canvas; color: CanvasText; border-radius: 6px; min-height: 34px; padding: 0 10px; font: inherit; font-size: 13px; display: inline-flex; align-items: center; justify-content: center; cursor: pointer; }
.copy-button:hover, .button-link:hover { background: var(--panel); text-decoration: none; }
.commit-strip { display: flex; justify-content: space-between; gap: 14px; align-items: center; padding: 12px 14px; }
.commit-subject { color: CanvasText; font-weight: 700; }
.muted, .meta { color: var(--muted); font-size: 13px; }
.files { border-collapse: collapse; width: 100%; }
.files td { border-top: 1px solid var(--border); padding: 10px 12px; vertical-align: top; }
.files tr:first-child td { border-top: 0; }
.kind { width: 54px; color: var(--muted); text-transform: uppercase; font-size: 11px; font-weight: 700; }
.hash { width: 112px; text-align: right; color: var(--muted); font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; }
.readme-panel .panel-title { border-bottom: 1px solid var(--border); }
.blob, .readme pre, .commit-message { margin: 0; padding: 14px; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; font-size: 13px; line-height: 1.5; background: Canvas; }
.blob-toolbar { display: flex; justify-content: space-between; gap: 14px; align-items: center; padding: 12px 14px; border-bottom: 1px solid var(--border); background: var(--panel); }
.blob-toolbar .panel-title { padding: 0; border: 0; background: transparent; overflow-wrap: anywhere; }
.actions { display: flex; gap: 8px; align-items: center; margin: 10px 14px 14px; font-size: 13px; }
.blob-toolbar .actions { margin: 0; }
.commits { list-style: none; margin: 0; padding: 0; }
.commits li { display: flex; justify-content: space-between; gap: 16px; padding: 12px 14px; border-top: 1px solid var(--border); }
.commits li:first-child { border-top: 0; }
.commit-detail { padding: 16px; }
.commit-detail .commit-message { border: 1px solid var(--border); border-radius: 6px; margin-bottom: 14px; background: var(--panel); }
.metadata-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px; }
.metadata-grid div { min-width: 0; }
.metadata-grid span { display: block; color: var(--muted); font-size: 12px; font-weight: 700; text-transform: uppercase; margin-bottom: 3px; }
.metadata-grid small { display: block; color: var(--muted); overflow-wrap: anywhere; }
.diff-summary { display: flex; gap: 12px; align-items: center; padding: 12px 14px; border-bottom: 1px solid var(--border); }
.additions { color: #1a7f37; font-weight: 700; }
.deletions { color: #cf222e; font-weight: 700; }
.changed-files .diff-stat { width: 120px; text-align: right; }
.diff-file { scroll-margin-top: 16px; }
.diff-header { display: flex; justify-content: space-between; gap: 12px; align-items: center; padding: 12px 14px; border-bottom: 1px solid var(--border); background: var(--panel); overflow-wrap: anywhere; }
.diff { margin: 0; overflow: auto; font-size: 12px; line-height: 1.45; }
.diff-line { display: block; padding: 0 12px; white-space: pre; }
.diff-line.add { background: color-mix(in srgb, #2da44e 16%, Canvas); }
.diff-line.del { background: color-mix(in srgb, #cf222e 14%, Canvas); }
.diff-line.hunk { background: color-mix(in srgb, #0969da 12%, Canvas); color: #0969da; }
.empty { margin: 14px; border: 1px solid var(--border); border-radius: 6px; padding: 14px; color: var(--muted); }
.sr-only { position: absolute; left: -9999px; width: 1px; height: 1px; overflow: hidden; }
@media (max-width: 720px) { .layout { padding: 14px; } .repo-topline, .commit-strip, .blob-toolbar, .diff-header { flex-direction: column; align-items: stretch; } .ref-selector { max-width: none; } .clone-panel { grid-template-columns: 1fr; } .clone-row { grid-template-columns: 1fr; } .hash { display: none; } .commits li { flex-direction: column; gap: 6px; } .metadata-grid { grid-template-columns: 1fr; } }
`

const webJS = `<script>
document.addEventListener('click', function (event) {
  const button = event.target.closest('[data-copy-target]');
  if (!button) return;
  const target = document.getElementById(button.getAttribute('data-copy-target'));
  if (!target) return;
  const value = target.value !== undefined ? target.value : target.textContent;
  navigator.clipboard.writeText(value).then(function () {
    const old = button.textContent;
    button.textContent = 'Copied';
    window.setTimeout(function () { button.textContent = old; }, 1200);
  });
});
document.addEventListener('change', function (event) {
  const select = event.target.closest('[data-ref-selector]');
  if (!select) return;
  const url = new URL(window.location.href);
  url.searchParams.set('ref', select.value);
  window.location.href = url.toString();
});
</script>`
