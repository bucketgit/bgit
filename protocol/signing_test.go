package protocol

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSignatureMessageV2Golden(t *testing.T) {
	got := string(SignatureMessage(RequestMetadata{
		Method:    http.MethodPost,
		Path:      "/repos/mine",
		Host:      "Broker.Example.Com",
		Timestamp: "1770000000",
		Nonce:     "nonce",
	}, []byte(`{"repo":"demo"}`)))
	want := "bgit-broker-v2\nPOST\n/repos/mine\nbroker.example.com\n1770000000\nnonce\n99caed5a504d67241e3f491cfb6f3088703b3291c70788b1064070947e653755"
	if got != want {
		t.Fatalf("signature message:\n got %q\nwant %q", got, want)
	}
}

func TestValidateRequestMetadata(t *testing.T) {
	now := time.Unix(1770000000, 0)
	accepted := false
	err := ValidateRequestMetadata(RequestMetadata{Timestamp: "1770000000", Nonce: "0123456789abcdef"}, now, 5*time.Minute, func(nonce string, expires time.Time) bool {
		accepted = nonce == "0123456789abcdef" && expires.Equal(now.Add(5*time.Minute))
		return accepted
	})
	if err != nil || !accepted {
		t.Fatalf("ValidateRequestMetadata = %v, accepted=%v", err, accepted)
	}
	if err := ValidateRequestMetadata(RequestMetadata{Timestamp: "1769990000", Nonce: "0123456789abcdef"}, now, time.Minute, func(string, time.Time) bool { return true }); !errors.Is(err, ErrSignatureExpired) {
		t.Fatalf("expired error = %v", err)
	}
	if err := ValidateRequestMetadata(RequestMetadata{Timestamp: "1770000000", Nonce: "0123456789abcdef"}, now, time.Minute, func(string, time.Time) bool { return false }); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay error = %v", err)
	}
}

func TestSignatureV2Fixture(t *testing.T) {
	data, err := os.ReadFile("../spec/testdata/signing-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Vectors []struct {
			Method, Path, Host, Timestamp, Nonce, Body, Message string
			Signatures                                          []struct {
				Name      string `json:"name"`
				PublicKey string `json:"public_key"`
				Signature string `json:"signature"`
			} `json:"signatures"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, vector := range fixture.Vectors {
		got := string(SignatureMessage(RequestMetadata{
			Method: vector.Method, Path: vector.Path, Host: vector.Host,
			Timestamp: vector.Timestamp, Nonce: vector.Nonce,
		}, []byte(vector.Body)))
		if got != vector.Message {
			t.Fatalf("fixture message = %q, want %q", got, vector.Message)
		}
		for _, signed := range vector.Signatures {
			key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(signed.PublicKey))
			if err != nil {
				t.Fatalf("parse %s key: %v", signed.Name, err)
			}
			wire, err := base64.StdEncoding.DecodeString(signed.Signature)
			if err != nil {
				t.Fatalf("decode %s signature: %v", signed.Name, err)
			}
			var signature ssh.Signature
			if err := ssh.Unmarshal(wire, &signature); err != nil {
				t.Fatalf("parse %s signature: %v", signed.Name, err)
			}
			if err := key.Verify([]byte(got), &signature); err != nil {
				t.Fatalf("verify %s signature: %v", signed.Name, err)
			}
		}
	}
}

func TestRepositoryValidation(t *testing.T) {
	if err := (Repository{Logical: "demo.git"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Repository{Bucket: "bucket", Prefix: "repo.git"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Repository{}).Validate(); err == nil {
		t.Fatal("expected empty repository to be invalid")
	}
}
