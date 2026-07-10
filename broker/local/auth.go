package local

import (
	"context"
	"errors"
	"strings"

	"github.com/bucketgit/bgit/protocol"
)

type IdentityVerifier interface {
	Verify(context.Context, protocol.AuthRequest) (protocol.Identity, error)
}
type IdentityVerifierFunc func(context.Context, protocol.AuthRequest) (protocol.Identity, error)

func (f IdentityVerifierFunc) Verify(ctx context.Context, request protocol.AuthRequest) (protocol.Identity, error) {
	return f(ctx, request)
}

func (b *Broker) Authorize(ctx context.Context, request protocol.AuthRequest) (protocol.AuthResponse, error) {
	if b.verifier == nil {
		return protocol.AuthResponse{}, errors.New("local broker identity verifier is required")
	}
	identity, err := b.verifier.Verify(ctx, request)
	if err != nil {
		return protocol.AuthResponse{}, err
	}
	state, err := b.LoadRepositoryState(ctx, request.Repo)
	if err != nil {
		return protocol.AuthResponse{}, err
	}
	for _, key := range state.Keys {
		if key.Suspended {
			continue
		}
		if sameIdentity(identity, key) && roleAllows(key.Role, request.Operation) {
			return protocol.AuthResponse{Allowed: true, User: key.User, Role: key.Role}, nil
		}
	}
	return protocol.AuthResponse{Allowed: false}, nil
}

func sameIdentity(identity protocol.Identity, key protocol.Key) bool {
	if strings.TrimSpace(identity.PublicKey) != "" && strings.TrimSpace(identity.PublicKey) == strings.TrimSpace(key.PublicKey) {
		return true
	}
	return strings.TrimSpace(identity.User) != "" && strings.EqualFold(strings.TrimSpace(identity.User), strings.TrimSpace(key.User))
}
func roleAllows(role, operation string) bool {
	rank := map[string]int{"read": 1, "triage": 2, "developer": 3, "maintainer": 4, "admin": 5, "owner": 6}[strings.ToLower(strings.TrimSpace(role))]
	switch strings.ToLower(strings.TrimSpace(operation)) {
	case "read":
		return rank >= 1
	case "write", "delete":
		return rank >= 3
	case "merge":
		return rank >= 4
	default:
		return false
	}
}
