package main

import (
	"embed"
	"os"
	"path/filepath"
	"strings"
)

//go:embed broker/gcp/package.json broker/gcp/index.js broker/aws/template.yaml
var brokerAssets embed.FS

func writeGCPBrokerSource(dir string) error {
	for _, name := range []string{"package.json", "index.js"} {
		data, err := brokerAssets.ReadFile("broker/gcp/" + name)
		if err != nil {
			return err
		}
		body := strings.ReplaceAll(string(data), "{{BROKER_VERSION}}", brokerVersion)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func awsBrokerCloudFormationTemplate() string {
	data, err := brokerAssets.ReadFile("broker/aws/template.yaml")
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(data), "{{BROKER_VERSION}}", brokerVersion)
}
