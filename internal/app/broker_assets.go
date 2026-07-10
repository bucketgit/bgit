package app

import (
	"io/fs"

	internalsetup "github.com/bucketgit/bgit/internal/setup"
)

type assetFileSystem struct{ fs.FS }

func (a assetFileSystem) ReadFile(name string) ([]byte, error) {
	return fs.ReadFile(a.FS, name)
}

var brokerAssets assetFileSystem

func ConfigureBrokerAssets(assets fs.FS) {
	brokerAssets = assetFileSystem{FS: assets}
}

func writeGCPBrokerSource(dir string) error {
	return internalsetup.WriteGCPBrokerSource(brokerAssets, "broker/gcp", dir, brokerVersion())
}

func writeGCPMaterializerSource(dir string) error {
	return internalsetup.WriteGCPMaterializerSource(brokerAssets, "broker/gcp", dir, brokerVersion())
}

func awsBrokerCloudFormationTemplate() string {
	template, err := internalsetup.AWSTemplate(brokerAssets, "broker/aws/template.yaml", brokerVersion())
	if err != nil {
		return ""
	}
	return template
}
