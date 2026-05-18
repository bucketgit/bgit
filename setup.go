package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"
)

const setupProbeTimeout = 2 * time.Second
const setupDialogProfilesPerProvider = 10
const setupRegionDialogItemsPerPage = 10

var errSetupBack = errors.New("setup back")
var setupProfileNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.@+-]+$`)
var setupAWSAccessKeyPattern = regexp.MustCompile(`^(A3T[A-Z0-9]|AKIA|ASIA)[A-Z0-9]{16}$`)
var setupAWSRegionPattern = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-[0-9]+$`)

type setupOptions struct {
	yes        bool
	provider   string
	profiles   []string
	configPath string
	region     string
	keys       []string
	noAgent    bool
}

type setupProfile struct {
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

type setupSSHKey struct {
	PublicKey string
	Source    string
	Comment   string
}

type setupSelection struct {
	Profiles                []setupProfile
	Keys                    []setupSSHKey
	IdentityName            string
	IdentityEmail           string
	Action                  string
	CreateProvider          string
	CreateName              string
	CreateAccessKey         string
	CreateSecretKey         string
	CreateRegion            string
	DefaultCreate           string
	DefaultCreateByProvider map[string]string
}

type brokerOwnerRequest struct {
	User       string   `json:"user,omitempty"`
	Role       string   `json:"role,omitempty"`
	PublicKeys []string `json:"public_keys,omitempty"`
}

func setupCommand(ctx context.Context, base config, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) >= 2 && args[0] == "profile" && args[1] == "create" {
		return setupProfileCreateCommand(args[2:], stdin, stdout)
	}
	opts, err := parseSetupArgs(args)
	if err != nil {
		return err
	}
	if len(opts.profiles) == 0 && base.gcloudConfigurationExplicit && strings.TrimSpace(base.gcloudConfiguration) != "" {
		opts.profiles = append(opts.profiles, strings.TrimSpace(base.gcloudConfiguration))
	}
	interactiveReader := bufio.NewReader(stdin)
	path := opts.configPath
	if path == "" {
		path, err = defaultGlobalConfigPath()
		if err != nil {
			return err
		}
	}
	fmt.Fprintln(stdout, "discovering cloud profiles...")
	profiles, err := discoverSetupProfiles(ctx)
	if err != nil {
		return err
	}
	global, err := readGlobalConfig(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		global = globalConfig{Version: globalConfigVersion}
	}
	if global.Version == 0 {
		global.Version = globalConfigVersion
	}
	profiles = markConfiguredSetupProfiles(profiles, global)
	profiles = filterSetupProfiles(profiles, opts.provider, opts.profiles, opts.region)
	if len(profiles) == 0 {
		if opts.yes {
			return errors.New("no cloud profiles found; install/configure gcloud or AWS CLI profiles first")
		}
		if len(setupAvailableCreateProviders()) == 0 {
			return errors.New("no cloud profiles found; install/configure gcloud or AWS CLI profiles first")
		}
	}
	keys, err := discoverSetupSSHKeys(setupSSHKeyOptions{Paths: opts.keys, NoAgent: opts.noAgent})
	if err != nil {
		return err
	}
selectAgain:
	for {
		selection := setupSelection{
			Profiles:                profiles,
			Keys:                    keys,
			IdentityName:            global.Identity.Name,
			IdentityEmail:           global.Identity.Email,
			DefaultCreate:           firstSetupRequestedProfile(opts),
			DefaultCreateByProvider: setupCreateProfileDefaults(profiles, opts),
		}
		if !opts.yes {
			selected, err := runSetupDialogWithRaw(interactiveReader, stdin, stdout, selection)
			if err != nil {
				return err
			}
			selection = selected
		}
		if selection.Action == "create-profile" {
			if err := setupInteractiveCreateProfile(selection, stdout); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "rediscovering cloud profiles...")
			profiles, err = discoverSetupProfiles(ctx)
			if err != nil {
				return err
			}
			profiles = markConfiguredSetupProfiles(profiles, global)
			profiles = filterSetupProfiles(profiles, opts.provider, opts.profiles, opts.region)
			continue selectAgain
		}
		identityChanged := setupSelectionIdentityChanged(selection, global)
		if len(selection.Profiles) == 0 && !identityChanged {
			return errors.New("setup requires at least one selected cloud profile")
		}
		if strings.TrimSpace(selection.IdentityEmail) != "" && !identityEmailPattern.MatchString(strings.TrimSpace(selection.IdentityEmail)) {
			return fmt.Errorf("email address %q looks invalid", strings.TrimSpace(selection.IdentityEmail))
		}
		if strings.TrimSpace(selection.IdentityName) != "" || strings.TrimSpace(selection.IdentityEmail) != "" {
			global.Identity = globalIdentityConfig{
				Name:  strings.TrimSpace(selection.IdentityName),
				Email: strings.TrimSpace(selection.IdentityEmail),
			}
		}
		if len(selection.Profiles) == 0 {
			if err := writeGlobalConfig(path, global); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "wrote BucketGit config %s\n", path)
			return nil
		}
		var publicKeys []string
		for _, key := range selection.Keys {
			publicKeys = append(publicKeys, key.PublicKey)
		}
		publicKeys = uniqueStrings(publicKeys)
		now := time.Now().UTC().Format(time.RFC3339)
		for _, profile := range selection.Profiles {
			if profile.Provider == "s3" && (profile.AccountID == "" || profile.ARN == "") {
				accountID, arn := awsCallerIdentity(ctx, profile.Name)
				profile.AccountID = firstNonEmpty(profile.AccountID, accountID)
				profile.ARN = firstNonEmpty(profile.ARN, arn)
			}
			cfg := base
			cfg.provider = profile.Provider
			cfg.gcloudConfiguration = profile.Name
			cfg.gcloudConfigurationExplicit = profile.Name != ""
			if profile.Provider == "gcs" {
				if err := requireSetupCLI("gcloud", "GCP"); err != nil {
					return err
				}
				if err := ensureGcloudSetupAuth(ctx, cfg, !opts.yes, stdin, stdout); err != nil {
					return err
				}
				if err := ensureGcloudSetupProjectAccess(ctx, cfg, !opts.yes, stdin, stdout); err != nil {
					return err
				}
				if project := gcloudConfigValue(ctx, cfg.gcloudConfiguration, "project"); project != "" {
					profile.ProjectID = project
				}
				if err := ensureGcloudSetupBilling(ctx, cfg, profile.ProjectID, !opts.yes, stdin, stdout); err != nil {
					return err
				}
				regions, err := resolveGCPSetupRegionsWithRaw(profile, opts.region, !opts.yes, interactiveReader, stdin, stdout)
				if err != nil {
					if errors.Is(err, errSetupBack) && !opts.yes {
						continue selectAgain
					}
					return err
				}
				if len(regions) == 0 {
					return fmt.Errorf("GCP profile %s requires at least one selected region", profile.Name)
				}
				for _, region := range regions {
					profile.Region = region
					if err := setupProvisionSelectedProfile(base, path, now, profile, opts, publicKeys, &global, stdout); err != nil {
						return err
					}
				}
				continue
			} else if profile.Provider == "s3" {
				if err := requireSetupCLI("aws", "AWS"); err != nil {
					return err
				}
				regions, err := resolveAWSSetupRegionsWithRaw(ctx, profile, opts.region, !opts.yes, interactiveReader, stdin, stdout)
				if err != nil {
					if errors.Is(err, errSetupBack) && !opts.yes {
						continue selectAgain
					}
					return err
				}
				if len(regions) == 0 {
					return fmt.Errorf("AWS profile %s requires at least one selected region", profile.Name)
				}
				for _, region := range regions {
					profile.Region = region
					if err := setupProvisionSelectedProfile(base, path, now, profile, opts, publicKeys, &global, stdout); err != nil {
						return err
					}
				}
				continue
			}
		}
		if err := writeGlobalConfig(path, global); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote BucketGit config %s\n", path)
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Next steps:")
		fmt.Fprintln(stdout, "  bgit init")
		fmt.Fprintln(stdout, "  bgit init --noninteractive --repo my-repo --profile PROFILE")
		fmt.Fprintln(stdout, "  git push -u origin main")
		return nil
	}
}

func setupProfileCreateCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	provider := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--provider":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return err
			}
			provider = normalizeSetupProvider(value)
			if provider == "" {
				return fmt.Errorf("unsupported setup profile provider %q", value)
			}
		case "gcp", "gcs", "aws", "s3":
			if provider == "" {
				provider = normalizeSetupProvider(arg)
				continue
			}
			rest = append(rest, arg)
		default:
			rest = append(rest, arg)
		}
	}
	if provider == "" {
		return errors.New("usage: bgit setup profile create --provider gcp|aws NAME")
	}
	switch provider {
	case "gcs":
		return createGcloudProfileCommand(rest, stdin, stdout)
	case "s3":
		return createAWSProfileCommand(rest, stdin, stdout)
	default:
		return errors.New("usage: bgit setup profile create --provider gcp|aws NAME")
	}
}

func setupInteractiveCreateProfile(selection setupSelection, stdout io.Writer) error {
	provider := selection.CreateProvider
	name := strings.TrimSpace(selection.CreateName)
	if name == "" {
		name = "default"
	}
	switch provider {
	case "gcs":
		if _, err := exec.LookPath("gcloud"); err != nil {
			return errors.New("gcloud is not installed")
		}
		return createGcloudProfileCommand([]string{"--yes", name}, strings.NewReader(""), stdout)
	case "s3":
		if _, err := exec.LookPath("aws"); err != nil {
			return errors.New("AWS CLI is not installed")
		}
		return createAWSProfileConfigured(name, selection.CreateAccessKey, selection.CreateSecretKey, selection.CreateRegion, stdout)
	default:
		return errors.New("unknown setup profile provider")
	}
}

func setupSelectionIdentityChanged(selection setupSelection, cfg globalConfig) bool {
	return strings.TrimSpace(selection.IdentityName) != strings.TrimSpace(cfg.Identity.Name) ||
		strings.TrimSpace(selection.IdentityEmail) != strings.TrimSpace(cfg.Identity.Email)
}

func firstSetupRequestedProfile(opts setupOptions) string {
	if len(opts.profiles) > 0 {
		return opts.profiles[0]
	}
	return ""
}

func setupCreateProfileDefaults(profiles []setupProfile, opts setupOptions) map[string]string {
	defaults := map[string]string{}
	if requested := firstSetupRequestedProfile(opts); requested != "" {
		if opts.provider == "" || opts.provider == "gcs" {
			defaults["gcs"] = requested
		}
		if opts.provider == "" || opts.provider == "s3" {
			defaults["s3"] = requested
		}
		return defaults
	}
	hasDefault := map[string]bool{}
	for _, profile := range profiles {
		if profile.Name == "default" {
			hasDefault[profile.Provider] = true
		}
	}
	if !hasDefault["gcs"] {
		defaults["gcs"] = "default"
	}
	if !hasDefault["s3"] {
		defaults["s3"] = "default"
	}
	return defaults
}

func setupProvisionSelectedProfile(base config, path, now string, profile setupProfile, opts setupOptions, publicKeys []string, global *globalConfig, stdout io.Writer) error {
	_ = path
	cfg := base
	cfg.provider = profile.Provider
	cfg.gcloudConfiguration = profile.Name
	cfg.gcloudConfigurationExplicit = profile.Name != ""
	brokerURL, err := provisionBrokerURL(cfg, sshSetupOptions{region: firstNonEmpty(opts.region, profile.Region)}, stdout)
	if err != nil {
		return err
	}
	if len(publicKeys) > 0 {
		if err := brokerUpsertOwners(brokerURL, publicKeys); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "imported %d owner key(s) into broker %s\n", len(publicKeys), brokerURL)
	}
	switch profile.Provider {
	case "gcs":
		serviceAccount := ""
		if strings.TrimSpace(profile.ProjectID) != "" {
			serviceAccount = gcpBrokerServiceAccountEmail(profile.ProjectID)
		}
		*global = upsertGlobalGCPProfile(*global, globalGCPProfile{
			Name:           profile.Name,
			ProjectID:      profile.ProjectID,
			Account:        profile.Account,
			ServiceAccount: serviceAccount,
			Regions: []globalProfileRegion{{
				Name:          profile.Region,
				BrokerURL:     brokerURL,
				BrokerVersion: brokerVersion,
				LastSetupAt:   now,
			}},
		})
	case "s3":
		*global = upsertGlobalAWSProfile(*global, globalAWSProfile{
			Name:      profile.Name,
			AccountID: profile.AccountID,
			ARN:       profile.ARN,
			Regions: []globalProfileRegion{{
				Name:          profile.Region,
				BrokerURL:     brokerURL,
				BrokerVersion: brokerVersion,
				LastSetupAt:   now,
			}},
		})
	}
	return nil
}

func offerSetupProfileBootstrap(opts setupOptions, reader *bufio.Reader, stdout io.Writer) error {
	provider := opts.provider
	if provider == "" {
		provider = promptSetupProvider(reader, stdout)
	}
	switch provider {
	case "gcs":
		if _, err := exec.LookPath("gcloud"); err != nil {
			return errors.New("gcloud is not installed")
		}
		fmt.Fprint(stdout, "No usable gcloud profiles found. Create one now? [y/N] ")
		if !readSetupYes(reader) {
			return errors.New("setup requires a cloud profile")
		}
		fmt.Fprint(stdout, "GCP profile name [default]: ")
		name := readSetupLine(reader)
		if name == "" && len(opts.profiles) > 0 {
			name = opts.profiles[0]
		}
		if name == "" {
			name = "default"
		}
		if err := runGcloudProfileCommand(stdout, "config", "configurations", "create", name); err != nil {
			return err
		}
		return runGcloudProfileCommand(stdout, "auth", "login", "--configuration", name)
	case "s3":
		if _, err := exec.LookPath("aws"); err != nil {
			return errors.New("AWS CLI is not installed")
		}
		fmt.Fprint(stdout, "No usable AWS profiles found. Run aws configure now? [y/N] ")
		if !readSetupYes(reader) {
			return errors.New("setup requires a cloud profile")
		}
		fmt.Fprint(stdout, "AWS profile name [default]: ")
		name := readSetupLine(reader)
		if name == "" && len(opts.profiles) > 0 {
			name = opts.profiles[0]
		}
		if name == "" {
			name = "default"
		}
		return runAWSProfileCommand(stdout, "configure", "--profile", name)
	default:
		return errors.New("setup requires --provider gcp or --provider aws to create a profile")
	}
}

func promptSetupProvider(reader *bufio.Reader, stdout io.Writer) string {
	gcloudOK := false
	awsOK := false
	if _, err := exec.LookPath("gcloud"); err == nil {
		gcloudOK = true
	}
	if _, err := exec.LookPath("aws"); err == nil {
		awsOK = true
	}
	if gcloudOK && !awsOK {
		return "gcs"
	}
	if awsOK && !gcloudOK {
		return "s3"
	}
	if !gcloudOK && !awsOK {
		return ""
	}
	fmt.Fprint(stdout, "Create profile for provider [gcp/aws]: ")
	switch strings.ToLower(readSetupLine(reader)) {
	case "gcp", "gcs":
		return "gcs"
	case "aws", "s3":
		return "s3"
	default:
		return ""
	}
}

func readSetupYes(reader *bufio.Reader) bool {
	answer := strings.ToLower(readSetupLine(reader))
	return answer == "y" || answer == "yes"
}

func readSetupLine(reader *bufio.Reader) string {
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return strings.TrimSpace(line)
}

type gcloudProjectOption struct {
	ID   string
	Name string
}

type gcloudBillingAccountOption struct {
	Name        string
	DisplayName string
	Open        bool
}

var defaultGCPSetupRegions = []string{
	"us-central1",
	"us-east1",
	"us-east4",
	"us-west1",
	"us-west2",
	"us-west3",
	"us-west4",
	"northamerica-northeast1",
	"southamerica-east1",
	"europe-west1",
	"europe-west2",
	"europe-west3",
	"europe-west4",
	"europe-west6",
	"europe-central2",
	"asia-east1",
	"asia-east2",
	"asia-northeast1",
	"asia-northeast2",
	"asia-northeast3",
	"asia-south1",
	"asia-southeast1",
	"asia-southeast2",
	"australia-southeast1",
}

var awsSetupRegions = []string{
	"us-east-1",
	"eu-west-1",
	"eu-central-1",
	"us-west-2",
	"ap-southeast-1",
	"ap-northeast-1",
	"ap-south-1",
	"sa-east-1",
	"ca-central-1",
	"af-south-1",
	"ap-east-1",
	"ap-east-2",
	"ap-northeast-2",
	"ap-northeast-3",
	"ap-south-2",
	"ap-southeast-2",
	"ap-southeast-3",
	"ap-southeast-4",
	"ap-southeast-5",
	"ap-southeast-6",
	"ap-southeast-7",
	"ca-west-1",
	"eu-central-2",
	"eu-north-1",
	"eu-south-1",
	"eu-south-2",
	"eu-west-2",
	"eu-west-3",
	"il-central-1",
	"me-central-1",
	"me-south-1",
	"mx-central-1",
	"us-east-2",
	"us-west-1",
}

func parseSetupArgs(args []string) (setupOptions, error) {
	var opts setupOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--yes", "-y":
			opts.yes = true
		case "--provider":
			value, next, err := optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, err
			}
			i = next
			opts.provider = normalizeSetupProvider(value)
			if opts.provider == "" {
				return opts, fmt.Errorf("unsupported setup provider %q", value)
			}
		case "--profile":
			value, next, err := optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, err
			}
			i = next
			opts.profiles = append(opts.profiles, value)
		case "--config":
			value, next, err := optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, err
			}
			i = next
			opts.configPath = expandHome(value)
		case "--region":
			value, next, err := optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, err
			}
			i = next
			opts.region = value
		case "--key":
			value, next, err := optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, err
			}
			i = next
			opts.keys = append(opts.keys, value)
		case "--no-agent":
			opts.noAgent = true
		default:
			return opts, fmt.Errorf("unsupported setup option %s", arg)
		}
	}
	return opts, nil
}

func discoverSetupProfiles(ctx context.Context) ([]setupProfile, error) {
	var profiles []setupProfile
	gcp, err := discoverGCPSetupProfiles(ctx)
	if err != nil {
		return nil, err
	}
	profiles = append(profiles, gcp...)
	aws, err := discoverAWSSetupProfiles(ctx)
	if err != nil {
		return nil, err
	}
	profiles = append(profiles, aws...)
	return profiles, nil
}

func discoverGCPSetupProfiles(ctx context.Context) ([]setupProfile, error) {
	if _, err := exec.LookPath("gcloud"); err != nil {
		return nil, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, setupProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, "gcloud", "config", "configurations", "list", "--format=value(name,is_active)").Output()
	if err != nil {
		return nil, nil
	}
	var profiles []setupProfile
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		active := len(fields) > 1 && strings.EqualFold(fields[1], "true")
		profile := setupProfile{Provider: "gcs", Name: name, Active: active}
		profile.Account = gcloudConfigValue(ctx, name, "account")
		profile.ProjectID = gcloudConfigValue(ctx, name, "project")
		profile.Region = firstNonEmpty(gcloudConfigValue(ctx, name, "run/region"), gcloudConfigValue(ctx, name, "functions/region"), "us-central1")
		profiles = append(profiles, profile)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return profiles, nil
}

func requireSetupCLI(binary, provider string) error {
	if _, err := exec.LookPath(binary); err != nil {
		return fmt.Errorf("%s CLI is not installed; install `%s` or deselect the %s profile", provider, binary, provider)
	}
	return nil
}

func gcloudConfigValue(ctx context.Context, profile, key string) string {
	probeCtx, cancel := context.WithTimeout(ctx, setupProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "gcloud", "--configuration", profile, "config", "get-value", key, "--quiet")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(out))
	if value == "(unset)" {
		return ""
	}
	return value
}

func discoverAWSSetupProfiles(ctx context.Context) ([]setupProfile, error) {
	_ = ctx
	names := map[string]struct{}{}
	for _, name := range awsProfilesFromFiles() {
		names[name] = struct{}{}
	}
	var sorted []string
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	var profiles []setupProfile
	for _, name := range sorted {
		profile := setupProfile{Provider: "s3", Name: name, Region: configuredAWSProfileRegion(name)}
		profiles = append(profiles, profile)
	}
	return profiles, nil
}

func awsProfilesFromFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	names := map[string]struct{}{}
	for _, path := range []string{filepath.Join(home, ".aws", "config"), filepath.Join(home, ".aws", "credentials")} {
		for _, name := range parseAWSProfileFile(path) {
			names[name] = struct{}{}
		}
	}
	var sorted []string
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return sorted
}

func parseAWSProfileFile(path string) []string {
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
		name := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
		name = strings.TrimSpace(strings.TrimPrefix(name, "profile "))
		if name != "" {
			names[name] = struct{}{}
		}
	}
	var sorted []string
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return sorted
}

func configuredAWSProfileRegion(profile string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, path := range []string{filepath.Join(home, ".aws", "config"), filepath.Join(home, ".aws", "credentials")} {
		if region := awsProfileFileValue(path, profile, "region"); region != "" {
			return region
		}
	}
	return ""
}

func awsProfileFileValue(path, profile, key string) string {
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
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			section = strings.TrimSpace(strings.TrimPrefix(section, "profile "))
			continue
		}
		if section != profile {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.TrimSpace(v)
	}
	return ""
}

func awsCallerIdentity(ctx context.Context, profile string) (string, string) {
	if _, err := exec.LookPath("aws"); err != nil {
		return "", ""
	}
	args := []string{"sts", "get-caller-identity", "--output", "json"}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	probeCtx, cancel := context.WithTimeout(ctx, setupProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, "aws", args...).Output()
	if err != nil {
		return "", ""
	}
	var resp struct {
		Account string `json:"Account"`
		ARN     string `json:"Arn"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", ""
	}
	return resp.Account, resp.ARN
}

func resolveGCPSetupRegion(profile setupProfile, explicitRegion string, interactive bool, stdin io.Reader, stdout io.Writer) (string, error) {
	regions, err := resolveGCPSetupRegionsWithRaw(profile, explicitRegion, interactive, stdin, stdin, stdout)
	if err != nil || len(regions) == 0 {
		return "", err
	}
	return regions[0], nil
}

func resolveGCPSetupRegionWithRaw(profile setupProfile, explicitRegion string, interactive bool, stdin io.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	regions, err := resolveGCPSetupRegionsWithRaw(profile, explicitRegion, interactive, stdin, rawInput, stdout)
	if err != nil || len(regions) == 0 {
		return "", err
	}
	return regions[0], nil
}

func resolveGCPSetupRegionsWithRaw(profile setupProfile, explicitRegion string, interactive bool, stdin io.Reader, rawInput io.Reader, stdout io.Writer) ([]string, error) {
	if strings.TrimSpace(explicitRegion) != "" {
		return []string{strings.TrimSpace(explicitRegion)}, nil
	}
	defaultRegion := firstNonEmpty(strings.TrimSpace(profile.Region), "us-central1")
	if !interactive {
		return []string{defaultRegion}, nil
	}
	regions := gcpSetupRegions(defaultRegion)
	reader, ok := stdin.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(stdin)
	}
	return runSetupRegionDialogWithRaw(reader, rawInput, stdout, "GCP", profile.Name, regions, setupRegionInitialSelection(profile))
}

func gcpSetupRegions(defaultRegion string) []string {
	seen := map[string]struct{}{}
	var regions []string
	add := func(region string) {
		region = strings.TrimSpace(region)
		if region == "" {
			return
		}
		if _, ok := seen[region]; ok {
			return
		}
		seen[region] = struct{}{}
		regions = append(regions, region)
	}
	add(defaultRegion)
	for _, region := range defaultGCPSetupRegions {
		add(region)
	}
	return regions
}

func resolveAWSSetupRegion(ctx context.Context, profile setupProfile, explicitRegion string, interactive bool, stdin io.Reader, stdout io.Writer) (string, error) {
	regions, err := resolveAWSSetupRegionsWithRaw(ctx, profile, explicitRegion, interactive, stdin, stdin, stdout)
	if err != nil || len(regions) == 0 {
		return "", err
	}
	return regions[0], nil
}

func resolveAWSSetupRegionWithRaw(ctx context.Context, profile setupProfile, explicitRegion string, interactive bool, stdin io.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	regions, err := resolveAWSSetupRegionsWithRaw(ctx, profile, explicitRegion, interactive, stdin, rawInput, stdout)
	if err != nil || len(regions) == 0 {
		return "", err
	}
	return regions[0], nil
}

func resolveAWSSetupRegionsWithRaw(ctx context.Context, profile setupProfile, explicitRegion string, interactive bool, stdin io.Reader, rawInput io.Reader, stdout io.Writer) ([]string, error) {
	if err := requireSetupCLI("aws", "AWS"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(explicitRegion) != "" {
		return []string{strings.TrimSpace(explicitRegion)}, nil
	}
	if !interactive {
		if strings.TrimSpace(profile.Region) != "" {
			return []string{strings.TrimSpace(profile.Region)}, nil
		}
		return nil, fmt.Errorf("AWS profile %s has no configured region; pass --region REGION or set aws_region/region in ~/.aws/config", profile.Name)
	}
	_ = ctx
	if len(awsSetupRegions) == 0 {
		return nil, fmt.Errorf("AWS profile %s has no enabled regions visible; pass --region REGION", profile.Name)
	}
	reader, ok := stdin.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(stdin)
	}
	return runSetupRegionDialogWithRaw(reader, rawInput, stdout, "AWS", profile.Name, awsSetupRegions, setupRegionInitialSelection(profile))
}

func markConfiguredSetupProfiles(profiles []setupProfile, cfg globalConfig) []setupProfile {
	configured := map[string][]string{}
	for _, profile := range cfg.GCPProfiles {
		if regions := configuredSetupProfileRegions(profile.Regions); len(regions) > 0 {
			configured["gcs:"+profile.Name] = regions
		}
	}
	for _, profile := range cfg.AWSProfiles {
		if regions := configuredSetupProfileRegions(profile.Regions); len(regions) > 0 {
			configured["s3:"+profile.Name] = regions
		}
	}
	if len(configured) == 0 {
		return profiles
	}
	var out []setupProfile
	for _, profile := range profiles {
		regions, ok := configured[profile.Provider+":"+profile.Name]
		if !ok {
			out = append(out, profile)
			continue
		}
		for _, region := range regions {
			next := profile
			next.Existing = true
			next.Region = region
			next.ConfiguredRegions = append([]string{}, regions...)
			out = append(out, next)
		}
	}
	return out
}

func configuredSetupProfileRegions(regions []globalProfileRegion) []string {
	var out []string
	for _, region := range regions {
		if strings.TrimSpace(region.BrokerURL) == "" {
			continue
		}
		if name := strings.TrimSpace(region.Name); name != "" {
			out = append(out, name)
		}
	}
	if len(out) > 0 {
		return uniqueStrings(out)
	}
	for _, region := range regions {
		if name := strings.TrimSpace(region.Name); name != "" {
			out = append(out, name)
		}
	}
	return uniqueStrings(out)
}

func setupRegionInitialSelection(profile setupProfile) []string {
	if len(profile.ConfiguredRegions) > 0 {
		return append([]string{}, profile.ConfiguredRegions...)
	}
	if profile.Existing && strings.TrimSpace(profile.Region) != "" {
		return []string{strings.TrimSpace(profile.Region)}
	}
	return nil
}

func filterSetupProfiles(profiles []setupProfile, provider string, names []string, explicitRegion string) []setupProfile {
	nameSet := map[string]struct{}{}
	for _, name := range names {
		nameSet[name] = struct{}{}
	}
	var out []setupProfile
	for _, profile := range profiles {
		if provider != "" && profile.Provider != provider {
			continue
		}
		if len(nameSet) > 0 {
			if !setupProfileNameSelected(profile, nameSet) {
				continue
			}
		}
		if strings.TrimSpace(explicitRegion) != "" {
			profile.Region = strings.TrimSpace(explicitRegion)
		}
		out = append(out, profile)
	}
	if strings.TrimSpace(explicitRegion) != "" {
		out = dedupeSetupProfiles(out)
	}
	return out
}

func setupProfileNameSelected(profile setupProfile, names map[string]struct{}) bool {
	candidates := []string{
		profile.Name,
		profile.Name + "." + profile.Region,
		providerProfileName(profile.Provider) + ":" + profile.Name,
		providerProfileName(profile.Provider) + ":" + profile.Name + "." + profile.Region,
		providerProfileName(profile.Provider) + ":" + profile.Name + "/" + profile.Region,
	}
	for _, candidate := range candidates {
		if _, ok := names[candidate]; ok {
			return true
		}
	}
	return false
}

func dedupeSetupProfiles(profiles []setupProfile) []setupProfile {
	seen := map[string]struct{}{}
	var out []setupProfile
	for _, profile := range profiles {
		key := profile.Provider + ":" + profile.Name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, profile)
	}
	return out
}

func normalizeSetupProvider(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "gcp", "gcs", "google":
		return "gcs"
	case "aws", "s3":
		return "s3"
	default:
		return ""
	}
}

type setupSSHKeyOptions struct {
	Paths   []string
	NoAgent bool
}

func discoverSetupSSHKeys(opts setupSSHKeyOptions) ([]setupSSHKey, error) {
	var keys []setupSSHKey
	for _, path := range opts.Paths {
		data, err := os.ReadFile(expandHome(path))
		if err != nil {
			return nil, err
		}
		for _, key := range parseSetupSSHKeys(string(data), path) {
			keys = append(keys, key)
		}
	}
	if !opts.NoAgent {
		agentKeys, err := sshAgentPublicKeys()
		if err == nil {
			for _, key := range agentKeys {
				keys = append(keys, setupSSHKey{PublicKey: key, Source: "ssh-agent", Comment: sshKeyComment(key)})
			}
		}
	}
	fileKeys, err := discoverSSHKeyFiles()
	if err != nil {
		return nil, err
	}
	keys = append(keys, fileKeys...)
	return dedupeSetupSSHKeys(keys), nil
}

func discoverSSHKeyFiles() ([]setupSSHKey, error) {
	var dirs []string
	home, err := os.UserHomeDir()
	if err == nil {
		dirs = append(dirs, filepath.Join(home, ".ssh"))
		if runtime.GOOS == "windows" {
			dirs = append(dirs, filepath.Join(home, "ssh"))
		}
	}
	var keys []setupSSHKey
	for _, dir := range dirs {
		matches, err := filepath.Glob(filepath.Join(dir, "*.pub"))
		if err != nil {
			return nil, err
		}
		sort.Strings(matches)
		for _, path := range matches {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			for _, key := range parseSetupSSHKeys(string(data), path) {
				keys = append(keys, key)
			}
		}
	}
	return keys, nil
}

func parseSetupSSHKeys(data, source string) []setupSSHKey {
	var keys []setupSSHKey
	for _, line := range splitPublicKeyLines(data) {
		keys = append(keys, setupSSHKey{PublicKey: line, Source: source, Comment: sshKeyComment(line)})
	}
	return keys
}

func dedupeSetupSSHKeys(keys []setupSSHKey) []setupSSHKey {
	seen := map[string]struct{}{}
	var out []setupSSHKey
	for _, key := range keys {
		normalized := normalizeSSHPublicKey(key.PublicKey)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		key.PublicKey = normalized
		out = append(out, key)
	}
	return out
}

func normalizeSSHPublicKey(key string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(key))[:min(2, len(strings.Fields(strings.TrimSpace(key))))], " ")
}

func sshKeyComment(key string) string {
	fields := strings.Fields(strings.TrimSpace(key))
	if len(fields) <= 2 {
		return ""
	}
	return strings.Join(fields[2:], " ")
}

func ensureGcloudSetupAuth(ctx context.Context, cfg config, interactive bool, stdin io.Reader, stdout io.Writer) error {
	if _, err := gcloudSetupAccessToken(ctx, cfg); err == nil {
		return nil
	} else if !gcloudAuthNeedsLogin(err.Error()) {
		return err
	}
	profile := strings.TrimSpace(cfg.gcloudConfiguration)
	if profile == "" {
		profile = "default"
	}
	loginCommand := fmt.Sprintf("gcloud auth login --configuration %s --no-launch-browser", profile)
	if !interactive {
		return fmt.Errorf("gcloud profile %s needs authentication; run `%s`", profile, loginCommand)
	}
	fmt.Fprintf(stdout, "gcloud profile %s needs authentication.\n", profile)
	fmt.Fprintf(stdout, "Starting `%s`.\n", loginCommand)
	fmt.Fprintln(stdout, "Open the URL printed by gcloud, finish the OAuth flow, then paste the code if prompted.")
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "login", "--configuration", profile, "--no-launch-browser")
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gcloud auth login failed: %w", err)
	}
	if _, err := gcloudSetupAccessToken(ctx, cfg); err != nil {
		return fmt.Errorf("gcloud profile %s is still not authenticated after login: %w", profile, err)
	}
	return nil
}

func ensureGcloudSetupProjectAccess(ctx context.Context, cfg config, interactive bool, stdin io.Reader, stdout io.Writer) error {
	project := gcloudConfigValue(ctx, cfg.gcloudConfiguration, "project")
	if project == "" {
		var err error
		project, err = ensureGcloudSetupProjectSelected(ctx, cfg, interactive, stdin, stdout)
		if err != nil {
			return err
		}
	}
	if err := gcloudSetupProjectAccess(ctx, cfg, project); err == nil {
		return nil
	} else if enabled, enableErr := maybeEnableGcloudSetupProjectAPIs(ctx, cfg, project, err, stdout); enableErr != nil {
		return enableErr
	} else if enabled {
		return nil
	} else if repaired, repairErr := maybeRepairGcloudQuotaProject(ctx, cfg, project, err, interactive, stdin, stdout); repairErr != nil {
		return repairErr
	} else if repaired {
		return nil
	} else if !gcloudAuthNeedsLogin(err.Error()) {
		return err
	}
	if err := runGcloudSetupLogin(ctx, cfg, interactive, stdin, stdout); err != nil {
		return err
	}
	if err := gcloudSetupProjectAccess(ctx, cfg, project); err == nil {
		return nil
	} else if enabled, enableErr := maybeEnableGcloudSetupProjectAPIs(ctx, cfg, project, err, stdout); enableErr != nil {
		return enableErr
	} else if enabled {
		return nil
	} else if repaired, repairErr := maybeRepairGcloudQuotaProject(ctx, cfg, project, err, interactive, stdin, stdout); repairErr != nil {
		return repairErr
	} else if !repaired {
		return fmt.Errorf("gcloud profile %s still cannot access project %s after login: %w", cfg.gcloudConfiguration, project, err)
	}
	return nil
}

func ensureGcloudSetupProjectSelected(ctx context.Context, cfg config, interactive bool, stdin io.Reader, stdout io.Writer) (string, error) {
	profile := strings.TrimSpace(cfg.gcloudConfiguration)
	if profile == "" {
		profile = "default"
	}
	if !interactive {
		return "", errors.New("gcloud project is unset; run `gcloud config set project PROJECT --configuration " + profile + "`")
	}
	reader := bufio.NewReader(stdin)
	account := gcloudConfigValue(ctx, cfg.gcloudConfiguration, "account")
	fmt.Fprintf(stdout, "gcloud profile %s", profile)
	if account != "" {
		fmt.Fprintf(stdout, " uses account %s", account)
	}
	fmt.Fprintln(stdout, " but has no project configured.")
	projects, _ := listGcloudSetupProjects(ctx, cfg)
	if len(projects) > 0 {
		fmt.Fprintln(stdout, "Visible projects:")
		for i, project := range projects {
			label := project.ID
			if strings.TrimSpace(project.Name) != "" {
				label += " - " + project.Name
			}
			fmt.Fprintf(stdout, "  %d. %s\n", i+1, label)
		}
	}
	fmt.Fprintln(stdout, "Choose a project number, type an existing project ID, type `create`, or leave blank to cancel.")
	fmt.Fprint(stdout, "Project: ")
	choice := readSetupLine(reader)
	if choice == "" {
		return "", errors.New("setup requires a gcloud project")
	}
	projectID := ""
	if n := parsePositiveInt(choice); n > 0 && n <= len(projects) {
		projectID = projects[n-1].ID
	} else if strings.EqualFold(choice, "create") {
		created, err := createGcloudSetupProject(ctx, cfg, reader, stdout)
		if err != nil {
			return "", err
		}
		projectID = created
	} else {
		projectID = choice
	}
	if err := setGcloudSetupProject(ctx, cfg, projectID, stdout); err != nil {
		return "", err
	}
	return projectID, nil
}

func listGcloudSetupProjects(ctx context.Context, cfg config) ([]gcloudProjectOption, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "gcloud", "projects", "list", "--configuration", cfg.gcloudConfiguration, "--format=value(projectId,name)")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var projects []gcloudProjectOption
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		project := gcloudProjectOption{ID: fields[0]}
		if len(fields) > 1 {
			project.Name = strings.Join(fields[1:], " ")
		}
		projects = append(projects, project)
	}
	return projects, scanner.Err()
}

func createGcloudSetupProject(ctx context.Context, cfg config, reader *bufio.Reader, stdout io.Writer) (string, error) {
	fmt.Fprint(stdout, "New project ID: ")
	projectBase := readSetupLine(reader)
	if projectBase == "" {
		return "", errors.New("project ID is required")
	}
	fmt.Fprintf(stdout, "Project display name [%s]: ", projectBase)
	name := readSetupLine(reader)
	if name == "" {
		name = projectBase
	}
	projectID, err := gcloudSetupInitialProjectID(projectBase)
	if err != nil {
		return "", err
	}
	if projectID != projectBase {
		fmt.Fprintf(stdout, "Project ID %s is not a valid GCP project ID; trying %s.\n", projectBase, projectID)
	}
	if err := runGcloudSetupProjectCreate(ctx, cfg, projectID, name, stdout); err == nil {
		return projectID, nil
	} else if !gcloudSetupProjectIDAlreadyExists(err) {
		return "", fmt.Errorf("create gcloud project %s: %w", projectID, err)
	}
	projectID, err = gcloudSetupProjectIDWithRandomSuffix(projectBase)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(stdout, "Project ID %s is already in use; trying %s.\n", projectBase, projectID)
	if err := runGcloudSetupProjectCreate(ctx, cfg, projectID, name, stdout); err != nil {
		return "", fmt.Errorf("create gcloud project %s: %w", projectID, err)
	}
	return projectID, nil
}

func runGcloudSetupProjectCreate(ctx context.Context, cfg config, projectID, name string, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, "gcloud", "projects", "create", projectID, "--configuration", cfg.gcloudConfiguration, "--name", name)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		_, _ = stdout.Write(out)
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gcloudSetupProjectIDAlreadyExists(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already in use") || strings.Contains(msg, "project id") && strings.Contains(msg, "in use")
}

func gcloudSetupInitialProjectID(base string) (string, error) {
	base = gcloudSetupProjectIDBase(base)
	if base == "" {
		return "", errors.New("project ID must contain at least one lowercase letter or digit")
	}
	if len(base) < 6 {
		return gcloudSetupProjectIDWithRandomSuffix(base)
	}
	if len(base) > 30 {
		base = strings.TrimRight(base[:30], "-")
	}
	if !gcloudSetupProjectIDValid(base) {
		return "", fmt.Errorf("invalid project ID %q", base)
	}
	return base, nil
}

func gcloudSetupProjectIDBase(base string) string {
	base = strings.ToLower(strings.TrimSpace(base))
	var b strings.Builder
	lastHyphen := false
	for _, ch := range base {
		valid := ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9'
		if valid {
			b.WriteRune(ch)
			lastHyphen = false
			continue
		}
		if ch == '-' || ch == '_' || ch == ' ' || ch == '.' {
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	base = strings.Trim(b.String(), "-")
	if base == "" {
		return ""
	}
	if base[0] < 'a' || base[0] > 'z' {
		base = "bgit-" + base
	}
	return base
}

func gcloudSetupProjectIDValid(id string) bool {
	if len(id) < 6 || len(id) > 30 {
		return false
	}
	if id[0] < 'a' || id[0] > 'z' {
		return false
	}
	last := id[len(id)-1]
	if last == '-' {
		return false
	}
	for _, ch := range id {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' {
			continue
		}
		return false
	}
	return true
}

func gcloudSetupProjectIDWithRandomSuffix(base string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(10000000))
	if err != nil {
		return "", fmt.Errorf("generate project ID suffix: %w", err)
	}
	return gcloudSetupProjectIDWithSuffix(base, fmt.Sprintf("%07d", n.Int64())), nil
}

func gcloudSetupProjectIDWithSuffix(base, suffix string) string {
	base = gcloudSetupProjectIDBase(base)
	if len(base) > 22 {
		base = strings.TrimRight(base[:22], "-")
	}
	return base + "-" + suffix
}

func setGcloudSetupProject(ctx context.Context, cfg config, projectID string, stdout io.Writer) error {
	profile := strings.TrimSpace(cfg.gcloudConfiguration)
	if profile == "" {
		profile = "default"
	}
	for _, args := range [][]string{
		{"config", "set", "project", projectID, "--configuration", profile},
		{"config", "set", "billing/quota_project", projectID, "--configuration", profile},
	} {
		cmd := exec.CommandContext(ctx, "gcloud", args...)
		cmd.Stdout = stdout
		cmd.Stderr = stdout
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("gcloud %s failed: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

func ensureGcloudSetupBilling(ctx context.Context, cfg config, project string, interactive bool, stdin io.Reader, stdout io.Writer) error {
	project = strings.TrimSpace(project)
	if project == "" {
		project = gcloudConfigValue(ctx, cfg.gcloudConfiguration, "project")
	}
	if project == "" {
		return errors.New("GCP project is not configured")
	}
	enabled, err := gcloudSetupBillingEnabled(ctx, cfg, project)
	if err == nil && enabled {
		return nil
	}
	if err != nil {
		if gcloudProjectServiceDisabled(err.Error()) {
			if enableErr := enableGcloudSetupProjectServices(ctx, cfg, project, []string{"cloudbilling.googleapis.com"}, stdout, "GCP Cloud Billing API"); enableErr != nil {
				return enableErr
			}
			enabled, err = gcloudSetupBillingEnabled(ctx, cfg, project)
			if err == nil && enabled {
				return nil
			}
		}
		if err != nil {
			return fmt.Errorf("check GCP billing for project %s: %w", project, err)
		}
	}
	profile := strings.TrimSpace(cfg.gcloudConfiguration)
	if profile == "" {
		profile = "default"
	}
	if !interactive {
		return fmt.Errorf("GCP project %s does not have billing enabled; run `gcloud billing projects link %s --billing-account BILLING_ACCOUNT --configuration %s`", project, project, profile)
	}
	accounts, err := listGcloudSetupBillingAccounts(ctx, cfg)
	if err != nil {
		return fmt.Errorf("list GCP billing accounts: %w", err)
	}
	var openAccounts []gcloudBillingAccountOption
	for _, account := range accounts {
		if account.Open {
			openAccounts = append(openAccounts, account)
		}
	}
	if len(openAccounts) == 0 {
		return fmt.Errorf("GCP project %s does not have billing enabled and no open billing accounts are visible to profile %s", project, profile)
	}
	reader := bufio.NewReader(stdin)
	fmt.Fprintf(stdout, "GCP project %s does not have billing enabled.\n", project)
	fmt.Fprintln(stdout, "Visible billing accounts:")
	for i, account := range openAccounts {
		label := account.Name
		if strings.TrimSpace(account.DisplayName) != "" {
			label += " - " + account.DisplayName
		}
		fmt.Fprintf(stdout, "  %d. %s\n", i+1, label)
	}
	fmt.Fprintln(stdout, "Choose a billing account number, type a billing account ID, or leave blank to cancel.")
	fmt.Fprint(stdout, "Billing account: ")
	choice := readSetupLine(reader)
	if choice == "" {
		return errors.New("setup requires billing to deploy the GCP broker")
	}
	billingAccount := choice
	if n := parsePositiveInt(choice); n > 0 && n <= len(openAccounts) {
		billingAccount = openAccounts[n-1].Name
	}
	if err := linkGcloudSetupBillingAccount(ctx, cfg, project, billingAccount, stdout); err != nil {
		return err
	}
	if err := waitForGcloudSetupBilling(ctx, cfg, project); err != nil {
		return err
	}
	return nil
}

func gcloudSetupBillingEnabled(ctx context.Context, cfg config, project string) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx,
		"gcloud", "billing", "projects", "describe", project,
		"--configuration", cfg.gcloudConfiguration,
		"--quiet",
		"--format=value(billingEnabled)",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.EqualFold(strings.TrimSpace(string(out)), "false") {
			return false, nil
		}
		return false, fmt.Errorf("%w\n%s", err, strings.TrimSpace(string(out)))
	}
	switch strings.ToLower(strings.TrimSpace(string(out))) {
	case "true", "yes", "1":
		return true, nil
	default:
		return false, nil
	}
}

func listGcloudSetupBillingAccounts(ctx context.Context, cfg config) ([]gcloudBillingAccountOption, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx,
		"gcloud", "billing", "accounts", "list",
		"--configuration", cfg.gcloudConfiguration,
		"--quiet",
		"--format=value(name,displayName,open)",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, strings.TrimSpace(string(out)))
	}
	var accounts []gcloudBillingAccountOption
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(strings.TrimSpace(scanner.Text()))
		if len(fields) == 0 {
			continue
		}
		account := gcloudBillingAccountOption{Name: fields[0], Open: true}
		if len(fields) > 1 {
			last := strings.ToLower(fields[len(fields)-1])
			if last == "true" || last == "false" {
				account.Open = last == "true"
				account.DisplayName = strings.Join(fields[1:len(fields)-1], " ")
			} else {
				account.DisplayName = strings.Join(fields[1:], " ")
			}
		}
		accounts = append(accounts, account)
	}
	return accounts, scanner.Err()
}

func linkGcloudSetupBillingAccount(ctx context.Context, cfg config, project, billingAccount string, stdout io.Writer) error {
	fmt.Fprintf(stdout, "linking GCP project %s to billing account %s\n", project, billingAccount)
	cmd := exec.CommandContext(ctx,
		"gcloud", "billing", "projects", "link", project,
		"--billing-account", billingAccount,
		"--configuration", cfg.gcloudConfiguration,
		"--quiet",
	)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		_, _ = stdout.Write(out)
	}
	if err != nil {
		if gcloudSetupBillingQuotaExceeded(string(out)) {
			return fmt.Errorf("link GCP project %s to billing account %s: billing quota exceeded; request a quota increase at https://support.google.com/code/contact/billing_quota_increase or choose a different billing account", project, billingAccount)
		}
		return fmt.Errorf("link GCP project %s to billing account %s: %w\n%s", project, billingAccount, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gcloudSetupBillingQuotaExceeded(message string) bool {
	msg := strings.ToLower(message)
	return strings.Contains(msg, "billing quota exceeded") ||
		strings.Contains(msg, "billing_quota_increase")
}

func waitForGcloudSetupBilling(ctx context.Context, cfg config, project string) error {
	for i := 0; i < 12; i++ {
		enabled, err := gcloudSetupBillingEnabled(ctx, cfg, project)
		if err == nil && enabled {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("GCP project %s billing was not visible as enabled before timeout", project)
}

func maybeRepairGcloudQuotaProject(ctx context.Context, cfg config, project string, accessErr error, interactive bool, stdin io.Reader, stdout io.Writer) (bool, error) {
	if !gcloudAuthNeedsLogin(accessErr.Error()) {
		return false, nil
	}
	quotaProject := gcloudConfigValue(ctx, cfg.gcloudConfiguration, "billing/quota_project")
	if quotaProject == "" || quotaProject == project {
		return false, nil
	}
	profile := strings.TrimSpace(cfg.gcloudConfiguration)
	if profile == "" {
		profile = "default"
	}
	command := fmt.Sprintf("gcloud config set billing/quota_project %s --configuration %s", project, profile)
	if !interactive {
		return false, fmt.Errorf("gcloud profile %s uses quota project %s while target project is %s; run `%s`", profile, quotaProject, project, command)
	}
	fmt.Fprintf(stdout, "gcloud profile %s uses quota project %s while target project is %s.\n", profile, quotaProject, project)
	fmt.Fprintf(stdout, "Set quota project to %s now? [Y/n] ", project)
	answer := ""
	_, _ = fmt.Fscanln(stdin, &answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "n" || answer == "no" {
		return false, fmt.Errorf("gcloud profile %s cannot use quota project %s; run `%s`", profile, quotaProject, command)
	}
	cmd := exec.CommandContext(ctx, "gcloud", "config", "set", "billing/quota_project", project, "--configuration", profile)
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("set gcloud quota project: %w", err)
	}
	if err := gcloudSetupProjectAccess(ctx, cfg, project); err != nil {
		return false, fmt.Errorf("gcloud profile %s still cannot access project %s after setting quota project: %w", profile, project, err)
	}
	return true, nil
}

func maybeEnableGcloudSetupProjectAPIs(ctx context.Context, cfg config, project string, accessErr error, stdout io.Writer) (bool, error) {
	if !gcloudProjectServiceDisabled(accessErr.Error()) {
		return false, nil
	}
	services := []string{
		"serviceusage.googleapis.com",
		"cloudresourcemanager.googleapis.com",
	}
	if err := enableGcloudSetupProjectServices(ctx, cfg, project, services, stdout, "GCP project APIs"); err != nil {
		return false, err
	}
	for i := 0; i < 12; i++ {
		if err := gcloudSetupProjectAccess(ctx, cfg, project); err == nil {
			return true, nil
		} else if !gcloudProjectServiceDisabled(err.Error()) {
			return false, err
		}
		time.Sleep(5 * time.Second)
	}
	if err := gcloudSetupProjectAccess(ctx, cfg, project); err != nil {
		return false, fmt.Errorf("gcloud project %s is still not ready after enabling required APIs: %w", project, err)
	}
	return true, nil
}

func enableGcloudSetupProjectServices(ctx context.Context, cfg config, project string, services []string, stdout io.Writer, label string) error {
	fmt.Fprintf(stdout, "enabling required GCP project APIs for %s\n", project)
	enableCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	args := append([]string{"services", "enable"}, services...)
	args = append(args,
		"--project", project,
		"--configuration", cfg.gcloudConfiguration,
		"--quiet",
	)
	cmd := exec.CommandContext(enableCtx, "gcloud", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("enable %s for %s: %w", label, project, err)
	}
	return waitForGCPServicesEnabled(cfg, project, services, stdout, label)
}

func gcloudSetupProjectAccess(ctx context.Context, cfg config, project string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "gcloud", "projects", "describe", project, "--configuration", cfg.gcloudConfiguration, "--format=value(projectId)")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gcloud project access check failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("gcloud project access check returned no project for %s", project)
	}
	return nil
}

func gcloudProjectServiceDisabled(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "service_disabled") ||
		strings.Contains(message, "api has not been used") ||
		strings.Contains(message, "it is disabled")
}

func runGcloudSetupLogin(ctx context.Context, cfg config, interactive bool, stdin io.Reader, stdout io.Writer) error {
	profile := strings.TrimSpace(cfg.gcloudConfiguration)
	if profile == "" {
		profile = "default"
	}
	loginCommand := fmt.Sprintf("gcloud auth login --configuration %s --no-launch-browser", profile)
	if !interactive {
		return fmt.Errorf("gcloud profile %s needs authentication; run `%s`", profile, loginCommand)
	}
	fmt.Fprintf(stdout, "gcloud profile %s needs authentication or a different account.\n", profile)
	fmt.Fprintf(stdout, "Starting `%s`.\n", loginCommand)
	fmt.Fprintln(stdout, "Open the URL printed by gcloud, finish the OAuth flow, then paste the code if prompted.")
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "login", "--configuration", profile, "--no-launch-browser")
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gcloud auth login failed: %w", err)
	}
	return nil
}

func gcloudSetupAccessToken(ctx context.Context, cfg config) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "gcloud", "auth", "print-access-token", "--configuration", cfg.gcloudConfiguration)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gcloud auth print-access-token failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("gcloud auth print-access-token returned an empty token")
	}
	return token, nil
}

func gcloudAuthNeedsLogin(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "reauthentication failed") ||
		strings.Contains(message, "gcloud auth login") ||
		strings.Contains(message, "no credential") ||
		strings.Contains(message, "invalid_grant") ||
		strings.Contains(message, "login required") ||
		strings.Contains(message, "user_project_denied") ||
		strings.Contains(message, "caller does not have required permission") ||
		strings.Contains(message, "does not have permission to access projects")
}

type setupRegionDialogState struct {
	Provider string
	Profile  string
	Regions  []string
	Selected map[string]bool
	Cursor   int
	Page     int
	Button   int
	Message  string
}

func runSetupRegionDialogWithRaw(reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, provider, profile string, regions []string, selected []string) ([]string, error) {
	rawMode, restore, err := setupDialogRawMode(rawInput)
	if err != nil {
		return nil, err
	}
	defer restore()
	state := setupRegionDialogState{Provider: provider, Profile: profile, Regions: regions, Selected: map[string]bool{}, Button: -1}
	for _, region := range selected {
		if region = strings.TrimSpace(region); region != "" {
			state.Selected[region] = true
		}
	}
	for {
		fmt.Fprint(stdout, renderSetupRegionDialogFrame(state, rawMode))
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errSetupBack
			}
			return nil, err
		}
		switch b {
		case 0x03:
			return nil, errors.New("setup canceled")
		case 0x04:
			if regions, ok := state.deploy(); ok {
				return regions, nil
			}
		case '\r', '\n', ' ', 'x', 'X':
			if regions, ok := state.activate(); ok {
				return regions, nil
			}
			if state.Button == 1 {
				return nil, errSetupBack
			}
		case '\t':
			state.tab()
		case 'q', 'Q':
			return nil, errSetupBack
		case 0x1b:
			next, err := reader.ReadByte()
			if err != nil {
				return nil, errSetupBack
			}
			if next == '[' {
				last, err := reader.ReadByte()
				if err != nil {
					return nil, errSetupBack
				}
				switch last {
				case 'A':
					state.up()
				case 'B':
					state.down()
				}
				continue
			}
			return nil, errSetupBack
		}
	}
}

func (s setupRegionDialogState) visibleRegions() []string {
	if len(s.Regions) == 0 {
		return nil
	}
	start := s.Page * setupRegionDialogItemsPerPage
	if start >= len(s.Regions) {
		start = 0
	}
	end := minSetupDialogInt(start+setupRegionDialogItemsPerPage, len(s.Regions))
	return s.Regions[start:end]
}

func (s *setupRegionDialogState) rows() int {
	rows := len(s.visibleRegions())
	if len(s.Regions) > setupRegionDialogItemsPerPage {
		rows++
	}
	return rows
}

func (s *setupRegionDialogState) up() {
	if s.rows() == 0 {
		return
	}
	s.Button = -1
	s.Message = ""
	if s.Cursor == 0 {
		s.Cursor = s.rows() - 1
		return
	}
	s.Cursor--
}

func (s *setupRegionDialogState) down() {
	if s.rows() == 0 {
		return
	}
	s.Button = -1
	s.Message = ""
	s.Cursor = (s.Cursor + 1) % s.rows()
}

func (s *setupRegionDialogState) tab() {
	s.Message = ""
	if s.Button == 1 {
		s.Button = -1
		s.Cursor = 0
		return
	}
	if s.Button < 0 {
		s.Button = 0
		return
	}
	s.Button = (s.Button + 1) % 2
}

func (s *setupRegionDialogState) activate() ([]string, bool) {
	if s.Button == 0 {
		return s.deploy()
	}
	if s.Button == 1 {
		return nil, false
	}
	visible := s.visibleRegions()
	if s.Cursor < len(visible) {
		region := visible[s.Cursor]
		s.Selected[region] = !s.Selected[region]
		return nil, false
	}
	if len(s.Regions) > setupRegionDialogItemsPerPage && s.Cursor == len(visible) {
		pages := (len(s.Regions) + setupRegionDialogItemsPerPage - 1) / setupRegionDialogItemsPerPage
		s.Page = (s.Page + 1) % pages
		s.Cursor = 0
	}
	return nil, false
}

func (s *setupRegionDialogState) deploy() ([]string, bool) {
	var selected []string
	for _, region := range s.Regions {
		if s.Selected[region] {
			selected = append(selected, region)
		}
	}
	if len(selected) > 0 {
		return selected, true
	}
	s.Message = "Select at least one region before deploy."
	return nil, false
}

func renderSetupRegionDialogFrame(state setupRegionDialogState, rawMode bool) string {
	rendered := renderSetupRegionDialogWithStyle(state, rawMode)
	if !rawMode {
		return rendered
	}
	rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	return "\x1b[?25l\x1b[H\x1b[2J" + rendered
}

func renderSetupRegionDialogWithStyle(state setupRegionDialogState, style bool) string {
	var lines []string
	lines = append(lines,
		"+------------------------------------------------------------+",
		"|                    BUCKETGIT SETUP                         |",
		"+------------------------------------------------------------+",
		setupDialogRow(fmt.Sprintf("%s profile %s regions", state.Provider, state.Profile)),
		"|                                                            |",
	)
	visible := state.visibleRegions()
	for i, region := range visible {
		marker := " "
		if state.Button < 0 && state.Cursor == i {
			marker = ">"
		}
		checked := " "
		if state.Selected[region] {
			checked = "x"
		}
		lines = append(lines, setupDialogRowStyled(fmt.Sprintf("%s [%s] %s", marker, checked, region), setupDialogSectionStyle(style, state.Button < 0)))
	}
	if len(state.Regions) > setupRegionDialogItemsPerPage {
		marker := " "
		if state.Button < 0 && state.Cursor == len(visible) {
			marker = ">"
		}
		lines = append(lines, setupDialogRowStyled(fmt.Sprintf("%s show next regions", marker), setupDialogSectionStyle(style, state.Button < 0)))
	}
	if state.Message != "" {
		lines = append(lines, setupDialogRowStyled(state.Message, setupDialogANSI(style, "33")))
	}
	okStyle := ""
	cancelStyle := ""
	if style && state.Button == 0 {
		okStyle = "\x1b[44;97m"
	}
	if style && state.Button == 1 {
		cancelStyle = "\x1b[44;97m"
	}
	lines = append(lines,
		"|                                                            |",
		"+------------------------------------------------------------+",
		setupDialogRow(setupDialogButton("[ OK ]", okStyle)+"    "+setupDialogButton("[ Cancel ]", cancelStyle)),
		setupDialogRow("Controls: arrows move  Space/Enter select  Ctrl-D deploy"),
		setupDialogRow("Tab rows/buttons  Esc back  Ctrl-C cancel"),
		"+------------------------------------------------------------+",
	)
	return strings.Join(lines, "\n") + "\n"
}

func runSetupDialog(stdin io.Reader, stdout io.Writer, initial setupSelection) (setupSelection, error) {
	reader, ok := stdin.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(stdin)
	}
	return runSetupDialogWithRaw(reader, stdin, stdout, initial)
}

func runSetupDialogWithRaw(reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, initial setupSelection) (setupSelection, error) {
	rawMode, restore, err := setupDialogRawMode(rawInput)
	if err != nil {
		return setupSelection{}, err
	}
	defer restore()

	state := setupDialogState{
		profiles:                initial.Profiles,
		keys:                    initial.Keys,
		identityName:            initial.IdentityName,
		identityEmail:           initial.IdentityEmail,
		initialIdentityName:     initial.IdentityName,
		initialIdentityEmail:    initial.IdentityEmail,
		createProviders:         setupAvailableCreateProviders(),
		defaultCreate:           initial.DefaultCreate,
		defaultCreateByProvider: initial.DefaultCreateByProvider,
		selectedProfiles:        make([]bool, len(initial.Profiles)),
		selectedKeys:            make([]bool, len(initial.Keys)),
		providerPages:           map[string]int{},
		button:                  -1,
	}
	for i := range initial.Keys {
		state.selectedKeys[i] = true
	}
	for {
		fmt.Fprint(stdout, renderSetupDialogFrame(state, rawMode))
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return setupSelection{}, errors.New("setup canceled")
			}
			return setupSelection{}, err
		}
		if state.discardingCreatePaste || state.discardingIdentityPaste {
			if b == '\r' || b == '\n' || (b >= 32 && b <= 126) {
				continue
			}
			state.discardingCreatePaste = false
			state.discardingIdentityPaste = false
		}
		switch b {
		case 0x03:
			return setupSelection{}, errors.New("setup canceled")
		case 0x04:
			if state.editingCreate {
				state.editingCreate = false
				state.editOriginal = ""
				continue
			}
			if state.editingIdentity {
				state.editingIdentity = false
				state.editOriginal = ""
				continue
			}
			if state.createProvider != "" {
				if selected, ok := state.deployCreateProfile(); ok {
					return selected, nil
				}
				continue
			}
			if selected, ok := state.deploy(); ok {
				return selected, nil
			}
		case '\r', '\n':
			if state.editingCreate {
				state.editingCreate = false
				state.editOriginal = ""
				state.message = ""
				continue
			}
			if state.editingIdentity {
				state.editingIdentity = false
				state.editOriginal = ""
				state.message = ""
				continue
			}
			if selected, ok := state.activate(); ok {
				return selected, nil
			} else if state.button == 1 {
				return setupSelection{}, errors.New("setup canceled")
			}
		case ' ', 'x', 'X':
			if state.editingCreate {
				state.appendCreateByte(b)
				continue
			}
			if state.editingIdentity {
				state.appendIdentityByte(b)
				continue
			}
			if selected, ok := state.activate(); ok {
				return selected, nil
			} else if state.button == 1 {
				return setupSelection{}, errors.New("setup canceled")
			}
		case '\t':
			state.discardingCreatePaste = false
			state.tab()
		case 'q', 'Q':
			return setupSelection{}, errors.New("setup canceled")
		case 0x7f, 0x08:
			state.discardingCreatePaste = false
			if state.editingCreate {
				state.backspaceCreate()
			}
			if state.editingIdentity {
				state.backspaceIdentity()
			}
		case 0x1b:
			state.discardingCreatePaste = false
			state.discardingIdentityPaste = false
			if state.editingCreate {
				state.setCreateFieldValue(state.editOriginal)
				state.editingCreate = false
				state.editOriginal = ""
				state.message = ""
				continue
			}
			if state.editingIdentity {
				state.setIdentityFieldValue(state.editOriginal)
				state.editingIdentity = false
				state.editOriginal = ""
				state.message = ""
				continue
			}
			next, err := reader.ReadByte()
			if err != nil {
				if state.createProvider != "" {
					state.cancelCreateProfile()
					continue
				}
				return setupSelection{}, errors.New("setup canceled")
			}
			if next == '[' {
				last, err := reader.ReadByte()
				if err != nil {
					return setupSelection{}, errors.New("setup canceled")
				}
				switch last {
				case 'A':
					state.up()
				case 'B':
					state.down()
				}
				continue
			}
			if state.createProvider != "" {
				state.cancelCreateProfile()
				continue
			}
			return setupSelection{}, errors.New("setup canceled")
		default:
			if state.editingCreate && b >= 32 && b <= 126 {
				state.appendCreateByte(b)
			}
			if state.editingIdentity && b >= 32 && b <= 126 {
				state.appendIdentityByte(b)
			}
		}
	}
}

func setupDialogRawMode(stdin io.Reader) (bool, func(), error) {
	file, ok := stdin.(*os.File)
	if !ok {
		return false, func() {}, nil
	}
	fd := int(file.Fd())
	if !term.IsTerminal(fd) {
		return false, func() {}, nil
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return false, nil, err
	}
	return true, func() {
		_ = term.Restore(fd, state)
		fmt.Fprint(os.Stdout, "\x1b[?25h")
	}, nil
}

func renderSetupDialogFrame(state setupDialogState, rawMode bool) string {
	rendered := renderSetupDialogWithStyle(state, rawMode)
	if !rawMode {
		return rendered
	}
	rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	return "\x1b[?25l\x1b[H\x1b[2J" + rendered
}

type setupDialogState struct {
	profiles                []setupProfile
	keys                    []setupSSHKey
	createProviders         []string
	selectedProfiles        []bool
	selectedKeys            []bool
	providerPages           map[string]int
	identityName            string
	identityEmail           string
	initialIdentityName     string
	initialIdentityEmail    string
	cursor                  int
	button                  int
	message                 string
	createProvider          string
	createName              string
	createAccessKey         string
	createSecretKey         string
	createRegion            string
	defaultCreate           string
	defaultCreateByProvider map[string]string
	editingCreate           bool
	discardingCreatePaste   bool
	editingIdentity         bool
	discardingIdentityPaste bool
	editOriginal            string
}

type setupDialogVisibleItem struct {
	Kind         string
	Provider     string
	ProfileIndex int
	KeyIndex     int
	Label        string
}

func setupAvailableCreateProviders() []string {
	var providers []string
	if _, err := exec.LookPath("gcloud"); err == nil {
		providers = append(providers, "gcs")
	}
	if _, err := exec.LookPath("aws"); err == nil {
		providers = append(providers, "s3")
	}
	return providers
}

func (s setupDialogState) selection() setupSelection {
	var profiles []setupProfile
	for i, profile := range s.profiles {
		if i < len(s.selectedProfiles) && s.selectedProfiles[i] {
			profiles = append(profiles, profile)
		}
	}
	var keys []setupSSHKey
	for i, key := range s.keys {
		if i < len(s.selectedKeys) && s.selectedKeys[i] {
			keys = append(keys, key)
		}
	}
	return setupSelection{
		Profiles:      profiles,
		Keys:          keys,
		IdentityName:  strings.TrimSpace(s.identityName),
		IdentityEmail: strings.TrimSpace(s.identityEmail),
	}
}

func (s *setupDialogState) rows() int {
	if s.createProvider != "" {
		return len(s.createFields())
	}
	return len(s.visibleItems())
}

func (s *setupDialogState) up() {
	if s.editingCreate || s.editingIdentity {
		return
	}
	if s.rows() == 0 {
		return
	}
	s.button = -1
	s.message = ""
	if s.cursor == 0 {
		s.cursor = s.rows() - 1
		return
	}
	s.cursor--
}

func (s *setupDialogState) down() {
	if s.editingCreate || s.editingIdentity {
		return
	}
	if s.rows() == 0 {
		return
	}
	s.button = -1
	s.message = ""
	s.cursor = (s.cursor + 1) % s.rows()
}

func (s *setupDialogState) activate() (setupSelection, bool) {
	if s.createProvider != "" {
		if s.button == 0 {
			return s.deployCreateProfile()
		}
		if s.button == 1 {
			s.cancelCreateProfile()
			return setupSelection{}, false
		}
		s.editingCreate = true
		s.editOriginal = s.createFieldValue()
		s.message = ""
		return setupSelection{}, false
	}
	if s.button == 0 {
		return s.deploy()
	}
	if s.button == 1 {
		return setupSelection{}, false
	}
	items := s.visibleItems()
	if s.cursor < 0 || s.cursor >= len(items) {
		return setupSelection{}, false
	}
	s.message = ""
	item := items[s.cursor]
	switch item.Kind {
	case "identity-name", "identity-email":
		s.editingIdentity = true
		s.editOriginal = s.identityFieldValue()
		s.message = ""
	case "create-profile":
		s.createProvider = item.Provider
		s.createName = firstNonEmpty(s.defaultCreateByProvider[item.Provider], s.defaultCreate)
		s.editingCreate = true
		s.editOriginal = s.createFieldValue()
		s.cursor = 0
		s.button = -1
		return setupSelection{}, false
	case "profile":
		s.selectedProfiles[item.ProfileIndex] = !s.selectedProfiles[item.ProfileIndex]
	case "more":
		s.nextProviderPage(item.Provider)
	case "key":
		if item.KeyIndex >= 0 && item.KeyIndex < len(s.keys) {
			s.selectedKeys[item.KeyIndex] = !s.selectedKeys[item.KeyIndex]
		}
	}
	return setupSelection{}, false
}

func (s *setupDialogState) deploy() (setupSelection, bool) {
	selected := s.selection()
	if len(selected.Profiles) == 0 && !s.identityChanged() {
		s.message = "Select a cloud profile or change identity before deploy."
		return setupSelection{}, false
	}
	return selected, true
}

func (s setupDialogState) identityChanged() bool {
	return strings.TrimSpace(s.identityName) != strings.TrimSpace(s.initialIdentityName) ||
		strings.TrimSpace(s.identityEmail) != strings.TrimSpace(s.initialIdentityEmail)
}

func (s *setupDialogState) deployCreateProfile() (setupSelection, bool) {
	name := strings.TrimSpace(s.createName)
	if name == "" {
		s.message = "Enter a profile name before OK."
		return setupSelection{}, false
	}
	if !setupProfileNamePattern.MatchString(name) {
		s.message = "Profile name can use letters, numbers, dot, dash, underscore, @, and +."
		return setupSelection{}, false
	}
	if s.createProvider == "s3" {
		accessKey := strings.TrimSpace(s.createAccessKey)
		secretKey := strings.TrimSpace(s.createSecretKey)
		region := strings.TrimSpace(s.createRegion)
		if accessKey == "" {
			s.message = "Enter an AWS access key ID before OK."
			return setupSelection{}, false
		}
		if !setupAWSAccessKeyPattern.MatchString(accessKey) {
			s.message = "AWS access key ID format looks invalid."
			return setupSelection{}, false
		}
		if secretKey == "" {
			s.message = "Enter an AWS secret access key before OK."
			return setupSelection{}, false
		}
		if strings.ContainsAny(secretKey, " \t\r\n") || len(secretKey) < 20 {
			s.message = "AWS secret access key format looks invalid."
			return setupSelection{}, false
		}
		if region != "" && !setupAWSRegionPattern.MatchString(region) {
			s.message = "AWS region format looks invalid."
			return setupSelection{}, false
		}
	}
	return setupSelection{
		Action:          "create-profile",
		CreateProvider:  s.createProvider,
		CreateName:      name,
		CreateAccessKey: strings.TrimSpace(s.createAccessKey),
		CreateSecretKey: strings.TrimSpace(s.createSecretKey),
		CreateRegion:    strings.TrimSpace(s.createRegion),
	}, true
}

func (s *setupDialogState) cancelCreateProfile() {
	s.createProvider = ""
	s.createName = ""
	s.createAccessKey = ""
	s.createSecretKey = ""
	s.createRegion = ""
	s.editingCreate = false
	s.discardingCreatePaste = false
	s.editOriginal = ""
	s.button = -1
	s.cursor = 0
	s.message = ""
}

func (s *setupDialogState) appendCreateByte(b byte) {
	if s.discardingCreatePaste {
		if b == '\r' || b == '\n' || (b >= 32 && b <= 126) {
			return
		}
		s.discardingCreatePaste = false
	}
	s.message = ""
	if b == '\r' || b == '\n' {
		s.editingCreate = false
		s.editOriginal = ""
		s.discardingCreatePaste = true
		return
	}
	value := s.createFieldValue()
	if len(value) >= 48 {
		return
	}
	if b < 32 || b > 126 {
		return
	}
	s.setCreateFieldValue(value + string(b))
}

func (s *setupDialogState) backspaceCreate() {
	s.message = ""
	value := s.createFieldValue()
	if len(value) == 0 {
		return
	}
	s.setCreateFieldValue(value[:len(value)-1])
}

func (s setupDialogState) createFields() []string {
	if s.createProvider == "s3" {
		return []string{"Profile name", "AWS Access Key ID", "AWS Secret Access Key", "Default region name"}
	}
	return []string{"Profile name"}
}

func (s setupDialogState) createFieldValue() string {
	return s.createFieldValueForRow(s.cursor)
}

func (s setupDialogState) createFieldValueForRow(row int) string {
	switch row {
	case 1:
		return s.createAccessKey
	case 2:
		return s.createSecretKey
	case 3:
		return s.createRegion
	default:
		return s.createName
	}
}

func (s *setupDialogState) setCreateFieldValue(value string) {
	switch s.cursor {
	case 1:
		s.createAccessKey = value
	case 2:
		s.createSecretKey = value
	case 3:
		s.createRegion = value
	default:
		s.createName = value
	}
}

func (s *setupDialogState) tab() {
	if s.editingCreate {
		s.editingCreate = false
		s.editOriginal = ""
	}
	if s.editingIdentity {
		s.editingIdentity = false
		s.editOriginal = ""
	}
	s.message = ""
	if s.createProvider != "" {
		if s.button == 1 {
			s.button = -1
			return
		}
		if s.button < 0 {
			s.button = 0
			return
		}
		s.button = (s.button + 1) % 2
		return
	}
	if s.button == 1 {
		s.button = -1
		s.cursor = 0
		return
	}
	items := s.visibleItems()
	if len(items) == 0 {
		s.button = (s.button + 1) % 2
		return
	}
	currentProvider := ""
	if s.cursor >= 0 && s.cursor < len(items) {
		currentProvider = items[s.cursor].Provider
	}
	for _, provider := range setupDialogProviderOrder(s.profiles, s.createProviders) {
		if currentProvider == "" || provider > currentProvider {
			if idx := firstSetupDialogProviderItem(items, provider); idx >= 0 {
				s.cursor = idx
				s.button = 0
				return
			}
		}
	}
	if currentProvider != "ssh" {
		if currentProvider != "identity" {
			if idx := firstSetupDialogProviderItem(items, "identity"); idx >= 0 {
				s.cursor = idx
				s.button = -1
				return
			}
		}
		if idx := firstSetupDialogProviderItem(items, "ssh"); idx >= 0 {
			s.cursor = idx
			s.button = -1
			return
		}
	}
	if s.button < 0 {
		s.button = 0
	} else {
		s.button = (s.button + 1) % 2
	}
}

func firstSetupDialogProviderItem(items []setupDialogVisibleItem, provider string) int {
	for i, item := range items {
		if item.Provider == provider {
			return i
		}
	}
	return -1
}

func (s *setupDialogState) nextProviderPage(provider string) {
	indices := s.profileIndicesByProvider()[provider]
	if len(indices) <= setupDialogProfilesPerProvider {
		return
	}
	pages := (len(indices) + setupDialogProfilesPerProvider - 1) / setupDialogProfilesPerProvider
	s.ensureProviderPages()
	s.providerPages[provider] = (s.providerPages[provider] + 1) % pages
	items := s.visibleItems()
	if idx := firstSetupDialogProviderItem(items, provider); idx >= 0 {
		s.cursor = idx
	}
}

func (s *setupDialogState) ensureProviderPages() {
	if s.providerPages == nil {
		s.providerPages = map[string]int{}
	}
}

func (s setupDialogState) visibleItems() []setupDialogVisibleItem {
	var items []setupDialogVisibleItem
	byProvider := s.profileIndicesByProvider()
	for _, provider := range setupDialogProviderOrder(s.profiles, s.createProviders) {
		indices := byProvider[provider]
		page := 0
		if s.providerPages != nil {
			page = s.providerPages[provider]
		}
		start := page * setupDialogProfilesPerProvider
		if start >= len(indices) {
			start = 0
		}
		end := start + setupDialogProfilesPerProvider
		if end > len(indices) {
			end = len(indices)
		}
		for _, profileIndex := range indices[start:end] {
			items = append(items, setupDialogVisibleItem{Kind: "profile", Provider: provider, ProfileIndex: profileIndex})
		}
		if len(indices) > setupDialogProfilesPerProvider {
			nextEnd := minSetupDialogInt(end+setupDialogProfilesPerProvider, len(indices))
			nextStart := end + 1
			if end >= len(indices) {
				nextStart = 1
				nextEnd = minSetupDialogInt(setupDialogProfilesPerProvider, len(indices))
			}
			items = append(items, setupDialogVisibleItem{
				Kind:     "more",
				Provider: provider,
				Label:    fmt.Sprintf("show next %s profiles (%d-%d of %d)", setupProviderLabel(provider), nextStart, nextEnd, len(indices)),
			})
		}
		if setupDialogCanCreateProvider(s.createProviders, provider) {
			items = append(items, setupDialogVisibleItem{
				Kind:     "create-profile",
				Provider: provider,
				Label:    fmt.Sprintf("create new %s profile", setupProviderLabel(provider)),
			})
		}
	}
	items = append(items, setupDialogVisibleItem{Kind: "identity-name", Provider: "identity", Label: "Name"})
	items = append(items, setupDialogVisibleItem{Kind: "identity-email", Provider: "identity", Label: "Email"})
	for i := range s.keys {
		items = append(items, setupDialogVisibleItem{Kind: "key", Provider: "ssh", KeyIndex: i})
	}
	return items
}

func setupDialogCanCreateProvider(providers []string, provider string) bool {
	for _, item := range providers {
		if item == provider {
			return true
		}
	}
	return false
}

func (s setupDialogState) profileIndicesByProvider() map[string][]int {
	byProvider := map[string][]int{}
	for i, profile := range s.profiles {
		byProvider[profile.Provider] = append(byProvider[profile.Provider], i)
	}
	return byProvider
}

func setupDialogProviderOrder(profiles []setupProfile, createProviders []string) []string {
	seen := map[string]struct{}{}
	for _, profile := range profiles {
		seen[profile.Provider] = struct{}{}
	}
	for _, provider := range createProviders {
		seen[provider] = struct{}{}
	}
	var order []string
	for _, provider := range []string{"gcs", "s3"} {
		if _, ok := seen[provider]; ok {
			order = append(order, provider)
			delete(seen, provider)
		}
	}
	var rest []string
	for provider := range seen {
		rest = append(rest, provider)
	}
	sort.Strings(rest)
	return append(order, rest...)
}

func minSetupDialogInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func renderSetupDialog(state setupDialogState) string {
	return renderSetupDialogWithStyle(state, false)
}

func renderSetupDialogWithStyle(state setupDialogState, style bool) string {
	if state.createProvider != "" {
		return renderSetupCreateProfileDialogWithStyle(state, style)
	}
	var lines []string
	lines = append(lines,
		"+------------------------------------------------------------+",
		"|                    BUCKETGIT SETUP                         |",
		"+------------------------------------------------------------+",
		"| Select cloud profiles to configure                         |",
		"|                                                            |",
	)
	activeSection := state.activeSection()
	itemIndex := 0
	byProvider := state.profileIndicesByProvider()
	for _, provider := range setupDialogProviderOrder(state.profiles, state.createProviders) {
		lines = append(lines, setupDialogRowStyled(setupProviderLabel(provider)+" profiles", setupDialogSectionStyle(style, activeSection == provider)))
		providerItems := setupDialogProviderVisibleItems(state, provider)
		for _, item := range providerItems {
			marker := " "
			if state.cursor == itemIndex {
				marker = ">"
			}
			switch item.Kind {
			case "create-profile":
				lines = append(lines, setupDialogRowStyled(fmt.Sprintf("%s     %s", marker, item.Label), setupDialogSectionStyle(style, activeSection == provider)))
			case "profile":
				profile := state.profiles[item.ProfileIndex]
				checked := " "
				if item.ProfileIndex < len(state.selectedProfiles) && state.selectedProfiles[item.ProfileIndex] {
					checked = "x"
				}
				detail := firstNonEmpty(profile.ProjectID, profile.AccountID, profile.Account, profile.ARN)
				rowStyle := setupDialogSectionStyle(style, activeSection == provider)
				if profile.Existing && style {
					rowStyle += "\x1b[1;97m"
				}
				lines = append(lines, setupDialogRowStyled(fmt.Sprintf("%s [%s] %s:%-12s %-34s", marker, checked, setupProviderLabel(profile.Provider), setupProfileDisplayName(profile), detail), rowStyle))
			case "more":
				lines = append(lines, setupDialogRowStyled(fmt.Sprintf("%s     %s", marker, item.Label), setupDialogSectionStyle(style, activeSection == provider)))
			}
			itemIndex++
		}
		if len(byProvider[provider]) == 0 {
			lines = append(lines, setupDialogRowStyled("  none", setupDialogSectionStyle(style, activeSection == provider)))
		}
	}
	lines = append(lines, setupDialogRow(""))
	lines = append(lines, setupDialogRowStyled("Identity", setupDialogSectionStyle(style, activeSection == "identity")))
	for _, field := range []struct {
		kind  string
		label string
		value string
	}{
		{kind: "identity-name", label: "Name", value: state.identityName},
		{kind: "identity-email", label: "Email", value: state.identityEmail},
	} {
		marker := " "
		if state.cursor == itemIndex {
			marker = ">"
		}
		active := activeSection == "identity" && state.button < 0 && state.cursor == itemIndex
		inputStyle := setupDialogSectionStyle(style, activeSection == "identity")
		if style && state.editingIdentity && active {
			inputStyle += "\x1b[44;97m"
		}
		lines = append(lines, setupDialogRowStyled(fmt.Sprintf("%s %-5s [%s]", marker, field.label, initDialogInputValue(field.value, 48, state.editingIdentity && active, style)), inputStyle))
		itemIndex++
	}
	lines = append(lines, setupDialogRow(""))
	lines = append(lines, setupDialogRowStyled("Owner SSH keys", setupDialogSectionStyle(style, activeSection == "ssh")))
	for i, key := range state.keys {
		marker := " "
		if state.cursor == itemIndex {
			marker = ">"
		}
		checked := " "
		if i < len(state.selectedKeys) && state.selectedKeys[i] {
			checked = "x"
		}
		label := firstNonEmpty(key.Comment, key.Source, shortSetupKey(key.PublicKey))
		lines = append(lines, setupDialogRowStyled(fmt.Sprintf("%s [%s] %-54s", marker, checked, label), setupDialogSectionStyle(style, activeSection == "ssh")))
		itemIndex++
	}
	if len(state.keys) == 0 {
		lines = append(lines, setupDialogRowStyled("  no SSH public keys found", setupDialogSectionStyle(style, activeSection == "ssh")))
	}
	ok := "[ OK ]"
	exit := "[ Exit ]"
	okStyle := ""
	exitStyle := ""
	if style && state.button == 0 {
		okStyle = "\x1b[44;97m"
	}
	if state.button == 1 {
		ok = "  OK  "
		exit = "[ EXIT ]"
		if style {
			exitStyle = "\x1b[44;97m"
		}
	}
	if state.message != "" {
		lines = append(lines, setupDialogRowStyled(state.message, setupDialogANSI(style, "33")))
	}
	lines = append(lines,
		"|                                                            |",
		"+------------------------------------------------------------+",
		setupDialogRow(setupDialogButton(ok, okStyle)+"    "+setupDialogButton(exit, exitStyle)),
		setupDialogRow("Controls: arrows move  Space/Enter select/edit  Ctrl-D deploy"),
		setupDialogRow("Tab sections/buttons  Esc cancel/revert"),
		"+------------------------------------------------------------+",
	)
	return strings.Join(lines, "\n") + "\n"
}

func renderSetupCreateProfileDialogWithStyle(state setupDialogState, style bool) string {
	provider := strings.ToUpper(setupProviderLabel(state.createProvider))
	var lines []string
	lines = append(lines,
		"+------------------------------------------------------------+",
		"|                    BUCKETGIT SETUP                         |",
		"+------------------------------------------------------------+",
		setupDialogRow(fmt.Sprintf("Create %s profile", provider)),
		"|                                                            |",
	)
	for i, label := range state.createFields() {
		active := state.button < 0 && state.cursor == i
		inputStyle := setupDialogSectionStyle(style, active)
		if style && state.editingCreate && active {
			inputStyle += "\x1b[44;97m"
		}
		marker := " "
		if active {
			marker = ">"
		}
		value := state.createFieldValueForRow(i)
		if state.createProvider == "s3" && i == 2 {
			value = strings.Repeat("*", len(value))
		}
		lines = append(lines, setupDialogRowStyled(fmt.Sprintf("%s %-21s [%s]", marker, label, initDialogInputValue(value, 28, state.editingCreate && active, style)), inputStyle))
	}
	if state.message != "" {
		lines = append(lines, setupDialogRowStyled(state.message, setupDialogANSI(style, "33")))
	}
	okStyle := ""
	cancelStyle := ""
	if style && state.button == 0 {
		okStyle = "\x1b[44;97m"
	}
	if style && state.button == 1 {
		cancelStyle = "\x1b[44;97m"
	}
	lines = append(lines,
		"|                                                            |",
		"+------------------------------------------------------------+",
		setupDialogRow(setupDialogButton("[ OK ]", okStyle)+"    "+setupDialogButton("[ Cancel ]", cancelStyle)),
		setupDialogRow("Enter edits/saves profile  Ctrl-D OK"),
		setupDialogRow("Tab field/buttons  Esc back  Ctrl-C cancel"),
		"+------------------------------------------------------------+",
	)
	return strings.Join(lines, "\n") + "\n"
}

func (s setupDialogState) activeSection() string {
	if s.createProvider != "" {
		return s.createProvider
	}
	if s.button >= 0 {
		return "buttons"
	}
	items := s.visibleItems()
	if s.cursor >= 0 && s.cursor < len(items) {
		return items[s.cursor].Provider
	}
	return ""
}

func (s setupDialogState) identityFieldValue() string {
	items := s.visibleItems()
	if s.cursor < 0 || s.cursor >= len(items) {
		return ""
	}
	switch items[s.cursor].Kind {
	case "identity-email":
		return s.identityEmail
	default:
		return s.identityName
	}
}

func (s *setupDialogState) setIdentityFieldValue(value string) {
	items := s.visibleItems()
	if s.cursor < 0 || s.cursor >= len(items) {
		return
	}
	switch items[s.cursor].Kind {
	case "identity-email":
		s.identityEmail = value
	default:
		s.identityName = value
	}
}

func (s *setupDialogState) appendIdentityByte(b byte) {
	if s.discardingIdentityPaste {
		if b == '\r' || b == '\n' || (b >= 32 && b <= 126) {
			return
		}
		s.discardingIdentityPaste = false
	}
	s.message = ""
	if b == '\r' || b == '\n' {
		s.editingIdentity = false
		s.editOriginal = ""
		s.discardingIdentityPaste = true
		return
	}
	value := s.identityFieldValue()
	if len(value) >= 80 {
		return
	}
	if b < 32 || b > 126 {
		return
	}
	s.setIdentityFieldValue(value + string(b))
}

func (s *setupDialogState) backspaceIdentity() {
	s.message = ""
	value := s.identityFieldValue()
	if len(value) == 0 {
		return
	}
	s.setIdentityFieldValue(value[:len(value)-1])
}

func setupDialogSectionStyle(style, active bool) string {
	if !style || !active {
		return ""
	}
	return "\x1b[48;5;236m"
}

func setupDialogANSI(style bool, code string) string {
	if !style {
		return ""
	}
	return "\x1b[" + code + "m"
}

func setupDialogButton(text, style string) string {
	if style == "" {
		return text
	}
	return style + text + "\x1b[0m"
}

func stripSetupANSI(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == 0x1b && i+1 < len(value) && value[i+1] == '[' {
			i += 2
			for i < len(value) && (value[i] < '@' || value[i] > '~') {
				i++
			}
			continue
		}
		b.WriteByte(value[i])
	}
	return b.String()
}

func setupDialogProviderVisibleItems(state setupDialogState, provider string) []setupDialogVisibleItem {
	var items []setupDialogVisibleItem
	for _, item := range state.visibleItems() {
		if item.Provider == provider {
			items = append(items, item)
		}
	}
	return items
}

func setupDialogRow(text string) string {
	visible := stripSetupANSI(text)
	if len(visible) > 58 {
		text = visible[:58]
		visible = text
	}
	return "| " + text + strings.Repeat(" ", 58-len(visible)) + " |"
}

func setupDialogRowStyled(text, style string) string {
	visible := text
	if len(visible) > 58 {
		visible = visible[:58]
		text = visible
	}
	if style != "" {
		text = style + text + "\x1b[0m"
	}
	return "| " + text + strings.Repeat(" ", 58-len(visible)) + " |"
}

func setupProviderLabel(provider string) string {
	if provider == "s3" {
		return "aws"
	}
	return "gcp"
}

func setupProfileDisplayName(profile setupProfile) string {
	if profile.Existing && strings.TrimSpace(profile.Region) != "" {
		return profile.Name + "." + profile.Region
	}
	return profile.Name
}

func shortSetupKey(key string) string {
	fields := strings.Fields(key)
	if len(fields) < 2 {
		return key
	}
	part := fields[1]
	if len(part) > 16 {
		part = part[:16]
	}
	return fields[0] + " " + part
}

func brokerUpsertOwners(brokerURL string, publicKeys []string) error {
	return brokerPost(brokerURL, "/owners/upsert", brokerOwnerRequest{User: "owner", Role: "owner", PublicKeys: publicKeys}, nil)
}

func upsertGlobalGCPProfile(cfg globalConfig, profile globalGCPProfile) globalConfig {
	for i, existing := range cfg.GCPProfiles {
		if existing.Name == profile.Name {
			profile.Regions = mergeGlobalProfileRegions(existing.Regions, profile.Regions)
			cfg.GCPProfiles[i] = profile
			return cfg
		}
	}
	cfg.GCPProfiles = append(cfg.GCPProfiles, profile)
	return cfg
}

func upsertGlobalAWSProfile(cfg globalConfig, profile globalAWSProfile) globalConfig {
	for i, existing := range cfg.AWSProfiles {
		if existing.Name == profile.Name {
			profile.Regions = mergeGlobalProfileRegions(existing.Regions, profile.Regions)
			cfg.AWSProfiles[i] = profile
			return cfg
		}
	}
	cfg.AWSProfiles = append(cfg.AWSProfiles, profile)
	return cfg
}

func mergeGlobalProfileRegions(existing, incoming []globalProfileRegion) []globalProfileRegion {
	out := append([]globalProfileRegion{}, existing...)
	for _, next := range incoming {
		matched := false
		for i := range out {
			if out[i].Name == next.Name {
				out[i] = next
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, next)
		}
	}
	return out
}
