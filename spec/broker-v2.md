# BucketGit Broker Protocol v2

## Signed Requests

Broker mutations and authenticated reads use SSH-key signatures. The canonical
message is UTF-8 text with seven newline-separated fields and no trailing
newline:

```text
bgit-broker-v2
METHOD
/request/path
lowercase-host
unix-timestamp
nonce
sha256-lowercase-hex-of-request-body
```

Required headers:

```text
X-Bgit-Signature-Version: 2
X-Bgit-Key: <authorized SSH public key>
X-Bgit-Key-Fingerprint: <SHA256 fingerprint>
X-Bgit-Timestamp: <Unix timestamp>
X-Bgit-Nonce: <unique nonce>
X-Bgit-Signed-Host: <lowercase request host>
X-Bgit-Signature: <base64 SSH signature blob>
X-Bgit-Signature-Message: <base64 canonical message>
```

The broker validates timestamp tolerance, consumes each key/nonce pair once,
reconstructs the canonical message, and verifies the SSH signature. RSA keys
use RSA-SHA2-256 where supported; Ed25519 remains supported.

## Repository Identity

Requests identify repositories using the JSON model in Go package `protocol`.
Logical repository name and team identity are the stable control-plane keys.
Provider, bucket, prefix, profile, and region describe physical storage and may
be hidden from normal clients by deployed brokers.

## Object Capabilities

Capabilities are short-lived and restricted to canonical repository-relative
paths. Clients cannot infer authorization from storage access. Deployed brokers
retain ownership of final ref updates and apply role, branch protection, PR,
and compare-and-swap policy.

## Compatibility

- Readers ignore unknown JSON fields.
- New optional fields use `omitempty` where omission is compatible.
- Existing fields are not renamed or removed within protocol v2.
- Missing optional endpoints produce a scoped upgrade warning and do not alter
  unrelated repository data.
