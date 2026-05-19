package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type s3GitStore struct {
	client *s3.Client
	bucket string
	prefix string
}

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
	key := joinObjectName(s.prefix, path)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, fs.ErrNotExist
		}
		return nil, s3AccessError("read", s.bucket, key, err)
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (s *s3GitStore) list(ctx context.Context, prefix string) ([]string, error) {
	queryPrefix := objectPrefix(joinObjectName(s.prefix, prefix))
	pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(queryPrefix),
	})
	var paths []string
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, s3AccessError("list", s.bucket, queryPrefix, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			rel := strings.TrimPrefix(key, objectPrefix(s.prefix))
			if rel != "" && !strings.HasSuffix(rel, "/") {
				paths = append(paths, rel)
			}
		}
	}
	return paths, nil
}

func (s *s3GitStore) write(ctx context.Context, path string, data []byte) error {
	key := joinObjectName(s.prefix, path)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return s3AccessError("write", s.bucket, key, err)
	}
	return nil
}

func (s *s3GitStore) delete(ctx context.Context, path string) error {
	key := joinObjectName(s.prefix, path)
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return s3AccessError("delete", s.bucket, key, err)
	}
	return nil
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

func s3AccessError(action, bucket, key string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s s3://%s/%s: %w", action, bucket, key, err)
}
