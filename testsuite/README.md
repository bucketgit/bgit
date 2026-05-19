# BucketGit Integration Test Suite

This suite exercises the built `bgit` binary against real local repositories and
local SQLite-backed broker runtimes. It intentionally lives outside the Go unit
tests because it starts broker servers, creates repositories, and runs full CLI
flows.

Run everything locally:

```bash
./testsuite/run.sh
```

Run one provider locally:

```bash
BGIT_TEST_PROVIDER=gcp ./testsuite/run.sh
BGIT_TEST_PROVIDER=aws ./testsuite/run.sh
```

Run a broker runtime directly:

```bash
./testsuite/run-local-broker.sh gcp
./testsuite/run-local-broker.sh aws
```

The local broker runner executes the real GCP Cloud Functions broker module or
the real AWS Lambda broker code extracted from the CloudFormation template. It
uses SQLite for broker metadata and local HTTP object capability URLs for Git
objects, so it does not require cloud credentials and never touches deployed
brokers.

Useful overrides:

```bash
BGIT_TEST_PROVIDER=gcp|aws|all
BGIT_TEST_RUN_ID=20260519092710
BGIT_TEST_LOCAL_BROKER_ROOT=/tmp/bgit-local-broker
BGIT_TEST_KEEP_ARTIFACTS=1
```

The suite creates throwaway worktrees under `testsuite/gcp/repo/`,
`testsuite/aws/repo/`, and `testsuite/local/`. Test SSH identities are under
`testsuite/sshkeys/`; these are generated fixtures only and must never be used
outside the test suite.

On success, the local broker runner removes its temporary broker root and the
worktrees for the provider it ran. On failure it keeps them for debugging. Set
`BGIT_TEST_KEEP_ARTIFACTS=1` to keep artifacts after successful runs too.
