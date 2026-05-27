package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

const (
	gitObjectCommit = "commit"
	gitObjectTree   = "tree"
	gitObjectBlob   = "blob"
	gitObjectTag    = "tag"
)

type gitObject struct {
	typ  string
	data []byte
}

type gitRemoteStore interface {
	read(ctx context.Context, path string) ([]byte, error)
	list(ctx context.Context, prefix string) ([]string, error)
}

type writableGitRemoteStore interface {
	gitRemoteStore
	write(ctx context.Context, path string, data []byte) error
	delete(ctx context.Context, path string) error
}

type fallbackGitRemoteStore struct {
	primary  gitRemoteStore
	fallback gitRemoteStore
}

type nativeGitRepo struct {
	cfg         config
	store       gitRemoteStore
	mu          sync.Mutex
	cache       map[string]gitObject
	offsetCache map[string]gitObject
	packs       []packIndex
}

type gcsGitStore struct {
	client *storage.Client
	bucket string
	prefix string
}

type localGitStore struct {
	root string
}

func (s *fallbackGitRemoteStore) read(ctx context.Context, path string) ([]byte, error) {
	data, err := s.primary.read(ctx, path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return data, err
	}
	return s.fallback.read(ctx, path)
}

func (s *fallbackGitRemoteStore) list(ctx context.Context, prefix string) ([]string, error) {
	paths, err := s.primary.list(ctx, prefix)
	if err == nil {
		return paths, nil
	}
	return s.fallback.list(ctx, prefix)
}

type packIndex struct {
	idxPath  string
	packPath string
	hashes   []string
	offsets  []uint64
}

type treeEntry struct {
	mode string
	name string
	hash string
	typ  string
}

type commitObject struct {
	hash               string
	tree               string
	parents            []string
	author             string
	email              string
	timestamp          int64
	committer          string
	committerEmail     string
	committerTimestamp int64
	subject            string
	body               string
}

func (r *nativeGitRepo) listFiles(ctx context.Context, args []string, stdout io.Writer) error {
	filter := ""
	if len(args) > 1 {
		return errors.New("ls accepts at most one prefix")
	}
	if len(args) == 1 {
		filter = strings.TrimPrefix(args[0], "/")
	}
	head, err := r.resolveRevision(ctx, branchRef(r.cfg.branch))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	commit, err := r.commit(ctx, head)
	if err != nil {
		return err
	}
	paths, err := r.listTree(ctx, commit.tree, "")
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		if filter == "" || strings.HasPrefix(path, filter) {
			fmt.Fprintln(stdout, path)
		}
	}
	return nil
}

func (r *nativeGitRepo) catFile(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("cat", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	commitish := flags.String("commit", "", "commit SHA")
	if err := flags.Parse(args); err != nil {
		return err
	}
	rest := flags.Args()
	if len(rest) != 1 {
		return errors.New("cat requires exactly one path")
	}
	revision := branchRef(r.cfg.branch)
	if *commitish != "" {
		revision = *commitish
	}
	hash, err := r.resolveRevision(ctx, revision)
	if err != nil {
		return err
	}
	commit, err := r.commit(ctx, hash)
	if err != nil {
		return err
	}
	blobHash, err := r.findPath(ctx, commit.tree, strings.TrimPrefix(rest[0], "/"))
	if err != nil {
		return err
	}
	obj, err := r.object(ctx, blobHash)
	if err != nil {
		return err
	}
	if obj.typ != gitObjectBlob {
		return fmt.Errorf("%s is not a blob", rest[0])
	}
	_, err = stdout.Write(obj.data)
	return err
}

func (r *nativeGitRepo) log(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("log", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	limit := flags.Int("limit", 50, "maximum commits to print")
	skip := flags.Int("skip", 0, "commits to skip")
	pathFilter := flags.String("path", "", "optional path filter")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("log received unexpected positional arguments")
	}
	head, err := r.resolveRevision(ctx, branchRef(r.cfg.branch))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	commits, err := r.walkCommits(ctx, head, *limit, *skip, strings.TrimPrefix(*pathFilter, "/"))
	if err != nil {
		return err
	}
	for _, commit := range commits {
		fmt.Fprintf(stdout, "%s\t%s\t%d\t%s\t%s\t%s\n", commit.hash, commit.tree, commit.timestamp, commit.author, commit.email, commit.subject)
	}
	return nil
}

func (r *nativeGitRepo) lsRemote(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("ls-remote", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	heads := flags.Bool("heads", false, "show heads only")
	tags := flags.Bool("tags", false, "show tags only")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("ls-remote does not accept positional arguments")
	}
	refs, err := r.refs(ctx)
	if err != nil {
		return err
	}
	var names []string
	for name := range refs {
		if *heads && !*tags && !strings.HasPrefix(name, "refs/heads/") {
			continue
		}
		if *tags && !*heads && !strings.HasPrefix(name, "refs/tags/") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(stdout, "%s %s\n", refs[name], name)
	}
	return nil
}

func (r *nativeGitRepo) initWorktree(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	branch := flags.String("branch", r.cfg.branch, "branch to check out")
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
	cfg := r.cfg
	if *branch != "" {
		cfg.branch = *branch
	}
	if cfg.branch == "" {
		cfg.branch = defaultBranch
	}
	if _, err := runGit(absTarget, "init", "--initial-branch", shortBranchName(cfg.branch)); err != nil {
		if _, fallbackErr := runGit(absTarget, "init"); fallbackErr != nil {
			return err
		}
	}
	if err := writeBucketGitConfig(absTarget, cfg); err != nil {
		return err
	}
	if err := setGitOrigin(absTarget, originForConfig(cfg)); err != nil {
		return err
	}
	if err := setGitBranchTracking(absTarget, cfg.branch, "origin"); err != nil {
		return err
	}
	cloneRepo := *r
	cloneRepo.cfg = cfg
	if err := cloneRepo.fetchIntoWorktree(ctx, absTarget, true, io.Discard); err != nil {
		return err
	}
	remoteBranch := "refs/remotes/bucketgit/" + shortBranchName(cfg.branch)
	if _, err := runGit(absTarget, "rev-parse", "--verify", remoteBranch); err == nil {
		if _, err := runGit(absTarget, "checkout", "--quiet", "-B", shortBranchName(cfg.branch), remoteBranch); err != nil {
			return err
		}
	} else {
		if _, err := runGit(absTarget, "checkout", "--quiet", "-B", shortBranchName(cfg.branch)); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "Cloned %s into '%s'\n", originForConfig(cfg), absTarget)
	return nil
}

func (r *nativeGitRepo) fetch(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("fetch", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	tags := flags.Bool("tags", true, "fetch tags")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("fetch does not accept positional arguments")
	}
	worktree, err := requireWorktree(".")
	if err != nil {
		return err
	}
	return r.fetchIntoWorktree(ctx, worktree, *tags, stdout)
}

func (r *nativeGitRepo) fetchIntoWorktree(ctx context.Context, worktree string, tags bool, stdout io.Writer) error {
	gitDir, err := localGitDir(worktree)
	if err != nil {
		return err
	}
	if err := r.copyRemoteObjectsToLocal(ctx, gitDir); err != nil {
		return err
	}
	return r.fetchRefsIntoWorktree(ctx, worktree, tags, stdout)
}

func (r *nativeGitRepo) fetchRefsIntoWorktree(ctx context.Context, worktree string, tags bool, stdout io.Writer) error {
	gitDir, err := localGitDir(worktree)
	if err != nil {
		return err
	}
	refs, err := r.refs(ctx)
	if err != nil {
		return err
	}
	var names []string
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	var updates []string
	for _, name := range names {
		hash := refs[name]
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			localRef := filepath.Join(gitDir, filepath.FromSlash("refs/remotes/bucketgit/"+strings.TrimPrefix(name, "refs/heads/")))
			oldHash := readRefFile(localRef)
			if err := writeRefFile(localRef, hash); err != nil {
				return err
			}
			short := strings.TrimPrefix(name, "refs/heads/")
			switch {
			case oldHash == "":
				updates = append(updates, fmt.Sprintf(" * [new branch]      %s     -> bucketgit/%s", short, short))
			case oldHash != hash:
				updates = append(updates, fmt.Sprintf("   %s..%s  %s     -> bucketgit/%s", shortHash(oldHash), shortHash(hash), short, short))
			}
		case tags && strings.HasPrefix(name, "refs/tags/"):
			localRef := filepath.Join(gitDir, filepath.FromSlash(name))
			oldHash := readRefFile(localRef)
			if err := writeRefFile(localRef, hash); err != nil {
				return err
			}
			if oldHash == "" {
				short := strings.TrimPrefix(name, "refs/tags/")
				updates = append(updates, fmt.Sprintf(" * [new tag]         %s     -> %s", short, short))
			}
		}
	}
	if len(updates) > 0 {
		fmt.Fprintf(stdout, "From %s\n", originForConfig(r.cfg))
		for _, update := range updates {
			fmt.Fprintln(stdout, update)
		}
	}
	return nil
}

func (r *nativeGitRepo) pull(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("pull", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	rebase := flags.Bool("rebase", false, "rebase instead of merge")
	if err := flags.Parse(args); err != nil {
		return err
	}
	rest := flags.Args()
	if len(rest) > 1 {
		return errors.New("pull accepts at most one branch")
	}
	worktree, err := requireWorktree(".")
	if err != nil {
		return err
	}
	if *rebase {
		return unsupportedCommand("rebase")
	}
	if err := r.fetchIntoWorktree(ctx, worktree, true, stdout); err != nil {
		return err
	}
	localRepo, err := openLocalRepository(worktree)
	if err != nil {
		return err
	}
	branch := ""
	if len(rest) == 1 {
		branch = rest[0]
	} else {
		branch = firstNonEmpty(localRepo.currentBranch(), r.cfg.branch, defaultBranch)
	}
	remoteHash, err := localRepo.resolveRevision("refs/remotes/bucketgit/" + shortBranchName(branch))
	if err != nil {
		return err
	}
	currentHash, _ := localRepo.resolveRevision("HEAD")
	if currentHash == remoteHash {
		fmt.Fprintln(stdout, "Already up to date.")
		return nil
	}
	if currentHash != "" {
		ancestor, err := localRepo.isAncestor(currentHash, remoteHash)
		if err != nil {
			return err
		}
		if !ancestor {
			return unsupportedCommand("merge")
		}
	}
	if err := localRepo.updateHEAD(remoteHash); err != nil {
		return err
	}
	before, _ := localRepo.treeFilesForCommit(currentHash)
	after, _ := localRepo.treeFilesForCommit(remoteHash)
	if err := localRepo.checkoutCommit(remoteHash); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Updating %s..%s\n", currentHash[:7], remoteHash[:7])
	fmt.Fprintln(stdout, "Fast-forward")
	localRepo.printChangeSummary(stdout, before, after, true)
	return nil
}

func readRefFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	hash := strings.TrimSpace(string(data))
	if !isHexHash(hash) {
		return ""
	}
	return hash
}

func (r *nativeGitRepo) push(ctx context.Context, args []string, stdout io.Writer) error {
	opts, err := parsePushArgs(args)
	if err != nil {
		return err
	}
	worktree, err := requireWorktree(".")
	if err != nil {
		return err
	}
	return r.pushWorktree(ctx, worktree, opts, stdout)
}

func (r *nativeGitRepo) pushWorktree(ctx context.Context, worktree string, opts pushOptions, stdout io.Writer) error {
	store, ok := r.store.(writableGitRemoteStore)
	if !ok {
		return errors.New("push requires a writable GCS store")
	}
	gitDir, err := localGitDir(worktree)
	if err != nil {
		return err
	}
	localRepo, err := openLocalRepository(worktree)
	if err != nil {
		return err
	}
	brokerURL := ""
	if !opts.skipBroker {
		brokerURL = optionalBrokerURLForPush()
	}
	if brokerURL != "" {
		cfg := r.cfg
		cfg.brokerURL = brokerURL
		if err := brokerRequirePush(ctx, cfg); err != nil {
			return brokerPushError(err)
		}
	}
	if err := uploadLocalObjects(ctx, store, gitDir); err != nil {
		return err
	}
	refs, err := r.refs(ctx)
	if err != nil {
		return err
	}
	updateRef := func(ref, oldHash, newHash string) error {
		if brokerURL != "" {
			if err := brokerUpdateRefWithOverride(brokerURL, r.cfg, ref, oldHash, newHash, opts.force); err != nil {
				return brokerPushError(err)
			}
			return nil
		}
		if newHash == zeroObjectID() {
			return store.delete(ctx, ref)
		}
		return store.write(ctx, ref, []byte(newHash+"\n"))
	}
	if opts.delete {
		for _, ref := range opts.refs {
			normalized := normalizeDeleteRef(ref)
			if err := updateRef(normalized, firstNonEmpty(refs[normalized], zeroObjectID()), zeroObjectID()); err != nil {
				return err
			}
			if err := updateLocalRemoteTrackingRef(gitDir, normalized, zeroObjectID()); err != nil {
				return err
			}
			if strings.HasPrefix(normalized, "refs/heads/") {
				if err := unsetGitBranchTracking(worktree, strings.TrimPrefix(normalized, "refs/heads/")); err != nil {
					return err
				}
			}
		}
		fmt.Fprintf(stdout, "To %s\n", originForConfig(r.cfg))
		for _, ref := range opts.refs {
			fmt.Fprintf(stdout, " - [deleted]         %s\n", shortRefName(normalizeDeleteRef(ref)))
		}
		return nil
	}
	var updates []string
	if len(opts.refs) == 0 && !opts.tags {
		hash, err := gitRevParse(worktree, "HEAD")
		if err != nil {
			return err
		}
		branch := firstNonEmpty(localRepo.currentBranch(), r.cfg.branch, defaultBranch)
		ref := branchRef(branch)
		oldHash := pushOldHash(gitDir, refs, ref)
		if oldHash != hash {
			if err := updateRef(ref, oldHash, hash); err != nil {
				return err
			}
			if err := updateLocalRemoteTrackingRef(gitDir, ref, hash); err != nil {
				return err
			}
			if err := setGitBranchTrackingIfOrigin(worktree, branch); err != nil {
				return err
			}
			updates = append(updates, pushUpdateLine(oldHash, hash, branch, branch))
		}
	} else {
		for _, refspec := range opts.refs {
			src, dst, ok := strings.Cut(refspec, ":")
			if !ok {
				src = refspec
				dst = branchRef(refspec)
			}
			hash, err := gitRevParse(worktree, src)
			if err != nil {
				return err
			}
			ref := normalizeDestinationRef(dst)
			oldHash := pushOldHash(gitDir, refs, ref)
			if oldHash == hash {
				continue
			}
			if err := updateRef(ref, oldHash, hash); err != nil {
				return err
			}
			if err := updateLocalRemoteTrackingRef(gitDir, ref, hash); err != nil {
				return err
			}
			if strings.HasPrefix(ref, "refs/heads/") {
				if err := setGitBranchTrackingIfOrigin(worktree, strings.TrimPrefix(ref, "refs/heads/")); err != nil {
					return err
				}
			}
			updates = append(updates, pushUpdateLine(oldHash, hash, shortRefName(src), shortRefName(normalizeDestinationRef(dst))))
		}
	}
	if opts.tags {
		tags, err := localShowRefs(worktree, "--tags")
		if err != nil {
			return err
		}
		for ref, hash := range tags {
			oldHash := firstNonEmpty(refs[ref], zeroObjectID())
			if oldHash == hash {
				continue
			}
			if err := updateRef(ref, oldHash, hash); err != nil {
				return err
			}
			if oldHash == zeroObjectID() {
				updates = append(updates, fmt.Sprintf(" * [new tag]         %s -> %s", shortRefName(ref), shortRefName(ref)))
			} else {
				updates = append(updates, fmt.Sprintf("   %s..%s  %s -> %s", shortHash(oldHash), shortHash(hash), shortRefName(ref), shortRefName(ref)))
			}
		}
	}
	if len(updates) == 0 {
		fmt.Fprintln(stdout, "Everything up-to-date")
		return nil
	}
	fmt.Fprintf(stdout, "To %s\n", originForConfig(r.cfg))
	for _, line := range updates {
		fmt.Fprintln(stdout, line)
	}
	return nil
}

func pushOldHash(gitDir string, refs map[string]string, ref string) string {
	if hash := refs[ref]; hash != "" {
		return hash
	}
	if hash := localRemoteTrackingHash(gitDir, ref); hash != "" {
		return hash
	}
	return zeroObjectID()
}

func pushUpdateLine(oldHash, newHash, src, dst string) string {
	if oldHash == zeroObjectID() {
		return fmt.Sprintf(" * [new branch]      %s -> %s", src, dst)
	}
	return fmt.Sprintf("   %s..%s  %s -> %s", shortHash(oldHash), shortHash(newHash), src, dst)
}

func localRemoteTrackingHash(gitDir, ref string) string {
	if !strings.HasPrefix(ref, "refs/heads/") {
		return ""
	}
	path := filepath.Join(gitDir, filepath.FromSlash("refs/remotes/bucketgit/"+strings.TrimPrefix(ref, "refs/heads/")))
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	hash := strings.TrimSpace(string(data))
	if !isHexHash(hash) {
		return ""
	}
	return hash
}

func updateLocalRemoteTrackingRef(gitDir, ref, hash string) error {
	if !strings.HasPrefix(ref, "refs/heads/") {
		return nil
	}
	path := filepath.Join(gitDir, filepath.FromSlash("refs/remotes/bucketgit/"+strings.TrimPrefix(ref, "refs/heads/")))
	if hash == zeroObjectID() {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeRefFile(path, hash)
}

func (r *nativeGitRepo) putFile(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	opts, err := parsePutArgs(args)
	if err != nil {
		return err
	}
	if opts.path == "" {
		return errors.New("put requires exactly one destination path")
	}
	if opts.message == "" || opts.author == "" || opts.email == "" {
		return errors.New("put requires -m, --author, and --email")
	}
	tmpDir, err := os.MkdirTemp("", "bgit-worktree-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	if err := r.initWorktree(ctx, []string{tmpDir}, io.Discard); err != nil {
		return err
	}
	relPath := strings.TrimPrefix(opts.path, "/")
	target := filepath.Join(tmpDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if opts.file == "" || opts.file == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
	} else {
		data, err := os.ReadFile(opts.file)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
	}
	if _, err := runGit(tmpDir, "add", "--", relPath); err != nil {
		return err
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME="+opts.author,
		"GIT_AUTHOR_EMAIL="+opts.email,
		"GIT_COMMITTER_NAME="+opts.author,
		"GIT_COMMITTER_EMAIL="+opts.email,
	)
	if _, err := runGitEnv(tmpDir, env, "commit", "--quiet", "-m", opts.message); err != nil {
		return err
	}
	hash, err := gitRevParse(tmpDir, "HEAD")
	if err != nil {
		return err
	}
	if err := r.pushWorktree(ctx, tmpDir, pushOptions{}, io.Discard); err != nil {
		return err
	}
	fmt.Fprintln(stdout, hash)
	return nil
}

type putOptions struct {
	path    string
	file    string
	message string
	author  string
	email   string
}

func parsePutArgs(args []string) (putOptions, error) {
	var opts putOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		key, value, hasValue := strings.Cut(arg, "=")
		switch key {
		case "--file":
			if !hasValue {
				i++
				if i >= len(args) {
					return opts, errors.New("--file requires a value")
				}
				value = args[i]
			}
			opts.file = value
		case "-m":
			i++
			if i >= len(args) {
				return opts, errors.New("-m requires a value")
			}
			opts.message = args[i]
		case "--author":
			if !hasValue {
				i++
				if i >= len(args) {
					return opts, errors.New("--author requires a value")
				}
				value = args[i]
			}
			opts.author = value
		case "--email":
			if !hasValue {
				i++
				if i >= len(args) {
					return opts, errors.New("--email requires a value")
				}
				value = args[i]
			}
			opts.email = value
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unsupported put option %s", arg)
			}
			if opts.path != "" {
				return opts, errors.New("put requires exactly one destination path")
			}
			opts.path = arg
		}
	}
	return opts, nil
}

func (r *nativeGitRepo) resolveRevision(ctx context.Context, revision string) (string, error) {
	if isHexHash(revision) {
		return revision, nil
	}
	if base, distance, ok := parseAncestorRevision(revision); ok {
		hash, err := r.resolveRevision(ctx, base)
		if err != nil {
			return "", err
		}
		for i := 0; i < distance; i++ {
			commit, err := r.commit(ctx, hash)
			if err != nil {
				return "", err
			}
			if len(commit.parents) == 0 {
				return "", fs.ErrNotExist
			}
			hash = commit.parents[0]
		}
		return hash, nil
	}
	refs, err := r.refs(ctx)
	if err != nil {
		return "", err
	}
	if hash, ok := refs[revision]; ok {
		return hash, nil
	}
	if !strings.HasPrefix(revision, "refs/") {
		if hash, ok := refs[branchRef(revision)]; ok {
			return hash, nil
		}
		if hash, ok := refs["refs/tags/"+revision]; ok {
			return hash, nil
		}
	}
	return "", fs.ErrNotExist
}

func (r *nativeGitRepo) refs(ctx context.Context) (map[string]string, error) {
	refs := map[string]string{}
	for _, dir := range []string{"refs/heads", "refs/tags"} {
		paths, err := r.store.list(ctx, dir)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			data, err := r.store.read(ctx, path)
			if err != nil {
				return nil, err
			}
			hash := strings.TrimSpace(string(data))
			if isHexHash(hash) {
				refs[path] = hash
			}
		}
	}
	data, err := r.store.read(ctx, "packed-refs")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) == 2 && isHexHash(parts[0]) {
				refs[parts[1]] = parts[0]
			}
		}
	}
	return refs, nil
}

func (r *nativeGitRepo) object(ctx context.Context, hash string) (gitObject, error) {
	hash = strings.TrimSpace(hash)
	r.mu.Lock()
	defer r.mu.Unlock()
	if obj, ok := r.cache[hash]; ok {
		return obj, nil
	}
	obj, err := r.looseObject(ctx, hash)
	if err == nil {
		r.cache[hash] = obj
		return obj, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return gitObject{}, err
	}
	obj, err = r.packedObject(ctx, hash)
	if err != nil {
		return gitObject{}, err
	}
	r.cache[hash] = obj
	return obj, nil
}

func (r *nativeGitRepo) looseObject(ctx context.Context, hash string) (gitObject, error) {
	if len(hash) != 40 {
		return gitObject{}, fs.ErrNotExist
	}
	data, err := r.store.read(ctx, "objects/"+hash[:2]+"/"+hash[2:])
	if err != nil {
		return gitObject{}, err
	}
	reader, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return gitObject{}, err
	}
	defer reader.Close()
	raw, err := io.ReadAll(reader)
	if err != nil {
		return gitObject{}, err
	}
	nul := bytes.IndexByte(raw, 0)
	if nul < 0 {
		return gitObject{}, errors.New("invalid git object header")
	}
	header := strings.Split(string(raw[:nul]), " ")
	if len(header) != 2 {
		return gitObject{}, errors.New("invalid git object header")
	}
	return gitObject{typ: header[0], data: raw[nul+1:]}, nil
}

func (r *nativeGitRepo) packedObject(ctx context.Context, hash string) (gitObject, error) {
	if err := r.loadPackIndexes(ctx); err != nil {
		return gitObject{}, err
	}
	for _, idx := range r.packs {
		i := sort.SearchStrings(idx.hashes, hash)
		if i < len(idx.hashes) && idx.hashes[i] == hash {
			return r.objectAtPackOffset(ctx, idx, idx.offsets[i])
		}
	}
	return gitObject{}, fs.ErrNotExist
}

func (r *nativeGitRepo) loadPackIndexes(ctx context.Context) error {
	if r.packs != nil {
		return nil
	}
	paths, err := r.store.list(ctx, "objects/pack")
	if err != nil {
		return err
	}
	for _, path := range paths {
		if !strings.HasSuffix(path, ".idx") {
			continue
		}
		idx, err := r.parsePackIndex(ctx, path)
		if err != nil {
			return err
		}
		r.packs = append(r.packs, idx)
	}
	return nil
}

func (r *nativeGitRepo) parsePackIndex(ctx context.Context, path string) (packIndex, error) {
	data, err := r.store.read(ctx, path)
	if err != nil {
		return packIndex{}, err
	}
	hashes, offsets, err := parsePackIndexData(data)
	if err != nil {
		return packIndex{}, err
	}
	return packIndex{idxPath: path, packPath: strings.TrimSuffix(path, ".idx") + ".pack", hashes: hashes, offsets: offsets}, nil
}

func parsePackIndexData(data []byte) ([]string, []uint64, error) {
	if len(data) < 8 || !bytes.Equal(data[:4], []byte{0xff, 't', 'O', 'c'}) {
		return nil, nil, errors.New("unsupported pack index format")
	}
	if version := binary.BigEndian.Uint32(data[4:8]); version != 2 {
		return nil, nil, fmt.Errorf("unsupported pack index version %d", version)
	}
	pos := 8
	if len(data) < pos+256*4 {
		return nil, nil, errors.New("truncated pack index fanout")
	}
	count := int(binary.BigEndian.Uint32(data[pos+255*4 : pos+256*4]))
	pos += 256 * 4
	if len(data) < pos+count*20 {
		return nil, nil, errors.New("truncated pack index hashes")
	}
	hashes := make([]string, count)
	for i := 0; i < count; i++ {
		hashes[i] = hex.EncodeToString(data[pos+i*20 : pos+(i+1)*20])
	}
	pos += count * 20
	pos += count * 4
	if len(data) < pos+count*4 {
		return nil, nil, errors.New("truncated pack index offsets")
	}
	rawOffsets := make([]uint32, count)
	for i := 0; i < count; i++ {
		rawOffsets[i] = binary.BigEndian.Uint32(data[pos+i*4 : pos+(i+1)*4])
	}
	pos += count * 4
	offsets := make([]uint64, count)
	for i, raw := range rawOffsets {
		if raw&0x80000000 == 0 {
			offsets[i] = uint64(raw)
			continue
		}
		largeIndex := int(raw & 0x7fffffff)
		if len(data) < pos+(largeIndex+1)*8 {
			return nil, nil, errors.New("truncated pack index large offsets")
		}
		offsets[i] = binary.BigEndian.Uint64(data[pos+largeIndex*8 : pos+(largeIndex+1)*8])
	}
	return hashes, offsets, nil
}

func (r *nativeGitRepo) objectAtPackOffset(ctx context.Context, idx packIndex, offset uint64) (gitObject, error) {
	key := fmt.Sprintf("%s:%d", idx.packPath, offset)
	if obj, ok := r.offsetCache[key]; ok {
		return obj, nil
	}
	pack, err := r.store.read(ctx, idx.packPath)
	if err != nil {
		return gitObject{}, err
	}
	obj, err := r.decodePackedObject(ctx, idx, pack, offset)
	if err != nil {
		return gitObject{}, err
	}
	r.offsetCache[key] = obj
	return obj, nil
}

func (r *nativeGitRepo) decodePackedObject(ctx context.Context, idx packIndex, pack []byte, offset uint64) (gitObject, error) {
	if len(pack) < 12 || !bytes.Equal(pack[:4], []byte("PACK")) {
		return gitObject{}, errors.New("invalid pack file")
	}
	pos := int(offset)
	typ, headerLen, err := parsePackObjectHeader(pack[pos:])
	if err != nil {
		return gitObject{}, err
	}
	bodyStart := pos + headerLen
	switch typ {
	case 1, 2, 3, 4:
		data, err := inflatePackedData(pack[bodyStart:])
		if err != nil {
			return gitObject{}, err
		}
		return gitObject{typ: packTypeName(typ), data: data}, nil
	case 6:
		baseOffset, n, err := parseOFSDeltaBase(pack[bodyStart:], uint64(pos))
		if err != nil {
			return gitObject{}, err
		}
		delta, err := inflatePackedData(pack[bodyStart+n:])
		if err != nil {
			return gitObject{}, err
		}
		base, err := r.objectAtPackOffset(ctx, idx, baseOffset)
		if err != nil {
			return gitObject{}, err
		}
		data, err := applyDelta(base.data, delta)
		if err != nil {
			return gitObject{}, err
		}
		return gitObject{typ: base.typ, data: data}, nil
	case 7:
		if len(pack[bodyStart:]) < 20 {
			return gitObject{}, errors.New("truncated ref delta")
		}
		baseHash := hex.EncodeToString(pack[bodyStart : bodyStart+20])
		delta, err := inflatePackedData(pack[bodyStart+20:])
		if err != nil {
			return gitObject{}, err
		}
		base, err := r.object(ctx, baseHash)
		if err != nil {
			return gitObject{}, err
		}
		data, err := applyDelta(base.data, delta)
		if err != nil {
			return gitObject{}, err
		}
		return gitObject{typ: base.typ, data: data}, nil
	default:
		return gitObject{}, fmt.Errorf("unsupported pack object type %d", typ)
	}
}

func parsePackObjectHeader(data []byte) (int, int, error) {
	if len(data) == 0 {
		return 0, 0, errors.New("truncated pack object header")
	}
	b := data[0]
	typ := int((b >> 4) & 0x7)
	pos := 1
	for b&0x80 != 0 {
		if pos >= len(data) {
			return 0, 0, errors.New("truncated pack object header")
		}
		b = data[pos]
		pos++
	}
	return typ, pos, nil
}

func inflatePackedData(data []byte) ([]byte, error) {
	reader, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func packTypeName(typ int) string {
	switch typ {
	case 1:
		return gitObjectCommit
	case 2:
		return gitObjectTree
	case 3:
		return gitObjectBlob
	case 4:
		return gitObjectTag
	default:
		return ""
	}
}

func parseOFSDeltaBase(data []byte, current uint64) (uint64, int, error) {
	if len(data) == 0 {
		return 0, 0, errors.New("truncated ofs delta")
	}
	c := data[0]
	offset := uint64(c & 0x7f)
	pos := 1
	for c&0x80 != 0 {
		if pos >= len(data) {
			return 0, 0, errors.New("truncated ofs delta")
		}
		c = data[pos]
		pos++
		offset = ((offset + 1) << 7) | uint64(c&0x7f)
	}
	if offset > current {
		return 0, 0, errors.New("invalid ofs delta base")
	}
	return current - offset, pos, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	if _, n, err := readDeltaVarint(delta[pos:]); err != nil {
		return nil, err
	} else {
		pos += n
	}
	resultSize, n, err := readDeltaVarint(delta[pos:])
	if err != nil {
		return nil, err
	}
	pos += n
	out := make([]byte, 0, resultSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			cpOffset := 0
			cpSize := 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					cpOffset |= int(delta[pos]) << (8 * i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(1<<uint(4+i)) != 0 {
					cpSize |= int(delta[pos]) << (8 * i)
					pos++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOffset+cpSize > len(base) {
				return nil, errors.New("delta copy exceeds base object")
			}
			out = append(out, base[cpOffset:cpOffset+cpSize]...)
			continue
		}
		if op == 0 {
			return nil, errors.New("invalid delta opcode")
		}
		size := int(op)
		if pos+size > len(delta) {
			return nil, errors.New("delta insert exceeds delta size")
		}
		out = append(out, delta[pos:pos+size]...)
		pos += size
	}
	if len(out) != resultSize {
		return nil, errors.New("delta result size mismatch")
	}
	return out, nil
}

func readDeltaVarint(data []byte) (int, int, error) {
	result := 0
	shift := 0
	for i, b := range data {
		result |= int(b&0x7f) << shift
		if b&0x80 == 0 {
			return result, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, errors.New("truncated delta varint")
}

func (r *nativeGitRepo) commit(ctx context.Context, hash string) (commitObject, error) {
	obj, err := r.object(ctx, hash)
	if err != nil {
		return commitObject{}, err
	}
	if obj.typ == gitObjectTag {
		target, err := parseTagTarget(obj.data)
		if err != nil {
			return commitObject{}, err
		}
		return r.commit(ctx, target)
	}
	if obj.typ != gitObjectCommit {
		return commitObject{}, fmt.Errorf("%s is not a commit", hash)
	}
	commit, err := parseCommit(hash, obj.data)
	if err != nil {
		return commitObject{}, err
	}
	return commit, nil
}

func parseCommit(hash string, data []byte) (commitObject, error) {
	commit := commitObject{hash: hash}
	text := string(data)
	header, message, _ := strings.Cut(text, "\n\n")
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			commit.tree = strings.TrimSpace(strings.TrimPrefix(line, "tree "))
		case strings.HasPrefix(line, "parent "):
			commit.parents = append(commit.parents, strings.TrimSpace(strings.TrimPrefix(line, "parent ")))
		case strings.HasPrefix(line, "author "):
			commit.author, commit.email, commit.timestamp = parseSignature(strings.TrimPrefix(line, "author "))
		case strings.HasPrefix(line, "committer "):
			commit.committer, commit.committerEmail, commit.committerTimestamp = parseSignature(strings.TrimPrefix(line, "committer "))
		}
	}
	commit.body = strings.TrimRight(message, "\n")
	for _, line := range strings.Split(message, "\n") {
		if strings.TrimSpace(line) != "" {
			commit.subject = strings.TrimSpace(line)
			break
		}
	}
	if commit.tree == "" {
		return commitObject{}, errors.New("commit missing tree")
	}
	return commit, nil
}

func parseSignature(value string) (string, string, int64) {
	emailStart := strings.LastIndex(value, " <")
	emailEnd := strings.LastIndex(value, ">")
	if emailStart < 0 || emailEnd < emailStart {
		return strings.TrimSpace(value), "", 0
	}
	name := strings.TrimSpace(value[:emailStart])
	email := value[emailStart+2 : emailEnd]
	rest := strings.Fields(value[emailEnd+1:])
	var ts int64
	if len(rest) > 0 {
		ts, _ = strconv.ParseInt(rest[0], 10, 64)
	}
	return name, email, ts
}

func parseTagTarget(data []byte) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "object ")), nil
		}
	}
	return "", errors.New("tag missing object")
}

func (r *nativeGitRepo) listTree(ctx context.Context, treeHash, base string) ([]string, error) {
	entries, err := r.treeEntries(ctx, treeHash)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		path := entry.name
		if base != "" {
			path = base + "/" + entry.name
		}
		if entry.typ == gitObjectTree {
			child, err := r.listTree(ctx, entry.hash, path)
			if err != nil {
				return nil, err
			}
			paths = append(paths, child...)
		} else {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

func (r *nativeGitRepo) findPath(ctx context.Context, treeHash, path string) (string, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	current := treeHash
	for i, part := range parts {
		if part == "" {
			continue
		}
		entries, err := r.treeEntries(ctx, current)
		if err != nil {
			return "", err
		}
		found := false
		for _, entry := range entries {
			if entry.name != part {
				continue
			}
			if i == len(parts)-1 {
				return entry.hash, nil
			}
			if entry.typ != gitObjectTree {
				return "", fs.ErrNotExist
			}
			current = entry.hash
			found = true
			break
		}
		if !found {
			return "", fs.ErrNotExist
		}
	}
	return "", fs.ErrNotExist
}

func (r *nativeGitRepo) treeEntries(ctx context.Context, hash string) ([]treeEntry, error) {
	obj, err := r.object(ctx, hash)
	if err != nil {
		return nil, err
	}
	if obj.typ != gitObjectTree {
		return nil, fmt.Errorf("%s is not a tree", hash)
	}
	var entries []treeEntry
	data := obj.data
	for len(data) > 0 {
		space := bytes.IndexByte(data, ' ')
		if space < 0 {
			return nil, errors.New("invalid tree entry")
		}
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			return nil, errors.New("invalid tree entry")
		}
		nameEnd := space + 1 + nul
		if len(data) < nameEnd+1+20 {
			return nil, errors.New("truncated tree entry hash")
		}
		mode := string(data[:space])
		name := string(data[space+1 : nameEnd])
		hash := hex.EncodeToString(data[nameEnd+1 : nameEnd+1+20])
		typ := gitObjectBlob
		if mode == "40000" || mode == "040000" {
			typ = gitObjectTree
		}
		entries = append(entries, treeEntry{mode: mode, name: name, hash: hash, typ: typ})
		data = data[nameEnd+1+20:]
	}
	return entries, nil
}

func (r *nativeGitRepo) walkCommits(ctx context.Context, head string, limit, skip int, pathFilter string) ([]commitObject, error) {
	var commits []commitObject
	stack := []string{head}
	seen := map[string]struct{}{}
	for len(stack) > 0 {
		hash := stack[0]
		stack = stack[1:]
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		commit, err := r.commit(ctx, hash)
		if err != nil {
			return nil, err
		}
		for _, parent := range commit.parents {
			stack = append(stack, parent)
		}
		if pathFilter != "" {
			ok, err := r.commitTouchesPath(ctx, commit, pathFilter)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		if skip > 0 {
			skip--
			continue
		}
		commits = append(commits, commit)
		if limit > 0 && len(commits) >= limit {
			break
		}
	}
	sort.SliceStable(commits, func(i, j int) bool {
		return commits[i].timestamp > commits[j].timestamp
	})
	return commits, nil
}

func (r *nativeGitRepo) commitTouchesPath(ctx context.Context, commit commitObject, path string) (bool, error) {
	current, currentErr := r.findPath(ctx, commit.tree, path)
	if len(commit.parents) == 0 {
		return currentErr == nil, nil
	}
	for _, parentHash := range commit.parents {
		parent, err := r.commit(ctx, parentHash)
		if err != nil {
			return false, err
		}
		parentPath, parentErr := r.findPath(ctx, parent.tree, path)
		if currentErr != nil && parentErr != nil {
			continue
		}
		if currentErr != nil || parentErr != nil || current != parentPath {
			return true, nil
		}
	}
	return false, nil
}

func isHexHash(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, ch := range value {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

func (r *nativeGitRepo) copyRemoteObjectsToLocal(ctx context.Context, gitDir string) error {
	paths, err := r.store.list(ctx, "objects")
	if err != nil {
		return err
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, "objects/") {
			continue
		}
		if strings.Contains(path, "/tmp_") || strings.HasSuffix(path, ".lock") {
			continue
		}
		target := filepath.Join(gitDir, filepath.FromSlash(path))
		if _, err := os.Stat(target); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		data, err := r.store.read(ctx, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func uploadLocalObjects(ctx context.Context, store writableGitRemoteStore, gitDir string) error {
	existing, _ := remoteObjectSet(ctx, store)
	packed, packFiles, cleanup, err := packLocalObjects(gitDir)
	if err == nil {
		defer cleanup()
		for _, file := range packFiles {
			if existing[file.remotePath] {
				continue
			}
			data, err := os.ReadFile(file.localPath)
			if err != nil {
				return err
			}
			if err := store.write(ctx, file.remotePath, data); err != nil {
				return err
			}
		}
	}
	objectRoot := filepath.Join(gitDir, "objects")
	return filepath.WalkDir(objectRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == "info" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".lock") || strings.HasPrefix(entry.Name(), "tmp_") {
			return nil
		}
		rel, err := filepath.Rel(gitDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if existing[rel] {
			return nil
		}
		if hash, ok := looseObjectHashFromPath(rel); ok && packed[hash] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return store.write(ctx, rel, data)
	})
}

type localPackUploadFile struct {
	localPath  string
	remotePath string
}

func remoteObjectSet(ctx context.Context, store gitRemoteStore) (map[string]bool, error) {
	paths, err := store.list(ctx, "objects")
	if err != nil {
		return nil, err
	}
	existing := make(map[string]bool, len(paths))
	for _, path := range paths {
		existing[strings.TrimPrefix(path, "/")] = true
	}
	return existing, nil
}

func packLocalObjects(gitDir string) (map[string]bool, []localPackUploadFile, func(), error) {
	tmpDir, err := os.MkdirTemp("", "bgit-push-pack-*")
	if err != nil {
		return nil, nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	prefix := filepath.Join(tmpDir, "pack")
	cmd := exec.Command("git", "--git-dir", gitDir, "pack-objects", "--all", prefix)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		cleanup()
		return nil, nil, func() {}, fmt.Errorf("git pack-objects: %w %s", err, strings.TrimSpace(stderr.String()))
	}
	packID := strings.TrimSpace(string(out))
	if packID == "" {
		cleanup()
		return nil, nil, func() {}, errors.New("git pack-objects produced no pack")
	}
	base := "pack-" + packID
	packPath := filepath.Join(tmpDir, base+".pack")
	idxPath := filepath.Join(tmpDir, base+".idx")
	idxData, err := os.ReadFile(idxPath)
	if err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	hashes, _, err := parsePackIndexData(idxData)
	if err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	packed := make(map[string]bool, len(hashes))
	for _, hash := range hashes {
		packed[hash] = true
	}
	files := []localPackUploadFile{
		{localPath: packPath, remotePath: "objects/pack/" + base + ".pack"},
		{localPath: idxPath, remotePath: "objects/pack/" + base + ".idx"},
	}
	return packed, files, cleanup, nil
}

func looseObjectHashFromPath(path string) (string, bool) {
	path = strings.TrimPrefix(filepath.ToSlash(path), "/")
	if len(path) != len("objects/00/00000000000000000000000000000000000000") {
		return "", false
	}
	if !strings.HasPrefix(path, "objects/") || path[10] != '/' {
		return "", false
	}
	hash := path[8:10] + path[11:]
	return hash, isHexHash(hash)
}

func localGitDir(worktree string) (string, error) {
	_, gitDir, err := findLocalRepository(worktree)
	if err != nil {
		return "", err
	}
	return gitDir, nil
}

func writeRefFile(path, hash string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(hash+"\n"), 0o644)
}

func gitRevParse(worktree, rev string) (string, error) {
	repo, err := openLocalRepository(worktree)
	if err != nil {
		return "", err
	}
	return repo.resolveRevision(rev)
}

func localShowRefs(worktree string, args ...string) (map[string]string, error) {
	repo, err := openLocalRepository(worktree)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	prefixes := []string{"refs/heads", "refs/tags"}
	if len(args) > 0 {
		prefixes = nil
		for _, arg := range args {
			switch arg {
			case "--heads":
				prefixes = append(prefixes, "refs/heads")
			case "--tags":
				prefixes = append(prefixes, "refs/tags")
			default:
				return nil, fmt.Errorf("unsupported show-ref option %s", arg)
			}
		}
	}
	for _, prefix := range prefixes {
		names, err := repo.listRefs(prefix)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			hash, err := repo.readRef(name)
			if err == nil {
				refs[name] = hash
			}
		}
	}
	return refs, nil
}

func normalizeDestinationRef(ref string) string {
	if strings.HasPrefix(ref, "refs/") {
		return ref
	}
	if strings.HasPrefix(ref, "tags/") {
		return "refs/" + ref
	}
	return branchRef(ref)
}

func openNativeGitRepo(store gitRemoteStore, cfg config) *nativeGitRepo {
	return &nativeGitRepo{
		cfg:         cfg,
		store:       store,
		cache:       map[string]gitObject{},
		offsetCache: map[string]gitObject{},
	}
}

func newNativeGitRepoForStore(cfg config, store gitRemoteStore) *nativeGitRepo {
	return &nativeGitRepo{cfg: cfg, store: store, cache: map[string]gitObject{}, offsetCache: map[string]gitObject{}}
}

func (s *gcsGitStore) read(ctx context.Context, path string) ([]byte, error) {
	name := joinObjectName(s.prefix, path)
	reader, err := s.client.Bucket(s.bucket).Object(name).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, fs.ErrNotExist
		}
		return nil, gcsAccessError("read", s.bucket, name, err)
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func (s *gcsGitStore) list(ctx context.Context, prefix string) ([]string, error) {
	queryPrefix := objectPrefix(joinObjectName(s.prefix, prefix))
	it := s.client.Bucket(s.bucket).Objects(ctx, &storage.Query{Prefix: queryPrefix})
	var paths []string
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return paths, nil
		}
		if err != nil {
			return nil, gcsAccessError("list", s.bucket, queryPrefix, err)
		}
		rel := strings.TrimPrefix(attrs.Name, objectPrefix(s.prefix))
		if rel != "" && !strings.HasSuffix(rel, "/") {
			paths = append(paths, rel)
		}
	}
}

func (s *gcsGitStore) write(ctx context.Context, path string, data []byte) error {
	name := joinObjectName(s.prefix, path)
	writer := s.client.Bucket(s.bucket).Object(name).NewWriter(ctx)
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return gcsAccessError("write", s.bucket, name, err)
	}
	if err := writer.Close(); err != nil {
		return gcsAccessError("write", s.bucket, name, err)
	}
	return nil
}

func (s *gcsGitStore) delete(ctx context.Context, path string) error {
	name := joinObjectName(s.prefix, path)
	err := s.client.Bucket(s.bucket).Object(name).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return gcsAccessError("delete", s.bucket, name, err)
}

func gcsAccessError(action, bucket, object string, err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	wrapped := fmt.Errorf("%s gs://%s/%s: %w", action, bucket, object, err)
	if !strings.Contains(message, "invalid_rapt") &&
		!strings.Contains(message, "invalid_grant") &&
		!strings.Contains(message, "USER_PROJECT_DENIED") {
		return wrapped
	}
	return fmt.Errorf(`%w

Google credentials need attention. If using default bgit auth, check the selected gcloud configuration:
  gcloud config configurations list
  gcloud auth print-access-token

If using --auth adc, refresh Application Default Credentials:
  gcloud auth application-default print-access-token
  gcloud auth application-default set-quota-project PROJECT_ID`, wrapped)
}

func (s *localGitStore) read(ctx context.Context, path string) ([]byte, error) {
	_ = ctx
	data, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(path)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fs.ErrNotExist
	}
	return data, err
}

func (s *localGitStore) list(ctx context.Context, prefix string) ([]string, error) {
	_ = ctx
	root := filepath.Join(s.root, filepath.FromSlash(prefix))
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	return paths, err
}

func (s *localGitStore) write(ctx context.Context, path string, data []byte) error {
	_ = ctx
	target := filepath.Join(s.root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o644)
}

func (s *localGitStore) delete(ctx context.Context, path string) error {
	_ = ctx
	err := os.Remove(filepath.Join(s.root, filepath.FromSlash(path)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
