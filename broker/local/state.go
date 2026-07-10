package local

import (
	"context"
	"errors"
	"io/fs"
	"sort"
	"strings"

	"github.com/bucketgit/bgit/protocol"
	"github.com/bucketgit/bgit/store"
	fsstore "github.com/bucketgit/bgit/store/fs"
)

const repositoryStatePath = ".bucketgit/broker-state/v1/repo.json"

type RepositoryState struct {
	Repo      protocol.Repository            `json:"repo"`
	Keys      []protocol.Key                 `json:"keys"`
	Refs      map[string]string              `json:"refs,omitempty"`
	Teams     []protocol.RepositoryTeamGrant `json:"teams,omitempty"`
	CreatedAt string                         `json:"created_at,omitempty"`
	UpdatedAt string                         `json:"updated_at,omitempty"`
}
type Owners struct {
	Keys []protocol.Key `json:"keys"`
}
type RepositoryIndex struct {
	Repos []protocol.Repository `json:"repos"`
}

func (b *Broker) LoadRepositoryState(ctx context.Context, repo protocol.Repository) (RepositoryState, error) {
	var state RepositoryState
	err := b.LoadJSON(ctx, repo, repositoryStatePath, &state)
	return state, err
}
func (b *Broker) SaveRepositoryState(ctx context.Context, state RepositoryState) error {
	if err := b.SaveJSON(ctx, state.Repo, repositoryStatePath, state); err != nil {
		return err
	}
	return b.UpsertRepository(ctx, state.Repo)
}

func (b *Broker) LoadRepositoryIndex(ctx context.Context) (RepositoryIndex, error) {
	backend, err := fsstore.New(b.root)
	if err != nil {
		return RepositoryIndex{}, err
	}
	var index RepositoryIndex
	data, err := backend.Read(ctx, "repos.json")
	if errors.Is(err, fs.ErrNotExist) {
		return index, nil
	}
	if err != nil {
		return index, err
	}
	if err := decodeJSON(data, &index); err != nil {
		return index, err
	}
	return index, nil
}
func (b *Broker) FindRepository(ctx context.Context, logical string) (protocol.Repository, bool, error) {
	index, err := b.LoadRepositoryIndex(ctx)
	if err != nil {
		return protocol.Repository{}, false, err
	}
	for _, repo := range index.Repos {
		if strings.EqualFold(strings.TrimSpace(repo.Logical), strings.TrimSpace(logical)) {
			return repo, true, nil
		}
	}
	return protocol.Repository{}, false, nil
}
func (b *Broker) UpsertRepository(ctx context.Context, repo protocol.Repository) error {
	if strings.TrimSpace(repo.Logical) == "" {
		return nil
	}
	unlock := b.LockRepository(protocol.Repository{Logical: "__index__"})
	defer unlock()
	backend, err := fsstore.New(b.root)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 16; attempt++ {
		var index RepositoryIndex
		expected, err := loadJSONState(ctx, backend, "repos.json", &index)
		if err != nil {
			return err
		}
		found := false
		for i := range index.Repos {
			if strings.EqualFold(strings.TrimSpace(index.Repos[i].Logical), strings.TrimSpace(repo.Logical)) {
				index.Repos[i] = repo
				found = true
				break
			}
		}
		if !found {
			index.Repos = append(index.Repos, repo)
		}
		sort.Slice(index.Repos, func(i, j int) bool { return index.Repos[i].Logical < index.Repos[j].Logical })
		if err := compareAndSwapJSON(ctx, backend, "repos.json", expected, index); errors.Is(err, store.ErrConflict) {
			continue
		} else {
			return err
		}
	}
	return store.ErrConflict
}

func loadJSONState(ctx context.Context, backend store.Writer, path string, target any) (store.ObjectState, error) {
	data, err := backend.Read(ctx, path)
	if errors.Is(err, fs.ErrNotExist) {
		return store.ObjectState{}, nil
	}
	if err != nil {
		return store.ObjectState{}, err
	}
	if err := decodeJSON(data, target); err != nil {
		return store.ObjectState{}, err
	}
	return store.ObjectState{Exists: true, Data: data}, nil
}

func compareAndSwapJSON(ctx context.Context, backend store.Writer, path string, expected store.ObjectState, value any) error {
	cas, ok := backend.(store.CompareAndSwapper)
	if !ok {
		return store.ErrNotSupported
	}
	data, err := encodeJSON(value)
	if err != nil {
		return err
	}
	return cas.CompareAndSwap(ctx, path, expected, data)
}
