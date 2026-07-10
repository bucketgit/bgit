// Package setup contains cloud CLI execution and broker deployment
// orchestration. Interactive profile selection remains in internal/cli.
package setup

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

func GCloudCommand(ctx context.Context, configuration string, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, "gcloud", arguments...)
	if configuration = strings.TrimSpace(configuration); configuration != "" {
		command.Env = append(os.Environ(), "CLOUDSDK_ACTIVE_CONFIG_NAME="+configuration)
	}
	return command
}

func AWSCommand(ctx context.Context, profile string, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, "aws", arguments...)
	if profile = strings.TrimSpace(profile); profile != "" {
		command.Env = append(os.Environ(), "AWS_PROFILE="+profile, "AWS_SDK_LOAD_CONFIG=1")
	}
	return command
}

type RetryOptions struct {
	Attempts int
	Delay    time.Duration
	Retry    func(output []byte, err error) bool
	Sleep    func(time.Duration)
}

// RunWithRetry rebuilds a command for each attempt so callers never reuse an
// executed exec.Cmd. It is used for eventually consistent cloud IAM updates.
func RunWithRetry(build func() *exec.Cmd, options RetryOptions) ([]byte, error) {
	if options.Attempts <= 0 {
		options.Attempts = 1
	}
	if options.Sleep == nil {
		options.Sleep = time.Sleep
	}
	var output []byte
	var err error
	for attempt := 0; attempt < options.Attempts; attempt++ {
		output, err = build().CombinedOutput()
		if err == nil || options.Retry == nil || !options.Retry(output, err) || attempt == options.Attempts-1 {
			return output, err
		}
		options.Sleep(options.Delay)
	}
	return output, err
}
