#!/usr/bin/env python3
"""Tests for the VM CID handling the lifecycle and certify harnesses share, and
for the repoint scenarios scripts/lifecycle runs behind FOREIGN_VM_TEST.

The harnesses hand the CPI the VM CID create_vm returned, which can carry a
cluster stamp and a token, and they use the bare VMID for every PVE API path.
scripts/_vmcid.py is the Python twin of the CPI's parser, and these tests run
it against the vector file the Go parser's tests read, so the two cannot
drift. The PVE reads are stubbed, so nothing here contacts a lab.

The request tests load scripts/lifecycle itself against a fake CPI binary that
records what it is sent, so they show the harness sends the requests it always
has when no context file is set. The scenario tests drive the foreign VM pass
against in-memory clusters and a model of the answers the CPI's handlers give,
so each step's assertions are shown to pass on the right answers and to fail on
the wrong ones.

Run:
    python3 scripts/_lifecycle_test.py
"""

from __future__ import annotations

import contextlib
import importlib.machinery
import importlib.util
import io
import json
import os
import stat
import sys
import tempfile
import types
import unittest
import unittest.mock
from pathlib import Path

SCRIPTS_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS_DIR))
import _lifecycle_foreign as foreign  # noqa: E402
import _pve_verify  # noqa: E402
import _vmcid  # noqa: E402

VECTORS = SCRIPTS_DIR.parent / "src" / "pve_cpi" / "internal" / "vmcid" / "testdata" / "cid_vectors.json"


def _load_vectors() -> dict:
    with VECTORS.open(encoding="utf-8") as fh:
        data = json.load(fh)
    if not data.get("valid") or not data.get("invalid"):
        raise AssertionError(f"{VECTORS} has no valid or no invalid entries")
    return data


class TestSharedVectors(unittest.TestCase):
    """The twin answers every shared vector the way the Go parser does."""

    def test_valid_vectors_parse(self) -> None:
        for case in _load_vectors()["valid"]:
            with self.subTest(raw=case["raw"]):
                got = _vmcid.parse(case["raw"])
                self.assertEqual(got, _vmcid.VMCID(case["vmid"], case["cluster"], case["token"]))
                self.assertEqual(str(got), case["raw"])
                self.assertEqual(got.stamped, bool(case["cluster"]))
                self.assertEqual(got.path_id, str(case["vmid"]))

    def test_invalid_vectors_raise(self) -> None:
        for raw in _load_vectors()["invalid"]:
            with self.subTest(raw=raw):
                with self.assertRaises(ValueError):
                    _vmcid.parse(raw)

    def test_non_string_raises(self) -> None:
        for raw in (101, None, b"101"):
            with self.subTest(raw=raw):
                with self.assertRaises(ValueError):
                    _vmcid.parse(raw)  # type: ignore[arg-type]


class TestAsPathID(unittest.TestCase):
    """as_path_id turns whatever the harness holds into PVE's VMID."""

    def test_forms(self) -> None:
        cases = [
            (101, "101"),
            ("101", "101"),
            (" 101 ", "101"),
            ("101-0a1b2c3d-4e5f6a7b", "101"),
            (" 101-0a1b2c3d-4e5f6a7b\n", "101"),
            ("not-a-cid", "not-a-cid"),
            ("", ""),
        ]
        for value, want in cases:
            with self.subTest(value=value):
                self.assertEqual(_vmcid.as_path_id(value), want)

    def test_bool_is_rejected(self) -> None:
        with self.assertRaises(ValueError):
            _vmcid.as_path_id(True)


class TestVerifierTakesStampedCIDs(unittest.TestCase):
    """The verifier reads PVE by VMID when the harness passes a stamped CID."""

    def _verifier(self) -> _pve_verify.PVEVerifier:
        return _pve_verify.PVEVerifier({
            "host": "pve.example.com",
            "port": 8006,
            "node": "pve1",
            "api_token": "root@pam!t=x",
            "verify_ssl": False,
        })

    def test_vm_exists_matches_the_vmid(self) -> None:
        v = self._verifier()
        with unittest.mock.patch.object(v, "cluster_vms", return_value=[(101, "pve2")]):
            self.assertTrue(v.vm_exists("101-0a1b2c3d-4e5f6a7b"))
            self.assertFalse(v.vm_exists("102-0a1b2c3d-4e5f6a7b"))

    def test_qemu_config_reads_the_bare_vmid_path(self) -> None:
        v = self._verifier()
        paths: list[str] = []

        def fake_get(path: str):
            paths.append(path)
            return {"name": "vm"}

        with unittest.mock.patch.object(v, "cluster_vms", return_value=[(101, "pve2")]), \
                unittest.mock.patch.object(v, "_get", side_effect=fake_get):
            self.assertEqual(v.qemu_config("101-0a1b2c3d-4e5f6a7b"), {"name": "vm"})
        self.assertEqual(paths, ["/nodes/pve2/qemu/101/config"])


class TestVMCIDWithToken(unittest.TestCase):
    """with_token swaps the token and nothing else."""

    def test_swaps_only_the_token(self) -> None:
        got = _vmcid.parse("101-0a1b2c3d-4e5f6a7b").with_token("deadbeef")
        self.assertEqual(str(got), "101-0a1b2c3d-deadbeef")

    def test_bare_cid_has_no_token(self) -> None:
        with self.assertRaises(ValueError):
            _vmcid.parse("101").with_token("deadbeef")

    def test_token_must_be_a_stamp(self) -> None:
        for token in ("", "DEADBEEF", "deadbee", "deadbeef0", "zzzzzzzz"):
            with self.subTest(token=token):
                with self.assertRaises(ValueError):
                    _vmcid.parse("101-0a1b2c3d-4e5f6a7b").with_token(token)

    def test_known_cluster(self) -> None:
        self.assertTrue(_vmcid.parse("101-0a1b2c3d-4e5f6a7b").known_cluster)
        self.assertFalse(_vmcid.parse("101-00000000-4e5f6a7b").known_cluster)
        self.assertFalse(_vmcid.parse("101").known_cluster)


# ---------------------------------------------------------------------------
# The request shape and the context files
# ---------------------------------------------------------------------------

LEGACY_KEYS = ["method", "arguments", "context", "api_version"]


class TestBuildRequest(unittest.TestCase):
    def test_no_context_is_the_request_the_harness_always_sent(self) -> None:
        got = foreign.build_request("has_vm", ["101"], "rid-1")
        self.assertEqual(
            got,
            {"method": "has_vm", "arguments": ["101"], "context": {"request_id": "rid-1"}, "api_version": 2},
        )
        self.assertEqual(list(got), LEGACY_KEYS)
        self.assertEqual(json.dumps(got), json.dumps(foreign.build_request("has_vm", ["101"], "rid-1", None)))
        self.assertEqual(json.dumps(got), json.dumps(foreign.build_request("has_vm", ["101"], "rid-1", {})))

    def test_context_keys_ride_after_request_id(self) -> None:
        extra = {"director_uuid": "d-1", "pve": {"host": "pve2.example", "node": "n2"}}
        got = foreign.build_request("info", [], "rid-2", extra)
        self.assertEqual(list(got), LEGACY_KEYS)
        self.assertEqual(list(got["context"]), ["request_id", "director_uuid", "pve"])
        self.assertEqual(got["context"]["pve"], {"host": "pve2.example", "node": "n2"})

    def test_the_request_does_not_alias_the_context(self) -> None:
        extra = {"pve": {"host": "h"}}
        got = foreign.build_request("info", [], "rid-3", extra)
        got["context"]["pve"]["host"] = "changed"
        self.assertEqual(extra, {"pve": {"host": "h"}})

    def test_a_context_cannot_replace_request_id(self) -> None:
        with self.assertRaises(foreign.ContextFileError):
            foreign.build_request("info", [], "rid-4", {"request_id": "other"})


class TestLoadContextFile(unittest.TestCase):
    def _write(self, text: str) -> Path:
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        path = Path(d.name) / "ctx.json"
        path.write_text(text, encoding="utf-8")
        return path

    def test_reads_a_director_style_object(self) -> None:
        path = self._write('{"director_uuid": "d-1", "pve": {"host": "h"}, "agent": {"mbus": "m"}}')
        self.assertEqual(
            foreign.load_context_file(path, "ctx"),
            {"director_uuid": "d-1", "pve": {"host": "h"}, "agent": {"mbus": "m"}},
        )

    def test_rejects_what_the_cpi_would_misread(self) -> None:
        cases = {
            "not json": "{",
            "array": "[]",
            "request_id": '{"request_id": "x"}',
            "empty uuid": '{"director_uuid": " "}',
            "numeric uuid": '{"director_uuid": 5}',
            "pve string": '{"pve": "host"}',
            "agent list": '{"agent": []}',
        }
        for name, text in cases.items():
            with self.subTest(name):
                with self.assertRaises(foreign.ContextFileError):
                    foreign.load_context_file(self._write(text), "ctx")

    def test_missing_file(self) -> None:
        with self.assertRaisesRegex(foreign.ContextFileError, "not readable"):
            foreign.load_context_file("/nonexistent/ctx.json", "ctx")


class TestClusterSettings(unittest.TestCase):
    BASE = {
        "host": "pve1.example", "port": 8006, "node": "pve1", "user": "root@pam", "realm": "pam",
        "password": "job-secret", "api_token": "job-token", "verify_ssl": True, "pve_ca_cert": "JOB-CA",
        "vm_ownership_guard": "enforce",
    }

    def test_no_pve_settings_leaves_the_config_alone(self) -> None:
        self.assertEqual(foreign.verifier_config(self.BASE, {}), self.BASE)
        self.assertEqual(foreign.verifier_config(self.BASE, {"director_uuid": "d"}), self.BASE)

    def test_the_input_is_not_mutated(self) -> None:
        before = json.loads(json.dumps(self.BASE))
        foreign.verifier_config(self.BASE, {"pve": {"host": "pve2.example", "api_token": "t"}})
        self.assertEqual(self.BASE, before)

    def test_a_new_endpoint_takes_its_own_credentials_and_drops_the_job_ca_and_node(self) -> None:
        got = foreign.verifier_config(
            self.BASE, {"pve": {"host": "pve2.example", "user": "svc@pve", "api_token": "svc!t=s"}}
        )
        self.assertEqual(got["host"], "pve2.example")
        self.assertEqual(got["api_token"], "svc!t=s")
        self.assertEqual(got["user"], "svc@pve")
        self.assertNotIn("password", got)
        self.assertNotIn("pve_ca_cert", got)
        self.assertNotIn("node", got)

    def test_a_new_endpoint_keeps_the_ca_and_node_it_names(self) -> None:
        got = foreign.verifier_config(
            self.BASE, {"pve": {"host": "pve2.example", "node": "pve9", "ca_cert": "AZ2-CA", "api_token": "t"}}
        )
        self.assertEqual((got["node"], got["pve_ca_cert"]), ("pve9", "AZ2-CA"))

    def test_an_entry_on_the_job_host_keeps_the_job_ca_and_node(self) -> None:
        got = foreign.verifier_config(self.BASE, {"pve": {"host": "PVE1.example.", "port": 8006, "vm_prefix": "x"}})
        self.assertEqual((got["node"], got["pve_ca_cert"]), ("pve1", "JOB-CA"))

    def test_credentials_travel_as_a_group(self) -> None:
        got = foreign.verifier_config(self.BASE, {"pve": {"password": "entry-secret"}})
        self.assertEqual(got["password"], "entry-secret")
        self.assertNotIn("api_token", got)
        self.assertNotIn("user", got)

    def test_a_flat_key_wins_over_the_nested_one(self) -> None:
        context = {"pve": {"host": "nested.example", "api_token": "t"}, "pve_host": "flat.example"}
        self.assertEqual(foreign.pve_settings(context)["host"], "flat.example")
        self.assertEqual(foreign.verifier_config(self.BASE, context)["host"], "flat.example")

    def test_verify_ssl_accepts_the_strings_the_cpi_accepts(self) -> None:
        self.assertIs(foreign.verifier_config(self.BASE, {"pve": {"verify_ssl": "false"}})["verify_ssl"], False)
        self.assertIs(foreign.verifier_config(self.BASE, {"pve": {"verify_ssl": " TRUE "}})["verify_ssl"], True)
        with self.assertRaises(foreign.ContextFileError):
            foreign.verifier_config(self.BASE, {"pve": {"verify_ssl": "maybe"}})

    def test_endpoint_comparison_ignores_case_dot_and_default_port(self) -> None:
        self.assertEqual(foreign.endpoint({"host": "PVE1.Example."}), ("pve1.example", 8006))
        self.assertEqual(foreign.endpoint({"host": "pve1.example", "port": 8006}), ("pve1.example", 8006))
        self.assertNotEqual(foreign.endpoint({"host": "a", "port": 8007}), foreign.endpoint({"host": "a"}))

    def test_effective_guard(self) -> None:
        self.assertEqual(foreign.effective_guard({}, {}), "enforce")
        self.assertEqual(foreign.effective_guard({"vm_ownership_guard": "warn"}, {}), "warn")
        self.assertEqual(foreign.effective_guard({"vm_ownership_guard": "warn"}, {"pve": {"vm_ownership_guard": "enforce"}}), "enforce")
        self.assertEqual(foreign.effective_guard({}, {"pve": {"vm_ownership_guard": " WARN "}}), "warn")
        self.assertEqual(foreign.effective_guard({"vm_ownership_guard": "warn"}, {"pve": {"vm_ownership_guard": ""}}), "enforce")


FAKE_CPI = """#!{python}
import json, os, sys
line = sys.stdin.readline()
with open(os.environ["FAKE_CPI_LOG"], "a", encoding="utf-8") as log:
    log.write(json.dumps({{"argv": sys.argv[1:], "stdin": line}}) + "\\n")
queue = os.environ["FAKE_CPI_RESPONSES"]
with open(queue, encoding="utf-8") as fh:
    pending = json.load(fh)
answer = pending.pop(0) if pending else {{"result": None, "error": None, "log": ""}}
with open(queue, "w", encoding="utf-8") as fh:
    json.dump(pending, fh)
print(json.dumps(answer))
"""

LIFECYCLE_ENV = (
    "CPI_CONFIG", "STEMCELL_PATH", "CPI_BIN", "CPI_CONTEXT_FILE", "FOREIGN_VM_TEST", "FOREIGN_CONTEXT_FILE",
    "TRACE", "NETWORK_TEST_MODE", "SNAPSHOT_DETACH_MODE", "PARKED_DISK_TEST", "DISK_MIGRATION_TEST",
    "STORAGE_SET_REPLICAS_TEST", "MULTI_STORAGE_PLACEMENT_TEST", "PARKER_RANGE_START", "PARKER_RANGE_END",
    "PARKER_PREFIX", "PARKER_POOL", "NETWORK_DNS", "VM_EPHEMERAL_MIB",
)


class LifecycleScriptCase(unittest.TestCase):
    """Loads scripts/lifecycle against a fake CPI binary that records each request."""

    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.tmp = Path(tmp.name)
        self.cfg = self.tmp / "cpi.json"
        self.cfg.write_text(json.dumps({"host": "pve1.example", "node": "pve1", "api_token": "u!t=s"}), encoding="utf-8")
        stemcell = self.tmp / "stemcell.tgz"
        stemcell.write_bytes(b"")
        self.log = self.tmp / "requests.jsonl"
        self.responses = self.tmp / "responses.json"
        self.responses.write_text("[]", encoding="utf-8")
        binary = self.tmp / "fakecpi"
        binary.write_text(FAKE_CPI.format(python=sys.executable), encoding="utf-8")
        binary.chmod(binary.stat().st_mode | stat.S_IXUSR)
        self.env = {
            "CPI_CONFIG": str(self.cfg), "STEMCELL_PATH": str(stemcell), "CPI_BIN": str(binary),
            "FAKE_CPI_LOG": str(self.log), "FAKE_CPI_RESPONSES": str(self.responses),
        }

    def write_json(self, name: str, value: object) -> str:
        path = self.tmp / name
        path.write_text(json.dumps(value), encoding="utf-8")
        return str(path)

    def queue(self, *responses: dict) -> None:
        self.responses.write_text(json.dumps(list(responses)), encoding="utf-8")

    def load(self, **extra_env: str) -> types.ModuleType:
        env = {k: v for k, v in os.environ.items() if k not in LIFECYCLE_ENV}
        env.update(self.env)
        env.update(extra_env)
        stack = contextlib.ExitStack()
        self.addCleanup(stack.close)
        stack.enter_context(unittest.mock.patch.dict(os.environ, env, clear=True))
        loader = importlib.machinery.SourceFileLoader("lifecycle_under_test", str(SCRIPTS_DIR / "lifecycle"))
        spec = importlib.util.spec_from_loader("lifecycle_under_test", loader)
        module = importlib.util.module_from_spec(spec)
        loader.exec_module(module)
        return module

    def requests(self) -> list[dict]:
        if not self.log.exists():
            return []
        return [json.loads(line) for line in self.log.read_text(encoding="utf-8").splitlines()]

    def sent(self) -> list[dict]:
        return [json.loads(r["stdin"]) for r in self.requests()]


class TestLifecycleRequestShape(LifecycleScriptCase):
    def test_default_requests_are_exactly_what_the_harness_always_sent(self) -> None:
        lifecycle = self.load()
        self.assertEqual(lifecycle.DEFAULT_CONTEXT, {})
        self.assertEqual(lifecycle.FOREIGN_VM_TEST, "off")
        self.queue({"result": True, "error": None, "log": ""})
        self.assertIs(lifecycle.call("has_vm", ["101-0a1b2c3d-4e5f6a7b"]), True)
        lifecycle.call("set_vm_metadata", ["101", {"deployment": "d"}])
        first, second = self.sent()
        self.assertEqual(list(first), LEGACY_KEYS)
        self.assertEqual(list(first["context"]), ["request_id"])
        self.assertIsInstance(first["context"]["request_id"], str)
        self.assertNotEqual(first["context"]["request_id"], second["context"]["request_id"])
        self.assertEqual(first["method"], "has_vm")
        self.assertEqual(first["arguments"], ["101-0a1b2c3d-4e5f6a7b"])
        self.assertEqual(first["api_version"], 2)
        self.assertEqual(second["arguments"], ["101", {"deployment": "d"}])
        self.assertEqual([r["argv"] for r in self.requests()], [["--config", str(self.cfg)]] * 2)

    def test_the_wire_line_is_the_legacy_json_encoding(self) -> None:
        lifecycle = self.load()
        lifecycle.call("info", [])
        (raw,) = self.requests()
        request = json.loads(raw["stdin"])
        rid = request["context"]["request_id"]
        legacy = json.dumps(
            {"method": "info", "arguments": [], "context": {"request_id": rid}, "api_version": 2}
        ) + "\n"
        self.assertEqual(raw["stdin"], legacy)

    def test_a_foreign_context_file_alone_changes_nothing_while_the_test_is_off(self) -> None:
        other = self.write_json("az2.json", {"director_uuid": "d-2", "pve": {"host": "pve2.example"}})
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            lifecycle = self.load(FOREIGN_CONTEXT_FILE=other)
        self.assertIn("ignored", stderr.getvalue())
        lifecycle.call("info", [])
        (request,) = self.sent()
        self.assertEqual(list(request["context"]), ["request_id"])

    def test_the_context_file_rides_on_every_call(self) -> None:
        path = self.write_json("az1.json", {"director_uuid": "d-1", "pve": {"vm_prefix": "az1"}})
        lifecycle = self.load(CPI_CONTEXT_FILE=path)
        lifecycle.call("info", [])
        lifecycle.call("has_vm", ["101"], config_path=str(self.cfg))
        for request in self.sent():
            self.assertEqual(list(request), LEGACY_KEYS)
            self.assertEqual(list(request["context"]), ["request_id", "director_uuid", "pve"])
            self.assertEqual(request["context"]["director_uuid"], "d-1")
            self.assertEqual(request["context"]["pve"], {"vm_prefix": "az1"})

    def test_an_explicit_context_replaces_the_default_for_that_call_only(self) -> None:
        path = self.write_json("az1.json", {"director_uuid": "d-1"})
        lifecycle = self.load(CPI_CONTEXT_FILE=path)
        other = {"director_uuid": "d-2", "pve": {"host": "pve2.example"}}
        lifecycle.call("info", [], context=other)
        lifecycle.call("info", [], context={})
        lifecycle.call("info", [])
        to_az2, bare, default = (r["context"] for r in self.sent())
        self.assertEqual({k: v for k, v in to_az2.items() if k != "request_id"}, other)
        self.assertEqual(list(bare), ["request_id"])
        self.assertEqual(default["director_uuid"], "d-1")

    def test_a_bad_context_file_stops_the_run_before_it_starts(self) -> None:
        for text in ("{", "[]", '{"request_id": "x"}'):
            with self.subTest(text=text):
                path = self.tmp / "bad.json"
                path.write_text(text, encoding="utf-8")
                with self.assertRaises(SystemExit) as raised:
                    self.load(CPI_CONTEXT_FILE=str(path))
                self.assertIn("CPI_CONTEXT_FILE", str(raised.exception))
        self.assertEqual(self.requests(), [])


class TestLifecycleForeignConfig(LifecycleScriptCase):
    def test_the_foreign_test_needs_its_context_file(self) -> None:
        with self.assertRaisesRegex(SystemExit, "FOREIGN_CONTEXT_FILE"):
            self.load(FOREIGN_VM_TEST="on")

    def test_the_foreign_context_must_name_another_endpoint(self) -> None:
        same = self.write_json("same.json", {"pve": {"host": "PVE1.example"}})
        with self.assertRaisesRegex(SystemExit, "different PVE endpoint"):
            self.load(FOREIGN_VM_TEST="on", FOREIGN_CONTEXT_FILE=same)
        nothing = self.write_json("nothing.json", {"director_uuid": "d"})
        with self.assertRaisesRegex(SystemExit, "different PVE endpoint"):
            self.load(FOREIGN_VM_TEST="on", FOREIGN_CONTEXT_FILE=nothing)

    def test_the_foreign_endpoint_is_compared_with_the_az1_context_applied(self) -> None:
        az1 = self.write_json("az1.json", {"pve": {"host": "pve2.example", "api_token": "t"}})
        az2 = self.write_json("az2.json", {"pve": {"host": "pve2.example", "api_token": "t"}})
        with self.assertRaisesRegex(SystemExit, "different PVE endpoint"):
            self.load(FOREIGN_VM_TEST="on", CPI_CONTEXT_FILE=az1, FOREIGN_CONTEXT_FILE=az2)

    def test_a_valid_pair_loads(self) -> None:
        az2 = self.write_json("az2.json", {"director_uuid": "d", "pve": {"host": "pve2.example", "api_token": "t"}})
        lifecycle = self.load(FOREIGN_VM_TEST=" ON ", FOREIGN_CONTEXT_FILE=az2)
        self.assertEqual(lifecycle.FOREIGN_VM_TEST, "on")
        self.assertEqual(lifecycle.FOREIGN_CONTEXT["pve"]["host"], "pve2.example")

    def test_the_mode_must_be_on_or_off(self) -> None:
        with self.assertRaisesRegex(SystemExit, "FOREIGN_VM_TEST must be on|off"):
            self.load(FOREIGN_VM_TEST="yes")


class TestLifecycleCallError(LifecycleScriptCase):
    def test_returns_the_error_body_in_one_attempt(self) -> None:
        lifecycle = self.load()
        body = {"type": "Bosh::Clouds::CloudError", "message": "refused", "ok_to_retry": True}
        self.queue({"result": None, "error": body, "log": ""})
        with unittest.mock.patch.object(lifecycle.time, "sleep") as sleep:
            got = lifecycle.call_error("delete_vm", ["101"])
        self.assertEqual(got, {"type": "Bosh::Clouds::CloudError", "message": "refused", "ok_to_retry": True})
        sleep.assert_not_called()
        self.assertEqual(len(self.requests()), 1)

    def test_a_result_is_an_unexpected_success(self) -> None:
        lifecycle = self.load()
        with self.assertRaises(foreign.UnexpectedSuccess):
            lifecycle.call_error("delete_vm", ["101"])

    def test_a_non_object_error_is_refused(self) -> None:
        lifecycle = self.load()
        self.queue({"result": None, "error": "boom", "log": ""})
        with self.assertRaisesRegex(RuntimeError, "not an object"):
            lifecycle.call_error("delete_vm", ["101"])

    def test_call_still_raises_on_an_error_and_keeps_its_message(self) -> None:
        lifecycle = self.load()
        self.queue({"result": None, "error": {"type": "Bosh::Clouds::CloudError", "message": "nope", "ok_to_retry": False}, "log": ""})
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaisesRegex(RuntimeError, "delete_vm failed: nope"):
                lifecycle.call("delete_vm", ["101"])


# ---------------------------------------------------------------------------
# The foreign VM pass
# ---------------------------------------------------------------------------

AZ1_STAMP = "aaaaaaaa"
AZ2_STAMP = "bbbbbbbb"
TOKEN = "11111111"
VMID = 4321
CID = f"{VMID}-{AZ1_STAMP}-{TOKEN}"


class FakeCluster:
    """An in-memory cluster with the verifier surface the pass reads and writes."""

    def __init__(self, label: str, stamp: str, node: str = "n1") -> None:
        self.label = label
        self.stamp = stamp
        self.node = node
        self.guests: dict[int, dict] = {}
        self.created: list[int] = []
        self.created_on: list[str] = []
        self.destroyed: list[int] = []
        self.destroyed_by_cpi: list[int] = []
        self.destroy_calls: list[tuple[str, int, dict]] = []
        self.runtime: dict[int, dict] = {}
        self.privileges: "list[str] | None" = []
        self.writes_allowed = True
        self.in_use_probes = 0
        self.appear_after_probes: "tuple[int, int] | None" = None
        self.created_extra: dict = {}

    def add_bosh_vm(self, vmid: int, cluster: str, token: str) -> None:
        self.guests[vmid] = {
            "name": f"vm-{vmid}", "digest": "d0", "memory": 1024,
            "tags": f"bosh-cpi;bosh-cluster--{cluster};bosh-cid--{token}",
        }

    # verifier surface
    def vm_exists(self, vmid) -> bool:
        return int(vmid) in self.guests

    def vmid_in_use(self, vmid) -> bool:
        self.in_use_probes += 1
        if self.appear_after_probes and self.in_use_probes > self.appear_after_probes[0]:
            self.guests[self.appear_after_probes[1]] = {"name": "someone-elses", "digest": "z"}
        return int(vmid) in self.guests

    def qemu_config(self, vmid, node=None) -> dict:
        if int(vmid) not in self.guests:
            raise _pve_verify.PVEVerifyError(f"no VM {vmid}")
        return dict(self.guests[int(vmid)])

    def cluster_nodes(self) -> list[str]:
        return ["n1", "n2"]

    def qemu_status(self, vmid, node=None) -> dict:
        if int(vmid) not in self.guests:
            raise _pve_verify.PVEVerifyError(f"no VM {vmid}")
        return dict(self.runtime.get(int(vmid), {"status": "stopped"}))

    def require_writes(self) -> None:
        if not self.writes_allowed:
            raise _pve_verify.PVEVerifyError("PVE_VERIFY_TRANSPORT=pmx inspects only")

    def missing_vm_privileges(self) -> "list[str] | None":
        return None if self.privileges is None else list(self.privileges)

    def create_empty_vm(self, node: str, vmid, name: str) -> str:
        self.created.append(int(vmid))
        self.created_on.append(node)
        self.guests[int(vmid)] = {"name": name, "digest": "h0", "memory": 16, **self.created_extra}
        return "UPID:create"

    def destroy_vm(self, node: str, vmid, **kwargs) -> str:
        self.destroy_calls.append((node, int(vmid), kwargs))
        self.destroyed.append(int(vmid))
        self.guests.pop(int(vmid), None)
        return "UPID:destroy"

    def await_task(self, node: str, upid: str, timeout: float = 120.0, poll: float = 1.0) -> None:
        return None


class FakeCPI:
    """Answers calls the way the handlers do, for one VM on az1 and whatever az2 holds.

    The model follows vm_ownership.go: the cluster check comes first, then the
    token check, then the BOSH marker. A VMID absent from the cluster is
    refused when the CID names another cluster. Each answer is the text the Go
    handlers format.
    """

    def __init__(self, az1: FakeCluster, az2: FakeCluster, az2_context: dict, guard: str = "enforce") -> None:
        self.az1, self.az2, self.az2_context, self.guard = az1, az2, az2_context, guard
        self.calls: list[tuple[str, list, str]] = []
        self.overrides: dict[tuple[str, str], object] = {}

    def _cluster(self, context) -> FakeCluster:
        return self.az2 if context is self.az2_context else self.az1

    def call(self, method: str, args: list, context=None):
        outcome = self._answer(method, args, context)
        if outcome[0] == "err":
            raise RuntimeError(f"{method} failed: {outcome[1]['message']}")
        return outcome[1]

    def call_error(self, method: str, args: list, context=None) -> dict:
        outcome = self._answer(method, args, context)
        if outcome[0] == "ok":
            raise foreign.UnexpectedSuccess(f"{method} returned a result ({outcome[1]!r})")
        return outcome[1]

    def _answer(self, method: str, args: list, context):
        cluster = self._cluster(context)
        self.calls.append((method, list(args), cluster.label))
        forced = self.overrides.get((method, cluster.label))
        if forced is not None:
            return forced
        cid = _vmcid.parse(args[0])
        verdict = self._verdict(cluster, cid)
        if verdict == "absent":
            if cid.known_cluster and cid.cluster != cluster.stamp:
                if method == "has_vm":
                    return "ok", False
                return "err", _err(
                    "Bosh::Clouds::CloudError",
                    f"{method}: refusing to answer for CID {cid}, because VM {cid.vmid} does not exist on this "
                    "cluster and the CID names another cluster (cluster-mismatch): the CID names cluster "
                    f"{cid.cluster}, this cluster's stamp is {cluster.stamp}",
                )
            if method == "has_vm":
                return "ok", False
            if method == "delete_vm":
                return "ok", None
            return "err", _err("Bosh::Clouds::VMNotFound", f"VM not found: {cid}")
        if verdict == "owned":
            if method == "delete_vm":
                cluster.guests.pop(cid.vmid, None)
                cluster.destroyed_by_cpi.append(cid.vmid)
            return "ok", (True if method == "has_vm" else None)
        if verdict == "gone":
            if method == "has_vm":
                return "ok", False
            return "err", _err("Bosh::Clouds::VMNotFound", f"VM not found: {cid}")
        reason = verdict
        warn = self.guard == "warn"
        if method == "has_vm":
            return "ok", warn
        if method == "get_disks":
            return ("ok", []) if warn else ("err", _err("Bosh::Clouds::VMNotFound", f"VM not found: {cid}"))
        return "err", _err(
            "Bosh::Clouds::CloudError",
            f"{method}: refusing to act on VM {cid.vmid} for CID {cid}, which this Director does not own "
            f"({reason}): detail",
        )

    @staticmethod
    def _verdict(cluster: FakeCluster, cid: _vmcid.VMCID) -> str:
        if cid.vmid not in cluster.guests:
            return "absent"
        tags = [t for t in str(cluster.guests[cid.vmid].get("tags", "")).split(";") if t]
        values = lambda prefix: [t[len(prefix):] for t in tags if t.startswith(prefix)]  # noqa: E731
        if cid.stamped:
            clusters = values("bosh-cluster--")
            if clusters:
                if clusters != [cid.cluster]:
                    return "cluster-mismatch"
            elif not cid.known_cluster or cid.cluster != cluster.stamp:
                return "cluster-mismatch"
            tokens = values("bosh-cid--")
            if not tokens:
                return "token-missing"
            if tokens != [cid.token]:
                return "gone"
        if "bosh-cpi" not in tags:
            return "not-bosh"
        return "owned"


def _err(type_: str, message: str, retry: bool = False) -> dict:
    return {"type": type_, "message": message, "ok_to_retry": retry}


class ForeignPassCase(unittest.TestCase):
    cid = CID
    guard = "enforce"
    az2_stamp = AZ2_STAMP

    def setUp(self) -> None:
        self.az1 = FakeCluster("az1", AZ1_STAMP)
        self.az2 = FakeCluster("az2", self.az2_stamp)
        parsed = _vmcid.parse(self.cid)
        self.az1.add_bosh_vm(parsed.vmid, parsed.cluster or AZ1_STAMP, parsed.token or TOKEN)
        if not parsed.stamped:
            self.az1.guests[parsed.vmid]["tags"] = "bosh-cpi"
        self.az2_context = {"director_uuid": "d-2", "pve": {"host": "pve2.example"}}
        self.cpi = FakeCPI(self.az1, self.az2, self.az2_context, self.guard)
        self.steps: list[str] = []
        self.said: list[str] = []
        self.tokens = iter(["22222222", "33333333", "44444444"])

    def make(self, cid: "str | None" = None, **overrides) -> foreign.ForeignVMPass:
        kwargs = dict(
            vm_cid=cid or self.cid, az2_context=self.az2_context, guard=self.guard, az1=self.az1, az2=self.az2,
            call=self.cpi.call, call_error=self.cpi.call_error, step=self.steps.append, say=self.said.append,
            token_source=lambda: next(self.tokens), sleep=lambda _s: None,
        )
        kwargs.update(overrides)
        return foreign.ForeignVMPass(**kwargs)

    def methods(self, cluster: str) -> list[str]:
        return [m for m, _args, where in self.cpi.calls if where == cluster]


class TestForeignPassStamped(ForeignPassCase):
    def test_the_whole_pass_passes_on_the_right_answers_and_leaves_both_clusters_clean(self) -> None:
        before = dict(self.az1.guests[VMID])
        self.make().run()
        self.assertEqual(self.steps[0][:3], "14a")
        self.assertEqual([s[:3] for s in self.steps], ["14a", "14b", "14c"])
        self.assertEqual(self.az1.guests[VMID], before)
        self.assertEqual(self.az2.guests, {})
        self.assertEqual(self.az2.created, [VMID])
        self.assertEqual(self.az2.destroyed, [VMID])
        self.assertEqual(self.az1.destroyed, [])

    def test_the_az1_cluster_only_sees_the_token_mismatch_calls(self) -> None:
        self.make().run()
        self.assertEqual(self.methods("az1"), ["has_vm", "delete_vm"])
        sent = [(m, a[0]) for m, a, where in self.cpi.calls if where == "az1"]
        wrong = sent[0][1]
        self.assertNotEqual(wrong, CID)
        self.assertTrue(wrong.startswith(f"{VMID}-{AZ1_STAMP}-"))
        self.assertEqual(sent[1][1], wrong)

    def test_the_absent_step_sends_the_stamped_cid_then_only_has_vm_for_the_bare_twin(self) -> None:
        self.make().absent_vmid(_vmcid.parse(CID), dict(self.az1.guests[VMID]))
        self.assertEqual(
            [(m, a) for m, a, where in self.cpi.calls if where == "az2"],
            [("has_vm", [CID]), ("delete_vm", [CID]), ("has_vm", [str(VMID)])],
        )
        self.assertEqual(self.az2.created, [])

    def test_no_bare_delete_vm_ever_reaches_az2_in_a_whole_run(self) -> None:
        self.make().run()
        for method, args, where in self.cpi.calls:
            if method == "delete_vm" and where == "az2":
                self.assertTrue(_vmcid.parse(args[0]).stamped, f"a bare delete_vm reached az2: {args}")

    def test_a_vm_that_takes_the_vmid_right_after_the_proof_is_never_sent_a_delete(self) -> None:
        # The az2 Director creates a VM in the window after the last absence
        # proof and before the CPI reads the cluster. The twin must not hand it
        # a delete_vm it could resolve to that VM.
        original = self.cpi._answer

        def racing(method, args, context):
            outcome = original(method, args, context)
            if method == "has_vm" and args[0] == str(VMID):
                # has_vm has just answered false, and the az2 Director's VM lands now.
                self.az2.guests[VMID] = {"name": "the-az2-directors-vm", "digest": "z", "tags": "bosh-cpi"}
            return outcome

        self.cpi._answer = racing  # type: ignore[method-assign]
        self.make().absent_vmid(_vmcid.parse(CID), dict(self.az1.guests[VMID]))
        self.assertEqual(self.az2.destroyed_by_cpi, [])
        self.assertEqual(self.az2.destroyed, [])
        self.assertIn(VMID, self.az2.guests)
        self.assertNotIn(("delete_vm", [str(VMID)], "az2"), self.cpi.calls)

    def test_the_hand_made_vm_is_destroyed_without_its_unreferenced_disks(self) -> None:
        self.make().run()
        self.assertEqual(self.az2.destroy_calls, [("n1", VMID, {"destroy_unreferenced_disks": False})])

    def test_the_collision_step_asserts_every_refusal(self) -> None:
        self.make().collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))
        self.assertEqual(self.methods("az2"), ["has_vm", "get_disks", "delete_vm", "reboot_vm", "set_vm_metadata"])
        self.assertEqual(self.az2.guests, {})

    def test_a_wrong_token_is_never_the_real_token(self) -> None:
        self.tokens = iter([TOKEN, TOKEN, "22222222"])
        wrong = self.make()._wrong_token_cid(_vmcid.parse(CID))
        self.assertEqual(str(wrong), f"{VMID}-{AZ1_STAMP}-22222222")
        self.tokens = iter([TOKEN] * 8)
        with self.assertRaisesRegex(RuntimeError, "differs"):
            self.make()._wrong_token_cid(_vmcid.parse(CID))


class TestForeignPassWarn(ForeignPassCase):
    guard = "warn"

    def test_warn_relaxes_only_the_two_reads(self) -> None:
        self.make().run()
        self.assertEqual(self.az2.guests, {})

    def test_warn_answers_the_reads_and_still_refuses_the_mutations(self) -> None:
        self.cpi.overrides[("has_vm", "az2")] = ("ok", False)
        with self.assertRaisesRegex(RuntimeError, "has_vm answered False.*want True.*warn"):
            self.make().collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))
        self.assertEqual(self.az2.destroyed, [VMID])


class TestForeignPassGuardArgument(ForeignPassCase):
    def test_an_unknown_guard_is_refused(self) -> None:
        with self.assertRaises(ValueError):
            self.make(guard="off")


class TestAbsentStepFailures(ForeignPassCase):
    def _absent(self) -> None:
        self.make().absent_vmid(_vmcid.parse(CID), dict(self.az1.guests[VMID]))

    def test_delete_vm_success_means_the_stamp_did_not_catch_the_repoint(self) -> None:
        self.cpi.overrides[("delete_vm", "az2")] = ("ok", None)
        with self.assertRaisesRegex(RuntimeError, "delete_vm was expected to fail.*share a root CA"):
            self._absent()

    def test_has_vm_true_is_wrong(self) -> None:
        self.cpi.overrides[("has_vm", "az2")] = ("ok", True)
        with self.assertRaisesRegex(RuntimeError, "has_vm answered True"):
            self._absent()

    def test_the_refusal_must_not_be_retriable(self) -> None:
        self.cpi.overrides[("delete_vm", "az2")] = ("err", _err("Bosh::Clouds::CloudError", "x", retry=True))
        with self.assertRaisesRegex(RuntimeError, "ok_to_retry"):
            self._absent()

    def test_vm_not_found_is_wrong_because_the_director_would_drop_its_record(self) -> None:
        self.cpi.overrides[("delete_vm", "az2")] = ("err", _err("Bosh::Clouds::VMNotFound", f"VM not found: {CID}"))
        with self.assertRaisesRegex(RuntimeError, "type 'Bosh::Clouds::VMNotFound'"):
            self._absent()

    def test_the_refusal_must_name_the_cluster_mismatch(self) -> None:
        self.cpi.overrides[("delete_vm", "az2")] = ("err", _err("Bosh::Clouds::CloudError", "delete_vm: something else"))
        with self.assertRaisesRegex(RuntimeError, "message lacks"):
            self._absent()

    def test_an_az1_vm_the_calls_changed_fails_the_step(self) -> None:
        before = dict(self.az1.guests[VMID])
        original = self.cpi._answer

        def touching(method, args, context):
            if method == "delete_vm" and args[0] == CID:
                self.az1.guests[VMID]["tags"] += ";bosh-deleting"
            return original(method, args, context)

        self.cpi._answer = touching  # type: ignore[method-assign]
        with self.assertRaisesRegex(RuntimeError, r"changed after the stamped CID reached az2 \(config keys \['tags'\]\)"):
            self.make().absent_vmid(_vmcid.parse(CID), before)

    def test_an_az1_vm_that_vanished_fails_the_step(self) -> None:
        before = dict(self.az1.guests[VMID])
        original = self.cpi._answer

        def destroying(method, args, context):
            self.az1.guests.pop(VMID, None)
            return original(method, args, context)

        self.cpi._answer = destroying  # type: ignore[method-assign]
        with self.assertRaisesRegex(RuntimeError, "is gone after the stamped CID"):
            self.make().absent_vmid(_vmcid.parse(CID), before)

    def test_a_vmid_in_use_on_az2_stops_everything_before_a_call_is_sent(self) -> None:
        self.az2.guests[VMID] = {"name": "not-ours", "digest": "z"}
        with self.assertRaisesRegex(RuntimeError, "already in use on az2"):
            self._absent()
        self.assertEqual(self.cpi.calls, [])
        self.assertEqual(self.az2.destroyed, [])

    def test_a_guest_that_takes_the_vmid_before_the_bare_twin_is_never_sent_a_delete(self) -> None:
        self.az2.appear_after_probes = (1, VMID)
        with self.assertRaisesRegex(RuntimeError, "bare-CID twin cannot run"):
            self._absent()
        self.assertNotIn(("delete_vm", [str(VMID)], "az2"), self.cpi.calls)
        self.assertIn(VMID, self.az2.guests)
        self.assertEqual(self.az2.destroyed, [])

    def test_an_unreadable_az2_stops_the_step(self) -> None:
        def broken(vmid):
            raise _pve_verify.PVEVerifyError("node pve2 is offline, so the absence of VMID 4321 cannot be proven")

        self.az2.vmid_in_use = broken  # type: ignore[method-assign]
        with self.assertRaisesRegex(_pve_verify.PVEVerifyError, "offline"):
            self._absent()
        self.assertEqual(self.cpi.calls, [])


class TestCollisionStepFailures(ForeignPassCase):
    def _collide(self) -> None:
        self.make().collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))

    def test_each_refusal_is_checked_and_a_wrong_one_fails_but_still_removes_the_vm(self) -> None:
        for method in ("delete_vm", "reboot_vm", "set_vm_metadata"):
            with self.subTest(method=method):
                self.setUp()
                self.cpi.overrides[(method, "az2")] = ("ok", None)
                with self.assertRaisesRegex(RuntimeError, f"{method} was expected to fail"):
                    self._collide()
                self.assertEqual(self.az2.guests, {})
                self.assertEqual(self.az2.destroyed, [VMID])

    def test_a_vm_not_found_for_a_mutation_is_wrong(self) -> None:
        self.cpi.overrides[("reboot_vm", "az2")] = ("err", _err("Bosh::Clouds::VMNotFound", "VM not found"))
        with self.assertRaisesRegex(RuntimeError, "reboot_vm: wrong answer"):
            self._collide()

    def test_the_reason_must_be_one_the_stamp_allows(self) -> None:
        self.cpi.overrides[("delete_vm", "az2")] = (
            "err",
            _err("Bosh::Clouds::CloudError",
                 f"delete_vm: refusing to act on VM {VMID} for CID {CID}, which this Director does not own (other-director): d"),
        )
        with self.assertRaisesRegex(RuntimeError, "names none of"):
            self._collide()

    def test_equal_cluster_stamps_make_the_reason_token_missing_and_that_passes(self) -> None:
        self.az2.stamp = AZ1_STAMP
        self.make().collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))
        self.assertEqual(self.az2.guests, {})

    def test_has_vm_true_under_enforce_fails(self) -> None:
        self.cpi.overrides[("has_vm", "az2")] = ("ok", True)
        with self.assertRaisesRegex(RuntimeError, "has_vm answered True.*want False"):
            self._collide()

    def test_get_disks_must_answer_vm_not_found_under_enforce(self) -> None:
        self.cpi.overrides[("get_disks", "az2")] = ("ok", [])
        with self.assertRaisesRegex(RuntimeError, "get_disks was expected to fail"):
            self._collide()

    def test_a_hand_made_vm_the_calls_changed_is_left_in_place_and_the_run_fails(self) -> None:
        original = self.cpi._answer

        def touching(method, args, context):
            if method == "reboot_vm":
                self.az2.guests[VMID]["digest"] = "changed"
            return original(method, args, context)

        self.cpi._answer = touching  # type: ignore[method-assign]
        with self.assertRaisesRegex(RuntimeError, r"hand-made VM 4321 changed after reboot_vm \(config keys \['digest'\]\)"):
            self._collide()
        self.assertEqual(self.az2.destroyed, [])
        self.assertIn(VMID, self.az2.guests)
        self.assertTrue(any("WARNING: could not remove" in line and "leaving it in place" in line for line in self.said))

    def test_a_vm_that_is_not_the_one_this_run_made_is_never_destroyed(self) -> None:
        original = self.cpi._answer

        def swapping(method, args, context):
            if method == "get_disks":
                self.az2.guests[VMID] = {"name": "an-operators-vm", "digest": "other"}
            return original(method, args, context)

        self.cpi._answer = swapping  # type: ignore[method-assign]
        with self.assertRaisesRegex(RuntimeError, "not the .*lifecycle-handmade-4321.* this run made"):
            self._collide()
        self.assertEqual(self.az2.destroyed, [])
        self.assertEqual(self.az2.guests[VMID]["name"], "an-operators-vm")

    def test_a_hand_made_vm_that_comes_up_tagged_is_refused(self) -> None:
        self.az2.created_extra = {"tags": "bosh-cpi"}
        with self.assertRaisesRegex(RuntimeError, "came up with tags"):
            self._collide()
        self.assertEqual(self.az2.destroyed, [])
        self.assertIn(VMID, self.az2.guests)

    def test_a_vmid_in_use_on_az2_is_never_created_over(self) -> None:
        self.az2.guests[VMID] = {"name": "not-ours", "digest": "z"}
        with self.assertRaisesRegex(RuntimeError, "already in use on az2"):
            self._collide()
        self.assertEqual(self.az2.created, [])
        self.assertEqual(self.az2.destroyed, [])
        self.assertEqual(self.cpi.calls, [])

    def test_a_removal_that_does_not_take_is_reported(self) -> None:
        self.az2.destroy_vm = lambda node, vmid, **kw: "UPID:noop"  # type: ignore[method-assign]
        with self.assertRaisesRegex(RuntimeError, "still on az2"):
            self._collide()

    def test_a_create_that_dies_midway_is_cleaned_up_only_if_it_is_ours(self) -> None:
        def dying(node, vmid, name):
            self.az2.guests[int(vmid)] = {"name": name, "digest": "h0"}
            raise _pve_verify.PVEVerifyError("POST failed")

        self.az2.create_empty_vm = dying  # type: ignore[method-assign]
        with self.assertRaisesRegex(_pve_verify.PVEVerifyError, "POST failed"):
            self._collide()
        self.assertEqual(self.az2.destroyed, [VMID])

    def test_the_node_comes_from_the_az2_verifier_or_the_first_online_node(self) -> None:
        self.az2.node = ""
        self.make().collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))
        self.assertEqual(self.az2.destroyed, [VMID])
        self.assertEqual(self.az2.created_on, ["n1"])
        self.assertEqual(self.az2.destroy_calls[0][0], "n1")

    def test_a_set_node_is_the_node_the_vm_is_made_and_destroyed_on(self) -> None:
        self.az2.node = "pve9"
        self.make().collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))
        self.assertEqual(self.az2.created_on, ["pve9"])
        self.assertEqual(self.az2.destroy_calls[0][0], "pve9")


class TestTokenMismatchStep(ForeignPassCase):
    def _mismatch(self) -> None:
        self.make().token_mismatch(_vmcid.parse(CID), dict(self.az1.guests[VMID]))

    def test_passes_on_vm_not_found_and_false(self) -> None:
        self._mismatch()
        self.assertEqual(self.az1.destroyed, [])

    def test_an_owned_answer_is_wrong(self) -> None:
        self.cpi.overrides[("has_vm", "az1")] = ("ok", True)
        with self.assertRaisesRegex(RuntimeError, "has_vm answered True"):
            self._mismatch()

    def test_a_refusal_in_place_of_vm_not_found_is_wrong(self) -> None:
        self.cpi.overrides[("delete_vm", "az1")] = ("err", _err("Bosh::Clouds::CloudError", "refusing"))
        with self.assertRaisesRegex(RuntimeError, "type 'Bosh::Clouds::CloudError', want 'Bosh::Clouds::VMNotFound'"):
            self._mismatch()

    def test_a_delete_that_succeeds_fails_the_step(self) -> None:
        self.cpi.overrides[("delete_vm", "az1")] = ("ok", None)
        with self.assertRaisesRegex(RuntimeError, "delete_vm was expected to fail"):
            self._mismatch()

    def test_a_delete_that_touched_the_vm_fails_the_step(self) -> None:
        original = self.cpi._answer

        def touching(method, args, context):
            if method == "delete_vm":
                self.az1.guests[VMID]["tags"] += ";bosh-deleting"
            return original(method, args, context)

        self.cpi._answer = touching  # type: ignore[method-assign]
        with self.assertRaisesRegex(RuntimeError, "changed after the wrong-token CID"):
            self._mismatch()

    def test_calls_go_to_the_default_context(self) -> None:
        seen: list[object] = []
        original = self.cpi._answer

        def spying(method, args, context):
            seen.append(context)
            return original(method, args, context)

        self.cpi._answer = spying  # type: ignore[method-assign]
        self._mismatch()
        self.assertEqual(seen, [None, None])


class TestForeignPassBareCID(ForeignPassCase):
    cid = "4321"

    def test_stamped_assertions_are_skipped_and_the_rest_runs(self) -> None:
        self.make().run()
        self.assertTrue(any("SKIPPED the stamped-CID assertions" in line for line in self.said))
        self.assertTrue(any("SKIPPED: CID 4321 is bare" in line for line in self.said))
        self.assertEqual(self.methods("az1"), [])
        self.assertEqual(self.az2.guests, {})

    def test_the_collision_is_a_not_bosh_refusal(self) -> None:
        self.make().run()
        refused = [(m, a) for m, a, where in self.cpi.calls if where == "az2" and m in ("delete_vm", "reboot_vm")]
        self.assertIn(("reboot_vm", ["4321"]), refused)


class TestForeignPassUnknownStamp(ForeignPassCase):
    cid = f"{VMID}-00000000-{TOKEN}"

    def test_the_unknown_stamp_skips_the_absent_assertions_and_collides_on_cluster_mismatch(self) -> None:
        self.make().run()
        self.assertTrue(any("unknown cluster stamp" in line for line in self.said))
        self.assertEqual(self.az2.guests, {})
        self.assertEqual(self.az2.destroyed, [VMID])
        # The token step still runs: the VM carries a cluster tag, so the wrong token is judged on a proven cluster.
        self.assertEqual(self.methods("az1"), ["has_vm", "delete_vm"])


# ---------------------------------------------------------------------------
# Secrets, preflight, and the link to the handlers' text
# ---------------------------------------------------------------------------

SECRET_CONTEXT = {
    "director_uuid": "d-2",
    "pve": {
        "host": "pve2.example", "user": "svc@pve", "password": "hunter2", "api_token": "svc!t=sekrit",
        "ca_cert": "-----BEGIN CERTIFICATE-----", "vm_prefix": "az2",
    },
    "agent": {"mbus": "https://user:mbus-pass@10.0.0.6:6868", "ntp": ["pool.ntp.org"]},
    "pve_password": "flat-secret", "pve_api_token": "flat-token", "pve_agent_mbus": "nats://u:p@h",
}


class TestRedactRequest(unittest.TestCase):
    def test_masks_every_secret_in_the_context_and_leaves_the_rest(self) -> None:
        req = foreign.build_request("info", ["a"], "rid", SECRET_CONTEXT)
        text = json.dumps(foreign.redact_request(req))
        for secret in ("hunter2", "sekrit", "mbus-pass", "flat-secret", "flat-token", "BEGIN CERTIFICATE", "nats://u:p@h"):
            self.assertNotIn(secret, text)
        shown = foreign.redact_request(req)
        self.assertEqual(shown["context"]["pve"]["host"], "pve2.example")
        self.assertEqual(shown["context"]["pve"]["user"], "svc@pve")
        self.assertEqual(shown["context"]["pve"]["vm_prefix"], "az2")
        self.assertEqual(shown["context"]["agent"]["ntp"], ["pool.ntp.org"])
        self.assertEqual(shown["context"]["director_uuid"], "d-2")
        self.assertEqual(shown["context"]["request_id"], "rid")
        self.assertEqual(shown["arguments"], ["a"])
        self.assertEqual(shown["method"], "info")

    def test_the_request_that_is_sent_is_not_changed(self) -> None:
        req = foreign.build_request("info", [], "rid", SECRET_CONTEXT)
        foreign.redact_request(req)
        self.assertEqual(req["context"]["pve"]["password"], "hunter2")

    def test_a_default_request_prints_as_it_always_did(self) -> None:
        req = foreign.build_request("info", [], "rid")
        self.assertEqual(json.dumps(foreign.redact_request(req)), json.dumps(req))


class TestLifecycleTrace(LifecycleScriptCase):
    def test_trace_prints_no_context_secret_and_the_cpi_still_receives_it(self) -> None:
        path = self.write_json("az1.json", SECRET_CONTEXT)
        lifecycle = self.load(CPI_CONTEXT_FILE=path, TRACE="1")
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            lifecycle.call("info", [])
        printed = stderr.getvalue()
        self.assertIn("->", printed)
        for secret in ("hunter2", "sekrit", "mbus-pass", "flat-secret"):
            self.assertNotIn(secret, printed)
        (sent,) = self.sent()
        self.assertEqual(sent["context"]["pve"]["api_token"], "svc!t=sekrit")
        self.assertEqual(sent["context"]["agent"]["mbus"], "https://user:mbus-pass@10.0.0.6:6868")


class TestPreflight(unittest.TestCase):
    def setUp(self) -> None:
        self.az2 = FakeCluster("az2", AZ2_STAMP)
        self.said: list[str] = []

    def test_passes_when_the_credentials_can_write(self) -> None:
        foreign.preflight(self.az2, self.said.append)
        self.assertEqual(self.said, [])

    def test_a_transport_that_cannot_write_stops_the_run(self) -> None:
        self.az2.writes_allowed = False
        with self.assertRaisesRegex(_pve_verify.PVEVerifyError, "inspects only"):
            foreign.preflight(self.az2, self.said.append)

    def test_missing_privileges_are_named(self) -> None:
        self.az2.privileges = ["VM.Allocate"]
        with self.assertRaisesRegex(RuntimeError, "lack VM.Allocate on /vms"):
            foreign.preflight(self.az2, self.said.append)

    def test_an_unknown_answer_warns_and_continues(self) -> None:
        self.az2.privileges = None
        foreign.preflight(self.az2, self.said.append)
        self.assertEqual(len(self.said), 1)
        self.assertIn("pool or a single VM", self.said[0])

    def test_an_unreadable_permission_listing_warns_and_continues(self) -> None:
        def broken() -> list:
            raise _pve_verify.PVEVerifyError("GET /access/permissions failed: HTTP 403")

        self.az2.missing_vm_privileges = broken  # type: ignore[method-assign]
        foreign.preflight(self.az2, self.said.append)
        self.assertTrue(any("could not read" in line for line in self.said))


class TestPreflightOnCannedPermissionAnswers(unittest.TestCase):
    """The real verifier against canned /access/permissions answers, one per shape."""

    CONFIG = {"host": "pve2.example", "port": 8006, "node": "pve2", "api_token": "svc@pve!t=s", "verify_ssl": False}

    def _preflight(self, payload: object) -> "tuple[list[str], BaseException | None]":
        said: list[str] = []
        body = json.dumps({"data": payload}).encode("utf-8")
        response = unittest.mock.MagicMock()
        response.read.return_value = body
        response.__enter__ = lambda r: r
        response.__exit__ = unittest.mock.MagicMock(return_value=False)
        verifier = _pve_verify.PVEVerifier(dict(self.CONFIG))
        error: "BaseException | None" = None
        with unittest.mock.patch("urllib.request.urlopen", return_value=response):
            try:
                foreign.preflight(verifier, said.append)
            except RuntimeError as exc:
                error = exc
        return said, error

    def test_every_answer_that_does_not_prove_a_lack_warns_and_lets_the_run_continue(self) -> None:
        shapes = {
            "a list": [],
            "no /vms entry": {},
            "an empty /vms entry": {"/vms": {}},
            "a /vms entry that is not an object": {"/vms": "all"},
            "privileges on a single VM": {"/vms/4321": {"VM.Audit": 1, "VM.Allocate": 1}},
            "privileges on a pool": {"/pool/lab": {"VM.Audit": 1, "VM.Allocate": 1}},
            "some at /vms and the rest on a pool": {"/vms": {"VM.Audit": 1}, "/pool/lab": {"VM.Allocate": 1}},
        }
        for name, payload in shapes.items():
            with self.subTest(name):
                said, error = self._preflight(payload)
                self.assertIsNone(error)
                self.assertEqual(len(said), 1)
                self.assertTrue(said[0].startswith("note:"))

    def test_an_answer_that_shows_the_privileges_granted_is_silent(self) -> None:
        said, error = self._preflight({"/vms": {"VM.Audit": 1, "VM.Allocate": 1}})
        self.assertIsNone(error)
        self.assertEqual(said, [])

    def test_only_a_positive_lack_at_vms_stops_the_run(self) -> None:
        said, error = self._preflight({"/vms": {"VM.Audit": 1}})
        self.assertIsInstance(error, RuntimeError)
        self.assertIn("lack VM.Allocate on /vms", str(error))
        self.assertEqual(said, [])


class TestPassMatchesTheHandlers(unittest.TestCase):
    """The text the pass asserts must still appear in the Go source that produces it."""

    ROOT = SCRIPTS_DIR.parent / "src" / "pve_cpi" / "internal"

    def _source(self, *parts: str) -> str:
        path = self.ROOT.joinpath(*parts)
        self.assertTrue(path.is_file(), f"{path} is missing, so the asserted answers cannot be checked against it")
        return path.read_text(encoding="utf-8")

    def test_refusal_text(self) -> None:
        source = self._source("cpi", "handlers", "vm_ownership.go")
        for fragment in (
            "%s: refusing to answer for CID %s, because VM %d does not exist on this cluster",
            "and the CID names another ",
            "cluster (%s)",
            "%s: refusing to act on VM %d for CID %s, which this Director does not own (%s)",
        ):
            with self.subTest(fragment=fragment):
                self.assertIn(fragment, source)

    def test_reasons(self) -> None:
        source = self._source("cpi", "handlers", "vm_ownership.go")
        for reason in ("cluster-mismatch", "token-missing", "not-bosh", "token-mismatch"):
            with self.subTest(reason=reason):
                self.assertIn(f'= "{reason}"', source)

    def test_vm_not_found(self) -> None:
        source = self._source("errors", "errors.go")
        self.assertIn('fmt.Sprintf("VM not found: %s", vmCID)', source)
        self.assertIn('TypeVMNotFound Type = "Bosh::Clouds::VMNotFound"', source)
        self.assertIn('TypeCloud', source)

    def test_the_pass_asserts_the_same_words(self) -> None:
        pass_source = (SCRIPTS_DIR / "_lifecycle_foreign.py").read_text(encoding="utf-8")
        for fragment in (
            "refusing to answer for CID", "does not exist on this cluster", "(cluster-mismatch)",
            "refusing to act on VM", "does not own (",
        ):
            with self.subTest(fragment=fragment):
                self.assertIn(fragment, pass_source)


class TestRuntimeStateOfTheHandMadeVM(ForeignPassCase):
    def _collide(self) -> None:
        self.make().collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))

    def test_a_vm_that_comes_up_running_is_refused(self) -> None:
        self.az2.runtime[VMID] = {"status": "running"}
        # created_extra cannot set runtime, so make the create land it
        original = self.az2.create_empty_vm

        def create(node, vmid, name):
            upid = original(node, vmid, name)
            self.az2.runtime[int(vmid)] = {"status": "running"}
            return upid

        self.az2.create_empty_vm = create  # type: ignore[method-assign]
        with self.assertRaisesRegex(RuntimeError, "came up 'running'"):
            self._collide()

    def test_a_started_vm_fails_the_step_and_is_left_in_place(self) -> None:
        original = self.cpi._answer

        def starting(method, args, context):
            if method == "reboot_vm":
                self.az2.runtime[VMID] = {"status": "running"}
            return original(method, args, context)

        self.cpi._answer = starting  # type: ignore[method-assign]
        with self.assertRaisesRegex(RuntimeError, "changed after reboot_vm .*status and lock"):
            self._collide()
        self.assertEqual(self.az2.destroyed, [])
        self.assertIn(VMID, self.az2.guests)

    def test_a_lock_fails_the_step(self) -> None:
        original = self.cpi._answer

        def locking(method, args, context):
            if method == "delete_vm":
                self.az2.runtime[VMID] = {"status": "stopped", "lock": "backup"}
            return original(method, args, context)

        self.cpi._answer = locking  # type: ignore[method-assign]
        with self.assertRaisesRegex(RuntimeError, "status and lock"):
            self._collide()
        self.assertEqual(self.az2.destroyed, [])

    def test_a_change_between_the_last_check_and_the_removal_is_not_destroyed(self) -> None:
        original = self.az2.vmid_in_use
        seen = {"n": 0}

        def in_use(vmid):
            seen["n"] += 1
            return original(vmid)

        self.az2.vmid_in_use = in_use  # type: ignore[method-assign]
        handmade = self.make()
        handmade._create_handmade(VMID)
        self.az2.runtime[VMID] = {"status": "running"}
        with self.assertRaisesRegex(RuntimeError, "leaving it in place"):
            handmade._remove_handmade()
        self.assertEqual(self.az2.destroyed, [])


class TestSlowCreate(ForeignPassCase):
    def _slow_create(self) -> "list[float]":
        sleeps: list[float] = []
        original = self.az2.create_empty_vm
        self.landed_after: "int | None" = None

        def create(node, vmid, name):
            upid = original(node, vmid, name)
            late = self.az2.guests.pop(int(vmid))
            self.late = late
            return upid

        self.az2.create_empty_vm = create  # type: ignore[method-assign]

        def await_task(node, upid, timeout=120.0, poll=1.0):
            raise _pve_verify.PVEVerifyError(f"task {upid} still running after {timeout:g}s")

        self.az2.await_task = await_task  # type: ignore[method-assign]
        self.sleeps = sleeps
        return sleeps

    def test_a_create_task_that_times_out_names_the_vm_in_the_failure(self) -> None:
        self._slow_create()
        with self.assertRaisesRegex(RuntimeError, "lifecycle-handmade-4321 on az2 node n1 did not finish.*removed by hand"):
            self.make().collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))

    def test_a_vm_that_lands_after_the_wait_is_found_and_removed(self) -> None:
        self._slow_create()
        probes = {"n": 0}
        original = self.az2.vmid_in_use

        def in_use(vmid):
            probes["n"] += 1
            if probes["n"] == 4:  # the free proof, then the cleanup's first, second, and third look
                self.az2.guests[VMID] = dict(self.late)
            return original(vmid)

        self.az2.vmid_in_use = in_use  # type: ignore[method-assign]
        # The await_task stub still raises while the cleanup destroys, so let the destroy task pass.
        passes = {"fail": True}

        def await_task(node, upid, timeout=120.0, poll=1.0):
            if passes["fail"] and upid == "UPID:create":
                raise _pve_verify.PVEVerifyError("still running")

        self.az2.await_task = await_task  # type: ignore[method-assign]
        with self.assertRaisesRegex(RuntimeError, "did not finish"):
            self.make(sleep=self.sleeps.append).collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))
        self.assertEqual(self.az2.destroyed, [VMID])
        self.assertTrue(self.sleeps)

    def test_a_vm_that_never_lands_is_reported_after_a_bounded_wait(self) -> None:
        self._slow_create()
        with self.assertRaisesRegex(RuntimeError, "did not finish"):
            self.make(sleep=self.sleeps.append, settle_attempts=3, settle_poll=0.5).collision(
                _vmcid.parse(CID), dict(self.az1.guests[VMID])
            )
        self.assertEqual(self.sleeps, [0.5, 0.5, 0.5])
        self.assertTrue(any("did not appear" in line and "lifecycle-handmade-4321" in line for line in self.said))
        self.assertEqual(self.az2.destroyed, [])


class TestSurvivingMutants(ForeignPassCase):
    def _az1_touch_on(self, method: str, cluster: str, absent_cid: bool = False) -> None:
        original = self.cpi._answer

        def touching(m, args, context):
            if m == method and self.cpi._cluster(context).label == cluster:
                self.az1.guests[VMID]["tags"] += ";bosh-deleting"
            return original(m, args, context)

        self.cpi._answer = touching  # type: ignore[method-assign]

    def test_an_az1_change_during_the_bare_twin_fails_the_absent_step(self) -> None:
        original = self.cpi._answer

        def touching(m, args, context):
            if m == "has_vm" and args[0] == str(VMID):
                self.az1.guests[VMID]["tags"] += ";bosh-deleting"
            return original(m, args, context)

        self.cpi._answer = touching  # type: ignore[method-assign]
        with self.assertRaisesRegex(RuntimeError, "changed after the bare CID reached az2"):
            self.make().absent_vmid(_vmcid.parse(CID), dict(self.az1.guests[VMID]))

    def test_an_az1_change_during_the_collision_step_fails_it(self) -> None:
        self._az1_touch_on("reboot_vm", "az2")
        with self.assertRaisesRegex(RuntimeError, "changed after the collision step"):
            self.make().collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))
        self.assertEqual(self.az2.guests, {})

    def test_get_disks_under_warn_must_answer_a_list(self) -> None:
        self.cpi.guard = "warn"
        self.cpi.overrides[("get_disks", "az2")] = ("ok", None)
        with self.assertRaisesRegex(RuntimeError, "get_disks answered None under warn, want a list"):
            self.make(guard="warn").collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))

    def test_a_create_that_died_before_a_snapshot_leaves_a_vm_with_another_name_alone(self) -> None:
        def dying(node, vmid, name):
            self.az2.guests[int(vmid)] = {"name": "an-operators-vm", "digest": "x"}
            raise _pve_verify.PVEVerifyError("POST failed")

        self.az2.create_empty_vm = dying  # type: ignore[method-assign]
        with self.assertRaisesRegex(_pve_verify.PVEVerifyError, "POST failed"):
            self.make().collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))
        self.assertEqual(self.az2.destroyed, [])
        self.assertEqual(self.az2.guests[VMID]["name"], "an-operators-vm")
        self.assertTrue(any("WARNING: could not remove" in line for line in self.said))

    def test_the_unknown_stamp_accepts_only_cluster_mismatch(self) -> None:
        pass_ = self.make(cid=f"{VMID}-00000000-{TOKEN}")
        self.az1.guests[VMID]["tags"] = f"bosh-cpi;bosh-cluster--{AZ1_STAMP};bosh-cid--{TOKEN}"
        original = self.cpi._answer

        def token_missing(m, args, context):
            outcome = original(m, args, context)
            if outcome[0] == "err" and "(cluster-mismatch)" in outcome[1]["message"] and m in ("delete_vm", "reboot_vm"):
                outcome = ("err", dict(outcome[1], message=outcome[1]["message"].replace("(cluster-mismatch)", "(token-missing)")))
            return outcome

        self.cpi._answer = token_missing  # type: ignore[method-assign]
        with self.assertRaisesRegex(RuntimeError, r"names none of \['does not own \(cluster-mismatch\)'\]"):
            pass_.collision(_vmcid.parse(f"{VMID}-00000000-{TOKEN}"), dict(self.az1.guests[VMID]))

    def test_the_known_stamp_accepts_token_missing_as_well(self) -> None:
        original = self.cpi._answer

        def token_missing(m, args, context):
            outcome = original(m, args, context)
            if outcome[0] == "err" and "does not own (cluster-mismatch)" in outcome[1]["message"]:
                outcome = ("err", dict(outcome[1], message=outcome[1]["message"].replace("(cluster-mismatch)", "(token-missing)")))
            return outcome

        self.cpi._answer = token_missing  # type: ignore[method-assign]
        self.make().collision(_vmcid.parse(CID), dict(self.az1.guests[VMID]))

    def test_removing_the_identity_check_alone_would_be_caught_by_the_name(self) -> None:
        handmade = self.make()
        handmade._handmade = {"node": "n1", "vmid": VMID, "name": "lifecycle-handmade-4321", "snapshot": None,
                              "unconfirmed": False}
        self.az2.guests[VMID] = {"name": "someone-elses", "digest": "x"}
        with self.assertRaisesRegex(RuntimeError, "not the .*lifecycle-handmade-4321.* this run made"):
            handmade._remove_handmade()
        self.assertEqual(self.az2.destroyed, [])


if __name__ == "__main__":
    unittest.main()
