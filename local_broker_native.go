package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type localBrokerServer struct {
	root          string
	objectRoot    string
	baseURL       string
	bootstrapHash string
	mu            sync.Mutex
}

type localBrokerRepoState struct {
	Repo      brokerRepo            `json:"repo"`
	Keys      []brokerKey           `json:"keys"`
	Refs      map[string]string     `json:"refs,omitempty"`
	Teams     []brokerRepoTeamGrant `json:"teams,omitempty"`
	CreatedAt string                `json:"created_at,omitempty"`
	UpdatedAt string                `json:"updated_at,omitempty"`
}

type localBrokerOwners struct {
	Keys []brokerKey `json:"keys"`
}

type localBrokerRepoIndex struct {
	Repos []brokerRepo `json:"repos"`
}

func isLocalBrokerURL(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), "local://")
}

func localBrokerPostContext(ctx context.Context, brokerURL, path string, req any, resp any) error {
	_ = ctx
	server, err := localBrokerServerForURL(brokerURL)
	if err != nil {
		return err
	}
	out, status, err := server.localPost(path, req)
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

func localBrokerCapabilityRead(ctx context.Context, capability brokerObjectCapabilityResponse) ([]byte, error) {
	_ = ctx
	server, repo, err := localBrokerCapabilityTarget(capability)
	if err != nil {
		return nil, err
	}
	return server.readObject(repo, capability.Object)
}

func localBrokerCapabilityWrite(ctx context.Context, capability brokerObjectCapabilityResponse, data []byte) error {
	_ = ctx
	server, repo, err := localBrokerCapabilityTarget(capability)
	if err != nil {
		return err
	}
	return server.writeObject(repo, capability.Object, data)
}

func localBrokerCapabilityDelete(ctx context.Context, capability brokerObjectCapabilityResponse) error {
	_ = ctx
	server, repo, err := localBrokerCapabilityTarget(capability)
	if err != nil {
		return err
	}
	return server.deleteObject(repo, capability.Object)
}

func localBrokerCapabilityTarget(capability brokerObjectCapabilityResponse) (*localBrokerServer, brokerRepo, error) {
	server, err := localBrokerServerForURL(capability.URL)
	if err != nil {
		return nil, brokerRepo{}, err
	}
	repo := brokerRepo{Provider: capability.Provider, Bucket: capability.Bucket, Prefix: capability.Prefix}
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
	root := expandHome(firstNonEmpty(profile.Root, "~/.bgit/local-broker"))
	server := &localBrokerServer{root: root, objectRoot: filepath.Join(root, "objects"), baseURL: localBrokerURL(profileName, regionName)}
	if err := os.MkdirAll(server.objectRoot, 0o755); err != nil {
		return nil, err
	}
	return server, nil
}

func (s *localBrokerServer) localPost(path string, req any) ([]byte, int, error) {
	switch path {
	case "/owners/upsert":
		r := req.(brokerOwnerRequest)
		owners, _ := s.loadOwners()
		for _, key := range r.PublicKeys {
			key = normalizeKey(key)
			if key == "" {
				continue
			}
			found := false
			for i := range owners.Keys {
				if normalizeKey(owners.Keys[i].PublicKey) == key {
					owners.Keys[i].User = firstNonEmpty(r.User, "owner")
					owners.Keys[i].Role = firstNonEmpty(r.Role, "owner")
					found = true
				}
			}
			if !found {
				owners.Keys = append(owners.Keys, brokerKey{User: firstNonEmpty(r.User, "owner"), Role: firstNonEmpty(r.Role, "owner"), PublicKey: key})
			}
		}
		if err := s.saveJSON(filepath.Join(s.root, "owners.json"), owners); err != nil {
			return nil, 500, err
		}
		return mustJSON(map[string]bool{"ok": true}), 200, nil
	case "/teams/create":
		return mustJSON(map[string]bool{"ok": true}), 200, nil
	case "/teams/list":
		return mustJSON(map[string]any{"teams": []brokerTeamInfo{{ID: coreTeamID, Name: coreTeamName}}}), 200, nil
	case "/teams/resolve":
		return mustJSON(map[string]any{"team": brokerTeamInfo{ID: coreTeamID, Name: coreTeamName}}), 200, nil
	case "/repos/create":
		r := req.(brokerRepoAdminRequest)
		repo := s.physicalRepo(r.Repo)
		if state, err := s.loadRepo(repo); err == nil && state.Repo.Logical != "" {
			return mustJSON(map[string]string{"error": "repository already exists"}), http.StatusConflict, nil
		}
		if state, err := s.loadRepoForRequest(repo); err == nil && state.Repo.Logical != "" {
			return mustJSON(map[string]string{"error": "repository already exists"}), http.StatusConflict, nil
		}
		if err := s.ensureRepoStorage(context.Background(), repo); err != nil {
			return nil, 500, err
		}
		now := time.Now().UTC().Format(time.RFC3339)
		state := localBrokerRepoState{Repo: repo, Refs: map[string]string{}, Teams: []brokerRepoTeamGrant{{ID: coreTeamID, Role: firstNonEmpty(r.Role, "developer")}}, CreatedAt: now, UpdatedAt: now}
		for _, key := range r.PublicKeys {
			if key = normalizeKey(key); key != "" {
				state.Keys = append(state.Keys, brokerKey{User: firstNonEmpty(r.User, "owner"), Role: firstNonEmpty(r.Role, "developer"), PublicKey: key})
			}
		}
		if len(state.Keys) == 0 {
			owners, _ := s.loadOwners()
			for _, key := range owners.Keys {
				state.Keys = append(state.Keys, brokerKey{User: key.User, Role: firstNonEmpty(r.Role, "developer"), PublicKey: key.PublicKey})
			}
		}
		if err := s.saveRepo(state); err != nil {
			return nil, 500, err
		}
		return mustJSON(map[string]any{"ok": true, "repo": state.Repo}), 200, nil
	case "/repos/get":
		r := req.(brokerRepoRequest)
		state, err := s.loadRepoForRequest(r.Repo)
		if err != nil {
			return mustJSON(map[string]string{"error": "repository not found"}), http.StatusNotFound, nil
		}
		return mustJSON(map[string]any{"ok": true, "repo": state.Repo, "teams": state.Teams}), 200, nil
	case "/repos/list":
		repos := s.localRepoInfos()
		return mustJSON(map[string]any{"repos": repos}), 200, nil
	case "/auth/status":
		r := req.(brokerAuthStatusRequest)
		state, _ := s.loadRepoForRequest(r.Repo)
		return mustJSON(brokerAuthStatus{BrokerURL: s.baseURL, BrokerVersion: brokerVersion(), Repo: state.Repo, User: "owner", Role: "owner", Capabilities: roleCapabilitiesForLocal("owner"), ResolvedAt: time.Now().UTC().Format(time.RFC3339)}), 200, nil
	case "/auth/check":
		return mustJSON(brokerAuthResponse{Allowed: true, User: "owner", Role: "owner"}), 200, nil
	case "/objects/capability":
		r := req.(brokerObjectCapabilityRequest)
		state, err := s.loadRepoForRequest(r.Repo)
		if err != nil {
			return mustJSON(map[string]string{"error": "repository not found"}), http.StatusNotFound, nil
		}
		objectPath, err := validateLocalBrokerCapabilityPath(r.Operation, r.Path)
		if err != nil {
			return mustJSON(map[string]string{"error": err.Error()}), http.StatusForbidden, nil
		}
		object := localBrokerObjectName(state.Repo, objectPath)
		return mustJSON(brokerObjectCapabilityResponse{Provider: state.Repo.Provider, Mode: "local", URL: s.baseURL, Bucket: state.Repo.Bucket, Prefix: state.Repo.Prefix, Object: object}), 200, nil
	case "/objects/read":
		r := req.(brokerObjectRequest)
		state, err := s.loadRepoForRequest(r.Repo)
		if err != nil {
			return mustJSON(map[string]string{"error": "repository not found"}), http.StatusNotFound, nil
		}
		data, err := s.readObject(state.Repo, r.Path)
		if err != nil {
			return mustJSON(map[string]string{"error": err.Error()}), http.StatusNotFound, nil
		}
		return mustJSON(brokerObjectResponse{Data: base64.StdEncoding.EncodeToString(data)}), 200, nil
	case "/objects/list":
		r := req.(brokerObjectRequest)
		state, err := s.loadRepoForRequest(r.Repo)
		if err != nil {
			return mustJSON(map[string]string{"error": "repository not found"}), http.StatusNotFound, nil
		}
		paths, err := s.listObjects(state.Repo, r.Prefix)
		if err != nil {
			return nil, 500, err
		}
		return mustJSON(brokerObjectResponse{Paths: paths}), 200, nil
	case "/refs/update":
		r := req.(brokerRefUpdateRequest)
		state, err := s.loadRepoForRequest(r.Repo)
		if err != nil {
			return mustJSON(map[string]string{"error": "repository not found"}), http.StatusNotFound, nil
		}
		current := s.localRefHash(state.Repo, r.Ref, firstNonEmpty(state.Refs[r.Ref], r.Old))
		if current != r.Old {
			return mustJSON(map[string]string{"error": "stale ref"}), http.StatusConflict, nil
		}
		if state.Refs == nil {
			state.Refs = map[string]string{}
		}
		if r.New == zeroObjectID() {
			delete(state.Refs, r.Ref)
			_ = s.deleteObject(state.Repo, r.Ref)
			_ = s.deleteObject(state.Repo, localBrokerRefRecordPath(r.Ref))
		} else {
			state.Refs[r.Ref] = r.New
			_ = s.writeObject(state.Repo, r.Ref, []byte(r.New+"\n"))
			record := map[string]any{"ref": r.Ref, "hash": r.New, "updated_by": "owner", "updated_at": time.Now().UTC().Format(time.RFC3339)}
			_ = s.writeObject(state.Repo, localBrokerRefRecordPath(r.Ref), mustJSON(record))
		}
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := s.saveRepo(state); err != nil {
			return nil, 500, err
		}
		return mustJSON(map[string]bool{"ok": true}), 200, nil
	default:
		return mustJSON(map[string]string{"error": "unknown broker endpoint"}), http.StatusNotFound, nil
	}
}

func (s *localBrokerServer) handle(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_objects/") {
		s.handleObject(w, r)
		return
	}
	if r.Method != http.MethodPost && r.URL.Path != "/health" && r.URL.Path != "/" {
		localBrokerJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	var req map[string]any
	if len(bytes.TrimSpace(body)) > 0 {
		_ = json.Unmarshal(body, &req)
	}
	switch r.URL.Path {
	case "/", "/health":
		localBrokerJSON(w, 200, map[string]any{"ok": true, "service": "bgit-local-broker", "version": brokerVersion()})
	case "/owners/upsert":
		s.handleOwnersUpsert(w, r, body)
	case "/teams/create":
		localBrokerJSON(w, 200, map[string]bool{"ok": true})
	case "/teams/list":
		localBrokerJSON(w, 200, map[string]any{"teams": []brokerTeamInfo{{ID: coreTeamID, Name: coreTeamName}}})
	case "/teams/resolve":
		localBrokerJSON(w, 200, map[string]any{"team": brokerTeamInfo{ID: coreTeamID, Name: coreTeamName}})
	case "/repos/create":
		s.handleReposCreate(w, r, body)
	case "/repos/get":
		s.handleReposGet(w, r, body)
	case "/repos/list":
		s.handleReposList(w, r)
	case "/auth/status":
		s.handleAuthStatus(w, r, body)
	case "/auth/check":
		s.handleAuthCheck(w, r, body)
	case "/objects/capability":
		s.handleObjectCapability(w, r, body)
	case "/objects/read":
		s.handleObjectsRead(w, r, body)
	case "/objects/list":
		s.handleObjectsList(w, r, body)
	case "/refs/update":
		s.handleRefsUpdate(w, r, body)
	default:
		localBrokerJSON(w, http.StatusNotFound, map[string]string{"error": "unknown broker endpoint"})
	}
}

func (s *localBrokerServer) handleOwnersUpsert(w http.ResponseWriter, r *http.Request, body []byte) {
	var req brokerOwnerRequest
	if !localBrokerDecode(w, body, &req) {
		return
	}
	owners, _ := s.loadOwners()
	if len(owners.Keys) > 0 {
		if _, ok := s.signedOwner(r, body, owners); !ok {
			localBrokerJSON(w, http.StatusForbidden, map[string]string{"error": "owner SSH signature required"})
			return
		}
	} else if !s.validBootstrap(r) {
		localBrokerJSON(w, http.StatusForbidden, map[string]string{"error": "owner bootstrap token required"})
		return
	}
	for _, key := range req.PublicKeys {
		key = normalizeKey(key)
		if key == "" {
			continue
		}
		found := false
		for i := range owners.Keys {
			if normalizeKey(owners.Keys[i].PublicKey) == key {
				owners.Keys[i].User = firstNonEmpty(req.User, "owner")
				owners.Keys[i].Role = firstNonEmpty(req.Role, "owner")
				found = true
			}
		}
		if !found {
			owners.Keys = append(owners.Keys, brokerKey{User: firstNonEmpty(req.User, "owner"), Role: firstNonEmpty(req.Role, "owner"), PublicKey: key})
		}
	}
	if err := s.saveJSON(filepath.Join(s.root, "owners.json"), owners); err != nil {
		localBrokerJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	localBrokerJSON(w, 200, map[string]bool{"ok": true})
}

func (s *localBrokerServer) handleReposCreate(w http.ResponseWriter, r *http.Request, body []byte) {
	var req brokerRepoAdminRequest
	if !localBrokerDecode(w, body, &req) {
		return
	}
	if _, ok := s.requireOwner(r, body); !ok {
		localBrokerJSON(w, http.StatusForbidden, map[string]string{"error": "broker admin SSH signature required"})
		return
	}
	repo := s.physicalRepo(req.Repo)
	state, err := s.loadRepo(repo)
	if err == nil && state.Repo.Logical != "" {
		localBrokerJSON(w, http.StatusConflict, map[string]string{"error": "repository already exists"})
		return
	}
	if state, err := s.loadRepoForRequest(repo); err == nil && state.Repo.Logical != "" {
		localBrokerJSON(w, http.StatusConflict, map[string]string{"error": "repository already exists"})
		return
	}
	if err := s.ensureRepoStorage(r.Context(), repo); err != nil {
		localBrokerJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	state = localBrokerRepoState{Repo: repo, Refs: map[string]string{}, Teams: []brokerRepoTeamGrant{{ID: coreTeamID, Role: firstNonEmpty(req.Role, "developer")}}, CreatedAt: now, UpdatedAt: now}
	for _, key := range req.PublicKeys {
		if key = normalizeKey(key); key != "" {
			state.Keys = append(state.Keys, brokerKey{User: firstNonEmpty(req.User, "owner"), Role: firstNonEmpty(req.Role, "developer"), PublicKey: key})
		}
	}
	if err := s.saveRepo(state); err != nil {
		localBrokerJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	localBrokerJSON(w, 200, map[string]any{"ok": true, "repo": state.Repo})
}

func (s *localBrokerServer) handleReposGet(w http.ResponseWriter, r *http.Request, body []byte) {
	var req brokerRepoRequest
	if !localBrokerDecode(w, body, &req) {
		return
	}
	state, err := s.loadRepoForRequest(req.Repo)
	if err != nil {
		localBrokerJSON(w, http.StatusNotFound, map[string]string{"error": "repository not found"})
		return
	}
	if _, ok := s.signedRepoKey(r, body, state, "read"); !ok {
		localBrokerJSON(w, http.StatusForbidden, map[string]string{"error": "read SSH signature required"})
		return
	}
	localBrokerJSON(w, 200, map[string]any{"ok": true, "repo": state.Repo, "teams": state.Teams})
}

func (s *localBrokerServer) handleReposList(w http.ResponseWriter, r *http.Request) {
	localBrokerJSON(w, 200, map[string]any{"repos": s.localRepoInfos()})
}

func (s *localBrokerServer) localRepoInfos() []brokerRepoInfo {
	repos := []brokerRepoInfo{}
	seen := map[string]bool{}
	for _, repo := range s.loadRepoIndex().Repos {
		state, err := s.loadRepo(repo)
		if err == nil && state.Repo.Logical != "" {
			repos = append(repos, brokerRepoInfo{Repo: state.Repo, Logical: state.Repo.Logical, Teams: state.Teams})
			seen[strings.ToLower(state.Repo.Logical)] = true
		}
	}
	_ = filepath.WalkDir(s.objectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) != "repo.json" {
			return nil
		}
		var state localBrokerRepoState
		if s.readJSON(path, &state) == nil && state.Repo.Logical != "" && !seen[strings.ToLower(state.Repo.Logical)] {
			repos = append(repos, brokerRepoInfo{Repo: state.Repo, Logical: state.Repo.Logical, Teams: state.Teams})
			seen[strings.ToLower(state.Repo.Logical)] = true
		}
		return nil
	})
	sort.Slice(repos, func(i, j int) bool { return repos[i].Logical < repos[j].Logical })
	return repos
}

func (s *localBrokerServer) handleAuthStatus(w http.ResponseWriter, r *http.Request, body []byte) {
	var req brokerAuthStatusRequest
	_ = json.Unmarshal(body, &req)
	state, _ := s.loadRepoForRequest(req.Repo)
	key, _ := s.signedRepoKey(r, body, state, "read")
	role := ""
	user := ""
	if key != nil {
		role = key.Role
		user = key.User
	}
	localBrokerJSON(w, 200, brokerAuthStatus{BrokerURL: s.baseURL, BrokerVersion: brokerVersion(), Repo: state.Repo, User: user, Role: role, Capabilities: roleCapabilitiesForLocal(role), ResolvedAt: time.Now().UTC().Format(time.RFC3339)})
}

func (s *localBrokerServer) handleAuthCheck(w http.ResponseWriter, r *http.Request, body []byte) {
	var req brokerAuthRequest
	if !localBrokerDecode(w, body, &req) {
		return
	}
	state, err := s.loadRepoForRequest(req.Repo)
	if err != nil {
		localBrokerJSON(w, http.StatusNotFound, map[string]string{"error": "repository not found"})
		return
	}
	key, ok := s.signedRepoKey(r, body, state, localBrokerOperation(req.Operation))
	if !ok {
		localBrokerJSON(w, 200, brokerAuthResponse{Allowed: false})
		return
	}
	localBrokerJSON(w, 200, brokerAuthResponse{Allowed: true, User: key.User, Role: key.Role})
}

func (s *localBrokerServer) handleObjectCapability(w http.ResponseWriter, r *http.Request, body []byte) {
	var req brokerObjectCapabilityRequest
	if !localBrokerDecode(w, body, &req) {
		return
	}
	state, err := s.loadRepoForRequest(req.Repo)
	if err != nil {
		localBrokerJSON(w, http.StatusNotFound, map[string]string{"error": "repository not found"})
		return
	}
	op := req.Operation
	if op != "read" {
		op = "write"
	}
	if _, ok := s.signedRepoKey(r, body, state, op); !ok {
		localBrokerJSON(w, http.StatusForbidden, map[string]string{"error": op + " SSH signature required"})
		return
	}
	objectPath, err := validateLocalBrokerCapabilityPath(req.Operation, req.Path)
	if err != nil {
		localBrokerJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	method := http.MethodGet
	if req.Operation == "write" {
		method = http.MethodPut
	} else if req.Operation == "delete" {
		method = http.MethodDelete
	}
	object := localBrokerObjectName(state.Repo, objectPath)
	localBrokerJSON(w, 200, brokerObjectCapabilityResponse{
		Provider: "test",
		Mode:     "signed_url",
		Method:   method,
		URL:      s.baseURL + "/_objects/" + url.PathEscape(state.Repo.Bucket) + "/" + base64.RawURLEncoding.EncodeToString([]byte(object)),
		Headers:  map[string]string{"content-type": "application/octet-stream"},
		Bucket:   state.Repo.Bucket,
		Prefix:   state.Repo.Prefix,
		Object:   object,
	})
}

func (s *localBrokerServer) handleObjectsRead(w http.ResponseWriter, r *http.Request, body []byte) {
	var req brokerObjectRequest
	if !localBrokerDecode(w, body, &req) {
		return
	}
	state, err := s.loadRepoForRequest(req.Repo)
	if err != nil {
		localBrokerJSON(w, http.StatusNotFound, map[string]string{"error": "repository not found"})
		return
	}
	if _, ok := s.signedRepoKey(r, body, state, "read"); !ok {
		localBrokerJSON(w, http.StatusForbidden, map[string]string{"error": "read SSH signature required"})
		return
	}
	data, err := s.readObject(state.Repo, req.Path)
	if err != nil {
		localBrokerJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	localBrokerJSON(w, 200, brokerObjectResponse{Data: base64.StdEncoding.EncodeToString(data)})
}

func (s *localBrokerServer) handleObjectsList(w http.ResponseWriter, r *http.Request, body []byte) {
	var req brokerObjectRequest
	if !localBrokerDecode(w, body, &req) {
		return
	}
	state, err := s.loadRepoForRequest(req.Repo)
	if err != nil {
		localBrokerJSON(w, http.StatusNotFound, map[string]string{"error": "repository not found"})
		return
	}
	if _, ok := s.signedRepoKey(r, body, state, "read"); !ok {
		localBrokerJSON(w, http.StatusForbidden, map[string]string{"error": "read SSH signature required"})
		return
	}
	paths, err := s.listObjects(state.Repo, req.Prefix)
	if err != nil {
		localBrokerJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	localBrokerJSON(w, 200, brokerObjectResponse{Paths: paths})
}

func (s *localBrokerServer) handleRefsUpdate(w http.ResponseWriter, r *http.Request, body []byte) {
	var req brokerRefUpdateRequest
	if !localBrokerDecode(w, body, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadRepoForRequest(req.Repo)
	if err != nil {
		localBrokerJSON(w, http.StatusNotFound, map[string]string{"error": "repository not found"})
		return
	}
	key, ok := s.signedRepoKey(r, body, state, "write")
	if !ok {
		localBrokerJSON(w, http.StatusForbidden, map[string]string{"error": "write SSH signature required"})
		return
	}
	unlock, err := s.acquireRefLock(state.Repo, req.Ref)
	if err != nil {
		localBrokerJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	defer unlock()
	current := s.localRefHash(state.Repo, req.Ref, firstNonEmpty(state.Refs[req.Ref], req.Old))
	if current != req.Old {
		localBrokerJSON(w, http.StatusConflict, map[string]string{"error": "stale ref"})
		return
	}
	if state.Refs == nil {
		state.Refs = map[string]string{}
	}
	if req.New == zeroObjectID() {
		delete(state.Refs, req.Ref)
		_ = s.deleteObject(state.Repo, req.Ref)
		_ = s.deleteObject(state.Repo, localBrokerRefRecordPath(req.Ref))
	} else {
		state.Refs[req.Ref] = req.New
		_ = s.writeObject(state.Repo, req.Ref, []byte(req.New+"\n"))
		record := map[string]any{"ref": req.Ref, "hash": req.New, "updated_by": key.User, "updated_at": time.Now().UTC().Format(time.RFC3339)}
		data, _ := json.MarshalIndent(record, "", "  ")
		_ = s.writeObject(state.Repo, localBrokerRefRecordPath(req.Ref), data)
	}
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.saveRepo(state); err != nil {
		localBrokerJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	localBrokerJSON(w, 200, map[string]bool{"ok": true})
}

func (s *localBrokerServer) acquireRefLock(repo brokerRepo, ref string) (func(), error) {
	lockName := base64.RawURLEncoding.EncodeToString([]byte(ref)) + ".lock"
	path := s.objectPath(repo, filepath.ToSlash(filepath.Join(".bucketgit", "broker-state", "v1", "locks", lockName)))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	deadline := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339Nano)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_, writeErr := f.WriteString(deadline)
			closeErr := f.Close()
			if writeErr != nil {
				_ = os.Remove(path)
				return nil, writeErr
			}
			if closeErr != nil {
				_ = os.Remove(path)
				return nil, closeErr
			}
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		data, readErr := os.ReadFile(path)
		if readErr == nil {
			if expires, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data))); parseErr == nil && time.Now().UTC().After(expires) {
				_ = os.Remove(path)
				continue
			}
		}
		return nil, fmt.Errorf("ref %s is locked", ref)
	}
	return nil, fmt.Errorf("ref %s is locked", ref)
}

func (s *localBrokerServer) handleObject(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/_objects/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	bucket, _ := url.PathUnescape(parts[0])
	objectBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	repo := brokerRepo{Bucket: bucket}
	object := string(objectBytes)
	switch r.Method {
	case http.MethodGet:
		data, err := s.readObject(repo, object)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	case http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		if err := s.writeObject(repo, object, data); err != nil {
			localBrokerJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		localBrokerJSON(w, 200, map[string]bool{"ok": true})
	case http.MethodDelete:
		_ = s.deleteObject(repo, object)
		localBrokerJSON(w, 200, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *localBrokerServer) physicalRepo(repo brokerRepo) brokerRepo {
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

func (s *localBrokerServer) loadRepo(repo brokerRepo) (localBrokerRepoState, error) {
	var state localBrokerRepoState
	if s.repoUsesCloudStorage(repo) {
		data, err := s.readObject(repo, ".bucketgit/broker-state/v1/repo.json")
		if err != nil {
			return state, err
		}
		err = json.Unmarshal(data, &state)
		state.Repo = s.physicalRepo(state.Repo)
		return state, err
	}
	err := s.readJSON(s.repoStatePath(repo), &state)
	if err == nil {
		state.Repo = s.physicalRepo(state.Repo)
	}
	return state, err
}

func (s *localBrokerServer) loadRepoForRequest(repo brokerRepo) (localBrokerRepoState, error) {
	physical := s.physicalRepo(repo)
	state, err := s.loadRepo(physical)
	if err == nil {
		return state, nil
	}
	logical := strings.TrimSpace(physical.Logical)
	if logical == "" {
		return localBrokerRepoState{}, err
	}
	if indexed, ok := s.indexedRepo(logical); ok {
		state, indexErr := s.loadRepo(indexed)
		if indexErr == nil && strings.EqualFold(strings.TrimSpace(state.Repo.Logical), logical) {
			return state, nil
		}
	}
	var found localBrokerRepoState
	walkErr := filepath.WalkDir(s.objectRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || filepath.Base(path) != "repo.json" {
			return nil
		}
		var candidate localBrokerRepoState
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
		return localBrokerRepoState{}, walkErr
	}
	return localBrokerRepoState{}, err
}

func (s *localBrokerServer) saveRepo(state localBrokerRepoState) error {
	if !s.repoUsesCloudStorage(state.Repo) {
		if err := s.saveJSON(s.repoStatePath(state.Repo), state); err != nil {
			return err
		}
	}
	if err := s.writeObject(state.Repo, ".bucketgit/broker-state/v1/repo.json", mustJSON(state)); err != nil {
		return err
	}
	return s.upsertRepoIndex(state.Repo)
}

func (s *localBrokerServer) repoIndexPath() string {
	return filepath.Join(s.root, "repos.json")
}

func (s *localBrokerServer) loadRepoIndex() localBrokerRepoIndex {
	var index localBrokerRepoIndex
	_ = s.readJSON(s.repoIndexPath(), &index)
	return index
}

func (s *localBrokerServer) indexedRepo(logical string) (brokerRepo, bool) {
	logical = strings.TrimSpace(logical)
	if logical == "" {
		return brokerRepo{}, false
	}
	for _, repo := range s.loadRepoIndex().Repos {
		if strings.EqualFold(strings.TrimSpace(repo.Logical), logical) {
			return repo, true
		}
	}
	return brokerRepo{}, false
}

func (s *localBrokerServer) upsertRepoIndex(repo brokerRepo) error {
	if strings.TrimSpace(repo.Logical) == "" {
		return nil
	}
	index := s.loadRepoIndex()
	for i := range index.Repos {
		if strings.EqualFold(strings.TrimSpace(index.Repos[i].Logical), strings.TrimSpace(repo.Logical)) {
			index.Repos[i] = repo
			return s.saveJSON(s.repoIndexPath(), index)
		}
	}
	index.Repos = append(index.Repos, repo)
	sort.Slice(index.Repos, func(i, j int) bool { return index.Repos[i].Logical < index.Repos[j].Logical })
	return s.saveJSON(s.repoIndexPath(), index)
}

func (s *localBrokerServer) repoStatePath(repo brokerRepo) string {
	return filepath.Join(s.bucketDir(repo.Bucket), ".bucketgit", "broker-state", "v1", "repo.json")
}

func (s *localBrokerServer) bucketDir(bucket string) string {
	if strings.HasPrefix(bucket, "file://") {
		path := strings.TrimPrefix(bucket, "file://")
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		clean := filepath.Clean(strings.TrimPrefix(path, "/"))
		if clean == "." {
			clean = "repo.git"
		}
		return filepath.Join(s.objectRoot, clean)
	}
	return filepath.Join(s.objectRoot, base64.RawURLEncoding.EncodeToString([]byte(bucket)))
}

func (s *localBrokerServer) ensureRepoStorage(ctx context.Context, repo brokerRepo) error {
	cfg, ok, err := localBrokerRepoCloudConfig(repo)
	if err != nil || !ok {
		return err
	}
	return ensureBucket(ctx, cfg)
}

func (s *localBrokerServer) repoUsesCloudStorage(repo brokerRepo) bool {
	_, ok, _ := localBrokerRepoCloudConfig(repo)
	return ok
}

func (s *localBrokerServer) cloudStore(ctx context.Context, repo brokerRepo) (writableGitRemoteStore, func(), bool, error) {
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

func localBrokerRepoCloudConfig(repo brokerRepo) (config, bool, error) {
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
		auth:                defaultAuthMode,
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
		auth:                defaultAuthMode,
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

func (s *localBrokerServer) readObject(repo brokerRepo, object string) ([]byte, error) {
	if store, closeStore, ok, err := s.cloudStore(context.Background(), repo); ok || err != nil {
		if closeStore != nil {
			defer closeStore()
		}
		if err != nil {
			return nil, err
		}
		return store.read(context.Background(), object)
	}
	return os.ReadFile(s.objectPath(repo, object))
}

func (s *localBrokerServer) writeObject(repo brokerRepo, object string, data []byte) error {
	if store, closeStore, ok, err := s.cloudStore(context.Background(), repo); ok || err != nil {
		if closeStore != nil {
			defer closeStore()
		}
		if err != nil {
			return err
		}
		return store.write(context.Background(), object, data)
	}
	path := s.objectPath(repo, object)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (s *localBrokerServer) deleteObject(repo brokerRepo, object string) error {
	if store, closeStore, ok, err := s.cloudStore(context.Background(), repo); ok || err != nil {
		if closeStore != nil {
			defer closeStore()
		}
		if err != nil {
			return err
		}
		return store.delete(context.Background(), object)
	}
	return os.Remove(s.objectPath(repo, object))
}

func (s *localBrokerServer) listObjects(repo brokerRepo, prefix string) ([]string, error) {
	if store, closeStore, ok, err := s.cloudStore(context.Background(), repo); ok || err != nil {
		if closeStore != nil {
			defer closeStore()
		}
		if err != nil {
			return nil, err
		}
		return store.list(context.Background(), prefix)
	}
	root := s.bucketDir(repo.Bucket)
	prefix = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(strings.TrimPrefix(prefix, "/"))), ".")
	if prefix == "/" {
		prefix = ""
	}
	searchRoot := root
	if strings.TrimSpace(repo.Prefix) != "" {
		searchRoot = filepath.Join(searchRoot, filepath.FromSlash(strings.Trim(repo.Prefix, "/")))
	}
	var paths []string
	err := filepath.WalkDir(searchRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(searchRoot, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if prefix == "" || strings.HasPrefix(rel, prefix) {
			paths = append(paths, rel)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	sort.Strings(paths)
	return paths, err
}

func (s *localBrokerServer) objectPath(repo brokerRepo, object string) string {
	root := s.bucketDir(repo.Bucket)
	prefix := strings.Trim(repo.Prefix, "/")
	if prefix != "" {
		object = prefix + "/" + strings.TrimPrefix(object, "/")
	}
	clean := filepath.Clean(strings.TrimPrefix(object, "/"))
	return filepath.Join(root, clean)
}

func (s *localBrokerServer) loadOwners() (localBrokerOwners, error) {
	var owners localBrokerOwners
	err := s.readJSON(filepath.Join(s.root, "owners.json"), &owners)
	return owners, err
}

func (s *localBrokerServer) readJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func (s *localBrokerServer) saveJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, mustJSON(value), 0o644)
}

func (s *localBrokerServer) validBootstrap(r *http.Request) bool {
	if s.bootstrapHash == "" {
		return false
	}
	return brokerSecretHash(r.Header.Get("x-bgit-bootstrap-token")) == s.bootstrapHash
}

func (s *localBrokerServer) requireOwner(r *http.Request, body []byte) (*brokerKey, bool) {
	owners, _ := s.loadOwners()
	return s.signedOwner(r, body, owners)
}

func (s *localBrokerServer) signedOwner(r *http.Request, body []byte, owners localBrokerOwners) (*brokerKey, bool) {
	for i := range owners.Keys {
		key := &owners.Keys[i]
		if (key.Role == "owner" || key.Role == "admin") && verifyLocalBrokerSignature(r, body, key.PublicKey) {
			return key, true
		}
	}
	return nil, false
}

func (s *localBrokerServer) signedRepoKey(r *http.Request, body []byte, state localBrokerRepoState, operation string) (*brokerKey, bool) {
	for i := range state.Keys {
		key := &state.Keys[i]
		if roleAllowsLocal(key.Role, operation) && verifyLocalBrokerSignature(r, body, key.PublicKey) {
			return key, true
		}
	}
	return nil, false
}

func verifyLocalBrokerSignature(r *http.Request, body []byte, publicKey string) bool {
	if strings.TrimSpace(publicKey) == "" || r.Header.Get("x-bgit-signature-version") != "2" {
		localBrokerSignatureDebug("missing key or signature version")
		return false
	}
	if normalizeKey(r.Header.Get("x-bgit-key")) != normalizeKey(publicKey) {
		localBrokerSignatureDebug("key mismatch")
		return false
	}
	messageB64 := r.Header.Get("x-bgit-signature-message")
	sigB64 := r.Header.Get("x-bgit-signature")
	message, err := base64.StdEncoding.DecodeString(messageB64)
	if err != nil {
		localBrokerSignatureDebug("message decode failed: " + err.Error())
		return false
	}
	want := brokerSignatureMessage(r.Method, r.URL.RequestURI(), strings.ToLower(r.Host), r.Header.Get("x-bgit-timestamp"), r.Header.Get("x-bgit-nonce"), body)
	if !bytes.Equal(message, want) {
		localBrokerSignatureDebug(fmt.Sprintf("message mismatch got=%q want=%q host=%q path=%q", string(message), string(want), r.Host, r.URL.RequestURI()))
		return false
	}
	var sig ssh.Signature
	sigRaw, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		localBrokerSignatureDebug("signature decode failed: " + err.Error())
		return false
	}
	_ = ssh.Unmarshal(sigRaw, &sig)
	if sig.Format == "" || len(sig.Blob) == 0 {
		localBrokerSignatureDebug("signature unmarshal failed")
		return false
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		localBrokerSignatureDebug("public key parse failed: " + err.Error())
		return false
	}
	if err := pub.Verify(message, &sig); err != nil {
		localBrokerSignatureDebug("signature verify failed: " + err.Error())
		return false
	}
	return true
}

func localBrokerSignatureDebug(msg string) {
	if os.Getenv("BGIT_LOCAL_BROKER_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, "local broker signature:", msg)
	}
}

func localBrokerOperation(operation string) string {
	switch strings.ToLower(operation) {
	case "write", "push", "upload-pack":
		return "write"
	default:
		return "read"
	}
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

func localBrokerObjectName(repo brokerRepo, objectPath string) string {
	prefix := strings.Trim(repo.Prefix, "/")
	if prefix == "" {
		return objectPath
	}
	return prefix + "/" + strings.TrimPrefix(objectPath, "/")
}

func localBrokerRefRecordPath(ref string) string {
	return ".bucketgit/broker-state/v1/refs/" + base64.RawURLEncoding.EncodeToString([]byte(ref)) + ".json"
}

func (s *localBrokerServer) localRefHash(repo brokerRepo, ref, fallback string) string {
	var record struct {
		Hash string `json:"hash"`
	}
	if s.readJSON(s.objectPath(repo, localBrokerRefRecordPath(ref)), &record) == nil && record.Hash != "" {
		return record.Hash
	}
	if data, err := s.readObject(repo, ref); err == nil {
		text := strings.TrimSpace(string(data))
		if len(text) == 40 {
			return text
		}
	}
	return fallback
}

func localBrokerDecode(w http.ResponseWriter, data []byte, dst any) bool {
	if err := json.Unmarshal(data, dst); err != nil {
		localBrokerJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return false
	}
	return true
}

func localBrokerJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
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
