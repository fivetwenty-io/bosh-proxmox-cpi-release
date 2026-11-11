"""Step 11r of scripts/lifecycle: an adopted migration mover on shared storage.

The pass stages the shape a journal-managed attach meets after an interrupted
cross-node migration. A disk sits alone on a holder on one node, that holder
carries the bosh-disk-mover tag, and the Director attaches the disk to a VM on
another node. The CPI then adopts the holder as its mover, migrates it to the
VM's node, and has to prove the migration landed. Real PVE answers a config
read on the old node with an HTTP 500 that says the configuration file does
not exist, so a check that accepts only a 404 as proof fails an attach that
worked.

The steps are these.

  setup   check the lab identity and the shared storage, start a private
          journal namespace in a scratch directory, and note every VMID the
          cluster already has
  create  create_disk pinned to a node other than the VM's, under a parker
          prefix unique to this run, so the disk parks alone on a fresh parker
  tag     add the bosh-disk-mover tag to that parker through pmx
  attach  attach_disk to the VM in one attempt, keeping the CPI's log
  assert  one new qmigrate task for the mover and it ended OK, the disk on the
          VM's node carrying its serial, no protection-restore WARN in the CPI
          log, and a clean storage-journal audit --summary. Under the journal
          the mover stays on the VM's node, empty and protected, because the
          journal deletes only holders its own request created, and the CPI
          says so in exactly one WARN and no other mover WARN. Without the
          journal the mover is destroyed.

The shared storage must be one that only the pve-cpi cluster mounts. A storage
another cluster also mounts would put this run's disk, and the journal's view
of that storage, in front of a CPI that is not under test.

Cleanup runs whether the pass passed or failed. It detaches and deletes the
disk through the CPI, destroys through pmx only the guests this run created,
and removes the scratch journal directory and the configs written into it.

Every PVE read goes through the verifier, which the caller builds with
PVE_VERIFY_TRANSPORT=pmx, and every PVE write goes through PmxWriter. Neither
touches a guest that the cluster held before the run, VMIDs 100 or 101, a
template, or a parker carrying another prefix. The pass refuses to start
unless the cluster names itself pve-cpi and every node is a lab-pve-cpi-N node.
"""

from __future__ import annotations

import copy
import json
import os
import re
import secrets
import shutil
import subprocess
import tempfile
import time
import urllib.parse
from pathlib import Path
from typing import Any, Callable

import _pve_verify
import _vmcid

LAB_CLUSTER = "pve-cpi"
LAB_NODE_RE = re.compile(r"^lab-pve-cpi-\d+$")
# Guests the lab keeps for itself. The pass never writes to them, whatever
# their tags say.
PROTECTED_VMIDS = frozenset({100, 101})

MOVER_TAG = "bosh-disk-mover"
PARKER_TAG = "bosh-parker"
STORAGE_SET = "lifecycle-11r"
PREFIX_STEM = "l11r-"
NAMESPACE_STEM = "lifecycle-11r-"

# Lines of storage-journal audit --summary that each mean the journal needs an
# operator. The command prints them only when there is something to say.
_UNCLEAN_AUDIT_PREFIXES = (
    "generation index:", "entry binding finding:", "conflict:", "vm-scan issue:", "issue:", "runbook:",
    "record:", "evidence:", "planned step:",
)
_AUDIT_HEADER_RE = re.compile(
    r"^audit: complete=(\S+) vm_scan_complete=(\S+) generation_index_healthy=(\S+) "
    r"cluster_continuity=(\S+) records=\d+$"
)


class LabRefused(RuntimeError):
    """The pass will not run here, and nothing has been written."""


# -- pure checks --------------------------------------------------------------


def lab_guard_verdict(status: Any) -> str:
    """Return "" when /cluster/status describes the pve-cpi lab, else the reason.

    The lab is one cluster named pve-cpi whose nodes are all named
    lab-pve-cpi-N and are all online. The az2 lab is pve-cpi-az2 with
    lab-pve-cpi-az2-N nodes, so it fails both checks.
    """
    if not isinstance(status, list):
        return "GET /cluster/status did not return a list"
    clusters = [e for e in status if isinstance(e, dict) and e.get("type") == "cluster"]
    nodes = [e for e in status if isinstance(e, dict) and e.get("type") == "node"]
    names = [str(c.get("name", "")) for c in clusters]
    if names != [LAB_CLUSTER]:
        return f"the cluster names itself {names!r}, and this pass runs only on the {LAB_CLUSTER!r} lab"
    if len(nodes) < 2:
        return f"the {LAB_CLUSTER} cluster reports {len(nodes)} node(s), and the pass needs two"
    foreign = sorted(str(n.get("name", "")) for n in nodes if not LAB_NODE_RE.fullmatch(str(n.get("name", ""))))
    if foreign:
        return f"nodes {foreign} are not lab-pve-cpi-N nodes"
    offline = sorted(str(n.get("name", "")) for n in nodes if n.get("online") not in (1, "1", True))
    if offline:
        return f"nodes {offline} are not online"
    return ""


def lab_nodes(status: list[dict[str, Any]]) -> list[str]:
    """The node names in a /cluster/status answer that passed lab_guard_verdict."""
    return sorted(str(e.get("name", "")) for e in status if isinstance(e, dict) and e.get("type") == "node")


# Shared storages that another cluster also mounts. The pass writes images into
# the parker band on its storage and binds a journal to it, so it must run on a
# storage only the pve-cpi cluster mounts.
FOREIGN_MOUNTED_STORAGES = frozenset({"nfs-images"})


def shared_storage_verdict(entry: "dict[str, Any] | None", storage: str, nodes: "list[str] | None" = None) -> str:
    """Return "" when the storage can be a set member the journal manages, else the reason.

    A storage set member must be shared NFS with images content. That is the
    rule internal/storageinventory/collector.go applies, so a storage that
    fails here would fail create_disk anyway, after the pass had started. The
    storage must also be one only pve-cpi mounts, and when its nodes field
    restricts it, that field must cover every lab node.
    """
    if storage.strip().lower() in FOREIGN_MOUNTED_STORAGES:
        return (f"storage {storage!r} is mounted by another cluster as well; "
                "pick an NFS storage that only the pve-cpi cluster mounts")
    if entry is None:
        return f"storage {storage!r} is not in the cluster's storage index"
    if str(entry.get("type", "")).strip().lower() != "nfs":
        return f"storage {storage!r} is {entry.get('type')!r}, and the pass needs NFS"
    content = {c.strip() for c in str(entry.get("content", "")).split(",") if c.strip()}
    if "images" not in content:
        return f"storage {storage!r} does not serve images content (content={entry.get('content')!r})"
    if entry.get("disable") in (1, "1", True):
        return f"storage {storage!r} is disabled"
    restricted = {n.strip() for n in str(entry.get("nodes", "") or "").split(",") if n.strip()}
    if restricted and nodes:
        missing = sorted(set(nodes) - restricted)
        if missing:
            return f"storage {storage!r} is limited to nodes {sorted(restricted)}, which leaves out {missing}"
    return ""


def tags_of(config: dict[str, Any]) -> list[str]:
    return [t.strip() for t in re.split(r"[;, ]+", str(config.get("tags", "") or "")) if t.strip()]


def has_tag(config: dict[str, Any], want: str) -> bool:
    return any(t.lower() == want.lower() for t in tags_of(config))


def with_mover_tag(config: dict[str, Any]) -> str:
    """The holder's tag string with bosh-disk-mover added and every other tag kept."""
    tags = tags_of(config)
    if not any(t.lower() == MOVER_TAG for t in tags):
        tags.append(MOVER_TAG)
    return ";".join(tags)


def disk_slots(config: dict[str, Any]) -> dict[str, str]:
    """The disk-bearing slots of a VM config, unused ones included, by slot."""
    out: dict[str, str] = {}
    for key, value in config.items():
        if not _pve_verify._DISK_SLOT_RE.match(str(key)):
            continue
        volid = str(value).split(",", 1)[0].strip()
        if volid and volid != "none":
            out[str(key)] = volid
    return out


def run_owned_verdict(
    vmid: int, config: dict[str, Any], *, prefix: str, preexisting: "set[int] | frozenset[int]",
    band: tuple[int, int],
) -> str:
    """Return "" when this run created the guest, else why the pass must not touch it.

    A guest is ours only when the cluster did not hold its VMID before the
    run, it sits in the parker band, it is not a template, and its parker
    prefix is the one this run made up. The prefix is random per run, so no
    other deployment's parker can carry it.
    """
    if vmid in PROTECTED_VMIDS:
        return f"VMID {vmid} is one the lab keeps"
    if vmid in preexisting:
        return f"VMID {vmid} was in the cluster before this run"
    if not band[0] <= vmid <= band[1]:
        return f"VMID {vmid} is outside the parker band {band[0]}-{band[1]}"
    if str(config.get("template", "")).strip() in ("1", "true", "True"):
        return f"VMID {vmid} is a template"
    identity = _pve_verify.parker_prefix_identity(str(config.get("tags", "") or ""), str(config.get("name", "") or ""))
    if identity != prefix:
        return f"VMID {vmid} carries parker prefix {identity!r}, not this run's {prefix!r}"
    if not (has_tag(config, PARKER_TAG) or has_tag(config, MOVER_TAG)):
        return f"VMID {vmid} carries neither the {PARKER_TAG} nor the {MOVER_TAG} tag"
    return ""


def log_lines(stderr: str) -> list[dict[str, Any]]:
    """The CPI's JSON log records in stderr. Anything else on stderr is skipped."""
    records = []
    for line in stderr.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            record = json.loads(line)
        except ValueError:
            continue
        if isinstance(record, dict):
            records.append(record)
    return records


def warnings_matching(stderr: str, needle: str) -> list[str]:
    """The msg of every WARN record whose msg contains needle, ignoring case."""
    return [
        str(r.get("msg", ""))
        for r in log_lines(stderr)
        if str(r.get("level", "")).upper().startswith("WARN") and needle.lower() in str(r.get("msg", "")).lower()
    ]


def protection_restore_warnings(stderr: str) -> list[str]:
    return warnings_matching(stderr, "protection restore")


def kept_mover_warnings(stderr: str) -> list[str]:
    return warnings_matching(stderr, "kept the migration mover")


# Every WARN that warnMoverLeft in attach_disk.go logs carries the mover_vmid
# field and ends its message with this sentence, whatever the reason it gives.
MOVER_LEFT_SUFFIX = "Nothing in the CPI removes it later"


def mover_left_warnings(stderr: str) -> list[str]:
    """The msg of every WARN record shaped like warnMoverLeft's, which says a mover was left behind."""
    return [
        str(r.get("msg", ""))
        for r in log_lines(stderr)
        if str(r.get("level", "")).upper().startswith("WARN")
        and ("mover_vmid" in r or MOVER_LEFT_SUFFIX.lower() in str(r.get("msg", "")).lower())
    ]


# The one WARN a journal-managed attach logs when it adopts a mover that an
# earlier request created and keeps it, because the journal deletes only the
# holders its own request created (destroyOrKeepMover in attach_disk.go).
ADOPTED_MOVER_KEPT = "kept the migration mover because an earlier request created it"


def adopted_mover_warning_findings(stderr: str) -> list[str]:
    """Every way the CPI's mover WARNs differ from one adopted-mover keep; [] when they match.

    The log must carry the adopted-mover keep exactly once and no other
    warnMoverLeft WARN, so a keep because the protection restore was cut off,
    or a destroy that failed, fails here. A cut-off or refused restore also
    fails the protection-restore check. Other WARNs that merely mention a
    mover, such as legacy provenance notes, are not mover-left warnings.
    """
    findings = []
    mover_warns = mover_left_warnings(stderr)
    kept = [w for w in mover_warns if ADOPTED_MOVER_KEPT.lower() in w.lower()]
    if len(kept) != 1:
        findings.append(f"expected the WARN {ADOPTED_MOVER_KEPT!r} exactly once, found it {len(kept)} times"
                        + (", verbatim: " + " | ".join(kept) if kept else ""))
    others = [w for w in mover_warns if ADOPTED_MOVER_KEPT.lower() not in w.lower()]
    if others:
        findings.append("the CPI logged other mover-left warnings, verbatim: " + " | ".join(others))
    return findings


def new_qmigrate_tasks(before: list[dict[str, Any]], after: list[dict[str, Any]], vmid: int) -> list[dict[str, Any]]:
    """The qmigrate tasks for vmid in after whose UPID was not in before."""
    seen = {str(t.get("upid", "")) for t in before if isinstance(t, dict)}
    return [
        t for t in after
        if isinstance(t, dict) and t.get("type") == "qmigrate" and str(t.get("id", "")) == str(vmid)
        and str(t.get("upid", "")) not in seen
    ]


def audit_summary_findings(exit_code: int, text: str) -> list[str]:
    """Every reason storage-journal audit --summary is not clean; [] when it is."""
    findings = []
    if exit_code != 0:
        findings.append(f"storage-journal audit exited {exit_code}")
    lines = [line.strip() for line in text.splitlines() if line.strip()]
    header = _AUDIT_HEADER_RE.match(lines[0]) if lines else None
    if header is None:
        findings.append("storage-journal audit printed no audit header")
    elif any(flag != "true" for flag in header.groups()):
        findings.append(lines[0])
    charging = [line for line in lines if line.startswith("charging:")]
    if charging != ["charging: none"]:
        findings.extend(charging or ["storage-journal audit printed no charging line"])
    findings.extend(line for line in lines if line.startswith(_UNCLEAN_AUDIT_PREFIXES))
    return findings


def config_overrides(
    *, storage: str, prefix: str, band: tuple[int, int], journal_dir: str, namespace: str, journal: bool,
    base_sets: "dict[str, Any] | None" = None,
) -> dict[str, Any]:
    """The keys the 11r config sets on top of CPI_CONFIG. None removes a key.

    The parker prefix is this run's own, so create_disk parks the disk alone
    on a fresh parker, and parker_pool is empty so no pool is made for it.
    disk_migration is removed so the attach runs under the on_attach default.
    With journal on, the disk comes from a one-member storage set added
    beside base_sets, the sets CPI_CONFIG already defines, so any other set
    binding in CPI_CONFIG still resolves. The journal lives in this run's
    scratch directory under its own namespace.
    """
    overrides: dict[str, Any] = {
        "detached_disk_strategy": "parked",
        "parked_disk_vmid_range_start": band[0],
        "parked_disk_vmid_range_end": band[1],
        "parker_prefix": prefix,
        "parker_pool": "",
        "disk_storage": storage,
        "disk_migration": None,
        "log_level": "info",
    }
    if journal:
        sets = copy.deepcopy(dict(base_sets or {}))
        if STORAGE_SET in sets:
            raise ValueError(f"CPI_CONFIG already defines a storage set named {STORAGE_SET!r}")
        sets[STORAGE_SET] = {"names": [storage], "strategy": {"name": "spread", "version": 1}}
        overrides.update(
            storage_sets=sets,
            persistent_storage_set=STORAGE_SET,
            storage_placement_namespace=namespace,
            storage_allocation_journal_dir=journal_dir,
        )
    return overrides


def apply_overrides(base: dict[str, Any], overrides: dict[str, Any]) -> dict[str, Any]:
    cfg = copy.deepcopy(base)
    for key, value in overrides.items():
        if value is None:
            cfg.pop(key, None)
        else:
            cfg[key] = value
    return cfg


_JOURNAL_KEYS = ("storage_allocation_journal_dir", "storage_placement_namespace", "persistent_storage_set",
                 "storage_sets")


def without_journal(cfg: dict[str, Any], base: dict[str, Any]) -> dict[str, Any]:
    """cfg with the journal keys this pass set put back the way base had them.

    Only what the pass added goes: a key base did not have is removed, and a
    key base had gets base's value back, so the base storage sets and their
    bindings stay exactly as CPI_CONFIG wrote them.
    """
    out = copy.deepcopy(cfg)
    for key in _JOURNAL_KEYS:
        if key in base:
            out[key] = copy.deepcopy(base[key])
        else:
            out.pop(key, None)
    return out


def lab_preflight(read: Callable[[str], Any], verifier: Any, storage: str, transport: str) -> list[str]:
    """Refuse unless the reads go through pmx, the cluster is the pve-cpi lab, and the storage fits.

    Returns the lab's node names. Nothing is written before this passes, and
    scripts/lifecycle calls it once more before step 1, so a run aimed at the
    wrong cluster stops before it creates anything at all.
    """
    if transport != "pmx" or getattr(verifier, "_transport", None) != "pmx":
        raise LabRefused("11r reads PVE only through pmx; run with PVE_VERIFY_TRANSPORT=pmx")
    status = read("/cluster/status")
    reason = lab_guard_verdict(status)
    if reason:
        raise LabRefused(f"11r refuses to run: {reason}")
    reason = shared_storage_verdict(verifier.storage_entry(storage), storage, lab_nodes(status))
    if reason:
        raise LabRefused(f"11r refuses to run: {reason}")
    return lab_nodes(status)


# -- pmx writes ---------------------------------------------------------------


class PmxWriter:
    """Writes to PVE through pmx, only to guests the pass has claimed.

    The pmx context is built the way the verifier's pmx reads build it, from
    the same CPI config, so the writes reach the same endpoint with the same
    credentials and trust. Each write checks that its VMID was claimed and is
    not one the lab keeps, and that its node is a lab node, before pmx runs.
    """

    def __init__(
        self, config: dict[str, Any], *, runner: Callable[..., Any] = subprocess.run,
        environ: "dict[str, str] | None" = None,
    ) -> None:
        self._verifier = _pve_verify.PVEVerifier(config)
        self._runner = runner
        self._environ = dict(os.environ if environ is None else environ)
        self._claimed: set[int] = set()

    def claim(self, vmid: int) -> None:
        if vmid in PROTECTED_VMIDS:
            raise RuntimeError(f"refusing to claim VMID {vmid}, which the lab keeps")
        self._claimed.add(int(vmid))

    def set_config(self, node: str, vmid: int, data: dict[str, str]) -> None:
        self._check(node, vmid)
        self._run("put", f"/nodes/{urllib.parse.quote(node)}/qemu/{int(vmid)}/config", data)

    def destroy(self, node: str, vmid: int) -> None:
        """Destroy the guest and purge it from job configs, never touching unreferenced volumes.

        pmx api delete sends each --data pair as a query parameter, which is
        where PVE reads DELETE options, as the verifier's urllib path does.
        """
        self._check(node, vmid)
        self._run("delete", f"/nodes/{urllib.parse.quote(node)}/qemu/{int(vmid)}",
                  {"purge": "1", "destroy-unreferenced-disks": "0"})

    def _check(self, node: str, vmid: int) -> None:
        if int(vmid) in PROTECTED_VMIDS or int(vmid) not in self._claimed:
            raise RuntimeError(f"refusing a pmx write to VMID {vmid}, which this run did not create")
        if not LAB_NODE_RE.fullmatch(node):
            raise RuntimeError(f"refusing a pmx write on node {node!r}, which is not a lab-pve-cpi-N node")

    def context(self, root: Path) -> tuple[dict[str, Any], str]:
        """The pmx context for this endpoint and the secret it reads from the environment."""
        v = self._verifier
        auth: dict[str, Any] = {"type": "password", "username": v._username_with_realm(),
                                "secret": "${PVE_VERIFY_PMX_SECRET}"}
        secret = v._password
        if v._token:
            identity, separator, secret = v._token.partition("=")
            username, bang, token_id = identity.partition("!")
            if not separator or not bang or not username or not token_id or not secret:
                raise RuntimeError("pmx writes need a complete API token")
            auth.update(type="token", username=username)
            auth["token-id"] = token_id
        tls: dict[str, Any] = {"insecure": not v.verify_ssl}
        if v.verify_ssl and v._ca:
            ca_file = root / "ca.pem"
            ca_file.write_text(v._ca, encoding="utf-8")
            ca_file.chmod(0o600)
            tls["ca-cert"] = str(ca_file)
        return ({"host": v.host, "port": v.port, "protocol": "https", "realm": v._realm, "auth": auth,
                 "tls": tls}, secret)

    def command(self, verb: str, path: str, data: dict[str, str], config_file: Path) -> list[str]:
        command = [self._environ.get("PVE_VERIFY_PMX_BIN", "pmx"), "--config", str(config_file),
                   "--context", "cpi-verifier", "--output", "json", "--no-log", "api", verb, path]
        for key, value in data.items():
            command.extend(["--data", f"{key}={value}"])
        return command

    def _run(self, verb: str, path: str, data: dict[str, str]) -> None:
        with tempfile.TemporaryDirectory(prefix="lifecycle-11r-pmx-") as directory:
            root = Path(directory)
            context, secret = self.context(root)
            config_file = root / "config.json"
            config_file.write_text(json.dumps({"current-context": "cpi-verifier",
                                               "contexts": {"cpi-verifier": context}}), encoding="utf-8")
            config_file.chmod(0o600)
            environment = {k: v for k, v in self._environ.items() if not k.startswith("PMX_")}
            environment["PVE_VERIFY_PMX_SECRET"] = secret
            try:
                result = self._runner(self.command(verb, path, data, config_file), env=environment,
                                      stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                      timeout=600, check=False)
            except (OSError, subprocess.TimeoutExpired) as error:
                raise RuntimeError(f"pmx {verb} {path} could not complete") from error
            if result.returncode:
                # pmx's error text can carry server content, so it stays out of
                # output that may be published, as it does for the verifier.
                raise RuntimeError(f"pmx {verb} {path} failed (exit {result.returncode})")


# -- the pass -----------------------------------------------------------------


SendFn = Callable[[str, list, str], "tuple[dict[str, Any], str]"]
CallFn = Callable[[str, list, str], Any]
JournalFn = Callable[..., "tuple[int, str, str]"]


class AdoptedMoverPass:
    """Step 11r, run against the VM the main sequence created.

    read(path) is a PVE GET through the verifier's pmx transport. send(method,
    args, config_path) makes one CPI request and returns the response envelope
    and the CPI's stderr. call(method, args, config_path) returns a result and
    retries what the CPI marks retriable, the way the Director does.
    journal(action, config_path, *flags) runs cpi storage-journal and returns
    its exit code, stdout, and stderr.
    """

    def __init__(
        self,
        *,
        verifier: Any,
        read: Callable[[str], Any],
        writer: Any,
        send: SendFn,
        call: CallFn,
        journal: JournalFn,
        base_config: dict[str, Any],
        transport: str,
        vm_cid: str,
        storage: str,
        band: tuple[int, int],
        journal_on: bool,
        disk_size_mib: int,
        step: Callable[[str], None],
        say: Callable[[str], None] = print,
        token_source: Callable[[int], str] = secrets.token_hex,
        workspace_root: "str | None" = None,
        sleep: Callable[[float], None] = time.sleep,
        gone_attempts: int = 30,
    ) -> None:
        self.verifier = verifier
        self._read = read
        self.writer = writer
        self._send = send
        self._call = call
        self._journal = journal
        self.base_config = base_config
        self.transport = transport
        self.vm_cid = vm_cid
        self.vmid = int(_vmcid.as_path_id(vm_cid))
        self.storage = storage
        self.band = band
        self.journal_on = journal_on
        self.disk_size_mib = disk_size_mib
        self._step = step
        self._say = say
        self._token_source = token_source
        self._workspace_root = workspace_root
        self._sleep = sleep
        self._gone_attempts = gone_attempts

        self.nodes: list[str] = []
        self.vm_node = ""
        self.source_node = ""
        self.prefix = ""
        self.namespace = ""
        self.workspace: "Path | None" = None
        self.config_path = ""
        self.plain_config_path = ""
        self.preexisting: set[int] = set()
        self.disk_cid = ""
        self.mover = 0
        self.tagged: dict[int, str] = {}
        self.owned: set[int] = set()
        self.attach_log = ""
        self._tasks_before: list[dict[str, Any]] = []

    # -- entry point ---------------------------------------------------------

    def run(self) -> None:
        """Run the pass, then clean up after it whether it passed or not."""
        self.preflight()
        failure: "BaseException | None" = None
        try:
            self.setup()
            self.create()
            self.tag()
            self.attach()
            self.check()
        except BaseException as exc:
            failure = exc
            raise
        finally:
            try:
                problems = self.cleanup()
            except Exception as exc:  # noqa: BLE001 - cleanup never replaces the pass's own error
                problems = [f"cleanup stopped: {exc}"]
            for problem in problems:
                self._say(f"11r cleanup: {problem}")
            if problems and failure is not None and hasattr(failure, "add_note"):
                failure.add_note("11r cleanup also left work behind: " + "; ".join(problems))
            if problems and failure is None:
                raise RuntimeError("11r passed, and its cleanup left work behind: " + "; ".join(problems))

    # -- steps ---------------------------------------------------------------

    def preflight(self) -> None:
        """Refuse before any write unless this is the pve-cpi lab and the storage fits."""
        self.nodes = lab_preflight(self._read, self.verifier, self.storage, self.transport)
        guests = self.live_guests()
        self.vm_node = guests.get(self.vmid, "")
        if self.vm_node not in self.nodes:
            raise LabRefused(f"11r cannot find VM {self.vmid} on a lab node (found {self.vm_node!r})")
        others = [n for n in self.nodes if n != self.vm_node]
        self.source_node = others[0]

    def setup(self) -> None:
        self._step(
            f"11r. adopted mover cross-node on shared storage {self.storage} "
            f"(journal {'on' if self.journal_on else 'off'}; disk on {self.source_node}, VM on {self.vm_node})"
        )
        self.preexisting = set(self.live_guests())
        self.prefix = PREFIX_STEM + self._token_source(4)
        self.namespace = NAMESPACE_STEM + self._token_source(16)
        self.workspace = Path(tempfile.mkdtemp(prefix="lifecycle-11r-", dir=self._workspace_root))
        self.workspace.chmod(0o700)
        journal_dir = self.workspace / "journal"
        journal_dir.mkdir(mode=0o700)
        overrides = config_overrides(
            storage=self.storage, prefix=self.prefix, band=self.band, journal_dir=str(journal_dir),
            namespace=self.namespace, journal=self.journal_on, base_sets=self.base_config.get("storage_sets"),
        )
        cfg = apply_overrides(self.base_config, overrides)
        self.config_path = self._write_config("cpi-11r.json", cfg)
        self.plain_config_path = self._write_config("cpi-11r-plain.json", without_journal(cfg, self.base_config))
        self._say(f"parker prefix {self.prefix}, namespace {self.namespace}, workspace {self.workspace}")
        if self.journal_on:
            code, out, err = self._journal(
                "initialize", self.config_path, "--authority-id", self.namespace, "--previous-writer-fenced",
                "--remote-tasks-settled",
            )
            if code != 0:
                raise RuntimeError(f"storage-journal initialize exited {code}: {(err or out).strip()}")
            try:
                cluster_id = json.loads(out).get("cluster_id")
            except (ValueError, AttributeError):
                cluster_id = None
            if not isinstance(cluster_id, str) or not cluster_id:
                raise RuntimeError(f"storage-journal initialize named no cluster: {out.strip()}")
            self._say(f"journal initialized for cluster {cluster_id}")

    def create(self) -> None:
        self._say(f"create_disk pinned to {self.source_node}")
        self.disk_cid = str(self._call("create_disk", [self.disk_size_mib, {"node": self.source_node}],
                                       self.config_path))
        stable_id = _pve_verify.disk_stable_id(self.disk_cid)
        if not stable_id:
            raise RuntimeError(f"the disk's CID carries no stable ID ({self.disk_cid!r}), so it cannot migrate")
        holders = self.holders()
        if len(holders) != 1:
            raise RuntimeError(f"the new disk should have exactly one holder, got {holders}")
        mover, node, _slot, config = holders[0]
        reason = run_owned_verdict(mover, config, prefix=self.prefix, preexisting=self.preexisting, band=self.band)
        if reason:
            raise RuntimeError(f"the disk parked on a holder this run did not create: {reason}")
        self._own(mover)
        if node != self.source_node:
            raise RuntimeError(f"parker {mover} is on {node}, not on {self.source_node} where the disk was pinned")
        if not has_tag(config, PARKER_TAG) or has_tag(config, MOVER_TAG):
            raise RuntimeError(f"holder {mover} is not a plain parker (tags {config.get('tags')!r})")
        slots = disk_slots(config)
        if len(slots) != 1:
            raise RuntimeError(f"parker {mover} should hold only this run's disk, and it holds {slots}")
        self.mover = mover
        self._say(f"create_disk={self.disk_cid} parked alone on parker {mover} on {node}")

    def tag(self) -> None:
        self._say(f"tag parker {self.mover} {MOVER_TAG} through pmx")
        config = self.verifier.qemu_config(self.mover, self.source_node)
        reason = run_owned_verdict(self.mover, config, prefix=self.prefix, preexisting=self.preexisting,
                                   band=self.band)
        if reason:
            raise RuntimeError(f"refusing to tag {self.mover}: {reason}")
        self.tagged[self.mover] = str(config.get("tags", "") or "")
        self.writer.set_config(self.source_node, self.mover, {"tags": with_mover_tag(config)})
        after = self.verifier.qemu_config(self.mover, self.source_node)
        if not has_tag(after, MOVER_TAG):
            raise RuntimeError(f"parker {self.mover} does not carry {MOVER_TAG} after the write")

    def attach(self) -> None:
        self._say(f"attach_disk to VM {self.vm_cid} on {self.vm_node} (one attempt)")
        self._tasks_before = self.qmigrate_tasks()
        response, self.attach_log = self._send("attach_disk", [self.vm_cid, self.disk_cid], self.config_path)
        error = response.get("error") if isinstance(response, dict) else {"message": f"no response: {response!r}"}
        if error is not None:
            message = str(error.get("message", "")) if isinstance(error, dict) else json.dumps(error)
            raise RuntimeError("11r attach_disk failed. The CPI said, verbatim:\n" + message + self._evidence())
        self._say("attach_disk=ok")

    def check(self) -> None:
        failures: list[str] = []
        tasks = new_qmigrate_tasks(self._tasks_before, self.qmigrate_tasks(), self.mover)
        if len(tasks) != 1:
            failures.append(f"expected one qmigrate task for mover {self.mover}, found {len(tasks)}: {tasks}")
        elif tasks[0].get("status") != "OK" or tasks[0].get("node") != self.source_node:
            failures.append(f"the qmigrate task did not end OK on {self.source_node}: {tasks[0]}")

        stable_id = _pve_verify.disk_stable_id(self.disk_cid)
        holders = self.holders()
        if [(h[0], h[1]) for h in holders] != [(self.vmid, self.vm_node)]:
            failures.append(f"the disk should be held only by VM {self.vmid} on {self.vm_node}, got "
                            f"{[(h[0], h[1], h[2]) for h in holders]}")
        else:
            _vmid, _node, slot, config = holders[0]
            if f"serial={stable_id}" not in str(config.get(slot, "")):
                failures.append(f"the VM's {slot} does not carry serial {stable_id}: {config.get(slot)!r}")

        if self.journal_on:
            failures.extend(self._kept_mover_failures())
        elif self.mover in self.live_guests():
            kept = kept_mover_warnings(self.attach_log)
            failures.append(f"mover {self.mover} was not destroyed"
                            + (". The CPI warned, verbatim: " + " | ".join(kept) if kept else ""))

        restores = protection_restore_warnings(self.attach_log)
        if restores:
            failures.append("the CPI logged protection-restore warnings, verbatim: " + " | ".join(restores))

        if self.journal_on:
            code, out, _err = self._journal("audit", self.config_path, "--summary")
            findings = audit_summary_findings(code, out)
            if findings:
                failures.append("storage-journal audit --summary is not clean: " + " | ".join(findings))
            else:
                self._say("storage-journal audit --summary is clean")

        if failures:
            raise RuntimeError("11r failed:\n  - " + "\n  - ".join(failures))
        fate = (f"mover kept on {self.vm_node}, empty and protected" if self.journal_on else "mover destroyed")
        self._say(f"11r=ok: one qmigrate OK, disk on {self.vm_node} with serial {stable_id}, {fate}")

    def _kept_mover_failures(self) -> list[str]:
        """Under the journal, the adopted mover stays on the VM's node, empty and protected."""
        failures = adopted_mover_warning_findings(self.attach_log)
        node = self.live_guests().get(self.mover, "")
        if node != self.vm_node:
            where = f"is on {node}" if node else "is gone"
            failures.append(f"mover {self.mover} {where}, and the journal should have kept it on {self.vm_node}")
            return failures
        config = self.verifier.qemu_config(self.mover, node)
        slots = disk_slots(config)
        if slots:
            failures.append(f"mover {self.mover} should hold no disks, and it holds {slots}")
        if str(config.get("protection", "0")).strip() not in ("1", "true", "True"):
            failures.append(f"mover {self.mover} does not have protection back on "
                            f"(protection={config.get('protection')!r})")
        return failures

    # -- cleanup -------------------------------------------------------------

    def cleanup(self) -> list[str]:
        """Remove what this run made. Return what could not be removed.

        Each phase runs on its own, so a failed read in one does not skip the
        next, and the scratch workspace, which holds configs carrying the PVE
        credentials, is removed last whatever happened before it.
        """
        problems: list[str] = []
        try:
            if self.disk_cid:
                try:
                    problems.extend(self._remove_disk())
                except Exception as exc:  # noqa: BLE001 - cleanup reports, it does not stop
                    problems.append(f"disk cleanup stopped: {exc}")
            try:
                problems.extend(self._remove_guests())
            except Exception as exc:  # noqa: BLE001
                problems.append(f"guest cleanup stopped: {exc}")
        finally:
            problems.extend(self._remove_workspace())
        return problems

    def _remove_workspace(self) -> list[str]:
        if self.workspace is None:
            return []
        shutil.rmtree(self.workspace, ignore_errors=True)
        if self.workspace.exists():
            return [f"could not remove the scratch workspace {self.workspace}"]
        return []

    def _remove_disk(self) -> list[str]:
        try:
            holders = self.holders()
        except Exception as exc:  # noqa: BLE001 - cleanup reports, it does not stop
            return [f"could not read where disk {self.disk_cid} is: {exc}"]
        if any(h[0] == self.vmid for h in holders):
            try:
                self._call("detach_disk", [self.vm_cid, self.disk_cid], self.config_path)
            except Exception as exc:  # noqa: BLE001
                self._say(f"11r cleanup: detach_disk failed, verbatim: {exc}")
        errors = []
        for label, path in (("journal", self.config_path), ("plain", self.plain_config_path)):
            if not path:
                continue
            try:
                self._call("delete_disk", [self.disk_cid], path)
            except Exception as exc:  # noqa: BLE001
                errors.append(f"delete_disk under the {label} config failed, verbatim: {exc}")
                continue
            try:
                exists = self.verifier.volume_exists(self.disk_cid)
            except Exception as exc:  # noqa: BLE001
                errors.append(f"delete_disk under the {label} config returned, and the volume check failed: {exc}")
                continue
            if not exists:
                self.disk_cid = ""
                return []
            errors.append(f"delete_disk under the {label} config returned, and the volume is still there")
        # The disk could not be freed through the CPI. Put back the tags of
        # any holder this run tagged, so the disk sits on an ordinary parker
        # that a later delete_disk can clean, and leave it there.
        for vmid, node, _slot, config in self.holders_or_empty():
            original = self.tagged.get(vmid)
            if original is not None and has_tag(config, MOVER_TAG):
                try:
                    self.writer.set_config(node, vmid, {"tags": original})
                except Exception as exc:  # noqa: BLE001
                    errors.append(f"could not restore the tags of holder {vmid}: {exc}")
            errors.append(f"disk {self.disk_cid} is left on VM {vmid} ({node}) for a later delete_disk")
        return errors

    def _remove_guests(self) -> list[str]:
        problems: list[str] = []
        try:
            guests = self.live_guests()
        except Exception as exc:  # noqa: BLE001
            return [f"could not list guests to clean up: {exc}"]
        if self.prefix:
            for vmid, node in sorted(guests.items()):
                if vmid in self.owned or vmid in self.preexisting or not self.band[0] <= vmid <= self.band[1]:
                    continue
                try:
                    config = self.verifier.qemu_config(vmid, node)
                except Exception:  # noqa: BLE001 - a guest we cannot read is not ours to touch
                    continue
                if not run_owned_verdict(vmid, config, prefix=self.prefix, preexisting=self.preexisting,
                                         band=self.band):
                    self._own(vmid)
        for vmid in sorted(self.owned):
            node = guests.get(vmid)
            if not node:
                continue
            try:
                config = self.verifier.qemu_config(vmid, node)
            except Exception as exc:  # noqa: BLE001
                problems.append(f"could not read guest {vmid} to remove it: {exc}")
                continue
            reason = run_owned_verdict(vmid, config, prefix=self.prefix, preexisting=self.preexisting,
                                       band=self.band)
            if reason:
                problems.append(f"left guest {vmid} alone: {reason}")
                continue
            slots = disk_slots(config)
            if slots:
                problems.append(f"left guest {vmid} on {node} in place, because it still holds {slots}")
                continue
            try:
                if str(config.get("protection", "0")).strip() in ("1", "true", "True"):
                    self.writer.set_config(node, vmid, {"protection": "0"})
                self.writer.destroy(node, vmid)
            except Exception as exc:  # noqa: BLE001
                problems.append(f"could not destroy guest {vmid} on {node}: {exc}")
                continue
            try:
                gone = self._gone(vmid)
            except Exception as exc:  # noqa: BLE001
                problems.append(f"could not confirm guest {vmid} is gone after its destroy: {exc}")
                continue
            if not gone:
                problems.append(f"guest {vmid} is still listed after its destroy")
        return problems

    # -- reads ---------------------------------------------------------------

    def live_guests(self) -> dict[int, str]:
        """Every QEMU guest on the lab's nodes, read from each node's own live listing."""
        guests: dict[int, str] = {}
        for node in self.nodes:
            for entry in self.verifier._as_list(self._read(f"/nodes/{urllib.parse.quote(node)}/qemu")):
                try:
                    guests[int(str(entry.get("vmid", "")).strip())] = node
                except ValueError:
                    continue
        return guests

    def holders(self) -> list[tuple[int, str, str, dict[str, Any]]]:
        """Every (vmid, node, slot, config) on the live cluster whose config references the disk."""
        bare = _pve_verify._bare_disk_cid(self.disk_cid)
        stable_id = _pve_verify.disk_stable_id(self.disk_cid)
        found = []
        for vmid, node in sorted(self.live_guests().items()):
            try:
                config = self.verifier.qemu_config(vmid, node)
            except Exception:  # noqa: BLE001 - a guest that moved or vanished mid-scan holds nothing we can prove
                continue
            for key, value in sorted(config.items()):
                if _pve_verify._DISK_SLOT_RE.match(str(key)) and _pve_verify._drive_entry_matches(
                        str(value), bare, stable_id):
                    found.append((vmid, node, str(key), config))
        return found

    def holders_or_empty(self) -> list[tuple[int, str, str, dict[str, Any]]]:
        try:
            return self.holders()
        except Exception:  # noqa: BLE001
            return []

    def qmigrate_tasks(self) -> list[dict[str, Any]]:
        """Every qmigrate task for the mover on every lab node, running ones included."""
        tasks: list[dict[str, Any]] = []
        for node in self.nodes:
            path = (f"/nodes/{urllib.parse.quote(node)}/tasks?vmid={self.mover}&typefilter=qmigrate"
                    "&source=all&limit=1000")
            for entry in self.verifier._as_list(self._read(path)):
                tasks.append(dict(entry, node=entry.get("node") or node))
        return tasks

    # -- helpers -------------------------------------------------------------

    def _own(self, vmid: int) -> None:
        self.owned.add(vmid)
        self.writer.claim(vmid)

    def _gone(self, vmid: int) -> bool:
        for attempt in range(self._gone_attempts):
            if vmid not in self.live_guests():
                return True
            if attempt + 1 < self._gone_attempts:
                self._sleep(2.0)
        return False

    def _write_config(self, name: str, cfg: dict[str, Any]) -> str:
        assert self.workspace is not None
        path = self.workspace / name
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(cfg, handle)
        return str(path)

    def _evidence(self) -> str:
        """What the cluster and the CPI log say after a failed attach, best effort."""
        lines = []
        try:
            tasks = new_qmigrate_tasks(self._tasks_before, self.qmigrate_tasks(), self.mover)
            lines.append(f"qmigrate tasks for mover {self.mover} since the attach began: "
                         + json.dumps([{k: t.get(k) for k in ("node", "status", "upid")} for t in tasks]))
        except Exception as exc:  # noqa: BLE001
            lines.append(f"qmigrate tasks unreadable: {exc}")
        for text in protection_restore_warnings(self.attach_log):
            lines.append("protection-restore WARN: " + text)
        for text in kept_mover_warnings(self.attach_log):
            lines.append("kept-mover WARN: " + text)
        return "".join("\n" + line for line in lines)
