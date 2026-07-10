package bgit

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPublicPackageDependencies(t *testing.T) {
	publicRoots := []string{"protocol", "store", "repository", "broker", "transport"}
	for _, root := range publicRoots {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range file.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if strings.Contains(importPath, "/internal/") || importPath == "github.com/bucketgit/bgit/internal" {
					t.Errorf("public package file %s imports internal package %s", path, importPath)
				}
				if root == "protocol" && strings.HasPrefix(importPath, "github.com/bucketgit/bgit/") {
					t.Errorf("protocol file %s imports higher-level BucketGit package %s", path, importPath)
				}
				if root == "repository" && (strings.Contains(importPath, "cloud.google.com/") || strings.Contains(importPath, "aws-sdk-go") || strings.Contains(importPath, "/broker/")) {
					t.Errorf("repository file %s imports provider or broker package %s", path, importPath)
				}
				if root == "store" && strings.Contains(importPath, "github.com/bucketgit/bgit/broker/") {
					t.Errorf("store file %s imports broker package %s", path, importPath)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
