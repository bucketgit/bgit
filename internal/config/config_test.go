package config

import "testing"

func TestParseRepositoryURI(t *testing.T) {
	parsed, name, err := ParseRepositoryURI("gs://bucket/team/demo.git")
	if err != nil || parsed.Provider != "gcs" || parsed.Bucket != "bucket" || parsed.Prefix != "team/demo.git" || name != "demo.git" {
		t.Fatalf("parsed=%#v name=%q err=%v", parsed, name, err)
	}
	if _, _, err := ParseRepositoryURI("file://demo.git"); err == nil {
		t.Fatal("file URI accepted as cloud bucket URI")
	}
}

func TestLocalSelection(t *testing.T) {
	for input, want := range map[string][2]string{"": {"default", "default"}, "local:work/eu": {"work", "eu"}, "work.eu": {"work", "eu"}} {
		profile, region := LocalSelection(input)
		if [2]string{profile, region} != want {
			t.Fatalf("LocalSelection(%q)=(%q,%q), want %v", input, profile, region, want)
		}
	}
}

func TestNormalizeLogicalRepository(t *testing.T) {
	if got, err := NormalizeLogicalRepository("demo"); err != nil || got != "demo.git" {
		t.Fatalf("normalize=%q,%v", got, err)
	}
	if _, err := NormalizeLogicalRepository("team/demo"); err == nil {
		t.Fatal("nested logical name accepted")
	}
}
