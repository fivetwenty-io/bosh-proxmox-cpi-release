# CPI Lifecycle Harness

The CPI lifecycle harness (`scripts/lifecycle`) exercises every CPI method a BOSH Director calls during a deploy, redeploy, and teardown cycle, end to end against a live Proxmox VE cluster. We run it with `./scripts/test integration lifecycle`, which adds one pass per configured disk storage pool, network mode, and snapshot-detach mode; the [certification hub](../index.md) documents the setup and invocation.

## Run history

No runs recorded yet.
