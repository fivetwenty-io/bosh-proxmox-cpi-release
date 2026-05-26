# Light Stemcells

Light stemcells let operators reference an existing or remotely hosted qcow2 image
instead of transferring image bytes through BOSH on every deploy. Two modes are
available:

1. **Pre-uploaded** — operator places a qcow2 on PVE storage out-of-band and
   references it via `cloud_properties.image_id`.
2. **CPI-assisted fetch** — operator references a remote URL via
   `cloud_properties.image_url`; the CPI fetches it once and caches it in PVE
   storage, deduplicating on re-deploy.

Both modes produce a `light:<storage>:import/<file>` stemcell CID. The CPI treats
light CIDs as operator-managed: `bosh delete-stemcell` returns success without
removing the underlying qcow2 volume. To reclaim storage, use `pvesm free` or
the PVE UI directly (see [Operator caveats](#operator-caveats)).

## When to use

- **Air-gapped lab** — upload once via `pvesm` or the PVE Upload API; reuse across
  redeploys without touching the BOSH director upload path.
- **Multi-deployment infrastructure** — point multiple deployments at the same CPI-fetched
  image from a private mirror; dedup avoids redundant downloads.
- **Large stemcells** — avoid the director-to-CPI upload bottleneck when network
  bandwidth between the BOSH director and the PVE storage node is a constraint.

## Storage requirements

Light stemcells require **file-content** PVE storage. Block-only backends cannot
accept qcow2 uploads and are rejected.

| PVE storage type | Supported | Notes |
|---|---|---|
| `dir` | yes | Local directory; requires `cloud_properties.node` on multi-node clusters. |
| `nfs` | yes | Shared; no node pin required. |
| `cifs` | yes | Shared; no node pin required. |
| `cephfs` | yes | Shared; no node pin required. |
| `glusterfs` | yes | Shared; no node pin required. |
| `btrfs` | yes | Local; requires `cloud_properties.node` on multi-node clusters. |
| `lvm` | **no** | Block-only. |
| `lvmthin` | **no** | Block-only. |
| `zfspool` | **no** | Block-only. |
| `rbd` | **no** | Block-only (raw RBD images; qcow2 unsupported). |

**Policy by cluster shape:**

- **Single-node** — any file-content backend is accepted. `cloud_properties.node`
  is optional.
- **Multi-node + shared storage** (nfs, cifs, cephfs, glusterfs) — accepted without
  node pinning.
- **Multi-node + local storage** (dir, btrfs) — `cloud_properties.node` is required.
  Without it, the CPI cannot guarantee that the uploaded image and any VM that uses it
  land on the same node.

## Mode 1: Pre-uploaded

The operator uploads the qcow2 to PVE storage manually; the CPI confirms the file
is present and returns the light CID. No bytes flow through the CPI.

### Operator workflow

1. Upload the qcow2 to the target PVE storage. Two paths are available:

   ```bash
   # From a PVE host shell:
   pvesm upload <storage> /path/to/bosh-stemcell-ubuntu-jammy-1.438.qcow2 --content import
   ```

   Or use the PVE web UI: navigate to the storage, open the **Content** tab, and
   click **Upload**.

2. Note the resulting volid. The PVE storage browser displays it in the form
   `<storage>:import/<filename>`. Example:
   `nfs-stemcells:import/bosh-stemcell-ubuntu-jammy-1.438-a1b2c3d4.qcow2`

3. Author the stemcell tarball. The minimal `stemcell.MF` below references the
   pre-uploaded volume via `cloud_properties.image_id`:

   ```yaml
   name: bosh-proxmox-kvm-ubuntu-jammy-go_agent-light
   version: "1.438"
   api_version: 2
   operating_system: ubuntu-jammy
   stemcell_formats:
     - proxmox-kvm
   cloud_properties:
     image_id: "nfs-stemcells:import/bosh-stemcell-ubuntu-jammy-1.438-a1b2c3d4.qcow2"
     name: ubuntu-jammy
     version: "1.438"
     disk_format: qcow2
     os_type: l26
     disk: 10240
     # Required when storage is local-dir on a multi-node cluster:
     # node: pve-node1
   ```

   The `bosh repack-stemcell` command can inject or replace `cloud_properties` in an
   existing stemcell tarball when you do not want to author one from scratch.

4. Upload to the director:

   ```bash
   bosh upload-stemcell <light-tarball.tgz>
   ```

   The CPI validates that `image_id` resolves to a real volume on PVE and returns the
   light CID `light:nfs-stemcells:import/bosh-stemcell-ubuntu-jammy-1.438-a1b2c3d4.qcow2`.
   No image bytes are transferred.

5. Deploy normally. `bosh deploy` passes the light CID to `create_vm`, which strips
   the `light:` prefix and imports the image directly from PVE storage.

### Error messages

| Error | Cause | Fix |
|---|---|---|
| `image_id %q is not a valid storage volid` | `image_id` does not match `<storage>:import/<file>` | Correct the volid format. |
| `light stemcell image_id %q not found on storage %q node %q` | File not present on PVE. | Run `pvesm list <storage>` to confirm the upload landed. See [Troubleshooting](#troubleshooting) for the rescan note. |
| `storage %q is local on a multi-node cluster` | Local-dir storage on a cluster with no node pin. | Add `cloud_properties.node: <nodename>`. |
| `storage %q (type=%q) is block-only` | LVM, ZFS, or RBD storage chosen. | Switch to a file-content storage (nfs, dir, cephfs). |

## Mode 2: CPI-assisted fetch

The CPI fetches the qcow2 from a remote URL, streams it into PVE storage, and caches
it there. Subsequent `bosh upload-stemcell` calls for the same image skip the download
entirely.

### URL schemes

| Scheme | Format | Notes |
|---|---|---|
| `https://` | `https://host/path/to/stemcell.qcow2` | Basic or Bearer auth supported. |
| `s3://` | `s3://bucket/key` | AWS S3 or S3-compatible (MinIO, Cloudflare R2) with optional endpoint override. |
| `bosh+blobstore:` | `bosh+blobstore:<blob-id>` | BOSH Director blobstore via HTTP. |
| `oci://` | `oci://registry/repo:tag` | OCI artifact registry. |

### Credentials

Credentials can be supplied per-stemcell in `cloud_properties` or centrally in the CPI config.

#### Per-stemcell credentials

Set `cloud_properties.image_url_auth` in `stemcell.MF`. Per-stemcell auth overrides any
config-level defaults.

```yaml
cloud_properties:
  image_url: "https://artifactory.corp/stemcells/ubuntu-jammy-1.438.qcow2"
  image_url_auth:
    type: bearer
    bearer_token: "<token>"
```

#### Centralized credentials (CPI config)

Add entries to `pve.fetch_credential_defaults` in the CPI job properties. Each entry maps
a URL prefix to an auth payload. When multiple entries match, the longest prefix wins.

```yaml
properties:
  pve:
    fetch_credential_defaults:
      - url_prefix: "https://artifactory.corp/"
        auth:
          type: basic
          username: robot
          password: "<password>"
      - url_prefix: "s3://stemcells-mirror/"
        auth:
          type: s3
          access_key_id: "AKIAEXAMPLE"
          secret_access_key: "<secret>"
          endpoint: "https://s3.lab.local"
```

Per-stemcell `image_url_auth` takes priority over config defaults. Among config defaults,
the entry with the longest matching `url_prefix` wins.

### Auth payload schemas

Each auth payload requires a `type` field that selects the scheme.

**`basic`**
```yaml
type: basic
username: "<username>"
password: "<password>"
```

**`bearer`**
```yaml
type: bearer
bearer_token: "<token>"
```

**`s3`**
```yaml
type: s3
access_key_id: "<access-key>"
secret_access_key: "<secret>"
# Optional — set for S3-compatible endpoints:
endpoint: "https://minio.lab.local"
region: "us-east-1"
force_path_style: true
```
When `endpoint` is set, path-style addressing is enabled automatically (needed for
MinIO and most S3-compatible servers).

**`oci`**
```yaml
type: oci
username: "<registry-user>"   # omit for anonymous pull
password: "<registry-token>"
```

**`blobstore`**
```yaml
type: blobstore
endpoint: "https://blobstore.director.internal:25251"
username: "<user>"    # optional
password: "<pass>"    # optional
```

### Dedup

The CPI deduplicates fetched images by `(name, version, sha8)` filename pattern. When
`bosh upload-stemcell` is called a second time for the same image:

1. The CPI scans PVE storage for any import volume with a matching `name`+`version` prefix.
2. If a match is found, it returns the existing light CID without fetching the remote URL.
3. After the fetch, an exact SHA-256 check provides a second dedup gate.

The result is that a re-deploy or second `bosh upload-stemcell` for an already-cached
image completes in milliseconds.

### Error messages

| Error | Cause | Fix |
|---|---|---|
| `unsupported URL scheme in %q` | `image_url` scheme not in the supported list. | Use `https://`, `s3://`, `bosh+blobstore:`, or `oci://`. |
| `fetch %q: HTTP 401` | Credentials missing or wrong. | Check `image_url_auth` or `fetch_credential_defaults`. |
| `resolve credentials: ... missing required field` | Malformed auth payload. | Check `type` field and required keys for the scheme. |
| `storage %q (type=%q) is block-only` | Fetch target storage is LVM/ZFS/RBD. | Switch `stemcell_storage` to a file-content backend. |
| `storage %q is local on a multi-node cluster` | Local storage, no node pin. | Add `cloud_properties.node`. |

## Operator caveats

Light stemcells are **operator-managed**. `bosh delete-stemcell` on a light CID
returns success but does NOT remove the underlying qcow2 volume — the CPI
recognizes the `light:` prefix and no-ops the delete with an INFO log entry.
This keeps `bosh delete-stemcell` safe to run while leaving image lifecycle
management entirely under operator control.

To free the storage when the image is no longer needed:

```bash
# From a PVE host shell:
pvesm free <storage>:import/<filename>

# Example:
pvesm free nfs-stemcells:import/bosh-stemcell-ubuntu-jammy-1.438-a1b2c3d4.qcow2
```

Alternatively, open the PVE web UI, navigate to the storage's **Content** tab, select
the volume, and click **Remove**.

## Troubleshooting

**Existence check fails immediately after `pvesm upload`**

PVE caches storage content listings. The CPI queries that listing during the
pre-uploaded existence check. If `pvesm list <storage>` does not yet show the file,
run `pvesm rescan` on the PVE node or wait approximately 10 seconds and retry
`bosh upload-stemcell`.

**Partial upload artifact left after a fetch failure**

Look for `*.partial` files in the storage content list:

```bash
pvesm list <storage> --content import
```

Delete any partial file before retrying:

```bash
pvesm free <storage>:import/<partial-filename>
```

**CPI version downgrade after light stemcells were used**

A CPI version that predates light-stemcell support cannot parse `light:` CIDs. Before
downgrading, re-upload all light stemcells using the normal (heavy) path so the
director holds non-light CIDs. Do not downgrade while any deployment references a
`light:` CID.

**`bosh delete-stemcell` returns success but the qcow2 is still present**

This is expected behavior for light stemcell CIDs. The CPI deliberately no-ops
the delete to preserve operator ownership of the image. Manage the image
lifecycle via `pvesm free` or the PVE UI as described above.

## See also

- [Persistent disks](persistent-disks.md) — storage backend classification and
  cloud-properties for disk pools.
- [ConfigDrive layout](configdrive.md) — ISO delivery for agent bootstrap.
- [BOSH light-stemcell convention](https://bosh.io/docs/stemcell/) — the equivalent
  feature in AWS, OpenStack, and GCP CPIs uses the same operator workflow.
