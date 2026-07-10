package gcs

import (
	"context"
	"errors"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/bucketgit/bgit/store"
	"github.com/bucketgit/bgit/store/storetest"
)

type fakeObject struct {
	data       []byte
	generation int64
}
type fakeGCS struct {
	mu      sync.Mutex
	next    int64
	objects map[string]fakeObject
}

func (f *fakeGCS) Read(_ context.Context, _, name string, generation int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.objects[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	if generation > 0 && object.generation != generation {
		return nil, store.ErrConflict
	}
	return append([]byte(nil), object.data...), nil
}
func (f *fakeGCS) List(_ context.Context, _, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for name := range f.objects {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}
func (f *fakeGCS) Write(_ context.Context, _, name string, data []byte, generation *int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, exists := f.objects[name]
	if generation != nil {
		if *generation < 0 && exists {
			return store.ErrConflict
		}
		if *generation >= 0 && (!exists || current.generation != *generation) {
			return store.ErrConflict
		}
	}
	f.next++
	f.objects[name] = fakeObject{data: append([]byte(nil), data...), generation: f.next}
	return nil
}
func (f *fakeGCS) Delete(_ context.Context, _, name string, generation *int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, exists := f.objects[name]
	if !exists {
		return fs.ErrNotExist
	}
	if generation != nil && current.generation != *generation {
		return store.ErrConflict
	}
	delete(f.objects, name)
	return nil
}
func (f *fakeGCS) Attrs(_ context.Context, _, name string) (ObjectAttrs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.objects[name]
	if !ok {
		return ObjectAttrs{}, fs.ErrNotExist
	}
	return ObjectAttrs{Generation: object.generation}, nil
}

func TestStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Writer {
		backend, err := NewWithAPI(&fakeGCS{objects: map[string]fakeObject{}}, "bucket", "repo.git")
		if err != nil {
			t.Fatal(err)
		}
		return backend
	})
}

func TestCompareAndSwapContract(t *testing.T) {
	storetest.RunCompareAndSwap(t, func(t *testing.T) store.Writer {
		backend, err := NewWithAPI(&fakeGCS{objects: map[string]fakeObject{}}, "bucket", "repo.git")
		if err != nil {
			t.Fatal(err)
		}
		return backend
	})
}

func TestNewValidatesOptions(t *testing.T) {
	if _, err := New(nil, "bucket", "repo.git"); err == nil {
		t.Fatal("expected nil client error")
	}
	client := new(storage.Client)
	if _, err := New(client, "", "repo.git"); err == nil {
		t.Fatal("expected empty bucket error")
	}
	if _, err := New(client, "bucket", "../repo.git"); !errors.Is(err, store.ErrInvalidPath) {
		t.Fatalf("prefix error = %v", err)
	}
}

func TestCompareAndSwapRef(t *testing.T) {
	backend, err := NewWithAPI(&fakeGCS{objects: map[string]fakeObject{}}, "bucket", "repo.git")
	if err != nil {
		t.Fatal(err)
	}
	one := "0123456789abcdef0123456789abcdef01234567"
	two := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := backend.CompareAndSwapRef(t.Context(), "refs/heads/main", "", one); err != nil {
		t.Fatal(err)
	}
	if err := backend.CompareAndSwapRef(t.Context(), "refs/heads/main", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", two); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale=%v", err)
	}
	if err := backend.CompareAndSwapRef(t.Context(), "refs/heads/main", one, two); err != nil {
		t.Fatal(err)
	}
}
