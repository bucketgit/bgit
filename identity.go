package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const defaultBucketGitIdentityName = "BucketGit Client"

var identityEmailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

type identityConfig struct {
	Name        string
	Email       string
	UsesDefault bool
}

func defaultBucketGitIdentityEmail() string {
	username := firstNonEmpty(os.Getenv("USER"), os.Getenv("USERNAME"), "username")
	username = strings.ToLower(strings.TrimSpace(username))
	var clean strings.Builder
	for _, r := range username {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			clean.WriteRune(r)
		}
	}
	if clean.Len() == 0 {
		return "username@bucketgit.com"
	}
	return clean.String() + "@bucketgit.com"
}

func readGlobalIdentity() globalIdentityConfig {
	path, err := defaultGlobalConfigPath()
	if err != nil {
		return globalIdentityConfig{}
	}
	cfg, err := readGlobalConfig(path)
	if err != nil {
		return globalIdentityConfig{}
	}
	return cfg.Identity
}

func effectiveRepositoryIdentity(repo *localRepository) identityConfig {
	name := firstNonEmpty(os.Getenv("GIT_AUTHOR_NAME"), repo.configValue("user.name"))
	email := firstNonEmpty(os.Getenv("GIT_AUTHOR_EMAIL"), repo.configValue("user.email"))
	if name == "" || email == "" {
		global := readGlobalIdentity()
		name = firstNonEmpty(name, global.Name)
		email = firstNonEmpty(email, global.Email)
	}
	defaultEmail := defaultBucketGitIdentityEmail()
	name = firstNonEmpty(name, defaultBucketGitIdentityName)
	email = firstNonEmpty(email, defaultEmail)
	return identityConfig{
		Name:        name,
		Email:       email,
		UsesDefault: name == defaultBucketGitIdentityName || email == defaultEmail,
	}
}

func (r *localRepository) identityValue(key string) string {
	if value := r.configValue(key); value != "" {
		return value
	}
	global := readGlobalIdentity()
	switch key {
	case "user.name":
		return global.Name
	case "user.email":
		return global.Email
	default:
		return ""
	}
}

func maybeConfigureIdentityBeforePush(stdin io.Reader, stdout io.Writer) error {
	repo, err := openLocalRepository(".")
	if err != nil {
		return nil
	}
	identity := effectiveRepositoryIdentity(repo)
	if !identity.UsesDefault {
		return nil
	}
	fmt.Fprintf(stdout, "BucketGit is using the default identity %s <%s>.\n", identity.Name, identity.Email)
	fmt.Fprintln(stdout, "You have not configured your name and email address yet.")
	fmt.Fprintf(stdout, "Configure a global BucketGit identity now? [Y/n] ")
	reader := bufio.NewReader(stdin)
	answer, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if errors.Is(err, io.EOF) && answer == "" {
		fmt.Fprintf(stdout, "Continuing as %s <%s>.\n", identity.Name, identity.Email)
		return nil
	}
	if answer == "n" || answer == "no" {
		fmt.Fprintf(stdout, "Continuing as %s <%s>.\n", identity.Name, identity.Email)
		return nil
	}
	name, email, err := readIdentityFields(reader, stdout, "", "")
	if err != nil {
		return err
	}
	if name == "" || email == "" {
		fmt.Fprintf(stdout, "Continuing as %s <%s>.\n", identity.Name, identity.Email)
		return nil
	}
	return writeGlobalIdentity(name, email)
}

func readIdentityFields(reader *bufio.Reader, stdout io.Writer, currentName, currentEmail string) (string, string, error) {
	fmt.Fprintf(stdout, "Name")
	if currentName != "" {
		fmt.Fprintf(stdout, " [%s]", currentName)
	}
	fmt.Fprint(stdout, ": ")
	name, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(currentName)
	}
	fmt.Fprintf(stdout, "Email")
	if currentEmail != "" {
		fmt.Fprintf(stdout, " [%s]", currentEmail)
	}
	fmt.Fprint(stdout, ": ")
	email, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	email = strings.TrimSpace(email)
	if email == "" {
		email = strings.TrimSpace(currentEmail)
	}
	if email != "" && !identityEmailPattern.MatchString(email) {
		return "", "", fmt.Errorf("email address %q looks invalid", email)
	}
	return name, email, nil
}

func writeGlobalIdentity(name, email string) error {
	path, err := defaultGlobalConfigPath()
	if err != nil {
		return err
	}
	cfg, err := readGlobalConfig(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		cfg = globalConfig{Version: globalConfigVersion}
	}
	cfg.Identity = globalIdentityConfig{Name: strings.TrimSpace(name), Email: strings.TrimSpace(email)}
	return writeGlobalConfig(path, cfg)
}
