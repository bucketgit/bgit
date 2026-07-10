package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/bucketgit/bgit/protocol"
)

type recordingCaller struct {
	path     string
	request  any
	response func(any)
}

func (c *recordingCaller) PostJSON(_ context.Context, path string, request, response any, _ http.Header) error {
	c.path, c.request = path, request
	if c.response != nil {
		c.response(response)
	}
	return nil
}

func TestAdministrativeEndpointsUseStableProtocolPaths(t *testing.T) {
	repo := protocol.Repository{Logical: "demo.git"}
	tests := []struct {
		name string
		path string
		call func(*Endpoints) error
	}{
		{"list users", "/broker/users/list", func(e *Endpoints) error {
			_, err := e.ListUsers(t.Context(), protocol.RepositoryAdminRequest{})
			return err
		}},
		{"list teams", "/teams/list", func(e *Endpoints) error {
			_, err := e.ListTeams(t.Context(), protocol.RepositoryAdminRequest{})
			return err
		}},
		{"list repositories", "/repos/list", func(e *Endpoints) error {
			_, err := e.ListRepositories(t.Context(), protocol.RepositoryAdminRequest{})
			return err
		}},
		{"repository info", "/repo/info", func(e *Endpoints) error {
			_, err := e.RepositoryInfo(t.Context(), protocol.RepositoryAdminRequest{Repo: repo})
			return err
		}},
		{"repository users", "/repo/users/list", func(e *Endpoints) error {
			_, err := e.ListRepositoryUsers(t.Context(), protocol.RepositoryAdminRequest{Repo: repo})
			return err
		}},
		{"repository teams", "/repo/teams/list", func(e *Endpoints) error {
			_, err := e.ListRepositoryTeams(t.Context(), protocol.RepositoryAdminRequest{Repo: repo})
			return err
		}},
		{"repository invites", "/keys/invite/list", func(e *Endpoints) error {
			_, err := e.ListRepositoryInvites(t.Context(), protocol.OwnerTransferRequest{Repo: repo})
			return err
		}},
		{"protections", "/protection/list", func(e *Endpoints) error {
			_, err := e.ListProtections(t.Context(), protocol.Protection{Repo: repo})
			return err
		}},
		{"profile", "/profile/get", func(e *Endpoints) error {
			_, err := e.GetUserProfile(t.Context(), protocol.UserProfileRequest{Repo: repo})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := &recordingCaller{}
			if err := test.call(NewEndpoints(caller)); err != nil {
				t.Fatal(err)
			}
			if caller.path != test.path {
				t.Fatalf("path=%q want %q", caller.path, test.path)
			}
		})
	}
}
