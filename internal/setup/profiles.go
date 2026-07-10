package setup

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultProbeTimeout = 10 * time.Second

type Profile struct {
	Provider          string
	Name              string
	Active            bool
	Existing          bool
	Account           string
	ProjectID         string
	AccountID         string
	ARN               string
	Region            string
	ConfiguredRegions []string
}

type ProfileDiscoveryOptions struct {
	LookPath     func(string) (string, error)
	Command      func(context.Context, string, ...string) *exec.Cmd
	HomeDir      func() (string, error)
	ProbeTimeout time.Duration
}

func (o ProfileDiscoveryOptions) withDefaults() ProfileDiscoveryOptions {
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Command == nil {
		o.Command = exec.CommandContext
	}
	if o.HomeDir == nil {
		o.HomeDir = os.UserHomeDir
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = defaultProbeTimeout
	}
	return o
}

func DiscoverProfiles(ctx context.Context, options ProfileDiscoveryOptions) ([]Profile, error) {
	options = options.withDefaults()
	gcp, err := DiscoverGCPProfiles(ctx, options)
	if err != nil {
		return nil, err
	}
	aws, err := DiscoverAWSProfiles(options)
	if err != nil {
		return nil, err
	}
	return append(gcp, aws...), nil
}

func DiscoverGCPProfiles(ctx context.Context, options ProfileDiscoveryOptions) ([]Profile, error) {
	options = options.withDefaults()
	if _, err := options.LookPath("gcloud"); err != nil {
		return nil, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, options.ProbeTimeout)
	defer cancel()
	out, err := options.Command(probeCtx, "gcloud", "config", "configurations", "list", "--format=value(name,is_active)").Output()
	if err != nil {
		return nil, nil
	}
	var profiles []Profile
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		profiles = append(profiles, Profile{
			Provider:  "gcs",
			Name:      name,
			Active:    len(fields) > 1 && strings.EqualFold(fields[1], "true"),
			Account:   GCloudConfigValue(ctx, name, "account", options),
			ProjectID: GCloudConfigValue(ctx, name, "project", options),
			Region: firstNonEmpty(
				GCloudConfigValue(ctx, name, "run/region", options),
				GCloudConfigValue(ctx, name, "functions/region", options),
				"us-central1",
			),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return profiles, nil
}

func GCloudConfigValue(ctx context.Context, profile, key string, options ProfileDiscoveryOptions) string {
	options = options.withDefaults()
	probeCtx, cancel := context.WithTimeout(ctx, options.ProbeTimeout)
	defer cancel()
	out, err := options.Command(probeCtx, "gcloud", "--configuration", profile, "config", "get-value", key, "--quiet").Output()
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(out))
	if value == "(unset)" {
		return ""
	}
	return value
}

func DiscoverAWSProfiles(options ProfileDiscoveryOptions) ([]Profile, error) {
	options = options.withDefaults()
	names := AWSProfilesFromFiles(options)
	profiles := make([]Profile, 0, len(names))
	for _, name := range names {
		profiles = append(profiles, Profile{Provider: "s3", Name: name, Region: ConfiguredAWSProfileRegion(name, options)})
	}
	return profiles, nil
}

func AWSProfilesFromFiles(options ProfileDiscoveryOptions) []string {
	options = options.withDefaults()
	home, err := options.HomeDir()
	if err != nil {
		return nil
	}
	names := map[string]struct{}{}
	for _, path := range []string{filepath.Join(home, ".aws", "config"), filepath.Join(home, ".aws", "credentials")} {
		for _, name := range ParseAWSProfileFile(path) {
			names[name] = struct{}{}
		}
	}
	return sortedKeys(names)
}

func ParseAWSProfileFile(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	names := map[string]struct{}{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"), "profile "))
		if name != "" {
			names[name] = struct{}{}
		}
	}
	return sortedKeys(names)
}

func ConfiguredAWSProfileRegion(profile string, options ProfileDiscoveryOptions) string {
	options = options.withDefaults()
	home, err := options.HomeDir()
	if err != nil {
		return ""
	}
	for _, path := range []string{filepath.Join(home, ".aws", "config"), filepath.Join(home, ".aws", "credentials")} {
		if region := AWSProfileFileValue(path, profile, "region"); region != "" {
			return region
		}
	}
	return ""
}

func AWSProfileFileValue(path, profile, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	section := ""
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"), "profile "))
			continue
		}
		if section != profile {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(name) == key {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
