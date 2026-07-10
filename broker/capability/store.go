// Package capability exposes a BucketGit object store backed by broker-issued
// short-lived object capabilities and broker-owned ref operations.
package capability

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	brokerclient "github.com/bucketgit/bgit/broker/client"
	"github.com/bucketgit/bgit/protocol"
	"github.com/bucketgit/bgit/store"
)

type Broker interface {
	PostJSON(ctx context.Context, endpoint string, request, response any, headers http.Header) error
}

type LocalHandler interface {
	Read(ctx context.Context, capability protocol.ObjectCapabilityResponse) ([]byte, error)
	Write(ctx context.Context, capability protocol.ObjectCapabilityResponse, data []byte) error
	Delete(ctx context.Context, capability protocol.ObjectCapabilityResponse) error
}

type ErrorClassifier func(error) bool

type Options struct {
	HTTPClient    *http.Client
	Local         LocalHandler
	IsNotFound    ErrorClassifier
	IsUnsupported ErrorClassifier
	ResumableAt   int64
}

type Store struct {
	broker Broker
	repo   protocol.Repository
	opts   Options
}

func New(broker Broker, repo protocol.Repository, opts Options) (*Store, error) {
	if broker == nil {
		return nil, fmt.Errorf("broker client is required")
	}
	if err := repo.Validate(); err != nil {
		return nil, err
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.ResumableAt == 0 {
		opts.ResumableAt = 32 * 1024 * 1024
	}
	return &Store{broker: broker, repo: repo, opts: opts}, nil
}

func (s *Store) Read(ctx context.Context, objectPath string) ([]byte, error) {
	objectPath, err := store.ValidatePath(objectPath, false)
	if err != nil {
		return nil, err
	}
	capability, err := s.objectCapability(ctx, objectPath, string(protocol.OperationRead), 0)
	if err == nil {
		return s.readCapability(ctx, capability)
	}
	if s.isNotFound(err) {
		return nil, fs.ErrNotExist
	}
	if !s.isUnsupported(err) {
		return nil, err
	}
	var response protocol.ObjectResponse
	if err := s.broker.PostJSON(ctx, "/objects/read", protocol.ObjectRequest{Repo: s.repo, Path: objectPath}, &response, nil); err != nil {
		if s.isNotFound(err) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	return base64.StdEncoding.DecodeString(response.Data)
}

func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	prefix, err := store.ValidatePath(prefix, true)
	if err != nil {
		return nil, err
	}
	var response protocol.ObjectResponse
	if err := s.broker.PostJSON(ctx, "/objects/list", protocol.ObjectRequest{Repo: s.repo, Prefix: prefix}, &response, nil); err != nil {
		return nil, err
	}
	return response.Paths, nil
}

func (s *Store) Write(ctx context.Context, objectPath string, data []byte) error {
	objectPath, err := store.ValidatePath(objectPath, false)
	if err != nil {
		return err
	}
	capability, err := s.objectCapability(ctx, objectPath, string(protocol.OperationWrite), int64(len(data)))
	if err != nil {
		return err
	}
	return s.writeCapability(ctx, capability, data)
}

func (s *Store) Delete(ctx context.Context, objectPath string) error {
	objectPath, err := store.ValidatePath(objectPath, false)
	if err != nil {
		return err
	}
	capability, err := s.objectCapability(ctx, objectPath, string(protocol.OperationDelete), 0)
	if err != nil {
		return err
	}
	return s.deleteCapability(ctx, capability)
}

func (s *Store) ListRefs(ctx context.Context) (map[string]string, error) {
	var response protocol.RefsResponse
	if err := s.broker.PostJSON(ctx, "/refs/list", protocol.RefsRequest{Repo: s.repo}, &response, nil); err != nil {
		return nil, err
	}
	refs := make(map[string]string, len(response.Refs))
	for ref, oid := range response.Refs {
		oid = strings.TrimSpace(oid)
		if strings.HasPrefix(ref, "refs/") && isSHA1(oid) {
			refs[ref] = oid
		}
	}
	return refs, nil
}

func isSHA1(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func (s *Store) CompareAndSwapRef(ctx context.Context, ref, oldOID, newOID string) error {
	return s.broker.PostJSON(ctx, "/refs/update", protocol.RefUpdateRequest{
		Repo: s.repo, Ref: ref, Old: oldOID, New: newOID,
	}, nil, nil)
}

func (s *Store) objectCapability(ctx context.Context, objectPath, operation string, size int64) (protocol.ObjectCapabilityResponse, error) {
	var response protocol.ObjectCapabilityResponse
	err := s.broker.PostJSON(ctx, "/objects/capability", protocol.ObjectCapabilityRequest{
		Repo: s.repo, Path: objectPath, Operation: operation, Size: size,
		Resumable: s.repo.Provider == string(protocol.ProviderGCS) && operation == string(protocol.OperationWrite) && size > s.opts.ResumableAt,
	}, &response, nil)
	return response, err
}

func (s *Store) readCapability(ctx context.Context, capability protocol.ObjectCapabilityResponse) ([]byte, error) {
	if capability.Mode == "local" {
		if s.opts.Local == nil {
			return nil, fmt.Errorf("local capability handler is required")
		}
		return s.opts.Local.Read(ctx, capability)
	}
	if capability.Mode == "sts" || capability.Provider == string(protocol.ProviderS3) {
		client := s3Client(capability)
		out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(capability.Bucket), Key: aws.String(capability.Object)})
		if err != nil {
			if isS3NotFound(err) {
				return nil, fs.ErrNotExist
			}
			return nil, err
		}
		defer out.Body.Close()
		return io.ReadAll(out.Body)
	}
	if err := validateCapabilityURL(capability.URL); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, firstNonEmpty(capability.Method, http.MethodGet), capability.URL, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(request.Header, capability.Headers)
	response, err := s.opts.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, fs.ErrNotExist
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, responseError("GET", response)
	}
	return io.ReadAll(response.Body)
}

func (s *Store) writeCapability(ctx context.Context, capability protocol.ObjectCapabilityResponse, data []byte) error {
	if capability.Mode == "local" {
		if s.opts.Local == nil {
			return fmt.Errorf("local capability handler is required")
		}
		return s.opts.Local.Write(ctx, capability, data)
	}
	if capability.Mode == "sts" || capability.Provider == string(protocol.ProviderS3) {
		_, err := s3Client(capability).PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(capability.Bucket), Key: aws.String(capability.Object), Body: bytes.NewReader(data),
		})
		return err
	}
	method := firstNonEmpty(capability.Method, http.MethodPut)
	if err := validateCapabilityURL(capability.URL); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, capability.URL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	setHeaders(request.Header, capability.Headers)
	response, err := s.opts.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseError(method, response)
	}
	return nil
}

func (s *Store) deleteCapability(ctx context.Context, capability protocol.ObjectCapabilityResponse) error {
	if capability.Mode == "local" {
		if s.opts.Local == nil {
			return fmt.Errorf("local capability handler is required")
		}
		return s.opts.Local.Delete(ctx, capability)
	}
	if capability.Mode == "sts" || capability.Provider == string(protocol.ProviderS3) {
		_, err := s3Client(capability).DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(capability.Bucket), Key: aws.String(capability.Object),
		})
		return err
	}
	method := firstNonEmpty(capability.Method, http.MethodDelete)
	if err := validateCapabilityURL(capability.URL); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, capability.URL, nil)
	if err != nil {
		return err
	}
	setHeaders(request.Header, capability.Headers)
	response, err := s.opts.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseError(method, response)
	}
	return nil
}

func (s *Store) isNotFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || (s.opts.IsNotFound != nil && s.opts.IsNotFound(err))
}

func (s *Store) isUnsupported(err error) bool {
	return errors.Is(err, protocol.ErrUnsupported) || (s.opts.IsUnsupported != nil && s.opts.IsUnsupported(err))
}

func s3Client(capability protocol.ObjectCapabilityResponse) *s3.Client {
	provider := credentials.NewStaticCredentialsProvider(
		capability.Credentials.AccessKeyID,
		capability.Credentials.SecretAccessKey,
		capability.Credentials.SessionToken,
	)
	return s3.New(s3.Options{Region: firstNonEmpty(capability.Region, "us-east-1"), Credentials: aws.NewCredentialsCache(provider)})
}

func setHeaders(target http.Header, values map[string]string) {
	for key, value := range values {
		target.Set(key, value)
	}
}

func responseError(method string, response *http.Response) error {
	body, _ := io.ReadAll(response.Body)
	return fmt.Errorf("broker object %s: %s %s", method, response.Status, strings.TrimSpace(string(body)))
}

func validateCapabilityURL(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return errors.New("broker returned an invalid object capability URL")
	}
	if parsed.Scheme == "http" {
		host := parsed.Hostname()
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			return errors.New("broker returned an insecure object capability URL")
		}
	}
	return nil
}

func isS3NotFound(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "NoSuchBucket", "NoSuchKey", "NotFound", "404":
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

var _ Broker = (*brokerclient.Client)(nil)
var _ store.Writer = (*Store)(nil)
var _ store.RefStore = (*Store)(nil)
