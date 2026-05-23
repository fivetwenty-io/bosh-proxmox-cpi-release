# PVE Host Tuning for BOSH Workloads

Operator-side knobs on the PVE node itself. None of these are required for the CPI to function — the defaults work for small deploys. Tune them once your BOSH director starts driving sustained concurrent CPI traffic (typical thresholds called out per section).

The CPI's [per-storage lock retry](pve-storage-locking.md) absorbs short bursts on its own. Host tuning matters when the contention is structural: too few API workers to accept the calls, or storage backends that serialise harder than the retry budget can ride out.

Apply every change on every node in the cluster. The PVE config files under `/etc/default/` are node-local, not replicated via pmxcfs.

## 1. Raise `pvedaemon` Worker Count

`pvedaemon` listens on `127.0.0.1:85` and handles every internal API call (`pveproxy` forwards to it, as do `qm`, `pvesm`, `pct`, and friends). With the default of 3 workers, a director worker pool of 8+ will queue calls behind the daemon under load — visible as slow `create_vm` / `create_disk` calls even when no storage lock is involved, and as HTTP 596 / auth-ticket EOFs when a worker recycles mid-burst (see [PVE Transient Transport Faults](pve-transient-transport.md)).

Edit `/etc/default/pvedaemon`:

```sh
MAX_WORKERS=8
```

Valid range: 1–127. Default: 3.

Restart:

```sh
systemctl restart pvedaemon
```

Verify:

```sh
ps -ef | grep '[p]vedaemon worker' | wc -l
```

Should print the number you set.

**Sizing guidance:**

- Small lab / single-deploy director: leave at default (3).

- Cloud Foundry-class deploy (10–40 concurrent `create_vm`): 6–8.

- Multiple directors against one PVE cluster: 10–16.

- Cap at the node's vCPU count. Each worker is a Perl process; idle cost is small (~30 MB RSS), but a saturated worker burns a core.

## 2. Raise `pveproxy` Worker Count

`pveproxy` is the HTTPS frontend on port 8006 — every CPI HTTP request lands here before being forwarded to `pvedaemon`. Same default (3), same setting name.

Edit `/etc/default/pveproxy`:

```sh
MAX_WORKERS=8
```

Restart:

```sh
systemctl restart pveproxy.service spiceproxy.service
```

Restarting `pveproxy` drops in-flight HTTPS connections, including web UI sessions and VNC consoles. Do it during a maintenance window or accept a few seconds of UI blip; CPI calls will retry on transport errors.

Verify:

```sh
ps -ef | grep '[p]veproxy worker' | wc -l
```

**Sizing guidance:** match `pvedaemon` MAX_WORKERS. Set both together — bumping one without the other moves the bottleneck rather than removing it.

## 3. Optional: Lengthen `pveproxy` Idle Timeout

Default request idle timeout on `pveproxy` is short enough that long-running task waits (stemcell import on slow storage, large persistent disk resize) occasionally surface as `Connection reset by peer` in CPI logs. The CPI retries on these, but if your logs show frequent transport-level resets:

Edit `/etc/default/pveproxy`:

```sh
TIMEOUT=1800
```

Restart `pveproxy` as above.

This is uncommon. Only set it if you see actual reset noise — over-long timeouts mask genuine hung tasks.

## 4. Split Stemcell and VM Storages

The single largest source of CPI latency under concurrent deploys is the per-storage lockfile (`/var/lock/pve-manager/pve-storage-<name>`). Stemcell import, root-disk resize, and persistent-disk allocation all contend for the same lock when they live on the same storage pool.

Put stemcells on one storage and VM root disks on another, both shared across the cluster. The CPI honours this via two distinct settings:

- `pve.stemcell_storage`

- `pve.vm_storage`

See [Configuration — Stemcell Storage](configuration.md#stemcell-storage) and [PVE Storage Locking](pve-storage-locking.md) for the full picture.

This is the single highest-leverage change for any deploy larger than a few VMs. Worker tuning helps the API plane; storage splitting helps the data plane, and the data plane is usually where deploys stall.

## 5. Storage Backend Throughput

The per-storage lock is held for the duration of the underlying I/O. A 4 GB stemcell import on a slow NFS mount holds the lock for tens of seconds; on a local NVMe or fast Ceph pool, it's gone in seconds. Two practical knobs:

- **Match storage type to workload.** Stemcell storage benefits most from sequential read throughput (qcow2 copy-out). VM storage benefits from random I/O for the resize and runtime. Persistent disks vary by job.

- **Watch `iostat -xz 2` during a deploy.** A storage backend pinned at 100 % `%util` is the bottleneck regardless of API worker count.

## 6. TCP Keepalives for Long-Lived BOSH Agent → Director Connections

Every BOSH-managed VM holds a long-lived TLS+TCP connection from its
bosh-agent to the director's NATS server on port 4222. Idle stretches
between deploy phases can exceed the host's default TCP keepalive
window (Linux defaults: `tcp_keepalive_time=7200` s before the first
keepalive probe is sent), at which point a stateful bridge,
conntrack entry, or upstream router may silently drop the half-open
flow. The next packet from either side gets RST'd, the agent reconnects,
and any RPC the director published during the gap is lost — surfacing
as `Timed out sending '<verb>' to instance: <inst>'`.

This is a *path-level* failure, distinct from the NATS-server
`ping_interval` (handled at the director itself via
`manifests/bosh/nats-tuning.yml`). Tighter host-level keepalives
ensure the TCP flow keeps reannouncing itself to anything tracking
it, well before any reasonable idle-drop timer fires.

Apply on every PVE node:

```sh
cat >/etc/sysctl.d/60-bosh-nats-keepalive.conf <<'EOF'
# Send the first TCP keepalive after 60 s idle (default 7200).
net.ipv4.tcp_keepalive_time = 60

# Then a probe every 15 s (default 75).
net.ipv4.tcp_keepalive_intvl = 15

# Drop the connection after 4 unanswered probes (default 9).
net.ipv4.tcp_keepalive_probes = 4
EOF
sysctl --system
```

If a Linux bridge or `iptables`/`nftables` rule on the host is doing
conntrack on the agent → director flow, also confirm
`net.netfilter.nf_conntrack_tcp_timeout_established` is well above
the cumulative keepalive window (default 432000 s — no action needed
unless an operator lowered it).

These knobs do not affect the agent's own NATS Go client (which has
its own application-level PING/PONG). They only keep network
elements between the VM and the director from concluding the flow
is dead during quiet periods.

## 7. Verify Together After Tuning

After applying sections 1–3:

```sh
grep -E '^(MAX_WORKERS|TIMEOUT)' /etc/default/pvedaemon /etc/default/pveproxy
ps -ef | grep -E '[p]ve(daemon|proxy) worker' | wc -l
systemctl status pvedaemon pveproxy --no-pager | grep -E '(Active|Main PID)'
```

Then run a BOSH deploy that previously showed contention and watch the CPI log for `pve: storage lock timeout, retrying` lines. If retry counts drop and `attempt` values stay below 3, the tuning landed. If retries are still climbing past 5, contention is structural — work on sections 4 and 5.

After applying section 6:

```sh
sysctl net.ipv4.tcp_keepalive_time net.ipv4.tcp_keepalive_intvl net.ipv4.tcp_keepalive_probes
```

Then run a deploy long enough to traverse a quiet phase (cf-deployment package compile, for instance) and confirm `bosh vms` does not surface `unresponsive agent` for instances that have been idle.

## What Not to Tune

- **PVE storage lock timeout.** Not user-configurable. The 30 s ceiling is baked into `PVE::Storage::Plugin::lock_storage`. Raising it would require patching Perl modules and would survive neither upgrades nor support contracts.

- **`pvestatd` and `pvescheduler`.** These run on fixed cadences and don't expose worker counts. They're not on the CPI's hot path.

- **Kernel `vm.dirty_*` knobs.** Tempting for NFS-backed storage, but the per-storage lock serialises before the kernel sees the I/O, so these don't help BOSH-deploy contention. They do help bulk migrations and backups.

## References

- `pvedaemon(8)`: https://pve.proxmox.com/pve-docs/pvedaemon.8.html

- `pveproxy(8)`: https://pve.proxmox.com/pve-docs/pveproxy.8.html

- [PVE Storage Locking](pve-storage-locking.md) — the failure mode this tuning addresses.

- [PVE Settings Required](pve-settings.md) — prerequisites the CPI cannot start without (distinct from this tuning, which is optional).
