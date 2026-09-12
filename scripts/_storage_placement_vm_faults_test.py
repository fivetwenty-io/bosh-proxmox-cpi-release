"""Offline exact-journal and real-forwarding checks for VM fault barriers."""
import copy
import hashlib
import io
import json
import tempfile
import threading
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

import _storage_placement_vm_faults as faults


class VMProxyTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name).resolve()
        self.config = {"host":"pve.example","api_token":"u@pve!t=SECRET","storage_placement_namespace":"test", "storage_allocation_journal_dir":str(self.root),"storage_sets":{"e":{"names":["e1"]}}}
        self.proxy = faults.JournalVMFaultProxy(self.config,"agent",mode="response_loss")
        self.record = {"id":"11111111-1111-4111-8111-111111111111","kind":"vm","namespace":"test","agent_id":"agent","state":"planned", "intent":{"plan":{"VMExecution":{"DiskFormat":"qcow2"},"Definitions":{"e1":{"Type":"nfs"}},"Targets":[{"Role":"root","StorageID":"e1","Mechanism":"full_clone","Source":{"Node":"n1","TemplateVMID":100,"VolumeID":"images:import/root.qcow2"}},{"Role":"ephemeral","StorageID":"e1","VirtualBytes":1024**3}]}},"steps":[]}
        namespace = self.root / hashlib.sha256(b"test").hexdigest()
        namespace.mkdir(mode=0o700)
        self.path = namespace / ("allocation-"+self.record["id"]+".json")

    def save(self, kind, storage="e1", parameters=None):
        self.record["steps"] = [{"id":"mutation","kind":kind,"state":"planned","target":{"node":"n1","vmid":123,"storage":storage,"backing":"nfs://nas/e1"},"parameters":parameters}]
        if kind == "vm.Storage.CreateVolume":
            self.record["steps"][0]["target"]["intended_volume"] = "e1:123/vm-123-ephemeral-0.qcow2"
        payload = json.dumps(self.record,separators=(",",":"))
        self.path.write_text('{"version":1,"sha256":"'+hashlib.sha256(payload.encode()).hexdigest()+'","payload":'+payload+'}')
        self.path.chmod(0o600)

    def handler(self, path="/nodes/n1/qemu",method="POST",fields=None):
        body = json.dumps(fields or {"vmid":123,"virtio0":"e1:1,import-from=images:import/root.qcow2"}).encode()
        return SimpleNamespace(path="/api2/json"+path,command=method,headers={"Authorization":"PVEAPIToken="+self.proxy.capability,"Content-Type":"application/json","Content-Length":str(len(body))},rfile=io.BytesIO(body),wfile=io.BytesIO(),connection=Mock(),send_error=Mock(),send_response=Mock(),send_header=Mock(),end_headers=Mock())

    def test_each_phase_requires_exact_production_kind_path_and_target(self):
        cases = [
            ("vm.QEMU.Create","POST","/nodes/n1/qemu",{"vmid":123,"virtio0":"e1:1"},None),
            ("vm.Nodes.CreateQemuClone","POST","/nodes/n1/qemu/100/clone",{"newid":123,"storage":"e1","target":"n1","full":True,"format":"qcow2"},None),
            ("vm.Storage.CreateVolume","POST","/nodes/n1/storage/e1/content",{"vmid":123,"filename":"vm-123-ephemeral-0.qcow2","size":"1G","format":"qcow2"},None),
            ("vm.Storage.Upload","POST","/nodes/n1/storage/e1/upload",{"content":"iso","filename":"vm-123-config.iso"},None),
            ("vm.Cluster.CreateHaResources","POST","/cluster/ha/resources",{"sid":"vm:123"},None),
            ("vm.Cluster.CreateHaRules","POST","/cluster/ha/rules",{"rule":"owned","type":"node-affinity","resources":"vm:123"},{"version":1,"kind":"ha_rule","rule":"owned","type":"node-affinity","resources":"vm:123"}),
        ]
        for kind,method,path,fields,parameters in cases:
            with self.subTest(kind=kind):
                self.save(kind,parameters=parameters)
                record,step = self.proxy.planned_mutation(method,path,fields)
                self.assertEqual(step["kind"],kind)
                self.assertEqual(record["agent_id"],"agent")
                with self.assertRaises(ValueError):
                    self.proxy.planned_mutation(method,path+"/outside",fields)
                self.record["agent_id"]="other"
                self.save(kind,parameters=parameters)
                with self.assertRaises(ValueError):
                    self.proxy.planned_mutation(method,path,fields)
                self.record["agent_id"]="agent"

    def test_uuid_ephemeral_name_requires_exact_birth_intent(self):
        self.save("vm.Storage.CreateVolume")
        step = self.record["steps"][0]
        name = "vm-123-bosh-"+hashlib.sha256(b"test").hexdigest()[:16]+"-ephemeral-"+self.record["id"]+".qcow2"
        fields = {"vmid":123,"filename":name,"size":"1G","format":"qcow2"}
        step["target"]["intended_volume"] = "e1:123/"+name
        args = (self.record,step,"POST","/nodes/n1/storage/e1/content",fields)
        self.assertTrue(faults.JournalVMFaultProxy.matches(*args))
        step["target"]["intended_volume"] = "e1:123/vm-123-ephemeral-0.qcow2"
        self.assertFalse(faults.JournalVMFaultProxy.matches(*args))
        fields["filename"] = "vm-123-ephemeral-0.qcow2"
        self.assertTrue(faults.JournalVMFaultProxy.matches(*args))
        fields["filename"] = name.replace(self.record["id"],"22222222-2222-4222-8222-222222222222")
        self.assertFalse(faults.JournalVMFaultProxy.matches(*args))

    def test_wrong_vm_import_source_or_storage_never_forwards(self):
        self.save("vm.QEMU.Create")
        for fields in ({"vmid":124},{"vmid":123,"virtio0":"other:1"},{"vmid":123,"virtio0":"e1:1,import-from=images:other"}):
            with patch.object(faults.http.client,"HTTPSConnection") as connection:
                self.proxy.forward(self.handler(fields=fields))
            connection.assert_not_called()

    def test_success_is_forwarded_once_before_loss_and_duplicate_is_blocked(self):
        self.save("vm.QEMU.Create")
        connection=Mock()
        connection.getresponse.return_value=SimpleNamespace(status=200,read=lambda:b'{"data":"UPID:n1:123:"}')
        first=self.handler()
        with patch.object(faults.http.client,"HTTPSConnection",return_value=connection):
            self.proxy.forward(first)
            self.proxy.forward(self.handler())
        self.assertEqual(connection.request.call_count,1)
        self.assertTrue(self.proxy.first_success.is_set())
        self.assertEqual(first.wfile.getvalue(),b"")
        self.assertEqual(self.proxy.evidence["fault_step"]["upid"],"UPID:n1:123:")
        self.assertEqual(self.proxy.evidence["blocked_mutations"],1)
        self.assertNotIn("SECRET",json.dumps(self.proxy.evidence))

    def test_clone_response_receipt_binds_original_step_and_destination(self):
        self.save("vm.Nodes.CreateQemuClone")
        fields={"newid":123,"storage":"e1","target":"n1","full":True,"format":"qcow2","description":"exact allocation marker","sshkeys":"EXCLUDED_PRIVATE_VALUE"}
        task="UPID:n1:00088AE5:03547171:6AA18319:qmclone:100:pmx@pve!pmx:"
        connection=Mock()
        connection.getresponse.return_value=SimpleNamespace(status=200,read=lambda:json.dumps({"data":task}).encode())
        with patch.object(faults.http.client,"HTTPSConnection",return_value=connection):
            self.proxy.forward(self.handler(path="/nodes/n1/qemu/100/clone",fields=fields))
        self.assertEqual(connection.request.call_count,1)
        receipt=self.proxy.evidence["recovered_task_evidence"]
        self.assertEqual(receipt["step"],self.record["steps"][0])
        self.assertEqual(receipt["allocation_id"],self.record["id"])
        self.assertEqual(receipt["namespace"],"test")
        self.assertEqual(receipt["path"],"/nodes/n1/qemu/100/clone")
        self.assertEqual(receipt["method"],"POST")
        self.assertEqual(receipt["response_status"],200)
        self.assertEqual(receipt["upid"],task)
        self.assertEqual(receipt["request_identity"],{"newid":123,"storage":"e1","target":"n1","description":"exact allocation marker","full":True,"format":"qcow2"})
        self.assertNotIn("EXCLUDED_PRIVATE_VALUE",json.dumps(receipt))
        self.record["steps"][0]["target"]["vmid"]=999
        self.assertEqual(receipt["step"]["target"]["vmid"],123)

    def test_receipt_is_durable_before_fault_barrier(self):
        self.save("vm.QEMU.Create")
        task="UPID:n1:00088AE5:03547171:6AA18319:qmcreate:123:pmx@pve!pmx:"
        payload=json.dumps({"data":task}).encode()
        connection=Mock()
        connection.getresponse.return_value=SimpleNamespace(status=200,read=lambda:payload)
        checked=[]
        def barrier():
            retained=self.proxy.evidence["recovered_task_evidence_file"]
            data=Path(retained["path"]).read_bytes()
            self.assertEqual(hashlib.sha256(data).hexdigest(),retained["sha256"])
            self.assertEqual(json.loads(data)["upid"],task)
            checked.append(True)
        self.proxy.first_success=Mock(set=barrier)
        with patch.object(faults.http.client,"HTTPSConnection",return_value=connection), patch.object(faults.os,"fsync",wraps=faults.os.fsync) as sync:
            self.proxy.forward(self.handler())
        self.assertEqual(checked,[True])
        self.assertEqual(sync.call_count,2)
        self.assertEqual(self.proxy.receipt_path.stat().st_mode & 0o777,0o600)

    def test_receipt_retention_failure_forwards_success_without_injection(self):
        self.save("vm.QEMU.Create")
        payload=b'{"data":"UPID:n1:123:"}'
        connection=Mock()
        connection.getresponse.return_value=SimpleNamespace(status=200,read=lambda:payload)
        first=self.handler()
        with patch.object(faults.http.client,"HTTPSConnection",return_value=connection), patch.object(faults.os,"fsync",side_effect=OSError(28,"No space left")):
            self.proxy.forward(first)
        self.assertEqual(connection.request.call_count,1)
        self.assertEqual(first.wfile.getvalue(),payload)
        self.assertFalse(self.proxy.first_success.is_set())
        self.assertFalse(self.proxy.evidence["response_withheld"])
        self.assertEqual(self.proxy.evidence["injection_aborted"],"response_evidence_retention_failed")
        self.assertEqual(self.proxy.vm_mode,"observe")
        self.assertTrue(self.proxy.receipt_path.exists())

    def test_crash_barrier_waits_only_after_successful_real_submission(self):
        self.proxy.vm_mode="crash"
        self.save("vm.QEMU.Create")
        connection=Mock()
        connection.getresponse.return_value=SimpleNamespace(status=200,read=lambda:b'{"data":"UPID:n1:123:"}')
        with patch.object(faults.http.client,"HTTPSConnection",return_value=connection):
            thread=threading.Thread(target=self.proxy.forward,args=(self.handler(),))
            thread.start()
            try:
                self.assertTrue(self.proxy.first_success.wait(2))
                self.assertTrue(thread.is_alive())
                connection.request.assert_called_once()
            finally:
                self.proxy.release.set()
                thread.join(2)
        self.assertFalse(thread.is_alive())

    def test_preexisting_volume_and_unbounded_cleanup_never_gain_authority(self):
        self.save("vm.QEMU.AttachDisk")
        with self.assertRaises(ValueError):
            self.proxy.planned_mutation("PUT","/nodes/n1/qemu/123/config",{"scsi1":"e1:unrelated.raw"})
        self.save("vm.delete.destroy")
        self.proxy.planned_mutation("DELETE","/nodes/n1/qemu/123",{"purge":"1","destroy-unreferenced-disks":"0"})
        with self.assertRaises(ValueError):
            self.proxy.planned_mutation("DELETE","/nodes/n1/qemu/123",{"purge":"1","destroy-unreferenced-disks":"1"})

    def test_observation_hook_receives_actual_read_response_without_mutation(self):
        self.proxy.response_hook = Mock(return_value=b'{"data":[]}')
        connection=Mock()
        connection.getresponse.return_value=SimpleNamespace(status=200,read=lambda:b'{"data":[{"avail":1}]}')
        handler=self.handler("/nodes/n1/storage","GET")
        with patch.object(faults.http.client,"HTTPSConnection",return_value=connection):
            self.proxy.forward(handler)
        self.assertEqual(self.proxy.response_hook.call_args.args[:3],("GET","/nodes/n1/storage",b'{"data":[{"avail":1}]}'))
        self.assertEqual(handler.wfile.getvalue(),b'{"data":[]}')
        self.assertEqual(self.proxy.evidence["mutations"],[])

    def test_upload_parser_preserves_exact_filename_and_rejects_duplicate_fields(self):
        body=b'--bound\r\nContent-Disposition: form-data; name="content"\r\n\r\niso\r\n--bound\r\nContent-Disposition: form-data; name="filename"; filename="vm-123-config.iso"\r\nContent-Type: application/octet-stream\r\n\r\nISO\r\n--bound--\r\n'
        self.assertEqual(faults.request_fields("multipart/form-data; boundary=bound",body),{"content":"iso","filename":"vm-123-config.iso"})
        with self.assertRaises(ValueError):
            faults.request_fields("application/x-www-form-urlencoded",b"vmid=123&vmid=124")


class VMCaseTests(unittest.TestCase):
    def test_completed_remote_task_never_implicitly_settles_unknown_journal(self):
        from _storage_placement_scenarios import CPIRejected
        allocation="11111111-1111-4111-8111-111111111111"
        step={"step_id":"root","allocation_id":allocation,"vmid":123,"upid":"UPID:n1:123:","kind":"vm.QEMU.Create","target":{"node":"n1","vmid":123}}
        record={"id":allocation,"steps":[{"id":"root","state":"planned"}],"intent":{"plan":{"Targets":[{"StorageID":"e1","Role":"root","VirtualBytes":1024}]}}}
        proof={"fault_step":step,"response_withheld":True,"blocked_mutations":0}
        proxy=Mock(config_override={},evidence=proof)
        proxy.__enter__=Mock(return_value=proxy)
        proxy.__exit__=Mock(return_value=False)
        proxy.current_record.return_value=record
        row={"ID":allocation,"State":"reconciliation_required"}
        before={"records":[],"audit":{"evidence":[]}}
        after={"records":[row],"generation_index_healthy":True,"cluster_continuity":True,"audit":{"complete":True,"vm_scan_complete":True,"evidence":[]}}
        directory=tempfile.TemporaryDirectory();self.addCleanup(directory.cleanup)
        runner=SimpleNamespace(report_path=Path(directory.name)/"report.json",cpi_bin="cpi",config_path="config",assert_authority=Mock(),configured_policy=lambda:{},vm_arguments=lambda _: ["agent"],derived_config=lambda *args:"private",call_once=Mock(side_effect=CPIRejected("lost")),audit=Mock(side_effect=[before,after]),verification=SimpleNamespace(volume_inventory=Mock(side_effect=[[],[("n1","e1","e1:owned",1024)]])),verifier=SimpleNamespace(qemu_config=lambda *_:{"scsi0":"e1:owned"},_get=lambda path: {"status":"stopped","exitstatus":"OK"} if "/tasks/" in path else {"size":1024}))
        with patch.object(faults,"JournalVMFaultProxy",return_value=proxy), patch.object(faults.subprocess,"run",return_value=SimpleNamespace(returncode=0,stdout=json.dumps(after))):
            result=faults.run_vm_fault(runner,"root","response_loss",{})
        runner.call_once.assert_called_once()
        self.assertFalse(result["task"]["journal_settled"])
        self.assertEqual(record["steps"][0]["state"],"planned")
        self.assertTrue(result["resources_retained_for_reconciliation"])
        runner.report_path=Path(directory.name)/"second-report.json"
        runner.audit=Mock(side_effect=[before,after])
        runner.verification.volume_inventory=Mock(side_effect=[[],[]])
        with patch.object(faults,"JournalVMFaultProxy",return_value=proxy), patch.object(faults.subprocess,"run",return_value=SimpleNamespace(returncode=0,stdout=json.dumps(after))), self.assertRaisesRegex(RuntimeError,"artifacts"):
            faults.run_vm_fault(runner,"root","response_loss",{})

    def test_aborted_crash_injection_supervises_cpi_without_killing(self):
        directory=tempfile.TemporaryDirectory();self.addCleanup(directory.cleanup)
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
        proxy=Mock(config_override={},evidence={"injection_aborted":"response_evidence_retention_failed"})
        proxy.first_success.wait.return_value=False
        proxy.__enter__=Mock(return_value=proxy)
        proxy.__exit__=Mock(side_effect=lambda *args:self.assertEqual(process.returncode,0))
        checkpoints=[]
        runner=SimpleNamespace(report_path=Path(directory.name)/"report.json",cpi_bin="cpi",configured_policy=lambda:{},vm_arguments=lambda _:["agent"],audit=lambda:{},verification=SimpleNamespace(volume_inventory=lambda _:[]),verifier=None,derived_config=lambda *args:"private",active_resources={})
        runner.checkpoint=lambda:checkpoints.append(copy.deepcopy(runner.active_resources))
        evidence={}
        with patch.object(faults,"JournalVMFaultProxy",return_value=proxy),patch.object(faults.subprocess,"Popen",return_value=process),self.assertRaisesRegex(RuntimeError,"supervised without injection"):
            faults.run_vm_fault(runner,"root","crash",evidence)
        process.kill.assert_not_called()
        self.assertEqual(calls,[30,30])
        self.assertTrue(any("supervised-fault-cpi-321" in row for row in checkpoints))
        self.assertEqual(runner.active_resources,{})
        self.assertEqual(evidence["supervised_process"]["state"],"completed")
        proxy.release.set.assert_called_once()

    def test_clone_missing_marker_remains_incomplete_and_unrelated_conflicts_fail(self):
        record={"id":"allocation"}
        fault={"kind":"vm.Nodes.CreateQemuClone"}
        report={"generation_index_healthy":True,"cluster_continuity":True,"audit":{"complete":False,"vm_scan_complete":True,"conflicts":["recorded VM target for allocation allocation lacks matching ownership provenance"]}}
        directory=tempfile.TemporaryDirectory();self.addCleanup(directory.cleanup)
        runner=SimpleNamespace(report_path=Path(directory.name)/"report.json",cpi_bin="cpi",config_path="config",assert_authority=Mock())
        def observe():
            with patch.object(faults.subprocess,"run",return_value=SimpleNamespace(returncode=1,stdout=json.dumps(report))):
                return faults.vm_fault_audit(runner,record,fault)
        self.assertFalse(observe()["audit"]["complete"])
        report["audit"]["conflicts"].append("unrelated allocation")
        with self.assertRaisesRegex(RuntimeError,"unrelated"):
            observe()
        report["audit"]["conflicts"].pop()
        report["audit"]["issues"]=["storage unreadable"]
        with self.assertRaisesRegex(RuntimeError,"incomplete"):
            observe()


if __name__ == "__main__":
    unittest.main()
