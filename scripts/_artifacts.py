"""Shared helper for the cpi-artifacts VM + RustFS S3 blobstore.

A `cpi-artifacts` VM runs RustFS (an Apache-2.0, S3-compatible, single-binary
object store) on the active env's isolated SDN network, parallel to the
create-env BOSH director. It gives the compiled-release pipelines (`scripts/bosh
compile-releases`, `scripts/cf precompile-releases`) and the CPI a local S3
endpoint on the same network as the director, instead of a remote store.

This module is the single source of truth consulted by every consumer:

  * `scripts/artifacts`   drives the VM lifecycle (bootstrap/teardown/buckets).
  * `scripts/bosh`        swaps its compiled-release store to RustFS when online.
  * `scripts/cf`          same, for the CF precompile pipeline.
  * `scripts/_integration` injects S3 fetch credentials into the CPI config.

The governing rule is fail-open: when the artifacts VM is absent or
unreachable, every helper that gates behavior returns "nothing applied" and the
existing flows run exactly as before. `artifacts_online()` returns False on any
error so a probe failure never blocks a deploy.

Pure helpers (config resolution, credential rule, endpoint/URL building, the
install-script and qm-create renderers, state I/O) are unit-tested in
`_artifacts_test.py`. The socket probe and the orchestration that shells out to
SSH live in `scripts/artifacts`.

Stdlib only — `scripts/lifecycle` and friends import this module under bare uv
with no pip dependencies, so it must not pull anything in.
"""

from __future__ import annotations

import dataclasses
import json
import secrets
import shlex
import socket
from pathlib import Path
from typing import Callable

# StoreConfig parity: the compiled-release pipelines already know how to upload
# to and reference an S3-compatible store via this shape.
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _compiled  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parent.parent
ENVS_DIR = REPO_ROOT / "manifests" / "envs"

# --- Defaults (all overridable via env var, then vars.yml, then this) ------- #
DEFAULT_IP = "172.31.0.11"  # slot after the director's .10, inside cpitest_reserved
DEFAULT_CORES = 2
DEFAULT_MEMORY_MIB = 4096  # 4 GiB
DEFAULT_DISK_GIB = 100
DEFAULT_S3_PORT = 9000
DEFAULT_CONSOLE_PORT = 9001
DEFAULT_RUSTFS_VERSION = "1.0.0-beta.3"
DEFAULT_TLS_MODE = "disabled"  # private SNAT-only lab net; "self-signed" to override
DEFAULT_BUCKETS = ("pve-cpi-bosh", "pve-cpi-cf")
DEFAULT_CIUSER = "ubuntu"
DEFAULT_REGION = "us-east-1"
# Ubuntu 24.04 noble cloud image (ships cloud-init + qemu-guest-agent).
DEFAULT_IMAGE_URL = (
    "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
)
VALID_TLS_MODES = ("disabled", "self-signed")
VM_NAME = "cpi-artifacts"
VM_TAG = "cpi-artifacts"  # PVE config tag for discovery-by-tag fallback

TRUTHY = ("1", "true", "yes", "on")


def _rustfs_url(version: str) -> str:
    """Default download URL for the musl static RustFS build of ``version``."""
    return (
        "https://dl.rustfs.com/artifacts/rustfs/release/"
        f"rustfs-linux-x86_64-musl-v{version}.zip"
    )


# --------------------------------------------------------------------------- #
# Config resolution (pure)
# --------------------------------------------------------------------------- #

Getter = Callable[[str], str]


def _truthy(value: str) -> bool:
    return value.strip().lower() in TRUTHY


def resolve_str(getenv: Getter, getvar: Getter, env_key: str, var_key: str, default: str) -> str:
    """Env var wins, then the vars.yml key, then the built-in default."""
    val = (getenv(env_key) or "").strip()
    if val:
        return val
    val = (getvar(var_key) or "").strip()
    if val:
        return val
    return default


def resolve_int(getenv: Getter, getvar: Getter, env_key: str, var_key: str, default: int) -> int:
    raw = resolve_str(getenv, getvar, env_key, var_key, str(default))
    try:
        return int(raw)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{env_key}/{var_key}: expected an integer, got {raw!r}") from exc


def resolve_credentials(
    access: str, secret: str, *, gen: "Callable[[int], str] | None" = None
) -> "tuple[str, str]":
    """Return the operator-supplied (access, secret) only when BOTH are set;
    otherwise generate a fresh hex pair safe for a systemd EnvironmentFile.

    Mirrors ocfp's credential rule: a half-configured pair (one set, one empty)
    is a mistake, so it is ignored rather than silently mixed with a generated
    half. Hex keys carry no '=' / quote / newline that an EnvironmentFile would
    mangle.
    """
    a = (access or "").strip()
    s = (secret or "").strip()
    if a and s:
        return a, s
    g = gen or secrets.token_hex
    return g(10), g(20)  # 20-char access, 40-char secret


def normalize_tls_mode(mode: str) -> str:
    m = (mode or "").strip().lower() or DEFAULT_TLS_MODE
    if m not in VALID_TLS_MODES:
        raise ValueError(f"tls_mode must be one of {VALID_TLS_MODES}, got {mode!r}")
    return m


def endpoint_url(ip: str, port: int, tls_mode: str) -> str:
    """S3 endpoint URL including scheme.

    ``disabled`` -> ``http://``; ``self-signed`` -> ``https://``. The scheme is
    carried in the endpoint so `_compiled.s3_https_url` and ``aws --endpoint-url``
    both target the right protocol without a second knob.
    """
    scheme = "https" if normalize_tls_mode(tls_mode) == "self-signed" else "http"
    return f"{scheme}://{ip}:{port}"


def parse_buckets(raw: str) -> "list[str]":
    """Split a space/comma-separated bucket list, preserving order, de-duped."""
    parts = [b.strip() for b in raw.replace(",", " ").split()]
    out: "list[str]" = []
    for b in parts:
        if b and b not in out:
            out.append(b)
    return out


@dataclasses.dataclass(frozen=True)
class ArtifactsConfig:
    """Fully-resolved artifacts VM + RustFS configuration."""

    env: str
    node: str
    vm_storage: str
    bridge: str
    ip: str
    gateway: str
    cidr_mask: int
    cores: int
    memory_mib: int
    disk_gib: int
    data_disk_gib: int  # 0 = single root disk (no separate ZFS data disk)
    s3_port: int
    console_port: int
    rustfs_version: str
    rustfs_url: str
    access_key: str
    secret_key: str
    tls_mode: str
    buckets: "tuple[str, ...]"
    ciuser: str
    image_url: str

    @property
    def endpoint(self) -> str:
        return endpoint_url(self.ip, self.s3_port, self.tls_mode)

    @property
    def ipconfig0(self) -> str:
        return f"ip={self.ip}/{self.cidr_mask},gw={self.gateway}"


def _cidr_mask(cidr: str, default: int = 24) -> int:
    """Prefix length from a CIDR like ``172.31.0.0/24`` (default 24)."""
    if "/" in (cidr or ""):
        try:
            return int(cidr.split("/", 1)[1])
        except ValueError:
            return default
    return default


def build_config(
    env: str,
    *,
    getenv: Getter,
    getvar: Getter,
    access_key: "str | None" = None,
    secret_key: "str | None" = None,
) -> ArtifactsConfig:
    """Resolve every knob from env vars, then vars.yml, then defaults.

    ``getvar`` reads a key (without leading slash) from the layered vars docs
    (env vars.yml winning over manifests/bosh/vars.yml); the caller supplies it.
    ``access_key``/``secret_key``, when given (e.g. from a prior state file),
    take precedence over freshly resolving + generating so a re-run reuses the
    VM's existing credentials rather than churning them.
    """
    version = resolve_str(getenv, getvar, "ARTIFACTS_RUSTFS_VERSION", "artifacts_rustfs_version", DEFAULT_RUSTFS_VERSION)
    rustfs_url = resolve_str(getenv, getvar, "ARTIFACTS_RUSTFS_URL", "artifacts_rustfs_url", _rustfs_url(version))
    tls_mode = normalize_tls_mode(
        resolve_str(getenv, getvar, "ARTIFACTS_TLS_MODE", "artifacts_tls_mode", DEFAULT_TLS_MODE)
    )
    buckets_raw = resolve_str(
        getenv, getvar, "ARTIFACTS_BUCKETS", "artifacts_buckets", " ".join(DEFAULT_BUCKETS)
    )
    buckets = tuple(parse_buckets(buckets_raw)) or DEFAULT_BUCKETS

    if access_key is not None and secret_key is not None:
        access, secret = access_key, secret_key
    else:
        access, secret = resolve_credentials(
            resolve_str(getenv, getvar, "ARTIFACTS_S3_ACCESS_KEY", "artifacts_s3_access_key", ""),
            resolve_str(getenv, getvar, "ARTIFACTS_S3_SECRET_KEY", "artifacts_s3_secret_key", ""),
        )

    cidr = (getvar("internal_cidr") or getvar("cpitest_sdn_subnet") or "").strip()
    return ArtifactsConfig(
        env=env,
        node=(getvar("pve_node") or "").strip(),
        vm_storage=(getvar("pve_vm_storage") or getvar("pve_stemcell_storage") or "").strip(),
        bridge=(getvar("pve_network_bridge") or getvar("cpitest_sdn_vnet") or "cpitest0").strip(),
        ip=resolve_str(getenv, getvar, "ARTIFACTS_VM_IP", "artifacts_vm_ip", DEFAULT_IP),
        gateway=(getvar("internal_gw") or getvar("cpitest_sdn_gateway") or "").strip(),
        cidr_mask=_cidr_mask(cidr),
        cores=resolve_int(getenv, getvar, "ARTIFACTS_VM_CPU", "artifacts_vm_cpu", DEFAULT_CORES),
        memory_mib=resolve_int(getenv, getvar, "ARTIFACTS_VM_MEMORY", "artifacts_vm_memory", DEFAULT_MEMORY_MIB),
        disk_gib=resolve_int(getenv, getvar, "ARTIFACTS_VM_DISK", "artifacts_vm_disk_gib", DEFAULT_DISK_GIB),
        data_disk_gib=resolve_int(getenv, getvar, "ARTIFACTS_DATA_DISK_GIB", "artifacts_data_disk_gib", 0),
        s3_port=resolve_int(getenv, getvar, "ARTIFACTS_S3_PORT", "artifacts_s3_port", DEFAULT_S3_PORT),
        console_port=resolve_int(getenv, getvar, "ARTIFACTS_CONSOLE_PORT", "artifacts_console_port", DEFAULT_CONSOLE_PORT),
        rustfs_version=version,
        rustfs_url=rustfs_url,
        access_key=access,
        secret_key=secret,
        tls_mode=tls_mode,
        buckets=buckets,
        ciuser=resolve_str(getenv, getvar, "ARTIFACTS_CIUSER", "artifacts_ciuser", DEFAULT_CIUSER),
        image_url=resolve_str(getenv, getvar, "ARTIFACTS_IMAGE_URL", "artifacts_image_url", DEFAULT_IMAGE_URL),
    )


# --------------------------------------------------------------------------- #
# State file (per-env; carries the generated secret -> git-ignored)
# --------------------------------------------------------------------------- #

def state_path(env: str) -> Path:
    return ENVS_DIR / env / "artifacts-state.json"


def state_from_config(cfg: ArtifactsConfig, *, vmid: int) -> dict:
    """The persisted record written after a successful bootstrap."""
    return {
        "vmid": vmid,
        "name": VM_NAME,
        "node": cfg.node,
        "ip": cfg.ip,
        "endpoint": cfg.endpoint,
        "region": DEFAULT_REGION,
        "s3_port": cfg.s3_port,
        "console_port": cfg.console_port,
        "tls_mode": cfg.tls_mode,
        "access_key": cfg.access_key,
        "secret_key": cfg.secret_key,
        "buckets": list(cfg.buckets),
        "rustfs_version": cfg.rustfs_version,
    }


def write_state(env: str, state: dict) -> Path:
    p = state_path(env)
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(json.dumps(state, indent=2) + "\n", encoding="utf-8")
    return p


def read_state(env: str) -> "dict | None":
    p = state_path(env)
    if not p.exists():
        return None
    try:
        data = json.loads(p.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else None
    except (json.JSONDecodeError, OSError):
        return None


def delete_state(env: str) -> bool:
    p = state_path(env)
    if p.exists():
        p.unlink()
        return True
    return False


# --------------------------------------------------------------------------- #
# Online probe + StoreConfig (consumed by every pipeline; fail-open)
# --------------------------------------------------------------------------- #

def probe_tcp(host: str, port: int, *, timeout: float = 2.0) -> bool:
    """True when a TCP connection to host:port succeeds within ``timeout``.

    Never raises — any error (DNS, refused, timeout, no route) is reported as
    offline so callers fail-open.
    """
    if not host:
        return False
    try:
        with socket.create_connection((host, int(port)), timeout=timeout):
            return True
    except (OSError, ValueError, OverflowError):
        return False


def artifacts_online(env: str, *, timeout: float = 2.0) -> bool:
    """True when this env's artifacts VM has a state file and answers on :S3.

    Resolved from the state file alone (no vars.yml read) so consumers stay
    import-light. Absent state or an unreachable endpoint -> False (fail-open).
    """
    state = read_state(env)
    if not state:
        return False
    host = str(state.get("ip", "")).strip()
    port = int(state.get("s3_port", DEFAULT_S3_PORT) or DEFAULT_S3_PORT)
    return probe_tcp(host, port, timeout=timeout)


def artifacts_store_config(env: str, bucket: str) -> "_compiled.StoreConfig | None":
    """Build an S3 StoreConfig from this env's artifacts state, or None.

    Returns None when no state file exists, so a caller can fall back to its
    default store. The endpoint carries its scheme (http/https) so the
    path-style https reference and ``aws --endpoint-url`` both target the right
    protocol.
    """
    state = read_state(env)
    if not state:
        return None
    endpoint = str(state.get("endpoint", "")).strip()
    if not endpoint:
        return None
    return _compiled.StoreConfig(
        kind="s3",
        s3_endpoint=endpoint,
        s3_bucket=bucket,
        s3_region=str(state.get("region", DEFAULT_REGION) or DEFAULT_REGION),
        s3_access_key=str(state.get("access_key", "")),
        s3_secret_key=str(state.get("secret_key", "")),
    )


def compiled_store_override(
    environ: "dict[str, str]", bucket: str
) -> "_compiled.StoreConfig | None":
    """The artifacts S3 store the compiled-release pipelines should use, or None.

    Returns None — leaving the caller's default store untouched — unless ALL of:

      * an env is explicitly selected (``BOSH_PVE_ENV`` set); the artifacts VM
        is a per-env, isolated-network concept, so an unselected env never
        engages it;
      * the operator has NOT set an explicit ``COMPILED_RELEASES_STORE`` or
        ``COMPILED_RELEASES_S3_ENDPOINT`` override — an explicit choice always
        wins;
      * the env's artifacts VM is online.

    This is the single gate both ``scripts/bosh`` and ``scripts/cf`` consult, so
    the fail-open semantics are identical for the BOSH and CF pipelines.
    """
    env = (environ.get("BOSH_PVE_ENV", "") or "").strip()
    if not env:
        return None
    explicit = (environ.get("COMPILED_RELEASES_STORE", "") or "").strip() or (
        environ.get("COMPILED_RELEASES_S3_ENDPOINT", "") or ""
    ).strip()
    if explicit:
        return None
    if not artifacts_online(env):
        return None
    return artifacts_store_config(env, bucket)


def aws_env(state: dict, base: "dict[str, str] | None" = None) -> "dict[str, str]":
    """Environment for an ``aws`` invocation against the RustFS endpoint.

    Path-style addressing is mandatory for an S3-compatible store on a bare
    host (no wildcard TLS for ``<bucket>.<endpoint>``); the EC2 metadata probe
    is disabled so the CLI does not hang looking for instance credentials.
    """
    env = dict(base or {})
    env["AWS_ACCESS_KEY_ID"] = str(state.get("access_key", ""))
    env["AWS_SECRET_ACCESS_KEY"] = str(state.get("secret_key", ""))
    env["AWS_DEFAULT_REGION"] = str(state.get("region", DEFAULT_REGION) or DEFAULT_REGION)
    env["AWS_S3_ADDRESSING_STYLE"] = "path"
    env["AWS_EC2_METADATA_DISABLED"] = "true"
    return env


# --------------------------------------------------------------------------- #
# Script renderers (pure; piped to bash over SSH by scripts/artifacts)
# --------------------------------------------------------------------------- #

def render_qm_create_script(
    cfg: ArtifactsConfig,
    *,
    vmid: int,
    image_path: str,
    sshkey_path: str,
) -> str:
    """Render the bash run on the PVE host to create + start the artifacts VM.

    Idempotent: a VM whose config already exists is left as-is (re-run safe).
    Uses ``qm`` directly rather than the REST API because importing a cloud
    image disk and attaching a cloud-init drive is far simpler and less
    error-prone over SSH, and reuses the same root@pve SSH channel net-up uses.
    The disk is imported from a local cloud image, resized, and the VM is given
    a static ipconfig0 plus the operator SSH key for post-boot provisioning.
    """
    q = shlex.quote
    storage = q(cfg.vm_storage)
    extra_disk = ""
    if cfg.data_disk_gib > 0:
        extra_disk = f'qm set "$VMID" --scsi1 {storage}:{int(cfg.data_disk_gib)} >/dev/null\n'
    tag = q(VM_TAG)
    return f"""set -euo pipefail
VMID={int(vmid)}
IMG={q(image_path)}
if qm config "$VMID" >/dev/null 2>&1; then
  echo "    qm: VM $VMID already exists; leaving as-is"
else
  qm create "$VMID" \\
    --name {q(VM_NAME)} \\
    --memory {int(cfg.memory_mib)} \\
    --cores {int(cfg.cores)} \\
    --sockets 1 \\
    --cpu host \\
    --ostype l26 \\
    --machine q35 \\
    --scsihw virtio-scsi-single \\
    --net0 virtio,bridge={q(cfg.bridge)} \\
    --agent enabled=1 \\
    --tags {tag} >/dev/null
  qm set "$VMID" --scsi0 {storage}:0,import-from="$IMG" >/dev/null
  qm resize "$VMID" scsi0 {int(cfg.disk_gib)}G >/dev/null
  {extra_disk}qm set "$VMID" --ide2 {storage}:cloudinit >/dev/null
  qm set "$VMID" --boot order=scsi0 >/dev/null
  qm set "$VMID" --ciuser {q(cfg.ciuser)} >/dev/null
  qm set "$VMID" --sshkeys {q(sshkey_path)} >/dev/null
  qm set "$VMID" --ipconfig0 {q(cfg.ipconfig0)} >/dev/null
  echo "    qm: created VM $VMID ({VM_NAME})"
fi
if [ "$(qm status "$VMID" | awk '{{print $2}}')" != "running" ]; then
  qm start "$VMID" >/dev/null
  echo "    qm: started VM $VMID"
else
  echo "    qm: VM $VMID already running"
fi
"""


def render_image_fetch_script(image_url: str, image_path: str) -> str:
    """Render the bash run on the PVE host to download the cloud image once.

    Guarded by file presence so a re-run does not re-download. Uses curl with a
    retry so a transient blip does not abort bootstrap.
    """
    q = shlex.quote
    return f"""set -euo pipefail
IMG={q(image_path)}
if [ -s "$IMG" ]; then
  echo "    image: $IMG present"
else
  echo "    image: downloading {q(image_url)}"
  mkdir -p "$(dirname "$IMG")"
  curl -fL --retry 3 --retry-delay 5 -o "$IMG.part" {q(image_url)}
  mv "$IMG.part" "$IMG"
fi
"""


def render_install_script(cfg: ArtifactsConfig) -> str:
    """Render the idempotent RustFS install script run on the guest over SSH.

    Every step is guarded so a re-run is a no-op. Installs the musl RustFS
    binary, a dedicated system user, an EnvironmentFile carrying the
    credentials, and a systemd unit; optionally lays a ZFS data pool; then
    creates the buckets against the local endpoint. Buckets are created on the
    guest (not the operator host) so bootstrap needs no S3 client on the Mac.
    """
    q = shlex.quote
    mode = normalize_tls_mode(cfg.tls_mode)
    tls_env = "RUSTFS_TLS_PATH=/opt/rustfs/tls\n" if mode == "self-signed" else ""
    local_scheme = "https" if mode == "self-signed" else "http"
    aws_insecure = " --no-verify-ssl" if mode == "self-signed" else ""
    buckets = " ".join(q(b) for b in cfg.buckets)

    data_setup = "mkdir -p /data\n"
    # Ubuntu 24.04 (noble) dropped the awscli deb, so install AWS CLI v2 from the
    # official bundled installer instead of apt.
    apt_pkgs = "unzip jq curl ca-certificates"
    if cfg.data_disk_gib > 0:
        apt_pkgs += " zfsutils-linux"
        data_setup = """# ZFS data pool on the second disk (idempotent).
if ! zpool list artpool >/dev/null 2>&1; then
  DISK=$(lsblk -dpno NAME,SIZE | awk '$1!~/loop/' | sed -n '2p' | awk '{print $1}')
  zpool create -f -o ashift=12 artpool "$DISK"
  zfs create -o compression=lz4 -o mountpoint=/data artpool/data
fi
"""

    tls_setup = ""
    if mode == "self-signed":
        tls_setup = f"""# Self-signed TLS cert with the VM IP as SAN (idempotent).
if [ ! -s /opt/rustfs/tls/rustfs_key.pem ]; then
  mkdir -p /opt/rustfs/tls
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \\
    -keyout /opt/rustfs/tls/rustfs_key.pem -out /opt/rustfs/tls/rustfs_cert.pem \\
    -days 3650 -subj "/CN={cfg.ip}" -addext "subjectAltName=IP:{cfg.ip}"
  chmod 0640 /opt/rustfs/tls/rustfs_cert.pem
  chmod 0600 /opt/rustfs/tls/rustfs_key.pem
  chown -R rustfs:rustfs /opt/rustfs/tls
fi
"""

    return f"""set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

echo "    install: waiting for cloud-init"
cloud-init status --wait >/dev/null 2>&1 || true

echo "    install: apt packages"
apt-get update -qq
apt-get install -y -qq {apt_pkgs} >/dev/null

# AWS CLI v2 (bundled installer; noble has no awscli deb) — only when missing.
if ! command -v aws >/dev/null 2>&1; then
  echo "    install: aws cli v2"
  curl -fL --retry 3 -o /tmp/awscliv2.zip "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip"
  unzip -q -o /tmp/awscliv2.zip -d /tmp
  /tmp/aws/install --update >/dev/null
  rm -rf /tmp/awscliv2.zip /tmp/aws
fi

# RustFS binary (musl static) — fetch only when missing.
if [ ! -x /usr/local/bin/rustfs ]; then
  echo "    install: rustfs {cfg.rustfs_version}"
  curl -fL --retry 3 -o /tmp/rustfs.zip {q(cfg.rustfs_url)}
  unzip -o /tmp/rustfs.zip -d /tmp/rustfs.d >/dev/null
  install -m 0755 "$(find /tmp/rustfs.d -name rustfs -type f | head -n1)" /usr/local/bin/rustfs
  rm -rf /tmp/rustfs.zip /tmp/rustfs.d
fi

# Dedicated system user.
if ! id rustfs >/dev/null 2>&1; then
  useradd --system --home /var/lib/rustfs --shell /usr/sbin/nologin rustfs
fi

{data_setup}chown -R rustfs:rustfs /data

{tls_setup}# EnvironmentFile carrying the S3 credentials (hex -> EnvironmentFile-safe).
umask 027
cat > /etc/default/rustfs <<'EOF'
RUSTFS_ADDRESS=:{int(cfg.s3_port)}
RUSTFS_CONSOLE_ADDRESS=:{int(cfg.console_port)}
RUSTFS_VOLUMES=/data
RUSTFS_ACCESS_KEY={cfg.access_key}
RUSTFS_SECRET_KEY={cfg.secret_key}
{tls_env}EOF
chmod 0640 /etc/default/rustfs
chown root:rustfs /etc/default/rustfs

cat > /etc/systemd/system/rustfs.service <<'EOF'
[Unit]
Description=RustFS object storage
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=rustfs
Group=rustfs
EnvironmentFile=/etc/default/rustfs
ExecStart=/usr/local/bin/rustfs
Restart=on-failure
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now rustfs >/dev/null 2>&1 || systemctl restart rustfs

# Wait for the S3 port, then ensure buckets (idempotent).
export AWS_ACCESS_KEY_ID={q(cfg.access_key)}
export AWS_SECRET_ACCESS_KEY={q(cfg.secret_key)}
export AWS_DEFAULT_REGION={q(DEFAULT_REGION)}
export AWS_EC2_METADATA_DISABLED=true
aws configure set default.s3.addressing_style path >/dev/null 2>&1 || true
EP={q(local_scheme)}"://127.0.0.1:{int(cfg.s3_port)}"
for i in $(seq 1 30); do
  if aws --endpoint-url "$EP"{aws_insecure} s3 ls >/dev/null 2>&1; then break; fi
  sleep 2
done
for b in {buckets}; do
  if aws --endpoint-url "$EP"{aws_insecure} s3api head-bucket --bucket "$b" >/dev/null 2>&1; then
    echo "    bucket: $b present"
  else
    aws --endpoint-url "$EP"{aws_insecure} s3 mb "s3://$b" >/dev/null
    echo "    bucket: $b created"
  fi
done
echo "    install: rustfs ready on $EP"
"""
