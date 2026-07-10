package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	localbroker "github.com/bucketgit/bgit/broker/local"
	"github.com/bucketgit/bgit/protocol"
	"github.com/bucketgit/bgit/store"
)

type localBrokerServer struct {
	root       string
	objectRoot string
	baseURL    string
	coreMu     sync.Mutex
	core       *localbroker.Broker
}

type localBrokerStoreAdapter struct{ store writableGitRemoteStore }

func (a localBrokerStoreAdapter) Read(ctx context.Context, path string) ([]byte, error) {
	return a.store.read(ctx, path)
}
func (a localBrokerStoreAdapter) List(ctx context.Context, prefix string) ([]string, error) {
	return a.store.list(ctx, prefix)
}
func (a localBrokerStoreAdapter) Write(ctx context.Context, path string, data []byte) error {
	return a.store.write(ctx, path, data)
}
func (a localBrokerStoreAdapter) Delete(ctx context.Context, path string) error {
	return a.store.delete(ctx, path)
}
func (a localBrokerStoreAdapter) ListRefs(ctx context.Context) (map[string]string, error) {
	refs, ok := a.store.(refListingGitRemoteStore)
	if !ok {
		return nil, store.ErrNotSupported
	}
	return refs.listRefs(ctx)
}
func (a localBrokerStoreAdapter) CompareAndSwapRef(ctx context.Context, ref, oldOID, newOID string) error {
	refs, ok := a.store.(refCASGitRemoteStore)
	if !ok {
		return store.ErrNotSupported
	}
	return refs.compareAndSwapRef(ctx, ref, oldOID, newOID)
}
func (a localBrokerStoreAdapter) CompareAndSwap(ctx context.Context, path string, expected store.ObjectState, replacement []byte) error {
	objects, ok := a.store.(objectCASGitRemoteStore)
	if !ok {
		return store.ErrNotSupported
	}
	return objects.compareAndSwap(ctx, path, expected, replacement)
}

var _ store.Writer = localBrokerStoreAdapter{}
var _ store.RefStore = localBrokerStoreAdapter{}
var _ store.CompareAndSwapper = localBrokerStoreAdapter{}

func (s *localBrokerServer) localCore() (*localbroker.Broker, error) {
	s.coreMu.Lock()
	defer s.coreMu.Unlock()
	if s.core != nil {
		return s.core, nil
	}
	core, err := localbroker.New(localbroker.Options{
		Root: s.root,
		Resolver: localbroker.ResolverFunc(func(ctx context.Context, repo protocol.Repository) (store.Writer, func() error, bool, error) {
			backend, closeBackend, ok, err := s.cloudStore(ctx, repo)
			if !ok || err != nil {
				return nil, nil, ok, err
			}
			closeFn := func() error {
				if closeBackend != nil {
					closeBackend()
				}
				return nil
			}
			return localBrokerStoreAdapter{store: backend}, closeFn, true, nil
		}),
	})
	if err != nil {
		return nil, err
	}
	s.core = core
	return core, nil
}

func isLocalBrokerURL(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), "local://")
}

func localBrokerPostContext(ctx context.Context, brokerURL, path string, req any, resp any) error {
	server, err := localBrokerServerForURL(brokerURL)
	if err != nil {
		return err
	}
	out, status, err := server.localPostContext(ctx, path, req)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = http.StatusText(status)
		}
		return brokerHTTPError(path, msg)
	}
	if resp != nil && len(out) > 0 {
		if err := json.Unmarshal(out, resp); err != nil {
			return err
		}
	}
	return nil
}

func localBrokerCapabilityRead(ctx context.Context, capability protocol.ObjectCapabilityResponse) ([]byte, error) {
	_ = ctx
	server, repo, err := localBrokerCapabilityTarget(capability)
	if err != nil {
		return nil, err
	}
	return server.readObject(repo, capability.Object)
}

func localBrokerCapabilityWrite(ctx context.Context, capability protocol.ObjectCapabilityResponse, data []byte) error {
	_ = ctx
	server, repo, err := localBrokerCapabilityTarget(capability)
	if err != nil {
		return err
	}
	return server.writeObject(repo, capability.Object, data)
}

func localBrokerCapabilityDelete(ctx context.Context, capability protocol.ObjectCapabilityResponse) error {
	_ = ctx
	server, repo, err := localBrokerCapabilityTarget(capability)
	if err != nil {
		return err
	}
	return server.deleteObject(repo, capability.Object)
}

func localBrokerCapabilityTarget(capability protocol.ObjectCapabilityResponse) (*localBrokerServer, protocol.Repository, error) {
	server, err := localBrokerServerForURL(capability.URL)
	if err != nil {
		return nil, protocol.Repository{}, err
	}
	repo := protocol.Repository{Provider: capability.Provider, Bucket: capability.Bucket, Prefix: capability.Prefix, Profile: capability.Profile, Region: capability.Region}
	return server, repo, nil
}

func localBrokerServerForURL(brokerURL string) (*localBrokerServer, error) {
	profileName, regionName := localProfileSelection(strings.TrimPrefix(strings.TrimSpace(brokerURL), "local://"))
	path, err := defaultGlobalConfigPath()
	if err != nil {
		return nil, err
	}
	global, err := readGlobalConfig(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	profile, _, ok := globalLocalProfileForSelection(global, profileName, regionName)
	if !ok {
		profile = ensureDefaultLocalProfile(global)
	}
	root := expandHome(profile.Root)
	if strings.TrimSpace(root) == "" {
		root, err = defaultLocalBrokerRoot()
		if err != nil {
			return nil, err
		}
	}
	server := &localBrokerServer{root: root, objectRoot: filepath.Join(root, "objects"), baseURL: localBrokerURL(profileName, regionName)}
	if err := os.MkdirAll(server.objectRoot, 0o755); err != nil {
		return nil, err
	}
	return server, nil
}

func (s *localBrokerServer) localPost(path string, req any) ([]byte, int, error) {
	return s.localPostContext(context.Background(), path, req)
}

func (s *localBrokerServer) localPostContext(ctx context.Context, path string, req any) ([]byte, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 499, err
	}
	switch path {
	case "/owners/upsert":
		r := req.(protocol.OwnerRequest)
		core, err := s.localCore()
		if err != nil {
			return nil, 500, err
		}
		if _, err := core.UpsertOwners(ctx, r); err != nil {
			return nil, 500, err
		}
		return mustJSON(map[string]bool{"ok": true}), 200, nil
	case "/teams/create":
		return mustJSON(map[string]bool{"ok": true}), 200, nil
	case "/teams/list":
		return mustJSON(map[string]any{"teams": []protocol.Team{{ID: coreTeamID, Name: coreTeamName}}}), 200, nil
	case "/teams/resolve":
		return mustJSON(map[string]any{"team": protocol.Team{ID: coreTeamID, Name: coreTeamName}}), 200, nil
	case "/repos/create":
		r := req.(protocol.RepositoryAdminRequest)
		repo := s.physicalRepo(r.Repo)
		if err := s.ensureRepoStorage(ctx, repo); err != nil {
			return nil, 500, err
		}
		var keys []protocol.Key
		for _, key := range r.PublicKeys {
			if key = normalizeKey(key); key != "" {
				keys = append(keys, protocol.Key{User: firstNonEmpty(r.User, "owner"), Role: firstNonEmpty(r.Role, "developer"), PublicKey: key})
			}
		}
		if len(keys) == 0 {
			owners, _ := s.loadOwners()
			for _, key := range owners.Keys {
				keys = append(keys, protocol.Key{User: key.User, Role: firstNonEmpty(r.Role, "developer"), PublicKey: key.PublicKey})
			}
		}
		core, err := s.localCore()
		if err != nil {
			return nil, 500, err
		}
		r.Repo = repo
		state, err := core.CreateRepository(ctx, r, keys)
		if err != nil {
			if errors.Is(err, protocol.ErrConflict) {
				return mustJSON(map[string]string{"error": "repository already exists"}), http.StatusConflict, nil
			}
			return nil, 500, err
		}
		return mustJSON(map[string]any{"ok": true, "repo": state.Repo}), 200, nil
	case "/repos/get":
		r := req.(protocol.RepositoryRequest)
		core, err := s.localCore()
		if err != nil {
			return nil, 500, err
		}
		state, err := core.GetRepository(ctx, s.physicalRepo(r.Repo))
		if err != nil {
			return mustJSON(map[string]string{"error": "repository not found"}), http.StatusNotFound, nil
		}
		return mustJSON(map[string]any{"ok": true, "repo": state.Repo, "teams": state.Teams}), 200, nil
	case "/repos/list":
		core, err := s.localCore()
		if err != nil {
			return nil, 500, err
		}
		repos, err := core.ListRepositories(ctx)
		if err != nil {
			return nil, 500, err
		}
		return mustJSON(map[string]any{"repos": repos}), 200, nil
	case "/auth/status":
		r := req.(protocol.AuthStatusRequest)
		state, _ := s.loadRepoForRequest(r.Repo)
		return mustJSON(protocol.AuthStatus{BrokerURL: s.baseURL, BrokerVersion: brokerVersion(), Repo: state.Repo, User: "owner", Role: "owner", Capabilities: roleCapabilitiesForLocal("owner"), ResolvedAt: time.Now().UTC().Format(time.RFC3339)}), 200, nil
	case "/auth/check":
		return mustJSON(protocol.AuthResponse{Allowed: true, User: "owner", Role: "owner"}), 200, nil
	case "/objects/capability":
		r := req.(protocol.ObjectCapabilityRequest)
		state, err := s.loadRepoForRequest(r.Repo)
		if err != nil {
			return mustJSON(map[string]string{"error": "repository not found"}), http.StatusNotFound, nil
		}
		objectPath, err := validateLocalBrokerCapabilityPath(r.Operation, r.Path)
		if err != nil {
			return mustJSON(map[string]string{"error": err.Error()}), http.StatusForbidden, nil
		}
		object := localBrokerObjectName(state.Repo, objectPath)
		return mustJSON(protocol.ObjectCapabilityResponse{Provider: state.Repo.Provider, Mode: "local", URL: s.baseURL, Bucket: state.Repo.Bucket, Prefix: state.Repo.Prefix, Object: object, Profile: state.Repo.Profile, Region: state.Repo.Region}), 200, nil
	case "/objects/read":
		r := req.(protocol.ObjectRequest)
		state, err := s.loadRepoForRequest(r.Repo)
		if err != nil {
			return mustJSON(map[string]string{"error": "repository not found"}), http.StatusNotFound, nil
		}
		data, err := s.readObject(state.Repo, r.Path)
		if err != nil {
			return mustJSON(map[string]string{"error": err.Error()}), http.StatusNotFound, nil
		}
		return mustJSON(protocol.ObjectResponse{Data: base64.StdEncoding.EncodeToString(data)}), 200, nil
	case "/objects/list":
		r := req.(protocol.ObjectRequest)
		state, err := s.loadRepoForRequest(r.Repo)
		if err != nil {
			return mustJSON(map[string]string{"error": "repository not found"}), http.StatusNotFound, nil
		}
		paths, err := s.listObjects(state.Repo, r.Prefix)
		if err != nil {
			return nil, 500, err
		}
		return mustJSON(protocol.ObjectResponse{Paths: paths}), 200, nil
	case "/issues/list", "/issues/view", "/issues/create", "/issues/update", "/issues/comment", "/issues/close", "/issues/reopen", "/issues/move", "/issues/take", "/issues/assign", "/issues/archive", "/issues/reorder":
		r := req.(protocol.IssueRequest)
		state, err := s.loadRepoForRequest(r.Repo)
		if err != nil {
			return mustJSON(map[string]string{"error": "repository not found"}), http.StatusNotFound, nil
		}
		return s.localIssueServiceEndpoint(ctx, path, r, state, "owner")
	case "/refs/update":
		r := req.(protocol.RefUpdateRequest)
		state, err := s.loadRepoForRequest(r.Repo)
		if err != nil {
			return mustJSON(map[string]string{"error": "repository not found"}), http.StatusNotFound, nil
		}
		r.Repo = state.Repo
		core, err := s.localCore()
		if err != nil {
			return nil, 500, err
		}
		if err := core.UpdateRef(ctx, r, "owner"); err != nil {
			status := 500
			if errors.Is(err, store.ErrConflict) {
				status = http.StatusConflict
			}
			return mustJSON(map[string]string{"error": err.Error()}), status, nil
		}
		return mustJSON(map[string]bool{"ok": true}), 200, nil
	case "/refs/list":
		r := req.(protocol.RefsRequest)
		state, err := s.loadRepoForRequest(r.Repo)
		if err != nil {
			return mustJSON(map[string]string{"error": "repository not found"}), http.StatusNotFound, nil
		}
		core, err := s.localCore()
		if err != nil {
			return nil, 500, err
		}
		reconciled, err := core.ReconcileRepository(ctx, state.Repo, "local-sync")
		if err != nil {
			return nil, 500, err
		}
		return mustJSON(protocol.RefsResponse{Refs: reconciled.Refs}), 200, nil
	default:
		return mustJSON(map[string]string{"error": "unknown broker endpoint"}), http.StatusNotFound, nil
	}
}

func (s *localBrokerServer) localIssueServiceEndpoint(ctx context.Context, path string, req protocol.IssueRequest, state localbroker.RepositoryState, user string) ([]byte, int, error) {
	core, err := s.localCore()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	switch path {
	case "/issues/list":
		issues, err := core.ListIssues(ctx, state.Repo, localbroker.IssueFilter{Type: req.Type, IncludeArchived: req.IncludeArchived})
		if err != nil {
			return nil, localIssueErrorStatus(err), err
		}
		return mustJSON(map[string]any{"issues": issues}), http.StatusOK, nil
	case "/issues/view":
		issue, err := core.GetIssue(ctx, state.Repo, req.ID)
		if err != nil {
			return nil, localIssueErrorStatus(err), err
		}
		return mustJSON(map[string]any{"issue": issue}), http.StatusOK, nil
	case "/issues/create":
		issue, err := core.CreateIssue(ctx, state.Repo, req, user)
		if err != nil {
			return nil, localIssueErrorStatus(err), err
		}
		return mustJSON(map[string]any{"issue": issue}), http.StatusOK, nil
	case "/issues/update":
		err = core.UpdateIssue(ctx, state.Repo, req, user)
	case "/issues/comment":
		err = core.CommentIssue(ctx, state.Repo, req.ID, req.Comment, user)
	case "/issues/close":
		err = core.SetIssueClosed(ctx, state.Repo, req.ID, true, user)
	case "/issues/reopen":
		err = core.SetIssueClosed(ctx, state.Repo, req.ID, false, user)
	case "/issues/reorder":
		err = core.MoveStory(ctx, state.Repo, req.ID, localbroker.StoryMove{Lane: req.Lane, AfterID: req.AfterID, Order: req.Order}, user)
	case "/issues/move":
		if req.AfterID != nil {
			err = core.MoveStory(ctx, state.Repo, req.ID, localbroker.StoryMove{Lane: req.Lane, AfterID: req.AfterID}, user)
		} else {
			err = core.MoveStoryToLaneEnd(ctx, state.Repo, req.ID, req.Lane, user)
		}
	case "/issues/take":
		err = core.TakeStory(ctx, state.Repo, req.ID, user)
	case "/issues/assign":
		err = core.AssignIssue(ctx, state.Repo, req.ID, req.Assignee, user)
	case "/issues/archive":
		err = core.ArchiveIssue(ctx, state.Repo, req.ID, req.Archived, user)
	default:
		return mustJSON(map[string]string{"error": "unknown broker endpoint"}), http.StatusNotFound, nil
	}
	if err != nil {
		return nil, localIssueErrorStatus(err), err
	}
	return mustJSON(map[string]bool{"ok": true}), http.StatusOK, nil
}

func localIssueErrorStatus(err error) int {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "not found") {
		return http.StatusNotFound
	}
	if strings.Contains(message, "required") || strings.Contains(message, "unknown board lane") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (s *localBrokerServer) physicalRepo(repo protocol.Repository) protocol.Repository {
	if repo.Logical == "" {
		repo.Logical = firstNonEmpty(repo.Prefix, "repo.git")
	}
	if logical, err := normalizeLogicalRepoName(repo.Logical); err == nil {
		repo.Logical = logical
	}
	repo.TeamID = firstNonEmpty(repo.TeamID, coreTeamID)
	repo.TeamName = firstNonEmpty(repo.TeamName, coreTeamName)
	if repo.Bucket == "" {
		repo.Bucket = repo.Logical
		repo.Provider = "file"
		repo.Prefix = ""
		return repo
	}
	if strings.HasPrefix(repo.Bucket, "s3://") || strings.HasPrefix(repo.Bucket, "gs://") {
		if cfg, ok, err := localBrokerCloudConfig(repo.Bucket); err == nil && ok {
			repo.Bucket = cfg.bucket
			repo.Provider = cfg.provider
			repo.Profile = cfg.gcloudConfiguration
			repo.Region = cfg.region
		}
		repo.Prefix = ""
		return repo
	}
	if strings.HasPrefix(repo.Bucket, "file://") {
		repo.Provider = "file"
		repo.Prefix = ""
		return repo
	}
	if repo.Provider == "s3" || repo.Provider == "gcs" {
		repo.Prefix = ""
		return repo
	}
	if repo.Prefix == "" && repo.Provider == "" {
		repo.Provider = "file"
	}
	return repo
}

func (s *localBrokerServer) loadRepo(repo protocol.Repository) (localbroker.RepositoryState, error) {
	core, err := s.localCore()
	if err != nil {
		return localbroker.RepositoryState{}, err
	}
	state, err := core.LoadRepositoryState(context.Background(), repo)
	if err == nil {
		state.Repo = s.physicalRepo(state.Repo)
	}
	return state, err
}

func (s *localBrokerServer) loadRepoForRequest(repo protocol.Repository) (localbroker.RepositoryState, error) {
	physical := s.physicalRepo(repo)
	state, err := s.loadRepo(physical)
	if err == nil {
		return state, nil
	}
	logical := strings.TrimSpace(physical.Logical)
	if logical == "" {
		return localbroker.RepositoryState{}, err
	}
	if indexed, ok := s.indexedRepo(logical); ok {
		state, indexErr := s.loadRepo(indexed)
		if indexErr == nil && strings.EqualFold(strings.TrimSpace(state.Repo.Logical), logical) {
			return state, nil
		}
	}
	var found localbroker.RepositoryState
	walkErr := filepath.WalkDir(s.objectRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || filepath.Base(path) != "repo.json" {
			return nil
		}
		var candidate localbroker.RepositoryState
		if readErr := s.readJSON(path, &candidate); readErr != nil {
			return nil
		}
		if strings.EqualFold(strings.TrimSpace(candidate.Repo.Logical), logical) {
			found = candidate
			return fs.SkipAll
		}
		return nil
	})
	if found.Repo.Logical != "" {
		return found, nil
	}
	if walkErr != nil {
		return localbroker.RepositoryState{}, walkErr
	}
	return localbroker.RepositoryState{}, err
}

func (s *localBrokerServer) saveRepo(state localbroker.RepositoryState) error {
	core, err := s.localCore()
	if err != nil {
		return err
	}
	return core.SaveRepositoryState(context.Background(), state)
}

func (s *localBrokerServer) indexedRepo(logical string) (protocol.Repository, bool) {
	logical = strings.TrimSpace(logical)
	if logical == "" {
		return protocol.Repository{}, false
	}
	core, err := s.localCore()
	if err != nil {
		return protocol.Repository{}, false
	}
	repo, ok, _ := core.FindRepository(context.Background(), logical)
	return repo, ok
}

func (s *localBrokerServer) bucketDir(bucket string) string {
	core, err := s.localCore()
	if err != nil {
		return filepath.Join(s.objectRoot, base64.RawURLEncoding.EncodeToString([]byte(bucket)))
	}
	return core.BucketDir(bucket)
}

func (s *localBrokerServer) ensureRepoStorage(ctx context.Context, repo protocol.Repository) error {
	cfg, ok, err := localBrokerRepoCloudConfig(repo)
	if err != nil || !ok {
		return err
	}
	return ensureBucket(ctx, cfg)
}

func (s *localBrokerServer) cloudStore(ctx context.Context, repo protocol.Repository) (writableGitRemoteStore, func(), bool, error) {
	cfg, ok, err := localBrokerRepoCloudConfig(repo)
	if err != nil || !ok {
		return nil, nil, ok, err
	}
	switch cfg.provider {
	case "s3":
		client, err := newS3Client(ctx, cfg, false)
		if err != nil {
			return nil, nil, true, err
		}
		return &s3GitStore{client: client, bucket: cfg.bucket, prefix: cfg.prefix}, nil, true, nil
	case "gcs":
		client, err := newStorageClient(ctx, cfg)
		if err != nil {
			return nil, nil, true, err
		}
		return &gcsGitStore{client: client, bucket: cfg.bucket, prefix: cfg.prefix}, func() { _ = client.Close() }, true, nil
	default:
		return nil, nil, false, nil
	}
}

func localBrokerRepoCloudConfig(repo protocol.Repository) (config, bool, error) {
	if cfg, ok, err := localBrokerCloudConfig(repo.Bucket); ok || err != nil {
		return cfg, ok, err
	}
	provider := strings.TrimSpace(repo.Provider)
	if provider != "s3" && provider != "gcs" {
		return config{}, false, nil
	}
	region := strings.TrimSpace(repo.Region)
	profile := strings.TrimSpace(repo.Profile)
	if provider == "s3" {
		region = firstNonEmpty(region, "us-east-1")
	} else {
		region = firstNonEmpty(region, "us-central1")
	}
	profile = firstNonEmpty(profile, "default")
	return config{
		provider:            provider,
		bucket:              strings.TrimSpace(repo.Bucket),
		prefix:              "",
		region:              region,
		auth:                defaultStorageAuthMode(),
		gcloudConfiguration: profile,
	}, true, nil
}

func localBrokerCloudConfig(bucketURI string) (config, bool, error) {
	scheme := ""
	rest := ""
	switch {
	case strings.HasPrefix(bucketURI, "s3://"):
		scheme = "s3"
		rest = strings.TrimPrefix(bucketURI, "s3://")
	case strings.HasPrefix(bucketURI, "gs://"):
		scheme = "gs"
		rest = strings.TrimPrefix(bucketURI, "gs://")
	default:
		return config{}, false, nil
	}
	host := strings.Split(rest, "/")[0]
	if strings.TrimSpace(host) == "" {
		return config{}, true, errors.New("local broker cloud repo URI must include a bucket name")
	}
	labels := strings.Split(host, ".")
	clean := labels[:0]
	for _, label := range labels {
		if strings.TrimSpace(label) != "" {
			clean = append(clean, strings.TrimSpace(label))
		}
	}
	if len(clean) == 0 {
		return config{}, true, errors.New("local broker cloud repo URI must include a bucket name")
	}
	profile := "default"
	region := "us-east-1"
	provider := "s3"
	if scheme == "gs" {
		provider = "gcs"
		region = "us-central1"
	}
	bucket := strings.Join(clean, ".")
	if len(clean) >= 3 && localBrokerLooksCloudRegion(scheme, clean[1]) {
		profile = clean[0]
		region = clean[1]
		bucket = strings.Join(clean[2:], ".")
	} else if len(clean) >= 2 {
		profile = clean[0]
		bucket = strings.Join(clean[1:], ".")
	}
	if strings.TrimSpace(bucket) == "" {
		return config{}, true, errors.New("local broker cloud repo URI must include a bucket name")
	}
	return config{
		provider:            provider,
		bucket:              bucket,
		prefix:              "",
		region:              region,
		auth:                defaultStorageAuthMode(),
		gcloudConfiguration: profile,
	}, true, nil
}

func localBrokerLooksCloudRegion(scheme, value string) bool {
	if setupAWSRegionPattern.MatchString(value) || looksLikeGCPRegion(value) {
		return true
	}
	if scheme != "gs" {
		return false
	}
	if !strings.Contains(value, "-") {
		return false
	}
	for i := len(value) - 1; i >= 0; i-- {
		if value[i] >= '0' && value[i] <= '9' {
			return true
		}
	}
	return false
}

func (s *localBrokerServer) readObject(repo protocol.Repository, object string) ([]byte, error) {
	core, err := s.localCore()
	if err != nil {
		return nil, err
	}
	return core.ReadObject(context.Background(), repo, object)
}

func (s *localBrokerServer) writeObject(repo protocol.Repository, object string, data []byte) error {
	core, err := s.localCore()
	if err != nil {
		return err
	}
	return core.WriteObject(context.Background(), repo, object, data)
}

func (s *localBrokerServer) deleteObject(repo protocol.Repository, object string) error {
	core, err := s.localCore()
	if err != nil {
		return err
	}
	return core.DeleteObject(context.Background(), repo, object)
}

func (s *localBrokerServer) listObjects(repo protocol.Repository, prefix string) ([]string, error) {
	core, err := s.localCore()
	if err != nil {
		return nil, err
	}
	return core.ListObjects(context.Background(), repo, prefix)
}

func (s *localBrokerServer) loadOwners() (localbroker.Owners, error) {
	core, err := s.localCore()
	if err != nil {
		return localbroker.Owners{}, err
	}
	return core.LoadOwners(context.Background())
}

func (s *localBrokerServer) readJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func roleAllowsLocal(role, operation string) bool {
	role = normalizeBrokerRole(role)
	if role == "owner" || role == "admin" {
		return true
	}
	if operation == "read" {
		return role == "read" || role == "triage" || role == "developer" || role == "maintainer"
	}
	if operation == "write" {
		return role == "developer" || role == "maintainer"
	}
	return false
}

func roleCapabilitiesForLocal(role string) map[string]bool {
	return map[string]bool{
		"read":  roleAllowsLocal(role, "read"),
		"push":  roleAllowsLocal(role, "write"),
		"merge": role == "maintainer" || role == "admin" || role == "owner",
	}
}

func validateLocalBrokerCapabilityPath(operation, objectPath string) (string, error) {
	path := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+objectPath)), "/")
	if path == "" || strings.Contains(path, "..") {
		return "", errors.New("invalid object path")
	}
	if operation == "write" && !(path == "objects" || strings.HasPrefix(path, "objects/")) {
		return "", errors.New("write capabilities are restricted to git object paths")
	}
	if operation == "read" {
		if path == "HEAD" || path == "packed-refs" || strings.HasPrefix(path, "refs/") || path == "objects" || strings.HasPrefix(path, "objects/") {
			return path, nil
		}
		return "", errors.New("read capabilities are restricted to git repository paths")
	}
	if operation == "delete" {
		return "", errors.New("delete capabilities are not supported")
	}
	return path, nil
}

func localBrokerObjectName(repo protocol.Repository, objectPath string) string {
	prefix := strings.Trim(repo.Prefix, "/")
	if prefix == "" {
		return objectPath
	}
	return prefix + "/" + strings.TrimPrefix(objectPath, "/")
}

func localBrokerIssuesPath() string {
	return ".bucketgit/broker-state/v1/issues.json"
}

func mustJSON(value any) []byte {
	data, _ := json.MarshalIndent(value, "", "  ")
	return data
}

func normalizeKey(key string) string {
	key = strings.TrimSpace(key)
	parts := strings.Fields(key)
	if len(parts) >= 2 {
		return parts[0] + " " + parts[1]
	}
	return key
}
