# Changelog

All notable changes to `bgit` are documented in this file.

This project follows semantic versioning.

## 0.4.0

Added

- Native Git SSH transport via `bgit ssh`, enabling standard Git clients to
  clone, fetch, and push bucket-backed repositories.
- `bgit ssh setup` and `bgit ssh scaffold` to configure repository remotes and
  `core.sshCommand`.
- Serverless broker provisioning for AWS and GCP, with repo registration and SSH
  public-key administration.
- Broker key management commands for listing, adding, suspending, and removing
  repository keys.
- Broker-enforced SSH authorization for Git data-plane operations.
- Broker-backed compare-and-swap ref updates for concurrent-push safety on both
  AWS and GCP.
- `bgit push --skip-broker` as an operator escape hatch for direct bucket ref
  writes when a broker is configured but unavailable.
- `bgit web` for serving a local browser UI for the configured remote
  repository, with `--local` for browsing the local `.git` store.
- `bgit web` repository UI with branch/tag switching, clone URL copy actions,
  commit metadata, and per-commit diffs.
- `bgit web` automatic port selection, starting at `127.0.0.1:8042` and
  incrementing up to 100 ports when the requested port is already in use.
- GCP broker bootstrap support for required API enablement and the named
  Firestore database `bgit`.
- Git protocol support for upload-pack and receive-pack, including packfile
  ingestion, thin-pack delta bases, push options, and sideband report status.

## 0.3.5

Initial public release of bgit.

Added

- Git repositories stored directly in GCS or S3 buckets.
- Standard `.git` checkouts created by `bgit init` and `bgit clone`, so normal
  Git tooling can inspect local repositories.
- Native local commands for common Git workflows, including `status`, `add`,
  `commit`, `checkout`, `branch`, `tag`, `merge`, `diff`, `show`, `reset`,
  `restore`, `stash`, `revert`, `cherry-pick`, `grep`, `blame`, `clean`,
  `describe`, `ls-files`, `ls-tree`, `archive`, `config`, and `rev-parse`.
- Remote commands for bucket-backed repositories, including `fetch`, `pull`,
  `push`, and `ls-remote`.
- `gs://` and `s3://` origins with default cloud credentials and optional
  named profiles through `--profile`.
- Repository access management with `bgit admin` for read, write, admin,
  public, and private access.
- Automatic bucket creation on first push when permissions allow it.
- Public repository discovery for read-only operations, with authenticated retry
  when a repository is private.
- Prebuilt release binaries for macOS, Linux, and Windows on amd64 and arm64.
