package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"cloud.google.com/go/compute/metadata"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"golang.org/x/oauth2/google"
)

var awsCallerIdentitySDK = awsCallerIdentityFromSDK
var gcpDefaultProjectID = gcpDefaultProjectIDFromADC

func awsCallerIdentityFromSDK(ctx context.Context, profile string) (string, string) {
	probeCtx, cancel := context.WithTimeout(ctx, setupProbeTimeout)
	defer cancel()
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithDefaultRegion(defaultAWSRegion()),
		awsconfig.WithRegion(defaultAWSRegion()),
	}
	if strings.TrimSpace(profile) != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(strings.TrimSpace(profile)))
	}
	cfg, err := awsconfig.LoadDefaultConfig(probeCtx, opts...)
	if err != nil {
		return "", ""
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(probeCtx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", ""
	}
	return aws.ToString(out.Account), aws.ToString(out.Arn)
}

func gcpDefaultProjectIDFromADC(ctx context.Context) string {
	for _, key := range []string{"GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, setupProbeTimeout)
	defer cancel()
	if creds, err := google.FindDefaultCredentials(probeCtx); err == nil {
		if projectID := strings.TrimSpace(creds.ProjectID); projectID != "" {
			return projectID
		}
	}
	if projectID := gcpADCProjectIDFromFile(); projectID != "" {
		return projectID
	}
	if metadata.OnGCE() {
		if projectID, err := metadata.ProjectIDWithContext(probeCtx); err == nil {
			return strings.TrimSpace(projectID)
		}
	}
	return ""
}

func gcpADCProjectIDFromFile() string {
	for _, path := range gcpADCConfigPaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg struct {
			ProjectID      string `json:"project_id"`
			QuotaProjectID string `json:"quota_project_id"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			continue
		}
		if projectID := strings.TrimSpace(cfg.ProjectID); projectID != "" {
			return projectID
		}
		if projectID := strings.TrimSpace(cfg.QuotaProjectID); projectID != "" {
			return projectID
		}
	}
	return ""
}

func gcpADCConfigPaths() []string {
	var paths []string
	if path := strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")); path != "" {
		paths = append(paths, path)
	}
	if configDir := strings.TrimSpace(os.Getenv("CLOUDSDK_CONFIG")); configDir != "" {
		paths = append(paths, filepath.Join(configDir, "application_default_credentials.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "gcloud", "application_default_credentials.json"))
	}
	return uniqueStrings(paths)
}
