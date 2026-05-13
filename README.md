# bgit

`bgit` is a Git CLI for repositories stored directly in object storage. It
keeps a normal `.git` checkout on disk, so developers can use familiar Git
commands locally, while `bgit` syncs Git objects, branches, and tags to a
`gs://` or `s3://` repository.

Use it when you want a lightweight Git backend in GCS or S3 without running a
Git server.

## Project

- Homepage: https://bucketgit.com/
- Author: Dennis Vink
- License: MIT

## Install

With Homebrew:

```bash
brew tap bucketgit/bgit
brew install bgit
```

Or build from source:

```bash
git clone https://github.com/bucketgit/bgit.git
cd bgit
go build -o bgit .
```

Check the installed version:

```bash
bgit --version
```

## Build

```bash
go build -o bgit .
```

## Features

- Clone, initialize, fetch, pull, and push repositories backed by GCS or S3.
- Store a repository at any `gs://bucket/path/to/repo.git` or
  `s3://bucket/path/to/repo.git` prefix.
- Work in a normal Git checkout with a standard `.git` directory.
- Use native local workflows for status, add, commit, checkout, branch, merge,
  tag, diff, log, show, reset, restore, stash, revert, grep, blame,
  cherry-pick, clean, describe, ls-files, ls-tree, archive, config, rev-parse,
  rm, and mv.
- Push branches and tags back to object storage with `bgit push`.
- Configure an origin with `bgit origin` or `bgit remote add origin`.
- Grant read, write, admin, public, or private bucket access with `bgit admin`.
- Create and save gcloud profiles with `bgit create-gcloud-profile`.
- Create the target GCS or S3 bucket automatically when permissions allow it.
- Run direct bucket inspection commands for scripts and automation.

## Requirements

- Go 1.22 or newer to build from source.
- The `git` executable available on `PATH` for repository initialization,
  checkout setup, and compatibility config/remote metadata.
- Google Cloud Storage access through `gcloud` or Application Default
  Credentials for `gs://` repositories.
- AWS credentials through the AWS SDK credential chain for `s3://`
  repositories.

By default, `bgit` asks `gcloud` for an OAuth access token and uses that token
for GCS API calls:

```bash
gcloud auth login
gcloud auth print-access-token
```

This follows the active gcloud configuration. To use a named gcloud profile:

```bash
bgit --profile test-profile clone gs://my-bucket/repositories/demo.git
bgit --profile test-profile push
```

Internally, bgit runs `gcloud auth print-access-token`. When a profile is set,
bgit runs that subprocess with `CLOUDSDK_ACTIVE_CONFIG_NAME` set to the profile
name so it matches gcloud's named configuration behavior.
Global flags such as `--profile` and `--auth` can be placed before or after the
command.

### Gcloud Profiles

Use an existing gcloud configuration for one command:

```bash
bgit push --profile test-profile
```

You can also save auth defaults in the checkout:

```bash
bgit config bucketgit.auth gcloud
bgit config bucketgit.profile test-profile
```

Check the saved profile:

```bash
bgit config bucketgit.profile
```

Use `bucketgit.auth adc` to make that checkout use ADC by default. If no auth
config is set, bgit defaults to `gcloud`; if no profile/configuration is set,
bgit uses the active gcloud configuration.

To create a new gcloud profile and save it in the current checkout:

```bash
bgit create-gcloud-profile my-profile
```

This runs `gcloud config configurations create my-profile`, then
`gcloud auth login --configuration my-profile`. Use `--yes` to skip bgit's
confirmation prompt. The gcloud browser login still runs.

```bash
bgit create-gcloud-profile --yes my-profile
```

For CI, service accounts, or environments where ADC is preferred, opt in
explicitly:

```bash
bgit --auth adc push
```

When `bgit put` or `bgit --bucket ... init` targets a GCS bucket that does not
exist, `bgit` attempts to create it in the active Google Cloud project. The
project is read from `GOOGLE_CLOUD_PROJECT`, `GCLOUD_PROJECT`, `GCP_PROJECT`,
or `gcloud config get-value project` using the selected configuration. The
environment variables take precedence, which is useful when a gcloud profile has
an account but no project set.

For S3 repositories, `bgit push` creates the bucket when it does not exist and
the selected AWS credentials have permission. Region selection follows
`AWS_REGION`, then `AWS_DEFAULT_REGION`, then `us-east-1`.

If Google returns an auth error, first check that the selected gcloud
configuration has the expected account and project:

```bash
gcloud config configurations list
CLOUDSDK_ACTIVE_CONFIG_NAME=test-profile gcloud auth print-access-token
CLOUDSDK_ACTIVE_CONFIG_NAME=test-profile gcloud config get-value project
```

### AWS Profiles

For `s3://` origins, bgit uses the AWS SDK credential chain. It supports
`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, temporary credentials through
`AWS_SESSION_TOKEN`, IAM roles, SSO-backed profiles, and the credentials/config
files written by the AWS CLI.

Region selection follows `AWS_REGION`, then `AWS_DEFAULT_REGION`, and defaults
to `us-east-1` when neither is set.

Use an AWS CLI profile for one command:

```bash
bgit clone s3://my-bucket/repositories/demo.git --profile work
bgit push --profile work
```

Save the profile in a checkout:

```bash
bgit config bucketgit.profile work
```

## Quickstart

Clone an existing object-storage-backed repository:

```bash
bgit clone gs://my-bucket/repositories/demo.git ./demo
bgit clone s3://my-bucket/repositories/demo.git ./demo-s3 --profile work
cd demo

git status
git log --oneline
```

For read-only remote operations such as `clone`, `fetch`, `pull`, and
`ls-remote`, `bgit` first tries an anonymous public read. If the repository is
private, it automatically retries with the configured GCS or AWS credentials.

Make a change and push it:

```bash
echo "hello" > README.md
bgit add README.md
bgit commit -m "Add README"
bgit push
```

Create a new repository from an existing directory:

```bash
mkdir demo
cd demo

bgit init
echo "hello" > README.md
bgit add README.md
bgit commit -m "Initial commit"

bgit origin gs://my-bucket/repositories/demo.git
# or:
bgit origin s3://my-bucket/repositories/demo.git
bgit push
```

## Repository URLs

Repository URLs use the `gs://` or `s3://` scheme:

```text
gs://bucket-name/path/to/repo.git
s3://bucket-name/path/to/repo.git
```

The bucket is the object-storage bucket name. Everything after the bucket is the
repository prefix. For example, this repository:

```text
gs://my-bucket/repositories/demo.git
```

is stored under:

```text
gs://my-bucket/repositories/demo.git/HEAD
gs://my-bucket/repositories/demo.git/objects/...
gs://my-bucket/repositories/demo.git/refs/...
```

The same layout is used for S3:

```text
s3://my-bucket/repositories/demo.git/HEAD
s3://my-bucket/repositories/demo.git/objects/...
s3://my-bucket/repositories/demo.git/refs/...
```

## Common Commands

```bash
bgit --version
bgit clone gs://my-bucket/repositories/demo.git [directory]
bgit clone s3://my-bucket/repositories/demo.git [directory]
bgit init [directory]
bgit origin gs://my-bucket/repositories/demo.git
bgit origin s3://my-bucket/repositories/demo.git

bgit fetch
bgit pull
bgit push
bgit push --tags
bgit push --delete feature
bgit ls-remote
bgit admin grant-write user:dev@example.com

bgit checkout -b feature
bgit checkout main
bgit branch
bgit merge feature
bgit tag v1.0.0

bgit status
bgit add -A
bgit commit -m "Update"
bgit diff
bgit log --oneline
bgit show HEAD
bgit restore README.md
bgit reset --hard HEAD
bgit stash
bgit revert HEAD
bgit config user.name "Ada Lovelace"
bgit rev-parse HEAD
```

Local workflow commands are implemented by `bgit` for the supported subset.
Commands outside that subset return `Unsupported` instead of delegating to the
system `git` binary.

## Origins

`bgit clone` writes the origin into `.git/config` automatically. To attach an
origin to an existing checkout, run:

```bash
bgit origin gs://my-bucket/repositories/demo.git
bgit origin s3://my-bucket/repositories/demo.git
```

You can also use Git-style remote commands:

```bash
bgit remote add origin gs://my-bucket/repositories/demo.git
bgit remote add origin s3://my-bucket/repositories/demo.git
bgit remote set-url origin gs://my-bucket/repositories/demo.git
```

If `bgit push` is run without an origin, it prints a copy-pasteable example:

```text
No configured push destination.
Either specify the repository from the command-line:

    bgit --bucket bucket-name --prefix path/to/repo.git push

or configure a bgit origin:

    bgit origin gs://bucket-name/path/to/repo.git
    bgit origin s3://bucket-name/path/to/repo.git

and then push:

    bgit push
```

## Access Control

`bgit admin` grants bucket access using the selected cloud profile. Run it
inside a checkout to infer the bucket and prefix from `.git/config`, or pass
`--bucket` explicitly.

For GCS repositories:

```bash
bgit admin grant-read user:dev@example.com
bgit admin grant-write serviceAccount:ci@project.iam.gserviceaccount.com
bgit admin --bucket my-bucket grant-admin admin@example.com
bgit admin make-public
bgit admin make-private
```

GCS `grant-read` grants `roles/storage.objectViewer` and
`roles/storage.legacyBucketReader`. `grant-write` grants
`roles/storage.objectAdmin` and `roles/storage.legacyBucketReader`.
`grant-admin` grants `roles/storage.admin`. `make-public` grants anonymous read
access at bucket level. `make-private` removes `allUsers` and
`allAuthenticatedUsers` from bgit's bucket-level read roles.

The caller must already have permission to read and update the bucket IAM
policy, such as `roles/storage.admin` on the bucket.

For S3 repositories:

```bash
bgit admin grant-read arn:aws:iam::123456789012:role/Developer
bgit admin --bucket s3://my-bucket/repositories/demo.git grant-write 123456789012
bgit admin --bucket s3://my-bucket/repositories/demo.git grant-admin arn:aws:iam::123456789012:role/Admin
bgit admin --bucket s3://my-bucket/repositories/demo.git make-public
bgit admin --bucket s3://my-bucket/repositories/demo.git make-private
```

S3 identities must be IAM or STS ARNs, 12 digit AWS account IDs, or `*`.
`grant-read` grants `s3:ListBucket` for the repository prefix and
`s3:GetObject` for objects under that prefix. `grant-write` adds
`s3:PutObject`, `s3:DeleteObject`, and multipart abort access. `grant-admin`
grants `s3:*` for the bucket and repository prefix. The caller must already have
permission to read and update the bucket policy.

S3 `make-public` removes bucket-level Block Public Access and adds anonymous
read access for the repository prefix. `make-private` removes bgit's anonymous
statements for that prefix and restores bucket-level Block Public Access.

## Branches And Tags

New repositories default to the `main` branch. Use `--branch` when cloning or
using direct GCS mode to target another branch:

```bash
bgit --branch develop clone gs://my-bucket/repositories/demo.git
bgit --branch release fetch
```

Tags are regular Git tags in the object-storage-backed repository:

```bash
bgit tag v1.0.0
bgit tag -a v1.0.1 -m "Release v1.0.1"
bgit push --tags
bgit ls-remote --tags
```

## Direct GCS Mode

Most developers should use `clone`, `init`, `origin`, and `push`. Direct GCS
mode is available for scripts and one-off inspection without a checkout:

```bash
bgit --bucket my-bucket --prefix repositories/demo.git ls docs/
bgit --bucket my-bucket --prefix repositories/demo.git cat docs/readme.md
bgit --bucket my-bucket --prefix repositories/demo.git log --limit 10
bgit --bucket my-bucket --prefix repositories/demo.git put docs/readme.md --file README.md -m "Add readme" --author "Ada Lovelace" --email ada@example.com
```

## How It Works

`bgit` stores Git objects and refs in an object-storage prefix using the normal Git
repository layout. Remote operations read and write those objects and refs
directly through the GCS or S3 API.

Local checkouts remain normal Git worktrees. `bgit` implements the supported
local workflow commands directly, uses the `git` executable only for repository
setup/config compatibility, and uses object-storage-backed remote updates for
collaboration.

## Unsupported Commands

Some Git commands depend on Git's network protocol, server-side hooks, packfile
maintenance, or repository features that `bgit` does not emulate. Unsupported
commands return:

```text
Unsupported: '<command>' is not supported by bgit
```

Unsupported commands include `rebase`, `daemon`, `submodule`, `lfs`, `gc`,
`fsck`, `repack`, `prune`, `worktree`, credential helpers, server helpers, and
related maintenance commands.

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
fork-to-pull-request workflow and the checks to run before opening a PR.

## License

`bgit` is released under the MIT License. See [LICENSE](LICENSE).

## Disclaimer

`bgit` is provided as-is, without warranty of any kind. You are responsible for
testing it against your own repositories, access controls, backup strategy, and
operational requirements before relying on it in production.

## Help

```bash
bgit help
bgit help push
bgit push --help
bgit --help push
bgit push help
bgit --version
```
