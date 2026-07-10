// Package client implements the signed BucketGit broker HTTP protocol.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bucketgit/bgit/protocol"
)

type SignatureProvider interface {
	HeaderSets(ctx context.Context, baseURL, path string, payload []byte) ([]http.Header, error)
}

type SignatureProviderFunc func(context.Context, string, string, []byte) ([]http.Header, error)

func (f SignatureProviderFunc) HeaderSets(ctx context.Context, baseURL, path string, payload []byte) ([]http.Header, error) {
	return f(ctx, baseURL, path, payload)
}

type ErrorDecoder func(endpoint string, status int, message string) error
type RetryPredicate func(status int, message string) bool
type SuccessObserver func(baseURL string, payload []byte, headers http.Header)

type Options struct {
	HTTPClient            *http.Client
	Signatures            SignatureProvider
	AllowUnsignedFallback bool
	DecodeError           ErrorDecoder
	Retry                 RetryPredicate
	ObserveSuccess        SuccessObserver
	MaxResponseBytes      int64
}

type Client struct {
	baseURL string
	http    *http.Client
	opts    Options
}

func New(baseURL string, opts Options) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid broker URL %q", baseURL)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = 64 << 20
	}
	return &Client{baseURL: baseURL, http: opts.HTTPClient, opts: opts}, nil
}

func (c *Client) PostJSON(ctx context.Context, endpoint string, request, response any, extraHeaders http.Header) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return c.PostJSONBytes(ctx, endpoint, payload, response, extraHeaders)
}

func (c *Client) PostJSONBytes(ctx context.Context, endpoint string, payload []byte, response any, extraHeaders http.Header) error {
	if !strings.HasPrefix(endpoint, "/") {
		return fmt.Errorf("broker endpoint must start with /: %q", endpoint)
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.IsAbs() || parsedEndpoint.Host != "" || parsedEndpoint.RawQuery != "" || parsedEndpoint.Fragment != "" || strings.HasPrefix(endpoint, "//") {
		return fmt.Errorf("invalid broker endpoint %q", endpoint)
	}
	headerSets, err := c.signatureHeaderSets(ctx, endpoint, payload)
	if err != nil {
		return err
	}
	var lastErr error
	for index, headers := range headerSets {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		copyHeaders(req.Header, headers)
		copyHeaders(req.Header, extraHeaders)
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxResponseBytes+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if int64(len(body)) > c.opts.MaxResponseBytes {
			return fmt.Errorf("broker %s response exceeds %d bytes", endpoint, c.opts.MaxResponseBytes)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if c.opts.ObserveSuccess != nil {
				c.opts.ObserveSuccess(c.baseURL, payload, headers)
			}
			if response != nil && len(body) > 0 {
				if err := json.Unmarshal(body, response); err != nil {
					return err
				}
			}
			return nil
		}
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		lastErr = c.decodeError(endpoint, resp.StatusCode, message)
		if index == len(headerSets)-1 || c.opts.Retry == nil || !c.opts.Retry(resp.StatusCode, message) {
			return lastErr
		}
	}
	return lastErr
}

func (c *Client) signatureHeaderSets(ctx context.Context, endpoint string, payload []byte) ([]http.Header, error) {
	var sets []http.Header
	if c.opts.Signatures != nil {
		var err error
		sets, err = c.opts.Signatures.HeaderSets(ctx, c.baseURL, endpoint, payload)
		if err != nil {
			return nil, err
		}
	}
	if c.opts.AllowUnsignedFallback || len(sets) == 0 {
		sets = append(sets, http.Header{})
	}
	return sets, nil
}

func (c *Client) decodeError(endpoint string, status int, message string) error {
	if c.opts.DecodeError != nil {
		return c.opts.DecodeError(endpoint, status, message)
	}
	kind := error(nil)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = protocol.ErrUnauthorized
	case http.StatusConflict:
		kind = protocol.ErrConflict
	case http.StatusNotImplemented:
		kind = protocol.ErrUnsupported
	}
	return &protocol.BrokerError{Endpoint: endpoint, Status: status, Message: message, Kind: kind}
}

func copyHeaders(target, source http.Header) {
	for key, values := range source {
		for _, value := range values {
			target.Add(key, value)
		}
	}
}
