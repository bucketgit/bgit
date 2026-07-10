package local

import "testing"

func TestParseTarget(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		kind   TargetKind
		scheme string
		bucket string
		prefix string
	}{
		{name: "logical", raw: "demo", kind: TargetLogicalAlias},
		{name: "gcs shorthand", raw: "bgit::gs://demo", kind: TargetStorageShorthand, scheme: "gs"},
		{name: "s3 shorthand", raw: "s3://demo.git", kind: TargetStorageShorthand, scheme: "s3"},
		{name: "file shorthand", raw: "file://demo", kind: TargetStorageShorthand, scheme: "file"},
		{name: "gcs explicit", raw: "gs://physical/repos/demo.git", kind: TargetStorageExplicit, scheme: "gs", bucket: "physical", prefix: "repos/demo.git"},
		{name: "s3 explicit", raw: "s3://physical/demo.git", kind: TargetStorageExplicit, scheme: "s3", bucket: "physical", prefix: "demo.git"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseTarget(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != test.kind || got.Scheme != test.scheme || got.Bucket != test.bucket || got.Prefix != test.prefix || got.Logical != "demo.git" {
				t.Fatalf("target = %#v", got)
			}
		})
	}
}

func TestParseTargetRejectsInvalidAddresses(t *testing.T) {
	for _, raw := range []string{"", "https://example.com/demo.git", "gs://", "file://demo/path"} {
		if _, err := ParseTarget(raw); err == nil {
			t.Fatalf("ParseTarget(%q) succeeded", raw)
		}
	}
}
