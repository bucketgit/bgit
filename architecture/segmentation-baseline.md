# Segmentation Verification Baseline

This record captures the behavior used to verify the package-segmentation
refactor. The migration does not intentionally alter CLI output, repository
layout, broker JSON, Git transport, or local-broker state.

## Verification

The segmented implementation passes:

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `staticcheck ./...`
- `node broker/test_support/signing_fixture_test.js`
- `go test ./...` from the independent `testsuite/sdk-consumer` module
- `./testsuite/run-local-broker.sh gcp` (25 integration files)
- `./testsuite/run-local-broker.sh aws` (25 integration files)
- cross-compilation for Darwin amd64/arm64, Linux amd64/arm64, and Windows
  amd64/arm64
- fuzz smoke tests for objects, refs, pack indexes, deltas, pkt-lines, and
  receive packs

## Behavioral Golden Coverage

CLI and protocol behavior is captured by the existing tests rather than copied
into a second snapshot format:

- internal application tests cover usage, clone, fetch, pull, push, PR, board, issue,
  CI, local-broker, SSH bridge, and web behavior;
- `testsuite/local` captures local porcelain behavior;
- `testsuite/aws` and `testsuite/gcp` execute identical broker scenarios;
- `spec/testdata/signing-v2.json` is the cross-language signature fixture;
- `testsuite/sdk-consumer` proves use from outside the BucketGit module.

The command list above is the release baseline. Any expected output change must
update its owning focused test and release notes.
