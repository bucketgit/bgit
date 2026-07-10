package client

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/bucketgit/bgit/protocol"
	"golang.org/x/crypto/ssh"
)

type NonceSource func() (string, error)

type V2SignatureOptions struct {
	Signers []ssh.Signer
	Now     func() time.Time
	Nonce   NonceSource
	Random  io.Reader
	Rank    func(fingerprint string) int
}

// V2Signatures creates replay-resistant broker signature headers. Callers own
// signer discovery and can inject deterministic time, nonce, and entropy in
// tests.
type V2Signatures struct {
	options V2SignatureOptions
}

func NewV2Signatures(options V2SignatureOptions) *V2Signatures {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Nonce == nil {
		options.Nonce = RandomNonce
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	return &V2Signatures{options: options}
}

func (s *V2Signatures) HeaderSets(_ context.Context, baseURL, path string, payload []byte) ([]http.Header, error) {
	host, err := signatureHost(baseURL)
	if err != nil {
		return nil, err
	}
	type signedHeader struct {
		fingerprint string
		header      http.Header
	}
	result := make([]signedHeader, 0, len(s.options.Signers))
	for _, signer := range s.options.Signers {
		if signer == nil {
			continue
		}
		nonce, err := s.options.Nonce()
		if err != nil {
			return nil, err
		}
		timestamp := fmt.Sprintf("%d", s.options.Now().Unix())
		message := protocol.SignatureMessage(protocol.RequestMetadata{Method: http.MethodPost, Path: path, Host: host, Timestamp: timestamp, Nonce: nonce}, payload)
		signature, err := signSSH(signer, s.options.Random, message)
		if err != nil {
			return nil, err
		}
		fingerprint := ssh.FingerprintSHA256(signer.PublicKey())
		header := http.Header{}
		header.Set(protocol.HeaderSignatureVersion, protocol.SignatureVersion)
		header.Set(protocol.HeaderKey, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))))
		header.Set(protocol.HeaderKeyFingerprint, fingerprint)
		header.Set(protocol.HeaderTimestamp, timestamp)
		header.Set(protocol.HeaderNonce, nonce)
		header.Set(protocol.HeaderSignedHost, host)
		header.Set(protocol.HeaderSignature, base64.StdEncoding.EncodeToString(ssh.Marshal(signature)))
		header.Set(protocol.HeaderSignatureMessage, base64.StdEncoding.EncodeToString(message))
		result = append(result, signedHeader{fingerprint: fingerprint, header: header})
	}
	if s.options.Rank != nil {
		sort.SliceStable(result, func(i, j int) bool {
			return s.options.Rank(result[i].fingerprint) < s.options.Rank(result[j].fingerprint)
		})
	}
	headers := make([]http.Header, len(result))
	for i := range result {
		headers[i] = result[i].header
	}
	return headers, nil
}

func RandomNonce() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func signatureHost(baseURL string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		return "", nil
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid broker URL %q", baseURL)
	}
	return strings.ToLower(parsed.Host), nil
}

func signSSH(signer ssh.Signer, random io.Reader, message []byte) (*ssh.Signature, error) {
	if signer.PublicKey().Type() == ssh.KeyAlgoRSA {
		if algorithmSigner, ok := signer.(ssh.AlgorithmSigner); ok {
			if signature, err := algorithmSigner.SignWithAlgorithm(random, message, ssh.KeyAlgoRSASHA256); err == nil {
				return signature, nil
			}
		}
	}
	return signer.Sign(random, message)
}

var _ SignatureProvider = (*V2Signatures)(nil)
