package app

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/bucketgit/bgit/broker/capability"
	"github.com/bucketgit/bgit/protocol"
)

type brokerGitStore struct {
	brokerURL string
	cfg       config
}

type brokerGitControlPlane struct {
	url string
}

func (b brokerGitControlPlane) PostJSON(ctx context.Context, endpoint string, request, response any, headers http.Header) error {
	extraHeaders := make(map[string]string, len(headers))
	for key := range headers {
		extraHeaders[key] = headers.Get(key)
	}
	return brokerPostJSONContextWithHeaders(ctx, b.url, endpoint, request, response, extraHeaders)
}

type brokerGitLocalCapabilities struct{}

func (brokerGitLocalCapabilities) Read(ctx context.Context, issued protocol.ObjectCapabilityResponse) ([]byte, error) {
	return localBrokerCapabilityRead(ctx, issued)
}

func (brokerGitLocalCapabilities) Write(ctx context.Context, issued protocol.ObjectCapabilityResponse, data []byte) error {
	return localBrokerCapabilityWrite(ctx, issued, data)
}

func (brokerGitLocalCapabilities) Delete(ctx context.Context, issued protocol.ObjectCapabilityResponse) error {
	return localBrokerCapabilityDelete(ctx, issued)
}

func (s *brokerGitStore) capabilityStore() (*capability.Store, error) {
	return capability.New(brokerGitControlPlane{url: s.brokerURL}, repoForBroker(s.cfg), capability.Options{
		HTTPClient:    http.DefaultClient,
		Local:         brokerGitLocalCapabilities{},
		IsNotFound:    isBrokerNotFoundError,
		IsUnsupported: isBrokerCapabilityUnsupported,
	})
}

func (s *brokerGitStore) read(ctx context.Context, objectPath string) ([]byte, error) {
	backend, err := s.capabilityStore()
	if err != nil {
		return nil, err
	}
	return backend.Read(ctx, strings.TrimPrefix(objectPath, "/"))
}

func (s *brokerGitStore) list(ctx context.Context, prefix string) ([]string, error) {
	backend, err := s.capabilityStore()
	if err != nil {
		return nil, err
	}
	return backend.List(ctx, strings.TrimPrefix(prefix, "/"))
}

func (s *brokerGitStore) write(ctx context.Context, objectPath string, data []byte) error {
	backend, err := s.capabilityStore()
	if err != nil {
		return err
	}
	return backend.Write(ctx, strings.TrimPrefix(objectPath, "/"), data)
}

func (s *brokerGitStore) delete(ctx context.Context, objectPath string) error {
	backend, err := s.capabilityStore()
	if err != nil {
		return err
	}
	return backend.Delete(ctx, strings.TrimPrefix(objectPath, "/"))
}

func (s *brokerGitStore) listRefs(ctx context.Context) (map[string]string, error) {
	backend, err := s.capabilityStore()
	if err != nil {
		return nil, err
	}
	return backend.ListRefs(ctx)
}

func isBrokerCapabilityUnsupported(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, protocol.ErrUnsupported) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "unknown broker endpoint") || strings.Contains(message, "404")
}
