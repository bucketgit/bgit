# BucketGit SDK Security Boundaries

The storage packages provide scoped object access. They do not authenticate a
user, evaluate roles, enforce branch protection, or authorize a ref update.

- Use `broker/client` plus `broker/capability` for deployed-broker repositories.
  Keep final ref updates on the broker endpoint.
- Use `broker/local` when an application needs the in-process local control
  plane, persisted conflict safeguards, and repository-scoped metadata.
- Treat S3/GCS credentials, SSH signers, signed URLs, and capability responses
  as secrets. Do not persist them in Git configuration or ordinary logs.
- Inject bounded HTTP clients and honor context cancellation.
- Keep v2 timestamp and nonce validation enabled. An unsigned compatibility
  fallback is an explicit caller decision for a trusted legacy broker only.
- Do not bypass `store.ValidatePath`; filesystem stores additionally reject
  symlink escapes.
- Handle `store.ErrConflict` as a failed compare-and-swap and reconcile before
  retrying. This applies to refs and local-broker metadata; blind replacement
  loses concurrent updates.
- Repository decoding defaults to a 128 MiB expanded-object limit and transport
  receive-pack limits encoded input to 512 MiB. Raising either limit increases
  memory-exhaustion exposure.

Applications embedding upload-pack or receive-pack must authenticate and
authorize the request before invoking the transport service. `transport` owns
Git framing and update correctness, not user identity or repository policy.
