# BucketGit Go Package Segmentation Plan

## Objective

Refactor `bgit` from a single `package main` implementation into a small public
Go SDK plus CLI-specific packages. The resulting packages must:

- Make the `bgit` executable easier to navigate and maintain.
- Let other Go applications read, write, serve, and coordinate BucketGit
  repositories without invoking the CLI as a subprocess.
- Preserve the existing on-disk/object-storage layout and broker wire protocol.
- Preserve all current repository modes throughout the migration.
- Keep authorization, final ref updates, branch protection, and compare-and-swap
  behavior at least as strong as they are today.

This is a code-organization and API-extraction effort. It is not a new storage
format, broker protocol version, or CLI redesign.

## Repository Modes To Preserve

The implementation should treat repository modes as compositions of reusable
broker and storage packages, not as five independent implementations.

| User-facing mode | Control plane | Object storage | Credentials used by client |
|---|---|---|---|
| `file://repo.git` | In-process local broker | Local filesystem | Local filesystem access |
| `s3://repo.git` | In-process local broker | AWS S3 | Configured AWS credentials |
| `gs://repo.git` | In-process local broker | Google Cloud Storage | Configured GCP credentials |
| AWS deployed broker | Remote AWS broker | S3 through broker-issued capabilities | SSH key plus short-lived STS credentials |
| GCP deployed broker | Remote GCP broker | GCS through broker-issued capabilities | SSH key plus signed URLs/upload sessions |

The package graph must make these combinations explicit. Provider-specific
packages should implement storage. Broker packages should implement control
plane behavior. The Git repository package should depend on interfaces, not on
AWS, GCP, HTTP, CLI config, or the local broker.

## Non-Goals

- Do not rewrite the cloud broker runtimes in Go as part of this effort.
- Do not change Git object paths, ref paths, pack formats, metadata namespaces,
  or the `.bucketgit/broker-state/` layout.
- Do not weaken broker-owned final ref updates for deployed brokers.
- Do not make raw object-store access equivalent to broker authorization.
- Do not expose the current CLI `config` struct as a public SDK API.
- Do not move the SDK to a separate repository until the API has stabilized.
- Do not combine package extraction with unrelated CLI or web UI redesigns.

## Current Coupling To Remove

The current codebase contains approximately 42,000 lines of Go in one package.
The relevant coupling points are:

- `main.go` owns CLI config, URI parsing, cloud clients, remote-store
  construction, and command dispatch.
- `native_git.go` defines the generic store interfaces, GCS and filesystem
  stores, Git object parsing, pack access, refs, fetch, pull, and push.
- `s3_store.go` implements S3 but consumes the CLI `config` type.
- `web.go` owns `brokerGitStore`, even though that store is needed outside the
  web UI.
- `ssh.go` mixes Git transport, broker signing, broker HTTP, cloud deployment,
  SSH identity discovery, and administration.
- `broker_data_path.go` mixes broker protocol DTOs with capability-backed S3
  and GCS object transfer.
- `local_broker_native.go` mixes the local broker runtime, protocol routing,
  metadata persistence, object capabilities, refs, issues, and board behavior.
- Broker models such as `brokerRepo`, issues, PRs, CI runs, users, capabilities,
  and ref updates are spread across CLI and web files.
- AWS and GCP broker runtimes are JavaScript implementations, so a Go-only
  protocol package cannot by itself prevent cross-language drift.

The migration must untangle these dependencies without a big-bang rewrite.

## Module And Versioning Strategy

### Initial location

Keep the packages in the `bucketgit/bgit` repository. Change the module path:

```go
module github.com/bucketgit/bgit
```

The executable can remain at the repository root during early phases. Move it
to `cmd/bgit` only after the extracted packages no longer import package-main
types.

### Public API maturity

- Tag SDK releases with normal `bgit` releases. Because the product already
  publishes `v1.x` tags and this plan intentionally keeps one module, the first
  segmented release establishes a Go module v1 API; a separate v0 line is not
  representable without introducing a second module.
- Review and minimize the exported API before that release. Future breaking
  changes require a `/v2` module path or a new major-version package.
- Keep the broker wire protocol version independent from the Go module version.
- Continue using replay-resistant broker request signature protocol v2.
- Do not create a second Go module or repository until package boundaries and
  public types have survived at least two releases.

### Compatibility policy

- Existing CLI configuration remains readable.
- Existing local-broker state remains readable and writable.
- Existing AWS/GCP brokers remain compatible with new clients.
- New Go clients must be able to communicate with supported older brokers.
- Wire fields may be added compatibly; fields may not be renamed or removed
  without a broker protocol version change.

## Target Package Layout

```text
bgit/
  cmd/bgit/                    executable entry point
  protocol/                    public broker wire model and signing contract
  store/                       provider-neutral object and ref interfaces
  store/fs/                    filesystem object storage
  store/s3/                    AWS S3 object storage
  store/gcs/                   Google Cloud Storage
  repository/                  Git objects, refs, revisions, packs and trees
  broker/client/               remote broker HTTP client
  broker/capability/           capability-backed object store
  broker/local/                in-process broker engine and persisted state
  transport/                   upload-pack, receive-pack and remote helper
  internal/config/             global and repository CLI configuration
  internal/identity/           SSH-agent/key discovery and selection
  internal/setup/              cloud discovery and deployment primitives
  internal/cli/                shared parsing and terminal presentation
  internal/web/                web routes, assets, middleware and events
  internal/app/                feature flows and concrete CLI/web adapters
  spec/                        protocol documentation, schemas and test vectors
```

Package names can be adjusted during implementation, but the dependency
direction below is mandatory.

## Required Dependency Direction

```text
cmd/bgit
  -> root compatibility facade -> internal/app
     -> internal/cli
     -> internal/config, internal/setup, internal/identity, internal/web
     -> transport, repository, broker/client, broker/local
     -> store/fs, store/s3, store/gcs

transport -> repository
repository -> store
broker/capability -> protocol, store
broker/client -> protocol
broker/local -> protocol, store
store/fs -> store
store/s3 -> store
store/gcs -> store
protocol -> standard library and narrowly scoped crypto dependencies
```

Forbidden dependencies:

- Public SDK packages must not import `internal/cli`, `internal/config`, or
  `internal/app`; the root CLI compatibility facade is the sole exception.
- `repository` must not import AWS, GCP, HTTP broker, local broker, or CLI
  configuration packages.
- Provider stores must not import broker or Git repository packages.
- `protocol` must not import storage providers, repository logic, or CLI code.
- Broker packages must not depend on command-line parsing or terminal output.

Use a dependency test or architecture linter to enforce these rules.

## Public Package APIs

The exact names may evolve before the first segmented release, but
implementation should target the following responsibilities and shapes.

### `store`

`store` is a provider-neutral key/value object namespace. All paths are relative
to an already scoped repository prefix.

```go
package store

type Reader interface {
    Read(ctx context.Context, path string) ([]byte, error)
    List(ctx context.Context, prefix string) ([]string, error)
}

type Writer interface {
    Reader
    Write(ctx context.Context, path string, data []byte) error
    Delete(ctx context.Context, path string) error
}

type RefStore interface {
    ListRefs(ctx context.Context) (map[string]string, error)
    CompareAndSwapRef(ctx context.Context, ref, oldOID, newOID string) error
}
```

Requirements:

- Standardize not-found behavior on `fs.ErrNotExist`.
- Validate that paths are relative, canonical, and cannot escape the repository
  namespace.
- Return deterministic, relative paths from `List`.
- Accept injected provider clients for tests and advanced consumers.
- Keep credential discovery in provider convenience constructors, not in the
  base interfaces.
- Distinguish raw object writes from authorized ref mutation.

### `store/fs`

```go
fsstore.New(root string, options ...Option) (*Store, error)
```

Responsibilities:

- Filesystem reads, lists, atomic writes, deletes, and sync where required.
- Symlink/path-escape prevention.
- Durable temporary-file-plus-rename writes.
- Optional file locking primitives used by the local broker.

### `store/s3`

```go
s3store.New(client S3API, bucket, prefix string, options ...Option) (*Store, error)
s3store.Load(ctx context.Context, Options) (*Store, error)
```

Responsibilities:

- S3 get/list/put/delete behavior.
- Region/profile/default credential convenience loading.
- Error translation into shared store errors.
- Repository prefix scoping.
- No broker authorization decisions.

### `store/gcs`

```go
gcsstore.New(client *storage.Client, bucket, prefix string, options ...Option) (*Store, error)
gcsstore.Load(ctx context.Context, Options) (*Store, error)
```

Responsibilities mirror S3. The constructor receiving an existing client is
the primary testable API; credential-loading helpers are optional convenience.

### `repository`

`repository` implements Git repository semantics over `store.Reader` or
`store.Writer`.

```go
type Repository struct { /* unexported fields */ }

func Open(objects store.Reader, refs store.RefStore, options ...Option) *Repository

func (r *Repository) Resolve(ctx context.Context, revision string) (OID, error)
func (r *Repository) Object(ctx context.Context, oid OID) (Object, error)
func (r *Repository) Commit(ctx context.Context, oid OID) (Commit, error)
func (r *Repository) Tree(ctx context.Context, revision string) ([]TreeEntry, error)
func (r *Repository) ListRefs(ctx context.Context) (map[string]OID, error)
func (r *Repository) MergeBase(ctx context.Context, a, b OID) (OID, error)
```

Additional internal or public sub-APIs should cover pack indexes, deltas,
reachable-object traversal, archive generation, diff data, upload preparation,
and receive-pack ingestion.

Requirements:

- Replace methods that parse CLI args or write formatted output with typed
  inputs and typed results.
- Keep SHA-1 as the supported object format initially, but model OIDs so future
  object formats are not impossible.
- Preserve loose object and pack behavior, including thin-pack ingestion.
- Keep caches private to a repository instance.
- Do not expose provider clients or CLI config.

### `protocol`

`protocol` defines the supported broker contract:

- Repository identity and logical-to-physical mapping.
- Authentication and authorization requests/responses.
- Object capability requests/responses.
- Ref listing and compare-and-swap updates.
- Users, keys, roles, teams, invites, and ownership transfer.
- Branch protection.
- Pull requests, reviews, comments, issues, task-board stories, and CI runs.
- Broker health, version, and capabilities.
- Canonical v2 request signing and verification test vectors.

Example API:

```go
type Repository struct {
    Provider string `json:"provider,omitempty"`
    Bucket   string `json:"bucket,omitempty"`
    Prefix   string `json:"prefix,omitempty"`
    Logical  string `json:"logical"`
    TeamID   string `json:"team_id,omitempty"`
    Profile  string `json:"profile,omitempty"`
    Region   string `json:"region,omitempty"`
}

type Signer interface {
    Sign(ctx context.Context, message []byte) (Signature, error)
}

func SignatureMessage(RequestMetadata, []byte) ([]byte, error)
```

Requirements:

- No request type should depend on CLI-only configuration.
- Unix timestamps, nonces, host, path, method, and body digest remain part of
  canonical v2 signing.
- JSON tags and omission behavior are compatibility-sensitive.
- Add golden JSON and signature vectors under `spec/testdata`.
- Run the same vectors against Go, GCP JavaScript, and embedded AWS JavaScript.

### `broker/client`

```go
type Client struct { /* base URL, HTTP client, signer, clock, nonce source */ }

func New(baseURL string, options ...Option) (*Client, error)
func (c *Client) Authorize(ctx context.Context, req protocol.AuthRequest) (...)
func (c *Client) ListRefs(ctx context.Context, repo protocol.Repository) (...)
func (c *Client) UpdateRef(ctx context.Context, req protocol.RefUpdateRequest) error
func (c *Client) ObjectCapability(ctx context.Context, req protocol.ObjectCapabilityRequest) (...)
```

Requirements:

- Accept an injected `http.Client`, signer, clock, and nonce source.
- Own HTTP status/error decoding and broker-version compatibility messages.
- Never print; return typed errors with endpoint, status, broker code, and safe
  message.
- Support explicit retry policy only for safe operations.
- Do not retry ref updates or other non-idempotent mutations implicitly.
- Keep private material and signed headers out of error strings and logs.

### `broker/capability`

Turn broker-issued capabilities into a `store.Writer` implementation:

```go
func New(client *brokerclient.Client, repo protocol.Repository, options ...Option) *Store
```

Responsibilities:

- Request per-object read/write/delete capabilities.
- Use S3 STS credentials, GCS signed URLs, resumable sessions, or local
  capabilities as returned by the broker.
- Use `/refs/list` and broker ref CAS rather than listing or writing raw ref
  objects when a broker controls the repository.
- Treat capability mode/provider as server-returned data, not client guesses.

### `broker/local`

The local broker should become a Go service object with direct methods. It must
not require an HTTP loopback server or simulate HTTP internally.

```go
type Broker struct { /* stores, identity verifier, clock, locks */ }

func New(options Options) (*Broker, error)
func (b *Broker) Authorize(ctx context.Context, req protocol.AuthRequest) (...)
func (b *Broker) ObjectCapability(ctx context.Context, req protocol.ObjectCapabilityRequest) (...)
func (b *Broker) UpdateRef(ctx context.Context, req protocol.RefUpdateRequest) error
```

Responsibilities:

- Persist authoritative metadata in `.bucketgit/broker-state/`.
- Support filesystem, S3, and GCS object backends.
- Preserve storage-backed ref state, short leases, and conflict recovery.
- Preserve users, teams, issues, board ordering/archive state, PRs, protection,
  and all current local endpoints.
- Keep the current local URL/config adapter in the CLI, not in the broker core.
- Offer an optional `http.Handler` adapter for conformance tests and embedders;
  the CLI should call methods directly.

### `transport`

```go
func ServeUploadPack(ctx context.Context, repo *repository.Repository, in io.Reader, out io.Writer) error
func ServeReceivePack(ctx context.Context, repo *repository.Repository, refs RefUpdater, in io.Reader, out io.Writer) error
func ServeRemoteHelper(ctx context.Context, resolver Resolver, args []string, in io.Reader, out, errOut io.Writer) error
```

Responsibilities:

- Git pkt-line protocol and advertised capabilities.
- Upload-pack reachable-object collection and pack generation.
- Receive-pack parsing, thin-pack handling, validation, report-status, atomic
  behavior, and ref updates.
- `git-remote-bgit` helper protocol.
- No cloud credential discovery, CLI config parsing, or terminal UI.

## Protocol Specification And Cross-Language Contract

The cloud brokers remain JavaScript. A public Go package therefore needs an
implementation-independent protocol source of truth.

Create:

```text
spec/
  README.md
  broker-v2.md
  schemas/
    repository.json
    auth.json
    object-capability.json
    ref-update.json
    issue.json
    pull-request.json
    ci-run.json
  testdata/
    signing-v2.json
    canonical-json/
    error-responses/
```

The specification must define:

- Endpoint paths, methods, status codes, and error body shape.
- Required and optional fields.
- Canonical request-signature construction.
- Timestamp tolerance and replay/nonce rules.
- Repository logical identity and physical mapping semantics.
- Allowed object capability paths and operations.
- Ref CAS and branch-protection semantics.
- Compatibility expectations for unknown fields and missing endpoints.

Do not generate all runtime code initially. First establish golden fixtures and
conformance tests. Generation can be considered after schemas prove stable.

## Error Model

Introduce typed errors early, before moving implementations:

```go
var ErrNotFound = fs.ErrNotExist
var ErrConflict = errors.New("bucketgit conflict")
var ErrUnauthorized = errors.New("bucketgit unauthorized")
var ErrUnsupported = errors.New("bucketgit unsupported")

type BrokerError struct {
    Endpoint string
    Status   int
    Code     string
    Message  string
}
```

Requirements:

- Preserve `errors.Is` behavior for not-found, conflict, unauthorized, and
  unsupported cases.
- Separate safe user-facing messages from provider/debug details.
- Avoid string matching for normal control flow once typed errors exist.
- Preserve compatibility hints for genuinely old brokers without mislabeling
  missing identities or permissions as version skew.

## Configuration Boundary

The current `config` struct is a CLI aggregation object and must remain
internal. Replace it at package boundaries with explicit options:

```go
type S3Options struct {
    Bucket  string
    Prefix  string
    Region  string
    Profile string
}

type RemoteRepository struct {
    BrokerURL string
    Repository protocol.Repository
}
```

`internal/config` remains responsible for:

- `~/.bgit/config.yaml`.
- `.git/config` BucketGit keys.
- Profile and region selection.
- Environment-variable overrides.
- Mapping configuration into public package constructors.

Public packages should not run `git config`, `gcloud`, `aws`, or interactive
prompts unless the package is explicitly a CLI/setup convenience package.

## Security Invariants

Every phase must preserve these invariants:

- All broker mutations use v2 request signatures.
- Replay protection remains based on timestamp and nonce validation.
- Object capability paths are canonical, repo-relative, and restricted to
  approved Git/object namespaces.
- Broker-issued credentials remain short-lived and repository-scoped.
- Deployed-broker final ref updates always go through broker CAS.
- Branch protection and PR requirements cannot be bypassed by the SDK's normal
  broker-backed API.
- Local-broker ref updates retain storage-backed leases and conflict checks.
- Filesystem stores reject symlink and traversal escapes.
- Provider credentials and signed URLs are never persisted in repository
  metadata or returned in ordinary errors.
- Direct/raw stores are documented as storage primitives, not authorization or
  policy enforcement.

## Migration Strategy

Use adapters and type aliases to migrate incrementally. At the end of every
phase, the CLI must build and the existing integration suite must pass.

### Phase 0: Baseline And Architecture Guardrails

- [x] Record current `go test ./...` results and integration-suite results.
- [x] Record golden CLI output for clone/fetch/push, local broker, PR, board,
  issues, CI, and web operations.
- [x] Add an architecture decision record describing package boundaries and
  dependency direction.
- [x] Change the module path to `github.com/bucketgit/bgit`.
- [x] Add a package dependency test that rejects forbidden imports.
- [x] Add `go vet ./...` and public example compilation to CI.
- [x] Define supported Go version and SDK compatibility policy.

Exit criteria:

- Existing behavior is captured well enough to distinguish refactoring from
  regression.
- Module-path change builds on all supported OS/architecture targets.

### Phase 1: Extract Protocol Types And Signing

- [x] Inventory every request/response model in `ssh.go`,
  `broker_commands.go`, `broker_data_path.go`, `auth_capabilities.go`, and
  `web.go`.
- [x] Create `protocol` types with stable JSON tags.
- [x] Move role, provider, operation, lane, PR status, and CI status constants
  into typed string definitions.
- [x] Extract v2 canonical signature message construction.
- [x] Extract signature header names and timestamp/nonce validation helpers.
- [x] Create golden signing fixtures covering RSA and Ed25519 keys.
- [x] Run those fixtures against Go and both JavaScript broker runtimes.
- [x] Use type aliases in package main during migration where practical.
- [x] Replace generic broker errors with typed protocol/client errors.

Exit criteria:

- All broker DTOs used by more than one feature come from `protocol`.
- Existing deployed brokers accept requests produced by the extracted signer.
- No JSON payload changes appear in captured fixtures.

### Phase 2: Extract Store Interfaces And Filesystem Backend

- [x] Create `store.Reader`, `store.Writer`, and `store.RefStore`.
- [x] Move fallback-store behavior into `store`.
- [x] Extract common path validation and object-prefix helpers.
- [x] Move `localGitStore` into `store/fs`.
- [x] Implement atomic filesystem writes and traversal/symlink tests.
- [x] Create a reusable store contract suite.
- [x] Adapt `nativeGitRepo` to consume the public interfaces through aliases or
  adapters.

Store contract tests must cover:

- Read/write/delete round trips.
- Missing objects and `fs.ErrNotExist`.
- Empty and nested prefix listing.
- Stable relative path results.
- Zero-byte and large objects.
- Concurrent writes where supported.
- `../`, absolute path, backslash, encoded traversal, and symlink escapes.

Exit criteria:

- Filesystem repositories pass the existing local-broker and Git tests through
  the extracted package.
- No public store API depends on CLI config.

### Phase 3: Extract S3 And GCS Backends

- [x] Move `s3GitStore` and provider error translation into `store/s3`.
- [x] Move `gcsGitStore` into `store/gcs`.
- [x] Introduce minimal provider client interfaces for unit tests.
- [x] Keep constructors accepting existing SDK clients.
- [x] Add convenience constructors for profile/default credentials.
- [x] Move bucket creation out of the core store into explicit provisioning
  helpers used by the CLI/local-broker bootstrap flow.
- [x] Run the shared store contract suite against fake/in-memory provider
  clients.
- [x] Keep real S3/GCS smoke tests outside normal CI unless credentials are
  explicitly supplied.

Exit criteria:

- `file://`, `s3://`, and `gs://` local-broker repositories use the same
  provider-neutral store interfaces.
- Real-provider smoke tests cover create, clone, fetch, push, ref conflict, and
  cleanup.

### Phase 4: Extract Git Repository Engine

- [x] Move object parsing, hashing, trees, commits, tags, refs, packs, deltas,
  revision resolution, and reachable-object traversal into `repository`.
- [x] Convert CLI-oriented methods into typed operations and results.
- [x] Move pack index and thin-pack behavior with focused tests.
- [x] Move archive/diff primitives needed by other Go consumers.
- [x] Keep local worktree porcelain separate; do not force it into the remote
  repository API in this phase.
- [x] Add examples for opening filesystem, S3, and GCS repositories.
- [x] Add fuzz tests for object, pkt-line, pack index, delta, and malformed ref
  parsing where practical.

Exit criteria:

- The same repository engine serves all five repository modes.
- `repository` imports only `store`, standard library, and narrowly justified
  Git-specific dependencies.
- Native Git interoperability tests pass for clone/fetch/push/branch/tag and
  thin packs.

### Phase 5: Extract Remote Broker Client And Capability Store

- [x] Move signed HTTP request handling into `broker/client`.
- [x] Move endpoint methods from CLI/web call sites into typed client methods.
- [x] Move `brokerGitStore` out of `web.go` into `broker/capability`.
- [x] Move STS and signed-URL capability execution into that package.
- [x] Inject HTTP clients, clocks, nonce sources, and signers.
- [x] Add compatibility tests against old supported broker responses.
- [x] Add cancellation, timeout, and safe retry tests.
- [x] Ensure ref listing and updates use broker methods, not raw storage refs.

Exit criteria:

- CLI, SSH transport, remote helper, and web UI share one broker client.
- No broker HTTP or capability implementation remains in `web.go`.
- No normal control flow depends on parsing broker error strings.

### Phase 6: Extract Local Broker

- [x] Replace endpoint-switch internals with direct service methods.
- [x] Move local metadata models to `protocol` or private local-broker state
  types as appropriate.
- [x] Move local state persistence, locks, leases, and reconciliation into
  `broker/local`.
- [x] Define a storage resolver mapping repository metadata to filesystem, S3,
  or GCS stores.
- [x] Preserve `.bucketgit/broker-state/` compatibility with migration tests.
- [x] Preserve issue/board/PR/protection behavior.
- [x] Add optional `http.Handler` adapter implementing the broker protocol for
  conformance and embedding.
- [x] Keep `local://` parsing and global-profile lookup in `internal/config`.
- [x] Remove any remaining assumption that the local broker is a long-lived
  HTTP server.

Exit criteria:

- Existing local repositories open without conversion.
- File/S3/GCS-backed local brokers pass the same broker behavior suite.
- Concurrent local-broker instances detect stale state and ref conflicts.
- Local broker can be embedded by another Go process without CLI globals.

### Phase 7: Extract Git Transport

- [x] Move pkt-line helpers and advertised capabilities into `transport`.
- [x] Move upload-pack and receive-pack services.
- [x] Move remote-helper protocol support.
- [x] Inject authorization/ref-update interfaces into receive-pack.
- [x] Remove transport dependencies on global configuration and `os.Stdin` /
  `os.Stdout`.
- [x] Keep SSH command parsing in the CLI adapter; pass typed service requests
  into `transport`.

Exit criteria:

- Another Go process can serve a BucketGit repository over upload-pack and
  receive-pack using only public packages.
- `git-remote-bgit`, `core.sshCommand`, and integration tests use the same
  transport implementation.

### Phase 8: Segment CLI, Setup, Identity, And Web

- [x] Move global/repository config into `internal/config`.
- [x] Move SSH key discovery/signing selection into `internal/identity`, while
  retaining public signer interfaces in `protocol` or `broker/client`.
- [x] Move AWS/GCP profile discovery, deployment command construction, source
  asset materialization, and retry primitives into `internal/setup`; keep the
  end-user deployment flow in `internal/app`.
- [x] Move global command parsing, help output, and shared terminal
  presentation into `internal/cli`; keep feature-specific dialogs in
  `internal/app`.
- [x] Move web routing, assets, CSRF validation, mutation policy, and event
  infrastructure into `internal/web`; keep repository-specific handler
  adapters in `internal/app`.
- [x] Move executable entry point to `cmd/bgit`.
- [x] Reduce root-level Go files to public packages, docs, and compatibility
  adapters scheduled for removal.

Exit criteria:

- `cmd/bgit` is a minimal entry point; `internal/app` owns composition and
  feature-specific presentation.
- Provider, repository, broker, and transport logic can be tested without
  invoking CLI commands.
- `setup.go`, `web.go`, `broker_commands.go`, and `main.go` no longer contain
  reusable protocol/storage/Git implementations.

### Phase 9: Public SDK Hardening

- [x] Add package documentation and runnable examples.
- [x] Add a top-level SDK overview documenting supported compositions.
- [x] Add examples for:
  - filesystem repository;
  - S3 repository;
  - GCS repository;
  - remote AWS/GCP broker client;
  - in-process broker;
  - upload-pack/receive-pack server;
  - custom object store implementation.
- [x] Document concurrency, credential, and authorization responsibilities.
- [x] Run `go vet`, `staticcheck`, race tests, fuzz smoke tests, and API review.
- [x] Generate an exported API inventory and review every symbol.
- [x] Remove or deprecate migration aliases.
- [ ] Publish the first explicitly supported SDK release.

Exit criteria:

- A separate Go project can import the packages from a tagged release.
- Examples compile against that tag.
- No example needs internal packages or CLI subprocesses.
- Public API ownership and compatibility policy are documented.

## Test Architecture

### Store contract suite

Provide a reusable test package that accepts a store factory. Run it against:

- Filesystem store on every platform.
- S3 fake client in unit tests.
- GCS fake client/emulator in unit tests.
- Broker capability store using a fake broker.
- Real S3/GCS only in opt-in smoke tests.

### Repository conformance suite

For every backend composition:

- Empty repository behavior.
- Initial push and clone.
- Fetch and pull.
- Branch creation/deletion.
- Tags and annotated tags.
- Fast-forward and rejected non-fast-forward push.
- Forced updates where policy allows.
- Packed and loose objects.
- Thin packs with existing bases.
- Large files and multiple packs.
- Concurrent ref updates and stale CAS.

### Broker conformance suite

Run the same behavioral cases against:

- In-process Go local broker.
- GCP JavaScript broker test harness.
- AWS embedded JavaScript broker test harness.
- Optional deployed AWS/GCP brokers in smoke tests.

Cover:

- Signature v2 and replay rejection.
- Repository visibility and roles.
- Object path capability allow/deny cases.
- Ref listing and CAS.
- Branch protection and PR merge paths.
- Users, teams, invites, ownership transfer, and key types.
- Issues, board ordering/archive state, PRs, CI records, and profiles.
- Materializer token rotation and old-token rejection.

### Compatibility fixtures

Keep serialized fixtures for at least the oldest supported broker version and
current version. Test unknown fields, missing optional fields, unknown
endpoints, and scoped compatibility warnings.

### Platform matrix

Run unit and local integration suites on:

- Linux amd64 and arm64 build targets.
- macOS amd64 and arm64 build targets.
- Windows amd64 and arm64 build targets.

Real cloud smoke tests can remain operator-run, but the package contract and
fake-provider suites must run in normal CI.

## CI Changes

- Add package-level jobs so failures identify `protocol`, `store`,
  `repository`, `broker`, `transport`, or CLI ownership.
- Run `go test -race` for provider-neutral packages on Linux.
- Compile all examples.
- Run JavaScript syntax and protocol fixture tests for both broker runtimes.
- Retain the existing full `go test ./...` and local broker integration suites.
- Add module/API checks before release.
- Keep real cloud smoke tests manual and credential-gated.

## Documentation Deliverables

- `spec/broker-v2.md`: wire protocol and security contract.
- Package docs for every exported package.
- SDK overview with the five supported compositions.
- Migration guide for code currently copying bgit internals.
- Custom store example.
- Custom broker-client example.
- Security guide explaining the difference between raw storage access and
  broker-authorized access.
- Version compatibility table for SDK, CLI, and broker protocol.

## Release And Rollout Plan

1. Merge each phase independently with no user-visible behavior change unless
   explicitly documented.
2. Publish normal `bgit` patch/minor releases during extraction.
3. Treat the generated `API.md` surface as the v1 compatibility contract from
   the first release containing the segmented module path.
4. Keep old internal adapters for at least one release after their replacement.
5. Deploy current broker assets before relying on newly added optional fields.
6. Do not require a broker upgrade for package-only refactors.
7. Announce the first supported SDK release only after external-example tests
   compile from a clean module.
8. Re-evaluate a separate `bucketgit-go` repository/module after two stable SDK
   releases. Split only if independent release cadence provides clear value.

## Risks And Mitigations

### Accidental wire incompatibility

Mitigation: protocol fixtures, cross-language signing vectors, captured JSON,
and old-broker compatibility tests before moving call sites.

### Over-exported API

Mitigation: keep implementation types unexported, use narrow interfaces,
publish as `v0`, and require an API review for every exported symbol.

### Circular dependencies

Mitigation: enforce the dependency graph in CI. Keep provider-neutral
interfaces in lower-level packages and composition in the CLI.

### Provider SDK leakage

Mitigation: provider-specific clients remain in provider packages. Generic
repository and broker APIs use BucketGit-owned interfaces/types.

### Security regression during refactor

Mitigation: preserve signed-request and object-path tests, test CAS at package
boundaries, and prohibit direct ref writes in broker-backed composition.

### Local state corruption or incompatible migration

Mitigation: fixture repositories from older releases, read/write round-trip
tests, backup-before-migration if a future format change becomes unavoidable,
and no format change in this segmentation effort.

### Refactor duration and merge conflicts

Mitigation: extract one boundary at a time, use adapters, avoid broad renames,
and require the full suite to pass after every phase.

## Suggested Pull Request Sequence

Keep pull requests reviewable and independently releasable:

1. Module path, ADR, baseline tests, and architecture checks.
2. Protocol constants/models without call-site migration.
3. V2 signer plus cross-language golden vectors.
4. Store interfaces and filesystem implementation.
5. S3 implementation.
6. GCS implementation.
7. Repository object/ref core.
8. Pack and revision engine.
9. Broker HTTP client.
10. Capability-backed store.
11. Local broker state/core service.
12. Local broker issues/board/PR/protection services.
13. Upload-pack and receive-pack transport.
14. Remote helper and SSH adapters.
15. Internal config/identity/setup packages.
16. Web and CLI segmentation.
17. Public examples, API review, and SDK release documentation.

Avoid PRs that simultaneously move files, rename all symbols, alter behavior,
and change protocol payloads.

## Definition Of Done

The segmentation is complete when:

- [x] `bgit` is composed from public Git/storage/broker/transport packages and
  internal CLI/setup/web packages.
- [x] The five repository modes use the same provider-neutral abstractions.
- [ ] Another Go module can import a tagged BucketGit release and open or serve
  repositories without invoking `bgit` as a subprocess.
- [x] Filesystem, S3, GCS, remote broker, and local broker implementations pass
  shared contract suites.
- [x] AWS and GCP JavaScript brokers pass the same protocol fixtures.
- [x] Existing repositories and local-broker state require no migration.
- [x] Existing supported brokers remain compatible.
- [x] Object capabilities, signature v2, branch protection, CAS, and local
  conflict safeguards retain direct test coverage.
- [x] The CLI, native Git SSH bridge, Git remote helper, web UI, and full
  integration suite pass on supported platforms.
- [ ] Public API documentation, examples, version policy, and security
  responsibilities are published.
