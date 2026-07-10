package cli

import "testing"

func TestParseGlobalOptions(t *testing.T) {
	options, rest, err := ParseGlobalOptions([]string{"--bucket", "s3://bucket/repo.git", "--profile=work", "--auth", "adc", "clone"}, GlobalOptionDefaults{Branch: "main", Auth: "gcloud"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Provider != "s3" || options.Bucket != "bucket" || options.Prefix != "repo.git" || options.Profile != "work" || options.Auth != "adc" || !options.AuthFlagExplicit || len(rest) != 1 || rest[0] != "clone" {
		t.Fatalf("options=%#v rest=%q", options, rest)
	}
}

func TestParseGlobalOptionsTracksExplicitDefaults(t *testing.T) {
	implicit, _, err := ParseGlobalOptions(nil, GlobalOptionDefaults{Auth: "gcloud", Profile: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if implicit.AuthExplicit || implicit.ProfileExplicit {
		t.Fatalf("implicit defaults marked explicit: %#v", implicit)
	}

	explicit, _, err := ParseGlobalOptions(nil, GlobalOptionDefaults{
		Auth:            "adc",
		Profile:         "work",
		AuthExplicit:    true,
		ProfileExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !explicit.AuthExplicit || !explicit.ProfileExplicit {
		t.Fatalf("explicit defaults not preserved: %#v", explicit)
	}
}

func TestParseGlobalOptionsRejectsInvalidAuthAndMissingValues(t *testing.T) {
	if _, _, err := ParseGlobalOptions([]string{"--auth", "invalid"}, GlobalOptionDefaults{}); err == nil {
		t.Fatal("invalid auth accepted")
	}
	if _, _, err := ParseGlobalOptions([]string{"--identity"}, GlobalOptionDefaults{}); err == nil {
		t.Fatal("missing identity accepted")
	}
}
