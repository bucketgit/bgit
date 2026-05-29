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

Real cloud local-broker smoke tests are opt-in because they create and delete
real buckets. They exercise the in-process local broker with AWS S3 and GCS as
the object backing store:

```bash
BGIT=./bgit testsuite/real/local_broker_cloud.sh all
BGIT=./bgit testsuite/real/local_broker_cloud.sh s3
BGIT=./bgit testsuite/real/local_broker_cloud.sh gs
```

Defaults are `charlieroot/eu-west-1` for AWS and `riedel/europe-west1` for GCP.
Override them with:

```bash
BGIT_REAL_AWS_PROFILE=charlieroot
BGIT_REAL_AWS_REGION=eu-west-1
BGIT_REAL_GCP_PROFILE=riedel
BGIT_REAL_GCP_REGION=europe-west1
BGIT_REAL_WORK_ROOT=/tmp/bgit-real-local-broker
```

On success, the local broker runner removes its temporary broker root and the
worktrees for the provider it ran. On failure it keeps them for debugging. Set
`BGIT_TEST_KEEP_ARTIFACTS=1` to keep artifacts after successful runs too.
