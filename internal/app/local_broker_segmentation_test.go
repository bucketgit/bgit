package app

import (
	"errors"
	"testing"

	"github.com/bucketgit/bgit/store"
)

func TestLocalBrokerStoreAdapterProvidesMetadataCAS(t *testing.T) {
	adapter := localBrokerStoreAdapter{store: &localGitStore{root: t.TempDir()}}
	path := ".bucketgit/broker-state/v1/issues.json"
	if err := adapter.CompareAndSwap(t.Context(), path, store.ObjectState{}, []byte(`{"next_id":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := adapter.CompareAndSwap(t.Context(), path, store.ObjectState{}, []byte(`{"next_id":2}`)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale metadata write error=%v", err)
	}
}
