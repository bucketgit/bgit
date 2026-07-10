package protocol

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type RequestMetadata struct {
	Method    string
	Path      string
	Host      string
	Timestamp string
	Nonce     string
}

// ValidateRequestMetadata validates the replay-sensitive fields of a v2
// signature before cryptographic verification. The callback must atomically
// return true only for a nonce that has not been observed before.
func ValidateRequestMetadata(meta RequestMetadata, now time.Time, maxSkew time.Duration, acceptNonce func(string, time.Time) bool) error {
	timestamp, err := strconv.ParseInt(strings.TrimSpace(meta.Timestamp), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid signature timestamp: %w", err)
	}
	if maxSkew <= 0 {
		return errors.New("signature timestamp skew must be positive")
	}
	signedAt := time.Unix(timestamp, 0)
	delta := now.Sub(signedAt)
	if delta < 0 {
		delta = -delta
	}
	if delta > maxSkew {
		return ErrSignatureExpired
	}
	nonce := strings.TrimSpace(meta.Nonce)
	if len(nonce) < 16 || len(nonce) > 256 {
		return errors.New("signature nonce must contain 16 to 256 characters")
	}
	for _, char := range nonce {
		if char <= 0x20 || char > 0x7e {
			return errors.New("signature nonce contains invalid characters")
		}
	}
	if acceptNonce == nil || !acceptNonce(nonce, signedAt.Add(maxSkew)) {
		return ErrReplay
	}
	return nil
}

// SignatureMessage returns the canonical v2 message signed by BucketGit SSH
// identities. Its byte representation is a wire compatibility contract.
func SignatureMessage(meta RequestMetadata, payload []byte) []byte {
	method := strings.ToUpper(strings.TrimSpace(meta.Method))
	if method == "" {
		method = http.MethodPost
	}
	requestPath := strings.TrimSpace(meta.Path)
	if requestPath == "" {
		requestPath = "/"
	}
	sum := sha256.Sum256(payload)
	return []byte(strings.Join([]string{
		SignaturePrefix,
		method,
		requestPath,
		strings.ToLower(strings.TrimSpace(meta.Host)),
		strings.TrimSpace(meta.Timestamp),
		strings.TrimSpace(meta.Nonce),
		fmt.Sprintf("%x", sum[:]),
	}, "\n"))
}
