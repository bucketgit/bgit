package gcs

import (
	"context"
	"errors"
	"io"
	"sort"

	"cloud.google.com/go/storage"
	"github.com/bucketgit/bgit/store"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

type storageAPI struct{ client *storage.Client }

func (a *storageAPI) Read(ctx context.Context, bucket, name string, generation int64) ([]byte, error) {
	object := a.client.Bucket(bucket).Object(name)
	if generation > 0 {
		object = object.Generation(generation)
	}
	reader, err := object.NewReader(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	defer reader.Close()
	return io.ReadAll(reader)
}
func (a *storageAPI) List(ctx context.Context, bucket, prefix string) ([]string, error) {
	it := a.client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	var names []string
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, translateError(err)
		}
		names = append(names, attrs.Name)
	}
	sort.Strings(names)
	return names, nil
}
func (a *storageAPI) Write(ctx context.Context, bucket, name string, data []byte, generation *int64) error {
	object := a.client.Bucket(bucket).Object(name)
	if generation != nil {
		conditions := storage.Conditions{GenerationMatch: *generation}
		if *generation < 0 {
			conditions = storage.Conditions{DoesNotExist: true}
		}
		object = object.If(conditions)
	}
	writer := object.NewWriter(ctx)
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return translateError(err)
	}
	return translateError(writer.Close())
}
func (a *storageAPI) Delete(ctx context.Context, bucket, name string, generation *int64) error {
	object := a.client.Bucket(bucket).Object(name)
	if generation != nil {
		object = object.If(storage.Conditions{GenerationMatch: *generation})
	}
	return translateError(object.Delete(ctx))
}
func (a *storageAPI) Attrs(ctx context.Context, bucket, name string) (ObjectAttrs, error) {
	attrs, err := a.client.Bucket(bucket).Object(name).Attrs(ctx)
	if err != nil {
		return ObjectAttrs{}, translateError(err)
	}
	return ObjectAttrs{Generation: attrs.Generation}, nil
}
func translateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return storage.ErrObjectNotExist
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) && (apiErr.Code == 409 || apiErr.Code == 412) {
		return store.ErrConflict
	}
	return err
}
