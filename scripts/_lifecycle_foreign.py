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
delete_vm.go, get_disks.go) and from the answers table in docs/multi-cluster.md.
The pass never acts on a VMID it did not create: the VM it hands to the CPI as
the "foreign" guest is one it made itself, and it proves a VMID absent before
it sends anything that could destroy it.
"""

from __future__ import annotations

import copy
import json
import secrets
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
        they get today's answers: false from has_vm and success from
        delete_vm, which is why the bare twin runs only after the VMID is
        proven absent.
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

        bare = cid.path_id
        # The twin deletes by bare VMID, which the CPI answers with success when
        # the VMID is absent and with a destroy when it is not. Prove the
        # absence again right before it, so nothing created in the meantime is hit.
        self._require_free_vmid(cid.vmid, "the bare-CID twin")
        has = self._call("has_vm", [bare], self.az2_context)
        if has is not False:
            raise RuntimeError(f"has_vm answered {has!r} for bare CID {bare} on az2, want false")
        self._call("delete_vm", [bare], self.az2_context)
        self._say("    bare CID answered as before: has_vm false, delete_vm success")
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
        self._handmade = {"node": node, "vmid": vmid, "name": name, "snapshot": None}
        upid = self.az2.create_empty_vm(node, vmid, name)
        self.az2.await_task(node, upid)
        config = self.az2.qemu_config(vmid, node=node)
        self._require_handmade_identity(config)
        if str(config.get("tags", "") or "").strip():
            raise RuntimeError(f"the hand-made VM {vmid} came up with tags {config.get('tags')!r}, want none")
        self._handmade["snapshot"] = config
        self._say(f"    made hand-made VM {vmid} ({name}) on az2 node {node}")

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
        self._say(f"    verify: hand-made VM {self._handmade['vmid']} untouched {when} = ok")

    def _remove_handmade(self) -> None:
        """Destroy the hand-made VM once it is proven to be the one this run made, unchanged."""
        handmade = self._handmade
        if handmade is None:
            return
        vmid, node = handmade["vmid"], handmade["node"]
        if not self.az2.vmid_in_use(vmid):
            self._handmade = None
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
        elif str(config.get("tags", "") or "").strip():
            raise RuntimeError(f"hand-made VM {vmid} on az2 carries tags; leaving it in place for inspection")
        self.az2.await_task(node, self.az2.destroy_vm(node, vmid))
        if self.az2.vmid_in_use(vmid):
            raise RuntimeError(f"hand-made VM {vmid} is still on az2 after its destroy task finished")
        self._handmade = None
        self._say(f"    verify: hand-made VM {vmid} removed from az2 = ok")

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
