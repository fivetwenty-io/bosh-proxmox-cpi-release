"""Offline safety and real transport-boundary tests for disposable fault controls."""
from __future__ import annotations

import copy
import hashlib
import io
import json
import os
import ssl
import stat
import sys
import tempfile
import threading
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _storage_fault_backend as backend
import _storage_placement_faults as faults


def fixture(path="/srv/disposable-nfs"):
    return {"storage_id": "p1", "nfs_server": "nas.example", "ssh_host": "nas.example", "ssh_user": "root",
        "mount_path": path, "filesystem_uuid": "12345678-abcd-1234-abcd-123456789012",
        "marker_nonce": "0123456789abcdef0123456789abcdef", "export_client": "192.0.2.0/24", "quota_uid": 65534, "inode_limit": 10000}


def manifest(item=None):
    return {"version": 1, "faults": {"namespace": "disposable", "backends": [item or fixture()], "proxy_storage_id": "p1", "disk_size_mib": 64}}


class ManifestTests(unittest.TestCase):
    def test_strict_valid_contract(self):
        value = manifest()
        self.assertEqual(faults.validate_fault_manifest(value, {"storage_placement_namespace": "disposable"}), value["faults"])

    def test_no_fixture_is_missing_not_certified(self):
        rows = faults.run_fault_scenarios({}, SimpleNamespace(base_config={}))
        self.assertEqual(len(rows), 15)
        self.assertTrue(all(row["status"] == "missing" for row in rows))

    def test_unsafe_or_ambiguous_controls_fail_validation(self):
        cases = [("mount_path", "/"), ("mount_path", "/srv/../root"), ("mount_path", "relative"),
                 ("ssh_host", "-oProxyCommand=anything"), ("ssh_user", "operator"),
                 ("quota_uid", 0), ("inode_limit", 200001), ("marker_nonce", "short"),
                 ("export_client", "client; reboot"), ("nfs_server", "host/path")]
        for key, value in cases:
            item = fixture(); item[key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                faults.validate_fault_manifest(manifest(item), {"storage_placement_namespace": "disposable"})

    def test_unknown_hooks_and_namespace_mismatch_fail(self):
        value = manifest(); value["faults"]["shell_hook"] = "anything"
        with self.assertRaises(ValueError):
            faults.validate_fault_manifest(value, {"storage_placement_namespace": "disposable"})
        with self.assertRaises(ValueError):
            faults.validate_fault_manifest(manifest(), {"storage_placement_namespace": "production"})

    def test_ssh_uses_fixed_program_strict_host_keys_and_stdin(self):
        controller = faults.BackendController(fixture(), "disposable")
        result = SimpleNamespace(returncode=0, stdout='{"restored":true}')
        with patch.object(faults.subprocess, "run", return_value=result) as run:
            self.assertTrue(controller.invoke("restore")["restored"])
        arguments = run.call_args.args[0]
        self.assertIn("-oStrictHostKeyChecking=yes", arguments)
        self.assertEqual(arguments[arguments.index("--") + 1], "root@nas.example")
        self.assertEqual(json.loads(run.call_args.kwargs["input"])["fixture"]["namespace"], "disposable")
        self.assertNotIn("shell", run.call_args.kwargs)

    def test_backend_failure_retains_bounded_private_output_without_public_disclosure(self):
        with tempfile.TemporaryDirectory() as directory:
            target=Path(directory)/"control"
            controller=faults.BackendController(fixture(), "disposable", evidence_directory=target)
            result=SimpleNamespace(returncode=1,stdout="x"*(1024*1024+1),stderr="PRIVATE-REMOTE-DETAIL")
            with patch.object(faults.subprocess,"run",return_value=result), self.assertRaises(RuntimeError) as caught:
                controller.invoke("apply","backend_quota_exhaustion")
            self.assertNotIn("PRIVATE-REMOTE-DETAIL",str(caught.exception))
            attempt=json.loads(next(target.glob("*.attempt.json")).read_text())
            retained=next(target.glob("*.result.json"));value=json.loads(retained.read_text())
            self.assertEqual(attempt["action"],"apply")
            self.assertEqual(value["stderr"],"PRIVATE-REMOTE-DETAIL")
            self.assertEqual(value["returncode"],1)
            self.assertTrue(value["stdout_truncated"])
            self.assertEqual(len(value["stdout"]),1024*1024)
            self.assertEqual(retained.stat().st_mode & 0o777,0o600)
            self.assertEqual(target.stat().st_mode & 0o777,0o700)

    def test_backend_evidence_failure_before_submission_prevents_command(self):
        with tempfile.TemporaryDirectory() as directory:
            controller=faults.BackendController(fixture(),"disposable",evidence_directory=Path(directory)/"control")
            with patch.object(controller,"retain",side_effect=OSError("full")), patch.object(faults.subprocess,"run") as run, self.assertRaises(OSError):
                controller.invoke("apply","backend_quota_exhaustion")
            run.assert_not_called()

    def test_backend_known_rejection_survives_result_retention_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            controller=faults.BackendController(fixture(),"disposable",evidence_directory=Path(directory)/"control")
            original=controller.retain
            def retain(path,value):
                if path.name.endswith(".result.json"):raise OSError("full")
                original(path,value)
            with patch.object(controller,"retain",side_effect=retain), patch.object(faults.subprocess,"run",return_value=SimpleNamespace(returncode=1,stdout="",stderr="secret")) as run, self.assertRaisesRegex(RuntimeError,"control failed and private evidence"):
                controller.invoke("apply","backend_quota_exhaustion")
            self.assertEqual(run.call_count,1)
            self.assertEqual(len(list(controller.evidence_directory.glob("*.attempt.json"))),1)

    def test_backend_timeout_survives_result_retention_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            controller=faults.BackendController(fixture(),"disposable",evidence_directory=Path(directory)/"control")
            original=controller.retain
            def retain(path,value):
                if path.name.endswith(".result.json"):raise OSError("full")
                original(path,value)
            with patch.object(controller,"retain",side_effect=retain), patch.object(faults.subprocess,"run",side_effect=faults.subprocess.TimeoutExpired("private-command",180)) as run, self.assertRaisesRegex(RuntimeError,"backend transport failed"):
                controller.invoke("apply","backend_quota_exhaustion")
            self.assertEqual(run.call_count,1)
            self.assertEqual(len(list(controller.evidence_directory.glob("*.attempt.json"))),1)


class CrashSupervisionTests(unittest.TestCase):
    def test_unconfirmed_disk_barrier_never_kills_submitted_cpi(self):
        process=SimpleNamespace(pid=321,stdin=io.StringIO(),returncode=None,kill=Mock())
        process.poll=lambda:process.returncode
        calls=[]
        def communicate(timeout):
            calls.append(timeout)
            if len(calls)==1:
                raise faults.subprocess.TimeoutExpired("cpi",timeout)
            process.returncode=0
            return ("{}","")
        process.communicate=communicate
        proxy=Mock(config_override={},evidence={})
        proxy.first_success.wait.return_value=False
        proxy.__enter__=Mock(return_value=proxy)
        proxy.__exit__=Mock(side_effect=lambda *args:self.assertEqual(process.returncode,0))
        runner=SimpleNamespace(cpi_bin="cpi",verification=SimpleNamespace(members={"persistent":{"fault"}}),derived_config=lambda *args:"private",active_resources={})
        checkpoints=[]
        runner.checkpoint=lambda:checkpoints.append(copy.deepcopy(runner.active_resources))
        evidence={}
        with patch.object(faults,"LoopbackPVEFaultProxy",return_value=proxy),patch.object(faults.subprocess,"Popen",return_value=process),patch.object(faults,"_config",return_value={}),patch.object(faults,"_snapshot",return_value={}),patch.object(faults.time,"monotonic",side_effect=[0,121]),self.assertRaisesRegex(RuntimeError,"supervised without injection"):
            faults._proxy_case(runner,"fault","crash_after_submission",64,evidence)
        process.kill.assert_not_called()
        self.assertEqual(calls,[30,30])
        self.assertTrue(any("supervised-fault-cpi-321" in row for row in checkpoints))
        self.assertEqual(runner.active_resources,{})
        self.assertEqual(evidence["supervised_process"]["method"],"create_disk")
        proxy.release.set.assert_called_once()


class BackendProofTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.mount = Path(self.directory.name).resolve()
        self.fixture = {key: value for key, value in fixture(str(self.mount)).items() if key not in ("storage_id", "ssh_host", "ssh_user")}
        self.fixture["namespace"] = "disposable"
        self.marker = {"namespace": "disposable", "nonce": self.fixture["marker_nonce"], "filesystem_uuid": self.fixture["filesystem_uuid"]}
        self.row = {"target": str(self.mount), "source": "/dev/loop42", "uuid": self.fixture["filesystem_uuid"], "fstype": "ext4", "options": "rw,usrquota"}
        self.exports = str(self.mount) + " 192.0.2.0/24(rw,all_squash,anonuid=65534)\n"

    def observe(self, rows=None, exports=None, marker=None):
        outputs = [json.dumps({"filesystems": rows or [self.row]}), json.dumps([{"addr_info": [{"local": "192.0.2.10"}]}]), self.exports if exports is None else exports]
        with patch.object(backend, "read_regular", return_value=self.marker if marker is None else marker), patch.object(backend, "command", side_effect=outputs), patch.object(backend.socket, "getaddrinfo", return_value=[(2, 1, 6, "", ("192.0.2.10", 0))]):
            return backend.inspect(self.fixture)

    def test_dedicated_filesystem_and_exact_export_are_observed(self):
        observed = self.observe()
        self.assertEqual(observed["mount"]["source"], "/dev/loop42")
        self.assertEqual(observed["exports"][0]["client"], "192.0.2.0/24")

    def test_marker_alone_cannot_claim_shared_root_filesystem(self):
        rows = [self.row, dict(self.row, target="/")]
        with self.assertRaisesRegex(RuntimeError, "exactly one"):
            self.observe(rows=rows)

    def test_wrong_uuid_nested_mounts_and_foreign_marker_fail(self):
        cases = [[dict(self.row, uuid="different")], [self.row, dict(self.row, target=str(self.mount / "child"), uuid="other")]]
        for rows in cases:
            with self.subTest(rows=rows), self.assertRaises(RuntimeError):
                self.observe(rows=rows)
        with self.assertRaises(RuntimeError):
            self.observe(marker={"namespace": "production"})

    def test_parent_or_nested_exports_fail_dedication(self):
        for path in (self.mount.parent, self.mount / "child"):
            with self.subTest(path=path), self.assertRaises(RuntimeError):
                self.observe(exports=self.exports + str(path) + " other(rw)\n")

    def test_export_alias_and_mount_symlinks_are_rejected(self):
        alias = self.mount / "alias"
        alias.symlink_to(self.mount, target_is_directory=True)
        with self.assertRaises(RuntimeError):
            self.observe(exports=self.exports + str(alias) + " other(rw)\n")
        self.fixture["mount_path"] = str(alias)
        with self.assertRaises(RuntimeError):
            self.observe()

    def test_nonlocal_nfs_server_is_not_controlled_by_ssh_host(self):
        with patch.object(backend, "read_regular", return_value=self.marker), patch.object(backend, "command", side_effect=[json.dumps({"filesystems": [self.row]}), json.dumps([{"addr_info": [{"local": "192.0.2.10"}]}])]), patch.object(backend.socket, "getaddrinfo", return_value=[(2, 1, 6, "", ("192.0.2.11", 0))]), self.assertRaises(RuntimeError):
            backend.inspect(self.fixture)

    def test_initial_read_only_or_other_export_client_never_mutates(self):
        for observed in ({"mount": dict(self.row, options="ro"), "exports": [{"client": self.fixture["export_client"]}]},
                         {"mount": self.row, "exports": [{"client": "someone-else"}]}):
            with patch.object(backend, "inspect", return_value=observed), patch.object(backend, "command") as call, self.assertRaises(RuntimeError):
                backend.apply(self.fixture, "backend_read_only")
            call.assert_not_called()
            self.assertFalse((self.mount / backend.STATE).exists())

    def test_outage_uses_only_exact_export_and_restores_prior_options(self):
        before = {"mount": self.row, "exports": [{"client": self.fixture["export_client"], "options": "all_squash,anonuid=65534,rw"}]}
        after_fault = {"mount": self.row, "exports": []}
        with patch.object(backend, "inspect", side_effect=[before, after_fault, after_fault, before]), patch.object(backend, "read_regular", side_effect=lambda path: json.loads(Path(path).read_text())), patch.object(backend, "command") as call:
            backend.apply(self.fixture, "backend_export_outage")
            self.assertTrue((self.mount / backend.STATE).is_file())
            result = backend.restore(self.fixture)
        self.assertTrue(result["restored"])
        self.assertEqual(call.call_args_list[0].args[0], ["exportfs", "-u", self.fixture["export_client"] + ":" + str(self.mount)])
        self.assertEqual(call.call_args_list[1].args[0], ["exportfs", "-i", "-o", "all_squash,anonuid=65534,rw", self.fixture["export_client"] + ":" + str(self.mount)])
        self.assertFalse((self.mount / backend.STATE).exists())

    def test_unknown_apply_result_attempts_restore_and_retains_state_if_uncertain(self):
        before = {"mount": self.row, "exports": [{"client": self.fixture["export_client"], "options": "rw"}]}
        with patch.object(backend, "inspect", return_value=before), patch.object(backend, "command", side_effect=RuntimeError("lost SSH outcome")), patch.object(backend, "restore", side_effect=RuntimeError("unverified restoration")) as restore, self.assertRaises(RuntimeError):
            backend.apply(self.fixture, "backend_read_only")
        restore.assert_called_once_with(self.fixture)
        self.assertEqual(json.loads((self.mount / backend.STATE).read_text())["fault"], "backend_read_only")

    def test_preexisting_filler_is_never_adopted_or_deleted(self):
        directory = self.mount / backend.FILL
        directory.mkdir()
        (directory / "0").write_text("foreign evidence")
        before = {"mount": self.row, "exports": [{"client": self.fixture["export_client"], "options": "rw"}]}
        with patch.object(backend, "inspect", return_value=before), patch.object(backend, "restore") as restore, self.assertRaises(RuntimeError):
            backend.apply(self.fixture, "backend_inode_exhaustion")
        restore.assert_not_called()
        self.assertEqual((directory / "0").read_text(), "foreign evidence")
        self.assertFalse((self.mount / backend.STATE).exists())

    def test_replaced_filler_directory_and_unrecorded_file_are_preserved(self):
        directory = self.mount / backend.FILL
        directory.mkdir()
        saved = {"filler_directory": {"device": directory.stat().st_dev, "inode": directory.stat().st_ino}}
        moved = self.mount / "original-filler"
        directory.rename(moved)
        directory.mkdir()
        (directory / "0").write_text("foreign")
        with self.assertRaises(RuntimeError):
            backend.restore_inodes(self.mount, saved)
        self.assertEqual((directory / "0").read_text(), "foreign")
        saved["filler_directory"]["inode"] = directory.stat().st_ino
        log = self.mount / backend.FILL_LOG
        log.write_text("")
        saved["filler_log"] = {"device": log.stat().st_dev, "inode": log.stat().st_ino}
        with self.assertRaises(RuntimeError):
            backend.restore_inodes(self.mount, saved)
        self.assertTrue((directory / "0").exists())

    def test_recorded_owned_inode_entries_restore_without_sweeping_other_files(self):
        directory = self.mount / backend.FILL
        directory.mkdir()
        log = self.mount / backend.FILL_LOG
        with log.open("w") as stream:
            backend.fill_inodes(directory, 3, stream)
        saved = {"filler_directory": {"device": directory.stat().st_dev, "inode": directory.stat().st_ino},
                 "filler_log": {"device": log.stat().st_dev, "inode": log.stat().st_ino}}
        backend.restore_inodes(self.mount, saved)
        self.assertFalse(directory.exists())
        self.assertFalse(log.exists())

    def test_command_success_without_observed_effect_fails_and_restores(self):
        before = {"mount": self.row, "exports": [{"client": self.fixture["export_client"], "options": "rw"}]}
        for fault in ("backend_read_only", "backend_export_outage"):
            state = self.mount / backend.STATE
            if state.exists():
                state.unlink()
            with self.subTest(fault=fault), patch.object(backend, "inspect", return_value=before), patch.object(backend, "command"), patch.object(backend, "restore") as restore, self.assertRaises(RuntimeError):
                backend.apply(self.fixture, fault)
            restore.assert_called_once_with(self.fixture)

    def test_other_fault_run_cannot_restore_saved_state(self):
        before = {"mount": self.row, "exports": [{"client": self.fixture["export_client"], "options": "rw"}]}
        saved = {"fixture": dict(self.fixture, run_id="another-run"), "fault": "backend_read_only", "before": before}
        with patch.object(backend, "inspect", return_value=before), patch.object(backend, "read_regular", return_value=saved), patch.object(backend, "command") as command, self.assertRaises(RuntimeError):
            backend.restore(dict(self.fixture, run_id="current-run"))
        command.assert_not_called()

    def test_inode_budget_rejects_before_filling(self):
        before = {"mount": self.row, "exports": [{"client": self.fixture["export_client"], "options": "rw"}]}
        with patch.object(backend, "inspect", return_value=before), patch.object(backend.os, "statvfs", return_value=SimpleNamespace(f_favail=10001)), self.assertRaises(RuntimeError):
            backend.apply(self.fixture, "backend_inode_exhaustion")
        self.assertFalse((self.mount / backend.FILL).exists())

    def test_quota_control_requires_actual_all_squash_uid_and_user_quotas(self):
        before = {"mount": self.row, "exports": [{"client": self.fixture["export_client"], "options": "rw,root_squash"}]}
        with patch.object(backend, "inspect", return_value=before), patch.object(backend, "quota") as quota, self.assertRaises(RuntimeError):
            backend.apply(self.fixture, "backend_quota_exhaustion")
        quota.assert_not_called()

    def test_quota_default_and_explicit_uid_apply_and_restore_exact_limits(self):
        for options in ("all_squash,hide,no_subtree_check,root_squash,rw,sec=sys,secure,sync,wdelay",
                        "rw,all_squash,anonuid=65534"):
            with self.subTest(options=options):
                before={"mount":self.row,"exports":[{"client":self.fixture["export_client"],"options":options}]}
                current={"block_hard":0,"block_soft":0,"space":16384,"inode_hard":0,"inode_soft":0,"inodes":4,"block_time":0,"inode_time":0,"valid":63}
                writes=[]
                def quota(fixture,write=None):
                    self.assertEqual(fixture["device"],"/dev/loop42")
                    if write is not None:
                        writes.append(dict(write));current.update(write)
                    return dict(current)
                with patch.object(backend,"inspect",return_value=before), patch.object(backend,"quota",side_effect=quota), patch.object(backend,"read_regular",side_effect=lambda path:json.loads(path.read_text())):
                    self.assertTrue(backend.apply(self.fixture,"backend_quota_exhaustion")["applied"])
                    self.assertTrue((self.mount/backend.STATE).exists())
                    self.assertEqual(current["block_hard"],16)
                    self.assertTrue(backend.restore(self.fixture)["restored"])
                self.assertEqual([(w["block_hard"],w["block_soft"]) for w in writes],[(16,0),(0,0)])
                self.assertFalse((self.mount/backend.STATE).exists())

    def test_quota_ambiguous_or_wrong_uid_refuses_before_state_or_write(self):
        for options in ("rw,all_squash,anonuid=123", "rw,all_squash,anonuid=65534,anonuid=65534",
                        "rw,all_squash,all_squash", "rw,all_squash,no_all_squash",
                        "rw,all_squash,anonuid=", "rw,all_squash,anonuid=-1", "rw,all_squash,anonuid=0",
                        "rw,all_squash,anonuid=65534,anonuid=123", "rw,all_squash,anonuid=2147483648",
                        "rw,all_squash,anonuid", "rw,no_all_squash"):
            with self.subTest(options=options):
                before={"mount":self.row,"exports":[{"client":self.fixture["export_client"],"options":options}]}
                with patch.object(backend,"inspect",return_value=before), patch.object(backend,"quota") as quota, self.assertRaises(RuntimeError):
                    backend.apply(self.fixture,"backend_quota_exhaustion")
                quota.assert_not_called()
                self.assertFalse((self.mount/backend.STATE).exists())
        before={"mount":self.row,"exports":[{"client":self.fixture["export_client"],"options":"rw,all_squash"}]}
        with patch.object(backend,"inspect",return_value=before), patch.object(backend,"quota") as quota, self.assertRaises(RuntimeError):
            backend.apply(dict(self.fixture,quota_uid=123),"backend_quota_exhaustion")
        quota.assert_not_called()

    def test_main_restores_read_only_mount_through_read_only_existing_lock(self):
        lock = self.mount / ".bosh-storage-fault.lock"
        lock.write_text("")
        lock.chmod(0o600)
        real_open = os.open
        def readonly_open(path, flags, *args):
            self.assertFalse(flags & os.O_RDWR)
            self.assertFalse(flags & os.O_WRONLY)
            self.assertFalse(flags & os.O_CREAT)
            return real_open(path, flags, *args)
        request = {"action":"restore", "fixture":self.fixture}
        with patch.object(backend, "inspect", return_value={}), patch.object(backend.os, "geteuid", return_value=0), patch.object(backend.os, "open", side_effect=readonly_open), patch.object(backend.os, "fstat", return_value=SimpleNamespace(st_mode=stat.S_IFREG|0o600, st_uid=0)), patch.object(backend, "restore", return_value={"restored":True}) as restore, patch.object(backend.sys, "stdin", io.StringIO(json.dumps(request))), patch.object(backend.sys, "stdout", io.StringIO()) as output:
            backend.main()
        restore.assert_called_once_with(self.fixture)
        self.assertEqual(json.loads(output.getvalue()), {"restored":True})

    def test_restore_never_creates_a_missing_coordination_lock(self):
        with patch.object(backend, "inspect", return_value={}), self.assertRaisesRegex(RuntimeError,"original coordination lock"):
            with backend.backend_lock(self.fixture, "restore"):
                self.fail("entered restoration without original lock")
        self.assertFalse((self.mount / ".bosh-storage-fault.lock").exists())

    def test_unrelated_mount_or_export_change_refuses_before_restore_write(self):
        before = {"mount": self.row, "exports": [{"client":self.fixture["export_client"],"options":"rw"}]}
        saved = {"fixture":self.fixture,"fault":"backend_read_only","before":before}
        for observed in ({"mount":dict(self.row,options="ro,noexec,usrquota"),"exports":before["exports"]},
                         {"mount":dict(self.row,options="ro,usrquota"),"exports":[{"client":"other","options":"rw"}]}):
            with self.subTest(observed=observed), patch.object(backend,"inspect",return_value=observed), patch.object(backend,"read_regular",return_value=saved), patch.object(backend,"command") as command, self.assertRaises(RuntimeError):
                backend.restore(self.fixture)
            command.assert_not_called()


class ProxyTests(unittest.TestCase):
    def setUp(self):
        self.config = {"host": "pve.example", "port": 8006, "api_token": "operator@pve!test=upstream-secret", "storage_placement_namespace": "disposable"}
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        root = Path(directory.name).resolve()
        namespace = root / hashlib.sha256(b"disposable").hexdigest()
        namespace.mkdir(mode=0o700)
        self.config["storage_allocation_journal_dir"] = str(root)
        allocation_id = "12345678-abcd-4abc-8abc-123456789012"
        locator = hashlib.sha256(b"disposable").hexdigest()[:16]
        filename = f"vm-123-bosh-{locator}-alloc-{allocation_id}.raw"
        self.record_path = namespace / ("allocation-" + allocation_id + ".json")
        self.write_record({"version":1,"kind":"disk","namespace":"disposable","id":allocation_id,
            "steps":[{"kind":"create_persistent_volume","state":"planned","target":{"node":"n1","storage":"p1","vmid":123,"intended_volume":"p1:123/"+filename}}]})
        self.record_path.chmod(0o600)
        self.proxy = faults.LoopbackPVEFaultProxy(self.config, {"p1"}, "response_lost_after_allocation")

    def write_record(self, record):
        payload = json.dumps(record, separators=(",", ":"))
        digest = hashlib.sha256(payload.encode()).hexdigest()
        self.record_path.write_text('{"version":1,"sha256":"' + digest + '","payload":' + payload + '}')

    def handler(self, method="POST", storage="p1", namespace="disposable", vmid=123, authorized=True):
        locator = hashlib.sha256(namespace.encode()).hexdigest()[:16]
        fields = {"vmid": vmid, "size": "1G", "filename": f"vm-123-bosh-{locator}-alloc-12345678-abcd-4abc-8abc-123456789012.raw"}
        body = json.dumps(fields).encode()
        return SimpleNamespace(path=f"/api2/json/nodes/n1/storage/{storage}/content", command=method,
            headers={"Content-Length": str(len(body)), "Content-Type": "application/json", "Authorization": "PVEAPIToken=" + (self.proxy.capability if authorized else "wrong")},
            rfile=io.BytesIO(body), wfile=io.BytesIO(), connection=Mock(), send_error=Mock(), send_response=Mock(), send_header=Mock(), end_headers=Mock())

    @staticmethod
    def upstream():
        response = SimpleNamespace(status=200, read=lambda: b'{"data":"p1:123/disk.raw"}')
        connection = Mock(); connection.getresponse.return_value = response
        return connection

    def test_upstream_tls_defaults_verified_and_custom_ca_is_loaded(self):
        context = self.proxy.upstream_context()
        self.assertEqual(context.verify_mode, ssl.CERT_REQUIRED)
        self.assertTrue(context.check_hostname)
        self.proxy.config["pve_ca_cert"] = "public custom CA"
        with patch.object(faults.ssl, "create_default_context") as create:
            self.proxy.upstream_context()
        create.assert_called_once_with(cadata="public custom CA")
        create.return_value.load_default_certs.assert_not_called()
        create.return_value.load_verify_locations.assert_not_called()

    def test_only_explicit_false_disables_upstream_tls(self):
        for value in (None, True):
            self.proxy.config["verify_ssl"] = value
            self.assertEqual(self.proxy.upstream_context().verify_mode, ssl.CERT_REQUIRED)
        self.proxy.config["verify_ssl"] = False
        self.assertEqual(self.proxy.upstream_context().verify_mode, ssl.CERT_NONE)

    def test_unauthorized_client_wrong_storage_namespace_and_vmid_cannot_forward(self):
        cases = [self.handler(authorized=False), self.handler(storage="outside"), self.handler(namespace="production"), self.handler(vmid=456)]
        for handler in cases:
            with self.subTest(path=handler.path), patch.object(faults.http.client, "HTTPSConnection") as connection:
                self.proxy.forward(handler)
            connection.assert_not_called()
            handler.send_error.assert_called()

    def test_response_loss_happens_after_real_forward_and_blocks_duplicate(self):
        first = self.handler(); second = self.handler()
        connection = self.upstream()
        with patch.object(faults.http.client, "HTTPSConnection", return_value=connection):
            self.proxy.forward(first)
            self.proxy.forward(second)
        self.assertEqual(connection.request.call_count, 1)
        self.assertEqual(connection.request.call_args.args[3]["Authorization"], "PVEAPIToken=" + self.config["api_token"])
        self.assertEqual(first.wfile.getvalue(), b"")
        self.assertTrue(self.proxy.first_success.is_set())
        self.assertTrue(self.proxy.evidence["response_withheld"])
        self.assertEqual(self.proxy.evidence["attempted_allocations"], 2)
        second.send_error.assert_called_with(503)

    def test_crash_barrier_is_handshaked_before_response(self):
        self.proxy.mode = "crash_after_submission"
        handler = self.handler()
        with patch.object(faults.http.client, "HTTPSConnection", return_value=self.upstream()):
            thread = threading.Thread(target=self.proxy.forward, args=(handler,))
            thread.start()
            try:
                self.assertTrue(self.proxy.first_success.wait(2))
                self.assertTrue(thread.is_alive())
                self.assertEqual(handler.wfile.getvalue(), b"")
            finally:
                self.proxy.release.set()
                thread.join(2)
        self.assertFalse(thread.is_alive())

    def test_api_failure_is_forwarded_not_counted_as_successful_fault(self):
        connection = self.upstream(); connection.getresponse.return_value.status = 500
        handler = self.handler()
        with patch.object(faults.http.client, "HTTPSConnection", return_value=connection):
            self.proxy.forward(handler)
        self.assertFalse(self.proxy.first_success.is_set())
        self.assertFalse(self.proxy.evidence["response_withheld"])
        handler.send_response.assert_called_with(500)

    def test_config_override_contains_private_capability_and_disables_direct_hops(self):
        self.proxy.server = SimpleNamespace(server_address=("127.0.0.1", 12345))
        override = self.proxy.config_override
        self.assertEqual(override["api_token"], self.proxy.capability)
        self.assertEqual(override["node_endpoints"], {})
        self.assertFalse(override["node_endpoints_discovery"])
        self.assertNotIn("upstream-secret", json.dumps(override))
        self.assertNotIn(self.proxy.capability, json.dumps(self.proxy.evidence))

    def test_proxy_requires_exact_prewrite_journal_target(self):
        original = json.loads(self.record_path.read_text())["payload"]
        for field, value in (("node", "n2"), ("vmid", 999), ("storage", "other"), ("external", True)):
            changed = copy.deepcopy(original)
            changed["steps"][0]["target"][field] = value
            self.write_record(changed)
            with self.subTest(field=field), patch.object(faults.http.client, "HTTPSConnection") as connection:
                self.proxy.forward(self.handler())
            connection.assert_not_called()
        self.record_path.unlink()
        with patch.object(faults.http.client, "HTTPSConnection") as connection:
            self.proxy.forward(self.handler())
        connection.assert_not_called()

    def test_corrupt_or_bare_journal_never_authorizes_forward(self):
        original = self.record_path.read_text()
        malformed = [json.dumps(json.loads(original)["payload"]), original.replace('"sha256":"', '"sha256":"0', 1),
                     original.replace('"version":1', '"version":1,"version":1', 1)]
        for raw in malformed:
            self.record_path.write_text(raw)
            with self.subTest(raw=raw[:40]), patch.object(faults.http.client, "HTTPSConnection") as connection:
                self.proxy.forward(self.handler())
            connection.assert_not_called()

    def test_ambiguous_api_error_follows_actual_success_and_never_reissues(self):
        self.proxy.mode="ambiguous_backend_error"
        handler=self.handler()
        connection=self.upstream()
        with patch.object(faults.http.client,"HTTPSConnection",return_value=connection):
            self.proxy.forward(handler)
            self.proxy.forward(self.handler())
        connection.request.assert_called_once()
        handler.send_response.assert_called_once_with(500)
        self.assertTrue(self.proxy.evidence["response_withheld"])
        self.assertEqual(self.proxy.evidence["allocation_status"],200)

    def test_observer_forwards_actual_failure_without_withholding(self):
        self.proxy.mode = "observe_allocation"
        connection = self.upstream()
        connection.getresponse.return_value.status = 500
        handler = self.handler()
        with patch.object(faults.http.client, "HTTPSConnection", return_value=connection):
            self.proxy.forward(handler)
        self.assertEqual(self.proxy.evidence["allocation_status"], 500)
        self.assertEqual(self.proxy.evidence["forwarded_allocations"], 1)
        self.assertFalse(self.proxy.evidence["response_withheld"])
        self.assertTrue(handler.wfile.getvalue())


class OutcomeTests(unittest.TestCase):
    def test_production_summary_ids_and_retained_actual_artifact_match(self):
        before = {"journal": {"records": [{"ID": "old", "SHA256": "hash"}], "evidence": []}, "volumes": []}
        after = {"journal": {"records": before["journal"]["records"] + [{"ID": "new", "SHA256": "newhash"}], "evidence": [{"volume_id": "p1:new", "allocation_id": "new"}]}, "volumes": [("n1", "p1", "p1:new", 1024)]}
        result = faults._verify_single_attempt(before, after)
        self.assertEqual(result["new_allocations"][0]["ID"], "new")
        for mutated in (dict(after, volumes=[("n1", "p1", "p1:unrecorded", 1024)]), dict(after, journal={"records": [], "evidence": []})):
            with self.assertRaises(RuntimeError):
                faults._verify_single_attempt(before, mutated)

    def test_existing_artifact_loss_and_multiple_allocations_fail(self):
        before = {"journal": {"records": [], "evidence": []}, "volumes": [("n1", "p1", "old", 1)]}
        after = copy.deepcopy(before); after["volumes"] = []
        with self.assertRaises(RuntimeError):
            faults._verify_single_attempt(before, after)
        after = copy.deepcopy(before); after["journal"]["records"] = [{"ID": "one"}, {"ID": "two"}]
        with self.assertRaises(RuntimeError):
            faults._verify_single_attempt(before, after)

    def test_submission_requires_exact_nonreturnable_record_and_owned_volume(self):
        proof = {"allocation_id":"new"}
        valid = {"new_allocations":[{"ID":"new","State":"reconciliation_required","CID":""}], "retained_volumes":["p1:new"]}
        faults._verify_submission(proof, valid, require_volume=True)
        for bad in ({"new_allocations":[],"retained_volumes":["p1:new"]},
                    {"new_allocations":[{"ID":"other","State":"planned"}],"retained_volumes":["p1:new"]},
                    {"new_allocations":[{"ID":"new","State":"ready_to_return"}],"retained_volumes":["p1:new"]},
                    {"new_allocations":valid["new_allocations"],"retained_volumes":[]}):
            with self.subTest(bad=bad), self.assertRaises(RuntimeError):
                faults._verify_submission(proof, bad, require_volume=True)

    def test_unrelated_preflight_error_does_not_certify_backend_fault(self):
        observed = {"mount":{"source":"/dev/test"},"exports":[]}
        controller = Mock(run_id="a"*32)
        controller.invoke.side_effect = lambda action, *args: ({"restored":True,"after":observed} if action=="restore" else observed)
        proof = {"attempted_allocations":0,"forwarded_allocations":0,"blocked_mutations":0,"allocation_status":None}
        proxy = Mock(evidence=proof, config_override={})
        proxy.__enter__ = Mock(return_value=proxy)
        proxy.__exit__ = Mock(return_value=False)
        runner = SimpleNamespace(base_config={}, verification=SimpleNamespace(members={"persistent":{"p1"}},diagnostic={"capacities":[{"Pair":{"StorageID":"p1","BackingKey":"nfs://nas.example/srv/disposable-nfs","Node":"n1"}}]}),
                                 derived_config=lambda config,label:"/private/config",call_once=Mock(side_effect=__import__("_storage_placement_scenarios").CPIRejected("unrelated selector rejection")))
        with patch.object(faults,"BackendController",return_value=controller), patch.object(faults,"LoopbackPVEFaultProxy",return_value=proxy), patch.object(faults,"_snapshot",return_value={}):
            with self.assertRaisesRegex(RuntimeError,"actual failed allocation"):
                faults._backend_case(runner,fixture(),"disposable","backend_read_only",64,{})
        self.assertEqual([call.args[0] for call in controller.invoke.call_args_list], ["inspect","apply","restore"])

    def test_backend_case_requires_server_failure_and_exact_unresolved_record(self):
        from _storage_placement_scenarios import CPIRejected
        for status, rejection, expected in ((500,CPIRejected("quota failure"),True),(403,CPIRejected("authorization"),False),(500,RuntimeError("transport harness failure"),False)):
            observed={"mount":{"source":"/dev/test"},"exports":[]}
            controller=Mock(run_id="a"*32)
            controller.invoke.side_effect=lambda action,*args:({"restored":True,"after":observed} if action=="restore" else observed)
            proof={"attempted_allocations":1,"forwarded_allocations":1,"blocked_mutations":0,"allocation_status":status,"allocation_id":"new"}
            proxy=Mock(evidence=proof,config_override={})
            proxy.__enter__=Mock(return_value=proxy)
            proxy.__exit__=Mock(return_value=False)
            runner=SimpleNamespace(base_config={},verification=SimpleNamespace(members={"persistent":{"p1"}},diagnostic={"capacities":[{"Pair":{"StorageID":"p1","BackingKey":"nfs://nas.example/srv/disposable-nfs","Node":"n1"}}]}),derived_config=lambda *args:"/private/config",call_once=Mock(side_effect=rejection))
            before={"journal":{"records":[],"evidence":[]},"volumes":[]}
            after={"journal":{"records":[{"ID":"new","State":"reconciliation_required","CID":""}],"evidence":[]},"volumes":[]}
            with self.subTest(status=status,error=type(rejection).__name__), patch.object(faults,"BackendController",return_value=controller), patch.object(faults,"LoopbackPVEFaultProxy",return_value=proxy), patch.object(faults,"_snapshot",side_effect=[before,after]):
                if expected:
                    result=faults._backend_case(runner,fixture(),"disposable","backend_quota_exhaustion",64,{})
                    self.assertTrue(result["restore_verified"])
                else:
                    with self.assertRaises(RuntimeError):
                        faults._backend_case(runner,fixture(),"disposable","backend_quota_exhaustion",64,{})
            self.assertEqual(controller.invoke.call_args.args,("restore",))

    def test_failed_scenario_leaves_remaining_rows_missing_without_more_faults(self):
        runner = SimpleNamespace(base_config={"storage_placement_namespace":"disposable"})
        with patch.object(faults,"_backend_case",side_effect=RuntimeError("failed")) as backend_case:
            rows = faults.run_fault_scenarios(manifest(),runner)
        self.assertEqual(len(rows),15)
        self.assertEqual([row["status"] for row in rows], ["failed"]+["missing"]*14)
        backend_case.assert_called_once()


if __name__ == "__main__":
    unittest.main()
