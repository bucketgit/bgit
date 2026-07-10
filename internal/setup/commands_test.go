package setup

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestCloudCommandsApplyProfileEnvironment(t *testing.T) {
	gcloud := GCloudCommand(context.Background(), "work", "projects", "list")
	aws := AWSCommand(context.Background(), "prod", "sts", "get-caller-identity")
	if !contains(gcloud.Env, "CLOUDSDK_ACTIVE_CONFIG_NAME=work") {
		t.Fatalf("gcloud env=%v", gcloud.Env)
	}
	if !contains(aws.Env, "AWS_PROFILE=prod") || !contains(aws.Env, "AWS_SDK_LOAD_CONFIG=1") {
		t.Fatalf("aws env=%v", aws.Env)
	}
}

func TestRunWithRetryRebuildsCommands(t *testing.T) {
	attempts := 0
	var sleeps []time.Duration
	_, err := RunWithRetry(func() *exec.Cmd {
		attempts++
		command := exec.Command(os.Args[0], "-test.run=TestSetupHelperProcess", "--")
		command.Env = append(os.Environ(), "BGIT_SETUP_HELPER=1")
		return command
	}, RetryOptions{Attempts: 3, Delay: time.Second, Retry: func([]byte, error) bool { return true }, Sleep: func(delay time.Duration) { sleeps = append(sleeps, delay) }})
	if err == nil || attempts != 3 || !reflect.DeepEqual(sleeps, []time.Duration{time.Second, time.Second}) {
		t.Fatalf("attempts=%d sleeps=%v err=%v", attempts, sleeps, err)
	}
}

func TestSetupHelperProcess(t *testing.T) {
	if os.Getenv("BGIT_SETUP_HELPER") != "1" {
		return
	}
	os.Exit(1)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
