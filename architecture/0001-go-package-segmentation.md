# ADR 0001: Segment BucketGit Into Importable Go Packages

Status: accepted

## Context

BucketGit's CLI, Git repository engine, cloud stores, broker protocol, local
broker, and transport previously shared one root package. Other Go programs
cannot reuse the protocol or repository behavior without copying code or
invoking the CLI.

## Decision

BucketGit will expose provider-neutral packages for protocol, storage, Git
repository behavior, broker clients/local brokers, and Git transport. The
executable delegates through a small root compatibility facade to
`internal/app`; configuration, setup, identity discovery, command parsing, and
web presentation live in focused `internal` packages.

Repository modes are compositions:

- `file://`: local broker plus filesystem store.
- `s3://`: local broker plus S3 store.
- `gs://`: local broker plus GCS store.
- AWS broker: remote broker client plus capability-backed S3 store.
- GCP broker: remote broker client plus capability-backed GCS store.

The SDK remains in the `bucketgit/bgit` repository and uses module path
`github.com/bucketgit/bgit`. Because BucketGit already publishes v1 tags, the
segmented SDK follows the module's v1 compatibility contract.

## Constraints

- Existing storage/state formats and broker protocol remain compatible.
- Final broker-backed ref updates remain broker-owned and compare-and-swap.
- Public packages cannot import CLI/internal packages.
- The Git repository package cannot import provider or broker implementations.
- AWS/GCP JavaScript brokers share golden protocol fixtures with Go.

## Consequences

- Migration uses adapters and type aliases to keep every intermediate commit
  releasable.
- Provider SDK dependencies remain isolated in provider packages.
- Package contracts and cross-language protocol fixtures become release gates.
- A separate SDK repository may be considered only after the public API has
  survived two stable releases.
