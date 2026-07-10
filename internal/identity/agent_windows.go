//go:build windows

package identity

import (
	"net"
	"os"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const windowsOpenSSHAgentPipe = `\\.\pipe\openssh-ssh-agent`

func AgentSigners() ([]ssh.Signer, func(), error) {
	socket := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	if !strings.HasPrefix(socket, `\\.\pipe\`) {
		socket = windowsOpenSSHAgentPipe
	}
	timeout := 5 * time.Second
	var connection net.Conn
	connection, err := winio.DialPipe(socket, &timeout)
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
