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

const setupProbeTimeout = 10 * time.Second
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
	action     string
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

type setupConfiguredBroker struct {
	Provider  string
	Profile   string
	Region    string
	BrokerURL string
	Detail    string
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
	fmt.Fprintln(stdout, "discovering broker profiles...")
	profiles, err := discoverSetupProfiles(ctx)
	if err != nil {
		return err
	}
	if opts.provider == "local" {
		name := "default"
		if len(opts.profiles) > 0 && strings.TrimSpace(opts.profiles[0]) != "" {
			name = strings.TrimSpace(opts.profiles[0])
		}
		profiles = append(profiles, setupProfile{Provider: "local", Name: name, Region: firstNonEmpty(opts.region, "default")})
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
	interactiveBrokerMenu := !opts.yes && len(opts.profiles) == 0 && opts.provider == "" && opts.region == ""
	if interactiveBrokerMenu {
	brokerMenu:
		for {
			action, broker, err := runSetupBrokerHomeWithRaw(interactiveReader, stdin, stdout, configuredSetupBrokers(global), setupAvailableCreateProviders())
			if err != nil {
				return err
			}
			if action == "broker" {
				for {
					action, err = runSetupBrokerActionWithRaw(interactiveReader, stdin, stdout, broker)
					if err != nil {
						return err
					}
					if action == "back" {
						continue brokerMenu
					}
					if action != "manage" {
						break
					}
					err = runSetupBrokerManageWithRaw(base, interactiveReader, stdin, stdout, broker)
					if errors.Is(err, errSetupBack) {
						continue
					}
					if err != nil {
						return err
					}
					return setupReturnToMenuOrQuit(ctx, base, args, stdin, stdout, interactiveReader)
				}
			}
			switch action {
			case "cancel":
				return nil
			case "back":
				continue brokerMenu
			case "manage":
				if err := runSetupBrokerManageWithRaw(base, interactiveReader, stdin, stdout, broker); err != nil {
					return err
				}
				return setupReturnToMenuOrQuit(ctx, base, args, stdin, stdout, interactiveReader)
			case "delete":
				ok, err := runSetupBrokerDeleteConfirmWithRaw(interactiveReader, stdin, stdout, broker)
				if err != nil {
					return err
				}
				if !ok {
					continue brokerMenu
				}
				if err := brokerDeleteCommand(ctx, base, []string{"--provider", setupProviderLabel(broker.Provider), "--profile", broker.Profile, "--region", broker.Region, "--yes"}, stdin, stdout); err != nil {
					return err
				}
				return setupReturnToMenuOrQuit(ctx, base, args, stdin, stdout, interactiveReader)
			case "update":
				opts.action = "update"
				opts.provider = broker.Provider
				opts.profiles = []string{broker.Profile}
				opts.region = broker.Region
				profiles = filterSetupProfiles(markConfiguredSetupProfiles(profilesWithoutConfiguredExpansion(profiles), global), opts.provider, opts.profiles, opts.region)
				break brokerMenu
			case "new":
				opts.action = "new"
				break brokerMenu
			default:
				opts.action = "upsert"
				break brokerMenu
			}
		}
	}
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
		if opts.action == "update" {
			selection.Profiles = profiles
			selection.Keys = nil
		} else if opts.yes {
			selection.Profiles = profiles
		} else if !opts.yes {
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
			if interactiveBrokerMenu {
				return setupReturnToMenuOrQuit(ctx, base, args, stdin, stdout, interactiveReader)
			}
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
			} else if profile.Provider == "local" {
				profile.Region = firstNonEmpty(profile.Region, opts.region, "default")
				if err := setupProvisionSelectedProfile(base, path, now, profile, opts, publicKeys, &global, stdout); err != nil {
					return err
				}
				continue
			}
		}
		if err := writeGlobalConfig(path, global); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote BucketGit config %s\n", path)
		if opts.action != "update" {
			fmt.Fprintln(stdout)
			fmt.Fprintln(stdout, "Next steps:")
			fmt.Fprintln(stdout, "  bgit init")
			fmt.Fprintln(stdout, "  bgit init --noninteractive --repo my-repo --profile PROFILE")
			fmt.Fprintln(stdout, "  git push -u origin main")
		}
		if interactiveBrokerMenu {
			return setupReturnToMenuOrQuit(ctx, base, args, stdin, stdout, interactiveReader)
		}
		return nil
	}
}

func setupReturnToMenuOrQuit(ctx context.Context, base config, args []string, stdin io.Reader, stdout io.Writer, reader *bufio.Reader) error {
	fmt.Fprintln(stdout)
	fmt.Fprint(stdout, "Press <enter> to return to the menu or press <q> to quit. ")
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(line), "q") {
		return nil
	}
	return setupCommand(ctx, base, args, stdin, stdout)
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

func configuredSetupBrokers(cfg globalConfig) []setupConfiguredBroker {
	var brokers []setupConfiguredBroker
	for _, profile := range cfg.GCPProfiles {
		for _, region := range profile.Regions {
			if strings.TrimSpace(region.BrokerURL) == "" {
				continue
			}
			brokers = append(brokers, setupConfiguredBroker{
				Provider:  "gcs",
				Profile:   profile.Name,
				Region:    region.Name,
				BrokerURL: region.BrokerURL,
				Detail:    firstNonEmpty(profile.ProjectID, profile.Account, profile.ServiceAccount),
			})
		}
	}
	for _, profile := range cfg.AWSProfiles {
		for _, region := range profile.Regions {
			if strings.TrimSpace(region.BrokerURL) == "" {
				continue
			}
			brokers = append(brokers, setupConfiguredBroker{
				Provider:  "s3",
				Profile:   profile.Name,
				Region:    region.Name,
				BrokerURL: region.BrokerURL,
				Detail:    firstNonEmpty(profile.AccountID, profile.ARN),
			})
		}
	}
	sort.Slice(brokers, func(i, j int) bool {
		a := setupBrokerQualifiedName(brokers[i])
		b := setupBrokerQualifiedName(brokers[j])
		if a == b {
			return brokers[i].BrokerURL < brokers[j].BrokerURL
		}
		return a < b
	})
	return brokers
}

func configuredSetupBrokerExists(cfg globalConfig, provider, profile, region string) bool {
	provider = normalizeSetupProvider(provider)
	profile = strings.TrimSpace(profile)
	region = strings.TrimSpace(region)
	for _, broker := range configuredSetupBrokers(cfg) {
		if broker.Provider == provider && broker.Profile == profile && broker.Region == region {
			return true
		}
	}
	return false
}

func profilesWithoutConfiguredExpansion(profiles []setupProfile) []setupProfile {
	seen := map[string]struct{}{}
	var out []setupProfile
	for _, profile := range profiles {
		key := profile.Provider + "\x00" + profile.Name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		profile.Existing = false
		profile.ConfiguredRegions = nil
		out = append(out, profile)
	}
	return out
}

func setupBrokerQualifiedName(broker setupConfiguredBroker) string {
	name := setupProviderLabel(broker.Provider) + ":" + broker.Profile
	if strings.TrimSpace(broker.Region) != "" {
		name += "." + broker.Region
	}
	return name
}

func printSetupBrokerManagement(stdout io.Writer, broker setupConfiguredBroker) {
	fmt.Fprintf(stdout, "Manage %s\n", setupBrokerQualifiedName(broker))
	fmt.Fprintf(stdout, "Broker: %s\n\n", broker.BrokerURL)
	profileArgs := fmt.Sprintf("--profile %s --region %s", broker.Profile, broker.Region)
	fmt.Fprintln(stdout, "Common broker management commands:")
	fmt.Fprintf(stdout, "  bgit %s admin broker-users list\n", profileArgs)
	fmt.Fprintf(stdout, "  bgit %s admin invite-broker-user USER --role user\n", profileArgs)
	fmt.Fprintf(stdout, "  bgit %s admin teams list\n", profileArgs)
	fmt.Fprintf(stdout, "  bgit %s admin teams create TEAM\n", profileArgs)
	fmt.Fprintf(stdout, "  bgit admin invite-user --broker %s --team TEAM --user USER --role developer REPO\n", broker.BrokerURL)
	fmt.Fprintf(stdout, "  bgit admin confirm-ownership-transfer --broker %s REPO\n", broker.BrokerURL)
}

func setupBrokerConfig(base config, broker setupConfiguredBroker) config {
	cfg := base
	cfg.provider = broker.Provider
	cfg.gcloudConfiguration = broker.Profile
	cfg.gcloudConfigurationExplicit = broker.Profile != ""
	cfg.region = broker.Region
	cfg.brokerURL = broker.BrokerURL
	return cfg
}

func setupProvisionSelectedProfile(base config, path, now string, profile setupProfile, opts setupOptions, publicKeys []string, global *globalConfig, stdout io.Writer) error {
	_ = path
	if opts.action == "new" && configuredSetupBrokerExists(*global, profile.Provider, profile.Name, profile.Region) {
		return fmt.Errorf("%s:%s.%s already has a broker; choose Update broker to redeploy it", setupProviderLabel(profile.Provider), profile.Name, profile.Region)
	}
	cfg := base
	cfg.provider = profile.Provider
	cfg.gcloudConfiguration = profile.Name
	cfg.gcloudConfigurationExplicit = profile.Name != ""
	broker, err := provisionBroker(cfg, sshSetupOptions{region: firstNonEmpty(opts.region, profile.Region)}, stdout)
	if err != nil {
		return err
	}
	brokerURL := broker.URL
	if len(publicKeys) > 0 {
		if err := brokerUpsertOwners(brokerURL, broker.BootstrapToken, publicKeys); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "imported %d owner key(s) into broker %s\n", len(publicKeys), brokerURL)
	} else if err := brokerEnsureCoreTeam(brokerURL); err != nil {
		return err
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
				BrokerVersion: brokerVersion(),
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
				BrokerVersion: brokerVersion(),
				LastSetupAt:   now,
			}},
		})
	case "local":
		root := expandHome(firstNonEmpty(os.Getenv("BGIT_LOCAL_BROKER_ROOT"), "~/.bgit/local-broker"))
		*global = upsertGlobalLocalProfile(*global, globalLocalProfile{
			Name:      profile.Name,
			Root:      root,
			Autostart: true,
			Regions: []globalProfileRegion{{
				Name:          firstNonEmpty(profile.Region, "default"),
				BrokerURL:     brokerURL,
				BrokerVersion: brokerVersion(),
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
	case "local":
		return "local"
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
			last, ok, err := setupReadEscapeSequence(reader)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errSetupBack
			}
			switch last {
			case 'A':
				state.up()
			case 'B':
				state.down()
			case 'C':
				if regions, ok := state.activate(); ok {
					return regions, nil
				}
			case 'D':
				return nil, errSetupBack
			}
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
			last, ok, err := setupReadEscapeSequence(reader)
			if err != nil {
				return setupSelection{}, err
			}
			if !ok {
				if state.createProvider != "" {
					state.cancelCreateProfile()
					continue
				}
				return setupSelection{}, errors.New("setup canceled")
			}
			switch last {
			case 'A':
				state.up()
			case 'B':
				state.down()
			case 'C':
				if selected, ok := state.activate(); ok {
					return selected, nil
				} else if state.button == 1 {
					return setupSelection{}, errors.New("setup canceled")
				}
			case 'D':
				if state.createProvider != "" {
					state.cancelCreateProfile()
					continue
				}
				return setupSelection{}, errors.New("setup canceled")
			default:
				if state.createProvider != "" {
					state.cancelCreateProfile()
				}
			}
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

type setupBrokerHomeState struct {
	Brokers         []setupConfiguredBroker
	CreateProviders []string
	Cursor          int
	Scroll          int
	Button          int
	Message         string
}

type setupBrokerActionState struct {
	Broker  setupConfiguredBroker
	Cursor  int
	Button  int
	Message string
}

type setupBrokerManageState struct {
	Broker  setupConfiguredBroker
	Cursor  int
	Scroll  int
	Button  int
	Message string
}

type setupBrokerManageAction struct {
	ID    string
	Label string
	Help  string
}

type setupTextField struct {
	Label    string
	Value    string
	Secret   bool
	Required bool
}

type setupTextFormState struct {
	Title        string
	Fields       []setupTextField
	Cursor       int
	Button       int
	Editing      bool
	EditOriginal string
	Message      string
}

type setupChoice struct {
	Label string
	Value string
	Help  string
	Group string
}

type setupSelectState struct {
	Title   string
	Choices []setupChoice
	Cursor  int
	Scroll  int
	Button  int
	Message string
}

type setupMultiSelectState struct {
	Title    string
	Choices  []setupChoice
	Selected []bool
	Cursor   int
	Scroll   int
	Button   int
	Message  string
}

func runSetupBrokerHomeWithRaw(reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, brokers []setupConfiguredBroker, createProviders []string) (string, setupConfiguredBroker, error) {
	rawMode, restore, err := setupDialogRawMode(rawInput)
	if err != nil {
		return "", setupConfiguredBroker{}, err
	}
	defer restore()
	state := setupBrokerHomeState{Brokers: brokers, CreateProviders: createProviders, Button: -1}
	for {
		fmt.Fprint(stdout, renderSetupBrokerHomeFrame(state, rawMode))
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "cancel", setupConfiguredBroker{}, nil
			}
			return "", setupConfiguredBroker{}, err
		}
		switch b {
		case 0x03:
			return "", setupConfiguredBroker{}, errors.New("setup canceled")
		case 0x04:
			action, broker, ok := state.activate()
			if ok {
				return action, broker, nil
			}
		case '\r', '\n', ' ':
			action, broker, ok := state.activate()
			if ok {
				return action, broker, nil
			}
		case '\t':
			state.tab()
		case 'q', 'Q':
			return "cancel", setupConfiguredBroker{}, nil
		case 0x1b:
			last, ok, err := setupReadEscapeSequence(reader)
			if err != nil {
				return "", setupConfiguredBroker{}, err
			}
			if !ok {
				return "cancel", setupConfiguredBroker{}, nil
			}
			switch last {
			case 'A':
				state.up()
			case 'B':
				state.down()
			case 'C':
				action, broker, ok := state.activate()
				if ok {
					return action, broker, nil
				}
			case 'D':
				return "cancel", setupConfiguredBroker{}, nil
			}
		}
	}
}

func (s *setupBrokerHomeState) rows() int {
	rows := len(s.Brokers)
	if len(s.CreateProviders) > 0 {
		rows++
	}
	if rows == 0 {
		rows = 1
	}
	return rows
}

func (s *setupBrokerHomeState) visibleRange() (int, int) {
	const maxRows = 10
	rows := s.rows()
	if s.Cursor < s.Scroll {
		s.Scroll = s.Cursor
	}
	if s.Cursor >= s.Scroll+maxRows {
		s.Scroll = s.Cursor - maxRows + 1
	}
	if s.Scroll < 0 {
		s.Scroll = 0
	}
	if s.Scroll > rows-maxRows {
		s.Scroll = maxSetupDialogInt(0, rows-maxRows)
	}
	end := minSetupDialogInt(s.Scroll+maxRows, rows)
	return s.Scroll, end
}

func (s *setupBrokerHomeState) up() {
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

func (s *setupBrokerHomeState) down() {
	if s.rows() == 0 {
		return
	}
	s.Button = -1
	s.Message = ""
	s.Cursor = (s.Cursor + 1) % s.rows()
}

func (s *setupBrokerHomeState) tab() {
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

func (s *setupBrokerHomeState) activate() (string, setupConfiguredBroker, bool) {
	if s.Button == 0 {
		return "new", setupConfiguredBroker{}, true
	}
	if s.Button == 1 {
		return "cancel", setupConfiguredBroker{}, true
	}
	if len(s.Brokers) == 0 {
		if len(s.CreateProviders) == 0 {
			s.Message = "Install gcloud or AWS CLI to create a broker."
			return "", setupConfiguredBroker{}, false
		}
		return "new", setupConfiguredBroker{}, true
	}
	if s.Cursor < len(s.Brokers) {
		return "broker", s.Brokers[s.Cursor], true
	}
	return "new", setupConfiguredBroker{}, true
}

func renderSetupBrokerHomeFrame(state setupBrokerHomeState, rawMode bool) string {
	rendered := renderSetupBrokerHomeWithStyle(state, rawMode)
	if !rawMode {
		return rendered
	}
	rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	return "\x1b[?25l\x1b[H\x1b[2J" + rendered
}

func renderSetupBrokerHomeWithStyle(state setupBrokerHomeState, style bool) string {
	width := setupDialogDynamicWidth(58, setupBreadcrumb("Broker setups"), "Up/Down move  Right/Enter select  Tab buttons")
	for _, broker := range state.Brokers {
		width = setupDialogDynamicWidth(width, setupBrokerQualifiedName(broker)+" "+firstNonEmpty(broker.Detail, strings.TrimPrefix(strings.TrimPrefix(broker.BrokerURL, "https://"), "http://")))
	}
	var lines []string
	lines = append(lines,
		setupDialogBorder(width),
		setupDialogTitleRow(width),
		setupDialogBorder(width),
		setupDialogRowWidth(setupBreadcrumb("Broker setups"), width),
		setupDialogRowWidth("", width),
	)
	start, end := state.visibleRange()
	rows := state.rows()
	for row := start; row < end; row++ {
		marker := " "
		if state.Button < 0 && state.Cursor == row {
			marker = ">"
		}
		rowStyle := setupDialogSectionStyle(style, state.Button < 0 && state.Cursor == row)
		switch {
		case row < len(state.Brokers):
			broker := state.Brokers[row]
			detail := firstNonEmpty(broker.Detail, strings.TrimPrefix(strings.TrimPrefix(broker.BrokerURL, "https://"), "http://"))
			lines = append(lines, setupDialogRowStyledWidth(fmt.Sprintf("%s %-24s %s", marker, setupBrokerQualifiedName(broker), detail), width, rowStyle))
		case len(state.CreateProviders) > 0:
			lines = append(lines, setupDialogRowStyledWidth(fmt.Sprintf("%s new broker", marker), width, rowStyle))
		default:
			lines = append(lines, setupDialogRowStyledWidth("  no brokers configured", width, rowStyle))
		}
	}
	if rows > 10 {
		lines = append(lines, setupDialogRowWidth(setupBrokerScrollBar(start, end, rows), width))
	}
	if state.Message != "" {
		lines = append(lines, setupDialogRowStyledWidth(state.Message, width, setupDialogANSI(style, "33")))
	}
	okStyle := ""
	exitStyle := ""
	if style && state.Button == 0 {
		okStyle = "\x1b[44;97m"
	}
	if style && state.Button == 1 {
		exitStyle = "\x1b[44;97m"
	}
	lines = append(lines,
		setupDialogRowWidth("", width),
		setupDialogBorder(width),
		setupDialogRowWidth(setupDialogButton("[ New ]", okStyle)+"    "+setupDialogButton("[ Exit ]", exitStyle), width),
		setupDialogRowWidth("Up/Down move  Right/Enter select  Tab buttons", width),
		setupDialogRowWidth("Left/Esc cancel  Ctrl-C cancel", width),
		setupDialogBorder(width),
	)
	return strings.Join(lines, "\n") + "\n"
}

func setupBrokerScrollBar(start, end, total int) string {
	const width = 24
	if total <= 0 {
		return "scroll [" + strings.Repeat("-", width) + "]"
	}
	thumb := maxSetupDialogInt(1, width*(end-start)/total)
	pos := 0
	if total > end-start {
		pos = (width - thumb) * start / (total - (end - start))
	}
	var b strings.Builder
	b.WriteString("scroll [")
	for i := 0; i < width; i++ {
		if i >= pos && i < pos+thumb {
			b.WriteByte('#')
		} else {
			b.WriteByte('-')
		}
	}
	b.WriteString(fmt.Sprintf("] %d-%d of %d", start+1, end, total))
	return b.String()
}

func runSetupBrokerActionWithRaw(reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, broker setupConfiguredBroker) (string, error) {
	rawMode, restore, err := setupDialogRawMode(rawInput)
	if err != nil {
		return "", err
	}
	defer restore()
	state := setupBrokerActionState{Broker: broker, Button: -1}
	for {
		fmt.Fprint(stdout, renderSetupBrokerActionFrame(state, rawMode))
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "cancel", nil
			}
			return "", err
		}
		switch b {
		case 0x03:
			return "", errors.New("setup canceled")
		case 0x04:
			action, ok := state.activate()
			if ok {
				return action, nil
			}
		case '\r', '\n', ' ':
			action, ok := state.activate()
			if ok {
				return action, nil
			}
		case '\t':
			state.tab()
		case 'q', 'Q':
			return "cancel", nil
		case 0x1b:
			last, ok, err := setupReadEscapeSequence(reader)
			if err != nil {
				return "", err
			}
			if !ok {
				return "back", nil
			}
			switch last {
			case 'A':
				state.up()
			case 'B':
				state.down()
			case 'C':
				action, ok := state.activate()
				if ok {
					return action, nil
				}
			case 'D':
				return "back", nil
			}
		}
	}
}

func (s *setupBrokerActionState) rows() int { return 4 }

func (s *setupBrokerActionState) up() {
	s.Button = -1
	s.Message = ""
	if s.Cursor == 0 {
		s.Cursor = s.rows() - 1
		return
	}
	s.Cursor--
}

func (s *setupBrokerActionState) down() {
	s.Button = -1
	s.Message = ""
	s.Cursor = (s.Cursor + 1) % s.rows()
}

func (s *setupBrokerActionState) tab() {
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

func (s *setupBrokerActionState) activate() (string, bool) {
	if s.Button == 1 {
		return "back", true
	}
	if s.Button == 0 {
		return "manage", true
	}
	switch s.Cursor {
	case 0:
		return "manage", true
	case 1:
		return "update", true
	case 2:
		return "delete", true
	default:
		return "back", true
	}
}

func renderSetupBrokerActionFrame(state setupBrokerActionState, rawMode bool) string {
	rendered := renderSetupBrokerActionWithStyle(state, rawMode)
	if !rawMode {
		return rendered
	}
	rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	return "\x1b[?25l\x1b[H\x1b[2J" + rendered
}

func renderSetupBrokerActionWithStyle(state setupBrokerActionState, style bool) string {
	const width = 76
	actions := []struct {
		Label string
		Help  string
	}{
		{"manage broker", "users, teams, invites, and ownership commands"},
		{"update broker", "redeploy stack/function for this profile and region"},
		{"delete broker", "decommission broker infrastructure"},
		{"back", "return to shell without changes"},
	}
	var lines []string
	lines = append(lines,
		setupDialogBorder(width),
		setupDialogRowWidth("BUCKETGIT SETUP", width),
		setupDialogBorder(width),
		setupDialogRowWidth("Broker "+setupBrokerQualifiedName(state.Broker), width),
		setupDialogRowWidth(strings.TrimPrefix(strings.TrimPrefix(state.Broker.BrokerURL, "https://"), "http://"), width),
		setupDialogRowWidth("", width),
	)
	for i, action := range actions {
		marker := " "
		if state.Button < 0 && state.Cursor == i {
			marker = ">"
		}
		rowStyle := setupDialogSectionStyle(style, state.Button < 0 && state.Cursor == i)
		lines = append(lines, setupDialogWrappedActionRows(marker, action.Label, action.Help, 15, width, rowStyle)...)
	}
	okStyle := ""
	exitStyle := ""
	if style && state.Button == 0 {
		okStyle = "\x1b[44;97m"
	}
	if style && state.Button == 1 {
		exitStyle = "\x1b[44;97m"
	}
	lines = append(lines,
		setupDialogRowWidth("", width),
		setupDialogBorder(width),
		setupDialogRowWidth(setupDialogButton("[ Manage ]", okStyle)+"    "+setupDialogButton("[ Back ]", exitStyle), width),
		setupDialogRowWidth("Up/Down move  Right/Enter select  Tab buttons", width),
		setupDialogRowWidth("Left/Esc back  Ctrl-C cancel", width),
		setupDialogBorder(width),
	)
	return strings.Join(lines, "\n") + "\n"
}

func setupBrokerManageActions() []setupBrokerManageAction {
	return []setupBrokerManageAction{
		{ID: "users-manage", Label: "manage broker users", Help: "invite users and manage broker roles"},
		{ID: "teams-manage", Label: "team management", Help: "create teams, manage members, and repository access"},
		{ID: "back", Label: "back", Help: "return to shell"},
	}
}

func runSetupBrokerManageWithRaw(base config, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, broker setupConfiguredBroker) error {
	rawMode, restore, err := setupDialogRawMode(rawInput)
	if err != nil {
		return err
	}
	defer restore()
	state := setupBrokerManageState{Broker: broker, Button: -1}
	cfg := setupBrokerConfig(base, broker)
	for {
		fmt.Fprint(stdout, renderSetupBrokerManageFrame(state, rawMode))
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch b {
		case 0x03:
			return errors.New("setup canceled")
		case 0x04:
			action, ok := state.activate()
			if ok {
				if action == "back" {
					return errSetupBack
				}
				msg, err := runSetupBrokerManageAction(cfg, broker, action, reader, rawInput, stdout)
				if err != nil {
					state.Message = err.Error()
				} else {
					state.Message = msg
				}
			}
		case '\r', '\n', ' ':
			action, ok := state.activate()
			if ok {
				if action == "back" {
					return errSetupBack
				}
				msg, err := runSetupBrokerManageAction(cfg, broker, action, reader, rawInput, stdout)
				if err != nil {
					state.Message = err.Error()
				} else {
					state.Message = msg
				}
			}
		case '\t':
			state.tab()
		case 'q', 'Q':
			return nil
		case 0x1b:
			last, ok, err := setupReadEscapeSequence(reader)
			if err != nil {
				return err
			}
			if !ok {
				return errSetupBack
			}
			switch last {
			case 'A':
				state.up()
			case 'B':
				state.down()
			case 'C':
				action, ok := state.activate()
				if ok {
					if action == "back" {
						return errSetupBack
					}
					msg, err := runSetupBrokerManageAction(cfg, broker, action, reader, rawInput, stdout)
					if err != nil {
						state.Message = err.Error()
					} else {
						state.Message = msg
					}
				}
			case 'D':
				return errSetupBack
			}
		}
	}
}

func (s *setupBrokerManageState) rows() int { return len(setupBrokerManageActions()) }

func (s *setupBrokerManageState) visibleRange() (int, int) {
	const maxRows = 10
	rows := s.rows()
	if s.Cursor < s.Scroll {
		s.Scroll = s.Cursor
	}
	if s.Cursor >= s.Scroll+maxRows {
		s.Scroll = s.Cursor - maxRows + 1
	}
	if s.Scroll < 0 {
		s.Scroll = 0
	}
	if s.Scroll > rows-maxRows {
		s.Scroll = maxSetupDialogInt(0, rows-maxRows)
	}
	return s.Scroll, minSetupDialogInt(s.Scroll+maxRows, rows)
}

func (s *setupBrokerManageState) up() {
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

func (s *setupBrokerManageState) down() {
	if s.rows() == 0 {
		return
	}
	s.Button = -1
	s.Message = ""
	s.Cursor = (s.Cursor + 1) % s.rows()
}

func (s *setupBrokerManageState) tab() {
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

func (s *setupBrokerManageState) activate() (string, bool) {
	if s.Button == 1 {
		return "back", true
	}
	actions := setupBrokerManageActions()
	if s.Button == 0 {
		if s.Cursor >= 0 && s.Cursor < len(actions) {
			return actions[s.Cursor].ID, true
		}
		return "back", true
	}
	if s.Cursor >= 0 && s.Cursor < len(actions) {
		return actions[s.Cursor].ID, true
	}
	return "", false
}

func renderSetupBrokerManageFrame(state setupBrokerManageState, rawMode bool) string {
	rendered := renderSetupBrokerManageWithStyle(state, rawMode)
	if !rawMode {
		return rendered
	}
	rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	return "\x1b[?25l\x1b[H\x1b[2J" + rendered
}

func renderSetupBrokerManageWithStyle(state setupBrokerManageState, style bool) string {
	const width = 76
	actions := setupBrokerManageActions()
	start, end := state.visibleRange()
	var lines []string
	lines = append(lines,
		setupDialogBorder(width),
		setupDialogRowWidth("BUCKETGIT SETUP", width),
		setupDialogBorder(width),
		setupDialogRowWidth("Manage "+setupBrokerQualifiedName(state.Broker), width),
		setupDialogRowWidth(strings.TrimPrefix(strings.TrimPrefix(state.Broker.BrokerURL, "https://"), "http://"), width),
		setupDialogRowWidth("", width),
	)
	for i := start; i < end; i++ {
		action := actions[i]
		marker := " "
		if state.Button < 0 && state.Cursor == i {
			marker = ">"
		}
		rowStyle := setupDialogSectionStyle(style, state.Button < 0 && state.Cursor == i)
		lines = append(lines, setupDialogWrappedActionRows(marker, action.Label, action.Help, 20, width, rowStyle)...)
	}
	if len(actions) > 10 {
		lines = append(lines, setupDialogRowWidth(setupBrokerScrollBar(start, end, len(actions)), width))
	}
	if state.Message != "" {
		lines = append(lines, setupDialogRowStyledWidth(state.Message, width, setupDialogANSI(style, "33")))
	}
	okStyle := ""
	backStyle := ""
	if style && state.Button == 0 {
		okStyle = "\x1b[44;97m"
	}
	if style && state.Button == 1 {
		backStyle = "\x1b[44;97m"
	}
	lines = append(lines,
		setupDialogRowWidth("", width),
		setupDialogBorder(width),
		setupDialogRowWidth(setupDialogButton("[ OK ]", okStyle)+"    "+setupDialogButton("[ Back ]", backStyle), width),
		setupDialogRowWidth("Up/Down move  Right/Enter select  Tab buttons", width),
		setupDialogRowWidth("Left/Esc back  Ctrl-C cancel", width),
		setupDialogBorder(width),
	)
	return strings.Join(lines, "\n") + "\n"
}

func runSetupBrokerManageAction(cfg config, broker setupConfiguredBroker, action string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	switch action {
	case "users-manage":
		msg, err := runSetupBrokerUsersWithRaw(cfg, broker, reader, rawInput, stdout)
		if errors.Is(err, errSetupBack) {
			return "No changes made.", nil
		}
		return msg, err
	case "teams-manage":
		return runSetupTeamManagementWithRaw(cfg, reader, rawInput, stdout)
	case "back":
		return "No changes made.", nil
	default:
		return "", fmt.Errorf("unknown broker management action %q", action)
	}
}

func runSetupBrokerUsersWithRaw(cfg config, broker setupConfiguredBroker, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	title := setupBreadcrumb("Manage broker users")
	for {
		choices, err := setupBrokerUserManagementChoices(cfg)
		if err != nil {
			return "", err
		}
		action, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, title, choices, "")
		if err != nil {
			return "", err
		}
		if !ok || action == "back" {
			return "", errSetupBack
		}
		if action == "invite-user" {
			msg, err := runSetupBrokerUserInviteWithRaw(cfg, broker, reader, rawInput, stdout)
			if err != nil {
				return "", err
			}
			if msg == "No changes made." {
				continue
			}
			return msg, nil
		}
		if action == "noop" {
			continue
		}
		if user, ok := strings.CutPrefix(action, "user:"); ok {
			msg, err := runSetupBrokerUserWithRaw(cfg, broker, user, reader, rawInput, stdout)
			if errors.Is(err, errSetupBack) {
				continue
			}
			return msg, err
		}
	}
}

func setupBrokerUserManagementChoices(cfg config) ([]setupChoice, error) {
	users, err := setupBrokerUsers(cfg)
	if err != nil {
		return nil, err
	}
	choices := []setupChoice{{Label: "invite user", Value: "invite-user", Help: "create a broker invite command"}}
	if len(users) == 0 {
		choices = append(choices, setupChoice{Label: "no broker users", Value: "noop", Group: "users:"})
	} else {
		sort.Slice(users, func(i, j int) bool {
			return strings.ToLower(firstNonEmpty(users[i].Username, users[i].ID)) < strings.ToLower(firstNonEmpty(users[j].Username, users[j].ID))
		})
		for _, user := range users {
			username := firstNonEmpty(user.Username, user.ID)
			if username == "" {
				continue
			}
			label := username
			if user.Pending || len(user.Keys) == 0 {
				label += " *"
			}
			choices = append(choices, setupChoice{Label: label, Value: "user:" + username, Help: setupBrokerUserStatus(user), Group: "users:"})
		}
	}
	choices = append(choices, setupChoice{Label: "back", Value: "back", Help: "return to broker management"})
	return choices, nil
}

func runSetupBrokerUserInviteWithRaw(cfg config, broker setupConfiguredBroker, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	var out bytes.Buffer
	fields, ok, err := runSetupTextFormWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage broker users", "Invite user"), []setupTextField{
		{Label: "Username", Required: true},
	})
	if err != nil || !ok {
		return "No changes made.", err
	}
	role, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage broker users", fields[0], "Role"), setupBrokerUserRoleChoices(), "user")
	if err != nil || !ok {
		return "No changes made.", err
	}
	if err := brokerAdminCommandWithInput(cfg, []string{"invite-broker-user", "--broker", broker.BrokerURL, "--user", fields[0], "--role", role}, strings.NewReader(""), &out); err != nil {
		return "", err
	}
	return runSetupPlainCommandOutputWithRaw(reader, stdout, "Create user invite", setupAcceptCommandFromOutput(out.String()))
}

func runSetupBrokerUserWithRaw(cfg config, broker setupConfiguredBroker, username string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	for {
		user, ok, err := setupBrokerUserByName(cfg, username)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("broker user %s not found", username)
		}
		choices := setupBrokerUserActionChoices(user)
		action, selected, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage broker users", username), choices, "")
		if err != nil {
			return "", err
		}
		if !selected || action == "back" {
			return "", errSetupBack
		}
		msg, err := runSetupBrokerUserAction(cfg, broker, user, action, reader, rawInput, stdout)
		if err != nil {
			return "", err
		}
		if action == "delete" {
			if _, err := runSetupBrokerOutputWithRaw(reader, rawInput, stdout, "Delete broker user", msg); err != nil {
				return "", err
			}
			return "No changes made.", errSetupBack
		}
		if msg == "No changes made." {
			continue
		}
		return msg, nil
	}
}

func setupBrokerUserActionChoices(user brokerUserInfo) []setupChoice {
	if user.BrokerRole == "owner" {
		return []setupChoice{
			{Label: "transfer ownership", Value: "transfer-owner", Help: "create an ownership transfer command"},
			{Label: "back", Value: "back", Help: "return to broker users"},
		}
	}
	choices := []setupChoice{{Label: "edit role", Value: "edit-role", Help: "change broker role"}}
	if user.Suspended {
		choices = append(choices, setupChoice{Label: "unsuspend user", Value: "unsuspend", Help: "restore broker access"})
	} else {
		choices = append(choices, setupChoice{Label: "suspend user", Value: "suspend", Help: "deny broker access"})
	}
	if user.Pending || len(user.Keys) == 0 {
		choices = append(choices, setupChoice{Label: "cancel invite", Value: "cancel-invite", Help: "cancel pending broker invite"})
	}
	choices = append(choices, setupChoice{Label: "delete user", Value: "delete", Help: "remove broker user and repository/team access"})
	choices = append(choices, setupChoice{Label: "back", Value: "back", Help: "return to broker users"})
	return choices
}

func runSetupBrokerUserAction(cfg config, broker setupConfiguredBroker, user brokerUserInfo, action string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	var out bytes.Buffer
	username := firstNonEmpty(user.Username, user.ID)
	role := firstNonEmpty(user.BrokerRole, "user")
	switch action {
	case "transfer-owner":
		repo, ok, err := runSetupRepoSelect(cfg, reader, rawInput, stdout, setupBreadcrumb("Manage broker users", username, "Transfer ownership"))
		if err != nil || !ok {
			return "No changes made.", err
		}
		if err := brokerAdminCommandWithInput(cfg, []string{"confirm-ownership-transfer", "--broker", broker.BrokerURL, repo}, strings.NewReader(""), &out); err != nil {
			return "", err
		}
		return runSetupPlainCommandOutputWithRaw(reader, stdout, "Owner transfer", setupAcceptCommandFromOutput(out.String()))
	case "edit-role":
		nextRole, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage broker users", username, "Role"), setupBrokerUserRoleChoices(), role)
		if err != nil || !ok {
			return "No changes made.", err
		}
		if err := brokerAdminCommandWithInput(cfg, []string{"broker-users", "upsert", username, "--role", nextRole}, strings.NewReader(""), &out); err != nil {
			return "", err
		}
		return strings.TrimSpace(out.String()), nil
	case "suspend":
		if err := brokerAdminCommandWithInput(cfg, []string{"broker-users", "upsert", username, "--role", role, "--suspended", "true"}, strings.NewReader(""), &out); err != nil {
			return "", err
		}
		return "suspended broker user " + username, nil
	case "unsuspend":
		if err := brokerAdminCommandWithInput(cfg, []string{"broker-users", "upsert", username, "--role", role, "--suspended", "false"}, strings.NewReader(""), &out); err != nil {
			return "", err
		}
		return "unsuspended broker user " + username, nil
	case "cancel-invite":
		if err := brokerAdminCommandWithInput(cfg, []string{"cancel-broker-invite", "--broker", broker.BrokerURL, "--user", username}, strings.NewReader(""), &out); err != nil {
			return "", err
		}
		return strings.TrimSpace(out.String()), nil
	case "delete":
		if err := brokerAdminCommandWithInput(cfg, []string{"broker-users", "delete", username}, strings.NewReader(""), &out); err != nil {
			return "", err
		}
		return strings.TrimSpace(out.String()), nil
	default:
		return "", fmt.Errorf("unknown broker user action %q", action)
	}
}

func setupBrokerRepoConfig(cfg config, repo string) (config, error) {
	logical, err := normalizeLogicalRepoName(repo)
	if err != nil {
		return cfg, err
	}
	cfg.logicalRepo = logical
	cfg.prefix = logical
	return cfg, nil
}

func runSetupTeamManagementWithRaw(cfg config, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	for {
		choices, err := setupTeamManagementChoices(cfg)
		if err != nil {
			return "", err
		}
		action, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Team management"), choices, "")
		if err != nil || !ok || action == "back" {
			return "No changes made.", err
		}
		switch action {
		case "create":
			msg, err := runSetupTeamCreateWithRaw(cfg, reader, rawInput, stdout)
			if err != nil {
				return "", err
			}
			if msg == "No changes made." {
				continue
			}
			return msg, nil
		default:
			team, ok := strings.CutPrefix(action, "team:")
			if !ok {
				return "", fmt.Errorf("unknown team management action %q", action)
			}
			teamInfo, _ := setupBrokerTeamInfo(cfg, team)
			msg, err := runSetupManagedTeamWithRaw(cfg, team, setupTeamDisplayName(team, teamInfo), reader, rawInput, stdout)
			if errors.Is(err, errSetupBack) {
				continue
			}
			return msg, err
		}
	}
}

func setupTeamManagementChoices(cfg config) ([]setupChoice, error) {
	teams, err := setupBrokerTeamChoices(cfg)
	if err != nil {
		return nil, err
	}
	choices := []setupChoice{
		{Label: "create team", Value: "create", Help: "create a team namespace"},
	}
	for _, team := range teams {
		team.Value = "team:" + team.Value
		choices = append(choices, team)
	}
	choices = append(choices, setupChoice{Label: "back", Value: "back", Help: "return to broker management"})
	return choices, nil
}

func runSetupTeamCreateWithRaw(cfg config, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	var out bytes.Buffer
	fields, ok, err := runSetupTextFormWithRaw(reader, rawInput, stdout, "Create team", []setupTextField{{Label: "Team name", Required: true}})
	if err != nil || !ok {
		return "No changes made.", err
	}
	if err := brokerAdminCommandWithInput(cfg, []string{"teams", "create", fields[0]}, strings.NewReader(""), &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func runSetupManagedTeamWithRaw(cfg config, teamID, teamName string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	teamLabel := firstNonEmpty(teamName, teamID)
	title := setupBreadcrumb("Manage team", teamLabel)
	for {
		action, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, title, []setupChoice{
			{Label: "manage users", Value: "members-manage", Help: "add, edit, or remove team users"},
			{Label: "manage repositories", Value: "repos-manage", Help: "create and manage team repositories"},
			{Label: "back", Value: "back", Help: "return to team management"},
		}, "")
		if err != nil {
			return "", err
		}
		if !ok || action == "back" {
			return "", errSetupBack
		}
		msg, err := runSetupManagedTeamAction(cfg, teamID, teamLabel, action, reader, rawInput, stdout)
		if err != nil {
			return "", err
		}
		if msg == "No changes made." {
			continue
		}
		return msg, nil
	}
}

func runSetupManagedTeamAction(cfg config, teamID, teamName, action string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	switch action {
	case "members-manage":
		msg, err := runSetupManagedTeamUsersWithRaw(cfg, teamID, teamName, reader, rawInput, stdout)
		if errors.Is(err, errSetupBack) {
			return "No changes made.", nil
		}
		return msg, err
	case "repos-manage":
		msg, err := runSetupManagedTeamRepositoriesWithRaw(cfg, teamID, teamName, reader, rawInput, stdout)
		if errors.Is(err, errSetupBack) {
			return "No changes made.", nil
		}
		return msg, err
	case "repos-list":
		if _, err := runSetupBrokerOutputWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories"), setupFormatTeamRepositories(cfg, teamID)); err != nil {
			return "", err
		}
		return "No changes made.", nil
	default:
		return "", fmt.Errorf("unknown team management action %q", action)
	}
}

func runSetupManagedTeamUsersWithRaw(cfg config, teamID, teamName string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	title := setupBreadcrumb("Manage team", teamName, "Manage users")
	for {
		choices, err := setupTeamUserManagementChoices(cfg, teamID)
		if err != nil {
			return "", err
		}
		action, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, title, choices, "")
		if err != nil {
			return "", err
		}
		if !ok || action == "back" {
			return "", errSetupBack
		}
		if action == "add-user" {
			msg, err := runSetupManagedTeamUserAdd(cfg, teamID, teamName, reader, rawInput, stdout)
			if err != nil {
				return "", err
			}
			if msg == "No changes made." {
				continue
			}
			return msg, nil
		}
		if action == "noop" {
			continue
		}
		if user, ok := strings.CutPrefix(action, "user:"); ok {
			msg, err := runSetupManagedTeamUserWithRaw(cfg, teamID, teamName, user, reader, rawInput, stdout)
			if errors.Is(err, errSetupBack) {
				continue
			}
			return msg, err
		}
	}
}

func setupTeamUserManagementChoices(cfg config, teamID string) ([]setupChoice, error) {
	team, err := setupBrokerTeamInfo(cfg, teamID)
	if err != nil {
		return nil, err
	}
	choices := []setupChoice{{Label: "add user", Value: "add-user", Help: "add a user with a team role cap"}}
	if len(team.Members) == 0 {
		choices = append(choices, setupChoice{Label: "no team users", Value: "noop", Group: "users:"})
	} else {
		for _, member := range team.Members {
			user := firstNonEmpty(member.Username, member.UserID)
			if user == "" {
				continue
			}
			choices = append(choices, setupChoice{Label: user, Value: "user:" + user, Help: "role cap " + firstNonEmpty(member.Role, "read"), Group: "users:"})
		}
	}
	choices = append(choices, setupChoice{Label: "back", Value: "back", Help: "return to team"})
	return choices, nil
}

func runSetupManagedTeamUserAdd(cfg config, teamID, teamName string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	var out bytes.Buffer
	user, ok, err := runSetupAvailableTeamUserSelect(cfg, teamID, reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Manage users", "Add user"))
	if err != nil || !ok {
		return "No changes made.", err
	}
	role, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Manage users", user, "Team role cap"), setupRepoRoleCapChoices(), "developer")
	if err != nil || !ok {
		return "No changes made.", err
	}
	if err := brokerAdminCommandWithInput(cfg, []string{"teams", "member", "add", teamID, user, "--role", role}, strings.NewReader(""), &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func runSetupManagedTeamUserWithRaw(cfg config, teamID, teamName, user string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	title := setupBreadcrumb("Manage team", teamName, "Manage users", user)
	for {
		action, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, title, []setupChoice{
			{Label: "edit role cap", Value: "edit", Help: "change this user's team role cap"},
			{Label: "remove user", Value: "remove", Help: "remove this user from the team"},
			{Label: "back", Value: "back", Help: "return to users"},
		}, "")
		if err != nil {
			return "", err
		}
		if !ok || action == "back" {
			return "", errSetupBack
		}
		msg, err := runSetupManagedTeamUserAction(cfg, teamID, teamName, user, action, reader, rawInput, stdout)
		if err != nil {
			return "", err
		}
		if msg == "No changes made." {
			continue
		}
		return msg, nil
	}
}

func runSetupManagedTeamUserAction(cfg config, teamID, teamName, user, action string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	var out bytes.Buffer
	switch action {
	case "edit":
		role, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Manage users", user, "Team role cap"), setupRepoRoleCapChoices(), "developer")
		if err != nil || !ok {
			return "No changes made.", err
		}
		if err := brokerAdminCommandWithInput(cfg, []string{"teams", "member", "add", teamID, user, "--role", role}, strings.NewReader(""), &out); err != nil {
			return "", err
		}
		return strings.TrimSpace(out.String()), nil
	case "remove":
		if err := brokerAdminCommandWithInput(cfg, []string{"teams", "member", "remove", teamID, user}, strings.NewReader(""), &out); err != nil {
			return "", err
		}
		return strings.TrimSpace(out.String()), nil
	default:
		return "", fmt.Errorf("unknown team user action %q", action)
	}
}

func runSetupManagedTeamRepositoriesWithRaw(cfg config, teamID, teamName string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	title := setupBreadcrumb("Manage team", teamName, "Repositories")
	for {
		choices, err := setupBrokerTeamRepositoryMenuChoices(cfg, teamID)
		if err != nil {
			return "", err
		}
		action, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, title, choices, "")
		if err != nil {
			return "", err
		}
		if !ok || action == "back" {
			return "", errSetupBack
		}
		if action == "create" {
			msg, err := runSetupTeamRepositoryCreateWithRaw(cfg, teamID, teamName, reader, rawInput, stdout)
			if err != nil {
				return "", err
			}
			if msg == "No changes made." {
				continue
			}
			return msg, nil
		}
		if repo, ok := strings.CutPrefix(action, "repo:"); ok {
			msg, err := runSetupManagedTeamRepositoryWithRaw(cfg, teamID, teamName, repo, reader, rawInput, stdout)
			if errors.Is(err, errSetupBack) {
				continue
			}
			if err != nil {
				return "", err
			}
			if msg == "No changes made." {
				continue
			}
			return msg, nil
		}
	}
}

func runSetupTeamRepositoryCreateWithRaw(cfg config, teamID, teamName string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	fields, ok, err := runSetupTextFormWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", "Create repository"), []setupTextField{
		{Label: "Repository", Required: true},
	})
	if err != nil || !ok {
		return "No changes made.", err
	}
	repoInput, ok, err := runSetupLocalBrokerRepoStorageRegionWithRaw(cfg, fields[0], reader, rawInput, stdout)
	if err != nil || !ok {
		return "No changes made.", err
	}
	repo, err := setupRepoForTeamCreate(cfg, repoInput, teamID, teamName)
	if err != nil {
		return "", err
	}
	logical := repo.Logical
	role, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", logicalRepoDisplayName(logical), "Role cap"), setupRepoRoleCapChoices(), "developer")
	if err != nil || !ok {
		return "No changes made.", err
	}
	brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
	if err != nil {
		return "", err
	}
	repo.TeamID = teamID
	repo.TeamName = teamName
	req := brokerRepoAdminRequest{Repo: repo, Role: role}
	if err := brokerPost(brokerURL, "/repos/create", req, nil); err != nil {
		return "", err
	}
	return fmt.Sprintf("created repository %s and granted %s %s access", logicalRepoDisplayName(logical), firstNonEmpty(teamName, teamID), role), nil
}

func runSetupLocalBrokerRepoStorageRegionWithRaw(cfg config, repoName string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, bool, error) {
	_, _, _, _ = reader, rawInput, stdout, cfg
	if cfg.provider != "local" {
		return repoName, true, nil
	}
	return repoName, true, nil
}

func setupRepoForTeamCreate(cfg config, value, teamID, teamName string) (brokerRepo, error) {
	if repo, ok, err := brokerRepoForStorageTarget(cfg, value, teamID); ok || err != nil {
		repo.TeamName = teamName
		return repo, err
	}
	logical, err := normalizeLogicalRepoName(value)
	if err != nil {
		return brokerRepo{}, err
	}
	actionCfg, err := setupBrokerRepoConfig(cfg, logical)
	if err != nil {
		return brokerRepo{}, err
	}
	actionCfg.provider = firstNonEmpty(actionCfg.provider, cfg.provider, "gcs")
	actionCfg.brokerURL = cfg.brokerURL
	actionCfg.teamID = teamID
	repo := repoForBroker(actionCfg)
	repo.TeamName = teamName
	return repo, nil
}

func setupBrokerTeamRepositoryMenuChoices(cfg config, teamID string) ([]setupChoice, error) {
	repos, err := setupBrokerTeamRepoChoices(cfg, teamID)
	if err != nil {
		return nil, err
	}
	choices := []setupChoice{
		{Label: "create repository", Value: "create", Help: "create a repository for this team"},
	}
	for _, repo := range repos {
		repo.Value = "repo:" + repo.Value
		choices = append(choices, repo)
	}
	choices = append(choices, setupChoice{Label: "back", Value: "back", Help: "return to team"})
	return choices, nil
}

func runSetupManagedTeamRepositoryWithRaw(cfg config, teamID, teamName, repo string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	repoName := logicalRepoDisplayName(repo)
	for {
		action, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", repoName), []setupChoice{
			{Label: "manage access", Value: "access-manage", Help: "users, invites, and team sharing"},
			{Label: "edit role cap", Value: "repos-edit", Help: "change this team's repository role cap"},
			{Label: "remove access", Value: "repos-remove", Help: "detach this repository from this team"},
			{Label: "back", Value: "back", Help: "return to repositories"},
		}, "")
		if err != nil {
			return "", err
		}
		if !ok || action == "back" {
			return "", errSetupBack
		}
		msg, err := runSetupManagedTeamRepositoryAction(cfg, teamID, teamName, repo, action, reader, rawInput, stdout)
		if err != nil {
			return "", err
		}
		if msg == "No changes made." {
			continue
		}
		return msg, nil
	}
}

func runSetupManagedTeamRepositoryAction(cfg config, teamID, teamName, repo, action string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	var out bytes.Buffer
	repoName := logicalRepoDisplayName(repo)
	switch action {
	case "assign-users":
		return runSetupRepoUserAssignmentWithRaw(cfg, teamID, teamName, repo, reader, rawInput, stdout)
	case "access-manage":
		msg, err := runSetupManagedTeamRepositoryAccessWithRaw(cfg, teamID, teamName, repo, reader, rawInput, stdout)
		if errors.Is(err, errSetupBack) {
			return "No changes made.", nil
		}
		return msg, err
	case "invite-user":
		user, ok, err := runSetupAvailableRepoInviteUserSelect(cfg, repo, teamID, reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", repoName, "Invite user"))
		if err != nil || !ok {
			return "No changes made.", err
		}
		role, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", repoName, "Role"), setupRepoRoleChoices(), "developer")
		if err != nil || !ok {
			return "No changes made.", err
		}
		brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
		if err != nil {
			return "", err
		}
		brokerRepo, err := setupBrokerTeamRepoForAction(cfg, repo, teamID)
		if err != nil {
			return "", err
		}
		var resp brokerOwnerTransferResponse
		if err := brokerPost(brokerURL, "/keys/invite/create", brokerOwnerTransferRequest{Repo: brokerRepo, BrokerURL: brokerURL, User: user, Role: role}, &resp); err != nil {
			return "", err
		}
		return runSetupPlainCommandOutputWithRaw(reader, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", repoName, "Invite user"), resp.AcceptCommand)
	case "cancel-invite":
		brokerRepo, err := setupBrokerTeamRepoForAction(cfg, repo, teamID)
		if err != nil {
			return "", err
		}
		user, ok, err := runSetupPendingRepoInviteSelect(cfg, brokerRepo, reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", repoName, "Pending invite"))
		if err != nil || !ok {
			return "No changes made.", err
		}
		brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
		if err != nil {
			return "", err
		}
		if err := brokerPost(brokerURL, "/keys/invite/cancel", brokerOwnerTransferRequest{Repo: brokerRepo, User: user}, nil); err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "cancelled invite for %s on %s\n", user, brokerRepo.Logical)
		return strings.TrimSpace(out.String()), nil
	case "grant-team":
		targetTeamID, ok, err := runSetupAvailableRepoTeamSelect(cfg, repo, teamID, reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", repoName, "Grant team access"))
		if err != nil || !ok {
			return "No changes made.", err
		}
		role, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", repoName, "Team role cap"), setupRepoRoleCapChoices(), "developer")
		if err != nil || !ok {
			return "No changes made.", err
		}
		actionCfg, err := setupBrokerRepoConfig(cfg, repo)
		if err != nil {
			return "", err
		}
		actionCfg.provider = firstNonEmpty(actionCfg.provider, cfg.provider, "gcs")
		actionCfg.teamID = strings.TrimSpace(teamID)
		if err := brokerAdminCommandWithInput(actionCfg, []string{"teams", "repo", "add", targetTeamID, role}, strings.NewReader(""), &out); err != nil {
			return "", err
		}
		return strings.TrimSpace(out.String()), nil
	case "repos-edit":
		return runSetupTeamRepoAccessUpsert(cfg, teamID, teamName, repo, reader, rawInput, stdout)
	case "repos-remove":
		actionCfg, err := setupBrokerRepoConfig(cfg, repo)
		if err != nil {
			return "", err
		}
		if err := brokerAdminCommandWithInput(actionCfg, []string{"teams", "repo", "remove", teamID}, strings.NewReader(""), &out); err != nil {
			return "", err
		}
		return strings.TrimSpace(out.String()), nil
	default:
		return "", fmt.Errorf("unknown team repository action %q", action)
	}
}

func runSetupManagedTeamRepositoryAccessWithRaw(cfg config, teamID, teamName, repo string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	repoName := logicalRepoDisplayName(repo)
	title := setupBreadcrumb("Manage team", teamName, "Repositories", repoName, "Manage access")
	for {
		choices, err := setupRepoAccessManagementChoices(cfg, repo, teamID)
		if err != nil {
			return "", err
		}
		action, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, title, choices, "")
		if err != nil {
			return "", err
		}
		if !ok || action == "back" {
			return "", errSetupBack
		}
		if action == "assign-users" || action == "invite-user" || action == "cancel-invite" || action == "grant-team" {
			msg, err := runSetupManagedTeamRepositoryAction(cfg, teamID, teamName, repo, action, reader, rawInput, stdout)
			if err != nil {
				return "", err
			}
			if msg == "No changes made." {
				continue
			}
			return msg, nil
		}
		if action == "noop" {
			continue
		}
		if user, ok := strings.CutPrefix(action, "user:"); ok {
			msg, err := runSetupManagedTeamRepositoryUserWithRaw(cfg, teamID, teamName, repo, user, reader, rawInput, stdout)
			if errors.Is(err, errSetupBack) {
				continue
			}
			return msg, err
		}
	}
}

func setupRepoAccessManagementChoices(cfg config, repo, teamID string) ([]setupChoice, error) {
	choices := []setupChoice{}
	if _, err := setupBrokerUsers(cfg); err == nil {
		choices = append(choices, setupChoice{Label: "assign users", Value: "assign-users", Help: "grant broker users repository access"})
	} else {
		choices = append(choices,
			setupChoice{Label: "invite user", Value: "invite-user", Help: "create an invite for this repository"},
			setupChoice{Label: "cancel invite", Value: "cancel-invite", Help: "cancel a pending invite by username"},
		)
	}
	choices = append(choices, setupChoice{Label: "grant team access", Value: "grant-team", Help: "share this repository with another team"})
	users, err := setupRepoUsers(cfg, repo, teamID)
	if err != nil {
		return nil, err
	}
	if len(users) > 0 {
		for _, user := range users {
			label := user.User
			help := "role " + user.Role
			details := []string{}
			if user.Grant {
				details = append(details, "broker user")
			}
			if user.Pending {
				details = append(details, "pending")
			}
			if user.KeyCount > 1 {
				details = append(details, fmt.Sprintf("%d keys", user.KeyCount))
			} else if user.KeyCount == 1 {
				details = append(details, "1 key")
			}
			if len(details) > 0 {
				help += ", " + strings.Join(details, ", ")
			}
			choices = append(choices, setupChoice{Label: label, Value: "user:" + user.User, Help: help, Group: "users:"})
		}
	} else {
		choices = append(choices, setupChoice{Label: "no repository users", Value: "noop", Help: "", Group: "users:"})
	}
	choices = append(choices, setupChoice{Label: "back", Value: "back", Help: "return to repository"})
	return choices, nil
}

func runSetupRepoUserAssignmentWithRaw(cfg config, teamID, teamName, repo string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	repoName := logicalRepoDisplayName(repo)
	choices, err := setupAssignableRepoUserChoices(cfg, repo, teamID)
	if err != nil {
		return "", err
	}
	if len(choices) == 0 {
		if _, err := runSetupBrokerOutputWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", repoName, "Assign users"), "No broker users are available to assign."); err != nil {
			return "", err
		}
		return "No changes made.", nil
	}
	selected, ok, err := runSetupMultiSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", repoName, "Assign users"), choices)
	if err != nil || !ok {
		return "No changes made.", err
	}
	role, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", repoName, "Role"), setupRepoRoleChoices(), "developer")
	if err != nil || !ok {
		return "No changes made.", err
	}
	brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
	if err != nil {
		return "", err
	}
	brokerRepo, err := setupBrokerTeamRepoForAction(cfg, repo, teamID)
	if err != nil {
		return "", err
	}
	for _, userID := range selected {
		if err := brokerPost(brokerURL, "/repo/users/upsert", brokerRepoAdminRequest{Repo: brokerRepo, UserID: userID, Role: role}, nil); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("assigned %d user(s) to %s as %s", len(selected), repoName, role), nil
}

func setupAssignableRepoUserChoices(cfg config, repo, teamID string) ([]setupChoice, error) {
	users, err := setupBrokerUsers(cfg)
	if err != nil {
		return nil, err
	}
	assigned, err := setupRepoUsers(cfg, repo, teamID)
	if err != nil {
		return nil, err
	}
	assignedNames := map[string]struct{}{}
	for _, user := range assigned {
		assignedNames[strings.ToLower(user.User)] = struct{}{}
	}
	var choices []setupChoice
	for _, user := range users {
		username := firstNonEmpty(user.Username, user.ID)
		if username == "" || user.ID == "" {
			continue
		}
		if _, ok := assignedNames[strings.ToLower(username)]; ok {
			continue
		}
		help := setupBrokerUserStatus(user)
		if user.Pending || len(user.Keys) == 0 {
			help += " · access activates after key setup"
		}
		choices = append(choices, setupChoice{Label: username, Value: user.ID, Help: help})
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].Label < choices[j].Label })
	return choices, nil
}

func runSetupAvailableRepoTeamSelect(cfg config, repo, currentTeamID string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string) (string, bool, error) {
	choices, err := setupAvailableRepoTeamChoices(cfg, repo, currentTeamID)
	if err != nil {
		return "", false, err
	}
	if len(choices) == 0 {
		if _, err := runSetupBrokerOutputWithRaw(reader, rawInput, stdout, title, "No teams are available to grant access."); err != nil {
			return "", false, err
		}
		return "", false, nil
	}
	return runSetupSelectWithRaw(reader, rawInput, stdout, title, choices, "")
}

func setupAvailableRepoTeamChoices(cfg config, repo, currentTeamID string) ([]setupChoice, error) {
	brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
	if err != nil {
		return nil, err
	}
	var teamsResp brokerTeamsResponse
	if err := brokerPost(brokerURL, "/teams/list", brokerRepoAdminRequest{}, &teamsResp); err != nil {
		return nil, err
	}
	repos, err := setupBrokerRepos(cfg)
	if err != nil {
		return nil, err
	}
	logical, err := normalizeLogicalRepoName(repo)
	if err != nil {
		return nil, err
	}
	attached := map[string]struct{}{}
	for _, item := range repos {
		itemLogical := firstNonEmpty(item.Logical, item.Repo.Logical)
		if itemLogical != logical {
			continue
		}
		for _, grant := range item.Teams {
			id := firstNonEmpty(grant.ID, grant.TeamID)
			if id != "" {
				attached[id] = struct{}{}
			}
		}
		if item.Repo.TeamID != "" {
			attached[item.Repo.TeamID] = struct{}{}
		}
	}
	repoTeams, err := setupBrokerRepoTeamIDs(cfg, repo, currentTeamID)
	if err != nil {
		return nil, err
	}
	for _, id := range repoTeams {
		attached[id] = struct{}{}
	}
	if currentTeamID != "" {
		attached[currentTeamID] = struct{}{}
	}
	var choices []setupChoice
	for _, team := range teamsResp.Teams {
		id := strings.TrimSpace(team.ID)
		if id == "" {
			continue
		}
		if _, ok := attached[id]; ok {
			continue
		}
		choices = append(choices, setupChoice{Label: setupTeamDisplayName(id, team), Value: id, Help: fmt.Sprintf("%d member(s)", len(team.Members))})
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].Label < choices[j].Label })
	return choices, nil
}

func setupBrokerRepoTeamIDs(cfg config, repo, teamID string) ([]string, error) {
	brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
	if err != nil {
		return nil, err
	}
	brokerRepo, err := setupBrokerTeamRepoForAction(cfg, repo, teamID)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Teams []brokerRepoTeamGrant `json:"teams"`
	}
	if err := brokerPost(brokerURL, "/repo/teams/list", brokerRepoInfoRequest{Repo: brokerRepo}, &resp); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(resp.Teams))
	for _, grant := range resp.Teams {
		id := firstNonEmpty(grant.ID, grant.TeamID)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

type setupRepoUser struct {
	User      string
	Role      string
	KeyCount  int
	PublicKey []string
	Grant     bool
	Pending   bool
}

func setupRepoUsers(cfg config, repo, teamID string) ([]setupRepoUser, error) {
	keys, err := setupRepoKeys(cfg, repo, teamID)
	if err != nil {
		return nil, err
	}
	byUser := map[string]*setupRepoUser{}
	for _, key := range keys {
		user := strings.TrimSpace(key.User)
		if user == "" {
			user = "unknown"
		}
		mapKey := strings.ToLower(user)
		entry := byUser[mapKey]
		if entry == nil {
			entry = &setupRepoUser{User: user, Role: firstNonEmpty(key.Role, "read")}
			byUser[mapKey] = entry
		}
		entry.KeyCount++
		entry.PublicKey = append(entry.PublicKey, key.PublicKey)
		entry.Role = strongerSetupRepoRole(entry.Role, firstNonEmpty(key.Role, "read"))
	}
	grants, grantErr := setupRepoUserGrants(cfg, repo, teamID)
	if grantErr == nil {
		brokerUsers, _ := setupBrokerUsers(cfg)
		byID := map[string]brokerUserInfo{}
		byName := map[string]brokerUserInfo{}
		for _, user := range brokerUsers {
			if user.ID != "" {
				byID[user.ID] = user
			}
			if user.Username != "" {
				byName[strings.ToLower(user.Username)] = user
			}
		}
		for _, grant := range grants {
			userName := firstNonEmpty(grant.User, grant.Username, grant.UserID)
			if user, ok := byID[grant.UserID]; ok {
				userName = firstNonEmpty(user.Username, userName)
			}
			if userName == "" {
				continue
			}
			mapKey := strings.ToLower(userName)
			entry := byUser[mapKey]
			if entry == nil {
				entry = &setupRepoUser{User: userName, Role: firstNonEmpty(grant.Role, "read")}
				byUser[mapKey] = entry
			}
			entry.Grant = true
			entry.Role = strongerSetupRepoRole(entry.Role, firstNonEmpty(grant.Role, "read"))
			if user, ok := byName[mapKey]; ok {
				entry.KeyCount = maxSetupDialogInt(entry.KeyCount, len(user.Keys))
				entry.Pending = user.Pending || len(user.Keys) == 0
			}
		}
	}
	users := make([]setupRepoUser, 0, len(byUser))
	for _, user := range byUser {
		users = append(users, *user)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].User < users[j].User })
	return users, nil
}

func setupRepoKeys(cfg config, repo, teamID string) ([]brokerKey, error) {
	brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
	if err != nil {
		return nil, err
	}
	actionCfg, err := setupBrokerRepoConfig(cfg, repo)
	if err != nil {
		return nil, err
	}
	actionCfg.provider = firstNonEmpty(actionCfg.provider, cfg.provider, "gcs")
	actionCfg.teamID = strings.TrimSpace(teamID)
	return brokerListKeys(brokerURL, actionCfg)
}

func setupRepoUserGrants(cfg config, repo, teamID string) ([]brokerRepoUserGrant, error) {
	brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
	if err != nil {
		return nil, err
	}
	brokerRepo, err := setupBrokerTeamRepoForAction(cfg, repo, teamID)
	if err != nil {
		return nil, err
	}
	var resp brokerRepoUsersResponse
	if err := brokerPost(brokerURL, "/repo/users/list", brokerRepoAdminRequest{Repo: brokerRepo}, &resp); err != nil {
		return nil, err
	}
	return resp.Users, nil
}

func strongerSetupRepoRole(a, b string) string {
	if setupRepoRoleRank(b) > setupRepoRoleRank(a) {
		return b
	}
	return a
}

func setupRepoRoleRank(role string) int {
	switch normalizeBrokerRole(role) {
	case "owner":
		return 6
	case "admin":
		return 5
	case "maintainer":
		return 4
	case "developer":
		return 3
	case "triage":
		return 2
	case "read":
		return 1
	default:
		return 0
	}
}

func runSetupManagedTeamRepositoryUserWithRaw(cfg config, teamID, teamName, repo, user string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	repoName := logicalRepoDisplayName(repo)
	title := setupBreadcrumb("Manage team", teamName, "Repositories", repoName, user)
	for {
		action, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, title, []setupChoice{
			{Label: "edit role cap", Value: "user-edit", Help: "change repository role for this user"},
			{Label: "suspend access", Value: "user-suspend", Help: "suspend this user's direct keys"},
			{Label: "remove access", Value: "user-remove", Help: "remove this user's direct keys"},
			{Label: "back", Value: "back", Help: "return to users"},
		}, "")
		if err != nil {
			return "", err
		}
		if !ok || action == "back" {
			return "", errSetupBack
		}
		msg, err := runSetupManagedTeamRepositoryUserAction(cfg, teamID, teamName, repo, user, action, reader, rawInput, stdout)
		if err != nil {
			return "", err
		}
		if msg == "No changes made." {
			continue
		}
		return msg, nil
	}
}

func runSetupManagedTeamRepositoryUserAction(cfg config, teamID, teamName, repo, user, action string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	repoName := logicalRepoDisplayName(repo)
	keys, err := setupRepoKeysForUser(cfg, repo, teamID, user)
	if err != nil {
		return "", err
	}
	grant, hasGrant, err := setupRepoUserGrantForUser(cfg, repo, teamID, user)
	if err != nil {
		return "", err
	}
	if len(keys) == 0 && !hasGrant {
		return "No repository access found for " + user + ".", nil
	}
	brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
	if err != nil {
		return "", err
	}
	actionCfg, err := setupBrokerRepoConfig(cfg, repo)
	if err != nil {
		return "", err
	}
	actionCfg.provider = firstNonEmpty(actionCfg.provider, cfg.provider, "gcs")
	actionCfg.teamID = strings.TrimSpace(teamID)
	switch action {
	case "user-edit":
		role, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", teamName, "Repositories", repoName, user, "Role"), setupRepoRoleChoices(), "developer")
		if err != nil || !ok {
			return "No changes made.", err
		}
		if hasGrant {
			if err := brokerPost(brokerURL, "/repo/users/upsert", brokerRepoAdminRequest{Repo: repoForBroker(actionCfg), UserID: grant.UserID, User: firstNonEmpty(grant.User, grant.Username, user), Role: role}, nil); err != nil {
				return "", err
			}
		}
		for _, key := range keys {
			if err := brokerPost(brokerURL, "/keys/remove", brokerKeyRequest{Repo: repoForBroker(actionCfg), Key: key.PublicKey}, nil); err != nil {
				return "", err
			}
		}
		publicKeys := make([]string, 0, len(keys))
		for _, key := range keys {
			publicKeys = append(publicKeys, key.PublicKey)
		}
		if err := brokerAddKeysWithSource(brokerURL, actionCfg, user, role, "setup", publicKeys); err != nil {
			return "", err
		}
		return fmt.Sprintf("updated %s on %s to %s", user, logicalRepoDisplayName(repo), role), nil
	case "user-suspend", "user-remove":
		path := "/keys/suspend"
		verb := "suspended"
		if action == "user-remove" {
			path = "/keys/remove"
			verb = "removed"
		}
		if action == "user-remove" && hasGrant {
			if err := brokerPost(brokerURL, "/repo/users/remove", brokerRepoAdminRequest{Repo: repoForBroker(actionCfg), UserID: grant.UserID, User: firstNonEmpty(grant.User, grant.Username, user)}, nil); err != nil {
				return "", err
			}
		}
		for _, key := range keys {
			if err := brokerPost(brokerURL, path, brokerKeyRequest{Repo: repoForBroker(actionCfg), Key: key.PublicKey}, nil); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("%s access for %s on %s", verb, user, logicalRepoDisplayName(repo)), nil
	default:
		return "", fmt.Errorf("unknown repository user action %q", action)
	}
}

func setupRepoKeysForUser(cfg config, repo, teamID, user string) ([]brokerKey, error) {
	keys, err := setupRepoKeys(cfg, repo, teamID)
	if err != nil {
		return nil, err
	}
	var out []brokerKey
	for _, key := range keys {
		if strings.EqualFold(strings.TrimSpace(key.User), strings.TrimSpace(user)) {
			out = append(out, key)
		}
	}
	return out, nil
}

func setupRepoUserGrantForUser(cfg config, repo, teamID, user string) (brokerRepoUserGrant, bool, error) {
	grants, err := setupRepoUserGrants(cfg, repo, teamID)
	if err != nil {
		return brokerRepoUserGrant{}, false, err
	}
	needle := strings.ToLower(strings.TrimSpace(user))
	for _, grant := range grants {
		if strings.ToLower(firstNonEmpty(grant.User, grant.Username, grant.UserID)) == needle {
			return grant, true, nil
		}
	}
	return brokerRepoUserGrant{}, false, nil
}

func runSetupPendingRepoInviteSelect(cfg config, repo brokerRepo, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string) (string, bool, error) {
	choices, err := setupPendingRepoInviteChoices(cfg, repo)
	if err != nil {
		return "", false, err
	}
	if len(choices) == 0 {
		if _, err := runSetupBrokerOutputWithRaw(reader, rawInput, stdout, title, "No pending invites."); err != nil {
			return "", false, err
		}
		return "", false, nil
	}
	return runSetupSelectWithRaw(reader, rawInput, stdout, title, choices, "")
}

func setupPendingRepoInviteChoices(cfg config, repo brokerRepo) ([]setupChoice, error) {
	brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
	if err != nil {
		return nil, err
	}
	var resp brokerRepoInvitesResponse
	if err := brokerPost(brokerURL, "/keys/invite/list", brokerOwnerTransferRequest{Repo: repo}, &resp); err != nil {
		return nil, err
	}
	choices := make([]setupChoice, 0, len(resp.Invites))
	for _, invite := range resp.Invites {
		user := strings.TrimSpace(invite.User)
		if user == "" {
			continue
		}
		choices = append(choices, setupChoice{Label: user, Value: user, Help: firstNonEmpty(invite.Role, "read")})
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].Label < choices[j].Label })
	return choices, nil
}

func setupBrokerTeamRepoForAction(cfg config, repo, teamID string) (brokerRepo, error) {
	actionCfg, err := setupBrokerRepoConfig(cfg, repo)
	if err != nil {
		return brokerRepo{}, err
	}
	actionCfg.provider = firstNonEmpty(actionCfg.provider, cfg.provider, "gcs")
	actionCfg.teamID = strings.TrimSpace(teamID)
	return repoForBroker(actionCfg), nil
}

func runSetupTeamRepoAccessUpsert(cfg config, teamID, teamName, repo string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	return runSetupTeamRepoAccessUpsertMany(cfg, teamID, teamName, []string{repo}, reader, rawInput, stdout)
}

func runSetupTeamRepoAccessUpsertMany(cfg config, teamID, teamName string, repos []string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer) (string, error) {
	var out bytes.Buffer
	if len(repos) == 0 {
		return "No changes made.", nil
	}
	role, ok, err := runSetupSelectWithRaw(reader, rawInput, stdout, setupBreadcrumb("Manage team", firstNonEmpty(teamName, teamID), "Repository role cap"), setupRepoRoleCapChoices(), "developer")
	if err != nil || !ok {
		return "No changes made.", err
	}
	for _, repo := range repos {
		actionCfg, err := setupBrokerRepoConfig(cfg, repo)
		if err != nil {
			return "", err
		}
		if err := brokerAdminCommandWithInput(actionCfg, []string{"teams", "repo", "add", teamID, role}, strings.NewReader(""), &out); err != nil {
			return "", err
		}
	}
	return strings.TrimSpace(out.String()), nil
}

func setupBrokerUserRoleChoices() []setupChoice {
	return []setupChoice{
		{Label: "user", Value: "user", Help: "normal broker user"},
		{Label: "admin", Value: "admin", Help: "broker administration"},
	}
}

func setupRepoRoleChoices() []setupChoice {
	return []setupChoice{
		{Label: "read", Value: "read", Help: "read repository"},
		{Label: "triage", Value: "triage", Help: "issues and PR triage"},
		{Label: "developer", Value: "developer", Help: "push branches"},
		{Label: "maintainer", Value: "maintainer", Help: "merge and maintain"},
		{Label: "admin", Value: "admin", Help: "repo administration"},
	}
}

func setupRepoRoleCapChoices() []setupChoice {
	choices := setupRepoRoleChoices()
	choices = append(choices, setupChoice{Label: "owner", Value: "owner", Help: "owner-only repository actions"})
	return choices
}

func runSetupTeamSelect(cfg config, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string) (string, bool, error) {
	teams, err := setupBrokerTeamChoices(cfg)
	if err != nil {
		return "", false, err
	}
	return runSetupSelectWithRaw(reader, rawInput, stdout, title, teams, "")
}

func runSetupUserSelect(cfg config, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string) (string, bool, error) {
	users, err := setupBrokerUserChoices(cfg)
	if err != nil {
		return "", false, err
	}
	return runSetupSelectWithRaw(reader, rawInput, stdout, title, users, "")
}

func runSetupAvailableRepoInviteUserSelect(cfg config, repo, teamID string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string) (string, bool, error) {
	users, err := setupAvailableRepoInviteUserChoices(cfg, repo, teamID)
	if err != nil {
		return "", false, err
	}
	if len(users) == 0 {
		if _, err := runSetupBrokerOutputWithRaw(reader, rawInput, stdout, title, "No users are available to invite."); err != nil {
			return "", false, err
		}
		return "", false, nil
	}
	return runSetupSelectWithRaw(reader, rawInput, stdout, title, users, "")
}

func runSetupAvailableTeamUserSelect(cfg config, teamID string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string) (string, bool, error) {
	users, err := setupAvailableTeamUserChoices(cfg, teamID)
	if err != nil {
		return "", false, err
	}
	if len(users) == 0 {
		if _, err := runSetupBrokerOutputWithRaw(reader, rawInput, stdout, title, "No users are available to add."); err != nil {
			return "", false, err
		}
		return "", false, nil
	}
	return runSetupSelectWithRaw(reader, rawInput, stdout, title, users, "")
}

func runSetupRepoSelect(cfg config, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string) (string, bool, error) {
	repos, err := setupBrokerRepoChoices(cfg)
	if err != nil {
		return "", false, err
	}
	return runSetupSelectWithRaw(reader, rawInput, stdout, title, repos, "")
}

func runSetupRepoMultiSelect(cfg config, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string) ([]string, bool, error) {
	repos, err := setupBrokerRepoChoices(cfg)
	if err != nil {
		return nil, false, err
	}
	return runSetupMultiSelectWithRaw(reader, rawInput, stdout, title, repos)
}

func runSetupTeamMemberSelect(cfg config, teamID string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string) (string, bool, error) {
	team, err := setupBrokerTeamInfo(cfg, teamID)
	if err != nil {
		return "", false, err
	}
	var choices []setupChoice
	for _, member := range team.Members {
		user := firstNonEmpty(member.Username, member.UserID)
		if user == "" {
			continue
		}
		choices = append(choices, setupChoice{Label: user, Value: user, Help: "team role cap " + firstNonEmpty(member.Role, "read")})
	}
	return runSetupSelectWithRaw(reader, rawInput, stdout, title, choices, "")
}

func runSetupTeamRepoSelect(cfg config, teamID string, reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string) (string, bool, error) {
	choices, err := setupBrokerTeamRepoChoices(cfg, teamID)
	if err != nil {
		return "", false, err
	}
	return runSetupSelectWithRaw(reader, rawInput, stdout, title, choices, "")
}

func setupBrokerTeamChoices(cfg config) ([]setupChoice, error) {
	brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
	if err != nil {
		return nil, err
	}
	var resp brokerTeamsResponse
	if err := brokerPost(brokerURL, "/teams/list", brokerRepoAdminRequest{}, &resp); err != nil {
		return nil, err
	}
	var choices []setupChoice
	for _, team := range resp.Teams {
		choices = append(choices, setupChoice{Label: team.Name, Value: team.ID, Help: fmt.Sprintf("%d member(s)", len(team.Members))})
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].Label < choices[j].Label })
	return choices, nil
}

func setupBrokerTeamInfo(cfg config, teamID string) (brokerTeamInfo, error) {
	brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
	if err != nil {
		return brokerTeamInfo{}, err
	}
	var resp brokerTeamsResponse
	if err := brokerPost(brokerURL, "/teams/list", brokerRepoAdminRequest{}, &resp); err != nil {
		return brokerTeamInfo{}, err
	}
	for _, team := range resp.Teams {
		if team.ID == teamID || strings.EqualFold(team.Name, teamID) {
			return team, nil
		}
	}
	return brokerTeamInfo{}, fmt.Errorf("team %s not found", teamID)
}

func setupFormatTeamMembers(team brokerTeamInfo) string {
	if len(team.Members) == 0 {
		return "No members."
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("%-32s  %-14s", "User", "Team role cap"))
	lines = append(lines, fmt.Sprintf("%-32s  %-14s", strings.Repeat("-", 4), strings.Repeat("-", 13)))
	for _, member := range team.Members {
		user := firstNonEmpty(member.Username, member.UserID)
		if user == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%-32s  %-14s", user, firstNonEmpty(member.Role, "read")))
	}
	if len(lines) > 2 {
		sort.Strings(lines[2:])
	}
	return strings.Join(lines, "\n")
}

func setupTeamDisplayName(fallback string, team brokerTeamInfo) string {
	return firstNonEmpty(team.Name, team.ID, fallback)
}

func setupBrokerUserChoices(cfg config) ([]setupChoice, error) {
	users, err := setupBrokerUsers(cfg)
	if err != nil {
		return nil, err
	}
	return setupBrokerUserChoicesFromUsers(users, nil), nil
}

func setupAvailableTeamUserChoices(cfg config, teamID string) ([]setupChoice, error) {
	users, err := setupBrokerUsers(cfg)
	if err != nil {
		return nil, err
	}
	team, err := setupBrokerTeamInfo(cfg, teamID)
	if err != nil {
		return nil, err
	}
	members := map[string]struct{}{}
	for _, member := range team.Members {
		user := strings.ToLower(firstNonEmpty(member.Username, member.UserID))
		if user != "" {
			members[user] = struct{}{}
		}
	}
	return setupBrokerUserChoicesFromUsers(users, members), nil
}

func setupAvailableRepoInviteUserChoices(cfg config, repo, teamID string) ([]setupChoice, error) {
	users, err := setupBrokerUsers(cfg)
	if err != nil {
		return nil, err
	}
	exclude := map[string]struct{}{}
	repoUsers, err := setupRepoUsers(cfg, repo, teamID)
	if err != nil {
		return nil, err
	}
	for _, user := range repoUsers {
		if user.User != "" {
			exclude[strings.ToLower(user.User)] = struct{}{}
		}
	}
	brokerRepo, err := setupBrokerTeamRepoForAction(cfg, repo, teamID)
	if err != nil {
		return nil, err
	}
	pending, err := setupPendingRepoInviteChoices(cfg, brokerRepo)
	if err != nil {
		return nil, err
	}
	for _, invite := range pending {
		if invite.Value != "" {
			exclude[strings.ToLower(invite.Value)] = struct{}{}
		}
	}
	return setupBrokerUserChoicesFromUsers(users, exclude), nil
}

func setupBrokerUsers(cfg config) ([]brokerUserInfo, error) {
	brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
	if err != nil {
		return nil, err
	}
	var resp brokerUsersResponse
	if err := brokerPost(brokerURL, "/broker/users/list", brokerRepoAdminRequest{}, &resp); err != nil {
		return nil, err
	}
	return resp.Users, nil
}

func setupBrokerUserByName(cfg config, username string) (brokerUserInfo, bool, error) {
	users, err := setupBrokerUsers(cfg)
	if err != nil {
		return brokerUserInfo{}, false, err
	}
	needle := strings.ToLower(strings.TrimSpace(username))
	for _, user := range users {
		if strings.ToLower(firstNonEmpty(user.Username, user.ID)) == needle {
			return user, true, nil
		}
	}
	return brokerUserInfo{}, false, nil
}

func setupBrokerUserStatus(user brokerUserInfo) string {
	parts := []string{firstNonEmpty(user.BrokerRole, "user")}
	if user.Suspended {
		parts = append(parts, "suspended")
	} else if user.Pending || len(user.Keys) == 0 {
		parts = append(parts, "pending")
	}
	if len(user.Keys) == 1 {
		parts = append(parts, "1 key")
	} else if len(user.Keys) > 1 {
		parts = append(parts, fmt.Sprintf("%d keys", len(user.Keys)))
	}
	return strings.Join(parts, " · ")
}

func setupBrokerUserChoicesFromUsers(users []brokerUserInfo, exclude map[string]struct{}) []setupChoice {
	var choices []setupChoice
	groupPending := exclude != nil
	for _, user := range users {
		username := firstNonEmpty(user.Username, user.ID)
		if username == "" {
			continue
		}
		if _, ok := exclude[strings.ToLower(username)]; ok {
			continue
		}
		label := username
		group := ""
		if groupPending && (user.Pending || len(user.Keys) == 0) {
			label += " *"
			group = "pending users:"
			label = "- " + label
		} else if user.Pending || len(user.Keys) == 0 {
			label += " *"
		}
		choices = append(choices, setupChoice{Label: label, Value: username, Help: user.BrokerRole, Group: group})
	}
	sort.Slice(choices, func(i, j int) bool {
		if choices[i].Group != choices[j].Group {
			return choices[i].Group < choices[j].Group
		}
		return choices[i].Label < choices[j].Label
	})
	return choices
}

func setupBrokerTeamRepoChoices(cfg config, teamID string) ([]setupChoice, error) {
	repos, err := setupBrokerRepos(cfg)
	if err != nil {
		return nil, err
	}
	var choices []setupChoice
	for _, repo := range repos {
		logical := firstNonEmpty(repo.Logical, repo.Repo.Logical)
		for _, grant := range repo.Teams {
			if grant.ID == teamID || grant.TeamID == teamID {
				choices = append(choices, setupChoice{Label: logicalRepoDisplayName(logical), Value: logical, Help: "role cap " + firstNonEmpty(grant.Role, "read")})
			}
		}
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].Label < choices[j].Label })
	return choices, nil
}

func setupFormatTeamRepositories(cfg config, teamID string) string {
	choices, err := setupBrokerTeamRepoChoices(cfg, teamID)
	if err != nil {
		return err.Error()
	}
	return setupFormatTeamRepositoriesForChoices(choices)
}

func setupFormatTeamRepositoriesForChoices(choices []setupChoice) string {
	if len(choices) == 0 {
		return "No repository access."
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("%-32s  %-14s", "Repository", "Role cap"))
	lines = append(lines, fmt.Sprintf("%-32s  %-14s", strings.Repeat("-", 10), strings.Repeat("-", 8)))
	for _, choice := range choices {
		role := strings.TrimPrefix(choice.Help, "role cap ")
		lines = append(lines, fmt.Sprintf("%-32s  %-14s", choice.Label, firstNonEmpty(role, "read")))
	}
	return strings.Join(lines, "\n")
}

func setupBrokerRepoChoices(cfg config) ([]setupChoice, error) {
	repos, err := setupBrokerRepos(cfg)
	if err != nil {
		return nil, err
	}
	var choices []setupChoice
	for _, repo := range repos {
		logical := firstNonEmpty(repo.Logical, repo.Repo.Logical)
		if logical == "" {
			continue
		}
		choices = append(choices, setupChoice{Label: logicalRepoDisplayName(logical), Value: logical, Help: firstNonEmpty(repo.Repo.TeamID, coreTeamName)})
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].Label < choices[j].Label })
	return choices, nil
}

func setupBrokerRepos(cfg config) ([]brokerRepoInfo, error) {
	brokerURL, err := brokerURLFromConfigOrDiscovery(cfg)
	if err != nil {
		return nil, err
	}
	var resp brokerRepoListResponse
	if err := brokerPost(brokerURL, "/repos/list", brokerRepoAdminRequest{}, &resp); err != nil {
		return nil, err
	}
	return resp.Repos, nil
}

func runSetupSelectWithRaw(reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string, choices []setupChoice, selected string) (string, bool, error) {
	if len(choices) == 0 {
		return "", false, fmt.Errorf("%s has no selectable entries", strings.ToLower(title))
	}
	rawMode, restore, err := setupDialogRawMode(rawInput)
	if err != nil {
		return "", false, err
	}
	defer restore()
	state := setupSelectState{Title: title, Choices: choices, Button: -1}
	for i, choice := range choices {
		if choice.Value == selected || choice.Label == selected {
			state.Cursor = i
			break
		}
	}
	for {
		fmt.Fprint(stdout, renderSetupSelectFrame(state, rawMode))
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", false, nil
			}
			return "", false, err
		}
		switch b {
		case 0x03:
			return "", false, errors.New("setup canceled")
		case 0x04:
			value, ok := state.activate()
			if ok {
				return value, value != "", nil
			}
		case '\r', '\n', ' ':
			value, ok := state.activate()
			if ok {
				return value, value != "", nil
			}
		case '\t':
			state.tab()
		case 0x1b:
			last, ok, err := setupReadEscapeSequence(reader)
			if err != nil {
				return "", false, err
			}
			if !ok {
				return "", false, nil
			}
			switch last {
			case 'A':
				state.up()
			case 'B':
				state.down()
			case 'C':
				value, ok := state.activate()
				if ok {
					return value, value != "", nil
				}
			case 'D':
				return "", false, nil
			}
		}
	}
}

func setupReadEscapeSequence(reader *bufio.Reader) (byte, bool, error) {
	next, err := reader.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if next != '[' {
		return next, true, nil
	}
	last, err := reader.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return last, true, nil
}

func (s *setupSelectState) rows() int { return len(s.Choices) }

func (s *setupSelectState) visibleRange() (int, int) {
	const maxRows = 10
	rows := s.rows()
	if s.Cursor < s.Scroll {
		s.Scroll = s.Cursor
	}
	if s.Cursor >= s.Scroll+maxRows {
		s.Scroll = s.Cursor - maxRows + 1
	}
	if s.Scroll < 0 {
		s.Scroll = 0
	}
	if s.Scroll > rows-maxRows {
		s.Scroll = maxSetupDialogInt(0, rows-maxRows)
	}
	return s.Scroll, minSetupDialogInt(s.Scroll+maxRows, rows)
}

func (s *setupSelectState) up() {
	if s.rows() == 0 {
		return
	}
	s.Button = -1
	if s.Cursor == 0 {
		s.Cursor = s.rows() - 1
		return
	}
	s.Cursor--
}

func (s *setupSelectState) down() {
	if s.rows() == 0 {
		return
	}
	s.Button = -1
	s.Cursor = (s.Cursor + 1) % s.rows()
}

func (s *setupSelectState) tab() {
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

func (s setupSelectState) activate() (string, bool) {
	if s.Button == 1 {
		return "", true
	}
	if s.Button == 0 || s.Button < 0 {
		if s.Cursor >= 0 && s.Cursor < len(s.Choices) {
			return s.Choices[s.Cursor].Value, true
		}
	}
	return "", false
}

func renderSetupSelectFrame(state setupSelectState, rawMode bool) string {
	rendered := renderSetupSelectWithStyle(state, rawMode)
	if !rawMode {
		return rendered
	}
	rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	return "\x1b[?25l\x1b[H\x1b[2J" + rendered
}

func renderSetupSelectWithStyle(state setupSelectState, style bool) string {
	start, end := state.visibleRange()
	width := setupDialogDynamicWidth(58, state.Title, "Up/Down move  Right/Enter select  Tab buttons", "Left/Esc back  Ctrl-C cancel")
	for i := start; i < end; i++ {
		choice := state.Choices[i]
		width = setupDialogDynamicWidth(width, fmt.Sprintf("> %-26s %s", choice.Label, choice.Help))
		if choice.Group != "" {
			width = setupDialogDynamicWidth(width, choice.Group)
		}
	}
	var lines []string
	lines = append(lines,
		setupDialogBorder(width),
		setupDialogTitleRow(width),
		setupDialogBorder(width),
		setupDialogRowWidth(state.Title, width),
		setupDialogRowWidth("", width),
	)
	lastGroup := ""
	for i := start; i < end; i++ {
		choice := state.Choices[i]
		if choice.Group != "" && choice.Group != lastGroup {
			lines = append(lines, setupDialogRowWidth(choice.Group, width))
			lastGroup = choice.Group
		}
		marker := " "
		if state.Button < 0 && state.Cursor == i {
			marker = ">"
		}
		lines = append(lines, setupDialogRowStyledWidth(fmt.Sprintf("%s %-26s %s", marker, choice.Label, choice.Help), width, setupDialogSectionStyle(style, state.Button < 0 && state.Cursor == i)))
	}
	if len(state.Choices) > 10 {
		lines = append(lines, setupDialogRowWidth(setupBrokerScrollBar(start, end, len(state.Choices)), width))
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
		setupDialogRowWidth("", width),
		setupDialogBorder(width),
		setupDialogRowWidth(setupDialogButton("[ OK ]", okStyle)+"    "+setupDialogButton("[ Cancel ]", cancelStyle), width),
		setupDialogRowWidth("Up/Down move  Right/Enter select  Tab buttons", width),
		setupDialogRowWidth("Left/Esc back  Ctrl-C cancel", width),
		setupDialogBorder(width),
	)
	rendered := strings.Join(lines, "\n") + "\n"
	if setupSelectHasPendingUserNote(state) {
		rendered += "* pending invite or no accepted key yet\n"
	}
	return rendered
}

func setupSelectHasPendingUserNote(state setupSelectState) bool {
	if !strings.Contains(strings.ToLower(state.Title), "username") {
		return false
	}
	start, end := state.visibleRange()
	for i := start; i < end; i++ {
		if strings.HasSuffix(strings.TrimSpace(state.Choices[i].Label), "*") {
			return true
		}
	}
	return false
}

func runSetupMultiSelectWithRaw(reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string, choices []setupChoice) ([]string, bool, error) {
	if len(choices) == 0 {
		return nil, false, fmt.Errorf("%s has no selectable entries", strings.ToLower(title))
	}
	state := setupMultiSelectState{Title: title, Choices: choices, Selected: make([]bool, len(choices)), Button: -1}
	for {
		fmt.Fprint(stdout, renderSetupMultiSelectFrame(state, true))
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, false, nil
			}
			return nil, false, err
		}
		switch b {
		case 0x03:
			return nil, false, errors.New("setup canceled")
		case 0x04:
			if selected, ok := state.deploy(); ok {
				return selected, true, nil
			}
		case '\r', '\n', ' ':
			if state.Button == 1 {
				return nil, false, nil
			}
			if state.Button == 0 {
				if selected, ok := state.deploy(); ok {
					return selected, true, nil
				}
				continue
			}
			state.toggle()
		case '\t':
			state.tab()
		case 0x1b:
			last, ok, err := setupReadEscapeSequence(reader)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				return nil, false, nil
			}
			switch last {
			case 'A':
				state.up()
			case 'B':
				state.down()
			case 'C':
				if selected, ok := state.deploy(); ok {
					return selected, true, nil
				}
			case 'D':
				return nil, false, nil
			}
		}
	}
}

func (s *setupMultiSelectState) rows() int { return len(s.Choices) }

func (s *setupMultiSelectState) visibleRange() (int, int) {
	const maxRows = 10
	rows := s.rows()
	if s.Cursor < s.Scroll {
		s.Scroll = s.Cursor
	}
	if s.Cursor >= s.Scroll+maxRows {
		s.Scroll = s.Cursor - maxRows + 1
	}
	if s.Scroll < 0 {
		s.Scroll = 0
	}
	if s.Scroll > rows-maxRows {
		s.Scroll = maxSetupDialogInt(0, rows-maxRows)
	}
	return s.Scroll, minSetupDialogInt(s.Scroll+maxRows, rows)
}

func (s *setupMultiSelectState) up() {
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

func (s *setupMultiSelectState) down() {
	if s.rows() == 0 {
		return
	}
	s.Button = -1
	s.Message = ""
	s.Cursor = (s.Cursor + 1) % s.rows()
}

func (s *setupMultiSelectState) tab() {
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

func (s *setupMultiSelectState) toggle() {
	s.Message = ""
	if s.Cursor >= 0 && s.Cursor < len(s.Selected) {
		s.Selected[s.Cursor] = !s.Selected[s.Cursor]
	}
}

func (s *setupMultiSelectState) deploy() ([]string, bool) {
	var selected []string
	for i, ok := range s.Selected {
		if ok {
			selected = append(selected, s.Choices[i].Value)
		}
	}
	if len(selected) == 0 {
		s.Message = "Select at least one repository."
		return nil, false
	}
	return selected, true
}

func renderSetupMultiSelectFrame(state setupMultiSelectState, rawMode bool) string {
	rendered := renderSetupMultiSelectWithStyle(state, rawMode)
	if !rawMode {
		return rendered
	}
	rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	return "\x1b[?25l\x1b[H\x1b[2J" + rendered
}

func renderSetupMultiSelectWithStyle(state setupMultiSelectState, style bool) string {
	start, end := state.visibleRange()
	width := setupDialogDynamicWidth(58, state.Title, "Space/Enter toggles  Right/Ctrl-D OK", "Left/Esc back  Arrows move  Tab buttons")
	for i := start; i < end; i++ {
		choice := state.Choices[i]
		width = setupDialogDynamicWidth(width, fmt.Sprintf("> [x] %-22s %s", choice.Label, choice.Help))
	}
	var lines []string
	lines = append(lines,
		setupDialogBorder(width),
		setupDialogTitleRow(width),
		setupDialogBorder(width),
		setupDialogRowWidth(state.Title, width),
		setupDialogRowWidth("", width),
	)
	for i := start; i < end; i++ {
		choice := state.Choices[i]
		marker := " "
		if state.Button < 0 && state.Cursor == i {
			marker = ">"
		}
		checked := "[ ]"
		if i < len(state.Selected) && state.Selected[i] {
			checked = "[x]"
		}
		lines = append(lines, setupDialogRowStyledWidth(fmt.Sprintf("%s %s %-22s %s", marker, checked, choice.Label, choice.Help), width, setupDialogSectionStyle(style, state.Button < 0 && state.Cursor == i)))
	}
	if len(state.Choices) > 10 {
		lines = append(lines, setupDialogRowWidth(setupBrokerScrollBar(start, end, len(state.Choices)), width))
	}
	if state.Message != "" {
		lines = append(lines, setupDialogRowStyledWidth(state.Message, width, setupDialogANSI(style, "33")))
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
		setupDialogRowWidth("", width),
		setupDialogBorder(width),
		setupDialogRowWidth(setupDialogButton("[ OK ]", okStyle)+"    "+setupDialogButton("[ Cancel ]", cancelStyle), width),
		setupDialogRowWidth("Space/Enter toggles  Right/Ctrl-D OK", width),
		setupDialogRowWidth("Left/Esc back  Arrows move  Tab buttons", width),
		setupDialogBorder(width),
	)
	return strings.Join(lines, "\n") + "\n"
}

func runSetupTextFormWithRaw(reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title string, fields []setupTextField) ([]string, bool, error) {
	state := setupTextFormState{Title: title, Fields: fields, Button: -1}
	rawMode, restore, err := setupDialogRawMode(rawInput)
	if err != nil {
		return nil, false, err
	}
	defer restore()
	if len(fields) == 1 {
		state.Editing = true
		state.EditOriginal = fields[0].Value
	}
	for {
		fmt.Fprint(stdout, renderSetupTextFormFrame(state, rawMode))
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, false, nil
			}
			return nil, false, err
		}
		switch b {
		case 0x03:
			return nil, false, errors.New("setup canceled")
		case 0x04:
			if values, ok := state.deploy(); ok {
				return values, true, nil
			}
		case '\r', '\n':
			if state.Editing {
				state.Editing = false
				state.EditOriginal = ""
				continue
			}
			if state.Button == 1 {
				return nil, false, nil
			}
			if values, ok := state.activate(); ok {
				return values, true, nil
			}
		case ' ':
			if state.Editing {
				state.appendByte(b)
				continue
			}
			if state.Button == 1 {
				return nil, false, nil
			}
			if values, ok := state.activate(); ok {
				return values, true, nil
			}
		case '\t':
			state.tab()
		case 0x7f, 0x08:
			if state.Editing {
				state.backspace()
			}
		case 0x1b:
			if state.Editing {
				state.Fields[state.Cursor].Value = state.EditOriginal
				state.Editing = false
				state.EditOriginal = ""
				continue
			}
			last, ok, err := setupReadEscapeSequence(reader)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				return nil, false, nil
			}
			switch last {
			case 'A':
				state.up()
			case 'B':
				state.down()
			case 'C':
				if values, ok := state.activate(); ok {
					return values, true, nil
				}
			case 'D':
				return nil, false, nil
			}
		default:
			if state.Editing {
				state.appendByte(b)
			}
		}
	}
}

func (s *setupTextFormState) rows() int { return len(s.Fields) }

func (s *setupTextFormState) up() {
	if s.Editing || s.rows() == 0 {
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

func (s *setupTextFormState) down() {
	if s.Editing || s.rows() == 0 {
		return
	}
	s.Button = -1
	s.Message = ""
	s.Cursor = (s.Cursor + 1) % s.rows()
}

func (s *setupTextFormState) tab() {
	if s.Editing {
		s.Editing = false
		s.EditOriginal = ""
	}
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

func (s *setupTextFormState) activate() ([]string, bool) {
	if s.Button == 0 {
		return s.deploy()
	}
	if s.Button == 1 {
		return nil, false
	}
	if s.Cursor >= 0 && s.Cursor < len(s.Fields) {
		s.Editing = true
		s.EditOriginal = s.Fields[s.Cursor].Value
		s.Message = ""
	}
	return nil, false
}

func (s *setupTextFormState) deploy() ([]string, bool) {
	var values []string
	for _, field := range s.Fields {
		value := strings.TrimSpace(field.Value)
		if field.Required && value == "" {
			s.Message = field.Label + " is required."
			return nil, false
		}
		values = append(values, value)
	}
	return values, true
}

func (s *setupTextFormState) appendByte(b byte) {
	if b == '\r' || b == '\n' {
		s.Editing = false
		s.EditOriginal = ""
		return
	}
	if b < 32 || b > 126 {
		return
	}
	if s.Cursor < 0 || s.Cursor >= len(s.Fields) || len(s.Fields[s.Cursor].Value) >= 160 {
		return
	}
	s.Fields[s.Cursor].Value += string(b)
}

func (s *setupTextFormState) backspace() {
	if s.Cursor < 0 || s.Cursor >= len(s.Fields) {
		return
	}
	value := s.Fields[s.Cursor].Value
	if len(value) == 0 {
		return
	}
	s.Fields[s.Cursor].Value = value[:len(value)-1]
}

func renderSetupTextFormFrame(state setupTextFormState, rawMode bool) string {
	rendered := renderSetupTextFormWithStyle(state, rawMode)
	if !rawMode {
		return rendered
	}
	rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	return "\x1b[?25l\x1b[H\x1b[2J" + rendered
}

func renderSetupTextFormWithStyle(state setupTextFormState, style bool) string {
	width := setupDialogDynamicWidth(58, state.Title, "Enter edits/saves field  Tab fields/buttons", "Ctrl-D OK  Esc cancel/revert  Ctrl-C cancel")
	for _, field := range state.Fields {
		width = setupDialogDynamicWidth(width, fmt.Sprintf("> %-18s [%s]", field.Label, initDialogInputValue(field.Value, 31, false, false)))
	}
	inputWidth := maxSetupDialogInt(31, width-24)
	var lines []string
	lines = append(lines,
		setupDialogBorder(width),
		setupDialogTitleRow(width),
		setupDialogBorder(width),
		setupDialogRowWidth(state.Title, width),
		setupDialogRowWidth("", width),
	)
	for i, field := range state.Fields {
		active := state.Button < 0 && state.Cursor == i
		marker := " "
		if active {
			marker = ">"
		}
		inputStyle := setupDialogSectionStyle(style, active)
		if style && state.Editing && active {
			inputStyle += "\x1b[44;97m"
		}
		value := field.Value
		if field.Secret {
			value = strings.Repeat("*", len(value))
		}
		lines = append(lines, setupDialogRowStyledWidth(fmt.Sprintf("%s %-18s [%s]", marker, field.Label, initDialogInputValue(value, inputWidth, state.Editing && active, style)), width, inputStyle))
	}
	if state.Message != "" {
		lines = append(lines, setupDialogRowStyledWidth(state.Message, width, setupDialogANSI(style, "33")))
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
		setupDialogRowWidth("", width),
		setupDialogBorder(width),
		setupDialogRowWidth(setupDialogButton("[ OK ]", okStyle)+"    "+setupDialogButton("[ Cancel ]", cancelStyle), width),
		setupDialogRowWidth("Enter edits/saves field  Tab fields/buttons", width),
		setupDialogRowWidth("Ctrl-D OK  Esc cancel/revert  Ctrl-C cancel", width),
		setupDialogBorder(width),
	)
	return strings.Join(lines, "\n") + "\n"
}

func runSetupBrokerOutputWithRaw(reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, title, body string) (string, error) {
	for {
		fmt.Fprint(stdout, renderSetupBrokerOutputFrame(title, body, true))
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "Done.", nil
			}
			return "", err
		}
		switch b {
		case 0x03:
			return "", errors.New("setup canceled")
		case '\r', '\n', ' ', 0x04, 0x1b, 'q', 'Q':
			return "Done.", nil
		}
	}
}

func runSetupPlainCommandOutputWithRaw(reader *bufio.Reader, stdout io.Writer, title, command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		command = "No command was returned."
	}
	rendered := strings.TrimSpace(title) + "\n\n" + command + "\n\nPress any key to continue\n"
	rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	fmt.Fprint(stdout, "\x1b[?25h\x1b[H\x1b[2J"+rendered)
	b, err := reader.ReadByte()
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if b == 0x03 {
		return "", errors.New("setup canceled")
	}
	return "Done.", nil
}

func setupAcceptCommandFromOutput(output string) string {
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		if strings.Contains(strings.ToLower(line), "give this command") {
			for _, candidate := range lines[i+1:] {
				candidate = strings.TrimSpace(candidate)
				if strings.HasPrefix(candidate, "bgit ") {
					return candidate
				}
			}
		}
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "bgit ") {
			return line
		}
	}
	return strings.TrimSpace(output)
}

func renderSetupBrokerOutputFrame(title, body string, rawMode bool) string {
	rendered := renderSetupBrokerOutputWithStyle(title, body, rawMode)
	if !rawMode {
		return rendered
	}
	rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	return "\x1b[?25l\x1b[H\x1b[2J" + rendered
}

func renderSetupBrokerOutputWithStyle(title, body string, style bool) string {
	width := setupDialogDynamicWidth(72, title, "Enter/Esc returns to previous menu")
	body = strings.TrimSpace(body)
	if body == "" {
		body = "No entries."
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		width = setupDialogDynamicWidth(width, line)
	}
	var lines []string
	lines = append(lines,
		setupDialogBorder(width),
		setupDialogTitleRow(width),
		setupDialogBorder(width),
		setupDialogRowWidth(title, width),
		setupDialogRowWidth("", width),
	)
	bodyLines := setupWrapOutputLines(body, width)
	if len(bodyLines) > 18 {
		bodyLines = bodyLines[:12]
		bodyLines = append(bodyLines, "...")
	}
	for _, line := range bodyLines {
		lines = append(lines, setupDialogRowWidth(line, width))
	}
	lines = append(lines,
		setupDialogRowWidth("", width),
		setupDialogBorder(width),
		setupDialogRowWidth("[ OK ]", width),
		setupDialogRowWidth("Enter/Esc returns to previous menu", width),
		setupDialogBorder(width),
	)
	return strings.Join(lines, "\n") + "\n"
}

func setupWrapOutputLines(body string, width int) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if len(stripSetupANSI(line)) <= width {
			out = append(out, line)
			continue
		}
		out = append(out, setupWrapHard(line, width)...)
	}
	return out
}

func setupWrapHard(line string, width int) []string {
	if width <= 0 {
		return []string{line}
	}
	var out []string
	prefix := ""
	if strings.HasPrefix(line, "  ") {
		prefix = "  "
	}
	remaining := line
	for len(stripSetupANSI(remaining)) > width {
		cut := width
		if prefix != "" && len(out) > 0 {
			cut = width - len(prefix)
		}
		if cut < 8 {
			cut = width
		}
		part := remaining[:minSetupDialogInt(cut, len(remaining))]
		if len(out) > 0 && prefix != "" {
			part = prefix + part
		}
		out = append(out, part)
		remaining = remaining[minSetupDialogInt(cut, len(remaining)):]
	}
	if remaining != "" {
		if len(out) > 0 && prefix != "" {
			remaining = prefix + remaining
		}
		out = append(out, remaining)
	}
	return out
}

func runSetupBrokerDeleteConfirmWithRaw(reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, broker setupConfiguredBroker) (bool, error) {
	rawMode, restore, err := setupDialogRawMode(rawInput)
	if err != nil {
		return false, err
	}
	defer restore()
	button := 1
	for {
		fmt.Fprint(stdout, renderSetupBrokerDeleteConfirmFrame(broker, button, rawMode))
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err
		}
		switch b {
		case 0x03:
			return false, errors.New("setup canceled")
		case '\t':
			button = (button + 1) % 2
		case '\r', '\n', ' ', 0x04:
			return button == 0, nil
		case 0x1b, 'q', 'Q':
			return false, nil
		}
	}
}

func renderSetupBrokerDeleteConfirmFrame(broker setupConfiguredBroker, button int, rawMode bool) string {
	rendered := renderSetupBrokerDeleteConfirmWithStyle(broker, button, rawMode)
	if !rawMode {
		return rendered
	}
	rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	return "\x1b[?25l\x1b[H\x1b[2J" + rendered
}

func renderSetupBrokerDeleteConfirmWithStyle(broker setupConfiguredBroker, button int, style bool) string {
	deleteStyle := ""
	cancelStyle := ""
	if style && button == 0 {
		deleteStyle = "\x1b[41;97m"
	}
	if style && button == 1 {
		cancelStyle = "\x1b[44;97m"
	}
	lines := []string{
		"+------------------------------------------------------------+",
		"|                    BUCKETGIT SETUP                         |",
		"+------------------------------------------------------------+",
		setupDialogRow("Delete broker " + setupBrokerQualifiedName(broker) + "?"),
		setupDialogRow("This removes broker infrastructure for this region."),
		setupDialogRow("Repository buckets are not deleted by broker delete."),
		"|                                                            |",
		"+------------------------------------------------------------+",
		setupDialogRow(setupDialogButton("[ Delete ]", deleteStyle) + "    " + setupDialogButton("[ Cancel ]", cancelStyle)),
		setupDialogRow("Tab buttons  Enter select  Esc cancel"),
		"+------------------------------------------------------------+",
	}
	return strings.Join(lines, "\n") + "\n"
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

func maxSetupDialogInt(a, b int) int {
	if a > b {
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

func setupBreadcrumb(parts ...string) string {
	var cleaned []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			cleaned = append(cleaned, part)
		}
	}
	return strings.Join(cleaned, " > ")
}

func setupDialogDynamicWidth(base int, values ...string) int {
	width := base
	for _, value := range values {
		if n := len(stripSetupANSI(value)); n > width {
			width = n
		}
	}
	if width < 58 {
		width = 58
	}
	if width > 100 {
		width = 100
	}
	return width
}

func setupDialogTitleRow(width int) string {
	title := "BUCKETGIT SETUP"
	if len(title) >= width {
		return setupDialogRowWidth(title, width)
	}
	left := (width - len(title)) / 2
	return setupDialogRowWidth(strings.Repeat(" ", left)+title, width)
}

func setupDialogRow(text string) string {
	visible := stripSetupANSI(text)
	if len(visible) > 58 {
		text = visible[:58]
		visible = text
	}
	return "| " + text + strings.Repeat(" ", 58-len(visible)) + " |"
}

func setupDialogBorder(width int) string {
	return "+" + strings.Repeat("-", width+2) + "+"
}

func setupDialogRowWidth(text string, width int) string {
	visible := stripSetupANSI(text)
	if len(visible) > width {
		text = visible[:width]
		visible = text
	}
	return "| " + text + strings.Repeat(" ", width-len(visible)) + " |"
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

func setupDialogRowStyledWidth(text string, width int, style string) string {
	visible := stripSetupANSI(text)
	if len(visible) > width {
		text = visible[:width]
		visible = text
	}
	if style != "" {
		text = style + text + "\x1b[0m"
	}
	return "| " + text + strings.Repeat(" ", width-len(visible)) + " |"
}

func setupDialogWrappedActionRows(marker, label, help string, labelWidth, width int, style string) []string {
	prefixWidth := 2 + labelWidth + 1
	helpWidth := width - prefixWidth
	if helpWidth < 12 {
		helpWidth = 12
	}
	parts := setupWrapWords(help, helpWidth)
	if len(parts) == 0 {
		parts = []string{""}
	}
	rows := []string{setupDialogRowStyledWidth(fmt.Sprintf("%s %-*s %s", marker, labelWidth, label, parts[0]), width, style)}
	for _, part := range parts[1:] {
		rows = append(rows, setupDialogRowStyledWidth(fmt.Sprintf("  %-*s %s", labelWidth, "", part), width, style))
	}
	return rows
}

func setupWrapWords(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	line := ""
	for _, word := range words {
		if line == "" {
			line = word
			continue
		}
		if len(line)+1+len(word) > width {
			lines = append(lines, line)
			line = word
			continue
		}
		line += " " + word
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func setupProviderLabel(provider string) string {
	if provider == "s3" {
		return "aws"
	}
	if provider == "local" {
		return "local"
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

func brokerUpsertOwners(brokerURL, bootstrapToken string, publicKeys []string) error {
	headers := map[string]string{}
	if strings.TrimSpace(bootstrapToken) != "" {
		headers["X-Bgit-Bootstrap-Token"] = strings.TrimSpace(bootstrapToken)
	}
	return brokerPostJSONContextWithHeaders(context.Background(), brokerURL, "/owners/upsert", brokerOwnerRequest{User: "owner", Role: "owner", PublicKeys: publicKeys}, nil, headers)
}

func brokerEnsureCoreTeam(brokerURL string) error {
	err := brokerPost(brokerURL, "/teams/create", brokerRepoAdminRequest{TeamID: coreTeamID, Name: coreTeamName}, nil)
	if err != nil && !strings.Contains(err.Error(), "team already exists") {
		return err
	}
	return nil
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

func upsertGlobalLocalProfile(cfg globalConfig, profile globalLocalProfile) globalConfig {
	for i, existing := range cfg.LocalProfiles {
		if existing.Name == profile.Name {
			if strings.TrimSpace(profile.Root) == "" {
				profile.Root = existing.Root
			}
			profile.Autostart = profile.Autostart || existing.Autostart
			profile.Regions = mergeGlobalProfileRegions(existing.Regions, profile.Regions)
			cfg.LocalProfiles[i] = profile
			return cfg
		}
	}
	cfg.LocalProfiles = append(cfg.LocalProfiles, profile)
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
