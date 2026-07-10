package setup

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func WriteGCPBrokerSource(assets fs.FS, sourceRoot, destination, version string) error {
	return writeNodeSource(assets, sourceRoot, destination, version, map[string]string{"package.json": "package.json", "index.js": "index.js"})
}

func WriteGCPMaterializerSource(assets fs.FS, sourceRoot, destination, version string) error {
	return writeNodeSource(assets, sourceRoot, destination, version, map[string]string{"package.json": "package.json", "materializer.js": "index.js"})
}

func AWSTemplate(assets fs.FS, path, version string) (string, error) {
	data, err := fs.ReadFile(assets, path)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(data), "{{BROKER_VERSION}}", version), nil
}

func writeNodeSource(assets fs.FS, sourceRoot, destination, version string, files map[string]string) error {
	if strings.TrimSpace(destination) == "" {
		return fmt.Errorf("deployment source destination is required")
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	for source, target := range files {
		data, err := fs.ReadFile(assets, filepath.ToSlash(filepath.Join(sourceRoot, source)))
		if err != nil {
			return err
		}
		body := strings.ReplaceAll(string(data), "{{BROKER_VERSION}}", version)
		if err := os.WriteFile(filepath.Join(destination, target), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}
