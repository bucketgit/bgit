package local

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"strings"

	"github.com/bucketgit/bgit/protocol"
)

func (b *Broker) UpdateRef(ctx context.Context, request protocol.RefUpdateRequest, user string) error {
	unlock := b.LockRepository(request.Repo)
	defer unlock()
	state, err := b.LoadRepositoryState(ctx, request.Repo)
	if err != nil {
		return err
	}
	if err := b.Repository(state.Repo).CompareAndSwapRef(ctx, request.Ref, request.Old, request.New); err != nil {
		return err
	}
	if state.Refs == nil {
		state.Refs = map[string]string{}
	}
	recordPath := ".bucketgit/broker-state/v1/refs/" + base64.RawURLEncoding.EncodeToString([]byte(request.Ref)) + ".json"
	if IsZeroOID(request.New) {
		delete(state.Refs, request.Ref)
		_ = b.DeleteObject(ctx, state.Repo, recordPath)
	} else {
		state.Refs[request.Ref] = request.New
		record := map[string]any{"ref": request.Ref, "hash": request.New, "updated_by": first(user, "owner"), "updated_at": b.now().UTC().Format("2006-01-02T15:04:05Z07:00")}
		if err := b.SaveJSON(ctx, state.Repo, recordPath, record); err != nil {
			return err
		}
	}
	state.UpdatedAt = b.now().UTC().Format("2006-01-02T15:04:05Z07:00")
	return b.SaveRepositoryState(ctx, state)
}

// ReconcileRepository repairs the broker's cached ref map and per-ref records
// from the storage backend's authoritative Git refs. It is safe to call after
// an interrupted update or when another local-broker process updated a shared
// bucket.
func (b *Broker) ReconcileRepository(ctx context.Context, repo protocol.Repository, user string) (RepositoryState, error) {
	unlock := b.LockRepository(repo)
	defer unlock()
	state, err := b.LoadRepositoryState(ctx, repo)
	if err != nil {
		return RepositoryState{}, err
	}
	refs, err := b.Repository(state.Repo).ListRefs(ctx)
	if err != nil {
		return RepositoryState{}, err
	}
	recordPrefix := ".bucketgit/broker-state/v1/refs/"
	records, err := b.ListObjects(ctx, state.Repo, recordPrefix)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return RepositoryState{}, err
	}
	wanted := make(map[string]struct{}, len(refs))
	for ref, oid := range refs {
		recordPath := recordPrefix + base64.RawURLEncoding.EncodeToString([]byte(ref)) + ".json"
		wanted[recordPath] = struct{}{}
		record := map[string]any{"ref": ref, "hash": oid, "updated_by": first(user, "reconcile"), "updated_at": b.now().UTC().Format("2006-01-02T15:04:05Z07:00")}
		if err := b.SaveJSON(ctx, state.Repo, recordPath, record); err != nil {
			return RepositoryState{}, err
		}
	}
	for _, recordPath := range records {
		if !strings.HasPrefix(recordPath, recordPrefix) {
			continue
		}
		if _, ok := wanted[recordPath]; !ok {
			if err := b.DeleteObject(ctx, state.Repo, recordPath); err != nil {
				return RepositoryState{}, err
			}
		}
	}
	state.Refs = refs
	state.UpdatedAt = b.now().UTC().Format("2006-01-02T15:04:05Z07:00")
	if err := b.SaveRepositoryState(ctx, state); err != nil {
		return RepositoryState{}, err
	}
	return state, nil
}

func IsZeroOID(value string) bool {
	return value == "" || value == "0000000000000000000000000000000000000000"
}
