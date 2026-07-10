package setup

import (
	"context"
	"os/exec"
)

type GCPFunctionDeployment struct {
	Configuration  string
	Name           string
	Region         string
	Source         string
	EntryPoint     string
	ServiceAccount string
	Environment    string
	Public         bool
}

func GCPFunctionDeployCommand(ctx context.Context, deployment GCPFunctionDeployment) *exec.Cmd {
	access := "--no-allow-unauthenticated"
	if deployment.Public {
		access = "--allow-unauthenticated"
	}
	return GCloudCommand(ctx, deployment.Configuration,
		"functions", "deploy", deployment.Name,
		"--gen2",
		"--runtime", "nodejs22",
		"--region", deployment.Region,
		"--source", deployment.Source,
		"--entry-point", deployment.EntryPoint,
		"--trigger-http",
		access,
		"--service-account", deployment.ServiceAccount,
		"--set-env-vars", deployment.Environment,
		"--quiet",
	)
}

type AWSStackDeployment struct {
	Profile            string
	Region             string
	StackName          string
	TemplatePath       string
	ArtifactBucket     string
	ParameterOverrides []string
	Capability         string
}

func AWSStackDeployCommand(ctx context.Context, deployment AWSStackDeployment) *exec.Cmd {
	arguments := []string{
		"cloudformation", "deploy",
		"--stack-name", deployment.StackName,
		"--template-file", deployment.TemplatePath,
		"--s3-bucket", deployment.ArtifactBucket,
		"--capabilities", deployment.Capability,
		"--region", deployment.Region,
	}
	if len(deployment.ParameterOverrides) > 0 {
		arguments = append(arguments, "--parameter-overrides")
		arguments = append(arguments, deployment.ParameterOverrides...)
	}
	return AWSCommand(ctx, deployment.Profile, arguments...)
}
