# Provisioning the storage allocation journal

Set-based allocation needs a private directory on durable storage. Set `pve.storage_allocation_journal_dir` to its absolute path. The CPI never chooses a temporary fallback or moves a journal when the path changes.

On the Director, the `pve_cpi` job's pre-start runs the local provisioning command for the `vcap` execution user. For example, `/var/vcap/store/pve_cpi/allocations` keeps the journal on the Director's persistent disk. The command creates missing directories with mode `0700`. An existing journal directory must already belong to `vcap` and its primary group, with mode `0700`. The command rejects symlinks and unsafe paths without changing their permissions or ownership.

If Director workers run under BPM, expose that same directory through `director.cpi_additional_volumes` with `writable: true` and `mount_only: true`. The [Director worker template](https://github.com/cloudfoundry/bosh/blob/main/jobs/director/templates/bpm.yml) adds these volumes to the worker's filesystem. BPM otherwise gives each job its own persistent directory, as described in the [BPM runtime documentation](https://bosh.io/docs/bpm/runtime/).

For `bosh create-env`, choose a durable host directory outside `~/.bosh/installations`, the rendered CPI job, and temporary directories. Run the following command as the same user that runs `bosh create-env`, using the CPI binary built for that host.

```sh
/path/to/cpi provision-journal --directory /absolute/durable/path/allocations
```

Set the create-env CPI's `storage_allocation_journal_dir` to that exact path. The host journal and the Director journal have separate paths and lifetimes. Keep the host journal through subsequent create-env updates and teardown, and include both journals in the corresponding backup and recovery procedures. Do not reuse an old namespace after losing or restoring its journal without completing the historical allocation audit.

To provision from a rendered CPI configuration, use the following command.

```sh
/path/to/cpi provision-journal --config /path/to/cpi.json
```

Provisioning makes no PVE requests and does not enroll a namespace or claim cluster authority. [Enrollment](storage-journal-operations.md) requires an explicit historical audit, previous-writer fencing, and confirmation that previous remote tasks have settled. The normal CPI wrapper does not provision or require a journal for unrelated existing-CID operations.

## The endpoint cache beside the journal

The same command also provisions an `endpoint-cache` directory beside the journal directory, never inside it, with the same owner and mode. For a journal at `/var/vcap/store/pve_cpi/allocations`, the cache directory is `/var/vcap/store/pve_cpi/endpoint-cache`. The CPI never creates this directory at run time, and it refuses one that is not owned by the user that runs the CPI, has any group or other bits, or is a symlink. If provisioning cannot create or accept the directory, it prints one warning on standard error and still succeeds, so the Director starts. A missing directory sends the CPI to a cache in its temporary directory. A directory the CPI refuses stops the search, so the CPI runs without a cache and discovery is off for that call. Under BPM, list the journal's parent directory in `director.cpi_additional_volumes` with `writable: true` and `mount_only: true` so that the workers see both directories. [Endpoint Failover and Discovery](endpoint-failover.md#beside-the-journal) explains how the CPI chooses the cache directory.
