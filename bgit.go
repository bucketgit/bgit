// Package bgit provides the compatibility entry points used by the bgit
// executable. Reusable repository functionality lives in the protocol, store,
// repository, broker, and transport packages.
package bgit

import (
	"embed"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/bucketgit/bgit/internal/app"
)

//go:embed CHANGELOG.md
var embeddedChangelog string

//go:embed broker/gcp/package.json broker/gcp/index.js broker/gcp/materializer.js broker/aws/template.yaml broker/test_support/sqlite_broker.js
var brokerAssets embed.FS

var version = ""
var configureOnce sync.Once

func configureApplication() {
	configureOnce.Do(func() {
		app.ConfigureRuntime(app.RuntimeOptions{Version: version, Changelog: embeddedChangelog})
		app.ConfigureBrokerAssets(brokerAssets)
	})
}

func Main() {
	if err := RunExecutable(os.Args[0], os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func RunExecutable(executable string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	configureApplication()
	return app.RunExecutable(executable, args, stdin, stdout, stderr)
}

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	configureApplication()
	return app.Run(args, stdin, stdout, stderr)
}
