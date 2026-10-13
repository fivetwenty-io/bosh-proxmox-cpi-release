"""Request context and repoint scenarios for scripts/lifecycle.

Two things live here so that they can be tested without a lab.

The first is the request builder and the context files. A BOSH Director merges
a cpi_config entry's properties into the context of every CPI call, so a call
for an entry that targets another cluster carries a director_uuid and a pve map.
scripts/lifecycle can now send the same thing from an optional context file.
With no file, build_request returns exactly the request the harness has always
sent.

The second is the foreign VM pass. It runs after the main sequence has a live
VM and proves what the CPI answers when a VM CID reaches the wrong cluster, as
it does after an operator repoints an availability zone:

  absent VMID    the stamped CID reaches a cluster where its VMID does not exist
  collision      the CID reaches a cluster where a hand-made VM holds its VMID
  token mismatch the CID has the right cluster and a wrong token

The expected answers come from the handlers (vm_ownership.go, has_vm.go,
delete_vm.go, get_disks.go, reboot_vm.go, set_vm_metadata.go) and are described
in docs/limitations.md. The pass covers has_vm, get_disks, delete_vm,
reboot_vm, and set_vm_metadata. It leaves attach_disk, detach_disk,
snapshot_disk, delete_snapshot, and create_disk to the spanning certification
and the Go handler tests, because each needs a disk on the second cluster.

The pass never acts on a VMID it did not create. The VM it hands to the CPI as
the "foreign" guest is one it made itself. No real delete_vm for a VMID the
pass did not make ever reaches the second cluster (step 14b's delete_vm, bare
or stamped, targets only the VM the pass made itself), because a CPI delete for a
bare VMID destroys any VM its Director owns, and a guest could take the VMID
between any check and the call. The stamped delete_vm calls are safe, since a
stamped CID for another cluster or token is refused, and the bare CID gets only
the read-only has_vm.
"""

from __future__ import annotations

import copy
import json
import secrets
import time
from pathlib import Path
from typing import Any, Callable

import _vmcid

CLOUD_ERROR = "Bosh::Clouds::CloudError"
VM_NOT_FOUND = "Bosh::Clouds::VMNotFound"

GUARD_ENFORCE = "enforce"
GUARD_WARN = "warn"


class ContextFileError(ValueError):
    """A context file the harness cannot send."""


class UnexpectedSuccess(RuntimeError):
    """A call that was expected to fail came back with a result."""


# ---------------------------------------------------------------------------
# Context files and the request shape
# ---------------------------------------------------------------------------


def load_context_file(path: "str | Path", label: str) -> dict[str, Any]:
    """Read a context file, a JSON object merged into each call's context.

    The object holds what a Director would merge in from a cpi_config entry,
    for example {"director_uuid": "...", "pve": {"host": "...", "node": "..."}}.
    request_id is refused because the harness owns it, so a file cannot make
    two calls share a request. director_uuid must be a non-empty string, and
    pve and agent must be objects, since the CPI reads them as maps.
    """
    p = Path(path)
    try:
        raw = json.loads(p.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise ContextFileError(f"{label} {p} is not readable JSON: {exc}") from exc
    if not isinstance(raw, dict):
        raise ContextFileError(f"{label} {p} must hold a JSON object, got {type(raw).__name__}")
    if "request_id" in raw:
        raise ContextFileError(f"{label} {p} must not set request_id; the harness assigns one per call")
    if "director_uuid" in raw and (not isinstance(raw["director_uuid"], str) or not raw["director_uuid"].strip()):
        raise ContextFileError(f"{label} {p}: director_uuid must be a non-empty string")
    for key in ("pve", "agent"):
        if key in raw and not isinstance(raw[key], dict):
            raise ContextFileError(f"{label} {p}: {key} must be an object, got {type(raw[key]).__name__}")
    return raw


_SECRET_KEY_FRAGMENTS = ("password", "token", "secret", "mbus", "ca_cert")
REDACTED = "***"


def redact_request(req: dict[str, Any]) -> dict[str, Any]:
    """Return a copy of req that is safe to print, with the context's secrets masked.

    A context file holds what a Director merges in from a cpi_config entry,
    which includes pve.password, pve.api_token, the CA certificate, and the
    agent's mbus URL, whose userinfo is a password. Any key in the context, at
    any depth, whose name contains password, token, secret, mbus, or ca_cert
    has its value replaced. Arguments and the rest of the request are left
    alone, as tracing has always printed them.
    """

    def mask(value: Any) -> Any:
        if isinstance(value, dict):
            return {
                key: REDACTED if any(f in str(key).lower() for f in _SECRET_KEY_FRAGMENTS) else mask(item)
                for key, item in value.items()
            }
        if isinstance(value, list):
            return [mask(item) for item in value]
        return value

    out = dict(req)
    context = req.get("context")
    if isinstance(context, dict):
        out["context"] = mask(context)
    return out


def build_request(
    method: str,
    args: list[Any],
    request_id: str,
    context_extra: "dict[str, Any] | None" = None,
) -> dict[str, Any]:
    """Build one JSON-RPC request.

    With no extra context the request is {"method", "arguments", "context":
    {"request_id"}, "api_version": 2}, the shape the harness has always sent. An
    extra context is merged after request_id, and it cannot replace request_id.
    """
    context: dict[str, Any] = {"request_id": request_id}
    for key, value in (context_extra or {}).items():
        if key == "request_id":
            raise ContextFileError("a context must not set request_id")
        context[key] = copy.deepcopy(value)
    return {
        "method": method,
        "arguments": args,
        "context": context,
        "api_version": 2,
    }


# ---------------------------------------------------------------------------
# What a context says about the cluster it targets
# ---------------------------------------------------------------------------

_PVE_PREFIX = "pve_"
_CREDENTIAL_KEYS = ("user", "password", "api_token")


def pve_settings(context: dict[str, Any]) -> dict[str, Any]:
    """Return the per-cluster settings a context carries, keyed without a pve_ prefix.

    The Director delivers an entry's properties nested, as context["pve"], and
    the CPI also reads flat pve_* keys. A flat key wins over the nested one, as
    it does in the CPI.
    """
    settings: dict[str, Any] = {}
    nested = context.get("pve")
    if isinstance(nested, dict):
        settings.update(nested)
    for key, value in context.items():
        if key.startswith(_PVE_PREFIX) and len(key) > len(_PVE_PREFIX):
            settings[key[len(_PVE_PREFIX):]] = value
    return settings


def _as_bool(value: Any, what: str) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str) and value.strip().lower() in ("true", "false"):
        return value.strip().lower() == "true"
    raise ContextFileError(f"{what} must be true or false, got {value!r}")


def endpoint(cfg: dict[str, Any]) -> tuple[str, int]:
    """Return a config's PVE API endpoint, normalised the way the CPI compares them."""
    host = str(cfg.get("host", "") or "").strip().lower().rstrip(".")
    port = int(cfg.get("port", 0) or 0) or 8006
    return host, port


def verifier_config(base_cfg: dict[str, Any], context: dict[str, Any]) -> dict[str, Any]:
    """Return the CPI config dict the PVE verifier needs for the cluster a context targets.

    It starts from the job-level CPI config and applies the context's pve
    settings, following the CPI's own rules: the credential group (user,
    password, api_token) travels together, so an entry that names any of them
    replaces all three; an entry that changes the endpoint drops the job's CA
    unless it names its own; and the job's node is dropped on a changed
    endpoint, because a node name means something only within one cluster.
    """
    cfg = copy.deepcopy(base_cfg)
    settings = pve_settings(context)
    if not settings:
        return cfg
    before = endpoint(cfg)
    for key in ("host", "port", "node", "realm"):
        if key in settings:
            cfg[key] = settings[key]
    if "verify_ssl" in settings:
        cfg["verify_ssl"] = _as_bool(settings["verify_ssl"], "pve.verify_ssl")
    if any(key in settings for key in _CREDENTIAL_KEYS):
        for key in _CREDENTIAL_KEYS:
            cfg.pop(key, None)
            if key in settings:
                cfg[key] = settings[key]
    if "ca_cert" in settings:
        cfg["pve_ca_cert"] = settings["ca_cert"]
    if endpoint(cfg) != before:
        if "ca_cert" not in settings:
            cfg.pop("pve_ca_cert", None)
        if "node" not in settings:
            cfg.pop("node", None)
    return cfg


def effective_guard(base_cfg: dict[str, Any], context: dict[str, Any]) -> str:
    """Return the vm_ownership_guard mode a request with this context runs under.

    The context's value wins over the job config's, and anything but "warn"
    means enforce, which is how the CPI reads the property.
    """
    settings = pve_settings(context)
    raw = settings["vm_ownership_guard"] if "vm_ownership_guard" in settings else base_cfg.get("vm_ownership_guard", "")
    return GUARD_WARN if str(raw or "").strip().lower() == GUARD_WARN else GUARD_ENFORCE


# ---------------------------------------------------------------------------
# Assertions on CPI answers
# ---------------------------------------------------------------------------


def _clip(text: str, limit: int = 600) -> str:
    return text if len(text) <= limit else text[:limit] + "..."


def check_error(
    op: str,
    err: dict[str, Any],
    *,
    type_: str,
    contains: "tuple[str, ...]" = (),
    any_of: "tuple[str, ...]" = (),
) -> None:
    """Assert err is a non-retriable error of type_ whose message names what it must.

    contains lists substrings that must all appear. any_of lists substrings of
    which at least one must appear, for answers that depend on whether two
    clusters share a stamp. The error text carries tag values, VMIDs, and
    Director UUIDs, never credentials, so a failure prints it.
    """
    message = str(err.get("message", ""))
    problems: list[str] = []
    if err.get("type") != type_:
        problems.append(f"type {err.get('type')!r}, want {type_!r}")
    if err.get("ok_to_retry") is not False:
        problems.append("ok_to_retry is set, so the Director would retry a refusal")
    problems.extend(f"message lacks {needle!r}" for needle in contains if needle not in message)
    if any_of and not any(needle in message for needle in any_of):
        problems.append(f"message names none of {list(any_of)!r}")
    if problems:
        raise RuntimeError(f"{op}: wrong answer ({'; '.join(problems)}): {_clip(message)}")


def config_differences(before: dict[str, Any], after: dict[str, Any]) -> list[str]:
    """Return the sorted config keys whose values differ, or that only one side has."""
    return sorted(key for key in set(before) | set(after) if before.get(key) != after.get(key))


def preflight(az2: Any, say: Callable[[str], None] = print) -> None:
    """Fail before anything runs if the pass cannot make and remove a VM on az2.

    It needs the urllib transport, because the pmx transport only inspects. It
    also needs credentials that hold VM.Allocate, but the permissions read can
    only warn. A read that fails, an answer of an unexpected shape, no entry
    for /vms, and privileges granted on a pool or a single VM all leave the
    question open, since PVE versions and least-privilege layouts differ, and
    the create in step 14b still fails loudly if the credentials cannot write.
    The run stops only when /vms is answered and positively lacks a privilege.
    """
    az2.require_writes()
    try:
        missing = az2.missing_vm_privileges()
    except Exception as exc:
        say(f"note: could not read the az2 credentials' privileges ({exc}); step 14b will find out when it writes")
        return
    if missing is None:
        say(
            "note: the az2 permissions answer does not show the credentials' privileges on /vms "
            "(they may be granted on a pool or a single VM); step 14b will find out when it writes"
        )
        return
    if missing:
        raise RuntimeError(
            f"the FOREIGN_CONTEXT_FILE credentials lack {', '.join(missing)} on /vms, "
            "so step 14b could not make and remove its hand-made VM on az2"
        )


# ---------------------------------------------------------------------------
# The foreign VM pass
# ---------------------------------------------------------------------------

CallFn = Callable[..., Any]


class ForeignVMPass:
    """The three repoint scenarios, run against the VM the main sequence created.

    az1 is the verifier for the cluster that holds the VM, and az2 is the one
    for the cluster the VM's CID is sent to as if its zone had been repointed.
    call(method, args, context) returns a result, and call_error(method, args,
    context) returns the error body of a call that must fail, or raises
    UnexpectedSuccess. A context of None means the harness's own context, and
    the pass passes the other cluster's context explicitly.
    """

    def __init__(
        self,
        *,
        vm_cid: str,
        az2_context: dict[str, Any],
        guard: str,
        az1: Any,
        az2: Any,
        call: CallFn,
        call_error: CallFn,
        step: Callable[[str], None],
        say: Callable[[str], None] = print,
        token_source: Callable[[], str] = lambda: secrets.token_hex(4),
        sleep: Callable[[float], None] = time.sleep,
        settle_attempts: int = 15,
        settle_poll: float = 2.0,
    ) -> None:
        if guard not in (GUARD_ENFORCE, GUARD_WARN):
            raise ValueError(f"guard must be enforce or warn, got {guard!r}")
        self.vm_cid = vm_cid
        self.az2_context = az2_context
        self.guard = guard
        self.az1 = az1
        self.az2 = az2
        self._call = call
        self._call_error = call_error
        self._step = step
        self._say = say
        self._token_source = token_source
        self._sleep = sleep
        self._settle_attempts = settle_attempts
        self._settle_poll = settle_poll
        self._handmade: "dict[str, Any] | None" = None

    # -- entry point ---------------------------------------------------------

    def run(self) -> None:
        """Run the three steps in order. A failure leaves nothing of ours on az2."""
        cid = _vmcid.parse(self.vm_cid)
        before = self.az1.qemu_config(cid.vmid)
        self.absent_vmid(cid, before)
        self.collision(cid, before)
        self.token_mismatch(cid, before)

    # -- the steps -----------------------------------------------------------

    def absent_vmid(self, cid: _vmcid.VMCID, before: dict[str, Any]) -> None:
        """A CID whose VMID does not exist on the cluster it reaches.

        A stamped CID naming another cluster is refused by delete_vm and
        answered false by has_vm, because the VM may still run on the cluster
        the stamp names. A bare CID and the all-zero stamp name no cluster, so
        has_vm still answers false for them. Their delete_vm answer, success,
        is not asserted here, because a delete that could reach a guest this
        run did not make is never sent.
        """
        self._step("14a. absent VMID after a repoint (the az1 CID reaches az2, where its VMID does not exist)")
        self._require_free_vmid(cid.vmid, "the absent-VMID step")

        if cid.known_cluster:
            has = self._call("has_vm", [str(cid)], self.az2_context)
            if has is not False:
                raise RuntimeError(f"has_vm answered {has!r} for stamped CID {cid} on az2, want false")
            err = self._expect_error("delete_vm", [str(cid)], self.az2_context)
            check_error(
                "delete_vm", err, type_=CLOUD_ERROR,
                contains=(
                    f"delete_vm: refusing to answer for CID {cid}",
                    f"VM {cid.vmid} does not exist on this cluster",
                    "(cluster-mismatch)",
                ),
            )
            self._say("    stamped CID refused by delete_vm and answered false by has_vm, as expected")
            self._assert_az1_untouched(cid, before, "after the stamped CID reached az2")
        else:
            self._say(
                f"    SKIPPED the stamped-CID assertions: CID {cid} carries "
                f"{'no cluster stamp' if not cid.stamped else 'the unknown cluster stamp'}, "
                "so it cannot name a cluster the CPI could tell apart"
            )

        # The bare twin sends only has_vm. A bare delete_vm would answer success
        # for an absent VMID and destroy a VM its Director owns if one took the
        # VMID after the check, so it never leaves this harness. The handler
        # tests pin that answer, where no live VM can race it.
        bare = cid.path_id
        self._require_free_vmid(cid.vmid, "the bare-CID twin")
        has = self._call("has_vm", [bare], self.az2_context)
        if has is not False:
            raise RuntimeError(f"has_vm answered {has!r} for bare CID {bare} on az2, want false")
        self._say("    bare CID answered as before by has_vm (false); no bare delete_vm is sent to a live cluster")
        self._assert_az1_untouched(cid, before, "after the bare CID reached az2")

    def collision(self, cid: _vmcid.VMCID, before: dict[str, Any]) -> None:
        """A CID whose VMID a hand-made VM holds on the cluster it reaches.

        The harness makes the VM itself, with no tags at all, so it is not
        BOSH's, and removes it when the step ends. Every method the answers
        table gives a refusal must refuse, and the VM must come out unchanged.
        """
        self._step("14b. collision after a repoint (a hand-made az2 VM holds the az1 VM's VMID)")
        if cid.known_cluster:
            reasons = ("cluster-mismatch", "token-missing")
        elif cid.stamped:
            reasons = ("cluster-mismatch",)
        else:
            reasons = ("not-bosh",)
        try:
            self._create_handmade(cid.vmid)
            self._assert_handmade_untouched("before any call reached it")
            self._collision_answers(cid, reasons)
        except BaseException:
            self._remove_handmade_after_failure()
            raise
        self._remove_handmade()
        self._assert_az1_untouched(cid, before, "after the collision step")

    def token_mismatch(self, cid: _vmcid.VMCID, before: dict[str, Any]) -> None:
        """A CID with the right cluster and a wrong token.

        The VM the CID named no longer exists and another VM holds its VMID, so
        delete_vm answers VMNotFound and has_vm answers false. Neither touches
        the VM, which here is the run's own.
        """
        self._step("14c. token mismatch (the az1 CID's cluster with a wrong token)")
        if not cid.stamped:
            self._say(f"    SKIPPED: CID {cid} is bare and carries no token to get wrong")
            return
        wrong = self._wrong_token_cid(cid)
        has = self._call("has_vm", [str(wrong)], None)
        if has is not False:
            raise RuntimeError(f"has_vm answered {has!r} for wrong-token CID {wrong}, want false")
        err = self._expect_error("delete_vm", [str(wrong)], None)
        check_error("delete_vm", err, type_=VM_NOT_FOUND, contains=(str(wrong),))
        self._say("    wrong-token CID answered VMNotFound by delete_vm and false by has_vm, as expected")
        self._assert_az1_untouched(cid, before, "after the wrong-token CID")

    # -- the collision answers ----------------------------------------------

    def _collision_answers(self, cid: _vmcid.VMCID, reasons: "tuple[str, ...]") -> None:
        text = str(cid)
        warn = self.guard == GUARD_WARN

        has = self._call("has_vm", [text], self.az2_context)
        if has is not warn:
            raise RuntimeError(
                f"has_vm answered {has!r} for CID {text} on the hand-made VM, want {warn!r} "
                f"under vm_ownership_guard {self.guard}"
            )
        self._say(f"    has_vm={str(warn).lower()} (vm_ownership_guard {self.guard})")
        self._assert_handmade_untouched("after has_vm")

        # get_disks is the other read warn relaxes. The hand-made VM has no
        # disks, so a warn listing is an empty list.
        if warn:
            disks = self._call("get_disks", [text], self.az2_context)
            if not isinstance(disks, list):
                raise RuntimeError(f"get_disks answered {disks!r} under warn, want a list")
        else:
            err = self._expect_error("get_disks", [text], self.az2_context)
            check_error("get_disks", err, type_=VM_NOT_FOUND, contains=(text,))
        self._say("    get_disks answered as the table says")
        self._assert_handmade_untouched("after get_disks")

        metadata = {"deployment": "lifecycle-test", "director": "local-harness"}
        calls: "list[tuple[str, list[Any]]]" = [
            ("delete_vm", [text]),
            ("reboot_vm", [text]),
            ("set_vm_metadata", [text, metadata]),
        ]
        for op, args in calls:
            err = self._expect_error(op, args, self.az2_context)
            check_error(
                op, err, type_=CLOUD_ERROR,
                contains=(f"{op}: refusing to act on VM {cid.vmid} for CID {text}",),
                any_of=tuple(f"does not own ({reason})" for reason in reasons),
            )
            self._say(f"    {op} refused, as the table says")
            self._assert_handmade_untouched(f"after {op}")

    # -- the hand-made VM ----------------------------------------------------

    def _az2_node(self) -> str:
        node = str(getattr(self.az2, "node", "") or "").strip()
        if node:
            return node
        online = self.az2.cluster_nodes()
        if not online:
            raise RuntimeError("az2 reports no online node to make the hand-made VM on")
        return online[0]

    def _create_handmade(self, vmid: int) -> None:
        """Make the hand-made VM, only after proving its VMID is free on az2."""
        self._require_free_vmid(vmid, "the collision step")
        node = self._az2_node()
        name = f"lifecycle-handmade-{vmid}"
        # Recorded before the create returns, so a create that dies midway is
        # still looked for. _remove_handmade checks the name before it destroys.
        # The create is unconfirmed until its task has finished, because a task
        # that outlives the wait can still land the VM afterwards.
        self._handmade = {"node": node, "vmid": vmid, "name": name, "snapshot": None, "unconfirmed": True}
        upid = self.az2.create_empty_vm(node, vmid, name)
        try:
            self.az2.await_task(node, upid)
        except Exception as exc:
            raise RuntimeError(
                f"the task that creates hand-made VM {name} on az2 node {node} did not finish cleanly ({exc}); "
                f"if a VM named {name} appears there, it is this run's and can be removed by hand"
            ) from exc
        self._handmade["unconfirmed"] = False
        config = self.az2.qemu_config(vmid, node=node)
        self._require_handmade_identity(config)
        if str(config.get("tags", "") or "").strip():
            raise RuntimeError(f"the hand-made VM {vmid} came up with tags {config.get('tags')!r}, want none")
        state = self._runtime_state()
        if state["status"] != "stopped" or state["lock"]:
            raise RuntimeError(
                f"the hand-made VM {vmid} came up {state['status']!r} with lock {state['lock']!r}, "
                "want stopped with no lock"
            )
        self._handmade["snapshot"] = config
        self._handmade["state"] = state
        self._say(f"    made hand-made VM {vmid} ({name}) on az2 node {node}")

    def _runtime_state(self) -> dict[str, str]:
        """The hand-made VM's power state and lock, which its config does not show."""
        assert self._handmade is not None
        status = self.az2.qemu_status(self._handmade["vmid"], node=self._handmade["node"])
        return {"status": str(status.get("status", "") or ""), "lock": str(status.get("lock", "") or "")}

    def _require_handmade_identity(self, config: dict[str, Any]) -> None:
        assert self._handmade is not None
        if config.get("name") != self._handmade["name"]:
            raise RuntimeError(
                f"VM {self._handmade['vmid']} on az2 is named {config.get('name')!r}, not the "
                f"{self._handmade['name']!r} this run made, so it is not ours to touch"
            )

    def _assert_handmade_untouched(self, when: str) -> None:
        assert self._handmade is not None and self._handmade["snapshot"] is not None
        now = self.az2.qemu_config(self._handmade["vmid"], node=self._handmade["node"])
        self._require_handmade_identity(now)
        differing = config_differences(self._handmade["snapshot"], now)
        if differing:
            raise RuntimeError(
                f"the hand-made VM {self._handmade['vmid']} changed {when} (config keys {differing})"
            )
        state = self._runtime_state()
        if state != self._handmade["state"]:
            raise RuntimeError(
                f"the hand-made VM {self._handmade['vmid']} changed {when} "
                f"(status and lock were {self._handmade['state']}, now {state})"
            )
        self._say(f"    verify: hand-made VM {self._handmade['vmid']} untouched {when} = ok")

    def _remove_handmade(self) -> None:
        """Destroy the hand-made VM once it is proven to be the one this run made, unchanged."""
        handmade = self._handmade
        if handmade is None:
            return
        vmid, node = handmade["vmid"], handmade["node"]
        if not self._holds_vmid(handmade):
            self._handmade = None
            if handmade["unconfirmed"]:
                self._say(
                    f"    WARNING: hand-made VM {handmade['name']} did not appear on az2 node {node}; "
                    "if its create task lands later, remove that VM by hand"
                )
            return
        config = self.az2.qemu_config(vmid, node=node)
        self._require_handmade_identity(config)
        if handmade["snapshot"] is not None:
            differing = config_differences(handmade["snapshot"], config)
            if differing:
                raise RuntimeError(
                    f"hand-made VM {vmid} on az2 changed (config keys {differing}); leaving it in place "
                    "for inspection instead of destroying it"
                )
            state = self._runtime_state()
            if state != handmade["state"]:
                raise RuntimeError(
                    f"hand-made VM {vmid} on az2 changed (status and lock were {handmade['state']}, now {state}); "
                    "leaving it in place for inspection instead of destroying it"
                )
        elif str(config.get("tags", "") or "").strip():
            raise RuntimeError(f"hand-made VM {vmid} on az2 carries tags; leaving it in place for inspection")
        # Never ask PVE to take volumes the VM does not reference: they are not ours.
        self.az2.await_task(node, self.az2.destroy_vm(node, vmid, destroy_unreferenced_disks=False))
        if self.az2.vmid_in_use(vmid):
            raise RuntimeError(f"hand-made VM {vmid} is still on az2 after its destroy task finished")
        self._handmade = None
        self._say(f"    verify: hand-made VM {vmid} removed from az2 = ok")

    def _holds_vmid(self, handmade: dict[str, Any]) -> bool:
        """True when a guest holds the VMID. After an unconfirmed create, wait a bounded time for the VM to land."""
        if self.az2.vmid_in_use(handmade["vmid"]):
            return True
        if not handmade["unconfirmed"]:
            return False
        for _ in range(self._settle_attempts):
            self._sleep(self._settle_poll)
            if self.az2.vmid_in_use(handmade["vmid"]):
                return True
        return False

    def _remove_handmade_after_failure(self) -> None:
        """Best-effort removal while an error is already in flight; say so if it fails."""
        try:
            self._remove_handmade()
        except Exception as exc:
            self._say(f"    WARNING: could not remove the hand-made VM: {exc}")

    # -- shared checks -------------------------------------------------------

    def _require_free_vmid(self, vmid: int, what: str) -> None:
        if self.az2.vmid_in_use(vmid):
            raise RuntimeError(
                f"VMID {vmid} is already in use on az2, so {what} cannot run: it would send the CPI a "
                "VMID that a guest this run did not create holds. Move the two entries' VMID bands apart."
            )

    def _assert_az1_untouched(self, cid: _vmcid.VMCID, before: dict[str, Any], when: str) -> None:
        if not self.az1.vm_exists(cid.vmid):
            raise RuntimeError(f"the az1 VM {cid.vmid} is gone {when}")
        differing = config_differences(before, self.az1.qemu_config(cid.vmid))
        if differing:
            raise RuntimeError(f"the az1 VM {cid.vmid} changed {when} (config keys {differing})")
        self._say(f"    verify: az1 VM {cid.vmid} untouched {when} = ok")

    def _wrong_token_cid(self, cid: _vmcid.VMCID) -> _vmcid.VMCID:
        for _ in range(8):
            token = self._token_source()
            if token != cid.token:
                return cid.with_token(token)
        raise RuntimeError("could not draw a token that differs from the VM's")

    def _expect_error(self, method: str, args: list[Any], context: "dict[str, Any] | None") -> dict[str, Any]:
        try:
            return self._call_error(method, args, context)
        except UnexpectedSuccess as exc:
            hint = ""
            if method == "delete_vm" and context is self.az2_context and "-" in str(args[0]):
                hint = (
                    " If both clusters share a root CA their stamps are equal and a stamped CID cannot tell them "
                    "apart; docs/limitations.md says so."
                )
            raise RuntimeError(f"{method} was expected to fail and did not: {exc}.{hint}") from exc
