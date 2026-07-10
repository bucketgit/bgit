package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bucketgit/bgit/protocol"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestPostJSONRetriesSignatureAndDecodesResponse(t *testing.T) {
	var calls int
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response(http.StatusForbidden, `{"error":"SSH signature required"}`), nil
		}
		if req.Header.Get(protocol.HeaderKeyFingerprint) != "SHA256:test" {
			t.Fatalf("fingerprint = %q", req.Header.Get(protocol.HeaderKeyFingerprint))
		}
		return response(http.StatusOK, `{"allowed":true,"user":"ada"}`), nil
	})}
	c, err := New("https://broker.example.com", Options{
		HTTPClient: httpClient,
		Signatures: SignatureProviderFunc(func(context.Context, string, string, []byte) ([]http.Header, error) {
			return []http.Header{{protocol.HeaderKeyFingerprint: []string{"bad"}}, {protocol.HeaderKeyFingerprint: []string{"SHA256:test"}}}, nil
		}),
		Retry: func(status int, message string) bool {
			return status == http.StatusForbidden && strings.Contains(message, "SSH signature required")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got protocol.AuthResponse
	if err := c.PostJSON(context.Background(), "/auth/check", protocol.AuthRequest{}, &got, nil); err != nil {
		t.Fatal(err)
	}
	if !got.Allowed || got.User != "ada" || calls != 2 {
		t.Fatalf("response = %#v, calls = %d", got, calls)
	}
}

func TestPostJSONReturnsTypedError(t *testing.T) {
	c, err := New("https://broker.example.com", Options{HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusConflict, `{"error":"stale ref"}`), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	err = c.PostJSON(context.Background(), "/refs/update", protocol.RefUpdateRequest{}, nil, nil)
	if !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("error = %v", err)
	}
	var brokerErr *protocol.BrokerError
	if !errors.As(err, &brokerErr) || brokerErr.Endpoint != "/refs/update" || brokerErr.Status != http.StatusConflict {
		t.Fatalf("broker error = %#v", brokerErr)
	}
}

func TestPostJSONHonorsCancellation(t *testing.T) {
	client, err := New("https://broker.example.com", Options{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.PostJSON(ctx, "/auth/check", protocol.AuthRequest{}, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error=%v", err)
	}
}

func TestPostJSONLimitsResponseBody(t *testing.T) {
	client, err := New("https://broker.example.com", Options{MaxResponseBytes: 8, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(http.StatusOK, `{"allowed":true}`), nil })}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PostJSON(context.Background(), "/auth/check", protocol.AuthRequest{}, nil, nil); err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("limit error=%v", err)
	}
}

func TestPostJSONRejectsEndpointAuthority(t *testing.T) {
	client, err := New("https://broker.example.com", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PostJSON(context.Background(), "//attacker.example/path", nil, nil, nil); err == nil {
		t.Fatal("authority endpoint accepted")
	}
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}
