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

func sshAgentSigners() ([]ssh.Signer, error) {
	sock := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	var conn net.Conn
	var err error
	if strings.HasPrefix(sock, `\\.\pipe\`) {
		conn, err = winio.DialPipe(sock, 5*time.Second)
	} else {
		conn, err = winio.DialPipe(windowsOpenSSHAgentPipe, 5*time.Second)
	}
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return agent.NewClient(conn).Signers()
}
