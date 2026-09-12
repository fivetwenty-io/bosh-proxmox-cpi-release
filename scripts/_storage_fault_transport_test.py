"""Transport contract tests; no PVE or SSH processes are started."""
import copy
import json
import os
import shlex
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import _storage_placement_faults as faults
import _storage_fault_backend as backend
from _storage_placement_faults_test import fixture, manifest


class PMXTransportTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.binary = self.root / "pmx binary"
        self.config = self.root / "private config.yml"
        self.identity = self.root / "physical identity"
        self.known_hosts = self.root / "physical known hosts"
        self.binary.write_bytes(b"candidate executable")
        self.config.write_text("private-token-do-not-print")
        self.identity.write_text("private-key-do-not-print")
        self.known_hosts.write_text("physical-host ssh-ed25519 public-key")
        self.value = fixture()
        self.value["transport"] = {
            "kind": "pmx",
            "binary": str(self.binary),
            "config": str(self.config),
            "context": "lab",
            "node": "sm-0",
            "identity_file": str(self.identity),
            "known_hosts_file": str(self.known_hosts),
            "host_key_alias": "physical-host",
        }

    def test_fixed_pmx_invocation_and_restore_binding(self):
        faults.validate_fault_manifest(manifest(self.value), {"storage_placement_namespace": "disposable"})
        controller = faults.BackendController(self.value, "disposable", "a" * 32)
        self.value["transport"]["node"] = "changed-after-construction"
        response = SimpleNamespace(returncode=0, stdout='{"restored":true}')
        with patch.dict(os.environ, {"PMX_CONTEXT": "wrong", "PMX_NODE": "wrong", "PMX_CONFIG": "wrong"}), patch.object(faults.subprocess, "run", return_value=response) as run:
            controller.invoke("apply", "backend_read_only")
            controller.invoke("restore")
        first, second = run.call_args_list
        argv = first.args[0]
        self.assertEqual(argv[:11], [str(self.binary), "--config", str(self.config), "--context", "lab", "ssh", "--user", "root", "--identity", str(self.identity), "sm-0"])
        self.assertEqual(argv, second.args[0])
        self.assertIn("-oStrictHostKeyChecking=yes", argv)
        self.assertIn("-oBatchMode=yes", argv)
        self.assertIn("-oUserKnownHostsFile=" + str(self.known_hosts), argv)
        self.assertIn("-oHostKeyAlias=physical-host", argv)
        self.assertIn("-oConnectTimeout=15", argv)
        self.assertEqual(shlex.split(argv[-1]), [Path(faults.__file__).with_name("_storage_fault_backend.py").read_text()])
        self.assertEqual(first.kwargs["timeout"], 180)
        self.assertNotIn("shell", first.kwargs)
        self.assertFalse(any(key.startswith("PMX_") for key in first.kwargs["env"]))
        sent = json.loads(first.kwargs["input"])["fixture"]
        self.assertEqual(sent, json.loads(second.kwargs["input"])["fixture"])
        self.assertEqual(len(sent["transport_binding"]), 64)
        self.assertNotIn("transport", sent)
        self.assertNotIn("private-token", first.kwargs["input"])

    def test_real_subprocess_preserves_spaced_paths_and_json_stdin(self):
        self.binary.write_text("#!" + sys.executable + "\nimport json,sys\nrequest=json.load(sys.stdin)\nprint(json.dumps({'argv':sys.argv[1:], 'fixture':request['fixture']}))\n")
        self.binary.chmod(0o700)
        controller = faults.BackendController(self.value, "disposable", "a" * 32)
        result = controller.invoke("inspect")
        self.assertEqual(result["argv"][:4], ["--config", str(self.config), "--context", "lab"])
        self.assertEqual(result["fixture"], controller.fixture)
        self.assertEqual(shlex.split(result["argv"][-1]), [Path(faults.__file__).with_name("_storage_fault_backend.py").read_text()])

    def test_changed_binding_refuses_before_any_process(self):
        for path in (self.binary, self.config, self.identity, self.known_hosts):
            with self.subTest(path=path.name):
                controller = faults.BackendController(self.value, "disposable")
                old = path.read_bytes()
                path.write_bytes(old + b"changed")
                with patch.object(faults.subprocess, "run") as run, self.assertRaisesRegex(RuntimeError, "binding changed"):
                    controller.invoke("restore")
                run.assert_not_called()
                path.write_bytes(old)

    def test_failures_never_fallback_or_expose_output(self):
        outcomes = [OSError("private-token-do-not-print"), subprocess.TimeoutExpired("private-token-do-not-print", 180, output="private-token-do-not-print"), SimpleNamespace(returncode=1, stdout="private-token-do-not-print", stderr="private-token-do-not-print"), SimpleNamespace(returncode=0, stdout="private-token-do-not-print")]
        for outcome in outcomes:
            with self.subTest(outcome=type(outcome).__name__):
                controller = faults.BackendController(self.value, "disposable")
                options = {"side_effect": outcome} if isinstance(outcome, Exception) else {"return_value": outcome}
                with patch.object(faults.subprocess, "run", **options) as run, self.assertRaises(RuntimeError) as caught:
                    controller.invoke("restore")
                self.assertEqual(run.call_count, 1)
                self.assertEqual(run.call_args.args[0][0], str(self.binary))
                self.assertNotIn("private-token", str(caught.exception))

    def test_schema_rejects_null_partial_hooks_and_option_identities(self):
        bad = [None, {}, dict(self.value["transport"], command="echo unsafe"), dict(self.value["transport"], node="-Jother"), dict(self.value["transport"], context="a b"), dict(self.value["transport"], host_key_alias="a b"), dict(self.value["transport"], config="relative"), dict(self.value["transport"], identity_file="relative"), dict(self.value["transport"], known_hosts_file="relative"), dict(self.value["transport"], kind="ssh")]
        for transport in bad:
            with self.subTest(transport=transport):
                value = copy.deepcopy(self.value)
                value["transport"] = transport
                with self.assertRaises(ValueError):
                    faults.validate_fault_manifest(manifest(value), {"storage_placement_namespace": "disposable"})

    def test_binding_is_stable_across_restore_process_and_changes_with_target(self):
        first = faults.BackendController(self.value, "disposable", "a" * 32)
        restored = faults.BackendController(self.value, "disposable", "a" * 32)
        self.assertEqual(first.fixture, restored.fixture)
        changed = copy.deepcopy(self.value)
        changed["transport"]["node"] = "sm-1"
        self.assertNotEqual(first.fixture, faults.BackendController(changed, "disposable", "a" * 32).fixture)

    def test_remote_restore_rejects_changed_transport_before_mutation(self):
        controller = faults.BackendController(self.value, "disposable", "a" * 32)
        observed = {"mount": {"source": "/dev/disposable"}}
        saved = {"fixture": dict(controller.fixture, transport_binding="other"), "before": observed}
        with patch.object(backend, "inspect", return_value=observed), patch.object(backend, "read_regular", return_value=saved), patch.object(backend, "command") as command, self.assertRaisesRegex(RuntimeError, "identity changed"):
            backend.restore(controller.fixture)
        command.assert_not_called()


if __name__ == "__main__":
    unittest.main()
