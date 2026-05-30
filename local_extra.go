package main

import (
	"archive/tar"
	"bytes"
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

type treeFile struct {
	hash string
	mode uint32
}

type changeSummary struct {
	files      int
	insertions int
	deletions  int
	created    []string
	deleted    []string
	renamed    []renameChange
}

type renameChange struct {
	from string
	to   string
	hash string
}

func (r *localRepository) diff(args []string, stdout io.Writer) error {
	cached := false
	var revisions []string
	for _, arg := range args {
		switch arg {
		case "--cached", "--staged":
			cached = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unsupported diff option %s", arg)
			}
			revisions = append(revisions, arg)
		}
	}
	if len(revisions) > 2 {
		return errors.New("diff accepts at most two revisions")
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	var left map[string]string
	if len(revisions) > 0 {
		left, err = r.revisionTreeFiles(revisions[0])
		if err != nil {
			return err
		}
	} else if cached {
		left, err = r.headTreeFiles()
		if err != nil {
			return err
		}
	} else {
		left = map[string]string{}
		for _, entry := range idx.entries {
			left[entry.path] = entry.hash
		}
	}
	right := map[string]string{}
	if len(revisions) == 2 {
		right, err = r.revisionTreeFiles(revisions[1])
		if err != nil {
			return err
		}
	} else if cached {
		for _, entry := range idx.entries {
			right[entry.path] = entry.hash
		}
	} else {
		paths := map[string]struct{}{}
		for path := range left {
			paths[path] = struct{}{}
		}
		for _, entry := range idx.entries {
			paths[entry.path] = struct{}{}
		}
		for path := range paths {
			hash, err := r.writeBlobFromWorktree(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			right[path] = hash
		}
	}
	return r.printDiff(left, right, stdout)
}

func (r *localRepository) revisionTreeFiles(revision string) (map[string]string, error) {
	hash, err := r.resolveRevision(revision)
	if err != nil {
		return nil, fmt.Errorf("unknown revision %q", revision)
	}
	commit, err := r.commitObject(hash)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	if err := r.collectTreeFiles(commit.tree, "", files); err != nil {
		return nil, err
	}
	return files, nil
}

func (r *localRepository) headTreeFiles() (map[string]string, error) {
	files := map[string]string{}
	if head, err := r.resolveRevision("HEAD"); err == nil {
		commit, err := r.commitObject(head)
		if err != nil {
			return nil, err
		}
		if err := r.collectTreeFiles(commit.tree, "", files); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func (r *localRepository) treeFilesForCommit(hash string) (map[string]treeFile, error) {
	commit, err := r.commitObject(hash)
	if err != nil {
		return nil, err
	}
	files := map[string]treeFile{}
	if err := r.collectTreeFileEntries(commit.tree, "", files); err != nil {
		return nil, err
	}
	return files, nil
}

func (r *localRepository) printChangeSummary(stdout io.Writer, before, after map[string]treeFile, fileStats bool) {
	summary := r.changeSummary(before, after)
	if summary.files == 0 {
		return
	}
	if fileStats {
		r.printFileStats(stdout, before, after)
	}
	parts := []string{fmt.Sprintf("%d %s changed", summary.files, plural(summary.files, "file", "files"))}
	if summary.insertions > 0 || len(summary.renamed) > 0 {
		parts = append(parts, fmt.Sprintf("%d %s(+)", summary.insertions, plural(summary.insertions, "insertion", "insertions")))
	}
	if summary.deletions > 0 || len(summary.renamed) > 0 {
		parts = append(parts, fmt.Sprintf("%d %s(-)", summary.deletions, plural(summary.deletions, "deletion", "deletions")))
	}
	fmt.Fprintf(stdout, " %s\n", strings.Join(parts, ", "))
	for _, path := range summary.created {
		fmt.Fprintf(stdout, " create mode 100644 %s\n", path)
	}
	for _, path := range summary.deleted {
		fmt.Fprintf(stdout, " delete mode 100644 %s\n", path)
	}
	for _, rename := range summary.renamed {
		fmt.Fprintf(stdout, " rename %s => %s (100%%)\n", rename.from, rename.to)
	}
}

func (r *localRepository) printStatSummary(stdout io.Writer, before, after map[string]treeFile) {
	summary := r.changeSummary(before, after)
	if summary.files == 0 {
		return
	}
	r.printFileStats(stdout, before, after)
	parts := []string{fmt.Sprintf("%d %s changed", summary.files, plural(summary.files, "file", "files"))}
	if summary.insertions > 0 || len(summary.renamed) > 0 {
		parts = append(parts, fmt.Sprintf("%d %s(+)", summary.insertions, plural(summary.insertions, "insertion", "insertions")))
	}
	if summary.deletions > 0 || len(summary.renamed) > 0 {
		parts = append(parts, fmt.Sprintf("%d %s(-)", summary.deletions, plural(summary.deletions, "deletion", "deletions")))
	}
	fmt.Fprintf(stdout, " %s\n", strings.Join(parts, ", "))
}

func (r *localRepository) printFileStats(stdout io.Writer, before, after map[string]treeFile) {
	renames := detectRenames(before, after)
	renamedFrom := map[string]struct{}{}
	renamedTo := map[string]struct{}{}
	for _, rename := range renames {
		renamedFrom[rename.from] = struct{}{}
		renamedTo[rename.to] = struct{}{}
		fmt.Fprintf(stdout, " %s | 0\n", renameStatPath(rename.from, rename.to))
	}
	paths := map[string]struct{}{}
	for path := range before {
		paths[path] = struct{}{}
	}
	for path := range after {
		paths[path] = struct{}{}
	}
	var sorted []string
	for path := range paths {
		if _, ok := renamedFrom[path]; ok {
			continue
		}
		if _, ok := renamedTo[path]; ok {
			continue
		}
		if before[path].hash != after[path].hash {
			sorted = append(sorted, path)
		}
	}
	sort.Strings(sorted)
	for _, path := range sorted {
		oldMeta, oldOK := before[path]
		newMeta, newOK := after[path]
		oldLines := r.blobLineCount(oldMeta.hash)
		newLines := r.blobLineCount(newMeta.hash)
		insertions := 0
		deletions := 0
		switch {
		case !oldOK:
			insertions = newLines
		case !newOK:
			deletions = oldLines
		case newLines >= oldLines:
			insertions = newLines - oldLines
		default:
			deletions = oldLines - newLines
		}
		total := insertions + deletions
		if total == 0 {
			total = 1
		}
		fmt.Fprintf(stdout, " %s | %d %s%s\n", path, total, strings.Repeat("+", insertions), strings.Repeat("-", deletions))
	}
}

func (r *localRepository) changeSummary(before, after map[string]treeFile) changeSummary {
	renames := detectRenames(before, after)
	renamedFrom := map[string]struct{}{}
	renamedTo := map[string]struct{}{}
	for _, rename := range renames {
		renamedFrom[rename.from] = struct{}{}
		renamedTo[rename.to] = struct{}{}
	}
	paths := map[string]struct{}{}
	for path := range before {
		paths[path] = struct{}{}
	}
	for path := range after {
		paths[path] = struct{}{}
	}
	var sorted []string
	for path := range paths {
		sorted = append(sorted, path)
	}
	sort.Strings(sorted)
	var summary changeSummary
	summary.renamed = renames
	summary.files += len(renames)
	for _, path := range sorted {
		if _, ok := renamedFrom[path]; ok {
			continue
		}
		if _, ok := renamedTo[path]; ok {
			continue
		}
		oldMeta, oldOK := before[path]
		newMeta, newOK := after[path]
		if oldOK && newOK && oldMeta.hash == newMeta.hash {
			continue
		}
		summary.files++
		if !oldOK {
			summary.created = append(summary.created, path)
		}
		if !newOK {
			summary.deleted = append(summary.deleted, path)
		}
		oldLines := r.blobLineCount(oldMeta.hash)
		newLines := r.blobLineCount(newMeta.hash)
		switch {
		case !oldOK:
			summary.insertions += newLines
		case !newOK:
			summary.deletions += oldLines
		default:
			if newLines >= oldLines {
				summary.insertions += newLines - oldLines
			} else {
				summary.deletions += oldLines - newLines
			}
		}
	}
	return summary
}

func (r *localRepository) blobLineCount(hash string) int {
	if hash == "" {
		return 0
	}
	obj, err := r.storeObject(hash)
	if err != nil {
		return 0
	}
	return len(splitLines(string(obj.data)))
}

func (r *localRepository) stashSubject(head string) string {
	branch := firstNonEmpty(r.currentBranch(), "HEAD")
	if head == "" {
		return "WIP on " + branch
	}
	commit, err := r.commitObject(head)
	if err != nil {
		return "WIP on " + branch
	}
	return fmt.Sprintf("WIP on %s: %s %s", branch, head[:7], commit.subject)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func formatGitDate(ts int64) string {
	return time.Unix(ts, 0).Format("Mon Jan 2 15:04:05 2006 -0700")
}

func detectRenames(before, after map[string]treeFile) []renameChange {
	var deleted []string
	var created []string
	for path, meta := range before {
		if _, ok := after[path]; !ok {
			for newPath, newMeta := range after {
				if _, existed := before[newPath]; !existed && meta.hash == newMeta.hash {
					deleted = append(deleted, path)
					break
				}
			}
		}
	}
	for path, meta := range after {
		if _, ok := before[path]; !ok {
			for oldPath, oldMeta := range before {
				if _, stillExists := after[oldPath]; !stillExists && meta.hash == oldMeta.hash {
					created = append(created, path)
					break
				}
			}
		}
	}
	sort.Strings(deleted)
	sort.Strings(created)
	used := map[string]struct{}{}
	var renames []renameChange
	for _, oldPath := range deleted {
		for _, newPath := range created {
			if _, ok := used[newPath]; ok {
				continue
			}
			if before[oldPath].hash == after[newPath].hash {
				used[newPath] = struct{}{}
				renames = append(renames, renameChange{from: oldPath, to: newPath, hash: before[oldPath].hash})
				break
			}
		}
	}
	return renames
}

func renameStatPath(from, to string) string {
	fromParts := strings.Split(from, "/")
	toParts := strings.Split(to, "/")
	start := 0
	for start < len(fromParts) && start < len(toParts) && fromParts[start] == toParts[start] {
		start++
	}
	endFrom := len(fromParts) - 1
	endTo := len(toParts) - 1
	for endFrom >= start && endTo >= start && fromParts[endFrom] == toParts[endTo] {
		endFrom--
		endTo--
	}
	oldMiddle := strings.Join(fromParts[start:endFrom+1], "/")
	newMiddle := strings.Join(toParts[start:endTo+1], "/")
	prefix := strings.Join(fromParts[:start], "/")
	suffix := strings.Join(fromParts[endFrom+1:], "/")
	middle := oldMiddle + " => " + newMiddle
	if prefix != "" {
		middle = prefix + "/" + middle
	}
	if suffix != "" {
		middle += "/" + suffix
	}
	return middle
}

func (r *localRepository) reset(args []string) error {
	mode := "mixed"
	rev := "HEAD"
	var paths []string
	pathMode := false
	for _, arg := range args {
		if pathMode {
			paths = append(paths, cleanRepoPath(arg))
			continue
		}
		switch arg {
		case "--soft":
			mode = "soft"
		case "--mixed":
			mode = "mixed"
		case "--hard":
			mode = "hard"
		case "--":
			pathMode = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unsupported reset option %s", arg)
			}
			if _, err := r.resolveRevision(arg); err == nil && len(paths) == 0 {
				rev = arg
			} else {
				paths = append(paths, cleanRepoPath(arg))
			}
		}
	}
	if len(paths) > 0 {
		if mode != "mixed" {
			return errors.New("path reset only supports mixed mode")
		}
		return r.resetIndexPaths(rev, paths)
	}
	hash, err := r.resolveRevision(rev)
	if err != nil {
		return err
	}
	if err := r.updateHEAD(hash); err != nil {
		return err
	}
	if mode == "soft" {
		return nil
	}
	commit, err := r.commitObject(hash)
	if err != nil {
		return err
	}
	idx, err := r.indexFromTree(commit.tree)
	if err != nil {
		return err
	}
	if mode == "hard" {
		return r.checkoutCommit(hash)
	}
	return r.writeIndex(idx)
}

func (r *localRepository) resetIndexPaths(rev string, paths []string) error {
	hash, err := r.resolveRevision(rev)
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
		}
		if !matched {
			idx.removePath(path)
		}
	}
	idx.sort()
	return r.writeIndex(idx)
}

func (r *localRepository) restore(args []string) error {
	source := "HEAD"
	sourceExplicit := false
	staged := false
	worktree := true
	var paths []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--staged":
			staged = true
			worktree = false
		case arg == "--worktree":
			worktree = true
		case arg == "--source":
			i++
			if i >= len(args) {
				return errors.New("--source requires a revision")
			}
			source = args[i]
			sourceExplicit = true
		case strings.HasPrefix(arg, "--source="):
			source = strings.TrimPrefix(arg, "--source=")
			sourceExplicit = true
		case strings.HasPrefix(arg, "-"):
			return fmt.Errorf("unsupported restore option %s", arg)
		default:
			paths = append(paths, cleanRepoPath(arg))
		}
	}
	if len(paths) == 0 {
		return errors.New("restore requires at least one path")
	}
	if worktree && !staged && !sourceExplicit {
		return r.restoreWorktreeFromIndex(paths)
	}
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
			if staged {
				idx.upsert(indexEntry{path: file, hash: meta.hash, mode: meta.mode})
			}
			if worktree {
				if err := r.writeBlobToWorktree(meta.hash, file); err != nil {
					return err
				}
			}
		}
		if !matched {
			if staged {
				idx.removePath(path)
			}
			if worktree {
				if err := os.RemoveAll(filepath.Join(r.worktree, filepath.FromSlash(path))); err != nil {
					return err
				}
			}
		}
	}
	idx.sort()
	return r.writeIndex(idx)
}

func (r *localRepository) restoreWorktreeFromIndex(paths []string) error {
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	entries := idx.byPath()
	for _, path := range paths {
		prefix := strings.TrimSuffix(path, "/") + "/"
		matched := false
		for file, entry := range entries {
			if file != path && !strings.HasPrefix(file, prefix) {
				continue
			}
			matched = true
			if err := r.writeBlobToWorktree(entry.hash, file); err != nil {
				return err
			}
		}
		if !matched {
			if err := os.RemoveAll(filepath.Join(r.worktree, filepath.FromSlash(path))); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *localRepository) stash(args []string, stdout io.Writer) error {
	action := "push"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "list":
		if hash, err := r.readRef("refs/stash"); err == nil {
			commit, err := r.commitObject(hash)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "stash@{0}: %s\n", commit.subject)
		}
		return nil
	case "pop", "apply":
		hash, err := r.readRef("refs/stash")
		if err != nil {
			return errors.New("No stash entries found.")
		}
		if err := r.checkoutCommit(hash); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "On branch "+firstNonEmpty(r.currentBranch(), "HEAD"))
		fmt.Fprintln(stdout, "Changes not staged for commit:")
		fmt.Fprintln(stdout, "  (use \"bgit add <file>...\" to update what will be committed)")
		fmt.Fprintln(stdout, "  (use \"bgit restore <file>...\" to discard changes in working directory)")
		if action == "pop" {
			return r.deleteRef("refs/stash")
		}
		return nil
	case "drop":
		if err := r.deleteRef("refs/stash"); err != nil {
			return errors.New("No stash entries found.")
		}
		fmt.Fprintln(stdout, "Dropped refs/stash@{0}")
		return nil
	case "push", "save":
		idx, err := r.readIndex()
		if err != nil {
			return err
		}
		for _, entry := range append([]indexEntry{}, idx.entries...) {
			if _, err := os.Stat(filepath.Join(r.worktree, filepath.FromSlash(entry.path))); errors.Is(err, fs.ErrNotExist) {
				idx.removePath(entry.path)
				continue
			}
			if err := r.addPathToIndex(&idx, entry.path); err != nil {
				return err
			}
		}
		files, err := r.allWorktreeFiles()
		if err != nil {
			return err
		}
		for _, file := range files {
			if err := r.addPathToIndex(&idx, file); err != nil {
				return err
			}
		}
		tree, err := r.writeTree(idx)
		if err != nil {
			return err
		}
		head, _ := r.resolveRevision("HEAD")
		subject := r.stashSubject(head)
		hash, err := r.commitWithParents(tree, []string{head}, subject)
		if err != nil {
			return err
		}
		if err := r.writeRef("refs/stash", hash); err != nil {
			return err
		}
		if head != "" {
			if err := r.checkoutCommit(head); err != nil {
				return err
			}
		}
		fmt.Fprintf(stdout, "Saved working directory and index state %s\n", subject)
		return nil
	default:
		return fmt.Errorf("unsupported stash command %s", action)
	}
}

func (r *localRepository) revert(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("revert requires exactly one commit")
	}
	return r.applyCommitDelta(args[0], true, "Revert", stdout)
}

func (r *localRepository) cherryPick(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("cherry-pick requires exactly one commit")
	}
	return r.applyCommitDelta(args[0], false, "Cherry-pick", stdout)
}

func (r *localRepository) grep(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("grep requires a pattern")
	}
	pattern := args[0]
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	for _, entry := range idx.entries {
		data, err := os.ReadFile(filepath.Join(r.worktree, filepath.FromSlash(entry.path)))
		if err != nil {
			continue
		}
		for n, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, pattern) {
				fmt.Fprintf(stdout, "%s:%d:%s\n", entry.path, n+1, line)
			}
		}
	}
	return nil
}

func (r *localRepository) blame(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("blame requires exactly one path")
	}
	path := cleanRepoPath(args[0])
	hash, err := r.resolveRevision("HEAD")
	if err != nil {
		return err
	}
	commit, err := r.commitObject(hash)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(r.worktree, filepath.FromSlash(path)))
	if err != nil {
		return err
	}
	short := hash
	if len(short) > 8 {
		short = short[:8]
	}
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		fmt.Fprintf(stdout, "%s (%s %d) %s\n", short, commit.author, i+1, line)
	}
	return nil
}

func (r *localRepository) clean(args []string, stdout io.Writer) error {
	dryRun := true
	force := false
	dirs := false
	for _, arg := range args {
		switch arg {
		case "-n", "--dry-run":
			dryRun = true
		case "-f", "--force":
			force = true
			dryRun = false
		case "-d":
			dirs = true
		default:
			return fmt.Errorf("unsupported clean option %s", arg)
		}
	}
	if !force && !dryRun {
		return errors.New("clean requires -f or -n")
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	files, err := r.untrackedFiles(idx.byPath())
	if err != nil {
		return err
	}
	for _, file := range files {
		if !dirs && strings.Contains(file, "/") {
			continue
		}
		if dryRun {
			fmt.Fprintf(stdout, "Would remove %s\n", file)
		} else {
			if err := os.Remove(filepath.Join(r.worktree, filepath.FromSlash(file))); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			fmt.Fprintf(stdout, "Removing %s\n", file)
		}
	}
	return nil
}

func (r *localRepository) describe(args []string, stdout io.Writer) error {
	rev := "HEAD"
	if len(args) > 0 {
		rev = args[0]
	}
	head, err := r.resolveRevision(rev)
	if err != nil {
		return err
	}
	tags, err := r.listRefs("refs/tags")
	if err != nil {
		return err
	}
	for _, ref := range tags {
		if hash, err := r.readRef(ref); err == nil && hash == head {
			fmt.Fprintln(stdout, strings.TrimPrefix(ref, "refs/tags/"))
			return nil
		}
	}
	commits, err := newNativeGitRepoForStore(config{branch: defaultBranch}, r.store).walkCommits(nilContext{}, head, 10000, 0, "")
	if err != nil {
		return err
	}
	distance := map[string]int{}
	for i, commit := range commits {
		distance[commit.hash] = i
	}
	type candidate struct {
		name string
		hash string
		dist int
	}
	var candidates []candidate
	for _, ref := range tags {
		hash, err := r.readRef(ref)
		if err != nil {
			continue
		}
		if d, ok := distance[hash]; ok {
			candidates = append(candidates, candidate{strings.TrimPrefix(ref, "refs/tags/"), hash, d})
		}
	}
	if len(candidates) == 0 {
		return errors.New("No names found, cannot describe anything.")
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].dist < candidates[j].dist })
	best := candidates[0]
	if best.dist == 0 {
		fmt.Fprintln(stdout, best.name)
	} else {
		fmt.Fprintf(stdout, "%s-%d-g%s\n", best.name, best.dist, head[:7])
	}
	return nil
}

func (r *localRepository) lsFiles(args []string, stdout io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("unsupported ls-files option %s", args[0])
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	idx.sort()
	for _, entry := range idx.entries {
		fmt.Fprintln(stdout, entry.path)
	}
	return nil
}

func (r *localRepository) lsTree(args []string, stdout io.Writer) error {
	recursive := false
	rev := "HEAD"
	for _, arg := range args {
		switch arg {
		case "-r":
			recursive = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unsupported ls-tree option %s", arg)
			}
			rev = arg
		}
	}
	hash, err := r.resolveRevision(rev)
	if err != nil {
		return err
	}
	commit, err := r.commitObject(hash)
	if err != nil {
		return err
	}
	return r.printTree(commit.tree, "", recursive, stdout)
}

func (r *localRepository) archive(args []string, stdout io.Writer) error {
	rev := "HEAD"
	for _, arg := range args {
		if strings.HasPrefix(arg, "--format=") {
			if strings.TrimPrefix(arg, "--format=") != "tar" {
				return errors.New("archive supports only --format=tar")
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unsupported archive option %s", arg)
		}
		rev = arg
	}
	hash, err := r.resolveRevision(rev)
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
	tw := tar.NewWriter(stdout)
	defer tw.Close()
	var paths []string
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		obj, err := r.storeObject(files[path].hash)
		if err != nil {
			return err
		}
		mode := int64(0o644)
		if files[path].mode == 0100755 {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: mode, Size: int64(len(obj.data)), ModTime: time.Unix(commit.timestamp, 0)}); err != nil {
			return err
		}
		if _, err := tw.Write(obj.data); err != nil {
			return err
		}
	}
	return nil
}

func (r *localRepository) show(args []string, stdout io.Writer) error {
	rev := "HEAD"
	stat := false
	for _, arg := range args {
		switch arg {
		case "--stat":
			stat = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unsupported show option %s", arg)
			}
			rev = arg
		}
	}
	if base, path, ok := strings.Cut(rev, ":"); ok {
		if base == "" {
			base = "HEAD"
		}
		hash, err := r.resolveRevision(base)
		if err != nil {
			return err
		}
		commit, err := r.commitObject(hash)
		if err != nil {
			return err
		}
		blob, err := r.findPathInTree(commit.tree, cleanRepoPath(path))
		if err != nil {
			return err
		}
		obj, err := r.storeObject(blob)
		if err != nil {
			return err
		}
		_, err = stdout.Write(obj.data)
		return err
	}
	hash, err := r.resolveRevision(rev)
	if err != nil {
		return err
	}
	obj, err := r.storeObject(hash)
	if err == nil && obj.typ == gitObjectBlob {
		_, err = stdout.Write(obj.data)
		return err
	}
	commit, err := r.commitObject(hash)
	if err != nil {
		return err
	}
	raw, _ := r.storeObject(hash)
	fmt.Fprintf(stdout, "commit %s\nAuthor: %s <%s>\nDate:   %s\n\n", commit.hash, commit.author, commit.email, formatGitDate(commit.timestamp))
	message := raw.data
	if split := bytes.Index(raw.data, []byte("\n\n")); split >= 0 {
		message = raw.data[split+2:]
	}
	for _, line := range strings.Split(strings.TrimRight(string(message), "\n"), "\n") {
		fmt.Fprintf(stdout, "    %s\n", line)
	}
	fmt.Fprintln(stdout)
	if stat {
		before := map[string]treeFile{}
		if len(commit.parents) > 0 {
			before, err = r.treeFilesForCommit(commit.parents[0])
			if err != nil {
				return err
			}
		}
		after := map[string]treeFile{}
		if err := r.collectTreeFileEntries(commit.tree, "", after); err != nil {
			return err
		}
		r.printStatSummary(stdout, before, after)
	}
	return nil
}

func (r *localRepository) merge(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("merge requires exactly one branch or commit")
	}
	head, err := r.resolveRevision("HEAD")
	if err != nil {
		return err
	}
	other, err := r.resolveRevision(args[0])
	if err != nil {
		return err
	}
	if ok, err := r.isAncestor(head, other); err != nil {
		return err
	} else if ok {
		before, err := r.treeFilesForCommit(head)
		if err != nil {
			return err
		}
		after, err := r.treeFilesForCommit(other)
		if err != nil {
			return err
		}
		if err := r.updateHEAD(other); err != nil {
			return err
		}
		if err := r.checkoutCommit(other); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Updating %s..%s\n", head[:7], other[:7])
		fmt.Fprintln(stdout, "Fast-forward")
		r.printChangeSummary(stdout, before, after, true)
		return nil
	}
	base, err := r.mergeBase(head, other)
	if err != nil {
		return err
	}
	tree, err := r.mergeTrees(base, head, other)
	if err != nil {
		return err
	}
	message := fmt.Sprintf("Merge %s", args[0])
	hash, err := r.commitWithParents(tree, []string{head, other}, message)
	if err != nil {
		return err
	}
	if err := r.updateHEAD(hash); err != nil {
		return err
	}
	if err := r.checkoutCommit(hash); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Merge made commit %s\n", hash[:7])
	return nil
}

func (r *localRepository) applyCommitDelta(rev string, reverse bool, label string, stdout io.Writer) error {
	if dirty, err := r.isDirty(); err != nil {
		return err
	} else if dirty {
		return errors.New("working tree has changes; commit or stash them first")
	}
	hash, err := r.resolveRevision(rev)
	if err != nil {
		return err
	}
	commit, err := r.commitObject(hash)
	if err != nil {
		return err
	}
	if len(commit.parents) > 1 {
		return errors.New("merge commits are not supported")
	}
	parentFiles := map[string]treeFile{}
	if len(commit.parents) == 1 {
		parent, err := r.commitObject(commit.parents[0])
		if err != nil {
			return err
		}
		if err := r.collectTreeFileEntries(parent.tree, "", parentFiles); err != nil {
			return err
		}
	}
	commitFiles := map[string]treeFile{}
	if err := r.collectTreeFileEntries(commit.tree, "", commitFiles); err != nil {
		return err
	}
	from, to := parentFiles, commitFiles
	if reverse {
		from, to = commitFiles, parentFiles
	}
	idx, err := r.applyTreeDelta(from, to)
	if err != nil {
		return err
	}
	tree, err := r.writeTree(idx)
	if err != nil {
		return err
	}
	head, _ := r.resolveRevision("HEAD")
	msg := commit.subject
	if reverse {
		msg = "Revert \"" + commit.subject + "\""
	}
	newHash, err := r.commitWithParents(tree, []string{head}, msg)
	if err != nil {
		return err
	}
	if err := r.updateHEAD(newHash); err != nil {
		return err
	}
	before := map[string]treeFile{}
	if head != "" {
		before, _ = r.treeFilesForCommit(head)
	}
	after, _ := r.treeFilesForCommit(newHash)
	branch := firstNonEmpty(r.currentBranch(), "HEAD")
	fmt.Fprintf(stdout, "[%s %s] %s\n", branch, newHash[:7], msg)
	r.printChangeSummary(stdout, before, after, false)
	return nil
}

func (r *localRepository) printDiff(left, right map[string]string, stdout io.Writer) error {
	paths := map[string]struct{}{}
	for path := range left {
		paths[path] = struct{}{}
	}
	for path := range right {
		paths[path] = struct{}{}
	}
	var sorted []string
	for path := range paths {
		if left[path] != right[path] {
			sorted = append(sorted, path)
		}
	}
	sort.Strings(sorted)
	for _, path := range sorted {
		var leftData, rightData []byte
		if left[path] != "" {
			obj, err := r.storeObject(left[path])
			if err != nil {
				return err
			}
			leftData = obj.data
		}
		if right[path] != "" {
			obj, err := r.storeObject(right[path])
			if err != nil {
				return err
			}
			rightData = obj.data
		}
		fmt.Fprintf(stdout, "diff --git a/%s b/%s\n", path, path)
		leftShort := "0000000"
		rightShort := "0000000"
		if left[path] != "" {
			leftShort = left[path][:7]
		}
		if right[path] != "" {
			rightShort = right[path][:7]
		}
		fmt.Fprintf(stdout, "index %s..%s 100644\n", leftShort, rightShort)
		if left[path] == "" {
			fmt.Fprintln(stdout, "new file mode 100644")
			fmt.Fprintf(stdout, "--- /dev/null\n+++ b/%s\n", path)
		} else if right[path] == "" {
			fmt.Fprintln(stdout, "deleted file mode 100644")
			fmt.Fprintf(stdout, "--- a/%s\n+++ /dev/null\n", path)
		} else {
			fmt.Fprintf(stdout, "--- a/%s\n+++ b/%s\n", path, path)
		}
		for _, line := range simpleLineDiff(string(leftData), string(rightData)) {
			fmt.Fprintln(stdout, line)
		}
	}
	return nil
}

func simpleLineDiff(left, right string) []string {
	a := splitLines(left)
	b := splitLines(right)
	if left == right {
		return nil
	}
	if simpleLineDiffShouldFallback(len(a), len(b)) {
		return simpleWholeFileDiff(a, b)
	}
	ops := simpleLineDiffOps(a, b)
	hunks := simpleLineDiffHunks(ops, 3)
	var out []string
	for _, hunk := range hunks {
		oldStart, oldCount, newStart, newCount := simpleDiffHunkRange(ops[hunk.start:hunk.end])
		out = append(out, fmt.Sprintf("@@ %s %s @@", hunkRangeFrom(oldStart, oldCount, "-"), hunkRangeFrom(newStart, newCount, "+")))
		for _, op := range ops[hunk.start:hunk.end] {
			out = append(out, string(op.kind)+op.text)
		}
	}
	return out
}

type simpleDiffOp struct {
	kind    byte
	text    string
	oldLine int
	newLine int
}

type simpleDiffHunk struct {
	start int
	end   int
}

func simpleLineDiffOps(a, b []string) []simpleDiffOp {
	if simpleLineDiffShouldFallback(len(a), len(b)) {
		return simpleWholeFileDiffOps(a, b)
	}
	rowCount := len(a) + 1
	colCount := len(b) + 1
	rows := make([][]int, rowCount)
	for i := range rows {
		rows[i] = make([]int, colCount)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				rows[i][j] = rows[i+1][j+1] + 1
			} else if rows[i+1][j] >= rows[i][j+1] {
				rows[i][j] = rows[i+1][j]
			} else {
				rows[i][j] = rows[i][j+1]
			}
		}
	}
	i, j := 0, 0
	var ops []simpleDiffOp
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			ops = append(ops, simpleDiffOp{kind: ' ', text: a[i], oldLine: i + 1, newLine: j + 1})
			i++
			j++
		case i < len(a) && (j == len(b) || rows[i+1][j] >= rows[i][j+1]):
			ops = append(ops, simpleDiffOp{kind: '-', text: a[i], oldLine: i + 1, newLine: j + 1})
			i++
		case j < len(b):
			ops = append(ops, simpleDiffOp{kind: '+', text: b[j], oldLine: i + 1, newLine: j + 1})
			j++
		}
	}
	return ops
}

func simpleLineDiffShouldFallback(leftLines, rightLines int) bool {
	const maxSimpleDiffCells = 900000
	maxInt := int(^uint(0) >> 1)
	if leftLines >= maxInt || rightLines >= maxInt {
		return true
	}
	if leftLines != 0 && rightLines > maxSimpleDiffCells/leftLines {
		return true
	}
	return false
}

func simpleWholeFileDiffOps(a, b []string) []simpleDiffOp {
	var ops []simpleDiffOp
	for i, line := range a {
		ops = append(ops, simpleDiffOp{kind: '-', text: line, oldLine: i + 1})
	}
	for i, line := range b {
		ops = append(ops, simpleDiffOp{kind: '+', text: line, newLine: i + 1})
	}
	return ops
}

func simpleLineDiffHunks(ops []simpleDiffOp, context int) []simpleDiffHunk {
	var hunks []simpleDiffHunk
	for i, op := range ops {
		if op.kind == ' ' {
			continue
		}
		start := i - context
		if start < 0 {
			start = 0
		}
		end := i + context + 1
		if end > len(ops) {
			end = len(ops)
		}
		if len(hunks) > 0 && start <= hunks[len(hunks)-1].end {
			if end > hunks[len(hunks)-1].end {
				hunks[len(hunks)-1].end = end
			}
			continue
		}
		hunks = append(hunks, simpleDiffHunk{start: start, end: end})
	}
	return hunks
}

func simpleDiffHunkRange(ops []simpleDiffOp) (int, int, int, int) {
	oldStart, newStart := 0, 0
	oldCount, newCount := 0, 0
	for _, op := range ops {
		if op.kind != '+' {
			if oldStart == 0 {
				oldStart = op.oldLine
			}
			oldCount++
		}
		if op.kind != '-' {
			if newStart == 0 {
				newStart = op.newLine
			}
			newCount++
		}
	}
	if oldStart == 0 && len(ops) > 0 {
		oldStart = ops[0].oldLine - 1
	}
	if newStart == 0 && len(ops) > 0 {
		newStart = ops[0].newLine - 1
	}
	return oldStart, oldCount, newStart, newCount
}

func hunkRangeFrom(start, count int, prefix string) string {
	if count == 0 {
		return prefix + strconv.Itoa(start) + ",0"
	}
	if count == 1 {
		return prefix + strconv.Itoa(start)
	}
	return prefix + strconv.Itoa(start) + "," + strconv.Itoa(count)
}

func simpleWholeFileDiff(a, b []string) []string {
	var out []string
	out = append(out, fmt.Sprintf("@@ %s %s @@", hunkRange("-", len(a)), hunkRange("+", len(b))))
	for _, line := range a {
		out = append(out, "-"+line)
	}
	for _, line := range b {
		out = append(out, "+"+line)
	}
	return out
}

func hunkRange(prefix string, lines int) string {
	if lines == 0 {
		return prefix + "0,0"
	}
	if lines == 1 {
		return prefix + "1"
	}
	return prefix + "1," + strconv.Itoa(lines)
}

func splitLines(text string) []string {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func (r *localRepository) collectTreeFileEntries(treeHash, base string, out map[string]treeFile) error {
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
			if err := r.collectTreeFileEntries(entry.hash, path, out); err != nil {
				return err
			}
			continue
		}
		mode := uint32(0100644)
		if entry.mode == "100755" {
			mode = 0100755
		}
		out[path] = treeFile{hash: entry.hash, mode: mode}
	}
	return nil
}

func (r *localRepository) indexFromTree(treeHash string) (gitIndex, error) {
	files := map[string]treeFile{}
	if err := r.collectTreeFileEntries(treeHash, "", files); err != nil {
		return gitIndex{}, err
	}
	var idx gitIndex
	for path, meta := range files {
		idx.entries = append(idx.entries, indexEntry{path: path, hash: meta.hash, mode: meta.mode})
	}
	idx.sort()
	return idx, nil
}

func (r *localRepository) writeBlobToWorktree(hash, path string) error {
	obj, err := r.storeObject(hash)
	if err != nil {
		return err
	}
	target := filepath.Join(r.worktree, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, obj.data, 0o644)
}

func (r *localRepository) findPathInTree(treeHash, path string) (string, error) {
	repo := newNativeGitRepoForStore(config{branch: defaultBranch}, r.store)
	return repo.findPath(nilContext{}, treeHash, path)
}

func (r *localRepository) commitWithParents(treeHash string, parents []string, message string) (string, error) {
	authorName := firstNonEmpty(os.Getenv("GIT_AUTHOR_NAME"), r.identityValue("user.name"), defaultBucketGitIdentityName)
	authorEmail := firstNonEmpty(os.Getenv("GIT_AUTHOR_EMAIL"), r.identityValue("user.email"), defaultBucketGitIdentityEmail())
	committerName := firstNonEmpty(os.Getenv("GIT_COMMITTER_NAME"), authorName)
	committerEmail := firstNonEmpty(os.Getenv("GIT_COMMITTER_EMAIL"), authorEmail)
	now := time.Now()
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", treeHash)
	for _, parent := range parents {
		if parent != "" {
			fmt.Fprintf(&buf, "parent %s\n", parent)
		}
	}
	fmt.Fprintf(&buf, "author %s <%s> %d %s\n", authorName, authorEmail, now.Unix(), timezoneOffset(now))
	fmt.Fprintf(&buf, "committer %s <%s> %d %s\n\n", committerName, committerEmail, now.Unix(), timezoneOffset(now))
	buf.WriteString(message)
	buf.WriteByte('\n')
	return r.writeObject(gitObjectCommit, buf.Bytes())
}

func (r *localRepository) applyTreeDelta(from, to map[string]treeFile) (gitIndex, error) {
	idx, err := r.readIndex()
	if err != nil {
		return gitIndex{}, err
	}
	paths := map[string]struct{}{}
	for path := range from {
		paths[path] = struct{}{}
	}
	for path := range to {
		paths[path] = struct{}{}
	}
	for path := range paths {
		if from[path].hash == to[path].hash {
			continue
		}
		if to[path].hash == "" {
			idx.removePath(path)
			if err := os.Remove(filepath.Join(r.worktree, filepath.FromSlash(path))); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return gitIndex{}, err
			}
			continue
		}
		if err := r.writeBlobToWorktree(to[path].hash, path); err != nil {
			return gitIndex{}, err
		}
		if err := r.addPathToIndex(&idx, path); err != nil {
			return gitIndex{}, err
		}
	}
	idx.sort()
	if err := r.writeIndex(idx); err != nil {
		return gitIndex{}, err
	}
	return idx, nil
}

func (r *localRepository) isDirty() (bool, error) {
	var buf bytes.Buffer
	if err := r.status([]string{"--short"}, &buf); err != nil {
		return false, err
	}
	return strings.TrimSpace(buf.String()) != "", nil
}

func (r *localRepository) printTree(treeHash, base string, recursive bool, stdout io.Writer) error {
	repo := newNativeGitRepoForStore(config{branch: defaultBranch}, r.store)
	entries, err := repo.treeEntries(nilContext{}, treeHash)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	for _, entry := range entries {
		path := entry.name
		if base != "" {
			path = base + "/" + entry.name
		}
		fmt.Fprintf(stdout, "%06s %s %s\t%s\n", entry.mode, entry.typ, entry.hash, path)
		if recursive && entry.typ == gitObjectTree {
			if err := r.printTree(entry.hash, path, true, stdout); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *localRepository) mergeBase(a, b string) (string, error) {
	ancestors := map[string]struct{}{}
	stack := []string{a}
	for len(stack) > 0 {
		hash := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := ancestors[hash]; ok {
			continue
		}
		ancestors[hash] = struct{}{}
		commit, err := r.commitObject(hash)
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
		commit, err := r.commitObject(hash)
		if err != nil {
			return "", err
		}
		stack = append(stack, commit.parents...)
	}
	return "", errors.New("no merge base found")
}

func (r *localRepository) mergeTrees(baseHash, oursHash, theirsHash string) (string, error) {
	baseCommit, err := r.commitObject(baseHash)
	if err != nil {
		return "", err
	}
	oursCommit, err := r.commitObject(oursHash)
	if err != nil {
		return "", err
	}
	theirsCommit, err := r.commitObject(theirsHash)
	if err != nil {
		return "", err
	}
	base := map[string]treeFile{}
	ours := map[string]treeFile{}
	theirs := map[string]treeFile{}
	if err := r.collectTreeFileEntries(baseCommit.tree, "", base); err != nil {
		return "", err
	}
	if err := r.collectTreeFileEntries(oursCommit.tree, "", ours); err != nil {
		return "", err
	}
	if err := r.collectTreeFileEntries(theirsCommit.tree, "", theirs); err != nil {
		return "", err
	}
	merged := map[string]treeFile{}
	for path, meta := range ours {
		merged[path] = meta
	}
	paths := map[string]struct{}{}
	for path := range base {
		paths[path] = struct{}{}
	}
	for path := range ours {
		paths[path] = struct{}{}
	}
	for path := range theirs {
		paths[path] = struct{}{}
	}
	for path := range paths {
		b := base[path]
		o := ours[path]
		t := theirs[path]
		oursChanged := o.hash != b.hash
		theirsChanged := t.hash != b.hash
		switch {
		case oursChanged && theirsChanged && o.hash != t.hash:
			return "", fmt.Errorf("merge conflict in %s", path)
		case theirsChanged:
			if t.hash == "" {
				delete(merged, path)
			} else {
				merged[path] = t
			}
		}
	}
	var idx gitIndex
	for path, meta := range merged {
		idx.entries = append(idx.entries, indexEntry{path: path, hash: meta.hash, mode: meta.mode})
	}
	idx.sort()
	return r.writeTree(idx)
}
