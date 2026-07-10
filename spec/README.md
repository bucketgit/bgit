# BucketGit Protocol Specification

This directory is the implementation-independent compatibility contract shared
by the Go client/local broker and the AWS/GCP JavaScript brokers.

- `broker-v2.md` documents signed request construction and control-plane
  invariants.
- `testdata/signing-v2.json` contains canonical signing vectors.

JSON schemas for repository, auth, capabilities, refs, issues, pull requests,
and CI will be added as those models move into the public `protocol` package.
