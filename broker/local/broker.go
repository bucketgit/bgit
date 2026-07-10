// Package local implements the in-process BucketGit broker's durable object
// and metadata substrate. Endpoint parsing and CLI profile lookup intentionally
// live outside this package.
package local

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bucketgit/bgit/protocol"
	"github.com/bucketgit/bgit/store"
	fsstore "github.com/bucketgit/bgit/store/fs"
)

type Resolver interface {
	Resolve(ctx context.Context, repo protocol.Repository) (backend store.Writer, close func() error, ok bool, err error)
}

type ResolverFunc func(context.Context, protocol.Repository) (store.Writer, func() error, bool, error)

func (f ResolverFunc) Resolve(ctx context.Context, repo protocol.Repository) (store.Writer, func() error, bool, error) {
	return f(ctx, repo)
}

type Options struct {
	Root     string
	Resolver Resolver
	Now      func() time.Time
	Verifier IdentityVerifier
}

type Broker struct {
	root       string
	objectRoot string
	resolver   Resolver
	locksMu    sync.Mutex
	locks      map[string]*sync.Mutex
	now        func() time.Time
	verifier   IdentityVerifier
}

type RepositoryStore struct {
	broker *Broker
	repo   protocol.Repository
}

func New(options Options) (*Broker, error) {
	root, err := filepath.Abs(strings.TrimSpace(options.Root))
	if err != nil || strings.TrimSpace(options.Root) == "" {
		return nil, fmt.Errorf("local broker root is required")
	}
	objectRoot := filepath.Join(root, "objects")
	backend, err := fsstore.New(objectRoot)
	if err != nil {
		return nil, err
	}
	_ = backend
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Broker{root: root, objectRoot: objectRoot, resolver: options.Resolver, locks: map[string]*sync.Mutex{}, now: now, verifier: options.Verifier}, nil
}

func (b *Broker) Root() string       { return b.root }
func (b *Broker) ObjectRoot() string { return b.objectRoot }
func (b *Broker) Repository(repo protocol.Repository) *RepositoryStore {
	return &RepositoryStore{broker: b, repo: repo}
}

func (r *RepositoryStore) Read(ctx context.Context, path string) ([]byte, error) {
	return r.broker.ReadObject(ctx, r.repo, path)
}
func (r *RepositoryStore) List(ctx context.Context, prefix string) ([]string, error) {
	return r.broker.ListObjects(ctx, r.repo, prefix)
}
func (r *RepositoryStore) Write(ctx context.Context, path string, data []byte) error {
	return r.broker.WriteObject(ctx, r.repo, path, data)
}
func (r *RepositoryStore) Delete(ctx context.Context, path string) error {
	return r.broker.DeleteObject(ctx, r.repo, path)
}
func (r *RepositoryStore) ListRefs(ctx context.Context) (map[string]string, error) {
	return store.ReadRefs(ctx, r)
}

func (r *RepositoryStore) CompareAndSwapRef(ctx context.Context, ref, oldOID, newOID string) error {
	backend, closeBackend, err := r.broker.backend(ctx, r.repo)
	if err != nil {
		return err
	}
	defer closeIgnoringError(closeBackend)
	refs, ok := backend.(store.RefStore)
	if !ok {
		return store.ErrNotSupported
	}
	return refs.CompareAndSwapRef(ctx, ref, oldOID, newOID)
}

func (r *RepositoryStore) CompareAndSwap(ctx context.Context, path string, expected store.ObjectState, replacement []byte) error {
	backend, closeBackend, err := r.broker.backend(ctx, r.repo)
	if err != nil {
		return err
	}
	defer closeIgnoringError(closeBackend)
	cas, ok := backend.(store.CompareAndSwapper)
	if !ok {
		return store.ErrNotSupported
	}
	return cas.CompareAndSwap(ctx, path, expected, replacement)
}

var _ store.Writer = (*RepositoryStore)(nil)
var _ store.RefStore = (*RepositoryStore)(nil)
var _ store.CompareAndSwapper = (*RepositoryStore)(nil)

func (b *Broker) BucketDir(bucket string) string {
	if strings.HasPrefix(bucket, "file://") {
		value := strings.TrimPrefix(bucket, "file://")
		if filepath.IsAbs(value) {
			return filepath.Clean(value)
		}
		value = filepath.Clean(strings.TrimPrefix(value, "/"))
		if value == "." {
			value = "repo.git"
		}
		return filepath.Join(b.objectRoot, value)
	}
	return filepath.Join(b.objectRoot, base64.RawURLEncoding.EncodeToString([]byte(bucket)))
}

func (b *Broker) ObjectPath(repo protocol.Repository, objectPath string) (string, error) {
	root := b.BucketDir(repo.Bucket)
	if prefix := strings.Trim(repo.Prefix, "/"); prefix != "" {
		objectPath = prefix + "/" + strings.TrimPrefix(objectPath, "/")
	}
	validated, err := store.ValidatePath(strings.TrimPrefix(objectPath, "/"), false)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(validated)), nil
}

func (b *Broker) ReadObject(ctx context.Context, repo protocol.Repository, objectPath string) ([]byte, error) {
	backend, closeBackend, err := b.backend(ctx, repo)
	if err != nil {
		return nil, err
	}
	defer closeIgnoringError(closeBackend)
	return backend.Read(ctx, objectPath)
}

func (b *Broker) WriteObject(ctx context.Context, repo protocol.Repository, objectPath string, data []byte) error {
	backend, closeBackend, err := b.backend(ctx, repo)
	if err != nil {
		return err
	}
	defer closeIgnoringError(closeBackend)
	return backend.Write(ctx, objectPath, data)
}

func (b *Broker) DeleteObject(ctx context.Context, repo protocol.Repository, objectPath string) error {
	backend, closeBackend, err := b.backend(ctx, repo)
	if err != nil {
		return err
	}
	defer closeIgnoringError(closeBackend)
	err = backend.Delete(ctx, objectPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (b *Broker) ListObjects(ctx context.Context, repo protocol.Repository, prefix string) ([]string, error) {
	backend, closeBackend, err := b.backend(ctx, repo)
	if err != nil {
		return nil, err
	}
	defer closeIgnoringError(closeBackend)
	paths, err := backend.List(ctx, prefix)
	sort.Strings(paths)
	return paths, err
}

func (b *Broker) LoadJSON(ctx context.Context, repo protocol.Repository, path string, target any) error {
	data, err := b.ReadObject(ctx, repo, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func (b *Broker) SaveJSON(ctx context.Context, repo protocol.Repository, path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return b.WriteObject(ctx, repo, path, data)
}

func (b *Broker) LoadJSONState(ctx context.Context, repo protocol.Repository, path string, target any) (store.ObjectState, error) {
	data, err := b.ReadObject(ctx, repo, path)
	if errors.Is(err, fs.ErrNotExist) {
		return store.ObjectState{}, nil
	}
	if err != nil {
		return store.ObjectState{}, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return store.ObjectState{}, err
	}
	return store.ObjectState{Exists: true, Data: data}, nil
}

func (b *Broker) CompareAndSwapJSON(ctx context.Context, repo protocol.Repository, path string, expected store.ObjectState, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	backend := b.Repository(repo)
	return backend.CompareAndSwap(ctx, path, expected, data)
}

func (b *Broker) LockRepository(repo protocol.Repository) func() {
	key := strings.Join([]string{repo.Provider, repo.Bucket, repo.Prefix, repo.Logical}, "\x00")
	b.locksMu.Lock()
	lock := b.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		b.locks[key] = lock
	}
	b.locksMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (b *Broker) backend(ctx context.Context, repo protocol.Repository) (store.Writer, func() error, error) {
	if b.resolver != nil {
		backend, closeBackend, ok, err := b.resolver.Resolve(ctx, repo)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			if backend == nil {
				return nil, nil, errors.New("local broker resolver returned a nil backend")
			}
			return backend, closeBackend, nil
		}
	}
	root := b.BucketDir(repo.Bucket)
	if prefix := strings.Trim(repo.Prefix, "/"); prefix != "" {
		root = filepath.Join(root, filepath.FromSlash(prefix))
	}
	backend, err := fsstore.New(root)
	return backend, nil, err
}

func closeIgnoringError(closeBackend func() error) {
	if closeBackend != nil {
		_ = closeBackend()
	}
}
