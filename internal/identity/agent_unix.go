//go:build !windows

package identity

import (
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func AgentSigners() ([]ssh.Signer, func(), error) {
	socket := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	if socket == "" {
		return nil, nil, nil
	}
	connection, err := net.Dial("unix", socket)
	if err != nil {
		return nil, nil, err
	}
	signers, err := agent.NewClient(connection).Signers()
	if err != nil {
		_ = connection.Close()
		return nil, nil, err
	}
	return signers, func() { _ = connection.Close() }, nil
}
