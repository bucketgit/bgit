package main

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type ignoreMatcher struct {
	rules []ignoreRule
}

type ignoreRule struct {
	pattern   string
	domain    string
	negated   bool
	dirOnly   bool
	hasSlash  bool
	anchored  bool
	recursive bool
}

func (r *localRepository) loadIgnoreMatcher() (*ignoreMatcher, error) {
	m := &ignoreMatcher{}
	if err := m.addIgnoreFile(filepath.Join(r.gitDir, "info", "exclude"), ""); err != nil {
		return nil, err
	}
	if err := filepath.WalkDir(r.worktree, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == r.gitDir || strings.HasPrefix(path, r.gitDir+string(filepath.Separator)) {
			return filepath.SkipDir
		}
		if !entry.IsDir() {
			return nil
		}
		if entry.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(r.worktree, path)
		if err != nil {
			return err
		}
		domain := ""
		if rel != "." {
			domain = filepath.ToSlash(rel)
		}
		if domain != "" && m.Match(domain, true) {
			return filepath.SkipDir
		}
		return m.addIgnoreFile(filepath.Join(path, ".gitignore"), domain)
	}); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *ignoreMatcher) addIgnoreFile(path, domain string) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if rule, ok := parseIgnoreRule(scanner.Text(), domain); ok {
			m.rules = append(m.rules, rule)
		}
	}
	return scanner.Err()
}

func parseIgnoreRule(line, domain string) (ignoreRule, bool) {
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	rule := ignoreRule{domain: strings.Trim(domain, "/")}
	if strings.HasPrefix(line, "!") {
		rule.negated = true
		line = strings.TrimPrefix(line, "!")
	}
	if !strings.HasSuffix(line, `\ `) {
		line = strings.TrimRight(line, " ")
	}
	line = strings.ReplaceAll(line, `\ `, " ")
	if strings.HasPrefix(line, "/") {
		rule.anchored = true
		line = strings.TrimLeft(line, "/")
	}
	if strings.HasSuffix(line, "/") {
		rule.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if line == "" {
		return ignoreRule{}, false
	}
	rule.pattern = filepath.ToSlash(line)
	rule.hasSlash = strings.Contains(rule.pattern, "/")
	rule.recursive = strings.Contains(rule.pattern, "**")
	return rule, true
}

func (m *ignoreMatcher) Match(path string, isDir bool) bool {
	path = cleanRepoPath(path)
	if path == "." || path == "" {
		return false
	}
	ignored := false
	for _, rule := range m.rules {
		if rule.match(path, isDir) {
			ignored = !rule.negated
		}
	}
	return ignored
}

func (r ignoreRule) match(path string, isDir bool) bool {
	rel, ok := r.relativePath(path)
	if !ok {
		return false
	}
	if r.dirOnly && !isDir {
		for _, dir := range parentDirs(rel) {
			if r.matchRelative(dir) {
				return true
			}
		}
		return false
	}
	return r.matchRelative(rel)
}

func (r ignoreRule) matchRelative(rel string) bool {
	if r.hasSlash || r.anchored {
		return matchIgnorePathPattern(r.pattern, rel, r.recursive)
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		if wildcardMatch(r.pattern, part) {
			return true
		}
	}
	return false
}

func parentDirs(path string) []string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) <= 1 {
		return nil
	}
	dirs := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		dirs = append(dirs, strings.Join(parts[:i], "/"))
	}
	return dirs
}

func (r ignoreRule) relativePath(path string) (string, bool) {
	if r.domain == "" {
		return path, true
	}
	if path == r.domain {
		return "", false
	}
	prefix := r.domain + "/"
	if strings.HasPrefix(path, prefix) {
		return strings.TrimPrefix(path, prefix), true
	}
	return "", false
}

func matchIgnorePathPattern(pattern, path string, recursive bool) bool {
	pattern = strings.Trim(pattern, "/")
	path = strings.Trim(path, "/")
	if pattern == "" || path == "" {
		return false
	}
	if !recursive {
		ok, err := filepath.Match(pattern, path)
		return err == nil && ok
	}
	return matchPathParts(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func matchPathParts(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		for len(pattern) > 1 && pattern[1] == "**" {
			pattern = pattern[1:]
		}
		if matchPathParts(pattern[1:], path) {
			return true
		}
		for i := range path {
			if matchPathParts(pattern[1:], path[i+1:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	ok, err := filepath.Match(pattern[0], path[0])
	return err == nil && ok && matchPathParts(pattern[1:], path[1:])
}

func wildcardMatch(pattern, name string) bool {
	ok, err := filepath.Match(pattern, name)
	return err == nil && ok
}
