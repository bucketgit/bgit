// Package gcs implements a BucketGit object store backed by Google Cloud Storage.
package gcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/bucketgit/bgit/store"
	"google.golang.org/api/option"
)

type ObjectAttrs struct{ Generation int64 }

type API interface {
	Read(context.Context, string, string, int64) ([]byte, error)
	List(context.Context, string, string) ([]string, error)
	Write(context.Context, string, string, []byte, *int64) error
	Delete(context.Context, string, string, *int64) error
	Attrs(context.Context, string, string) (ObjectAttrs, error)
}

type Store struct {
	client API
	bucket string
	prefix string
	close  func() error
}

type Options struct {
	Bucket          string
	Prefix          string
	CredentialsFile string
}

func Load(ctx context.Context, options Options) (*Store, error) {
	var clientOptions []option.ClientOption
	if path := strings.TrimSpace(options.CredentialsFile); path != "" {
		clientOptions = append(clientOptions, option.WithCredentialsFile(path))
	}
	client, err := storage.NewClient(ctx, clientOptions...)
	if err != nil {
		return nil, fmt.Errorf("create GCS client: %w", err)
	}
	backend, err := New(client, options.Bucket, options.Prefix)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	backend.close = client.Close
	return backend, nil
}

func (s *Store) Close() error {
	if s == nil || s.close == nil {
		return nil
	}
	return s.close()
}

func New(client *storage.Client, bucket, prefix string) (*Store, error) {
	if client == nil {
		return nil, fmt.Errorf("GCS client is required")
	}
	return NewWithAPI(&storageAPI{client: client}, bucket, prefix)
}

func NewWithAPI(client API, bucket, prefix string) (*Store, error) {
	if client == nil {
		return nil, fmt.Errorf("GCS client is required")
	}
	bucket = strings.TrimSpace(bucket)
	if bucket == "" {
		return nil, fmt.Errorf("GCS bucket is required")
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix != "" {
		if _, err := store.ValidatePath(prefix, false); err != nil {
			return nil, fmt.Errorf("GCS prefix: %w", err)
		}
	}
	return &Store{client: client, bucket: bucket, prefix: prefix}, nil
}

func (s *Store) Read(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := s.objectName(path, false)
	if err != nil {
		return nil, err
	}
	data, err := s.client.Read(ctx, s.bucket, name, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, storage.ErrObjectNotExist) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, s.accessError("read", name, err)
	}
	return data, nil
}
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query, err := s.objectName(prefix, true)
	if err != nil {
		return nil, err
	}
	if query != "" && !strings.HasSuffix(query, "/") {
		query += "/"
	}
	names, err := s.client.List(ctx, s.bucket, query)
	if err != nil {
		return nil, s.accessError("list", query, err)
	}
	var paths []string
	for _, name := range names {
		rel := strings.TrimPrefix(name, objectPrefix(s.prefix))
		if rel != "" && !strings.HasSuffix(rel, "/") {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths)
	return paths, nil
}
func (s *Store) Write(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, err := s.objectName(path, false)
	if err != nil {
		return err
	}
	if err := s.client.Write(ctx, s.bucket, name, data, nil); err != nil {
		return s.accessError("write", name, err)
	}
	return nil
}
func (s *Store) Delete(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, err := s.objectName(path, false)
	if err != nil {
		return err
	}
	err = s.client.Delete(ctx, s.bucket, name, nil)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	if err != nil {
		return s.accessError("delete", name, err)
	}
	return nil
}
func (s *Store) ListRefs(ctx context.Context) (map[string]string, error) {
	return store.ReadRefs(ctx, s)
}

func (s *Store) CompareAndSwapRef(ctx context.Context, ref, oldOID, newOID string) error {
	if err := store.ValidateRefUpdate(ref, oldOID, newOID); err != nil {
		return err
	}
	name, err := s.objectName(ref, false)
	if err != nil {
		return err
	}
	attrs, err := s.client.Attrs(ctx, s.bucket, name)
	notFound := errors.Is(err, fs.ErrNotExist) || errors.Is(err, storage.ErrObjectNotExist)
	if err != nil && !notFound {
		return s.accessError("read ref attributes", name, err)
	}
	current := ""
	if !notFound {
		data, err := s.client.Read(ctx, s.bucket, name, attrs.Generation)
		if err != nil {
			return s.accessError("read ref", name, err)
		}
		current = strings.TrimSpace(string(data))
	}
	if !(store.IsZeroOID(oldOID) && notFound) && current != oldOID {
		return store.ErrConflict
	}
	generation := attrs.Generation
	if notFound {
		generation = -1
	}
	if store.IsZeroOID(newOID) {
		if notFound {
			return nil
		}
		err = s.client.Delete(ctx, s.bucket, name, &generation)
	} else {
		err = s.client.Write(ctx, s.bucket, name, []byte(newOID+"\n"), &generation)
	}
	if errors.Is(err, store.ErrConflict) {
		return store.ErrConflict
	}
	if err != nil {
		return s.accessError("update ref", name, err)
	}
	return nil
}

func (s *Store) CompareAndSwap(ctx context.Context, path string, expected store.ObjectState, replacement []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, err := s.objectName(path, false)
	if err != nil {
		return err
	}
	attrs, err := s.client.Attrs(ctx, s.bucket, name)
	notFound := errors.Is(err, fs.ErrNotExist) || errors.Is(err, storage.ErrObjectNotExist)
	if err != nil && !notFound {
		return s.accessError("read conditional object attributes", name, err)
	}
	var current []byte
	if !notFound {
		current, err = s.client.Read(ctx, s.bucket, name, attrs.Generation)
		if err != nil {
			return s.accessError("read conditional object", name, err)
		}
	}
	if (!notFound) != expected.Exists || !notFound && !bytes.Equal(current, expected.Data) {
		return store.ErrConflict
	}
	generation := attrs.Generation
	if notFound {
		generation = -1
	}
	if err := s.client.Write(ctx, s.bucket, name, replacement, &generation); errors.Is(err, store.ErrConflict) {
		return store.ErrConflict
	} else if err != nil {
		return s.accessError("replace conditional object", name, err)
	}
	return nil
}

func (s *Store) objectName(value string, allowEmpty bool) (string, error) {
	rel, err := store.ValidatePath(value, allowEmpty)
	if err != nil {
		return "", err
	}
	if s.prefix == "" {
		return rel, nil
	}
	if rel == "" {
		return s.prefix, nil
	}
	return s.prefix + "/" + rel, nil
}
func (s *Store) accessError(action, object string, err error) error {
	return fmt.Errorf("%s gs://%s/%s: %w", action, s.bucket, object, err)
}
func objectPrefix(value string) string {
	value = strings.Trim(value, "/")
	if value == "" {
		return ""
	}
	return value + "/"
}

var _ store.RefStore = (*Store)(nil)
var _ store.CompareAndSwapper = (*Store)(nil)
