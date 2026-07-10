package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bucketgit/bgit/protocol"
	"github.com/bucketgit/bgit/store"
	fsstore "github.com/bucketgit/bgit/store/fs"
	"github.com/bucketgit/bgit/store/storetest"
)

func TestRepositoryStoreContract(t *testing.T) {
	factory := func(t *testing.T) store.Writer {
		broker, err := New(Options{Root: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return broker.Repository(protocol.Repository{Provider: "file", Bucket: "contract.git", Logical: "contract.git"})
	}
	storetest.Run(t, factory)
	storetest.RunCompareAndSwap(t, factory)
}

func TestCloudResolvedRepositoryStoreContracts(t *testing.T) {
	for _, provider := range []string{"s3", "gcs"} {
		t.Run(provider, func(t *testing.T) {
			factory := func(t *testing.T) store.Writer {
				resolved, err := fsstore.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				broker, err := New(Options{Root: t.TempDir(), Resolver: ResolverFunc(func(_ context.Context, repo protocol.Repository) (store.Writer, func() error, bool, error) {
					if repo.Provider != provider {
						t.Fatalf("provider=%q want %q", repo.Provider, provider)
					}
					return resolved, nil, true, nil
				})})
				if err != nil {
					t.Fatal(err)
				}
				return broker.Repository(protocol.Repository{Provider: provider, Bucket: "contract.git", Logical: "contract.git"})
			}
			storetest.Run(t, factory)
			storetest.RunCompareAndSwap(t, factory)
		})
	}
}

func TestConcurrentBrokerInstancesDoNotLoseIssueWrites(t *testing.T) {
	root := t.TempDir()
	repo := protocol.Repository{Provider: "file", Bucket: "shared.git", Logical: "shared.git"}
	const writers = 12
	var wait sync.WaitGroup
	results := make(chan error, writers)
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			broker, err := New(Options{Root: root})
			if err == nil {
				_, err = broker.CreateIssue(t.Context(), repo, protocol.IssueRequest{Title: fmt.Sprintf("issue-%d", index)}, "owner")
			}
			results <- err
		}(index)
	}
	wait.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("unexpected concurrent mutation error: %v", err)
		}
	}
	broker, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	issues, err := broker.ListIssues(t.Context(), repo, IssueFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != succeeded || succeeded == 0 {
		t.Fatalf("persisted=%d succeeded=%d", len(issues), succeeded)
	}
}

func TestFilesystemBrokerObjectRoundTripAndIsolation(t *testing.T) {
	broker, err := New(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	one := protocol.Repository{Provider: "file", Bucket: "one.git", Logical: "one.git"}
	two := protocol.Repository{Provider: "file", Bucket: "two.git", Logical: "two.git"}
	if err := broker.WriteObject(context.Background(), one, "objects/aa/object", []byte("one")); err != nil {
		t.Fatal(err)
	}
	data, err := broker.ReadObject(context.Background(), one, "objects/aa/object")
	if err != nil || string(data) != "one" {
		t.Fatalf("read = %q, %v", data, err)
	}
	if _, err := broker.ReadObject(context.Background(), two, "objects/aa/object"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("isolated read error = %v", err)
	}
}

func TestHTTPHandlerUsesDirectBrokerServices(t *testing.T) {
	repo := protocol.Repository{Provider: "file", Bucket: "http.git", Logical: "http.git"}
	broker, err := New(Options{Root: t.TempDir(), Verifier: IdentityVerifierFunc(func(context.Context, protocol.AuthRequest) (protocol.Identity, error) {
		return protocol.Identity{User: "dev"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SaveRepositoryState(t.Context(), RepositoryState{Repo: repo, Keys: []protocol.Key{{User: "dev", Role: "developer"}}}); err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{"repo":{"provider":"file","bucket":"http.git","prefix":"","origin":"","logical":"http.git"},"type":"story","title":"From HTTP"}`)
	request := httptest.NewRequest(http.MethodPost, "/issues/create", body)
	response := httptest.NewRecorder()
	broker.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	issues, err := broker.ListIssues(t.Context(), repo, IssueFilter{Type: "story"})
	if err != nil || len(issues) != 1 || issues[0].Title != "From HTTP" {
		t.Fatalf("issues=%#v err=%v", issues, err)
	}
}

func TestLoadsExistingBrokerStateLayout(t *testing.T) {
	root := t.TempDir()
	broker, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	repo := protocol.Repository{Provider: "file", Bucket: "legacy.git", Logical: "legacy.git"}
	state := RepositoryState{Repo: repo, Refs: map[string]string{"refs/heads/main": "0123456789abcdef0123456789abcdef01234567"}}
	data, _ := json.Marshal(state)
	path := filepath.Join(broker.BucketDir(repo.Bucket), filepath.FromSlash(repositoryStatePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := broker.LoadRepositoryState(t.Context(), repo)
	if err != nil || loaded.Repo.Logical != "legacy.git" || loaded.Refs["refs/heads/main"] == "" {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
}

func TestBoardServiceOrdersAndArchivesStories(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	broker, err := New(Options{Root: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := protocol.Repository{Provider: "file", Bucket: "board.git", Logical: "board.git"}
	one, err := broker.CreateIssue(t.Context(), repo, protocol.IssueRequest{Type: "story", Title: "One", Lane: "backlog"}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	two, err := broker.CreateIssue(t.Context(), repo, protocol.IssueRequest{Type: "story", Title: "Two", Lane: "backlog"}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.MoveStory(t.Context(), repo, two.ID, StoryMove{Lane: "backlog", Order: 1}, "owner"); err != nil {
		t.Fatal(err)
	}
	issues, err := broker.ListIssues(t.Context(), repo, IssueFilter{Type: "story"})
	if err != nil || len(issues) != 2 || issues[0].ID != two.ID || issues[1].ID != one.ID || issues[0].Position != 1 || issues[1].Position != 2 {
		t.Fatalf("ordered issues = %#v, %v", issues, err)
	}
	if err := broker.ArchiveIssue(t.Context(), repo, two.ID, true, "owner"); err != nil {
		t.Fatal(err)
	}
	issues, err = broker.ListIssues(t.Context(), repo, IssueFilter{Type: "story"})
	if err != nil || len(issues) != 1 || issues[0].ID != one.ID {
		t.Fatalf("active issues = %#v, %v", issues, err)
	}
	issues, err = broker.ListIssues(t.Context(), repo, IssueFilter{Type: "story", IncludeArchived: true})
	if err != nil || len(issues) != 2 {
		t.Fatalf("all issues = %#v, %v", issues, err)
	}
}

func TestAuthorizeUsesInjectedIdentityAndRepositoryRole(t *testing.T) {
	repo := protocol.Repository{Provider: "file", Bucket: "auth.git", Logical: "auth.git"}
	broker, err := New(Options{Root: t.TempDir(), Verifier: IdentityVerifierFunc(func(context.Context, protocol.AuthRequest) (protocol.Identity, error) {
		return protocol.Identity{User: "dev"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SaveRepositoryState(t.Context(), RepositoryState{Repo: repo, Keys: []protocol.Key{{User: "dev", Role: "developer"}}}); err != nil {
		t.Fatal(err)
	}
	response, err := broker.Authorize(t.Context(), protocol.AuthRequest{Repo: repo, Operation: "write"})
	if err != nil || !response.Allowed || response.Role != "developer" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	response, err = broker.Authorize(t.Context(), protocol.AuthRequest{Repo: repo, Operation: "merge"})
	if err != nil || response.Allowed {
		t.Fatalf("merge response=%#v err=%v", response, err)
	}
}

func TestFilesystemBrokerRejectsTraversal(t *testing.T) {
	broker, err := New(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	repo := protocol.Repository{Provider: "file", Bucket: "repo.git", Logical: "repo.git"}
	if err := broker.WriteObject(context.Background(), repo, "../escape", []byte("bad")); err == nil {
		t.Fatal("traversal write succeeded")
	}
}

func TestUpdateRefPersistsGitRefAndRejectsStaleExpectedOID(t *testing.T) {
	broker, err := New(Options{Root: t.TempDir(), Now: func() time.Time {
		return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}
	repo := protocol.Repository{Provider: "file", Bucket: "refs.git", Logical: "refs.git"}
	if err := broker.SaveRepositoryState(t.Context(), RepositoryState{Repo: repo, Refs: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	const oid = "0123456789abcdef0123456789abcdef01234567"
	request := protocol.RefUpdateRequest{Repo: repo, Ref: "refs/heads/main", New: oid}
	if err := broker.UpdateRef(t.Context(), request, "owner"); err != nil {
		t.Fatal(err)
	}
	refs, err := broker.Repository(repo).ListRefs(t.Context())
	if err != nil || refs[request.Ref] != oid {
		t.Fatalf("refs=%#v err=%v", refs, err)
	}
	state, err := broker.LoadRepositoryState(t.Context(), repo)
	if err != nil || state.Refs[request.Ref] != oid {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	recordPath := ".bucketgit/broker-state/v1/refs/cmVmcy9oZWFkcy9tYWlu.json"
	var record map[string]any
	if err := broker.LoadJSON(t.Context(), repo, recordPath, &record); err != nil || record["hash"] != oid {
		t.Fatalf("record=%#v err=%v", record, err)
	}
	stale := request
	stale.Old = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	stale.New = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := broker.UpdateRef(t.Context(), stale, "owner"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale update error=%v", err)
	}
	refs, err = broker.Repository(repo).ListRefs(t.Context())
	if err != nil || refs[request.Ref] != oid {
		t.Fatalf("refs after stale update=%#v err=%v", refs, err)
	}
}

func TestReconcileRepositoryRepairsCachedRefs(t *testing.T) {
	broker, err := New(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	repo := protocol.Repository{Provider: "file", Bucket: "reconcile.git", Logical: "reconcile.git"}
	const oid = "0123456789abcdef0123456789abcdef01234567"
	state := RepositoryState{Repo: repo, Refs: map[string]string{"refs/heads/stale": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	if err := broker.SaveRepositoryState(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	if err := broker.Repository(repo).CompareAndSwapRef(t.Context(), "refs/heads/main", "", oid); err != nil {
		t.Fatal(err)
	}
	reconciled, err := broker.ReconcileRepository(t.Context(), repo, "repair")
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled.Refs) != 1 || reconciled.Refs["refs/heads/main"] != oid {
		t.Fatalf("refs=%#v", reconciled.Refs)
	}
	loaded, err := broker.LoadRepositoryState(t.Context(), repo)
	if err != nil || loaded.Refs["refs/heads/main"] != oid {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
}

func TestOwnerAndRepositoryServicesPersistState(t *testing.T) {
	broker, err := New(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	owners, err := broker.UpsertOwners(t.Context(), protocol.OwnerRequest{User: "owner", Role: "owner", PublicKeys: []string{"ssh-ed25519 AAAATEST comment"}})
	if err != nil || len(owners.Keys) != 1 || owners.Keys[0].PublicKey != "ssh-ed25519 AAAATEST" {
		t.Fatalf("owners=%#v err=%v", owners, err)
	}
	repo := protocol.Repository{Provider: "file", Bucket: "admin.git", Logical: "admin.git"}
	request := protocol.RepositoryAdminRequest{Repo: repo, User: "owner", Role: "developer"}
	state, err := broker.CreateRepository(t.Context(), request, owners.Keys)
	if err != nil || state.Repo.Logical != repo.Logical || len(state.Keys) != 1 {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	if _, err := broker.CreateRepository(t.Context(), request, owners.Keys); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("duplicate error=%v", err)
	}
	repositories, err := broker.ListRepositories(t.Context())
	if err != nil || len(repositories) != 1 || repositories[0].Repo.Logical != repo.Logical {
		t.Fatalf("repositories=%#v err=%v", repositories, err)
	}
}
