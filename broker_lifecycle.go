package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type brokerDeleteOptions struct {
	provider          string
	profile           string
	region            string
	configPath        string
	deleteData        bool
	yes               bool
	firestoreDatabase string
}

func brokerCommand(ctx context.Context, base config, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bgit broker delete [args]")
	}
	switch args[0] {
	case "delete", "decommission":
		return brokerDeleteCommand(ctx, base, args[1:], stdin, stdout)
	default:
		return fmt.Errorf("unknown broker command %q", args[0])
	}
}

type managedLocalBroker struct {
	URL            string
	Root           string
	BootstrapToken string
}

func ensureManagedLocalBroker(ctx context.Context, profile globalLocalProfile, region globalProfileRegion) (*managedLocalBroker, error) {
	_ = ctx
	root := expandHome(firstNonEmpty(profile.Root, "~/.bgit/local-broker"))
	token, err := randomBrokerSecret()
	if err != nil {
		return nil, err
	}
	return &managedLocalBroker{
		URL:            localBrokerURL(firstNonEmpty(profile.Name, "default"), firstNonEmpty(region.Name, "default")),
		Root:           root,
		BootstrapToken: token,
	}, nil
}

func (b *managedLocalBroker) Close() {
	_ = b
}

func localBrokerURL(profile, region string) string {
	return "local://" + firstNonEmpty(strings.TrimSpace(profile), "default") + "/" + firstNonEmpty(strings.TrimSpace(region), "default")
}

func brokerDeleteCommand(ctx context.Context, base config, args []string, stdin io.Reader, stdout io.Writer) error {
	opts, err := parseBrokerDeleteArgs(args)
	if err != nil {
		return err
	}
	if !opts.yes {
		fmt.Fprint(stdout, "Delete bgit broker infrastructure? [y/N] ")
		answer, _ := bufioReadLine(stdin)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
			return errors.New("broker delete cancelled")
		}
	}
	provider := firstNonEmpty(opts.provider, normalizeSetupProvider(firstNonEmpty(base.provider, "gcp")))
	if provider == "" {
		return errors.New("broker delete requires --provider gcp|aws")
	}
	cfg := base
	cfg.provider = mapSetupProviderToConfig(provider)
	if opts.profile != "" {
		cfg.gcloudConfiguration = opts.profile
		cfg.gcloudConfigurationExplicit = true
	}
	switch provider {
	case "gcs":
		if err := deleteGCPBroker(ctx, cfg, opts, stdout); err != nil {
			return err
		}
	case "s3":
		if err := deleteAWSBroker(ctx, cfg, opts, stdout); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported broker provider %q", provider)
	}
	if err := removeDeletedBrokerFromGlobalConfig(opts, provider, cfg.gcloudConfiguration); err != nil {
		return err
	}
	return nil
}

func parseBrokerDeleteArgs(args []string) (brokerDeleteOptions, error) {
	var opts brokerDeleteOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--provider":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, err
			}
			opts.provider = normalizeSetupProvider(value)
			if opts.provider == "" {
				return opts, fmt.Errorf("unsupported broker provider %q", value)
			}
		case "--profile":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, err
			}
			opts.profile = value
		case "--region":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, err
			}
			opts.region = value
		case "--config":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, err
			}
			opts.configPath = expandHome(value)
		case "--firestore-database":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, err
			}
			opts.firestoreDatabase = value
		case "--data":
			opts.deleteData = true
		case "--yes", "-y":
			opts.yes = true
		default:
			return opts, fmt.Errorf("unsupported broker delete option %s", arg)
		}
	}
	return opts, nil
}

func deleteGCPBroker(ctx context.Context, cfg config, opts brokerDeleteOptions, stdout io.Writer) error {
	region := firstNonEmpty(strings.TrimSpace(opts.region), defaultGCPRegion(cfg))
	fmt.Fprintf(stdout, "deleting GCP bgit broker function in %s\n", region)
	cmd := gcloudCommand(cfg.gcloudConfiguration, "functions", "delete", "bgit-broker", "--gen2", "--region", region, "--quiet")
	if out, err := cmd.CombinedOutput(); err != nil {
		if !brokerDeleteMissing(string(out), err) {
			return fmt.Errorf("delete GCP bgit broker function: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		fmt.Fprintf(stdout, "GCP function bgit-broker was already absent\n")
	}
	runDelete := gcloudCommand(cfg.gcloudConfiguration, "run", "services", "delete", "bgit-broker", "--region", region, "--quiet")
	if out, err := runDelete.CombinedOutput(); err != nil && !brokerDeleteMissing(string(out), err) {
		return fmt.Errorf("delete GCP bgit broker Cloud Run service: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	if opts.deleteData {
		database := firstNonEmpty(strings.TrimSpace(opts.firestoreDatabase), os.Getenv("BGIT_FIRESTORE_DATABASE"), "bgit")
		fmt.Fprintf(stdout, "deleting GCP Firestore database %s\n", database)
		deleteDB := gcloudCommand(cfg.gcloudConfiguration, "firestore", "databases", "delete", "--database="+database, "--quiet")
		if out, err := deleteDB.CombinedOutput(); err != nil && !brokerDeleteMissing(string(out), err) {
			return fmt.Errorf("delete GCP Firestore database %s: %w\n%s", database, err, strings.TrimSpace(string(out)))
		}
	}
	fmt.Fprintf(stdout, "deleted GCP bgit broker\n")
	return nil
}

func deleteAWSBroker(ctx context.Context, cfg config, opts brokerDeleteOptions, stdout io.Writer) error {
	region := firstNonEmpty(strings.TrimSpace(opts.region), defaultAWSRegion())
	profile := strings.TrimSpace(firstNonEmpty(opts.profile, cfg.gcloudConfiguration))
	fmt.Fprintf(stdout, "deleting AWS CloudFormation stack bgit-broker in %s\n", region)
	args := []string{"cloudformation", "delete-stack", "--stack-name", "bgit-broker", "--region", region}
	if out, err := awsCommand(ctx, profile, args...).CombinedOutput(); err != nil {
		if !brokerDeleteMissing(string(out), err) {
			return fmt.Errorf("delete AWS bgit broker stack: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		fmt.Fprintf(stdout, "AWS stack bgit-broker was already absent\n")
		return nil
	}
	waitArgs := []string{"cloudformation", "wait", "stack-delete-complete", "--stack-name", "bgit-broker", "--region", region}
	if out, err := awsCommand(ctx, profile, waitArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("wait for AWS bgit broker stack deletion: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(stdout, "deleted AWS bgit broker\n")
	return nil
}

func brokerDeleteMissing(out string, err error) bool {
	message := strings.ToLower(out + "\n" + err.Error())
	return strings.Contains(message, "not found") ||
		strings.Contains(message, "not exist") ||
		strings.Contains(message, "does not exist") ||
		strings.Contains(message, "could not be found") ||
		strings.Contains(message, "resource not found") ||
		strings.Contains(message, "stack with id bgit-broker does not exist")
}

func removeDeletedBrokerFromGlobalConfig(opts brokerDeleteOptions, provider, profile string) error {
	path := opts.configPath
	var err error
	if path == "" {
		path, err = defaultGlobalConfigPath()
		if err != nil {
			return err
		}
	}
	global, err := readGlobalConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	switch provider {
	case "gcs":
		for i := range global.GCPProfiles {
			if profile == "" || global.GCPProfiles[i].Name == profile {
				global.GCPProfiles[i].Regions = clearGlobalProfileRegion(global.GCPProfiles[i].Regions, opts.region)
			}
		}
	case "s3":
		for i := range global.AWSProfiles {
			if profile == "" || global.AWSProfiles[i].Name == profile {
				global.AWSProfiles[i].Regions = clearGlobalProfileRegion(global.AWSProfiles[i].Regions, opts.region)
			}
		}
	}
	return writeGlobalConfig(path, global)
}

func clearGlobalProfileRegion(regions []globalProfileRegion, region string) []globalProfileRegion {
	region = strings.TrimSpace(region)
	if region == "" {
		return nil
	}
	var out []globalProfileRegion
	for _, entry := range regions {
		if entry.Name != region {
			out = append(out, entry)
		}
	}
	return out
}

func mapSetupProviderToConfig(provider string) string {
	switch provider {
	case "gcs":
		return "gcs"
	case "s3":
		return "s3"
	default:
		return provider
	}
}

func bufioReadLine(stdin io.Reader) (string, error) {
	reader := bufio.NewReader(stdin)
	return reader.ReadString('\n')
}
