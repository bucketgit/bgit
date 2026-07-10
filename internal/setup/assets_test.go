package setup

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestDeploymentAssetsSubstituteVersion(t *testing.T) {
	assets := fstest.MapFS{
		"gcp/package.json":    {Data: []byte(`{"version":"{{BROKER_VERSION}}"}`)},
		"gcp/index.js":        {Data: []byte(`exports.version = "{{BROKER_VERSION}}"`)},
		"gcp/materializer.js": {Data: []byte(`exports.materializer = "{{BROKER_VERSION}}"`)},
		"aws/template.yaml":   {Data: []byte(`Version: {{BROKER_VERSION}}`)},
	}
	destination := t.TempDir()
	if err := WriteGCPBrokerSource(assets, "gcp", destination, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "index.js"))
	if err != nil || string(data) != `exports.version = "1.2.3"` {
		t.Fatalf("data=%q err=%v", data, err)
	}
	template, err := AWSTemplate(assets, "aws/template.yaml", "1.2.3")
	if err != nil || template != "Version: 1.2.3" {
		t.Fatalf("template=%q err=%v", template, err)
	}
}
