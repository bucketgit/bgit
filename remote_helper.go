package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

func remoteHelperCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	address := remoteHelperAddress(args)
	if address == "" {
		return errors.New("usage: git-remote-bgit <repository> [<url>]")
	}
	br := bufio.NewReader(stdin)
	bw := bufio.NewWriter(stdout)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			return nil
		case line == "capabilities":
			fmt.Fprintln(bw, "connect")
			fmt.Fprintln(bw)
			if err := bw.Flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "connect "):
			service := strings.TrimSpace(strings.TrimPrefix(line, "connect "))
			if service != gitUploadPackService && service != gitReceivePackService {
				return fmt.Errorf("unsupported git remote helper service %q", service)
			}
			cfg, err := configForRemoteHelperAddress(address)
			if err != nil {
				return err
			}
			fmt.Fprintln(bw)
			if err := bw.Flush(); err != nil {
				return err
			}
			return serveGitServiceWithConfig(context.Background(), service, cfg, br, stdout)
		case strings.HasPrefix(line, "option "):
			fmt.Fprintln(bw, "unsupported")
			if err := bw.Flush(); err != nil {
				return err
			}
		default:
			fmt.Fprintf(stderr, "unsupported git remote helper command %q\n", line)
			return fmt.Errorf("unsupported git remote helper command %q", line)
		}
	}
}

func remoteHelperAddress(args []string) string {
	if len(args) >= 2 {
		return strings.TrimSpace(args[1])
	}
	if len(args) == 1 {
		return strings.TrimSpace(args[0])
	}
	return ""
}

func configForRemoteHelperAddress(address string) (config, error) {
	address = strings.TrimSpace(address)
	address = strings.TrimPrefix(address, "bgit::")
	if address == "" {
		return config{}, errors.New("missing bgit remote helper URL")
	}
	if strings.HasPrefix(address, "bgit://") {
		parsed, err := url.Parse(address)
		if err != nil {
			return config{}, err
		}
		repo := strings.Trim(strings.Trim(parsed.Host+"/"+strings.Trim(parsed.Path, "/"), "/"), "/")
		if repo == "" {
			return config{}, errors.New("bgit remote helper URL must include a repository name")
		}
		return configForRemoteHelperLogicalRepo(repo)
	}
	if strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://") {
		return configForRemoteHelperBrokerURL(address)
	}
	if strings.HasPrefix(address, "s3://") || strings.HasPrefix(address, "gs://") || strings.HasPrefix(address, "gcs://") {
		cfg, _, err := parseRepoURI(address)
		if err != nil {
			return config{}, err
		}
		return mergeSSHRepoAuth(cfg), nil
	}
	return configForRemoteHelperLogicalRepo(address)
}

func configForRemoteHelperBrokerURL(raw string) (config, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return config{}, err
	}
	repoPart := strings.Trim(parsed.Path, "/")
	if repoPart == "" {
		return config{}, errors.New("bgit broker URL must include a repository name")
	}
	parts := strings.Split(repoPart, "/")
	logical, err := normalizeLogicalRepoName(parts[len(parts)-1])
	if err != nil {
		return config{}, err
	}
	parsed.Path = strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/"), "/"+parts[len(parts)-1])
	parsed.RawQuery = ""
	parsed.Fragment = ""
	brokerURL := strings.TrimRight(parsed.String(), "/")
	cfg := config{
		provider:    "gcs",
		brokerURL:   brokerURL,
		logicalRepo: logical,
		prefix:      logical,
		branch:      defaultBranch,
		origin:      fmt.Sprintf("git@%s:%s", defaultSSHHost, logical),
	}
	return mergeSSHRepoAuth(cfg), nil
}

func configForRemoteHelperLogicalRepo(repo string) (config, error) {
	logical, err := normalizeLogicalRepoName(repo)
	if err != nil {
		return config{}, err
	}
	if localCfg, err := readLocalConfig("."); err == nil && strings.TrimSpace(localCfg.brokerURL) != "" {
		localCfg.logicalRepo = logical
		localCfg.prefix = logical
		localCfg.origin = fmt.Sprintf("git@%s:%s", defaultSSHHost, logical)
		return mergeSSHRepoAuth(localCfg), nil
	}
	return mergeSSHRepoAuth(config{
		provider:    "gcs",
		logicalRepo: logical,
		prefix:      logical,
		branch:      defaultBranch,
		origin:      fmt.Sprintf("git@%s:%s", defaultSSHHost, logical),
	}), nil
}
