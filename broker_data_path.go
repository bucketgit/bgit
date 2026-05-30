package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type brokerObjectCapabilityRequest struct {
	Repo      brokerRepo `json:"repo"`
	Path      string     `json:"path"`
	Operation string     `json:"operation"`
	Size      int64      `json:"size,omitempty"`
	Resumable bool       `json:"resumable,omitempty"`
}

type brokerObjectCapabilityResponse struct {
	Provider    string                     `json:"provider"`
	Mode        string                     `json:"mode"`
	Method      string                     `json:"method,omitempty"`
	URL         string                     `json:"url,omitempty"`
	Headers     map[string]string          `json:"headers,omitempty"`
	Bucket      string                     `json:"bucket,omitempty"`
	Prefix      string                     `json:"prefix,omitempty"`
	Object      string                     `json:"object,omitempty"`
	Profile     string                     `json:"profile,omitempty"`
	Region      string                     `json:"region,omitempty"`
	Credentials brokerObjectAWSCredentials `json:"credentials,omitempty"`
}

type brokerObjectAWSCredentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
}

type brokerRefsRequest struct {
	Repo brokerRepo `json:"repo"`
}

type brokerRefsResponse struct {
	Refs map[string]string `json:"refs"`
}

func (s *brokerGitStore) listRefs(ctx context.Context) (map[string]string, error) {
	var resp brokerRefsResponse
	if err := brokerPostContext(ctx, s.brokerURL, "/refs/list", brokerRefsRequest{Repo: repoForBroker(s.cfg)}, &resp); err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for ref, hash := range resp.Refs {
		if strings.HasPrefix(ref, "refs/") && isHexHash(strings.TrimSpace(hash)) {
			refs[ref] = strings.TrimSpace(hash)
		}
	}
	return refs, nil
}

func (s *brokerGitStore) write(ctx context.Context, objectPath string, data []byte) error {
	capability, err := s.objectCapability(ctx, objectPath, "write", int64(len(data)))
	if err != nil {
		return err
	}
	return s.writeWithCapability(ctx, capability, data)
}

func (s *brokerGitStore) delete(ctx context.Context, objectPath string) error {
	capability, err := s.objectCapability(ctx, objectPath, "delete", 0)
	if err != nil {
		return err
	}
	return s.deleteWithCapability(ctx, capability)
}

func (s *brokerGitStore) readWithCapability(ctx context.Context, objectPath string) ([]byte, bool, error) {
	capability, err := s.objectCapability(ctx, objectPath, "read", 0)
	if err != nil {
		if isBrokerNotFoundError(err) {
			return nil, true, fs.ErrNotExist
		}
		if isBrokerCapabilityUnsupported(err) {
			return nil, false, nil
		}
		return nil, true, err
	}
	data, err := s.getWithCapability(ctx, capability)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, fs.ErrNotExist
	}
	return data, true, err
}

func (s *brokerGitStore) objectCapability(ctx context.Context, objectPath, operation string, size int64) (brokerObjectCapabilityResponse, error) {
	var resp brokerObjectCapabilityResponse
	req := brokerObjectCapabilityRequest{
		Repo:      repoForBroker(s.cfg),
		Path:      strings.TrimPrefix(objectPath, "/"),
		Operation: operation,
		Size:      size,
		Resumable: s.cfg.provider == "gcs" && operation == "write" && size > 32*1024*1024,
	}
	err := brokerPostContext(ctx, s.brokerURL, "/objects/capability", req, &resp)
	return resp, err
}

func (s *brokerGitStore) getWithCapability(ctx context.Context, capability brokerObjectCapabilityResponse) ([]byte, error) {
	if capability.Mode == "local" {
		return localBrokerCapabilityRead(ctx, capability)
	}
	if capability.Mode == "sts" || capability.Provider == "s3" {
		client := s3ClientForBrokerCapability(capability)
		out, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(capability.Bucket),
			Key:    aws.String(capability.Object),
		})
		if err != nil {
			if isS3NotFound(err) {
				return nil, fs.ErrNotExist
			}
			return nil, err
		}
		defer out.Body.Close()
		return io.ReadAll(out.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, firstNonEmpty(capability.Method, http.MethodGet), capability.URL, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range capability.Headers {
		httpReq.Header.Set(key, value)
	}
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode == http.StatusNotFound {
		return nil, fs.ErrNotExist
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(httpResp.Body)
		return nil, fmt.Errorf("broker object GET: %s %s", httpResp.Status, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(httpResp.Body)
}

func (s *brokerGitStore) writeWithCapability(ctx context.Context, capability brokerObjectCapabilityResponse, data []byte) error {
	if capability.Mode == "local" {
		return localBrokerCapabilityWrite(ctx, capability, data)
	}
	if capability.Mode == "sts" || capability.Provider == "s3" {
		client := s3ClientForBrokerCapability(capability)
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(capability.Bucket),
			Key:    aws.String(capability.Object),
			Body:   bytes.NewReader(data),
		})
		return err
	}
	method := firstNonEmpty(capability.Method, http.MethodPut)
	httpReq, err := http.NewRequestWithContext(ctx, method, capability.URL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	for key, value := range capability.Headers {
		httpReq.Header.Set(key, value)
	}
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(httpResp.Body)
		return fmt.Errorf("broker object %s: %s %s", method, httpResp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *brokerGitStore) deleteWithCapability(ctx context.Context, capability brokerObjectCapabilityResponse) error {
	if capability.Mode == "local" {
		return localBrokerCapabilityDelete(ctx, capability)
	}
	if capability.Mode == "sts" || capability.Provider == "s3" {
		client := s3ClientForBrokerCapability(capability)
		_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(capability.Bucket),
			Key:    aws.String(capability.Object),
		})
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, firstNonEmpty(capability.Method, http.MethodDelete), capability.URL, nil)
	if err != nil {
		return err
	}
	for key, value := range capability.Headers {
		httpReq.Header.Set(key, value)
	}
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(httpResp.Body)
		return fmt.Errorf("broker object DELETE: %s %s", httpResp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func s3ClientForBrokerCapability(capability brokerObjectCapabilityResponse) *s3.Client {
	region := firstNonEmpty(capability.Region, defaultAWSRegion())
	creds := credentials.NewStaticCredentialsProvider(
		capability.Credentials.AccessKeyID,
		capability.Credentials.SecretAccessKey,
		capability.Credentials.SessionToken,
	)
	return s3.New(s3.Options{
		Region:      region,
		Credentials: aws.NewCredentialsCache(creds),
	})
}

func isBrokerCapabilityUnsupported(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "unknown broker endpoint") ||
		strings.Contains(message, "404")
}
