"""Offline production-boundary checks for the fixed Director workflows."""
import copy
import io
import tempfile
import hashlib
from pathlib import Path
import json
import types
import unittest
import threading
from unittest.mock import patch
import _storage_placement_director as subject


class DirectorTests(unittest.TestCase):
    def workflow(self):
        value = subject.DirectorScenarios.__new__(subject.DirectorScenarios)
        value.name = "storage-cert-test"
        value.fixture = {"environment": "https://director.example"}
        value.task_evidence = []
        value.command_evidence_dir = Path(self.enterContext(tempfile.TemporaryDirectory()))
        value.require_workload_stemcells = unittest.mock.Mock()
        value.runner = types.SimpleNamespace(active_resources={"director_workflow":{}}, checkpoint=lambda:None)
        return value

    def test_operation_requires_invocation_task_not_unrelated_success(self):
        value = self.workflow()
        value.tasks = unittest.mock.Mock(side_effect=[{}, {"12": {"state": "done"}}])
        value.bosh = lambda *args, **kwargs: {"task_ids": [], "stdout_sha256": "hash"}
        with self.assertRaisesRegex(RuntimeError, "identify"):
            value.operation(["deploy", "/fixture.json"])
        value.tasks = unittest.mock.Mock(side_effect=[{}, {"12": {"state": "done"}, "13": {"state": "error"}}])
        value.bosh = lambda *args, **kwargs: {"task_ids": ["13"]}
        with self.assertRaisesRegex(RuntimeError, "successfully"):
            value.operation(["recreate", "service/id"])
        value.tasks = unittest.mock.Mock(side_effect=[{}, {"12": {"state": "done"}, "13": {"state": "error"}}])
        value.bosh = lambda *args, **kwargs: {"task_ids": ["12"]}
        self.assertEqual(value.operation(["deploy", "/fixture.json"])["tasks"][0]["id"], "12")

    def test_global_delete_vm_binds_one_new_task_to_exact_cid(self):
        value = self.workflow()
        row = {"id": 12, "state": "done", "deployment": None, "description": "delete vm 100076"}
        value.bosh = unittest.mock.Mock(side_effect=[[], {"task_ids": ["12"]}, [row]])
        result = value.operation(["delete-vm", "100076"], scoped=False)
        self.assertEqual(result["tasks"][0]["vm_cid"], "100076")
        self.assertIsNone(result["tasks"][0]["deployment"])
        for call in (value.bosh.call_args_list[0], value.bosh.call_args_list[2]):
            self.assertEqual(call.args[0], ["curl", "/tasks?verbose=2&limit=100"])

    def test_global_delete_rejects_wrong_stale_failed_or_ambiguous_task(self):
        good = {"id": 12, "state": "done", "deployment": None, "description": "delete vm 100076"}
        cases = [([], ["12"], {**good, "description": "delete vm 100077"}),
                 ([], ["12"], {**good, "deployment": "storage-cert-test"}),
                 ([], ["12"], {**good, "state": "error"}),
                 ([good], ["12"], good), ([], ["13"], good),
                 ([], ["12", "13"], good), ([], [], good)]
        for before, emitted, row in cases:
            with self.subTest(before=before, emitted=emitted, row=row):
                value = self.workflow()
                value.bosh = unittest.mock.Mock(side_effect=[before, {"task_ids": emitted}, [row]])
                with self.assertRaises(RuntimeError):
                    value.operation(["delete-vm", "100076"], scoped=False)
                self.assertEqual(sum(call.args[0][0] == "delete-vm" for call in value.bosh.call_args_list), 1)
                self.assertEqual(value.task_evidence, [])

    def test_unscoped_other_workflow_refuses_before_io(self):
        value = self.workflow()
        value.bosh = unittest.mock.Mock()
        with self.assertRaisesRegex(RuntimeError, "unscoped workflow"):
            value.operation(["delete-deployment"], scoped=False)
        value.bosh.assert_not_called()

    def test_cli_extracts_only_emitted_task_identifiers(self):
        value = self.workflow()
        with patch.object(subject.subprocess, "run", return_value=types.SimpleNamespace(returncode=0, stdout="Task 42\nTask 42 done\nignored Task 99\n")) as call:
            self.assertEqual(value.bosh(["recreate", "service/id"])["task_ids"], ["42"])
            self.assertIn("storage-cert-test", call.call_args.args[0])

    def test_retained_audit_paths_fail_before_ssh(self):
        value = subject.DirectorObserver.__new__(subject.DirectorObserver)
        value.fixture = {"linux_cpi_sha256": "a"*64}
        value.artifact = {}
        for path in ("/tmp/cpi", "/var/vcap/store/pve_cpi/storage-certification/../candidate-cpi"):
            with self.assertRaisesRegex(RuntimeError, "path"):
                value.snapshot(audit_binary=path)
        with self.assertRaisesRegex(RuntimeError, "identities"):
            value.snapshot(audit_binary="/var/vcap/store/pve_cpi/storage-certification/"+"a"*32+"/candidate-cpi", audit_config="/var/vcap/store/pve_cpi/storage-certification/"+"b"*32+"/candidate-config.json")

    def test_strict_fixture_catches_actual_new_required_fields_and_types(self):
        fixture = dict(dedicated_director=True, expected_root_mechanism="linked_clone", observer_policy={k:None for k in subject.DIRECTOR_POLICY_KEYS}, environment="director", namespace="remote", ssh={"host":"host", "user":"vcap"}, candidate_archive="/a.tgz", candidate_sha256="a"*64, linux_cpi_sha256="b"*64, compilation_release="/compile.tgz", deployment_manifest="/deploy.json", cloud_config="/cloud.json", baseline_cloud_config="/baseline.json", compilation_vm_type="compile",workload_vm_type="service",errand_vm_type="errand",errand_name="test")
        self.assertEqual(subject.validate_director_manifest({"director":fixture},{"storage_placement_namespace":"local"}), fixture)
        for key in ("compilation_release", "linux_cpi_sha256"):
            bad=copy.deepcopy(fixture); del bad[key]
            with self.assertRaises(RuntimeError): subject.validate_director_manifest({"director":bad},{"storage_placement_namespace":"local"})
        for value in ({}, 12, None):
            bad=copy.deepcopy(fixture); bad["ssh"]["host"]=value
            with self.assertRaises(RuntimeError): subject.validate_director_manifest({"director":bad},{"storage_placement_namespace":"local"})

    def test_private_known_hosts_path_rejects_relative_and_newline(self):
        fixture = dict(dedicated_director=True, expected_root_mechanism="linked_clone", observer_policy={k:None for k in subject.DIRECTOR_POLICY_KEYS}, environment="director", namespace="remote", ssh={"host":"host", "user":"vcap", "known_hosts_file":"/private/known hosts"}, candidate_archive="/a.tgz", candidate_sha256="a"*64, linux_cpi_sha256="b"*64, compilation_release="/compile.tgz", deployment_manifest="/deploy.json", cloud_config="/cloud.json", baseline_cloud_config="/baseline.json", compilation_vm_type="compile",workload_vm_type="service",errand_vm_type="errand",errand_name="test")
        self.assertEqual(subject.validate_director_manifest({"director":fixture},{"storage_placement_namespace":"local"}),fixture)
        for path in ("relative", "/private/key\noption", 3, None):
            fixture["ssh"]["known_hosts_file"]=path
            with self.assertRaisesRegex(RuntimeError,"known hosts"):
                subject.validate_director_manifest({"director":fixture},{"storage_placement_namespace":"local"})

    def test_observer_and_rollout_use_same_explicit_trust_file(self):
        from _storage_placement_rollout import RolloutScenarios
        fixture={"namespace":"remote","linux_cpi_sha256":"a"*64,"ssh":{"host":"host","user":"vcap","known_hosts_file":"/private/known hosts","identity_file":"/private/key"}}
        observer=subject.DirectorObserver.__new__(subject.DirectorObserver)
        observer.fixture=fixture;observer.artifact={}
        rollout=RolloutScenarios.__new__(RolloutScenarios)
        rollout.director=types.SimpleNamespace(fixture=fixture,run_id="b"*32)
        for invoke in (observer.snapshot,rollout.retain_auditor):
            with patch.object(subject.subprocess,"run",return_value=types.SimpleNamespace(returncode=1)) as call:
                with self.assertRaises(RuntimeError):invoke()
                argv=call.call_args.args[0]
                self.assertIn("StrictHostKeyChecking=yes",argv)
                self.assertIn('UserKnownHostsFile="/private/known hosts"',argv)
                self.assertIn("IdentitiesOnly=yes",argv)
                self.assertIn("/private/key",argv)

    def test_real_event_ndjson_is_parsed_as_separate_objects(self):
        value=self.workflow()
        raw='{"stage":"Applying problem resolutions","state":"started"}\n{"stage":"Applying problem resolutions","state":"finished","task":"VM for service/uuid (0) is missing"}\n'
        with patch.object(subject.subprocess,"run",return_value=types.SimpleNamespace(returncode=0,stdout=raw)):
            events=value.bosh(["curl","/tasks/12/output?type=event"],as_json=True)
        self.assertEqual(len(events),2)
        self.assertEqual(events[1]["state"],"finished")

    def test_task_api_rejects_foreign_deployment(self):
        value=self.workflow()
        value.bosh=lambda *a,**kw:[{"id":12,"state":"done","deployment":"another"}]
        with self.assertRaisesRegex(RuntimeError,"deployment"):
            value.tasks()

    def test_missing_director_fixtures_report_every_case_without_io(self):
        runner=types.SimpleNamespace(base_config={})
        with patch.object(subject,"DirectorScenarios",side_effect=AssertionError("live constructor")):
            rows=subject.run_director_scenarios({},runner)
        self.assertEqual([row["scenario_id"] for row in rows],list(subject.DIRECTOR_IDS))
        self.assertTrue(all(row["status"]=="missing" for row in rows))

    def test_constructor_failure_keeps_every_required_row_visible(self):
        runner = types.SimpleNamespace(base_config={})
        with patch.object(subject, "validate_director_manifest", return_value={"fixture": True}), patch.object(subject, "DirectorScenarios", side_effect=ValueError("invalid archive or manifest")):
            rows = subject.run_director_scenarios({}, runner)
        self.assertEqual([row["scenario_id"] for row in rows], list(subject.DIRECTOR_IDS))
        self.assertEqual([row["status"] for row in rows], ["failed"] + ["missing"] * (len(subject.DIRECTOR_IDS) - 1))

    def test_initial_baseline_mismatch_refuses_any_cloud_update(self):
        value = self.workflow()
        value.bosh = unittest.mock.Mock(return_value={"Tables": [{"Rows": []}]})
        value.baseline_cloud = {"vm_types": [{"name": "declared"}]}
        value.read_cloud_config = lambda: {"vm_types": [{"name": "actually-present"}]}
        value.snapshot = unittest.mock.Mock(side_effect=AssertionError("later observation"))
        with self.assertRaisesRegex(RuntimeError, "actual initial"):
            value.compilation()
        self.assertEqual(value.bosh.call_count, 1)
        self.assertEqual(value.bosh.call_args.args[0], ["deployments"])
        value.snapshot.assert_not_called()

    def test_compilation_observer_finishes_before_final_absence_audit(self):
        value = self.workflow()
        value.baseline_cloud = {}
        value.read_cloud_config = lambda: {}
        value.cloud_path, value.deployment_path = "cloud.json", "deployment.json"
        value.fixture["compilation_release"] = "compile.tgz"
        value.bosh = unittest.mock.Mock(return_value={"Tables": [{"Rows": []}]})
        live = {"id": "allocation", "cid": "101", "state": "ready_to_return"}
        value.snapshot = unittest.mock.Mock(side_effect=[{"records": {}, "policy": {}}, {"records": {"allocation": live}}, {"records": {"allocation": {**live, "state": "deleted"}}}])
        value.records_with_tag = lambda snapshot, role: list(snapshot["records"].values())
        observed = threading.Event()
        def read_actual(snapshot, record):
            observed.set()
            return {"vm": {"devices": {"virtio0": "pool:root"}, "iso": {"ide2": "pool:iso"}}}
        value.live_vm_record = read_actual
        def operation(command):
            self.assertTrue(observed.wait(5), "observer never reached the real-read boundary")
            return {"tasks": [{"id": "1", "state": "done"}]}
        value.operation = operation
        value.runner.verifier = types.SimpleNamespace(vm_exists=lambda cid: False)
        value.runner.verification = types.SimpleNamespace(volume_inventory=lambda verifier: [])
        value.service_evidence = lambda snapshot: {"vm_cid": "service"}
        value.observer = types.SimpleNamespace(artifact={"sha256": "candidate"})
        value.runner.policy, value.cloud, value.deployment = {}, {}, {}
        value.retain_compilation_observation = unittest.mock.Mock()
        with unittest.mock.patch("_storage_placement_director.validate_director_policy"):
            result = value.compilation()
        self.assertTrue(result["all_compilation_resources_absent"])
        self.assertEqual(len(result["compilation_vms"]), 1)
        self.assertEqual(value.snapshot.call_count, 3)

    def test_cloud_config_restoration_uses_safe_semantic_decode(self):
        value=self.workflow()
        responses=[types.SimpleNamespace(returncode=0,stdout="vm_types: []\n"),types.SimpleNamespace(returncode=0,stdout='{"vm_types":[]}')]
        with patch.object(subject.subprocess,"run",side_effect=responses) as calls:
            self.assertEqual(value.read_cloud_config(),{"vm_types":[]})
        self.assertIn("safe_load",calls.call_args_list[1].args[0][-1])
        self.assertIn("aliases: false",calls.call_args_list[1].args[0][-1])

    def test_resurrection_refuses_stateful_disabled_before_mutation(self):
        value=self.workflow()
        value.snapshot=lambda:{"auto_fix_stateful_nodes":False,"resurrector_enabled":True}
        value.bosh=unittest.mock.Mock(side_effect=AssertionError("mutation"))
        with self.assertRaisesRegex(RuntimeError,"stateful resurrection"):
            value.resurrection()
        value.bosh.assert_not_called()


    def test_resurrection_requires_exact_instance_event_and_restores_prior_state(self):
        for task_instance in ("service/uuid", "service/foreign"):
            with self.subTest(task_instance=task_instance):
                value=self.workflow()
                value.fixture["resurrection_timeout_seconds"]=60
                old={"instance":"service/uuid","vm_cid":"100","allocation_uuid":"old","disks":[]}
                current={**old,"vm_cid":"101","allocation_uuid":"new"}
                value.service=old
                snapshot={"auto_fix_stateful_nodes":True,"resurrector_enabled":True,"records":{"old":{"state":"deleted"}}}
                value.snapshot=lambda:snapshot
                value.service_evidence=lambda *args:current
                value.runner.report={}
                value.runner.verifier=types.SimpleNamespace(vm_exists=lambda cid:False)
                value.tasks=unittest.mock.Mock(side_effect=[{}, {"42":{"state":"done","description":"scan and fix","user":"hm","deployment":value.name}}])
                commands=[]
                def bosh(command,**kwargs):
                    commands.append(command)
                    if command==["curl","/resurrection"]: return {"resurrection":False}
                    if command[-1].endswith("?type=event"):
                        return [{"stage":"Applying problem resolutions","state":"finished","task":"VM for '"+task_instance+" (0)' missing"}]
                    return {}
                value.bosh=bosh
                value.operation=lambda *args,**kwargs:{}
                if task_instance==old["instance"]:
                    result=value.resurrection()
                    self.assertTrue(result["resurrection_setting_restored"])
                    self.assertIn(["update-resurrection","off"],commands)
                    self.assertTrue(value.runner.report["director_recovery"]["restored"])
                else:
                    with self.assertRaisesRegex(RuntimeError,"exact instance"):
                        value.resurrection()
                    self.assertFalse(value.runner.report["director_recovery"]["restored"])
                    self.assertNotIn(["update-resurrection","off"],commands)

    def test_fixed_remote_program_reads_real_layout_without_secrets(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            namespace="remote"
            files={
                "/var/vcap/jobs/pve_cpi/config/cpi.json":{"storage_placement_namespace":namespace,"storage_allocation_journal_dir":"/journal","password":"never-export","vm_storage":"nfs-e"},
                "/var/vcap/bosh/spec.json":{"job":{"templates":[{"name":"pve_cpi","version":"job-fp"}]},"packages":{"pve_cpi":{"version":"source.2"}}},
                "/var/vcap/bosh/settings.json":{"vm":{"name":"guest-name"},"agent_id":"agent"},
                "/var/vcap/jobs/director/config/director.yml":{"port":25556,"scan_and_fix":{"auto_fix_stateful_nodes":True},"secret":"never-export"},
                "/var/vcap/jobs/health_monitor/config/health_monitor.yml":{"plugins":[{"name":"resurrector","options":{"password":"never-export"}}]},
                "/journal/"+hashlib.sha256(namespace.encode()).hexdigest()+"/authority.json":{"payload":{"enrollment":{"cluster_id":"actual-cluster"}}},
            }
            for name,value in files.items():
                target=root/name.lstrip("/");target.parent.mkdir(parents=True,exist_ok=True);target.write_text(json.dumps(value))
            dmi=root/"sys/class/dmi/id/product_uuid";dmi.parent.mkdir(parents=True);dmi.write_text("9e924a72-b79d-4f41-a616-ecc000bf5605\n")
            binary=root/"var/vcap/packages/pve_cpi/bin/cpi";binary.parent.mkdir(parents=True);binary.write_bytes(b"candidate")
            class VirtualPath:
                def __init__(self,value): self.value=str(value)
                def __str__(self): return self.value
                def __truediv__(self,value): return VirtualPath(self.value.rstrip("/")+"/"+str(value))
                @property
                def parent(self): return VirtualPath(str(Path(self.value).parent))
                def open(self,*args,**kwargs): return (root/self.value.lstrip("/")).open(*args,**kwargs)
                def read_text(self): return (root/self.value.lstrip("/")).read_text()
                def read_bytes(self): return (root/self.value.lstrip("/")).read_bytes()
                def lstat(self):
                    if self.value=="/journal":return types.SimpleNamespace(st_mode=0o40700,st_uid=1000,st_gid=1000)
                    return (root/self.value.lstrip("/")).lstat()
            mapped=VirtualPath
            output=io.StringIO()
            local_info=io.BytesIO(b'{"uuid":"11111111-1111-4111-8111-111111111111"}')
            with patch("pwd.getpwnam",return_value=types.SimpleNamespace(pw_uid=1000,pw_gid=1000)),patch("urllib.request.build_opener",return_value=types.SimpleNamespace(open=lambda *a,**k:local_info)),patch("pathlib.Path",side_effect=mapped),patch("sys.stdin",io.StringIO(json.dumps({"namespace":"remote", "audit_sha256":hashlib.sha256(b"candidate").hexdigest()}))),patch("sys.stdout",output),patch("subprocess.run",return_value=types.SimpleNamespace(returncode=0,stdout='{"records":null}')) as audit_call:
                exec(compile(subject.REMOTE_AUDIT,"remote","exec"),{})
            self.assertEqual(audit_call.call_args.kwargs["user"],1000)
            self.assertEqual(audit_call.call_args.kwargs["group"],1000)
            self.assertEqual(audit_call.call_args.kwargs["extra_groups"],[])
            result=json.loads(output.getvalue())
            self.assertEqual(result["cluster_id"],"actual-cluster")
            self.assertEqual(result["product_uuid"],"9e924a72-b79d-4f41-a616-ecc000bf5605")
            self.assertNotIn("vm_cid",result)
            self.assertTrue(result["auto_fix_stateful_nodes"] and result["resurrector_enabled"])
            self.assertEqual(result["records"],{})
            self.assertNotIn("never-export",output.getvalue())
            with patch("pathlib.Path",side_effect=mapped), patch("sys.stdin",io.StringIO(json.dumps({"namespace":"remote", "audit_sha256":"0"*64}))), patch("subprocess.run") as mutation, self.assertRaisesRegex(RuntimeError,"executable checksum"):
                exec(compile(subject.REMOTE_AUDIT,"remote","exec"),{})
            mutation.assert_not_called()

    def test_guest_identity_resolves_unique_vm_without_settings_vmid(self):
        identity = "9e924a72-b79d-4f41-a616-ecc000bf5605"
        verifier = types.SimpleNamespace(
            _get=lambda path: [{"type": "qemu", "vmid": 123, "node": "n1"}, {"type": "lxc", "vmid": 124}],
            qemu_config=lambda vmid, node: {"smbios1": "uuid=" + identity.upper() + ",manufacturer=QEMU"})
        self.assertEqual(subject.observed_director_vmid(verifier, identity), "123")
        for rows in ([], {}, [{"type": "qemu", "vmid": 123, "node": "n1"}, {"type": "qemu", "vmid": 125, "node": "n2"}],
                     [{"type": "qemu", "vmid": 123}], [{"type": "unexpected"}]):
            with self.subTest(rows=rows):
                verifier._get = lambda path: rows
                with self.assertRaises(RuntimeError):
                    subject.observed_director_vmid(verifier, identity)
        for invalid in (None, "", "0" * 36, "00000000-0000-0000-0000-000000000000"):
            with self.subTest(identity=invalid), self.assertRaises(RuntimeError):
                subject.observed_director_vmid(verifier, invalid)
        verifier._get = lambda path: [{"type": "qemu", "vmid": 123, "node": "n1"}, {"type": "qemu", "vmid": 125, "node": "n2"}]
        for malformed in ({"uuid": identity}, "uuid=invalid", "uuid=", "uuid=" + identity + ",uuid=" + identity):
            with self.subTest(smbios=malformed):
                verifier.qemu_config = lambda vmid, node: {"smbios1": "uuid=" + identity if vmid == "123" else malformed}
                with self.assertRaises(RuntimeError):
                    subject.observed_director_vmid(verifier, identity)
        for malformed in ("0123", "١٢٣", True, 1.5, 1000000000):
            with self.subTest(vmid=malformed):
                verifier._get = lambda path: [{"type": "qemu", "vmid": malformed, "node": "n1"}]
                with self.assertRaises(RuntimeError):
                    subject.observed_director_vmid(verifier, identity)
        verifier._get = unittest.mock.Mock(side_effect=RuntimeError("inventory unavailable"))
        with self.assertRaisesRegex(RuntimeError, "inventory unavailable"):
            subject.observed_director_vmid(verifier, identity)

    def test_remote_source_bounds_and_serialization_names(self):
        compile(subject.REMOTE_AUDIT, "remote-audit", "exec")
        self.assertIn('authority["enrollment"]["cluster_id"]', subject.REMOTE_AUDIT)
        self.assertIn('64*1024*1024', subject.REMOTE_AUDIT)
        self.assertIn('if identity in envelopes', subject.REMOTE_AUDIT)


if __name__ == "__main__":
    unittest.main()
