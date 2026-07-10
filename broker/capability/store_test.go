package capability

import (
	"context"
	"encoding/base64"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/bucketgit/bgit/protocol"
	"github.com/bucketgit/bgit/store"
	"github.com/bucketgit/bgit/store/storetest"
)

type fakeBroker struct {
	post func(context.Context, string, any, any) error
}

func TestCapabilityRejectsInsecureRemoteURL(t *testing.T) {
	broker := fakeBroker{post: func(_ context.Context, _ string, _ any, response any) error {
		*response.(*protocol.ObjectCapabilityResponse) = protocol.ObjectCapabilityResponse{URL: "http://metadata.google.internal/token", Method: http.MethodGet}
		return nil
	}}
	s, err := New(broker, protocol.Repository{Logical: "demo.git"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(context.Background(), "HEAD"); err == nil || !strings.Contains(err.Error(), "insecure") {
		t.Fatalf("error=%v", err)
	}
}

func (f fakeBroker) PostJSON(ctx context.Context, endpoint string, request, response any, _ http.Header) error {
	return f.post(ctx, endpoint, request, response)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestReadSignedURLCapability(t *testing.T) {
	broker := fakeBroker{post: func(_ context.Context, endpoint string, _ any, response any) error {
		if endpoint != "/objects/capability" {
			t.Fatalf("endpoint = %s", endpoint)
		}
		*response.(*protocol.ObjectCapabilityResponse) = protocol.ObjectCapabilityResponse{URL: "https://objects.example.com/blob", Method: http.MethodGet}
		return nil
	}}
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("payload")), Header: http.Header{}}, nil
	})}
	s, err := New(broker, protocol.Repository{Logical: "demo.git"}, Options{HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.Read(context.Background(), "objects/ab/cdef")
	if err != nil || string(data) != "payload" {
		t.Fatalf("Read = %q, %v", data, err)
	}
}

func TestReadLegacyFallback(t *testing.T) {
	broker := fakeBroker{post: func(_ context.Context, endpoint string, _ any, response any) error {
		switch endpoint {
		case "/objects/capability":
			return protocol.ErrUnsupported
		case "/objects/read":
			*response.(*protocol.ObjectResponse) = protocol.ObjectResponse{Data: base64.StdEncoding.EncodeToString([]byte("legacy"))}
			return nil
		default:
			t.Fatalf("endpoint = %s", endpoint)
			return nil
		}
	}}
	s, err := New(broker, protocol.Repository{Logical: "demo.git"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.Read(context.Background(), "HEAD")
	if err != nil || string(data) != "legacy" {
		t.Fatalf("Read = %q, %v", data, err)
	}
}

func TestCapabilityStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Writer {
		memory := &capabilityMemory{values: map[string][]byte{}}
		broker := capabilityContractBroker{memory: memory}
		backend, err := New(broker, protocol.Repository{Logical: "contract.git"}, Options{Local: memory})
		if err != nil {
			t.Fatal(err)
		}
		return backend
	})
}

type capabilityContractBroker struct{ memory *capabilityMemory }

func (b capabilityContractBroker) PostJSON(ctx context.Context, endpoint string, request, response any, _ http.Header) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch endpoint {
	case "/objects/capability":
		value := request.(protocol.ObjectCapabilityRequest)
		*response.(*protocol.ObjectCapabilityResponse) = protocol.ObjectCapabilityResponse{Mode: "local", Object: value.Path}
	case "/objects/list":
		value := request.(protocol.ObjectRequest)
		paths, err := b.memory.list(ctx, value.Prefix)
		if err != nil {
			return err
		}
		*response.(*protocol.ObjectResponse) = protocol.ObjectResponse{Paths: paths}
	default:
		return protocol.ErrUnsupported
	}
	return nil
}

type capabilityMemory struct {
	mu     sync.RWMutex
	values map[string][]byte
}

func (m *capabilityMemory) Read(ctx context.Context, capability protocol.ObjectCapabilityResponse) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.values[capability.Object]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), value...), nil
}
func (m *capabilityMemory) Write(ctx context.Context, capability protocol.ObjectCapabilityResponse, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[capability.Object] = append([]byte(nil), data...)
	return nil
}
func (m *capabilityMemory) Delete(ctx context.Context, capability protocol.ObjectCapabilityResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.values[capability.Object]; !ok {
		return fs.ErrNotExist
	}
	delete(m.values, capability.Object)
	return nil
}
func (m *capabilityMemory) list(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var paths []string
	for path := range m.values {
		if strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}
