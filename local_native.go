package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type localRepository struct {
	worktree string
	gitDir   string
	cwd      string
	store    *localGitStore
}

type indexEntry struct {
	ctimeSec uint32
	ctimeNS  uint32
	mtimeSec uint32
	mtimeNS  uint32
	dev      uint32
	ino      uint32
	mode     uint32
	uid      uint32
	gid      uint32
	size     uint32
	hash     string
	flags    uint16
	path     string
}

type gitIndex struct {
	entries []indexEntry
}

type localTreeNode struct {
	files map[string]indexEntry
	dirs  map[string]*localTreeNode
}

func nativeLocalCommand(cmd string, args []string, stdout io.Writer) error {
	repo, err := openLocalRepository(".")
	if err != nil {
		return err
	}
	switch cmd {
	case "status":
		return repo.status(args, stdout)
	case "add":
		return repo.add(args)
	case "rm", "delete":
		return repo.remove(args, stdout)
	case "mv", "move":
		return repo.move(args)
	case "commit":
		return repo.commit(args, stdout)
	case "checkout", "switch":
		return repo.checkout(cmd, args, stdout)
	case "branch":
		return repo.branch(args, stdout)
	case "tag":
		return repo.tag(args, stdout)
	case "log":
		return repo.log(args, stdout)
	case "rev-parse":
		return repo.revParse(args, stdout)
	case "diff":
		return repo.diff(args, stdout)
	case "reset":
		return repo.reset(args)
	case "restore":
		return repo.restore(args)
	case "stash":
		return repo.stash(args, stdout)
	case "revert":
		return repo.revert(args, stdout)
	case "grep":
		return repo.grep(args, stdout)
	case "blame":
		return repo.blame(args, stdout)
	case "cherry-pick":
		return repo.cherryPick(args, stdout)
	case "clean":
		return repo.clean(args, stdout)
	case "describe":
		return repo.describe(args, stdout)
	case "ls-files":
		return repo.lsFiles(args, stdout)
	case "ls-tree":
		return repo.lsTree(args, stdout)
	case "archive":
		return repo.archive(args, stdout)
	case "show":
		return repo.show(args, stdout)
	case "merge":
		return repo.merge(args, stdout)
	case "config":
		return repo.config(args, stdout)
	default:
		return unsupportedCommand(cmd)
	}
}

func openLocalRepository(start string) (*localRepository, error) {
	cwd, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(cwd); err == nil && !info.IsDir() {
		cwd = filepath.Dir(cwd)
	}
	worktree, gitDir, err := findLocalRepository(start)
	if err != nil {
		return nil, err
	}
	return &localRepository{
		worktree: worktree,
		gitDir:   gitDir,
		cwd:      filepath.Clean(cwd),
		store:    &localGitStore{root: gitDir},
	}, nil
}

func findLocalRepository(start string) (string, string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(abs)
	if err == nil && !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	for {
		dotGit := filepath.Join(abs, ".git")
		if info, err := os.Stat(dotGit); err == nil {
			if info.IsDir() {
				return abs, dotGit, nil
			}
			data, err := os.ReadFile(dotGit)
			if err != nil {
				return "", "", err
			}
			line := strings.TrimSpace(string(data))
			if strings.HasPrefix(line, "gitdir:") {
				gitDir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
				if !filepath.IsAbs(gitDir) {
					gitDir = filepath.Join(abs, gitDir)
				}
				return abs, filepath.Clean(gitDir), nil
			}
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", "", errors.New("this command must be run inside a git worktree created by bgit clone/init")
		}
		abs = parent
	}
}

func (r *localRepository) status(args []string, stdout io.Writer) error {
	short := false
	for _, arg := range args {
		switch arg {
		case "--short", "-s":
			short = true
		default:
			return fmt.Errorf("unsupported status option %s", arg)
		}
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	head, _ := r.resolveRevision("HEAD")
	headFiles := map[string]string{}
	if head != "" {
		commit, err := r.commitObject(head)
		if err != nil {
			return err
		}
		if err := r.collectTreeFiles(commit.tree, "", headFiles); err != nil {
			return err
		}
	}
	indexByPath := idx.byPath()
	renames := r.statusRenames(indexByPath, headFiles)
	renamedFrom := map[string]struct{}{}
	renamedTo := map[string]statusRename{}
	for _, rename := range renames {
		renamedFrom[rename.from] = struct{}{}
		renamedTo[rename.to] = rename
	}
	paths := map[string]struct{}{}
	for path := range headFiles {
		paths[path] = struct{}{}
	}
	for path := range indexByPath {
		paths[path] = struct{}{}
	}
	var sorted []string
	for path := range paths {
		sorted = append(sorted, path)
	}
	sort.Strings(sorted)
	var staged []statusItem
	var unstaged []statusItem
	for _, path := range sorted {
		if _, ok := renamedFrom[path]; ok {
			continue
		}
		indexStatus, worktreeStatus, err := r.pathStatus(path, indexByPath, headFiles)
		if err != nil {
			return err
		}
		if indexStatus != " " || worktreeStatus != " " {
			if short {
				if rename, ok := renamedTo[path]; ok && indexStatus == "A" {
					fmt.Fprintf(stdout, "R%s %s -> %s\n", worktreeStatus, r.displayPath(rename.from), r.displayPath(rename.to))
				} else {
					fmt.Fprintf(stdout, "%s%s %s\n", indexStatus, worktreeStatus, r.displayPath(path))
				}
			}
			if indexStatus != " " {
				if rename, ok := renamedTo[path]; ok && indexStatus == "A" {
					staged = append(staged, statusItem{code: "R", path: rename.to, from: rename.from})
				} else {
					staged = append(staged, statusItem{code: indexStatus, path: path})
				}
			}
			if worktreeStatus != " " {
				unstaged = append(unstaged, statusItem{code: worktreeStatus, path: path})
			}
		}
	}
	untracked, err := r.untrackedFiles(indexByPath)
	if err != nil {
		return err
	}
	for _, path := range untracked {
		if short {
			fmt.Fprintf(stdout, "?? %s\n", r.displayPath(path))
		}
	}
	if short {
		return nil
	}
	return r.printLongStatus(stdout, staged, unstaged, untracked, head != "")
}

type statusItem struct {
	code string
	path string
	from string
}

type statusRename struct {
	from string
	to   string
}

func (r *localRepository) pathStatus(path string, indexByPath map[string]indexEntry, headFiles map[string]string) (string, string, error) {
	entry, tracked := indexByPath[path]
	indexStatus := " "
	worktreeStatus := " "
	headHash, inHead := headFiles[path]
	switch {
	case tracked && !inHead:
		indexStatus = "A"
	case !tracked && inHead:
		indexStatus = "D"
	case tracked && inHead && entry.hash != headHash:
		indexStatus = "M"
	}
	if tracked {
		hash, err := r.worktreeBlobHash(path)
		if errors.Is(err, fs.ErrNotExist) {
			worktreeStatus = "D"
		} else if err != nil {
			return "", "", err
		} else if hash != entry.hash {
			worktreeStatus = "M"
		}
	}
	return indexStatus, worktreeStatus, nil
}

func (r *localRepository) statusRenames(indexByPath map[string]indexEntry, headFiles map[string]string) []statusRename {
	before := map[string]treeFile{}
	for path, hash := range headFiles {
		before[path] = treeFile{hash: hash, mode: 0100644}
	}
	after := map[string]treeFile{}
	for path, entry := range indexByPath {
		after[path] = treeFile{hash: entry.hash, mode: entry.mode}
	}
	var renames []statusRename
	for _, rename := range detectRenames(before, after) {
		renames = append(renames, statusRename{from: rename.from, to: rename.to})
	}
	return renames
}

func (r *localRepository) printLongStatus(stdout io.Writer, staged, unstaged []statusItem, untracked []string, hasHead bool) error {
	fmt.Fprintf(stdout, "On branch %s\n", firstNonEmpty(r.currentBranch(), "HEAD"))
	sections := 0
	if !hasHead {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "No commits yet")
		sections++
	}
	if len(staged) > 0 {
		if sections > 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintln(stdout, "Changes to be committed:")
		if hasHead {
			fmt.Fprintln(stdout, "  (use \"bgit restore --staged <file>...\" to unstage)")
		} else {
			fmt.Fprintln(stdout, "  (use \"bgit rm --cached <file>...\" to unstage)")
		}
		for _, item := range staged {
			if item.code == "R" {
				fmt.Fprintf(stdout, "\trenamed:    %s -> %s\n", r.displayPath(item.from), r.displayPath(item.path))
			} else {
				fmt.Fprintf(stdout, "\t%s:   %s\n", longStatusLabel(item.code), r.displayPath(item.path))
			}
		}
		fmt.Fprintln(stdout)
		sections++
	}
	if len(unstaged) > 0 {
		if sections > 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintln(stdout, "Changes not staged for commit:")
		fmt.Fprintln(stdout, "  (use \"bgit add/rm <file>...\" to update what will be committed)")
		fmt.Fprintln(stdout, "  (use \"bgit restore <file>...\" to discard changes in working directory)")
		for _, item := range unstaged {
			fmt.Fprintf(stdout, "\t%s:   %s\n", longStatusLabel(item.code), r.displayPath(item.path))
		}
		fmt.Fprintln(stdout)
		sections++
	}
	if len(untracked) > 0 {
		if sections > 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintln(stdout, "Untracked files:")
		fmt.Fprintln(stdout, "  (use \"bgit add <file>...\" to include in what will be committed)")
		for _, path := range untracked {
			fmt.Fprintf(stdout, "\t%s\n", r.displayPath(path))
		}
		fmt.Fprintln(stdout)
	}
	if len(staged) == 0 && len(unstaged) == 0 && len(untracked) == 0 {
		fmt.Fprintln(stdout, "nothing to commit, working tree clean")
	} else if len(staged) == 0 && len(unstaged) == 0 && len(untracked) > 0 {
		fmt.Fprintln(stdout, "nothing added to commit but untracked files present (use \"bgit add\" to track)")
	} else if len(staged) == 0 {
		fmt.Fprintln(stdout, "no changes added to commit (use \"bgit add\" and/or \"bgit commit -a\")")
	}
	return nil
}

func longStatusLabel(code string) string {
	switch code {
	case "A":
		return "new file"
	case "D":
		return "deleted"
	case "M":
		return "modified"
	default:
		return "changed"
	}
}

func (r *localRepository) displayPath(repoPath string) string {
	full := filepath.Join(r.worktree, filepath.FromSlash(repoPath))
	rel, err := filepath.Rel(r.cwd, full)
	if err != nil || rel == "" {
		return repoPath
	}
	return filepath.ToSlash(rel)
}

func (r *localRepository) add(args []string) error {
	if len(args) == 0 {
		return nil
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			switch arg {
			case "-A", "--all", ".":
			default:
				return fmt.Errorf("unsupported add option %s", arg)
			}
		}
	}
	for _, arg := range args {
		if arg == "-A" || arg == "--all" || arg == "." {
			for _, entry := range append([]indexEntry{}, idx.entries...) {
				if _, err := os.Stat(filepath.Join(r.worktree, filepath.FromSlash(entry.path))); errors.Is(err, fs.ErrNotExist) {
					idx.removePath(entry.path)
				}
			}
			files, err := r.allWorktreeFiles()
			if err != nil {
				return err
			}
			for _, path := range files {
				if err := r.addPathToIndex(&idx, path); err != nil {
					return err
				}
			}
			continue
		}
		matches, err := r.expandPathArg(arg)
		if err != nil {
			return err
		}
		for _, path := range matches {
			if err := r.addPathToIndex(&idx, path); err != nil {
				return err
			}
		}
	}
	idx.sort()
	return r.writeIndex(idx)
}

func (r *localRepository) remove(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("rm requires at least one path")
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	recursive := false
	var paths []string
	for _, arg := range args {
		switch arg {
		case "-r", "--recursive":
			recursive = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unsupported rm option %s", arg)
			}
			paths = append(paths, arg)
		}
	}
	for _, arg := range paths {
		rel := cleanRepoPath(arg)
		full := filepath.Join(r.worktree, filepath.FromSlash(rel))
		if info, err := os.Stat(full); err == nil && info.IsDir() && !recursive {
			return fmt.Errorf("not removing %s recursively without -r", rel)
		}
		idx.removePath(rel)
		if err := os.RemoveAll(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		fmt.Fprintf(stdout, "rm '%s'\n", rel)
	}
	idx.sort()
	return r.writeIndex(idx)
}

func (r *localRepository) move(args []string) error {
	if len(args) != 2 {
		return errors.New("mv requires source and destination")
	}
	src := cleanRepoPath(args[0])
	dst := cleanRepoPath(args[1])
	if err := os.MkdirAll(filepath.Dir(filepath.Join(r.worktree, filepath.FromSlash(dst))), 0o755); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(r.worktree, filepath.FromSlash(src)), filepath.Join(r.worktree, filepath.FromSlash(dst))); err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	if entry, ok := idx.find(src); ok {
		idx.removePath(src)
		entry.path = dst
		idx.upsert(entry)
	} else if err := r.addPathToIndex(&idx, dst); err != nil {
		return err
	}
	idx.sort()
	return r.writeIndex(idx)
}

func (r *localRepository) commit(args []string, stdout io.Writer) error {
	opts, err := parseCommitArgs(args)
	if err != nil {
		return err
	}
	if opts.message == "" {
		return errors.New("commit requires -m")
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	if opts.all {
		for _, entry := range append([]indexEntry{}, idx.entries...) {
			if _, err := os.Stat(filepath.Join(r.worktree, filepath.FromSlash(entry.path))); errors.Is(err, fs.ErrNotExist) {
				idx.removePath(entry.path)
				continue
			}
			if err := r.addPathToIndex(&idx, entry.path); err != nil {
				return err
			}
		}
		idx.sort()
		if err := r.writeIndex(idx); err != nil {
			return err
		}
	}
	treeHash, err := r.writeTree(idx)
	if err != nil {
		return err
	}
	parent, _ := r.resolveRevision("HEAD")
	before := map[string]treeFile{}
	if parent != "" {
		parentCommit, err := r.commitObject(parent)
		if err != nil {
			return err
		}
		if err := r.collectTreeFileEntries(parentCommit.tree, "", before); err != nil {
			return err
		}
		if parentCommit.tree == treeHash {
			return errors.New("nothing to commit")
		}
	}
	after := idx.treeFiles()
	authorName := firstNonEmpty(os.Getenv("GIT_AUTHOR_NAME"), r.configValue("user.name"), "bgit")
	authorEmail := firstNonEmpty(os.Getenv("GIT_AUTHOR_EMAIL"), r.configValue("user.email"), "bgit@example.com")
	committerName := firstNonEmpty(os.Getenv("GIT_COMMITTER_NAME"), authorName)
	committerEmail := firstNonEmpty(os.Getenv("GIT_COMMITTER_EMAIL"), authorEmail)
	now := time.Now()
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", treeHash)
	if parent != "" {
		fmt.Fprintf(&buf, "parent %s\n", parent)
	}
	fmt.Fprintf(&buf, "author %s <%s> %d %s\n", authorName, authorEmail, now.Unix(), timezoneOffset(now))
	fmt.Fprintf(&buf, "committer %s <%s> %d %s\n\n", committerName, committerEmail, now.Unix(), timezoneOffset(now))
	buf.WriteString(opts.message)
	buf.WriteByte('\n')
	hash, err := r.writeObject(gitObjectCommit, buf.Bytes())
	if err != nil {
		return err
	}
	if err := r.updateHEAD(hash); err != nil {
		return err
	}
	branch := r.currentBranch()
	if branch == "" {
		branch = "HEAD"
	}
	root := ""
	if parent == "" {
		root = " (root-commit)"
	}
	fmt.Fprintf(stdout, "[%s%s %s] %s\n", branch, root, hash[:7], strings.Split(opts.message, "\n")[0])
	r.printChangeSummary(stdout, before, after, false)
	return nil
}

func (r *localRepository) checkout(cmd string, args []string, stdout io.Writer) error {
	opts, err := parseCheckoutArgs(args)
	if err != nil {
		return err
	}
	if len(opts.paths) > 0 {
		source := firstNonEmpty(opts.target, "HEAD")
		return r.checkoutPaths(source, opts.paths)
	}
	if opts.target == "" {
		return errors.New(cmd + " requires a branch or commit")
	}
	if !opts.create {
		if _, err := r.resolveRevision(branchRef(opts.target)); err != nil {
			if _, revErr := r.resolveRevision(opts.target); revErr != nil && r.pathExistsInHead(opts.target) {
				return r.checkoutPaths("HEAD", []string{cleanRepoPath(opts.target)})
			}
		}
	}
	if opts.create {
		start := opts.start
		if start == "" {
			start = "HEAD"
		}
		hash, err := r.resolveRevision(start)
		if err != nil {
			return err
		}
		if err := r.writeRef(branchRef(opts.target), hash); err != nil {
			return err
		}
		if err := r.setHEADSymbolic(branchRef(opts.target)); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Switched to a new branch '%s'\n", opts.target)
		return r.checkoutCommit(hash)
	}
	if hash, err := r.resolveRevision(branchRef(opts.target)); err == nil {
		if err := r.setHEADSymbolic(branchRef(opts.target)); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Switched to branch '%s'\n", opts.target)
		return r.checkoutCommit(hash)
	}
	hash, err := r.resolveRevision(opts.target)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.gitDir, "HEAD"), []byte(hash+"\n"), 0o644); err != nil {
		return err
	}
	if commit, err := r.commitObject(hash); err == nil {
		fmt.Fprintf(stdout, "HEAD is now at %s %s\n", hash[:7], commit.subject)
	}
	return r.checkoutCommit(hash)
}

func (r *localRepository) pathExistsInHead(path string) bool {
	head, err := r.resolveRevision("HEAD")
	if err != nil {
		return false
	}
	commit, err := r.commitObject(head)
	if err != nil {
		return false
	}
	files := map[string]string{}
	if err := r.collectTreeFiles(commit.tree, "", files); err != nil {
		return false
	}
	path = cleanRepoPath(path)
	if _, ok := files[path]; ok {
		return true
	}
	prefix := strings.TrimSuffix(path, "/") + "/"
	for file := range files {
		if strings.HasPrefix(file, prefix) {
			return true
		}
	}
	return false
}

func (r *localRepository) checkoutPaths(source string, paths []string) error {
	hash, err := r.resolveRevision(source)
	if err != nil {
		return err
	}
	commit, err := r.commitObject(hash)
	if err != nil {
		return err
	}
	files := map[string]treeFile{}
	if err := r.collectTreeFileEntries(commit.tree, "", files); err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	for _, path := range paths {
		prefix := strings.TrimSuffix(path, "/") + "/"
		matched := false
		for file, meta := range files {
			if file != path && !strings.HasPrefix(file, prefix) {
				continue
			}
			matched = true
			idx.upsert(indexEntry{path: file, hash: meta.hash, mode: meta.mode})
			if err := r.writeBlobToWorktree(meta.hash, file); err != nil {
				return err
			}
		}
		if !matched {
			idx.removePath(path)
			if err := os.RemoveAll(filepath.Join(r.worktree, filepath.FromSlash(path))); err != nil {
				return err
			}
		}
	}
	idx.sort()
	return r.writeIndex(idx)
}

func (r *localRepository) branch(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		branches, err := r.listRefs("refs/heads")
		if err != nil {
			return err
		}
		current := r.currentBranch()
		for _, name := range branches {
			short := strings.TrimPrefix(name, "refs/heads/")
			prefix := "  "
			if short == current {
				prefix = "* "
			}
			fmt.Fprintln(stdout, prefix+short)
		}
		return nil
	}
	if args[0] == "-d" || args[0] == "-D" {
		if len(args) != 2 {
			return errors.New("branch -d requires a branch")
		}
		ref := branchRef(args[1])
		hash, _ := r.readRef(ref)
		if err := r.deleteRef(ref); err != nil {
			return err
		}
		short := hash
		if len(short) > 7 {
			short = short[:7]
		}
		fmt.Fprintf(stdout, "Deleted branch %s (was %s).\n", args[1], short)
		return nil
	}
	if len(args) > 2 {
		return errors.New("branch accepts branch name and optional start point")
	}
	start := "HEAD"
	if len(args) == 2 {
		start = args[1]
	}
	hash, err := r.resolveRevision(start)
	if err != nil {
		return err
	}
	return r.writeRef(branchRef(args[0]), hash)
}

func (r *localRepository) tag(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		tags, err := r.listRefs("refs/tags")
		if err != nil {
			return err
		}
		for _, name := range tags {
			fmt.Fprintln(stdout, strings.TrimPrefix(name, "refs/tags/"))
		}
		return nil
	}
	if args[0] == "-d" {
		if len(args) != 2 {
			return errors.New("tag -d requires a tag name")
		}
		if err := r.deleteRef("refs/tags/" + args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Deleted tag '%s'\n", args[1])
		return nil
	}
	annotated := false
	message := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-a":
			annotated = true
		case "-m":
			i++
			if i >= len(args) {
				return errors.New("tag -m requires a message")
			}
			message = args[i]
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) < 1 || len(rest) > 2 {
		return errors.New("tag accepts tag name and optional commit")
	}
	target := "HEAD"
	if len(rest) == 2 {
		target = rest[1]
	}
	hash, err := r.resolveRevision(target)
	if err != nil {
		return err
	}
	if annotated || message != "" {
		if message == "" {
			return errors.New("annotated tags require -m")
		}
		now := time.Now()
		name := firstNonEmpty(r.configValue("user.name"), "bgit")
		email := firstNonEmpty(r.configValue("user.email"), "bgit@example.com")
		var buf bytes.Buffer
		fmt.Fprintf(&buf, "object %s\n", hash)
		fmt.Fprintf(&buf, "type commit\n")
		fmt.Fprintf(&buf, "tag %s\n", rest[0])
		fmt.Fprintf(&buf, "tagger %s <%s> %d %s\n\n", name, email, now.Unix(), timezoneOffset(now))
		buf.WriteString(message)
		buf.WriteByte('\n')
		hash, err = r.writeObject(gitObjectTag, buf.Bytes())
		if err != nil {
			return err
		}
	}
	return r.writeRef("refs/tags/"+rest[0], hash)
}

func (r *localRepository) log(args []string, stdout io.Writer) error {
	oneline := false
	limit := 50
	var rev string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--oneline":
			oneline = true
		case "-n":
			i++
			if i >= len(args) {
				return errors.New("-n requires a value")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return err
			}
			limit = n
		default:
			if strings.HasPrefix(arg, "-n") && len(arg) > 2 {
				n, err := strconv.Atoi(strings.TrimPrefix(arg, "-n"))
				if err != nil {
					return err
				}
				limit = n
			} else if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unsupported log option %s", arg)
			} else {
				rev = arg
			}
		}
	}
	if rev == "" {
		rev = "HEAD"
	}
	head, err := r.resolveRevision(rev)
	if err != nil {
		return err
	}
	repo := newNativeGitRepoForStore(config{branch: defaultBranch}, r.store)
	commits, err := repo.walkCommits(nilContext{}, head, limit, 0, "")
	if err != nil {
		return err
	}
	for _, commit := range commits {
		if oneline {
			fmt.Fprintf(stdout, "%s %s\n", commit.hash[:7], commit.subject)
		} else {
			fmt.Fprintf(stdout, "commit %s\nAuthor: %s <%s>\n\n    %s\n\n", commit.hash, commit.author, commit.email, commit.subject)
		}
	}
	return nil
}

func (r *localRepository) revParse(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("rev-parse requires a revision")
	}
	for _, arg := range args {
		if arg == "--verify" {
			continue
		}
		if arg == "--show-toplevel" {
			fmt.Fprintln(stdout, r.worktree)
			continue
		}
		if arg == "--git-dir" {
			fmt.Fprintln(stdout, r.gitDir)
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unsupported rev-parse option %s", arg)
		}
		hash, err := r.resolveRevision(arg)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, hash)
	}
	return nil
}

func (r *localRepository) readIndex() (gitIndex, error) {
	data, err := os.ReadFile(filepath.Join(r.gitDir, "index"))
	if errors.Is(err, fs.ErrNotExist) {
		return gitIndex{}, nil
	}
	if err != nil {
		return gitIndex{}, err
	}
	if len(data) < 12 || string(data[:4]) != "DIRC" {
		return gitIndex{}, errors.New("unsupported git index")
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version != 2 {
		return gitIndex{}, fmt.Errorf("unsupported git index version %d", version)
	}
	count := int(binary.BigEndian.Uint32(data[8:12]))
	pos := 12
	var idx gitIndex
	for i := 0; i < count; i++ {
		if len(data) < pos+62 {
			return gitIndex{}, errors.New("truncated git index")
		}
		start := pos
		entry := indexEntry{
			ctimeSec: binary.BigEndian.Uint32(data[pos:]),
			ctimeNS:  binary.BigEndian.Uint32(data[pos+4:]),
			mtimeSec: binary.BigEndian.Uint32(data[pos+8:]),
			mtimeNS:  binary.BigEndian.Uint32(data[pos+12:]),
			dev:      binary.BigEndian.Uint32(data[pos+16:]),
			ino:      binary.BigEndian.Uint32(data[pos+20:]),
			mode:     binary.BigEndian.Uint32(data[pos+24:]),
			uid:      binary.BigEndian.Uint32(data[pos+28:]),
			gid:      binary.BigEndian.Uint32(data[pos+32:]),
			size:     binary.BigEndian.Uint32(data[pos+36:]),
			hash:     hex.EncodeToString(data[pos+40 : pos+60]),
			flags:    binary.BigEndian.Uint16(data[pos+60 : pos+62]),
		}
		pos += 62
		end := bytes.IndexByte(data[pos:], 0)
		if end < 0 {
			return gitIndex{}, errors.New("unterminated git index path")
		}
		entry.path = string(data[pos : pos+end])
		pos += end + 1
		for (pos-start)%8 != 0 {
			pos++
		}
		idx.entries = append(idx.entries, entry)
	}
	return idx, nil
}

func (r *localRepository) writeIndex(idx gitIndex) error {
	idx.sort()
	var body bytes.Buffer
	body.WriteString("DIRC")
	writeUint32(&body, 2)
	writeUint32(&body, uint32(len(idx.entries)))
	for _, entry := range idx.entries {
		startLen := body.Len()
		writeUint32(&body, entry.ctimeSec)
		writeUint32(&body, entry.ctimeNS)
		writeUint32(&body, entry.mtimeSec)
		writeUint32(&body, entry.mtimeNS)
		writeUint32(&body, entry.dev)
		writeUint32(&body, entry.ino)
		writeUint32(&body, entry.mode)
		writeUint32(&body, entry.uid)
		writeUint32(&body, entry.gid)
		writeUint32(&body, entry.size)
		hashBytes, err := hex.DecodeString(entry.hash)
		if err != nil {
			return err
		}
		body.Write(hashBytes)
		flags := uint16(len(entry.path))
		if flags > 0x0fff {
			flags = 0x0fff
		}
		writeUint16(&body, flags)
		body.WriteString(entry.path)
		body.WriteByte(0)
		for (body.Len()-startLen)%8 != 0 {
			body.WriteByte(0)
		}
	}
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return os.WriteFile(filepath.Join(r.gitDir, "index"), body.Bytes(), 0o644)
}

func (r *localRepository) addPathToIndex(idx *gitIndex, rel string) error {
	rel = cleanRepoPath(rel)
	full := filepath.Join(r.worktree, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	if info.IsDir() {
		files, err := r.filesUnder(rel)
		if err != nil {
			return err
		}
		for _, file := range files {
			if err := r.addPathToIndex(idx, file); err != nil {
				return err
			}
		}
		return nil
	}
	hash, err := r.writeBlobFromWorktree(rel)
	if err != nil {
		return err
	}
	stat := info.Sys()
	_ = stat
	mtime := info.ModTime()
	mode := uint32(0100644)
	if info.Mode()&0o111 != 0 {
		mode = 0100755
	}
	entry := indexEntry{
		ctimeSec: uint32(mtime.Unix()),
		mtimeSec: uint32(mtime.Unix()),
		mode:     mode,
		size:     uint32(info.Size()),
		hash:     hash,
		path:     rel,
	}
	idx.upsert(entry)
	return nil
}

func (r *localRepository) writeBlobFromWorktree(path string) (string, error) {
	data, err := os.ReadFile(filepath.Join(r.worktree, filepath.FromSlash(path)))
	if err != nil {
		return "", err
	}
	return r.writeObject(gitObjectBlob, data)
}

func (r *localRepository) writeObject(typ string, data []byte) (string, error) {
	var raw bytes.Buffer
	fmt.Fprintf(&raw, "%s %d", typ, len(data))
	raw.WriteByte(0)
	raw.Write(data)
	hash := sha1.Sum(raw.Bytes())
	hashHex := hex.EncodeToString(hash[:])
	target := filepath.Join(r.gitDir, "objects", hashHex[:2], hashHex[2:])
	if _, err := os.Stat(target); err == nil {
		return hashHex, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(raw.Bytes()); err != nil {
		_ = writer.Close()
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, compressed.Bytes(), 0o444); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			_ = os.Remove(tmp)
			return hashHex, nil
		}
		return "", err
	}
	return hashHex, nil
}

func (r *localRepository) writeTree(idx gitIndex) (string, error) {
	root := &localTreeNode{files: map[string]indexEntry{}, dirs: map[string]*localTreeNode{}}
	for _, entry := range idx.entries {
		parts := strings.Split(entry.path, "/")
		node := root
		for _, part := range parts[:len(parts)-1] {
			if node.dirs[part] == nil {
				node.dirs[part] = &localTreeNode{files: map[string]indexEntry{}, dirs: map[string]*localTreeNode{}}
			}
			node = node.dirs[part]
		}
		node.files[parts[len(parts)-1]] = entry
	}
	return r.writeTreeNode(root)
}

func (r *localRepository) writeTreeNode(node *localTreeNode) (string, error) {
	type item struct {
		name string
		mode string
		hash string
	}
	var items []item
	for name, child := range node.dirs {
		hash, err := r.writeTreeNode(child)
		if err != nil {
			return "", err
		}
		items = append(items, item{name: name, mode: "40000", hash: hash})
	}
	for name, entry := range node.files {
		mode := "100644"
		if entry.mode == 0100755 {
			mode = "100755"
		}
		items = append(items, item{name: name, mode: mode, hash: entry.hash})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	var data bytes.Buffer
	for _, item := range items {
		data.WriteString(item.mode)
		data.WriteByte(' ')
		data.WriteString(item.name)
		data.WriteByte(0)
		hashBytes, err := hex.DecodeString(item.hash)
		if err != nil {
			return "", err
		}
		data.Write(hashBytes)
	}
	return r.writeObject(gitObjectTree, data.Bytes())
}

func (r *localRepository) checkoutCommit(hash string) error {
	commit, err := r.commitObject(hash)
	if err != nil {
		return err
	}
	files := map[string]string{}
	if err := r.collectTreeFiles(commit.tree, "", files); err != nil {
		return err
	}
	current, err := r.readIndex()
	if err != nil {
		return err
	}
	for _, entry := range current.entries {
		if _, ok := files[entry.path]; !ok {
			err := os.Remove(filepath.Join(r.worktree, filepath.FromSlash(entry.path)))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	idx := gitIndex{}
	for path, blobHash := range files {
		obj, err := r.storeObject(blobHash)
		if err != nil {
			return err
		}
		target := filepath.Join(r.worktree, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, obj.data, 0o644); err != nil {
			return err
		}
		if err := r.addPathToIndex(&idx, path); err != nil {
			return err
		}
	}
	return r.writeIndex(idx)
}

func (r *localRepository) commitObject(hash string) (commitObject, error) {
	repo := newNativeGitRepoForStore(config{branch: defaultBranch}, r.store)
	return repo.commit(nilContext{}, hash)
}

func (r *localRepository) isAncestor(ancestor, descendant string) (bool, error) {
	stack := []string{descendant}
	seen := map[string]struct{}{}
	for len(stack) > 0 {
		hash := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if hash == ancestor {
			return true, nil
		}
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		commit, err := r.commitObject(hash)
		if err != nil {
			return false, err
		}
		stack = append(stack, commit.parents...)
	}
	return false, nil
}

func (r *localRepository) storeObject(hash string) (gitObject, error) {
	repo := newNativeGitRepoForStore(config{branch: defaultBranch}, r.store)
	return repo.object(nilContext{}, hash)
}

func (r *localRepository) collectTreeFiles(treeHash, base string, out map[string]string) error {
	repo := newNativeGitRepoForStore(config{branch: defaultBranch}, r.store)
	entries, err := repo.treeEntries(nilContext{}, treeHash)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := entry.name
		if base != "" {
			path = base + "/" + entry.name
		}
		if entry.typ == gitObjectTree {
			if err := r.collectTreeFiles(entry.hash, path, out); err != nil {
				return err
			}
			continue
		}
		out[path] = entry.hash
	}
	return nil
}

func (r *localRepository) resolveRevision(rev string) (string, error) {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		rev = "HEAD"
	}
	if isHexHash(rev) {
		return rev, nil
	}
	if base, distance, ok := parseAncestorRevision(rev); ok {
		hash, err := r.resolveRevision(base)
		if err != nil {
			return "", err
		}
		for i := 0; i < distance; i++ {
			commit, err := r.commitObject(hash)
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
	if rev == "HEAD" {
		data, err := os.ReadFile(filepath.Join(r.gitDir, "HEAD"))
		if err != nil {
			return "", err
		}
		line := strings.TrimSpace(string(data))
		if strings.HasPrefix(line, "ref:") {
			return r.readRef(strings.TrimSpace(strings.TrimPrefix(line, "ref:")))
		}
		if isHexHash(line) {
			return line, nil
		}
		return "", fs.ErrNotExist
	}
	candidates := []string{rev}
	if !strings.HasPrefix(rev, "refs/") {
		candidates = append(candidates, branchRef(rev), "refs/tags/"+rev, "refs/remotes/bucketgit/"+rev)
	}
	for _, ref := range candidates {
		hash, err := r.readRef(ref)
		if err == nil {
			return hash, nil
		}
	}
	return "", fs.ErrNotExist
}

func parseAncestorRevision(rev string) (string, int, bool) {
	base, suffix, ok := strings.Cut(strings.TrimSpace(rev), "~")
	if !ok {
		return "", 0, false
	}
	if base == "" {
		base = "HEAD"
	}
	distance := 1
	if suffix != "" {
		parsed, err := strconv.Atoi(suffix)
		if err != nil || parsed < 0 {
			return "", 0, false
		}
		distance = parsed
	}
	return base, distance, true
}

func (r *localRepository) readRef(ref string) (string, error) {
	data, err := os.ReadFile(filepath.Join(r.gitDir, filepath.FromSlash(ref)))
	if err == nil {
		hash := strings.TrimSpace(string(data))
		if isHexHash(hash) {
			return hash, nil
		}
	}
	data, err = os.ReadFile(filepath.Join(r.gitDir, "packed-refs"))
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.Fields(line)
			if len(parts) == 2 && parts[1] == ref && isHexHash(parts[0]) {
				return parts[0], nil
			}
		}
	}
	return "", fs.ErrNotExist
}

func (r *localRepository) writeRef(ref, hash string) error {
	return writeRefFile(filepath.Join(r.gitDir, filepath.FromSlash(ref)), hash)
}

func (r *localRepository) deleteRef(ref string) error {
	err := os.Remove(filepath.Join(r.gitDir, filepath.FromSlash(ref)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (r *localRepository) updateHEAD(hash string) error {
	data, err := os.ReadFile(filepath.Join(r.gitDir, "HEAD"))
	if err != nil {
		return err
	}
	line := strings.TrimSpace(string(data))
	if strings.HasPrefix(line, "ref:") {
		return r.writeRef(strings.TrimSpace(strings.TrimPrefix(line, "ref:")), hash)
	}
	return os.WriteFile(filepath.Join(r.gitDir, "HEAD"), []byte(hash+"\n"), 0o644)
}

func (r *localRepository) setHEADSymbolic(ref string) error {
	return os.WriteFile(filepath.Join(r.gitDir, "HEAD"), []byte("ref: "+ref+"\n"), 0o644)
}

func (r *localRepository) currentBranch() string {
	data, err := os.ReadFile(filepath.Join(r.gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	if strings.HasPrefix(line, "ref: refs/heads/") {
		return strings.TrimPrefix(line, "ref: refs/heads/")
	}
	return ""
}

func (r *localRepository) listRefs(prefix string) ([]string, error) {
	var refs []string
	root := filepath.Join(r.gitDir, filepath.FromSlash(prefix))
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(r.gitDir, path)
		if err == nil {
			refs = append(refs, filepath.ToSlash(rel))
		}
		return nil
	})
	data, err := os.ReadFile(filepath.Join(r.gitDir, "packed-refs"))
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.Fields(line)
			if len(parts) == 2 && strings.HasPrefix(parts[1], prefix+"/") {
				refs = append(refs, parts[1])
			}
		}
	}
	sort.Strings(refs)
	return refs, nil
}

func (r *localRepository) worktreeBlobHash(path string) (string, error) {
	data, err := os.ReadFile(filepath.Join(r.worktree, filepath.FromSlash(path)))
	if err != nil {
		return "", err
	}
	var raw bytes.Buffer
	fmt.Fprintf(&raw, "blob %d", len(data))
	raw.WriteByte(0)
	raw.Write(data)
	sum := sha1.Sum(raw.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

func (r *localRepository) untrackedFiles(index map[string]indexEntry) ([]string, error) {
	ignores, err := r.loadIgnoreMatcher()
	if err != nil {
		return nil, err
	}
	var files []string
	err = filepath.WalkDir(r.worktree, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == r.gitDir || strings.HasPrefix(path, r.gitDir+string(filepath.Separator)) {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			rel, err := filepath.Rel(r.worktree, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel != "." && ignores.Match(rel, true) && !hasTrackedPathUnder(index, rel) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(r.worktree, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := index[rel]; !ok {
			if ignores.Match(rel, false) {
				return nil
			}
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func (r *localRepository) allWorktreeFiles() ([]string, error) {
	return r.untrackedFiles(map[string]indexEntry{})
}

func (r *localRepository) filesUnder(rel string) ([]string, error) {
	rel = cleanRepoPath(rel)
	root := filepath.Join(r.worktree, filepath.FromSlash(rel))
	ignores, err := r.loadIgnoreMatcher()
	if err != nil {
		return nil, err
	}
	var files []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			item, err := filepath.Rel(r.worktree, path)
			if err != nil {
				return err
			}
			item = filepath.ToSlash(item)
			if item != "." && ignores.Match(item, true) {
				return filepath.SkipDir
			}
			return nil
		}
		item, err := filepath.Rel(r.worktree, path)
		if err != nil {
			return err
		}
		item = filepath.ToSlash(item)
		if !ignores.Match(item, false) {
			files = append(files, item)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func hasTrackedPathUnder(index map[string]indexEntry, dir string) bool {
	prefix := strings.Trim(dir, "/") + "/"
	for path := range index {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func (r *localRepository) expandPathArg(arg string) ([]string, error) {
	rel := cleanRepoPath(arg)
	full := filepath.Join(r.worktree, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return r.filesUnder(rel)
	}
	return []string{rel}, nil
}

func (r *localRepository) configValue(key string) string {
	cfg, err := readLocalConfigFile(filepath.Join(r.gitDir, "config"))
	if err != nil {
		return ""
	}
	value, ok := cfg.get(key)
	if !ok {
		return ""
	}
	return value
}

func (idx *gitIndex) byPath() map[string]indexEntry {
	out := map[string]indexEntry{}
	for _, entry := range idx.entries {
		out[entry.path] = entry
	}
	return out
}

func (idx *gitIndex) find(path string) (indexEntry, bool) {
	for _, entry := range idx.entries {
		if entry.path == path {
			return entry, true
		}
	}
	return indexEntry{}, false
}

func (idx *gitIndex) upsert(entry indexEntry) {
	for i := range idx.entries {
		if idx.entries[i].path == entry.path {
			idx.entries[i] = entry
			return
		}
	}
	idx.entries = append(idx.entries, entry)
}

func (idx *gitIndex) removePath(path string) {
	var entries []indexEntry
	prefix := strings.TrimSuffix(path, "/") + "/"
	for _, entry := range idx.entries {
		if entry.path != path && !strings.HasPrefix(entry.path, prefix) {
			entries = append(entries, entry)
		}
	}
	idx.entries = entries
}

func (idx *gitIndex) sort() {
	sort.Slice(idx.entries, func(i, j int) bool {
		return idx.entries[i].path < idx.entries[j].path
	})
}

func (idx gitIndex) treeFiles() map[string]treeFile {
	files := map[string]treeFile{}
	for _, entry := range idx.entries {
		files[entry.path] = treeFile{hash: entry.hash, mode: entry.mode}
	}
	return files
}

type commitOptions struct {
	message string
	all     bool
}

func parseCommitArgs(args []string) (commitOptions, error) {
	var opts commitOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-a", "--all":
			opts.all = true
		case "-m":
			i++
			if i >= len(args) {
				return opts, errors.New("-m requires a value")
			}
			opts.message = args[i]
		case "-am", "-ma":
			opts.all = true
			i++
			if i >= len(args) {
				return opts, errors.New("-am requires a value")
			}
			opts.message = args[i]
		default:
			if strings.HasPrefix(arg, "-am") && len(arg) > 3 {
				opts.all = true
				opts.message = strings.TrimPrefix(arg, "-am")
			} else if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unsupported commit option %s", arg)
			}
		}
	}
	return opts, nil
}

type checkoutOptions struct {
	create bool
	target string
	start  string
	paths  []string
}

func parseCheckoutArgs(args []string) (checkoutOptions, error) {
	var opts checkoutOptions
	pathMode := false
	for i := 0; i < len(args); i++ {
		if pathMode {
			opts.paths = append(opts.paths, cleanRepoPath(args[i]))
			continue
		}
		switch args[i] {
		case "-b", "-c":
			opts.create = true
			i++
			if i >= len(args) {
				return opts, errors.New("-b requires a branch")
			}
			opts.target = args[i]
		case "--":
			pathMode = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return opts, fmt.Errorf("unsupported checkout option %s", args[i])
			}
			if opts.target == "" {
				opts.target = args[i]
			} else if opts.start == "" {
				opts.start = args[i]
			} else {
				return opts, errors.New("checkout received too many arguments")
			}
		}
	}
	if pathMode && len(opts.paths) == 0 {
		return opts, errors.New("checkout requires at least one path")
	}
	if opts.create && len(opts.paths) > 0 {
		return opts, errors.New("checkout -b cannot be combined with path checkout")
	}
	return opts, nil
}

func cleanRepoPath(path string) string {
	path = filepath.ToSlash(filepath.Clean(path))
	path = strings.TrimPrefix(path, "../")
	path = strings.TrimPrefix(path, "/")
	if path == "." {
		return ""
	}
	return path
}

func writeUint32(w io.Writer, value uint32) {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], value)
	_, _ = w.Write(buf[:])
}

func writeUint16(w io.Writer, value uint16) {
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], value)
	_, _ = w.Write(buf[:])
}

func timezoneOffset(t time.Time) string {
	_, offset := t.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return fmt.Sprintf("%s%02d%02d", sign, offset/3600, (offset%3600)/60)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type nilContext struct{}

func (nilContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (nilContext) Done() <-chan struct{}       { return nil }
func (nilContext) Err() error                  { return nil }
func (nilContext) Value(any) any               { return nil }
