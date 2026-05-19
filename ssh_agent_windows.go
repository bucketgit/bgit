//go:build windows

package main

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

func sshAgentSigners() ([]ssh.Signer, func(), error) {
	sock := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	var conn net.Conn
	var err error
	timeout := 5 * time.Second
	if strings.HasPrefix(sock, `\\.\pipe\`) {
		conn, err = winio.DialPipe(sock, &timeout)
	} else {
		conn, err = winio.DialPipe(windowsOpenSSHAgentPipe, &timeout)
	}
	if err != nil {
		return nil, nil, err
	}
	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return signers, func() { _ = conn.Close() }, nil
}
