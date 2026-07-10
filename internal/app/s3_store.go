package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/bucketgit/bgit/store"
	s3store "github.com/bucketgit/bgit/store/s3"
)

type s3GitStore struct {
	client *s3.Client
	bucket string
	prefix string
}

var _ objectCASGitRemoteStore = (*s3GitStore)(nil)

func newS3Client(ctx context.Context, cfg config, anonymous bool) (*s3.Client, error) {
	region := awsRegion(cfg)
	if anonymous {
		return s3.New(s3.Options{
			Region:      region,
			Credentials: aws.AnonymousCredentials{},
		}), nil
	}
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithDefaultRegion(region),
	}
	if strings.TrimSpace(cfg.region) != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if strings.TrimSpace(cfg.gcloudConfiguration) != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(strings.TrimSpace(cfg.gcloudConfiguration)))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(awsCfg), nil
}

func awsRegion(cfg config) string {
	if value := strings.TrimSpace(cfg.region); value != "" {
		return value
	}
	return defaultAWSRegion()
}

func defaultAWSRegion() string {
	if value := strings.TrimSpace(os.Getenv("AWS_REGION")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("AWS_DEFAULT_REGION")); value != "" {
		return value
	}
	return "us-east-1"
}

func (s *s3GitStore) read(ctx context.Context, path string) ([]byte, error) {
	backend, err := s3store.New(s.client, s.bucket, s.prefix)
	if err != nil {
		return nil, err
	}
	return backend.Read(ctx, path)
}

func (s *s3GitStore) list(ctx context.Context, prefix string) ([]string, error) {
	backend, err := s3store.New(s.client, s.bucket, s.prefix)
	if err != nil {
		return nil, err
	}
	return backend.List(ctx, prefix)
}

func (s *s3GitStore) write(ctx context.Context, path string, data []byte) error {
	backend, err := s3store.New(s.client, s.bucket, s.prefix)
	if err != nil {
		return err
	}
	return backend.Write(ctx, path, data)
}

func (s *s3GitStore) delete(ctx context.Context, path string) error {
	backend, err := s3store.New(s.client, s.bucket, s.prefix)
	if err != nil {
		return err
	}
	return backend.Delete(ctx, path)
}

func (s *s3GitStore) listRefs(ctx context.Context) (map[string]string, error) {
	backend, err := s3store.New(s.client, s.bucket, s.prefix)
	if err != nil {
		return nil, err
	}
	return backend.ListRefs(ctx)
}

func (s *s3GitStore) compareAndSwapRef(ctx context.Context, ref, oldOID, newOID string) error {
	backend, err := s3store.New(s.client, s.bucket, s.prefix)
	if err != nil {
		return err
	}
	return backend.CompareAndSwapRef(ctx, ref, oldOID, newOID)
}

func (s *s3GitStore) compareAndSwap(ctx context.Context, path string, expected store.ObjectState, replacement []byte) error {
	backend, err := s3store.New(s.client, s.bucket, s.prefix)
	if err != nil {
		return err
	}
	return backend.CompareAndSwap(ctx, path, expected, replacement)
}

func ensureS3Bucket(ctx context.Context, cfg config) error {
	client, err := newS3Client(ctx, cfg, false)
	if err != nil {
		return err
	}
	_, err = client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(cfg.bucket)})
	if err == nil {
		return nil
	}
	if !isS3NotFound(err) {
		return fmt.Errorf("check bucket s3://%s: %w", cfg.bucket, err)
	}
	input := &s3.CreateBucketInput{Bucket: aws.String(cfg.bucket)}
	region := awsRegion(cfg)
	if region != "" && region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(region),
		}
	}
	if _, err := client.CreateBucket(ctx, input); err != nil {
		return fmt.Errorf("create bucket s3://%s in region %s: %w", cfg.bucket, region, err)
	}
	return nil
}

func isS3NotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchBucket", "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}
