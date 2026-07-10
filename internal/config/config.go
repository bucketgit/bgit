package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

type Repository struct {
	Provider string
	Bucket   string
	Prefix   string
	Branch   string
	Origin   string
}

func ParseRepositoryURI(raw string) (Repository, string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return Repository{}, "", err
	}
	provider, scheme := "", parsed.Scheme
	switch scheme {
	case "gs", "gcs":
		provider, scheme = "gcs", "gs"
	case "s3":
		provider = "s3"
	default:
		return Repository{}, "", errors.New("repository URI must start with gs:// or s3://")
	}
	if parsed.Host == "" {
		return Repository{}, "", errors.New("repository URI must include a bucket name")
	}
	prefix := strings.Trim(parsed.Path, "/")
	if prefix == "" {
		return Repository{}, "", errors.New("repository URI must include a repository prefix")
	}
	parts := strings.Split(prefix, "/")
	name := parts[len(parts)-1]
	origin := fmt.Sprintf("%s://%s/%s", scheme, parsed.Host, prefix)
	return Repository{Provider: provider, Bucket: parsed.Host, Prefix: prefix, Branch: "main", Origin: origin}, name, nil
}

func LocalSelection(value string) (string, string) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "local:")
	if value == "" {
		return "default", "default"
	}
	for _, separator := range []string{"/", "."} {
		if strings.Contains(value, separator) {
			parts := strings.SplitN(value, separator, 2)
			return fallback(parts[0]), fallback(parts[1])
		}
	}
	return value, "default"
}

func NormalizeLogicalRepository(name string) (string, error) {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".git")
	if name == "" {
		return "", errors.New("logical repo name is required")
	}
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("logical repo names must be flat; use %q instead of a path", filepath.Base(name))
	}
	if name == "." || name == ".." {
		return "", errors.New("logical repo name is invalid")
	}
	return name + ".git", nil
}

func fallback(value string) string {
	if strings.TrimSpace(value) == "" {
		return "default"
	}
	return strings.TrimSpace(value)
}
