package identity

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ExplicitSigners loads private keys selected through BucketGit environment
// variables or an explicit non-fingerprint identity preference.
func ExplicitSigners(preference string) []ssh.Signer {
	var paths []string
	for _, environment := range []string{"BGIT_SSH_KEY", "BGIT_SSH_KEYS"} {
		for _, value := range filepath.SplitList(os.Getenv(environment)) {
			if value = strings.TrimSpace(value); value != "" {
				paths = append(paths, value)
			}
		}
	}
	var inline []string
	if value := strings.TrimSpace(os.Getenv("GIT_SSH_PRIVATE_KEY")); value != "" {
		inline = append(inline, value)
	}
	if value := strings.TrimSpace(preference); value != "" && !strings.HasPrefix(value, "SHA256:") {
		paths = append(paths, value)
	}
	var signers []ssh.Signer
	for _, value := range inline {
		if signer, err := ssh.ParsePrivateKey([]byte(value)); err == nil {
			signers = append(signers, signer)
		}
	}
	seen := map[string]struct{}{}
	for _, name := range paths {
		name = expandHome(name)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		if signer, err := ssh.ParsePrivateKey(data); err == nil {
			signers = append(signers, signer)
		}
	}
	return signers
}

func FingerprintRank(fingerprint string, preferred []string) int {
	for index, value := range preferred {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(fingerprint)) {
			return index
		}
	}
	return len(preferred) + 1
}

func expandHome(value string) string {
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
