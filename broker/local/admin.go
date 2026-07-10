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

const ownersPath = "owners.json"

func (b *Broker) LoadOwners(ctx context.Context) (Owners, error) {
	backend, err := fsstore.New(b.root)
	if err != nil {
		return Owners{}, err
	}
	data, err := backend.Read(ctx, ownersPath)
	if errors.Is(err, fs.ErrNotExist) {
		return Owners{}, nil
	}
	if err != nil {
		return Owners{}, err
	}
	var owners Owners
	if err := decodeJSON(data, &owners); err != nil {
		return Owners{}, err
	}
	return owners, nil
}

func (b *Broker) UpsertOwners(ctx context.Context, request protocol.OwnerRequest) (Owners, error) {
	unlock := b.LockRepository(protocol.Repository{Logical: "__owners__"})
	defer unlock()
	backend, err := fsstore.New(b.root)
	if err != nil {
		return Owners{}, err
	}
	for attempt := 0; attempt < 16; attempt++ {
		var owners Owners
		expected, err := loadJSONState(ctx, backend, ownersPath, &owners)
		if err != nil {
			return Owners{}, err
		}
		for _, publicKey := range request.PublicKeys {
			publicKey = normalizePublicKey(publicKey)
			if publicKey == "" {
				continue
			}
			found := false
			for index := range owners.Keys {
				if normalizePublicKey(owners.Keys[index].PublicKey) == publicKey {
					owners.Keys[index].User = first(request.User, "owner")
					owners.Keys[index].Role = first(request.Role, "owner")
					found = true
				}
			}
			if !found {
				owners.Keys = append(owners.Keys, protocol.Key{User: first(request.User, "owner"), Role: first(request.Role, "owner"), PublicKey: publicKey})
			}
		}
		if err := compareAndSwapJSON(ctx, backend, ownersPath, expected, owners); errors.Is(err, store.ErrConflict) {
			continue
		} else if err != nil {
			return Owners{}, err
		}
		return owners, nil
	}
	return Owners{}, store.ErrConflict
}

// CreateRepository persists repository metadata after the caller has
// provisioned any provider bucket required by the selected storage backend.
func (b *Broker) CreateRepository(ctx context.Context, request protocol.RepositoryAdminRequest, ownerKeys []protocol.Key) (RepositoryState, error) {
	repo := request.Repo
	if err := repo.Validate(); err != nil {
		return RepositoryState{}, err
	}
	unlock := b.LockRepository(repo)
	defer unlock()
	if state, err := b.LoadRepositoryState(ctx, repo); err == nil && state.Repo.Logical != "" {
		return state, protocol.ErrConflict
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return RepositoryState{}, err
	}
	if existing, found, err := b.FindRepository(ctx, repo.Logical); err != nil {
		return RepositoryState{}, err
	} else if found {
		return RepositoryState{Repo: existing}, protocol.ErrConflict
	}
	now := b.now().UTC().Format("2006-01-02T15:04:05Z07:00")
	state := RepositoryState{Repo: repo, Refs: map[string]string{}, Teams: []protocol.RepositoryTeamGrant{{ID: "t_core", Role: first(request.Role, "developer")}}, CreatedAt: now, UpdatedAt: now}
	for _, key := range ownerKeys {
		key.PublicKey = normalizePublicKey(key.PublicKey)
		if key.PublicKey == "" {
			continue
		}
		key.User = first(request.User, key.User, "owner")
		key.Role = first(request.Role, "developer")
		state.Keys = append(state.Keys, key)
	}
	if err := b.CompareAndSwapJSON(ctx, state.Repo, repositoryStatePath, store.ObjectState{}, state); err != nil {
		return RepositoryState{}, err
	}
	if err := b.UpsertRepository(ctx, state.Repo); err != nil {
		return RepositoryState{}, err
	}
	return state, nil
}

func (b *Broker) GetRepository(ctx context.Context, repo protocol.Repository) (RepositoryState, error) {
	if strings.TrimSpace(repo.Logical) != "" {
		if indexed, ok, err := b.FindRepository(ctx, repo.Logical); err != nil {
			return RepositoryState{}, err
		} else if ok {
			repo = indexed
		}
	}
	return b.LoadRepositoryState(ctx, repo)
}

func (b *Broker) ListRepositories(ctx context.Context) ([]protocol.RepositoryInfo, error) {
	index, err := b.LoadRepositoryIndex(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]protocol.RepositoryInfo, 0, len(index.Repos))
	for _, repo := range index.Repos {
		state, err := b.LoadRepositoryState(ctx, repo)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		result = append(result, protocol.RepositoryInfo{Repo: repo, Logical: repo.Logical, Teams: state.Teams})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Repo.Logical < result[j].Repo.Logical })
	return result, nil
}

func normalizePublicKey(value string) string {
	parts := strings.Fields(strings.TrimSpace(value))
	if len(parts) >= 2 {
		return parts[0] + " " + parts[1]
	}
	return strings.TrimSpace(value)
}
