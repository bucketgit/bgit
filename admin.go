package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"cloud.google.com/go/iam"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	storageAdmin              iam.RoleName = "roles/storage.admin"
	storageObjectAdmin        iam.RoleName = "roles/storage.objectAdmin"
	storageObjectViewer       iam.RoleName = "roles/storage.objectViewer"
	storageLegacyBucketReader iam.RoleName = "roles/storage.legacyBucketReader"
)

type adminGrantOptions struct {
	provider       string
	bucket         string
	prefix         string
	action         string
	member         string
	serviceAccount bool
}

func adminCommand(cfg config, args []string, stdout io.Writer) error {
	opts, err := parseAdminGrantArgs(args)
	if err != nil {
		return err
	}
	if opts.bucket != "" {
		cfg.bucket = opts.bucket
	}
	if opts.prefix != "" {
		cfg.prefix = opts.prefix
	}
	if opts.provider != "" {
		cfg.provider = opts.provider
	}
	cfg.bucket = normalizeAdminBucket(cfg.bucket)
	if cfg.bucket == "" {
		localCfg, err := readLocalConfig(".")
		if err == nil {
			cfg = mergeConfig(cfg, localCfg)
		}
	}
	if cfg.bucket == "" {
		return errors.New("admin requires a bucket; run inside a bgit checkout or pass --bucket BUCKET")
	}
	ctx := context.Background()
	if cfg.provider == "s3" {
		return adminS3Command(ctx, cfg, opts, stdout)
	}

	if isAdminVisibilityAction(opts.action) {
		return adminGCSVisibilityCommand(ctx, cfg, opts, stdout)
	}

	roles, label, err := adminGrantRoles(opts.action)
	if err != nil {
		return err
	}
	member := normalizeIAMMember(opts.member, opts.serviceAccount)

	client, err := newStorageClient(ctx, cfg)
	if err != nil {
		return fmt.Errorf("create storage client: %w", err)
	}
	defer client.Close()

	handle := client.Bucket(cfg.bucket).IAM()
	policy, err := handle.Policy(ctx)
	if err != nil {
		return fmt.Errorf("get IAM policy for gs://%s: %w", cfg.bucket, err)
	}
	for _, role := range roles {
		policy.Add(member, role)
	}
	if err := handle.SetPolicy(ctx, policy); err != nil {
		return fmt.Errorf("set IAM policy for gs://%s: %w", cfg.bucket, err)
	}
	fmt.Fprintf(stdout, "granted %s access to %s on gs://%s\n", label, member, cfg.bucket)
	return nil
}

func parseAdminGrantArgs(args []string) (adminGrantOptions, error) {
	var opts adminGrantOptions
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--bucket":
			i++
			if i >= len(args) || args[i] == "" {
				return opts, errors.New("admin --bucket requires a bucket name")
			}
			opts.provider, opts.bucket, opts.prefix = normalizeAdminTarget(args[i])
		case strings.HasPrefix(arg, "--bucket="):
			opts.provider, opts.bucket, opts.prefix = normalizeAdminTarget(strings.TrimPrefix(arg, "--bucket="))
			if opts.bucket == "" {
				return opts, errors.New("admin --bucket requires a bucket name")
			}
		case arg == "--service-account":
			opts.serviceAccount = true
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) == 0 {
		return opts, errors.New("usage: bgit admin grant-read|grant-write|grant-admin IDENTITY")
	}
	opts.action = normalizeAdminAction(positional[0])
	if isAdminVisibilityAction(opts.action) {
		if len(positional) != 1 {
			return opts, errors.New("usage: bgit admin make-public|make-private")
		}
		return opts, nil
	}
	if len(positional) != 2 {
		return opts, errors.New("usage: bgit admin grant-read|grant-write|grant-admin IDENTITY")
	}
	if _, _, err := adminGrantRoles(opts.action); err != nil {
		return opts, err
	}
	opts.member = positional[1]
	if opts.member == "" {
		return opts, errors.New("admin grant requires an identity")
	}
	return opts, nil
}

func isAdminVisibilityAction(action string) bool {
	switch normalizeAdminAction(action) {
	case "make-public", "public", "make-private", "private":
		return true
	default:
		return false
	}
}

func normalizeAdminBucket(raw string) string {
	_, bucket, _ := normalizeAdminTarget(raw)
	return bucket
}

func normalizeAdminTarget(raw string) (string, string, string) {
	bucket := strings.TrimSpace(raw)
	if bucket == "" {
		return "", "", ""
	}
	if strings.HasPrefix(bucket, "gs://") || strings.HasPrefix(bucket, "gcs://") || strings.HasPrefix(bucket, "s3://") {
		parsed, err := url.Parse(bucket)
		if err == nil && parsed.Host != "" {
			provider := "gcs"
			if parsed.Scheme == "s3" {
				provider = "s3"
			}
			return provider, parsed.Host, strings.Trim(parsed.Path, "/")
		}
	}
	bucket = strings.Trim(bucket, "/")
	if slash := strings.Index(bucket, "/"); slash >= 0 {
		return "", bucket[:slash], strings.Trim(bucket[slash+1:], "/")
	}
	return "", bucket, ""
}

func normalizeAdminAction(action string) string {
	action = strings.ToLower(strings.TrimSpace(action))
	action = strings.ReplaceAll(action, "_", "-")
	action = strings.TrimPrefix(action, "grant-")
	action = strings.TrimSuffix(action, "-access")
	return action
}

func adminGrantRoles(action string) ([]iam.RoleName, string, error) {
	switch normalizeAdminAction(action) {
	case "read":
		return []iam.RoleName{storageObjectViewer, storageLegacyBucketReader}, "read", nil
	case "write":
		return []iam.RoleName{storageObjectAdmin, storageLegacyBucketReader}, "write", nil
	case "admin":
		return []iam.RoleName{storageAdmin}, "admin", nil
	default:
		return nil, "", errors.New("supported admin commands: grant-read, grant-write, grant-admin, make-public, make-private")
	}
}

func adminGCSVisibilityCommand(ctx context.Context, cfg config, opts adminGrantOptions, stdout io.Writer) error {
	client, err := newStorageClient(ctx, cfg)
	if err != nil {
		return fmt.Errorf("create storage client: %w", err)
	}
	defer client.Close()

	handle := client.Bucket(cfg.bucket).IAM()
	policy, err := handle.Policy(ctx)
	if err != nil {
		return fmt.Errorf("get IAM policy for gs://%s: %w", cfg.bucket, err)
	}
	switch normalizeAdminAction(opts.action) {
	case "make-public", "public":
		policy.Add("allUsers", storageObjectViewer)
		policy.Add("allUsers", storageLegacyBucketReader)
		if err := handle.SetPolicy(ctx, policy); err != nil {
			return fmt.Errorf("set IAM policy for gs://%s: %w", cfg.bucket, err)
		}
		fmt.Fprintf(stdout, "made gs://%s public\n", cfg.bucket)
	case "make-private", "private":
		for _, member := range []string{"allUsers", "allAuthenticatedUsers"} {
			policy.Remove(member, storageObjectViewer)
			policy.Remove(member, storageLegacyBucketReader)
		}
		if err := handle.SetPolicy(ctx, policy); err != nil {
			return fmt.Errorf("set IAM policy for gs://%s: %w", cfg.bucket, err)
		}
		fmt.Fprintf(stdout, "made gs://%s private\n", cfg.bucket)
	default:
		return errors.New("supported visibility commands: make-public, make-private")
	}
	return nil
}

func normalizeIAMMember(raw string, serviceAccount bool) string {
	member := strings.TrimSpace(raw)
	if member == "allUsers" || member == "allAuthenticatedUsers" || strings.Contains(member, ":") {
		return member
	}
	if serviceAccount || strings.HasSuffix(member, ".gserviceaccount.com") {
		return "serviceAccount:" + member
	}
	return "user:" + member
}

type s3BucketPolicy struct {
	Version   string              `json:"Version"`
	Statement []s3PolicyStatement `json:"Statement"`
}

type s3PolicyStatement struct {
	Sid       string                    `json:"Sid,omitempty"`
	Effect    string                    `json:"Effect"`
	Principal any                       `json:"Principal"`
	Action    any                       `json:"Action"`
	Resource  any                       `json:"Resource"`
	Condition map[string]map[string]any `json:"Condition,omitempty"`
}

func adminS3Command(ctx context.Context, cfg config, opts adminGrantOptions, stdout io.Writer) error {
	client, err := newS3Client(ctx, cfg, false)
	if err != nil {
		return fmt.Errorf("create S3 client: %w", err)
	}
	if isAdminVisibilityAction(opts.action) {
		return adminS3VisibilityCommand(ctx, client, cfg, opts, stdout)
	}
	principal, err := normalizeAWSPrincipal(opts.member)
	if err != nil {
		return err
	}
	policy, err := readS3BucketPolicy(ctx, client, cfg.bucket)
	if err != nil {
		return err
	}
	label := normalizeAdminAction(opts.action)
	policy.addBucketGitGrant(cfg.bucket, cfg.prefix, label, principal)
	if err := writeS3BucketPolicy(ctx, client, cfg.bucket, policy); err != nil {
		return err
	}
	target := s3AdminTarget(cfg)
	fmt.Fprintf(stdout, "granted %s access to %s on %s\n", label, principal, target)
	return nil
}

func adminS3VisibilityCommand(ctx context.Context, client *s3.Client, cfg config, opts adminGrantOptions, stdout io.Writer) error {
	target := s3AdminTarget(cfg)
	policy, err := readS3BucketPolicy(ctx, client, cfg.bucket)
	if err != nil {
		return err
	}
	switch normalizeAdminAction(opts.action) {
	case "make-public", "public":
		if _, err := client.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{
			Bucket: aws.String(cfg.bucket),
		}); err != nil && !isS3NoSuchPublicAccessBlock(err) {
			return fmt.Errorf("delete public access block for s3://%s: %w", cfg.bucket, err)
		}
		policy.addBucketGitGrant(cfg.bucket, cfg.prefix, "read", "*")
		if err := writeS3BucketPolicy(ctx, client, cfg.bucket, policy); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "made %s public\n", target)
	case "make-private", "private":
		policy.removeBucketGitPublicGrants(cfg.prefix)
		if err := writeS3BucketPolicy(ctx, client, cfg.bucket, policy); err != nil {
			return err
		}
		if _, err := client.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
			Bucket: aws.String(cfg.bucket),
			PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
				BlockPublicAcls:       aws.Bool(true),
				IgnorePublicAcls:      aws.Bool(true),
				BlockPublicPolicy:     aws.Bool(true),
				RestrictPublicBuckets: aws.Bool(true),
			},
		}); err != nil {
			return fmt.Errorf("set public access block for s3://%s: %w", cfg.bucket, err)
		}
		fmt.Fprintf(stdout, "made %s private\n", target)
	default:
		return errors.New("supported visibility commands: make-public, make-private")
	}
	return nil
}

func writeS3BucketPolicy(ctx context.Context, client *s3.Client, bucket string, policy s3BucketPolicy) error {
	if len(policy.Statement) == 0 {
		if _, err := client.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String(bucket)}); err != nil && !isS3NoSuchBucketPolicy(err) {
			return fmt.Errorf("delete bucket policy for s3://%s: %w", bucket, err)
		}
		return nil
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	if _, err := client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucket),
		Policy: aws.String(string(data)),
	}); err != nil {
		return fmt.Errorf("set bucket policy for s3://%s: %w", bucket, err)
	}
	return nil
}

func s3AdminTarget(cfg config) string {
	target := fmt.Sprintf("s3://%s", cfg.bucket)
	if strings.Trim(cfg.prefix, "/") != "" {
		target += "/" + strings.Trim(cfg.prefix, "/")
	}
	return target
}

func readS3BucketPolicy(ctx context.Context, client *s3.Client, bucket string) (s3BucketPolicy, error) {
	out, err := client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(bucket)})
	if err != nil {
		if isS3NoSuchBucketPolicy(err) {
			return s3BucketPolicy{Version: "2012-10-17"}, nil
		}
		return s3BucketPolicy{}, fmt.Errorf("get bucket policy for s3://%s: %w", bucket, err)
	}
	policy := s3BucketPolicy{}
	if err := json.Unmarshal([]byte(aws.ToString(out.Policy)), &policy); err != nil {
		return s3BucketPolicy{}, fmt.Errorf("parse bucket policy for s3://%s: %w", bucket, err)
	}
	if policy.Version == "" {
		policy.Version = "2012-10-17"
	}
	return policy, nil
}

func (p *s3BucketPolicy) addBucketGitGrant(bucket, prefix, action, principal string) {
	if p.Version == "" {
		p.Version = "2012-10-17"
	}
	prefix = strings.Trim(prefix, "/")
	baseSid := s3GrantSid(action, principal, prefix)
	p.removeStatements(baseSid)
	for _, statement := range s3GrantStatements(bucket, prefix, action, principal, baseSid) {
		p.Statement = append(p.Statement, statement)
	}
}

func (p *s3BucketPolicy) removeStatements(baseSid string) {
	var statements []s3PolicyStatement
	for _, statement := range p.Statement {
		if statement.Sid != baseSid+"List" && statement.Sid != baseSid+"Objects" && statement.Sid != baseSid+"Admin" {
			statements = append(statements, statement)
		}
	}
	p.Statement = statements
}

func (p *s3BucketPolicy) removeBucketGitPublicGrants(prefix string) {
	prefix = strings.Trim(prefix, "/")
	for _, action := range []string{"read", "write", "admin"} {
		p.removeStatements(s3GrantSid(action, "*", prefix))
	}
}

func s3GrantStatements(bucket, prefix, action, principal, baseSid string) []s3PolicyStatement {
	bucketARN := "arn:aws:s3:::" + bucket
	objectARN := bucketARN + "/*"
	if prefix != "" {
		objectARN = bucketARN + "/" + prefix + "/*"
	}
	principalValue := any(map[string]any{"AWS": principal})
	if principal == "*" {
		principalValue = "*"
	}
	switch action {
	case "admin":
		return []s3PolicyStatement{{
			Sid:       baseSid + "Admin",
			Effect:    "Allow",
			Principal: principalValue,
			Action:    "s3:*",
			Resource:  []string{bucketARN, objectARN},
		}}
	case "write":
		return []s3PolicyStatement{
			s3ListStatement(bucketARN, prefix, principalValue, baseSid),
			{
				Sid:       baseSid + "Objects",
				Effect:    "Allow",
				Principal: principalValue,
				Action:    []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"},
				Resource:  objectARN,
			},
		}
	default:
		return []s3PolicyStatement{
			s3ListStatement(bucketARN, prefix, principalValue, baseSid),
			{
				Sid:       baseSid + "Objects",
				Effect:    "Allow",
				Principal: principalValue,
				Action:    "s3:GetObject",
				Resource:  objectARN,
			},
		}
	}
}

func s3ListStatement(bucketARN, prefix string, principal any, baseSid string) s3PolicyStatement {
	statement := s3PolicyStatement{
		Sid:       baseSid + "List",
		Effect:    "Allow",
		Principal: principal,
		Action:    "s3:ListBucket",
		Resource:  bucketARN,
	}
	if prefix != "" {
		statement.Condition = map[string]map[string]any{
			"StringLike": {
				"s3:prefix": []string{prefix, prefix + "/*"},
			},
		}
	}
	return statement
}

func s3GrantSid(action, principal, prefix string) string {
	sum := sha1.Sum([]byte(action + "\x00" + principal + "\x00" + prefix))
	return "BucketGit" + strings.Title(action) + hex.EncodeToString(sum[:])[:12]
}

func normalizeAWSPrincipal(raw string) (string, error) {
	principal := strings.TrimSpace(raw)
	if principal == "" {
		return "", errors.New("AWS principal is required")
	}
	if principal == "*" || principal == "allUsers" {
		return "*", nil
	}
	if strings.HasPrefix(principal, "arn:aws:iam::") || strings.HasPrefix(principal, "arn:aws:sts::") {
		return principal, nil
	}
	if regexp.MustCompile(`^\d{12}$`).MatchString(principal) {
		return "arn:aws:iam::" + principal + ":root", nil
	}
	return "", errors.New("AWS admin identity must be an IAM/STS ARN, a 12 digit AWS account ID, or *")
}

func isS3NoSuchBucketPolicy(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "NoSuchBucketPolicy"
	}
	return false
}

func isS3NoSuchPublicAccessBlock(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchPublicAccessBlockConfiguration", "NoSuchPublicAccessBlock", "NoSuchBucket", "NotFound", "404":
			return true
		}
	}
	return false
}
