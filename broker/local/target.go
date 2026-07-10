package local

import (
	"errors"
	"net/url"
	"strings"

	"github.com/bucketgit/bgit/protocol"
)

// TargetKind identifies how a BucketGit repository address must be resolved.
type TargetKind string

const (
	TargetLogicalAlias     TargetKind = "logical-alias"
	TargetStorageShorthand TargetKind = "storage-shorthand"
	TargetStorageExplicit  TargetKind = "storage-explicit"
)

// Target is a provider-neutral classification of a local-broker repository
// address. Shorthand targets have no Prefix and require identity-based bucket
// resolution; explicit targets name a physical bucket and repository prefix.
type Target struct {
	Kind     TargetKind
	Scheme   string
	Logical  string
	Bucket   string
	Prefix   string
	Original string
}

// ParseTarget classifies logical aliases and file, S3, or GCS repository
// addresses without performing credential lookup or provisioning resources.
func ParseTarget(raw string) (Target, error) {
	original := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "bgit::"))
	if original == "" {
		return Target{}, errors.New("repository target is required")
	}
	if !strings.Contains(original, "://") {
		logical, err := normalizeTargetLogical(original)
		if err != nil {
			return Target{}, err
		}
		return Target{Kind: TargetLogicalAlias, Logical: logical, Original: original}, nil
	}
	parsed, err := url.Parse(original)
	if err != nil {
		return Target{}, err
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if scheme == "gcs" {
		scheme = "gs"
	}
	if scheme != "file" && scheme != "s3" && scheme != "gs" {
		return Target{}, errors.New("repository target must use file://, s3://, or gs://")
	}
	name := strings.TrimSpace(parsed.Host)
	if name == "" && scheme == "file" {
		name = strings.Trim(strings.TrimSpace(parsed.Path), "/")
		parsed.Path = ""
	}
	if name == "" {
		return Target{}, errors.New("repository target must include a repository or bucket name")
	}
	prefix := strings.Trim(strings.TrimSpace(parsed.Path), "/")
	if prefix == "" {
		logical, err := normalizeTargetLogical(name)
		if err != nil {
			return Target{}, err
		}
		return Target{Kind: TargetStorageShorthand, Scheme: scheme, Logical: logical, Original: original}, nil
	}
	if scheme == "file" {
		return Target{}, errors.New("file repository shorthand must not include a path")
	}
	parts := strings.Split(prefix, "/")
	logical, err := normalizeTargetLogical(parts[len(parts)-1])
	if err != nil {
		return Target{}, err
	}
	return Target{Kind: TargetStorageExplicit, Scheme: scheme, Logical: logical, Bucket: name, Prefix: prefix, Original: original}, nil
}

func normalizeTargetLogical(name string) (string, error) {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".git")
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return "", errors.New("repository target must include a flat repository name")
	}
	return name + ".git", nil
}

func (t Target) Provider() protocol.Provider {
	switch t.Scheme {
	case "file":
		return protocol.ProviderFile
	case "s3":
		return protocol.ProviderS3
	case "gs":
		return protocol.ProviderGCS
	default:
		return ""
	}
}
