//go:build windows

package app

import (
	internalidentity "github.com/bucketgit/bgit/internal/identity"
	"golang.org/x/crypto/ssh"
)

func sshAgentSigners() ([]ssh.Signer, func(), error) {
	return internalidentity.AgentSigners()
}
