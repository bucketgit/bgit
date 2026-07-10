package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	localbroker "github.com/bucketgit/bgit/broker/local"
	internalconfig "github.com/bucketgit/bgit/internal/config"
	"github.com/bucketgit/bgit/protocol"
	transportpkg "github.com/bucketgit/bgit/transport"
)

type remoteHelperSession struct {
	config config
	close  func()
}

func (s *remoteHelperSession) Close() {
	if s != nil && s.close != nil {
		s.close()
	}
}

func remoteHelperCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	resolver := transportpkg.ResolverFunc(func(ctx context.Context, address, service string, input io.Reader, output io.Writer) error {
		session, err := resolveRemoteHelperSession(ctx, address)
		if err != nil {
			return err
		}
		defer session.Close()
		return serveGitServiceWithConfig(ctx, service, session.config, input, output)
	})
	return transportpkg.ServeRemoteHelper(context.Background(), resolver, args, stdin, stdout, stderr)
}

func remoteHelperAddress(args []string) string {
	return transportpkg.RemoteHelperAddress(args)
}

func configForRemoteHelperAddress(address string) (config, error) {
	session, err := resolveRemoteHelperSession(context.Background(), address)
	if err != nil {
		return config{}, err
	}
	defer session.Close()
	return session.config, nil
}

func resolveRemoteHelperSession(ctx context.Context, address string) (*remoteHelperSession, error) {
	address = strings.TrimSpace(address)
	address = strings.TrimPrefix(address, "bgit::")
	if address == "" {
		return nil, errors.New("missing bgit remote helper URL")
	}
	if strings.HasPrefix(address, "bgit://") {
		parsed, err := url.Parse(address)
		if err != nil {
			return nil, err
		}
		repo := strings.Trim(strings.Trim(parsed.Host+"/"+strings.Trim(parsed.Path, "/"), "/"), "/")
		if repo == "" {
			return nil, errors.New("bgit remote helper URL must include a repository name")
		}
		return resolveRemoteHelperLogicalSession(ctx, repo)
	}
	if strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://") {
		cfg, err := configForRemoteHelperBrokerURL(address)
		return &remoteHelperSession{config: cfg}, err
	}
	target, err := localbroker.ParseTarget(address)
	if err != nil {
		return nil, err
	}
	switch target.Kind {
	case localbroker.TargetStorageExplicit:
		cfg, _, err := parseRepoURI(address)
		if err != nil {
			return nil, err
		}
		return &remoteHelperSession{config: mergeSSHRepoAuth(cfg)}, nil
	case localbroker.TargetStorageShorthand:
		return resolveRemoteHelperStorageSession(ctx, target)
	default:
		return resolveRemoteHelperLogicalSession(ctx, target.Logical)
	}
}

func resolveRemoteHelperStorageSession(ctx context.Context, target localbroker.Target) (*remoteHelperSession, error) {
	global, _, err := loadGlobalConfigForInit("")
	if err != nil {
		return nil, err
	}
	profile, region, managed, err := startDefaultLocalBroker(global)
	if err != nil {
		return nil, err
	}
	storageProfile, storageRegion := storageProfileRegionFromOptions(target.Original, "", "")
	repo, err := localBrokerRepoForTarget(config{
		provider:            "local",
		brokerURL:           managed.URL,
		gcloudConfiguration: storageProfile,
		region:              storageRegion,
	}, target.Original, coreTeamID)
	if err != nil {
		managed.Close()
		return nil, err
	}
	server, err := localBrokerServerForURL(managed.URL)
	if err != nil {
		managed.Close()
		return nil, err
	}
	state, err := server.loadRepoForRequest(repo)
	if err != nil || strings.TrimSpace(state.Repo.Logical) == "" {
		managed.Close()
		return nil, fmt.Errorf("BucketGit repository %s was not found in existing %s storage; initialize it before using the remote helper: %w", target.Logical, target.Scheme, firstNonNil(err, os.ErrNotExist))
	}
	cfg := configForRemoteHelperLocalRepository(state.Repo, profile.Name, region.Name, managed.URL)
	return &remoteHelperSession{config: cfg, close: managed.Close}, nil
}

func configForRemoteHelperLocalRepository(repo protocol.Repository, profile, region, brokerURL string) config {
	logical := firstNonEmpty(strings.TrimSpace(repo.Logical), strings.TrimSpace(repo.Prefix))
	return mergeSSHRepoAuth(config{
		provider:            "local",
		bucket:              strings.TrimSpace(repo.Bucket),
		prefix:              strings.Trim(strings.TrimSpace(repo.Prefix), "/"),
		branch:              defaultBranch,
		origin:              fmt.Sprintf("git@%s:%s", defaultSSHHost, logical),
		brokerURL:           brokerURL,
		logicalRepo:         logical,
		teamID:              firstNonEmpty(strings.TrimSpace(repo.TeamID), coreTeamID),
		storageProvider:     strings.TrimSpace(repo.Provider),
		storageProfile:      strings.TrimSpace(repo.Profile),
		storageRegion:       strings.TrimSpace(repo.Region),
		gcloudConfiguration: "local:" + firstNonEmpty(profile, "default") + "/" + firstNonEmpty(region, "default"),
	})
}

func firstNonNil(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return errors.New("repository state is missing")
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
	session, err := resolveRemoteHelperLogicalSession(context.Background(), repo)
	if err != nil {
		return config{}, err
	}
	defer session.Close()
	return session.config, nil
}

func resolveRemoteHelperLogicalSession(ctx context.Context, repo string) (*remoteHelperSession, error) {
	logical, err := normalizeLogicalRepoName(repo)
	if err != nil {
		return nil, err
	}
	if localCfg, err := readLocalConfig("."); err == nil && strings.TrimSpace(localCfg.brokerURL) != "" {
		localCfg.logicalRepo = logical
		localCfg.prefix = logical
		localCfg.origin = fmt.Sprintf("git@%s:%s", defaultSSHHost, logical)
		managed, err := ensureLocalBrokerForCommand(ctx, &localCfg)
		if err != nil {
			return nil, err
		}
		return &remoteHelperSession{config: mergeSSHRepoAuth(localCfg), close: func() { managed.Close() }}, nil
	}
	global, _, err := loadGlobalConfigForInit("")
	if err != nil {
		return nil, err
	}
	var candidates []*remoteHelperSession
	for _, localProfile := range global.LocalProfiles {
		regions := localProfile.Regions
		if len(regions) == 0 {
			regions = []internalconfig.ProfileRegion{{Name: firstNonEmpty(localProfile.Region, "default")}}
		}
		for _, localRegion := range regions {
			managed, manageErr := ensureManagedLocalBroker(ctx, localProfile, localRegion)
			if manageErr != nil {
				return nil, manageErr
			}
			server, serverErr := localBrokerServerForURL(managed.URL)
			if serverErr != nil {
				managed.Close()
				return nil, serverErr
			}
			indexed, ok := server.indexedRepo(logical)
			if !ok {
				managed.Close()
				continue
			}
			state, stateErr := server.loadRepoForRequest(indexed)
			if stateErr != nil {
				managed.Close()
				continue
			}
			candidates = append(candidates, &remoteHelperSession{
				config: configForRemoteHelperLocalRepository(state.Repo, localProfile.Name, localRegion.Name, managed.URL),
				close:  managed.Close,
			})
		}
	}
	for _, known := range global.Repos {
		knownLogical, normalizeErr := normalizeLogicalRepoName(known.Name)
		if normalizeErr != nil || !strings.EqualFold(knownLogical, logical) || strings.TrimSpace(known.BrokerURL) == "" {
			continue
		}
		cfg := mergeSSHRepoAuth(config{
			provider:            "gcs",
			brokerURL:           strings.TrimRight(strings.TrimSpace(known.BrokerURL), "/"),
			logicalRepo:         logical,
			prefix:              logical,
			branch:              defaultBranch,
			origin:              fmt.Sprintf("git@%s:%s", defaultSSHHost, logical),
			gcloudConfiguration: strings.TrimSpace(known.Profile),
		})
		candidates = append(candidates, &remoteHelperSession{config: cfg})
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	for _, candidate := range candidates {
		candidate.Close()
	}
	if len(candidates) > 1 {
		return nil, fmt.Errorf("BucketGit repository %s is ambiguous in BGIT_HOME; use an explicit bgit::gs://, bgit::s3://, bgit::file://, or broker URL", logical)
	}
	return nil, fmt.Errorf("BucketGit repository %s has no checkout or BGIT_HOME mapping; use an explicit bgit::gs://, bgit::s3://, bgit::file://, or broker URL", logical)
}
