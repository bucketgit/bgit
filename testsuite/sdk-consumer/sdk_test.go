package consumer

import (
	"bytes"
	"context"
	"testing"

	localbroker "github.com/bucketgit/bgit/broker/local"
	"github.com/bucketgit/bgit/protocol"
	"github.com/bucketgit/bgit/repository"
	fsstore "github.com/bucketgit/bgit/store/fs"
	"github.com/bucketgit/bgit/transport"
)

func TestExternalModuleCanComposeFilesystemLocalBrokerAndTransport(t *testing.T) {
	objects, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = repository.Open(objects, objects)

	broker, err := localbroker.New(localbroker.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	repoInfo := protocol.Repository{Provider: "file", Bucket: "demo.git", Logical: "demo.git"}
	scoped := broker.Repository(repoInfo)
	repo := repository.Open(scoped, scoped)
	var input, output bytes.Buffer
	_ = transport.ServeUploadPack(context.Background(), repo, &input, &output)
}
