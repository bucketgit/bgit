package identity

import "testing"

func TestDefaultEmailAndEffective(t *testing.T) {
	if got := DefaultEmail("Dennis Vink!"); got != "dennisvink@bucketgit.com" {
		t.Fatalf("DefaultEmail=%q", got)
	}
	value := Effective("", "", "dennis@bucketgit.com")
	if value.Name != DefaultName || !value.UsesDefault {
		t.Fatalf("Effective=%#v", value)
	}
	if !ValidEmail("user@example.com") || ValidEmail("invalid") {
		t.Fatal("email validation mismatch")
	}
}
