package bgit_test

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"

	"github.com/bucketgit/bgit/broker/capability"
	brokerclient "github.com/bucketgit/bgit/broker/client"
	localbroker "github.com/bucketgit/bgit/broker/local"
	"github.com/bucketgit/bgit/protocol"
	"github.com/bucketgit/bgit/repository"
	"github.com/bucketgit/bgit/store"
	gcsstore "github.com/bucketgit/bgit/store/gcs"
	s3store "github.com/bucketgit/bgit/store/s3"
	"github.com/bucketgit/bgit/transport"
)

func Example_s3Repository() {
	ctx := context.Background()
	objects, _ := s3store.Load(ctx, s3store.Options{Bucket: "123456789012-demo", Profile: "work", Region: "eu-west-1"})
	_ = repository.Open(objects, objects)
}

func Example_gcsRepository() {
	ctx := context.Background()
	objects, _ := gcsstore.Load(ctx, gcsstore.Options{Bucket: "project-demo"})
	if objects != nil {
		defer objects.Close()
	}
	_ = repository.Open(objects, objects)
}

func Example_remoteBrokerRepository() {
	ctx := context.Background()
	signatures := brokerclient.SignatureProviderFunc(func(context.Context, string, string, []byte) ([]http.Header, error) {
		return []http.Header{{}}, nil
	})
	client, _ := brokerclient.New("https://broker.example.com", brokerclient.Options{HTTPClient: http.DefaultClient, Signatures: signatures})
	repoInfo := protocol.Repository{Provider: "gcp", Bucket: "project-demo", Prefix: "repo.git", Logical: "demo.git"}
	objects, _ := capability.New(client, repoInfo, capability.Options{HTTPClient: http.DefaultClient})
	_ = repository.Open(objects, objects)
	_ = ctx
}

func Example_inProcessBroker() {
	controlPlane, _ := localbroker.New(localbroker.Options{Root: "/srv/bgit"})
	repoInfo := protocol.Repository{Provider: "file", Bucket: "demo.git", Logical: "demo.git"}
	objects := controlPlane.Repository(repoInfo)
	_ = repository.Open(objects, objects)
}

func Example_transportServer() {
	ctx := context.Background()
	objects := &exampleStore{objects: map[string][]byte{}, refs: map[string]string{}}
	repo := repository.Open(objects, objects)
	var input, output bytes.Buffer
	_ = transport.ServeUploadPack(ctx, repo, &input, &output)
	_ = transport.ServeReceivePack(ctx, repo, objects, &input, &output)
}

func Example_customStore() {
	objects := &exampleStore{objects: map[string][]byte{}, refs: map[string]string{}}
	_ = repository.Open(objects, objects)
}

type exampleStore struct {
	objects map[string][]byte
	refs    map[string]string
}

func (s *exampleStore) Read(_ context.Context, path string) ([]byte, error) {
	value, ok := s.objects[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), value...), nil
}
func (s *exampleStore) List(context.Context, string) ([]string, error) { return nil, nil }
func (s *exampleStore) Write(_ context.Context, path string, value []byte) error {
	s.objects[path] = append([]byte(nil), value...)
	return nil
}
func (s *exampleStore) Delete(_ context.Context, path string) error {
	delete(s.objects, path)
	return nil
}
func (s *exampleStore) ListRefs(context.Context) (map[string]string, error) {
	result := map[string]string{}
	for ref, oid := range s.refs {
		result[ref] = oid
	}
	return result, nil
}
func (s *exampleStore) CompareAndSwapRef(_ context.Context, ref, oldOID, newOID string) error {
	if s.refs[ref] != oldOID {
		return store.ErrConflict
	}
	if newOID == "" {
		delete(s.refs, ref)
	} else {
		s.refs[ref] = newOID
	}
	return nil
}
