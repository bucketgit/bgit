# BucketGit Go SDK

BucketGit's module path is `github.com/bucketgit/bgit`. The CLI composes the
same packages exposed to other Go programs; cloud credentials and terminal UI
are not dependencies of the repository engine.

The SDK shares BucketGit's existing v1 release line. Consumers should pin a
tagged BucketGit release; incompatible public API changes require a future
major module path.
See [API.md](API.md) for the generated public surface,
[MIGRATING-SDK.md](MIGRATING-SDK.md) for migration guidance, and
[SECURITY-SDK.md](SECURITY-SDK.md) for authorization boundaries.

## Compatibility

| Component | Compatibility contract |
|---|---|
| Go SDK | v1 from the first segmented release; pin a normal `bgit` release |
| CLI configuration | existing global and repository configuration remains readable |
| local-broker state | `.bucketgit/broker-state/` remains readable without conversion |
| broker wire protocol | v2 signatures and additive JSON fields; protocol version is independent of module version |
| deployed brokers | supported older responses and missing optional endpoints retain compatibility handling |

## Packages

- `protocol`: broker JSON models, constants, typed errors, and v2 request
  signature canonicalization and replay-field validation.
- `store`: provider-neutral object and ref interfaces, path validation, fallback
  readers, ref parsing, and shared errors.
- `store/fs`: atomic filesystem object storage and cross-process ref CAS.
- `store/s3`: injected-client or standard AWS credential-chain S3 storage with
  conditional ref updates.
- `store/gcs`: injected-client or ADC GCS storage with generation-matched ref
  updates.
- `repository`: SHA-1 Git objects, refs, commits, trees, tags, pack indexes,
  deltas, reachability, ancestry, and merge-base operations.
- `broker/client`: signed broker HTTP requests and typed endpoint methods.
- `broker/capability`: broker-authorized object access through S3 STS, GCS
  signed URLs, or local capabilities, including supported legacy reads.
- `broker/local`: in-process local broker persistence, scoped repository stores,
  ref CAS, and issue/task-board services.
- `transport`: pkt-line framing, advertised capabilities, upload-pack,
  receive-pack, and Git remote-helper protocol handling.

## Filesystem Repository

```go
objects, err := fsstore.New("/srv/git/demo.git")
if err != nil {
    return err
}
repo := repository.Open(objects, objects)
head, err := repo.Resolve(ctx, "main")
```

## S3 Repository

Use `s3store.New` when the application already owns an AWS SDK client. Use
`s3store.Load` for the standard AWS credential chain:

```go
objects, err := s3store.Load(ctx, s3store.Options{
    Bucket:  "123456789012-demo",
    Profile: "work",
    Region:  "eu-west-1",
})
repo := repository.Open(objects, objects)
```

Bucket creation and IAM policy management are provisioning operations and are
not performed by store constructors.

## GCS Repository

```go
objects, err := gcsstore.Load(ctx, gcsstore.Options{Bucket: "project-demo"})
if err != nil {
    return err
}
defer objects.Close()
repo := repository.Open(objects, objects)
```

`gcsstore.New` accepts an existing `*storage.Client` and never closes it.

## Remote Broker Repository

Create a `broker/client.Client` with an injected signature provider, then pass
it to `capability.New`. The capability store performs object I/O while ref
updates remain broker-authorized CAS operations.

```go
client, err := brokerclient.New(brokerURL, brokerclient.Options{
    HTTPClient: httpClient,
    Signatures: signer,
})
objects, err := capability.New(client, identity, capability.Options{
    HTTPClient: httpClient,
})
repo := repository.Open(objects, objects)
```

Applications are responsible for selecting trusted SSH identities, protecting
private keys, applying HTTP timeouts, and deciding whether unsigned fallback is
allowed for a legacy broker.

## Local Broker

```go
broker, err := local.New(local.Options{Root: stateRoot, Resolver: resolver})
scoped := broker.Repository(protocol.Repository{
    Provider: "file", Bucket: "demo.git", Logical: "demo.git",
})
repo := repository.Open(scoped, scoped)
```

A resolver may return S3 or GCS stores for cloud-backed local repositories. The
repository's `.bucketgit/broker-state/` namespace is authoritative; callers
must not rewrite it outside broker APIs. Resolver stores used for mutable
broker metadata must implement `store.CompareAndSwapper`; the built-in
filesystem, S3, and GCS stores use expiring file leases, ETags, and generations
respectively to reject stale metadata writes.

## Transport

`transport.ServeUploadPack` is read-only. `transport.ServeReceivePack` requires
a `ReceiveStore`, validates and ingests the pack, checks fast-forward updates,
and performs ref CAS. Authorization must happen before invoking either service.

`transport.ServeRemoteHelper` handles the helper protocol but delegates address
resolution and service authorization through its `Resolver` interface.

## Concurrency And Errors

- Store paths are canonical, relative slash-separated paths.
- Missing objects use `fs.ErrNotExist`.
- Conditional ref failures use `store.ErrConflict`.
- Conditional metadata failures also use `store.ErrConflict`; reload broker
  state before retrying a mutation.
- Broker failures expose `protocol.BrokerError` and support `errors.Is` for
  conflict, authorization, and unsupported-operation classes.
- Repository instances own mutable caches and are safe for concurrent reads.
- Repository decoding defaults to a 128 MiB decompressed object ceiling;
  applications that intentionally store larger Git objects can opt in with
  `repository.WithMaxObjectSize` after assessing their memory budget.
- Receive-pack rejects encoded packs larger than 512 MiB and delta-expanded
  objects larger than the repository default before allocating their declared
  size.
- GCS clients returned by `Load` must be closed; injected clients remain owned
  by the caller.
