package bgit_test

import (
	"context"
	"log"

	"github.com/bucketgit/bgit/repository"
	fsstore "github.com/bucketgit/bgit/store/fs"
)

func Example() {
	objects, err := fsstore.New("/srv/git/example.git")
	if err != nil {
		log.Fatal(err)
	}
	repo := repository.Open(objects, nil)
	_, _ = repo.Resolve(context.Background(), "main")
}
