"""Director slots: where a Director's create-env state and creds live.

A slot is one directory holding the files `bosh create-env` keeps for one
Director: state.json (the VM, disk, and stemcell CIDs it owns) and creds.yml
(the generated vars-store). The default slot is manifests/bosh/ in this
checkout, which is where local use has always kept them.

BOSH_STATE_DIR names a different slot. scripts/bosh, scripts/certify, and the
shared director env in _integration.py all honor it, so a second Director (the
upgrade certification run, say) can stand beside the main one without either
command ever reading or rewriting the other's state.

A non-default slot also carries slot.yml, a plain vars file with the values
that must differ per Director:

    internal_ip                the Director's static IP (required; it must sit
                               in the env's reserved band, and it must not be
                               the default slot's IP, the gateway, or the
                               artifacts VM's address)
    pve_create_env_deployment  the deployment segment of the Director VM name
                               (required, and it must differ from the env's)
    bosh_alias                 the bosh CLI alias (default: pve-<dir name>;
                               never 'pve')
    director_name              the Director's name (optional; scripts/bosh reads
                               it from the vars layers, this file's value
                               winning, and names the Director ocfp-mgmt only
                               when no layer sets one)

scripts/bosh layers slot.yml last, after manifests/bosh/vars.yml and the env
bundle's vars.yml, so its keys win everywhere the manifests read them.

The guard here is what makes the certification run safe to point at a lab
whose main Director must survive. A destructive command refuses outright when
its slot is a checkout's default slot or the main checkout's state, when its
state names a Director VM listed in BOSH_PROTECTED_DIRECTOR_CIDS or recorded
by the main Director's state, when it shares a Director or a disk with a
protected state, when its creds.yml is a copy of a protected one, or when its
state records anything that create-env did not write in this slot. That last
check rests on slot-owner.json, which scripts/bosh writes after each
create-env and delete-env in a non-default slot, once the state it records
has passed the location, deny list, and shared Director or disk checks again.
"""

from __future__ import annotations

import hashlib
import ipaddress
import json
import os
import re
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path

import yaml

ENV_VAR = "BOSH_STATE_DIR"
REFUSE_DEFAULT_ENV = "BOSH_REFUSE_DEFAULT_SLOT"
PROTECTED_CIDS_ENV = "BOSH_PROTECTED_DIRECTOR_CIDS"
SLOT_FILE = "slot.yml"
STATE_FILE = "state.json"
CREDS_FILE = "creds.yml"
OWNER_FILE = "slot-owner.json"
DEFAULT_ALIAS = "pve"
DEFAULT_DIRECTOR_NAME = "ocfp-mgmt"
DEFAULT_ENV = "cpitest"
# The artifacts VM's address when its env sets no artifacts_vm_ip; it matches
# _artifacts.DEFAULT_IP, which this module doesn't import to stay stdlib-light.
DEFAULT_ARTIFACTS_IP = "172.31.0.11"
DOCS = "docs/certification/upgrade.md"
# The two tracked files every checkout's manifests/bosh/ holds. A slot
# directory that holds both is some checkout's default slot.
TRACKED_DEFAULT_FILES = ("cpi.yml", "cloud-config.yml")

_ALIAS_UNSAFE = re.compile(r"[^A-Za-z0-9_-]+")
# A Proxmox VMID, which is what the CPI returns as a Director VM's CID.
_VM_CID = re.compile(r"[1-9][0-9]*")
# The bosh flags that point create-env, delete-env, or int at a state or
# vars-store file other than the slot's own.
PATH_FLAGS = ("--state", "--vars-store")
# The public certificates in creds.yml that tell one Director's generated
# credentials from another's. bosh never regenerates a variable that exists,
# so a copied creds.yml keeps them, and two creds files that share the digest
# of any of them are copies. No secret takes part in the comparison.
_CREDS_FINGERPRINT = (
    ("director_ssl", "ca"),
    ("director_ssl", "certificate"),
    ("default_ca", "certificate"),
)

SLOT_REMEDY = (
    "Set BOSH_STATE_DIR to a directory kept only for this Director (for "
    "example ~/.bosh-slots/certification) and put a slot.yml there with its "
    "own internal_ip, then run again."
)


# The recovery steps for a state that this slot's own create-env may have
# written, which is what an owner record that is missing or doesn't match
# looks like. Emptying the slot before the VM is gone strands the slot's
# Director on its IP, so the PVE check comes first.
OWN_STATE_REMEDY = (
    "Confirm in PVE which Director the VM or disk the state records belongs "
    "to. If it carries this slot's Director name and IP, delete it there by "
    "hand and then empty the slot, because emptying the slot first would "
    "leave that Director running on the slot's IP. If it belongs to another "
    "slot's Director, empty the slot and leave that VM alone. The "
    f'"Destructive by design" section of {DOCS} has the steps.'
)


class SlotError(RuntimeError):
    """A slot's configuration is unusable; the message says what to fix."""


class OwnerRefusedError(SlotError):
    """The state a command left in a slot fails the guard's cross-checks."""


@dataclass(frozen=True)
class Slot:
    dir: Path
    default_dir: Path

    @property
    def state(self) -> Path:
        return self.dir / STATE_FILE

    @property
    def creds(self) -> Path:
        return self.dir / CREDS_FILE

    @property
    def config(self) -> Path:
        return self.dir / SLOT_FILE

    @property
    def owner(self) -> Path:
        return self.dir / OWNER_FILE

    @property
    def is_default(self) -> bool:
        return _same_path(self.dir, self.default_dir)

    def values(self) -> dict:
        """slot.yml as a mapping; {} when the file is absent."""
        if not self.config.exists():
            return {}
        try:
            data = yaml.safe_load(self.config.read_text(encoding="utf-8"))
        except UnicodeDecodeError:
            raise SlotError(
                f"cannot read {self.config}: it is not valid UTF-8 text. Fix the "
                "file or remove it."
            ) from None
        except (ValueError, RecursionError):
            raise SlotError(
                f"cannot read {self.config}: it holds a value YAML can't load. "
                "Fix the file or remove it."
            ) from None
        except (OSError, yaml.YAMLError) as exc:
            raise SlotError(
                f"cannot read {self.config}: {exc}. Fix the file or remove it."
            ) from exc
        if data is None:
            return {}
        if not isinstance(data, dict):
            raise SlotError(
                f"{self.config} must be a YAML mapping of vars "
                "(internal_ip, bosh_alias, director_name, pve_create_env_deployment)."
            )
        return data

    def value(self, key: str) -> str:
        raw = self.values().get(key)
        return "" if raw is None else str(raw).strip()

    @property
    def alias(self) -> str:
        """The bosh CLI alias for this slot's Director.

        The default slot keeps 'pve' so local use is unchanged. Any other slot
        gets its own alias, because `alias-env` stores the Director's address
        and credentials under the alias, and sharing one would point the main
        slot's commands at the wrong Director.
        """
        if self.is_default:
            return DEFAULT_ALIAS
        configured = self.value("bosh_alias")
        if configured:
            return configured
        base = _ALIAS_UNSAFE.sub("-", self.dir.name).strip("-") or "slot"
        return f"{DEFAULT_ALIAS}-{base}"

    @property
    def director_name(self) -> str:
        return self.value("director_name") or DEFAULT_DIRECTOR_NAME

    def vars_layer(self) -> list[str]:
        """`-l slot.yml` when a non-default slot carries one, else []."""
        if self.is_default or not self.config.exists():
            return []
        return ["-l", str(self.config)]


def _same_path(a: Path, b: Path) -> bool:
    """True when two paths name the same file or directory.

    realpath catches symlinks and `..`; samefile also catches hard links and
    case-insensitive filesystems once both exist.
    """
    if os.path.realpath(a) == os.path.realpath(b):
        return True
    try:
        return a.exists() and b.exists() and os.path.samefile(a, b)
    except OSError:
        return False


def resolve(repo_root: Path, environ: "dict[str, str] | None" = None) -> Slot:
    """The slot BOSH_STATE_DIR names, or the default slot when it is unset.

    A relative BOSH_STATE_DIR resolves against the current directory, the
    same way a shell would read it.
    """
    env = os.environ if environ is None else environ
    default_dir = Path(repo_root) / "manifests" / "bosh"
    raw = (env.get(ENV_VAR) or "").strip()
    if not raw:
        return Slot(dir=default_dir, default_dir=default_dir)
    chosen = Path(os.path.expanduser(raw))
    if not chosen.is_absolute():
        chosen = Path.cwd() / chosen
    return Slot(dir=Path(os.path.normpath(chosen)), default_dir=default_dir)


def refuse_mode(environ: "dict[str, str] | None" = None) -> bool:
    """True when BOSH_REFUSE_DEFAULT_SLOT asks every command to refuse the default slot."""
    env = os.environ if environ is None else environ
    return (env.get(REFUSE_DEFAULT_ENV) or "").strip() not in ("", "0")


def protected_cids(environ: "dict[str, str] | None" = None) -> list[str]:
    """The Director VM CIDs BOSH_PROTECTED_DIRECTOR_CIDS lists, in order.

    The value is a comma-separated list of positive decimal integers, the
    bare Proxmox VMIDs, with whitespace allowed around each one, so
    " 3808, 3809 " names two VMs. A value that is empty or only whitespace
    lists none. Anything else, an empty entry or a trailing comma included,
    raises SlotError, because an entry no VM CID can equal would leave the
    deny list silently protecting nothing.
    """
    env = os.environ if environ is None else environ
    raw = env.get(PROTECTED_CIDS_ENV) or ""
    if not raw.strip():
        return []
    cids: list[str] = []
    for position, part in enumerate(raw.split(","), start=1):
        entry = part.strip()
        if not _VM_CID.fullmatch(entry):
            raise SlotError(
                f"{PROTECTED_CIDS_ENV} entry {position} is not a Proxmox VMID. The "
                "variable must be a comma-separated list of positive whole "
                "numbers with nothing else in it, such as 3808 or 3808,3809"
            )
        cids.append(entry)
    return cids


# --------------------------------------------------------------------------- #
# Which directories are some checkout's default slot
# --------------------------------------------------------------------------- #

def checkout_default_slot(directory: Path) -> bool:
    """True when a directory is the manifests/bosh/ of some checkout.

    That is any directory named bosh whose parent is named manifests and whose
    grandparent holds .git (a directory in a clone, a file in a linked
    worktree), and any directory that holds the tracked cpi.yml and
    cloud-config.yml. Such a directory is where that checkout keeps its main
    Director's state, whichever checkout it is.
    """
    real = Path(os.path.realpath(directory))
    if (real.name == "bosh" and real.parent.name == "manifests"
            and (real.parent.parent / ".git").exists()):
        return True
    return all((real / name).exists() for name in TRACKED_DEFAULT_FILES)


def _git(repo_root: Path, *argv: str) -> str:
    """stdout of a git command in repo_root; SlotError when it fails."""
    try:
        proc = subprocess.run(
            ["git", "-C", str(repo_root), *argv], capture_output=True, text=True,
        )
    except OSError as exc:
        raise SlotError(f"git could not run in {repo_root} ({exc})") from exc
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout).strip().splitlines()
        reason = detail[0] if detail else f"exit {proc.returncode}"
        raise SlotError(f"`git {argv[0]}` failed in {repo_root} ({reason})")
    return proc.stdout


def main_checkout_root(repo_root: Path) -> "Path | None":
    """The main checkout's root for the checkout at repo_root.

    `git worktree list --porcelain` names the main worktree first. Returns
    None when repo_root is not a git checkout at all (it has no .git) or when
    the repository is bare and so has no main checkout. When repo_root has a
    .git but git can't say which checkout is the main one, this raises
    SlotError, because the main checkout's state then can't be ruled out.
    """
    if not (Path(repo_root) / ".git").exists():
        return None
    listing = _git(repo_root, "worktree", "list", "--porcelain")
    first_block = listing.strip().split("\n\n", 1)[0].splitlines()
    if not first_block or not first_block[0].startswith("worktree "):
        raise SlotError(f"`git worktree list` in {repo_root} named no worktree")
    if "bare" in first_block[1:]:
        return None
    main = Path(first_block[0][len("worktree "):])
    if (main / ".git").exists():
        return main
    # A clone made with --separate-git-dir lists its git directory here
    # instead of its checkout. When repo_root is that main checkout, git says
    # so directly. From a linked worktree of such a clone there is no way to
    # find the main checkout, so the guard refuses.
    git_dir, common_dir, toplevel = _git(
        repo_root, "rev-parse", "--path-format=absolute",
        "--git-dir", "--git-common-dir", "--show-toplevel",
    ).splitlines()[:3]
    if _same_path(Path(git_dir), Path(common_dir)):
        return Path(toplevel)
    raise SlotError(
        f"git names {main} as the main worktree of {repo_root}, but it holds no "
        ".git, so the main checkout can't be found"
    )


# --------------------------------------------------------------------------- #
# State files and the owner record
# --------------------------------------------------------------------------- #

def read_state(state_path: Path) -> "dict | None":
    """A create-env state file as a mapping.

    Returns None when the file is absent and {} when it is empty. Raises
    SlotError when it exists but can't be read or isn't a JSON object, so a
    caller can refuse instead of guessing what Director it records.
    """
    if not state_path.exists():
        return None
    try:
        raw = state_path.read_text(encoding="utf-8").strip()
    except OSError as exc:
        raise SlotError(f"cannot read {state_path} ({exc.strerror or exc})") from exc
    except UnicodeDecodeError:
        raise SlotError(f"{state_path} is not valid UTF-8 text") from None
    if not raw:
        return {}
    try:
        data = json.loads(raw)
    except ValueError as exc:
        raise SlotError(f"{state_path} is not valid JSON") from exc
    if not isinstance(data, dict):
        raise SlotError(f"{state_path} is not a JSON object")
    return data


def _text(value: object) -> str:
    return "" if value is None else str(value).strip()


def _entry_cids(entries: object) -> list[str]:
    """The sorted, distinct `cid` values of a state's disks or stemcells list."""
    if not isinstance(entries, list):
        return []
    cids = {_text(entry.get("cid")) for entry in entries if isinstance(entry, dict)}
    return sorted(cid for cid in cids if cid)


def state_vm_cid(state: "dict | None") -> str:
    """The Director VM CID a parsed state records, or ""."""
    if not state:
        return ""
    return _text(state.get("current_vm_cid"))


def state_is_empty(state: "dict | None") -> bool:
    """True when a parsed state is absent or every value in it is empty.

    create-env writes director_id and installation_id before it builds
    anything, and a create-env that fails partway can leave a disk, a
    stemcell, or a director_id behind without a VM. Each of those makes the
    state non-empty, because the next create-env or delete-env acts on it.
    """
    return not state or not any(state.values())


def state_identity(state: "dict | None") -> dict:
    """What a create-env state names: its Director, VM, disks, and stemcells.

    This is the content of the owner record, and the guard compares it with
    the record to tell a state this slot's create-env wrote from one that came
    from anywhere else.
    """
    state = state or {}
    return {
        "director_id": _text(state.get("director_id")),
        "current_vm_cid": _text(state.get("current_vm_cid")),
        "current_disk_id": _text(state.get("current_disk_id")),
        "disk_cids": _entry_cids(state.get("disks")),
        "stemcell_cids": _entry_cids(state.get("stemcells")),
    }


def _owner_identity(record: dict) -> dict:
    """An owner record read back in the shape state_identity returns."""
    def cids(value: object) -> list[str]:
        if not isinstance(value, list):
            return []
        return sorted({_text(v) for v in value if _text(v)})
    return {
        "director_id": _text(record.get("director_id")),
        "current_vm_cid": _text(record.get("current_vm_cid")),
        "current_disk_id": _text(record.get("current_disk_id")),
        "disk_cids": cids(record.get("disk_cids")),
        "stemcell_cids": cids(record.get("stemcell_cids")),
    }


def current_vm_cid(state_path: Path) -> str:
    """The Director VM CID a create-env state file records, or "".

    Raises SlotError when the file exists but can't be read.
    """
    return state_vm_cid(read_state(state_path))


def read_owner(slot: Slot) -> "dict | None":
    """slot-owner.json as a mapping, or None when the slot has none."""
    if not slot.owner.exists():
        return None
    try:
        data = json.loads(slot.owner.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise SlotError(f"cannot read {slot.owner}") from exc
    if not isinstance(data, dict):
        raise SlotError(f"{slot.owner} is not a JSON object")
    return data


def write_owner(slot: Slot, main_root: "Path | None",
                environ: "dict[str, str] | None" = None) -> bool:
    """Record that this slot's own create-env or delete-env wrote its state.

    scripts/bosh calls this after every create-env and delete-env in a
    non-default slot, once the guard has let the command run, so the state was
    empty or already owned before it ran. The command can take minutes, and
    another process can put a state in the slot meanwhile, so the state about
    to be recorded goes through the guard's location, deny list, and shared
    Director or disk checks again. main_root is the main checkout, or None
    when the repository has none.

    Writes state_identity() of the state and returns True, or returns False
    and leaves any earlier record alone when the state is absent or empty.
    Raises OwnerRefusedError, writing nothing, when the state fails a check,
    and SlotError when the state can't be read or the record can't be written.
    """
    if slot.is_default:
        return False
    state = read_state(slot.state)
    if state_is_empty(state):
        return False
    protected = protected_states(slot, main_root)
    found = None
    if _location_reason(slot, protected) is not None:
        found = ("the slot's directory or state path now resolving to a "
                 "Director state that belongs to a checkout's main Director",
                 False)
    else:
        conflict = _conflict(slot, protected, protected_cids(environ), state)
        if conflict is not None:
            found = (_conflict_summary(conflict), conflict[0] == "listed")
    if found is not None:
        summary, listed_only = found
        message = (
            f"will not record {slot.state} as this slot's own. This command ran "
            "in this slot and may have created or changed a Director VM on the "
            f"slot's internal_ip, so do not remove {STATE_FILE} yet. The check "
            f"found {summary}. {OWN_STATE_REMEDY}"
        )
        if listed_only:
            message += (
                f" The VMID in {PROTECTED_CIDS_ENV} may be stale and reassigned "
                "to this slot's Director, so check in PVE whether that VM "
                "carries this slot's Director name and IP before you update "
                "the variable."
            )
        raise OwnerRefusedError(message)
    record = state_identity(state)
    fd, tmp_name = tempfile.mkstemp(dir=slot.dir, prefix=f".{OWNER_FILE}.", suffix=".tmp")
    tmp = Path(tmp_name)
    try:
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                json.dump(record, fh, indent=2, sort_keys=True)
                fh.write("\n")
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(tmp, slot.owner)
        except BaseException:
            tmp.unlink(missing_ok=True)
            raise
    except OSError as exc:
        raise SlotError(f"cannot write {slot.owner} ({exc.strerror or exc})") from exc
    return True


# --------------------------------------------------------------------------- #
# The guard
# --------------------------------------------------------------------------- #

def protected_states(slot: Slot, main_root: "Path | None") -> list[Path]:
    """State files a non-default slot must never be, or share a Director with."""
    paths = [slot.default_dir / STATE_FILE]
    if main_root is not None:
        main_state = main_root / "manifests" / "bosh" / STATE_FILE
        if not any(_same_path(main_state, p) for p in paths):
            paths.append(main_state)
    return paths


def _read_creds(path: Path) -> "dict | None":
    """A creds.yml as a mapping, or None when the file is absent.

    Raises SlotError when it exists but can't be read or parsed. The error
    never quotes the file, because a YAML error can carry a line of it.
    """
    if not path.exists():
        return None
    try:
        data = yaml.safe_load(path.read_text(encoding="utf-8"))
    except OSError as exc:
        raise SlotError(f"cannot read {path} ({exc.strerror or 'read failed'})") from None
    except UnicodeDecodeError:
        raise SlotError(f"{path} is not valid UTF-8 text") from None
    except (yaml.YAMLError, ValueError, RecursionError):
        raise SlotError(f"{path} is not valid YAML") from None
    if data is None:
        return {}
    if not isinstance(data, dict):
        raise SlotError(f"{path} is not a YAML mapping")
    return data


def _creds_fingerprint(creds: dict) -> "set[str]":
    """SHA-256 digests of the public certificates a creds mapping holds.

    Only the certificates named in _CREDS_FINGERPRINT are hashed, so the
    comparison never holds a password or a private key beyond the parse.
    """
    digests: set[str] = set()
    for keys in _CREDS_FINGERPRINT:
        node: object = creds
        for key in keys:
            node = node.get(key) if isinstance(node, dict) else None
        if isinstance(node, str) and node.strip():
            digests.add(hashlib.sha256(node.strip().encode("utf-8")).hexdigest())
    return digests


def _creds_digests(path: Path) -> "set[str] | None":
    """The certificate digests of a creds.yml, or None when the file is absent.

    The parsed mapping goes out of scope before this returns, so the caller
    holds only digests. Raises SlotError when the file can't be read.
    """
    creds = _read_creds(path)
    if creds is None:
        return None
    return _creds_fingerprint(creds)


def _creds_copy_reason(slot: Slot, protected: "list[Path]") -> "str | None":
    """Why the slot's creds.yml looks like a protected Director's, or None.

    A creds.yml copied in from the main Director's slot hands every Director
    command in this slot the main Director's credentials, so a command that
    reaches the main Director through a wrong alias would succeed. The
    comparison runs in memory, on digests of the two files' public
    certificates, and never prints either file.
    """
    try:
        fingerprint = _creds_digests(slot.creds)
    except SlotError as exc:
        return (
            f"{exc}, so there is no way to check that it isn't a copy of the "
            "main Director's creds. Fix the file, or remove it if the slot "
            "holds no Director."
        )
    if not fingerprint:
        return None
    for state_path in protected:
        other_path = state_path.with_name(CREDS_FILE)
        try:
            other = _creds_digests(other_path)
        except SlotError as exc:
            return (
                f"{exc}, so there is no way to check that {slot.creds} isn't a "
                "copy of it. Fix or restore that file, then run again."
            )
        if other and fingerprint & other:
            return (
                f"{slot.creds} holds the same CA certificates as "
                f"{other_path}, so it is a copy of the main Director's creds, and "
                "a Director command here could reach the main Director with "
                "them. Remove creds.yml from this slot, along with any state.json "
                "that this slot's create-env didn't write, and run again."
            )
    return None


def _location_reason(slot: Slot, protected: "list[Path]") -> "str | None":
    """Why the slot's directory is some checkout's own Director slot, or None."""
    if checkout_default_slot(slot.dir):
        return (
            f"The slot {slot.dir} is a checkout's manifests/bosh directory, where "
            f"that checkout keeps its main Director's state. {SLOT_REMEDY}"
        )
    for path in protected:
        if _same_path(slot.state, path) or _same_path(slot.dir, path.parent):
            return (
                f"The Director state path {slot.state} resolves to {path}, the "
                f"main checkout's Director state. {SLOT_REMEDY}"
            )
    return None


def _conflict(slot: Slot, protected: "list[Path]", listed: "list[str]",
              state: "dict | None") -> "tuple[str, Path | None, str] | None":
    """What a parsed state shares with a protected Director, or None.

    Returns (kind, path, reason). The kind is "unreadable" when a protected
    state can't be read, "listed" for a VM the deny list names, "vm" for the
    VM a protected state records, or "shared" for a Director or persistent
    disk that a protected state records. The path is the protected state
    involved, when there is one, and the reason is the guard's refusal text.

    The deny set is the listed VM CIDs plus the VM each protected state
    records.
    """
    mine = state_identity(state)
    cid = mine["current_vm_cid"]

    others: list[tuple[Path, dict]] = []
    for path in protected:
        try:
            others.append((path, state_identity(read_state(path))))
        except SlotError as exc:
            return ("unreadable", path, (
                f"{exc}, so there is no way to check that {slot.state} isn't a "
                "copy of it. Fix or restore that file, then run again."
            ))

    # The deny set is the variable's list plus the VM each protected state
    # records now. The variable goes stale when create-env rebuilds the main
    # Director under a new VMID, and the main state keeps up on its own.
    denied: dict[str, "Path | None"] = {listed_cid: None for listed_cid in listed}
    for path, other in others:
        if other["current_vm_cid"]:
            denied.setdefault(other["current_vm_cid"], path)
    if cid and cid in denied:
        source = denied[cid]
        if source is None:
            return ("listed", None, (
                f"{slot.state} records a Director VM that {PROTECTED_CIDS_ENV} "
                "protects, so it is another Director's state. Remove state.json and "
                "creds.yml from this slot, leave that VM alone, and run again."
            ))
        reason = (
            f"{slot.state} records the same Director VM as {source}, so it is a "
            "copy of the main Director's state rather than a slot of its own. "
            "Remove state.json and creds.yml from this slot and run again."
        )
        if listed:
            reason += (
                f" {PROTECTED_CIDS_ENV} doesn't list that VM, so the variable is "
                "out of date. Each create-env that rebuilds the main Director "
                "gives it a new VMID, so set the variable to the VMID the main "
                "Director has now."
            )
        return ("vm", source, reason)

    for path, other in others:
        same_director = bool(mine["director_id"]) and mine["director_id"] == other["director_id"]
        shared_disks = set(mine["disk_cids"]) & set(other["disk_cids"])
        if same_director or shared_disks:
            return ("shared", path, (
                f"{slot.state} records the same Director or persistent disk as "
                f"{path}, so it is a copy of the main Director's state, such as "
                "one taken while that Director was being rebuilt. Remove "
                "state.json and creds.yml from this slot, leave that Director's "
                "VM and disk alone, and run again."
            ))
    return None


def _conflict_reason(slot: Slot, protected: "list[Path]", listed: "list[str]",
                     state: "dict | None") -> "str | None":
    """Why a parsed state shares a Director, VM, or disk with a protected one.

    Used by the guard for the slot's current state. write_owner uses
    _conflict directly, because its remedy differs.
    """
    conflict = _conflict(slot, protected, listed, state)
    return None if conflict is None else conflict[2]


def _conflict_summary(conflict: "tuple[str, Path | None, str]") -> str:
    """What a conflict found, as a noun phrase that quotes no file content."""
    kind, path, _reason = conflict
    if kind == "unreadable":
        return (f"that the protected state {path} can't be read, so the state "
                "can't be ruled out as a copy of it")
    if kind == "listed":
        return f"a Director VM that {PROTECTED_CIDS_ENV} lists"
    if kind == "vm":
        return f"the same Director VM as {path}"
    return f"the same Director or persistent disk as {path}"


def refusal_reason(slot: Slot, main_root: "Path | None",
                   environ: "dict[str, str] | None" = None, *,
                   require_deny_list: bool = True) -> "str | None":
    """Why a destructive command must not run against this slot, or None.

    Each reason is a full sentence that says what the guard found and what to
    do about it. The guard never quotes a state or creds file's contents.
    """
    if slot.is_default:
        return (
            f"The Director state path {slot.state} resolves to the default slot "
            f"{slot.default_dir}, which holds the main Director's state. {SLOT_REMEDY}"
        )
    protected = protected_states(slot, main_root)
    reason = _location_reason(slot, protected)
    if reason is not None:
        return reason

    try:
        listed = protected_cids(environ)
    except SlotError as exc:
        return (
            f"{exc}. Fix the variable, and in CI the repository variable of the "
            "same name, then run again."
        )
    if require_deny_list and not listed:
        return (
            f"{PROTECTED_CIDS_ENV} is empty, so nothing names the Director VMs "
            "this command must never touch. Set it to a comma-separated list "
            "of their VM CIDs (on Proxmox, the VMID, such as 100), and in CI "
            "set the repository variable of the same name."
        )

    try:
        state = read_state(slot.state)
    except SlotError as exc:
        return (
            f"{exc}, so there is no way to tell which Director it records. "
            f"Restore state.json and {OWNER_FILE} together from the run "
            "directory's backup, or move both aside if the slot holds no "
            "Director."
        )
    reason = _conflict_reason(slot, protected, listed, state)
    if reason is not None:
        return reason

    if not state_is_empty(state):
        try:
            owner = read_owner(slot)
        except SlotError as exc:
            owner = None
            unreadable = str(exc)
        else:
            unreadable = ""
        if owner is None:
            missing = unreadable or f"{slot.owner} is missing"
            return (
                f"{slot.state} records a Director, but {missing}, so the state "
                "was not created in this slot. A state copied, moved, or linked in "
                "from another Director's slot looks exactly like this, and so "
                "does a slot that a release without this guard built. "
                f"{OWN_STATE_REMEDY}"
            )
        if _owner_identity(owner) != state_identity(state):
            return (
                f"{slot.state} records a different Director, VM, disk, or "
                f"stemcell from the ones create-env left in this slot "
                f"({slot.owner}), so the state was not created in this slot. A "
                "state copied in from another slot looks like this, and so does "
                "this slot's own create-env or delete-env that was interrupted, "
                "or one that an earlier release of this guard recorded with fewer "
                f"details. {OWN_STATE_REMEDY}"
            )

    return _creds_copy_reason(slot, protected)


def refusal_message(action: str, slot: Slot, reason: str) -> str:
    return (
        f"refusing to {action} in the Director slot {slot.dir}. {reason} A "
        "command here could delete or replace the Director that the slot's "
        f"state records. See {DOCS}."
    )


def guard(action: str, slot: Slot, main_root: "Path | None",
          environ: "dict[str, str] | None" = None, *,
          require_deny_list: bool = True) -> "str | None":
    """The refusal message for a destructive action, or None when it may run."""
    reason = refusal_reason(slot, main_root, environ,
                            require_deny_list=require_deny_list)
    return None if reason is None else refusal_message(action, slot, reason)


def guard_repo(action: str, slot: Slot, repo_root: Path,
               environ: "dict[str, str] | None" = None, *,
               require_deny_list: bool = True) -> "str | None":
    """guard() with the main checkout looked up from repo_root.

    When git can't say which checkout is the main one, the command refuses,
    because the main checkout's state can't be ruled out.
    """
    try:
        main_root = main_checkout_root(repo_root)
    except SlotError as exc:
        return refusal_message(
            action, slot,
            f"{exc}, so the main checkout's Director state can't be ruled out. "
            "Run from a working git checkout of this repository.",
        )
    return guard(action, slot, main_root, environ,
                 require_deny_list=require_deny_list)


def path_arguments(args: "list[str]") -> "list[tuple[str, str]]":
    """Each --state or --vars-store argument in a bosh argv, as (flag, value).

    bosh takes both `--state=PATH` and `--state PATH`. A flag with nothing
    after it comes back with an empty value.
    """
    found: list[tuple[str, str]] = []
    index = 0
    while index < len(args):
        arg = str(args[index])
        for flag in PATH_FLAGS:
            if arg == flag:
                value = str(args[index + 1]) if index + 1 < len(args) else ""
                found.append((flag, value))
                index += 1
                break
            if arg.startswith(f"{flag}="):
                found.append((flag, arg[len(flag) + 1:]))
                break
        index += 1
    return found


def explicit_path_problem(args: "list[str]", slot: Slot, main_root: "Path | None",
                          base_dir: Path) -> "str | None":
    """Why a --state or --vars-store argument must not reach bosh, or None.

    scripts/bosh hands create-env and delete-env the slot's own state.json and
    creds.yml, and bosh takes the last --state it is given, so an extra one
    would act on a file the guard never checked. A path that resolves to a
    protected state or its creds.yml, or anywhere outside the slot's
    directory, is refused. A relative path resolves against base_dir, the
    directory scripts/bosh runs bosh in.
    """
    protected = protected_states(slot, main_root)
    slot_real = os.path.realpath(slot.dir)
    for flag, value in path_arguments(args):
        if not value:
            return (
                f"{flag} has no path after it. Leave it out, because scripts/bosh "
                f"already passes the slot's own {STATE_FILE} and {CREDS_FILE}."
            )
        path = Path(os.path.expanduser(value))
        if not path.is_absolute():
            path = Path(base_dir) / path
        for state in protected:
            if _same_path(path, state) or _same_path(path, state.with_name(CREDS_FILE)):
                return (
                    f"{flag} names {path}, which resolves to the main Director's "
                    f"files in {state.parent}. Leave {flag} out, because "
                    f"scripts/bosh already passes the slot's own {STATE_FILE} and "
                    f"{CREDS_FILE}."
                )
        real = os.path.realpath(path)
        if os.path.commonpath([real, slot_real]) != slot_real:
            return (
                f"{flag} names {path}, which is outside the Director slot "
                f"{slot.dir}, so the guard never checked it. Leave {flag} out, "
                f"because scripts/bosh already passes the slot's own {STATE_FILE} "
                f"and {CREDS_FILE}."
            )
    return None


def explicit_path_refusal(action: str, args: "list[str]", slot: Slot,
                          repo_root: Path) -> "str | None":
    """explicit_path_problem() as a refusal message, with the main checkout
    looked up from repo_root. bosh runs in repo_root, so relative paths
    resolve there. When git can't name the main checkout, an argv that
    carries either flag is refused.
    """
    if not path_arguments(args):
        return None
    try:
        main_root = main_checkout_root(repo_root)
    except SlotError as exc:
        return refusal_message(
            action, slot,
            f"{exc}, so there is no way to check where --state or --vars-store "
            "points. Run from a working git checkout of this repository.",
        )
    problem = explicit_path_problem(args, slot, main_root, repo_root)
    return None if problem is None else refusal_message(action, slot, problem)


# --------------------------------------------------------------------------- #
# slot.yml checks
# --------------------------------------------------------------------------- #

@dataclass(frozen=True)
class EnvFacts:
    """What the env bundle and the base vars say about the main Director."""

    env_name: str = DEFAULT_ENV
    default_ip: str = ""
    gateway: str = ""
    deployment: str = ""
    reserved: "list[str] | None" = None
    artifacts_ip: str = ""

    @property
    def reserved_key(self) -> str:
        return f"{self.env_name.replace('-', '_')}_reserved"


def _load_vars(path: Path) -> dict:
    if not path.exists():
        return {}
    try:
        data = yaml.safe_load(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, yaml.YAMLError):
        return {}
    return data if isinstance(data, dict) else {}


def env_facts(repo_root: Path, env_name: str = "") -> EnvFacts:
    """Read the main Director's IP, gateway, VM name segment, and bands.

    The env bundle's vars.yml wins over manifests/bosh/vars.yml, the same way
    scripts/bosh layers them. Both are plain vars files. A value neither file
    sets stays empty, and config_problem then fails closed on it.

    A checkout without manifests/bosh/vars.yml reads vars.yml.example in its
    place. No Director command can run in such a checkout, because every
    create-env and render passes vars.yml to bosh, so the example's values
    only stand in for a dry run.
    """
    env_name = (env_name or "").strip() or DEFAULT_ENV
    bosh_dir = Path(repo_root) / "manifests" / "bosh"
    base_file = bosh_dir / "vars.yml"
    if not base_file.exists():
        base_file = bosh_dir / "vars.yml.example"
    base = _load_vars(base_file)
    env_dir = Path(repo_root) / "manifests" / "envs" / env_name
    layered = {**base, **_load_vars(env_dir / "vars.yml")}
    artifacts = _load_vars(env_dir / "artifacts.yml")

    def text(key: str, source: dict = layered) -> str:
        raw = source.get(key)
        return "" if raw is None else str(raw).strip()

    facts = EnvFacts(env_name=env_name)
    reserved = layered.get(facts.reserved_key)
    artifacts_ip = (text("artifacts_vm_ip") or text("artifacts_vm_ip", artifacts)
                    or DEFAULT_ARTIFACTS_IP)
    return EnvFacts(
        env_name=env_name,
        default_ip=text("internal_ip"),
        gateway=text("internal_gw"),
        deployment=text("pve_create_env_deployment"),
        reserved=[str(r) for r in reserved] if isinstance(reserved, list) else None,
        artifacts_ip=artifacts_ip,
    )


def _addresses(entries: "list[str]") -> "list[tuple[int, int]]":
    """Inclusive (first, last) integer ranges for reserved-style entries.

    An entry is one address, a range "a-b", or a CIDR. Raises ValueError for
    anything else.
    """
    spans: list[tuple[int, int]] = []
    for entry in entries:
        text = str(entry).strip()
        if "-" in text:
            lo, _, hi = text.partition("-")
            spans.append((int(ipaddress.ip_address(lo.strip())),
                          int(ipaddress.ip_address(hi.strip()))))
        elif "/" in text:
            net = ipaddress.ip_network(text, strict=False)
            spans.append((int(net.network_address), int(net.broadcast_address)))
        else:
            value = int(ipaddress.ip_address(text))
            spans.append((value, value))
    return spans


def _inside(ip: str, entries: "list[str]") -> bool:
    value = int(ipaddress.ip_address(ip))
    return any(lo <= value <= hi for lo, hi in _addresses(entries))


def config_problem(slot: Slot, facts: EnvFacts) -> "str | None":
    """What is wrong with a non-default slot's slot.yml, or None.

    A Director without its own IP would come up on the main Director's
    address, and the duplicate IP would knock both off the network. So a
    non-default slot must name an internal_ip that sits in the env's reserved
    band, where no deployment's dynamic placement lands, and that isn't the
    main Director's, the gateway's, or the artifacts VM's. It must also name
    its own VM name segment and keep off the main Director's alias. Whenever a
    value it is compared against can't be read, the check fails closed,
    because nothing then proves the two differ.
    """
    if slot.is_default:
        return None
    if not slot.config.exists():
        return (
            f"{slot.config} is missing. A Director slot other than the default "
            "needs its own internal_ip and pve_create_env_deployment there (plus "
            "optional bosh_alias and director_name), so it never comes up on the "
            f"main Director's address or under its VM name. See {DOCS}."
        )
    try:
        ip = slot.value("internal_ip")
        alias = slot.value("bosh_alias")
        deployment = slot.value("pve_create_env_deployment")
        slot_reserved = slot.values().get(facts.reserved_key)
    except SlotError as exc:
        return str(exc)
    if not ip:
        return (
            f"{slot.config} sets no internal_ip. Give this Director a free "
            f"static IP of its own on the env network. See {DOCS}."
        )
    try:
        ipaddress.ip_address(ip)
    except ValueError:
        return f"{slot.config} sets internal_ip to {ip!r}, which is not an IP address."
    if not facts.default_ip:
        return (
            "cannot read the default slot's Director IP (internal_ip in the env "
            "bundle's vars.yml or manifests/bosh/vars.yml), so there is no way to "
            f"check that {slot.config} picks a different one. Make sure "
            "manifests/bosh/vars.yml exists and sets internal_ip."
        )
    if ip == facts.default_ip:
        return (
            f"{slot.config} sets internal_ip to {ip}, which is the default "
            "slot's Director IP. Pick a free address on the same network that "
            "no other Director or static VM uses."
        )
    if not facts.gateway:
        return (
            "cannot read the env's gateway (internal_gw in the env bundle's "
            "vars.yml or manifests/bosh/vars.yml), so there is no way to check "
            f"that {slot.config} doesn't put the Director on it. Make sure the "
            "env bundle or manifests/bosh/vars.yml sets internal_gw."
        )
    for label, claimed in (("the env's gateway (internal_gw)", facts.gateway),
                           ("the artifacts VM's address", facts.artifacts_ip)):
        if claimed and ip == claimed:
            return (
                f"{slot.config} sets internal_ip to {ip}, which is {label}. "
                "Pick a free address in the env's reserved band that nothing "
                f"else claims. See {DOCS}."
            )
    if facts.reserved is None:
        return (
            f"cannot read {facts.reserved_key} from the {facts.env_name} env "
            "bundle's vars.yml, so there is no way to check that "
            f"{slot.config}'s internal_ip sits where no deployment's dynamic "
            "placement can land. Set BOSH_PVE_ENV to an env bundle that defines "
            "its reserved bands."
        )
    try:
        in_env_band = _inside(ip, facts.reserved)
        in_slot_band = (_inside(ip, [str(r) for r in slot_reserved])
                        if isinstance(slot_reserved, list) else True)
    except ValueError:
        return (
            f"cannot parse the reserved bands for {facts.env_name}, so there is "
            f"no way to check {slot.config}'s internal_ip. Fix {facts.reserved_key}."
        )
    if not (in_env_band and in_slot_band):
        return (
            f"{slot.config} sets internal_ip to {ip}, which is outside the "
            f"env's reserved band ({facts.reserved_key}), so a deployment's "
            "dynamic placement could take it. Pick a free address inside that "
            f"band. See {DOCS}."
        )
    if alias == DEFAULT_ALIAS:
        return (
            f"{slot.config} sets bosh_alias to '{DEFAULT_ALIAS}', the main "
            "Director's alias, and alias-env would repoint it at this slot's "
            "Director. Pick another alias, or leave bosh_alias out to get "
            f"{DEFAULT_ALIAS}-<slot directory name>."
        )
    if not deployment:
        return (
            f"{slot.config} sets no pve_create_env_deployment, so this Director's "
            "VM would carry the main Director's name segment and share its "
            "create-env pool. Set it to a value of its own, such as "
            "create-env-certification."
        )
    if not facts.deployment:
        return (
            "cannot read the main Director's pve_create_env_deployment (in the "
            "env bundle's vars.yml or manifests/bosh/vars.yml), so there is no "
            f"way to check that {slot.config} picks a different one. Make sure "
            "manifests/bosh/vars.yml exists and sets it."
        )
    if deployment == facts.deployment:
        return (
            f"{slot.config} sets pve_create_env_deployment to the main "
            f"Director's value, {deployment}. Pick a value of its own, such as "
            "create-env-certification."
        )
    return None
