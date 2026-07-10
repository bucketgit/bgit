// Package web owns the embedded assets and presentation primitives for the
// local BucketGit web application. Repository operations are supplied by the
// application layer rather than imported from the CLI.
package web

import "embed"

//go:embed www/*
var assets embed.FS

func ReadAsset(path string) ([]byte, error) {
	return assets.ReadFile(path)
}
