# Changelog

All notable changes to `bgit` are documented in this file.

This project follows semantic versioning.

## 1.2.2

Added

- Added Go-native local broker support with repository-backed metadata,
  filesystem-backed `file://` repositories, and AWS/GCP-backed `s3://` or
  `gs://` repositories behind the normal broker authorization model.
- Added storage-backed local broker ref safeguards with per-ref state records
  and short leases before ref materialization.

## 1.2.1

Fixed

- Improved push efficiency

## 1.2.0

- Added broker-backed CI run records, `bgit ci` commands, and a `bgit web` CI
  view for trusted provider/materializer build handoff against broker refs.
- Added managed CI materializer tokens with secret rotation via
  `bgit admin ci rotate-secret`.
- Hardened broker security with replay-resistant v2 request signatures,
  one-time owner bootstrap tokens, constrained object capabilities, private CI
  materializer invocation, and CSRF protection for local `bgit web` mutations.
- Various bug fixes

## 1.1.3

Added

- Added a broker-backed Task board for low-friction story tracking, with
  backlog/ready/doing/review/done lanes, story comments, taking and reassigning
  stories, drag-and-drop lane moves, optimistic committing state, an "Only me"
  filter, and repo-prefixed story IDs.
- Added `bgit board` commands for listing, creating, moving, taking, and
  commenting on task-board stories.
- Added a `bgit web` pull-request creation flow with base/compare branch
  selection, diff preview, and mergeability/conflict status before creating the
  PR.
- Added user profile settings in `bgit web`, including bio, SSH-key display,
  avatar upload, drag-to-pan cropping, and zoom controls.
- Added QoL feature `bgit admin broker upgrade`, which upgrades the broker associated with
  the current repository profile without going through the setup UI.

Fixed

- `bgit pull` defaults to the currently checked-out branch when no branch is
  provided, instead of using `bucketgit.branch` from repository configuration.
- Updated `golang.org/x/crypto`, `google.golang.org/grpc`, and the AWS S3 SDK
  to address dependency security advisories.

## 1.1.2

Fixed

- `bgit web` now shows team-aware broker clone URLs, so non-core team
  repositories can be cloned from the displayed command.

## 1.1.1

Fixed

- Assets reloading bugfix in bgit web

## 1.1.0

Changed

- Added broker users, broker admins, teams, team-to-repository grants, and
  exact-FQDN TXT discovery for team clone URLs.
- `bgit setup` now seeds the default `core` team, and flat repository flows map
  through `core` while still accepting explicit team clone URLs.
- `bgit setup` now starts from configured brokers, with explicit new, update,
  manage, and delete paths instead of mixing broker creation and redeploys.
- Broker user creation now uses an invite/accept flow, setup management fields
  use selectable roles/users/teams/repos, and invalid roles are rejected.

## 1.0.1

Changed

- Broker logical repository names are now flat. Path-shaped names are rejected
  in the CLI, web settings, and broker to reserve URL paths for team routing.

## 1.0.0

Breaking changes

- BucketGit is now broker-first. Normal repository operations go through a
  broker-backed repo model by default; legacy direct bucket and cloud IAM flows
  moved under `bgit direct`.
- `bgit admin` now manages broker-backed repository users, keys, protection,
  issues, visibility, and danger-zone repository controls instead of cloud IAM.
- Repository setup and selection now use broker profiles from
  `~/.bgit/config.yaml`, including region-qualified profiles.

Added

- Broker-first setup and repository initialization, including cloud profile
  discovery, owner SSH key import, multi-region broker provisioning, and
  `~/.bgit/config.yaml`.
- Broker-issued object-transfer capabilities, logical repo mapping, roles,
  branch protection, pull requests, issues, and GitHub SSH key import.
- Repository visibility, read-only mode, logical rename, destructive owner-only
  delete controls, owner transfer, member invites, and repo-scoped invite
  cancellation.
- `bgit web` as a broker-aware repository browser with embedded assets,
  pull-request review flows, issues, settings, capability-aware controls, and
  local/remote state indicators.
- `bgit direct` as the explicit low-level object-storage and cloud IAM recovery
  path.
- Local broker integration test mode for GCP and AWS runtimes, with coverage for
  roles, branch protection, PRs, issues, native Git transport, public/private
  access, identity selection, and danger-zone controls.

Changed

- Push/fetch/read paths use the broker by default, with region-qualified
  profiles and `--profile NAME --region REGION` disambiguation.
- Setup is more guided for GCP/AWS onboarding, project/billing/API checks, and
  interactive profile, region, and SSH key selection.
- BucketGit identity is configurable globally or per repo, with a clear prompt
  before pushing with the default client identity.

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
