// Package protocol defines the stable data model shared by BucketGit clients
// and brokers. JSON field names are part of the broker compatibility contract.
package protocol

import "fmt"

const (
	SignatureVersion = "2"
	SignaturePrefix  = "bgit-broker-v2"
)

const (
	HeaderSignatureVersion = "X-Bgit-Signature-Version"
	HeaderKey              = "X-Bgit-Key"
	HeaderKeyFingerprint   = "X-Bgit-Key-Fingerprint"
	HeaderTimestamp        = "X-Bgit-Timestamp"
	HeaderNonce            = "X-Bgit-Nonce"
	HeaderSignedHost       = "X-Bgit-Signed-Host"
	HeaderSignature        = "X-Bgit-Signature"
	HeaderSignatureMessage = "X-Bgit-Signature-Message"
)

type Provider string

const (
	ProviderFile Provider = "file"
	ProviderS3   Provider = "s3"
	ProviderGCS  Provider = "gcs"
)

type Operation string

const (
	OperationRead   Operation = "read"
	OperationWrite  Operation = "write"
	OperationDelete Operation = "delete"
	OperationMerge  Operation = "merge"
)

type Role string

const (
	RoleOwner      Role = "owner"
	RoleAdmin      Role = "admin"
	RoleMaintainer Role = "maintainer"
	RoleDeveloper  Role = "developer"
	RoleTriage     Role = "triage"
	RoleRead       Role = "read"
)

func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleMaintainer, RoleDeveloper, RoleTriage, RoleRead:
		return true
	default:
		return false
	}
}

type Repository struct {
	Provider string `json:"provider"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
	Origin   string `json:"origin"`
	Logical  string `json:"logical,omitempty"`
	Host     string `json:"host,omitempty"`
	Profile  string `json:"profile,omitempty"`
	Region   string `json:"region,omitempty"`
	TeamID   string `json:"team_id,omitempty"`
	TeamName string `json:"team_name,omitempty"`
}

func (r Repository) Validate() error {
	if r.Logical == "" && (r.Bucket == "" || r.Prefix == "") {
		return fmt.Errorf("repository requires a logical name or bucket and prefix")
	}
	return nil
}

type AuthRequest struct {
	Repo      Repository `json:"repo"`
	Operation string     `json:"operation"`
}

type AuthResponse struct {
	Allowed bool   `json:"allowed"`
	User    string `json:"user,omitempty"`
	Role    string `json:"role,omitempty"`
}

type RefUpdateRequest struct {
	Repo     Repository `json:"repo"`
	Ref      string     `json:"ref"`
	Old      string     `json:"old"`
	New      string     `json:"new"`
	Override bool       `json:"override,omitempty"`
}

type RefsRequest struct {
	Repo Repository `json:"repo"`
}

type RefsResponse struct {
	Refs map[string]string `json:"refs"`
}

type ObjectCapabilityRequest struct {
	Repo      Repository `json:"repo"`
	Path      string     `json:"path"`
	Operation string     `json:"operation"`
	Size      int64      `json:"size,omitempty"`
	Resumable bool       `json:"resumable,omitempty"`
}

type AWSCredentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
}

type ObjectCapabilityResponse struct {
	Provider    string            `json:"provider"`
	Mode        string            `json:"mode"`
	Method      string            `json:"method,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Bucket      string            `json:"bucket,omitempty"`
	Prefix      string            `json:"prefix,omitempty"`
	Object      string            `json:"object,omitempty"`
	Profile     string            `json:"profile,omitempty"`
	Region      string            `json:"region,omitempty"`
	Credentials AWSCredentials    `json:"credentials,omitempty"`
}

type ObjectRequest struct {
	Repo   Repository `json:"repo"`
	Path   string     `json:"path,omitempty"`
	Prefix string     `json:"prefix,omitempty"`
}

type ObjectResponse struct {
	Data  string   `json:"data,omitempty"`
	Paths []string `json:"paths,omitempty"`
}
