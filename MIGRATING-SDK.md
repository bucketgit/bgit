# Migrating To The BucketGit Go SDK

Code that previously copied BucketGit internals should import the owning public
package from `github.com/bucketgit/bgit` instead.

| Previous responsibility | Public package |
|---|---|
| raw filesystem/S3/GCS object access | `store/fs`, `store/s3`, `store/gcs` |
| Git objects, refs, revisions, packs, diffs | `repository` |
| broker request/response JSON | `protocol` |
| signed remote broker calls | `broker/client` |
| broker-issued object capabilities | `broker/capability` |
| in-process local control plane | `broker/local` |
| upload-pack, receive-pack, remote helper | `transport` |

Do not import `internal/*`; those packages implement the `bgit` application and
may change without SDK compatibility guarantees. Do not use a raw store to
replace broker authorization. For a deployed broker, object access uses
`broker/capability` and final ref mutation remains a signed broker CAS call.

Pin a tagged release in `go.mod`. `API.md` records the v1 public surface;
breaking changes require a future major module path. Existing CLI configuration and local-broker state
are application concerns and are intentionally not exposed as public SDK types.
