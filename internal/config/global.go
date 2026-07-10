package config

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

func firstNonEmptyValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func expandHomePath(value string) string {
	if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			if value == "~" {
				return home
			}
			return filepath.Join(home, value[2:])
		}
	}
	return value
}

const GlobalVersion = 1

type Global struct {
	Version       int
	Identity      Identity
	GCPProfiles   []GCPProfile
	AWSProfiles   []AWSProfile
	LocalProfiles []LocalProfile
	Repos         []Repo
}

type Identity struct {
	Name  string
	Email string
}

type GCPProfile struct {
	Name           string
	ProjectID      string
	Account        string
	Region         string
	ServiceAccount string
	BrokerURL      string
	BrokerVersion  string
	LastSetupAt    string
	Regions        []ProfileRegion
}

type AWSProfile struct {
	Name          string
	AccountID     string
	ARN           string
	Region        string
	BrokerURL     string
	BrokerVersion string
	LastSetupAt   string
	Regions       []ProfileRegion
}

type LocalProfile struct {
	Name          string
	Root          string
	Autostart     bool
	Region        string
	BrokerURL     string
	BrokerVersion string
	LastSetupAt   string
	Regions       []ProfileRegion
}

type ProfileRegion struct {
	Name          string
	BrokerURL     string
	BrokerVersion string
	LastSetupAt   string
}

type Repo struct {
	Name      string
	Profile   string
	BrokerURL string
}

func DefaultGlobalPath() (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.yaml"), nil
}

func DefaultLocalBrokerRoot() (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "local-broker"), nil
}

func DefaultCacheDir() (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "cache"), nil
}

func HomeDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("BGIT_HOME")); value != "" {
		return expandHomePath(value), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bgit"), nil
}

func ReadGlobal(path string) (Global, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Global{}, err
	}
	cfg, err := ParseGlobalYAML(data)
	if err != nil {
		return cfg, err
	}
	NormalizeProfileRegions(&cfg)
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

func ParseGlobalYAML(data []byte) (Global, error) {
	var raw globalConfigYAML
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return Global{}, err
	}
	return globalConfigFromYAML(raw), nil
}

func globalConfigFromYAML(raw globalConfigYAML) Global {
	cfg := Global{
		Version: raw.Version,
		Identity: Identity{
			Name:  raw.Identity.Name,
			Email: raw.Identity.Email,
		},
	}
	if cfg.Version == 0 {
		cfg.Version = GlobalVersion
	}
	for name, profile := range raw.GCP.Profiles {
		next := GCPProfile{
			Name:           name,
			ProjectID:      profile.ProjectID,
			Account:        profile.Account,
			ServiceAccount: profile.ServiceAccount,
		}
		for regionName, region := range profile.Regions {
			next.Regions = append(next.Regions, ProfileRegion{
				Name:          regionName,
				BrokerURL:     region.BrokerURL,
				BrokerVersion: region.BrokerVersion,
				LastSetupAt:   region.LastSetupAt,
			})
		}
		SortProfileRegions(next.Regions)
		cfg.GCPProfiles = append(cfg.GCPProfiles, next)
	}
	for name, profile := range raw.AWS.Profiles {
		next := AWSProfile{
			Name:      name,
			AccountID: profile.AccountID,
			ARN:       profile.ARN,
		}
		for regionName, region := range profile.Regions {
			next.Regions = append(next.Regions, ProfileRegion{
				Name:          regionName,
				BrokerURL:     region.BrokerURL,
				BrokerVersion: region.BrokerVersion,
				LastSetupAt:   region.LastSetupAt,
			})
		}
		SortProfileRegions(next.Regions)
		cfg.AWSProfiles = append(cfg.AWSProfiles, next)
	}
	for name, profile := range raw.Local.Profiles {
		next := LocalProfile{Name: name, Root: profile.Root, Autostart: profile.Autostart}
		for regionName, region := range profile.Regions {
			next.Regions = append(next.Regions, ProfileRegion{
				Name:          regionName,
				BrokerURL:     region.BrokerURL,
				BrokerVersion: region.BrokerVersion,
				LastSetupAt:   region.LastSetupAt,
			})
		}
		SortProfileRegions(next.Regions)
		cfg.LocalProfiles = append(cfg.LocalProfiles, next)
	}
	for name, repo := range raw.Repos {
		cfg.Repos = append(cfg.Repos, Repo{Name: name, Profile: repo.Profile, BrokerURL: repo.BrokerURL})
	}
	sortGlobalConfig(&cfg)
	return cfg
}

func globalConfigToYAML(cfg Global) globalConfigYAML {
	NormalizeProfileRegions(&cfg)
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
		out.Version = GlobalVersion
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

func sortGlobalConfig(cfg *Global) {
	sort.Slice(cfg.GCPProfiles, func(i, j int) bool {
		return cfg.GCPProfiles[i].Name < cfg.GCPProfiles[j].Name
	})
	for i := range cfg.GCPProfiles {
		SortProfileRegions(cfg.GCPProfiles[i].Regions)
	}
	sort.Slice(cfg.AWSProfiles, func(i, j int) bool {
		return cfg.AWSProfiles[i].Name < cfg.AWSProfiles[j].Name
	})
	for i := range cfg.AWSProfiles {
		SortProfileRegions(cfg.AWSProfiles[i].Regions)
	}
	sort.Slice(cfg.LocalProfiles, func(i, j int) bool {
		return cfg.LocalProfiles[i].Name < cfg.LocalProfiles[j].Name
	})
	for i := range cfg.LocalProfiles {
		SortProfileRegions(cfg.LocalProfiles[i].Regions)
	}
	sort.Slice(cfg.Repos, func(i, j int) bool {
		return cfg.Repos[i].Name < cfg.Repos[j].Name
	})
}

func SortProfileRegions(regions []ProfileRegion) {
	sort.Slice(regions, func(i, j int) bool {
		return regions[i].Name < regions[j].Name
	})
}

func WriteGlobal(path string, cfg Global) error {
	if cfg.Version == 0 {
		cfg.Version = GlobalVersion
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

func NormalizeProfileRegions(cfg *Global) {
	for i := range cfg.GCPProfiles {
		profile := &cfg.GCPProfiles[i]
		if len(profile.Regions) == 0 && strings.TrimSpace(profile.BrokerURL) != "" {
			profile.Regions = append(profile.Regions, ProfileRegion{
				Name:          firstNonEmptyValue(profile.Region, "us-central1"),
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
			profile.Regions = append(profile.Regions, ProfileRegion{
				Name:          firstNonEmptyValue(profile.Region, "us-east-1"),
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
			profile.Regions = append(profile.Regions, ProfileRegion{
				Name:          firstNonEmptyValue(profile.Region, "default"),
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
