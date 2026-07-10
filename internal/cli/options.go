package cli

import (
	"fmt"
	"net/url"
	"strings"
)

type GlobalOptions struct {
	Provider         string
	Bucket           string
	Prefix           string
	Branch           string
	Region           string
	Auth             string
	Profile          string
	Identity         string
	AuthExplicit     bool
	AuthFlagExplicit bool
	ProfileExplicit  bool
	VersionRequested bool
}

type GlobalOptionDefaults struct {
	Branch          string
	Auth            string
	Profile         string
	AuthExplicit    bool
	ProfileExplicit bool
}

func ParseGlobalOptions(arguments []string, defaults GlobalOptionDefaults) (GlobalOptions, []string, error) {
	options := GlobalOptions{
		Branch:          defaults.Branch,
		Auth:            defaults.Auth,
		Profile:         defaults.Profile,
		AuthExplicit:    defaults.AuthExplicit,
		ProfileExplicit: defaults.ProfileExplicit,
	}
	var rest []string
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		name, value, hasValue := strings.Cut(argument, "=")
		takeValue := func() (string, error) {
			if hasValue {
				return value, nil
			}
			index++
			if index >= len(arguments) {
				return "", fmt.Errorf("%s requires a value", name)
			}
			return arguments[index], nil
		}
		switch name {
		case "--bucket":
			value, err := takeValue()
			if err != nil {
				return options, nil, err
			}
			applyBucket(&options, value)
		case "--prefix":
			value, err := takeValue()
			if err != nil {
				return options, nil, err
			}
			options.Prefix = value
		case "--branch":
			value, err := takeValue()
			if err != nil {
				return options, nil, err
			}
			options.Branch = value
		case "--region":
			value, err := takeValue()
			if err != nil {
				return options, nil, err
			}
			options.Region = value
		case "--auth":
			value, err := takeValue()
			if err != nil {
				return options, nil, err
			}
			options.Auth, options.AuthExplicit, options.AuthFlagExplicit = value, true, true
		case "--configuration", "--profile":
			value, err := takeValue()
			if err != nil {
				return options, nil, err
			}
			options.Profile, options.ProfileExplicit = value, true
		case "--identity":
			value, err := takeValue()
			if err != nil {
				return options, nil, err
			}
			options.Identity = value
		case "--version", "-v":
			options.VersionRequested = true
		default:
			rest = append(rest, argument)
		}
	}
	options.Prefix = strings.Trim(options.Prefix, "/")
	options.Auth = strings.ToLower(strings.TrimSpace(options.Auth))
	if options.Auth == "" {
		options.Auth = "gcloud"
	}
	if options.Auth != "gcloud" && options.Auth != "adc" {
		return options, nil, fmt.Errorf("unsupported auth mode %q", options.Auth)
	}
	return options, rest, nil
}

func applyBucket(options *GlobalOptions, raw string) {
	provider, bucket, prefix := splitBucketTarget(raw)
	if provider != "" {
		options.Provider = provider
	}
	if bucket != "" {
		options.Bucket = bucket
	} else {
		options.Bucket = raw
	}
	if prefix != "" && options.Prefix == "" {
		options.Prefix = prefix
	}
}

func splitBucketTarget(raw string) (string, string, string) {
	bucket := strings.TrimSpace(raw)
	if bucket == "" {
		return "", "", ""
	}
	if strings.HasPrefix(bucket, "gs://") || strings.HasPrefix(bucket, "gcs://") || strings.HasPrefix(bucket, "s3://") {
		parsed, err := url.Parse(bucket)
		if err == nil && parsed.Host != "" {
			provider := "gcs"
			if parsed.Scheme == "s3" {
				provider = "s3"
			}
			return provider, parsed.Host, strings.Trim(parsed.Path, "/")
		}
	}
	bucket = strings.Trim(bucket, "/")
	if slash := strings.Index(bucket, "/"); slash >= 0 {
		return "", bucket[:slash], strings.Trim(bucket[slash+1:], "/")
	}
	return "", bucket, ""
}
