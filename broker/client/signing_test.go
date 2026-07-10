package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/bucketgit/bgit/protocol"
	"golang.org/x/crypto/ssh"
)

func TestV2SignaturesUsesInjectedClockNonceAndSigner(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	provider := NewV2Signatures(V2SignatureOptions{
		Signers: []ssh.Signer{signer},
		Now:     func() time.Time { return time.Unix(1770000000, 0) },
		Nonce:   func() (string, error) { return "0123456789abcdef", nil },
	})
	headers, err := provider.HeaderSets(context.Background(), "https://Broker.Example.com", "/repos/mine", []byte(`{"repo":"demo"}`))
	if err != nil || len(headers) != 1 {
		t.Fatalf("headers=%#v err=%v", headers, err)
	}
	header := headers[0]
	if header.Get(protocol.HeaderTimestamp) != "1770000000" || header.Get(protocol.HeaderNonce) != "0123456789abcdef" || header.Get(protocol.HeaderSignedHost) != "broker.example.com" {
		t.Fatalf("headers=%#v", header)
	}
	message, err := base64.StdEncoding.DecodeString(header.Get(protocol.HeaderSignatureMessage))
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.SignatureMessage(protocol.RequestMetadata{Method: "POST", Path: "/repos/mine", Host: "broker.example.com", Timestamp: "1770000000", Nonce: "0123456789abcdef"}, []byte(`{"repo":"demo"}`))
	if string(message) != string(want) {
		t.Fatalf("message=%q want=%q", message, want)
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(header.Get(protocol.HeaderSignature))
	if err != nil {
		t.Fatal(err)
	}
	var signature ssh.Signature
	if err := ssh.Unmarshal(signatureBytes, &signature); err != nil {
		t.Fatal(err)
	}
	if err := signer.PublicKey().Verify(message, &signature); err != nil {
		t.Fatal(err)
	}
}
