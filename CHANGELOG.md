# Changelog

All notable changes to `bgit` are documented in this file.

This project follows semantic versioning.

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
