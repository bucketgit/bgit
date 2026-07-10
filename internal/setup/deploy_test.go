package setup

import (
	"context"
	"reflect"
	"testing"
)

func TestGCPFunctionDeployCommand(t *testing.T) {
	command := GCPFunctionDeployCommand(context.Background(), GCPFunctionDeployment{
		Configuration: "work", Name: "bgit-broker", Region: "europe-west1",
		Source: "/tmp/source", EntryPoint: "broker", ServiceAccount: "broker@example.iam.gserviceaccount.com",
		Environment: "BROKER_VERSION=1.0.0", Public: true,
	})
	want := []string{"gcloud", "functions", "deploy", "bgit-broker", "--gen2", "--runtime", "nodejs22", "--region", "europe-west1", "--source", "/tmp/source", "--entry-point", "broker", "--trigger-http", "--allow-unauthenticated", "--service-account", "broker@example.iam.gserviceaccount.com", "--set-env-vars", "BROKER_VERSION=1.0.0", "--quiet"}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("args=%q\nwant=%q", command.Args, want)
	}
}

func TestAWSStackDeployCommand(t *testing.T) {
	command := AWSStackDeployCommand(context.Background(), AWSStackDeployment{Profile: "work", Region: "eu-west-1", StackName: "bgit-broker", TemplatePath: "/tmp/template.yaml", ArtifactBucket: "artifacts", Capability: "CAPABILITY_NAMED_IAM", ParameterOverrides: []string{"OwnerBootstrapHash=hash"}})
	want := []string{"aws", "cloudformation", "deploy", "--stack-name", "bgit-broker", "--template-file", "/tmp/template.yaml", "--s3-bucket", "artifacts", "--capabilities", "CAPABILITY_NAMED_IAM", "--region", "eu-west-1", "--parameter-overrides", "OwnerBootstrapHash=hash"}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("args=%q\nwant=%q", command.Args, want)
	}
}
