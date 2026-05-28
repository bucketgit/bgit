package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const globalConfigVersion = 1

type globalConfig struct {
	Version       int
	Identity      globalIdentityConfig
	GCPProfiles   []globalGCPProfile
	AWSProfiles   []globalAWSProfile
	LocalProfiles []globalLocalProfile
	Repos         []globalRepoConfig
}

type globalIdentityConfig struct {
	Name  string
	Email string
}

type globalGCPProfile struct {
	Name           string
	ProjectID      string
	Account        string
	Region         string
	ServiceAccount string
	BrokerURL      string
	BrokerVersion  string
	LastSetupAt    string
	Regions        []globalProfileRegion
}

type globalAWSProfile struct {
	Name          string
	AccountID     string
	ARN           string
	Region        string
	BrokerURL     string
	BrokerVersion string
	LastSetupAt   string
	Regions       []globalProfileRegion
}

type globalLocalProfile struct {
	Name          string
	Root          string
	Autostart     bool
	Region        string
	BrokerURL     string
	BrokerVersion string
	LastSetupAt   string
	Regions       []globalProfileRegion
}

type globalProfileRegion struct {
	Name          string
	BrokerURL     string
	BrokerVersion string
	LastSetupAt   string
}

type globalRepoConfig struct {
	Name      string
	Profile   string
	BrokerURL string
}

func defaultGlobalConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bgit", "config.yaml"), nil
}

func readGlobalConfig(path string) (globalConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return globalConfig{}, err
	}
	cfg, err := parseGlobalConfigYAML(data)
	if err != nil {
		return cfg, err
	}
	normalizeGlobalConfigProfileRegions(&cfg)
	return cfg, nil
}

type globalConfigYAML struct {
	Version  int                       `yaml:"version"`
	Identity globalIdentityYAML        `yaml:"identity,omitempty"`
	GCP      globalGCPConfigYAML       `yaml:"gcp,omitempty"`
	AWS      globalAWSConfigYAML       `yaml:"aws,omitempty"`
	Local    globalLocalConfigYAML     `yaml:"local,omitempty"`
	Repos    map[string]globalRepoYAML `yaml:"repos,omitempty"`
}

type globalIdentityYAML struct {
	Name  string `yaml:"name,omitempty"`
	Email string `yaml:"email,omitempty"`
}

type globalGCPConfigYAML struct {
	Profiles map[string]globalGCPProfileYAML `yaml:"profiles,omitempty"`
}

type globalAWSConfigYAML struct {
	Profiles map[string]globalAWSProfileYAML `yaml:"profiles,omitempty"`
}

type globalLocalConfigYAML struct {
	Profiles map[string]globalLocalProfileYAML `yaml:"profiles,omitempty"`
}

type globalGCPProfileYAML struct {
	ProjectID      string                             `yaml:"project_id,omitempty"`
	Account        string                             `yaml:"account,omitempty"`
	ServiceAccount string                             `yaml:"service_account,omitempty"`
	Regions        map[string]globalProfileRegionYAML `yaml:"regions,omitempty"`
}

type globalAWSProfileYAML struct {
	AccountID string                             `yaml:"account_id,omitempty"`
	ARN       string                             `yaml:"arn,omitempty"`
	Regions   map[string]globalProfileRegionYAML `yaml:"regions,omitempty"`
}

type globalLocalProfileYAML struct {
	Root      string                             `yaml:"root,omitempty"`
	Autostart bool                               `yaml:"autostart,omitempty"`
	Regions   map[string]globalProfileRegionYAML `yaml:"regions,omitempty"`
}

type globalProfileRegionYAML struct {
	BrokerURL     string `yaml:"broker_url,omitempty"`
	BrokerVersion string `yaml:"broker_version,omitempty"`
	LastSetupAt   string `yaml:"last_setup_at,omitempty"`
}

type globalRepoYAML struct {
	Profile   string `yaml:"profile,omitempty"`
	BrokerURL string `yaml:"broker_url,omitempty"`
}

func parseGlobalConfigYAML(data []byte) (globalConfig, error) {
	var raw globalConfigYAML
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return globalConfig{}, err
	}
	return globalConfigFromYAML(raw), nil
}

func globalConfigFromYAML(raw globalConfigYAML) globalConfig {
	cfg := globalConfig{
		Version: raw.Version,
		Identity: globalIdentityConfig{
			Name:  raw.Identity.Name,
			Email: raw.Identity.Email,
		},
	}
	if cfg.Version == 0 {
		cfg.Version = globalConfigVersion
	}
	for name, profile := range raw.GCP.Profiles {
		next := globalGCPProfile{
			Name:           name,
			ProjectID:      profile.ProjectID,
			Account:        profile.Account,
			ServiceAccount: profile.ServiceAccount,
		}
		for regionName, region := range profile.Regions {
			next.Regions = append(next.Regions, globalProfileRegion{
				Name:          regionName,
				BrokerURL:     region.BrokerURL,
				BrokerVersion: region.BrokerVersion,
				LastSetupAt:   region.LastSetupAt,
			})
		}
		sortGlobalProfileRegions(next.Regions)
		cfg.GCPProfiles = append(cfg.GCPProfiles, next)
	}
	for name, profile := range raw.AWS.Profiles {
		next := globalAWSProfile{
			Name:      name,
			AccountID: profile.AccountID,
			ARN:       profile.ARN,
		}
		for regionName, region := range profile.Regions {
			next.Regions = append(next.Regions, globalProfileRegion{
				Name:          regionName,
				BrokerURL:     region.BrokerURL,
				BrokerVersion: region.BrokerVersion,
				LastSetupAt:   region.LastSetupAt,
			})
		}
		sortGlobalProfileRegions(next.Regions)
		cfg.AWSProfiles = append(cfg.AWSProfiles, next)
	}
	for name, profile := range raw.Local.Profiles {
		next := globalLocalProfile{Name: name, Root: profile.Root, Autostart: profile.Autostart}
		for regionName, region := range profile.Regions {
			next.Regions = append(next.Regions, globalProfileRegion{
				Name:          regionName,
				BrokerURL:     region.BrokerURL,
				BrokerVersion: region.BrokerVersion,
				LastSetupAt:   region.LastSetupAt,
			})
		}
		sortGlobalProfileRegions(next.Regions)
		cfg.LocalProfiles = append(cfg.LocalProfiles, next)
	}
	for name, repo := range raw.Repos {
		cfg.Repos = append(cfg.Repos, globalRepoConfig{Name: name, Profile: repo.Profile, BrokerURL: repo.BrokerURL})
	}
	sortGlobalConfig(&cfg)
	return cfg
}

func globalConfigToYAML(cfg globalConfig) globalConfigYAML {
	normalizeGlobalConfigProfileRegions(&cfg)
	sortGlobalConfig(&cfg)
	out := globalConfigYAML{
		Version: cfg.Version,
		Identity: globalIdentityYAML{
			Name:  cfg.Identity.Name,
			Email: cfg.Identity.Email,
		},
		GCP:   globalGCPConfigYAML{Profiles: map[string]globalGCPProfileYAML{}},
		AWS:   globalAWSConfigYAML{Profiles: map[string]globalAWSProfileYAML{}},
		Local: globalLocalConfigYAML{Profiles: map[string]globalLocalProfileYAML{}},
		Repos: map[string]globalRepoYAML{},
	}
	if out.Version == 0 {
		out.Version = globalConfigVersion
	}
	for _, profile := range cfg.GCPProfiles {
		next := globalGCPProfileYAML{
			ProjectID:      profile.ProjectID,
			Account:        profile.Account,
			ServiceAccount: profile.ServiceAccount,
			Regions:        map[string]globalProfileRegionYAML{},
		}
		for _, region := range profile.Regions {
			next.Regions[region.Name] = globalProfileRegionYAML{
				BrokerURL:     region.BrokerURL,
				BrokerVersion: region.BrokerVersion,
				LastSetupAt:   region.LastSetupAt,
			}
		}
		out.GCP.Profiles[profile.Name] = next
	}
	for _, profile := range cfg.AWSProfiles {
		next := globalAWSProfileYAML{
			AccountID: profile.AccountID,
			ARN:       profile.ARN,
			Regions:   map[string]globalProfileRegionYAML{},
		}
		for _, region := range profile.Regions {
			next.Regions[region.Name] = globalProfileRegionYAML{
				BrokerURL:     region.BrokerURL,
				BrokerVersion: region.BrokerVersion,
				LastSetupAt:   region.LastSetupAt,
			}
		}
		out.AWS.Profiles[profile.Name] = next
	}
	for _, profile := range cfg.LocalProfiles {
		next := globalLocalProfileYAML{
			Root:      profile.Root,
			Autostart: profile.Autostart,
			Regions:   map[string]globalProfileRegionYAML{},
		}
		for _, region := range profile.Regions {
			next.Regions[region.Name] = globalProfileRegionYAML{
				BrokerURL:     region.BrokerURL,
				BrokerVersion: region.BrokerVersion,
				LastSetupAt:   region.LastSetupAt,
			}
		}
		out.Local.Profiles[profile.Name] = next
	}
	for _, repo := range cfg.Repos {
		out.Repos[repo.Name] = globalRepoYAML{Profile: repo.Profile, BrokerURL: repo.BrokerURL}
	}
	return out
}

func sortGlobalConfig(cfg *globalConfig) {
	sort.Slice(cfg.GCPProfiles, func(i, j int) bool {
		return cfg.GCPProfiles[i].Name < cfg.GCPProfiles[j].Name
	})
	for i := range cfg.GCPProfiles {
		sortGlobalProfileRegions(cfg.GCPProfiles[i].Regions)
	}
	sort.Slice(cfg.AWSProfiles, func(i, j int) bool {
		return cfg.AWSProfiles[i].Name < cfg.AWSProfiles[j].Name
	})
	for i := range cfg.AWSProfiles {
		sortGlobalProfileRegions(cfg.AWSProfiles[i].Regions)
	}
	sort.Slice(cfg.LocalProfiles, func(i, j int) bool {
		return cfg.LocalProfiles[i].Name < cfg.LocalProfiles[j].Name
	})
	for i := range cfg.LocalProfiles {
		sortGlobalProfileRegions(cfg.LocalProfiles[i].Regions)
	}
	sort.Slice(cfg.Repos, func(i, j int) bool {
		return cfg.Repos[i].Name < cfg.Repos[j].Name
	})
}

func sortGlobalProfileRegions(regions []globalProfileRegion) {
	sort.Slice(regions, func(i, j int) bool {
		return regions[i].Name < regions[j].Name
	})
}

func writeGlobalConfig(path string, cfg globalConfig) error {
	if cfg.Version == 0 {
		cfg.Version = globalConfigVersion
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(globalConfigToYAML(cfg))
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func normalizeGlobalConfigProfileRegions(cfg *globalConfig) {
	for i := range cfg.GCPProfiles {
		profile := &cfg.GCPProfiles[i]
		if len(profile.Regions) == 0 && strings.TrimSpace(profile.BrokerURL) != "" {
			profile.Regions = append(profile.Regions, globalProfileRegion{
				Name:          firstNonEmpty(profile.Region, "us-central1"),
				BrokerURL:     profile.BrokerURL,
				BrokerVersion: profile.BrokerVersion,
				LastSetupAt:   profile.LastSetupAt,
			})
		}
		profile.Region = ""
		profile.BrokerURL = ""
		profile.BrokerVersion = ""
		profile.LastSetupAt = ""
	}
	for i := range cfg.AWSProfiles {
		profile := &cfg.AWSProfiles[i]
		if len(profile.Regions) == 0 && strings.TrimSpace(profile.BrokerURL) != "" {
			profile.Regions = append(profile.Regions, globalProfileRegion{
				Name:          firstNonEmpty(profile.Region, "us-east-1"),
				BrokerURL:     profile.BrokerURL,
				BrokerVersion: profile.BrokerVersion,
				LastSetupAt:   profile.LastSetupAt,
			})
		}
		profile.Region = ""
		profile.BrokerURL = ""
		profile.BrokerVersion = ""
		profile.LastSetupAt = ""
	}
	for i := range cfg.LocalProfiles {
		profile := &cfg.LocalProfiles[i]
		if len(profile.Regions) == 0 && strings.TrimSpace(profile.BrokerURL) != "" {
			profile.Regions = append(profile.Regions, globalProfileRegion{
				Name:          firstNonEmpty(profile.Region, "default"),
				BrokerURL:     profile.BrokerURL,
				BrokerVersion: profile.BrokerVersion,
				LastSetupAt:   profile.LastSetupAt,
			})
		}
		profile.Region = ""
		profile.BrokerURL = ""
		profile.BrokerVersion = ""
		profile.LastSetupAt = ""
	}
}
