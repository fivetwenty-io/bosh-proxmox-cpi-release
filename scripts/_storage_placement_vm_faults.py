"""Journal-bound VM fault barriers for an explicitly disposable scenario."""
from __future__ import annotations

import hashlib
import hmac
import http.client
import json
import os
import re
import socket
import stat
import subprocess
import time
import uuid
from email.parser import BytesParser
from email.policy import default
from pathlib import Path
from urllib.parse import parse_qs, quote, unquote, urlsplit

from _storage_placement_faults import LoopbackPVEFaultProxy
from _storage_placement_scenarios import ScenarioFailure, verified_record_envelope, _unique_json_object

PHASE_KINDS = {
    "root": {"vm.QEMU.Create", "vm.Nodes.CreateQemuClone"},
    "ephemeral": {"vm.Storage.CreateVolume"},
    "iso": {"vm.Storage.Upload"},
    "ha": {"vm.Cluster.CreateHaResources", "vm.Cluster.UpdateHaResources", "vm.Cluster.CreateHaRules"},
}


def request_fields(content_type, body):
    if content_type.startswith("multipart/form-data"):
        message = BytesParser(policy=default).parsebytes(b"Content-Type: " + content_type.encode() + b"\r\nMIME-Version: 1.0\r\n\r\n" + body)
        if not message.is_multipart():
            raise ValueError("invalid upload body")
        fields = {}
        for part in message.iter_parts():
            key = part.get_param("name", header="content-disposition")
            if not key or key in fields:
                raise ValueError("ambiguous upload fields")
            fields[key] = part.get_filename() if key == "filename" else part.get_payload(decode=True).decode()
        return fields
    if not body:
        return {}
    if "application/json" in content_type:
        value = json.loads(body, object_pairs_hook=_unique_json_object, parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite mutation field")))
        if not isinstance(value, dict):
            raise ValueError("mutation body must be an object")
        return value
    pairs = parse_qs(body.decode(), keep_blank_values=True, strict_parsing=True)
    if any(len(values) != 1 for values in pairs.values()):
        raise ValueError("ambiguous mutation fields")
    return {key: values[0] for key, values in pairs.items()}


class JournalVMFaultProxy(LoopbackPVEFaultProxy):
    """Forward only active planned writes for one exact scenario agent ID."""

    def __init__(self, config, agent_id, phase="root", mode="observe", response_hook=None, receipt_path=None):
        if phase not in PHASE_KINDS or mode not in {"observe", "response_loss", "crash"}:
            raise ValueError("unsupported VM fault phase or mode")
        super().__init__(config, {name for value in config.get("storage_sets", {}).values() for name in value.get("names", [])}, "observe_allocation")
        self.vm_mode, self.phase, self.agent_id = mode, phase, agent_id
        self.response_hook = response_hook
        self.receipt_path = Path(receipt_path) if receipt_path else Path(config["storage_allocation_journal_dir"])/("fault-response-"+str(uuid.uuid4())+".json")
        if mode != "observe":
            parent = self.receipt_path.parent
            info = parent.stat()
            if not self.receipt_path.is_absolute() or parent.resolve() != parent or not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or self.receipt_path.exists() or self.receipt_path.is_symlink():
                raise ValueError("fault response destination must be new and privately owned")
        self.poisoned = False
        self.evidence.update({"phase": phase, "mode": mode, "mutations": [], "fault_step": None})

    def retain_response(self, receipt):
        path = self.receipt_path
        parent = path.parent
        info = parent.stat()
        if not path.is_absolute() or parent.resolve() != parent or not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise ValueError("response evidence parent is not a private owned directory")
        data = (json.dumps(receipt,sort_keys=True,separators=(",",":"))+"\n").encode()
        if len(data) > 65536:
            raise ValueError("response evidence exceeds retained bound")
        fd = os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
        with os.fdopen(fd,"wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        directory = os.open(parent,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        self.evidence["recovered_task_evidence_file"] = {"path":str(path),"sha256":hashlib.sha256(data).hexdigest()}

    def current_record(self):
        root = Path(self.config["storage_allocation_journal_dir"])
        directory = root / hashlib.sha256(self.config["storage_placement_namespace"].encode()).hexdigest()
        if directory.resolve() != directory:
            raise ValueError("journal path is redirected")
        records = []
        for path in directory.glob("allocation-*.json"):
            descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
            with os.fdopen(descriptor, "rb") as stream:
                info = os.fstat(stream.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
                    raise ValueError("journal record is not private")
                record = verified_record_envelope(stream.read(16 * 1024 * 1024 + 1))
            if record.get("kind") == "vm" and record.get("agent_id") == self.agent_id:
                if record.get("namespace") != self.config["storage_placement_namespace"] or path.name != "allocation-" + record["id"] + ".json":
                    raise ValueError("journal identity differs from scenario")
                if record.get("state") not in {"deleted", "cleaned", "vm_deleted_retained"}:
                    records.append(record)
        if len(records) != 1:
            raise ValueError("scenario lacks one active VM journal identity")
        return records[0]

    def planned_mutation(self, method, path, fields):
        record = self.current_record()
        active = max(0, len(record.get("attempts", [])) - 1)
        steps = [step for step in record.get("steps", []) if step.get("attempt", 0) == active and step.get("state") == "planned"]
        matches = [step for step in steps if self.matches(record, step, method, path, fields)]
        if len(matches) != 1:
            raise ValueError("write lacks one exact planned mutation")
        return record, matches[0]

    @staticmethod
    def matches(record, step, method, path, fields):
        target, kind = step.get("target", {}), step.get("kind")
        node, vmid, storage = target.get("node"), str(target.get("vmid", "")), target.get("storage")
        if target.get("external") or not node or not vmid.isdigit() or int(vmid) <= 0:
            return False
        prefix = "/nodes/" + node + "/qemu"
        if kind == "vm.QEMU.Create":
            return method == "POST" and path == prefix and str(fields.get("vmid")) == vmid and JournalVMFaultProxy.drives_within_plan(record, fields, creating=True)
        if kind == "vm.Nodes.CreateQemuClone":
            plan = (record.get("attempts") or [{"plan":record["intent"]}])[-1]["plan"]["plan"]
            roots = [item for item in plan.get("Targets", []) if item.get("Role") == "root"]
            if len(roots) != 1:
                return False
            source = roots[0].get("Source", {})
            expected = "/nodes/" + str(source.get("Node")) + "/qemu/" + str(source.get("TemplateVMID")) + "/clone"
            mechanism = roots[0].get("Mechanism")
            full = mechanism == "full_clone"
            if mechanism not in {"full_clone","linked_clone"} or fields.get("full") not in ((True,1,"1") if full else (False,0,"0")):
                return False
            if full:
                if fields.get("storage") != storage or fields.get("format") != plan.get("VMExecution",{}).get("DiskFormat"):
                    return False
            elif "storage" in fields or "format" in fields:
                return False
            return method == "POST" and path == expected and str(fields.get("newid")) == vmid and fields.get("target", source.get("Node")) == node
        if kind == "vm.Storage.CreateVolume":
            plan = (record.get("attempts") or [{"plan":record["intent"]}])[-1]["plan"]["plan"]
            ephemeral = [item for item in plan.get("Targets",[]) if item.get("Role") == "ephemeral" and item.get("StorageID") == storage]
            if len(ephemeral) != 1 or type(ephemeral[0].get("VirtualBytes")) is not int:
                return False
            size = str((ephemeral[0]["VirtualBytes"]+1024**3-1)//1024**3)+"G"
            if fields.get("size") != size or fields.get("format") != JournalVMFaultProxy.ephemeral_format(plan,storage) or set(fields)-{"vmid","filename","size","format"}:
                return False
            names = [JournalVMFaultProxy.ephemeral_name(plan,storage,vmid,record), JournalVMFaultProxy.ephemeral_name(plan,storage,vmid)]
            file_storage = plan.get("Definitions",{}).get(storage,{}).get("Type","").lower() in {"dir","nfs","cifs","glusterfs","btrfs"}
            names = [name for name in names if target.get("intended_volume") == str(storage)+":"+(vmid+"/" if file_storage else "")+name]
            if len(names) != 1:
                return False
            return method == "POST" and path == "/nodes/"+node+"/storage/"+str(storage)+"/content" and str(fields.get("vmid")) == vmid and fields.get("filename") == names[0]
        if kind == "vm.Storage.Upload":
            return method == "POST" and path == "/nodes/"+node+"/storage/"+str(storage)+"/upload" and fields == {"content":"iso", "filename":"vm-"+vmid+"-config.iso"}
        vm_path = prefix + "/" + vmid
        if kind == "vm.delete.stop":
            return method == "POST" and path == vm_path+"/status/stop"
        if kind == "vm.delete.destroy":
            return method == "DELETE" and path == vm_path and set(fields) <= {"purge","destroy-unreferenced-disks"} and fields.get("destroy-unreferenced-disks") in (False,0,"0")
        if kind == "vm.delete.volume":
            volume = target.get("intended_volume", "")
            return method == "DELETE" and bool(volume) and path == "/nodes/"+node+"/storage/"+str(storage)+"/content/"+volume
        config_kinds = {"vm.Nodes.UpdateQemuConfig", "vm.QEMU.AttachDisk", "vm.QEMU.DetachDisk"}
        if kind in config_kinds:
            return method == "PUT" and path == vm_path+"/config" and JournalVMFaultProxy.drives_within_plan(record, fields)
        routes = {
            "vm.QEMU.Start": ("POST", "/status/start"),
            "vm.QEMU.ResizeDisk": ("PUT", "/resize"),
            "vm.Nodes.UpdateQemuResize": ("PUT", "/resize"),
            "vm.Nodes.CreateQemuAgentExec": ("POST", "/agent/exec"),
            "vm.Nodes.CreateQemuFirewallIpset": ("POST", "/firewall/ipset"),
            "vm.Nodes.CreateQemuFirewallRules": ("POST", "/firewall/rules"),
            "vm.Nodes.UpdateQemuFirewallOptions": ("PUT", "/firewall/options"),
        }
        if kind in routes:
            verb, suffix = routes[kind]
            return method == verb and path == vm_path+suffix
        if kind == "vm.Nodes.CreateQemuFirewallIpset2":
            return method == "POST" and re.fullmatch(re.escape(vm_path)+r"/firewall/ipset/[^/]+", path) is not None
        if kind == "vm.Cluster.CreateHaResources":
            return method == "POST" and path == "/cluster/ha/resources" and fields.get("sid") == "vm:"+vmid
        if kind == "vm.Cluster.UpdateHaResources":
            return method == "PUT" and path == "/cluster/ha/resources/vm:"+vmid
        parameters = step.get("parameters") or {}
        if kind == "vm.Cluster.CreateHaRules":
            expected = {key:value for key,value in parameters.items() if key not in {"version","kind"}}
            return method == "POST" and path == "/cluster/ha/rules" and expected and fields == expected
        if kind == "vm.Cluster.CreateSdnVnetsSubnets":
            return method == "POST" and path == "/cluster/sdn/vnets/"+str(parameters.get("vnet"))+"/subnets" and fields.get("subnet") == parameters.get("cidr")
        # Shared SDN apply, pools, and cleanup require their own exact scope proof.
        return False

    @staticmethod
    def ephemeral_format(plan,storage):
        kind=plan.get("Definitions",{}).get(storage,{}).get("Type","").lower()
        if kind in {"lvm","lvmthin","zfspool","rbd"}:
            return "raw"
        if kind in {"dir","nfs","cifs","glusterfs","btrfs"}:
            return plan["VMExecution"]["DiskFormat"]
        raise ValueError("ephemeral format lacks supported frozen backend")

    @staticmethod
    def ephemeral_name(plan, storage, vmid, record=None):
        kind = plan.get("Definitions",{}).get(storage,{}).get("Type","").lower()
        name = "vm-"+vmid+"-ephemeral-0"
        if record is not None:
            import uuid
            namespace, allocation = record.get("namespace"), record.get("id")
            if not isinstance(namespace, str) or not namespace.strip() or not isinstance(allocation, str):
                raise ValueError("ephemeral filename lacks exact allocation identity")
            parsed = uuid.UUID(allocation)
            if parsed.version != 4 or str(parsed) != allocation:
                raise ValueError("ephemeral allocation UUID is not canonical")
            locator = hashlib.sha256(namespace.encode()).hexdigest()[:16]
            name = "vm-"+vmid+"-bosh-"+locator+"-ephemeral-"+allocation

        if kind in {"dir","nfs","cifs","glusterfs","btrfs"}:
            return name+"."+plan["VMExecution"]["DiskFormat"]
        if kind in {"lvm","lvmthin","zfspool","rbd"}:
            return name
        raise ValueError("ephemeral filename lacks supported frozen backend")

    @staticmethod
    def drives_within_plan(record, fields, creating=False):
        plan = (record.get("attempts") or [{"plan":record["intent"]}])[-1]["plan"]["plan"]
        storage = {item.get("StorageID") for item in plan.get("Targets", [])}
        source_volumes = {item.get("Source", {}).get("VolumeID") for item in plan.get("Targets", [])}
        owned = {volume for step in record.get("steps", []) if step.get("state") == "observed" for volume in step.get("volids") or []}
        for key, value in fields.items():
            if not re.fullmatch(r"(?:scsi|virtio|sata|ide|efidisk|tpmstate)\d+", key):
                continue
            drive = str(value).split(",", 1)[0]
            if drive not in {"none", "cdrom"} and (":" not in drive or drive.split(":",1)[0] not in storage):
                return False
            if drive not in {"none", "cdrom"} and drive not in owned and not (creating and re.fullmatch(r"[0-9]+(?:\.[0-9]+)?", drive.split(":",1)[1])):
                return False
            for part in str(value).split(",")[1:]:
                if part.startswith("import-from=") and part.split("=",1)[1] not in source_volumes:
                    return False
        return True

    def forward(self, handler):
        if not hmac.compare_digest(handler.headers.get("Authorization", ""), "PVEAPIToken="+self.capability):
            handler.send_error(403)
            return
        url = urlsplit(handler.path)
        if url.scheme or url.netloc or not url.path.startswith("/api2/json/"):
            handler.send_error(400)
            return
        path = unquote(url.path[len("/api2/json"):])
        mutation = handler.command != "GET"
        try:
            length = int(handler.headers.get("Content-Length", "0"))
            if length < 0 or length > 32 * 1024 * 1024:
                raise ValueError("mutation body exceeds bound")
            body = handler.rfile.read(length)
            record = step = None
            if mutation:
                with self.lock:
                    if self.poisoned:
                        raise ValueError("faulted mutation cannot be reissued")
                    fields = request_fields(handler.headers.get("Content-Type", ""), body)
                    query = request_fields("application/x-www-form-urlencoded", url.query.encode())
                    if set(fields) & set(query):
                        raise ValueError("ambiguous mutation query fields")
                    fields.update(query)
                    record, step = self.planned_mutation(handler.command, path, fields)
                    if any(row["step_id"] == step["id"] for row in self.evidence["mutations"]):
                        raise ValueError("planned mutation already forwarded")
                    self.evidence["mutations"].append({"allocation_id":record["id"],"step_id":step["id"],"kind":step["kind"],"node":step["target"]["node"],"vmid":step["target"]["vmid"]})
            connection = http.client.HTTPSConnection(self.config["host"], int(self.config.get("port",8006)), timeout=120, context=self.upstream_context())
            try:
                headers = {key:value for key,value in handler.headers.items() if key.lower() not in {"host","authorization","cookie","csrfpreventiontoken","connection","transfer-encoding","accept-encoding"}}
                headers.update(self.upstream_auth(connection))
                connection.request(handler.command, handler.path, body, headers)
                response = connection.getresponse()
                payload = response.read()
                if mutation and self.vm_mode != "observe" and step["kind"] in PHASE_KINDS[self.phase] and 200 <= response.status < 300:
                    with self.lock:
                        self.poisoned = True
                        self.evidence["response_withheld"] = True
                        self.evidence["fault_step"] = dict(self.evidence["mutations"][-1])
                        self.evidence["fault_step"]["response_status"] = response.status
                        self.evidence["fault_step"]["target"] = dict(step["target"])
                        allowed = {"vmid","newid","storage","format","full","target","filename","size","content","sid","rule","state"}
                        self.evidence["fault_step"]["request_identity"] = {key:value for key,value in fields.items() if key in allowed}
                        data = json.loads(payload).get("data")
                        if isinstance(data,str) and data.startswith("UPID:"):
                            identity_keys = {"vmid","description"}
                            if step["kind"] == "vm.Nodes.CreateQemuClone":
                                identity_keys = {"newid","description","target","storage","full","format"}
                            elif step["kind"] == "vm.Storage.Upload":
                                identity_keys = {"content","filename"}
                            self.evidence["recovered_task_evidence"] = {
                                "version":1, "namespace":record["namespace"], "allocation_id":record["id"],
                                "step":json.loads(json.dumps(step)), "method":handler.command, "path":path,
                                "request_identity":{key:value for key,value in fields.items() if key in identity_keys},
                                "response_status":response.status, "upid":data,
                            }

                        self.evidence["fault_step"]["upid"] = data if isinstance(data,str) and data.startswith("UPID:") else ""
                        volume = data.get("volid") if isinstance(data,dict) else data
                        self.evidence["fault_step"]["response_volume"] = volume if isinstance(volume,str) and volume.startswith(str(step["target"].get("storage"))+":") else ""
                    if "recovered_task_evidence" in self.evidence:
                        try:
                            self.retain_response(self.evidence["recovered_task_evidence"])
                        except (OSError, ValueError):
                            # Retention failure must not itself manufacture the
                            # unknown outcome whose only recovery receipt was lost.
                            self.evidence["injection_aborted"] = "response_evidence_retention_failed"
                            self.evidence["response_withheld"] = False
                            self.poisoned = False
                            self.vm_mode = "observe"
                            handler.send_response(response.status)
                            handler.send_header("Content-Type","application/json")
                            handler.send_header("Content-Length",str(len(payload)))
                            handler.end_headers()
                            handler.wfile.write(payload)
                            return
                    self.first_success.set()
                    if self.vm_mode == "crash":
                        self.release.wait(120)
                    handler.close_connection = True
                    try:
                        handler.connection.shutdown(socket.SHUT_RDWR)
                    except OSError:
                        pass
                    return
                if not mutation and self.response_hook is not None:
                    payload = self.response_hook(handler.command, path, payload, self)
                    if not isinstance(payload, bytes):
                        raise ValueError("response hook must return bytes")
                handler.send_response(response.status)
                handler.send_header("Content-Type","application/json")
                handler.send_header("Content-Length",str(len(payload)))
                handler.end_headers()
                handler.wfile.write(payload)
            finally:
                connection.close()
        except (ValueError, KeyError, TypeError, OSError, RuntimeError, http.client.HTTPException):
            if mutation:
                with self.lock:
                    self.poisoned = True
                    self.evidence["blocked_mutations"] += 1
            handler.close_connection = True
            handler.send_error(503)


def supervise_fault_process(runner, process, evidence):
    """Keep the relay alive until a non-injected CPI request has actually ended."""
    key = "supervised-fault-cpi-"+str(process.pid)
    state = {"pid":process.pid,"agent_id":evidence.get("agent_id", ""),"method":evidence.get("method","create_vm"),"state":"waiting_for_cpi_completion","automatic_kill":False}
    evidence["supervised_process"] = state
    runner.active_resources[key] = state
    def checkpoint():
        try:
            runner.checkpoint()
        except Exception:
            evidence["supervision_checkpoint_failed"] = True
    checkpoint()
    if process.stdin is not None:
        try:
            process.stdin.close()
        except OSError:
            pass
        process.stdin = None
    while process.poll() is None:
        try:
            process.communicate(timeout=30)
        except subprocess.TimeoutExpired:
            checkpoint()
        except OSError:
            try:
                process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                checkpoint()
    state["state"] = "completed"
    state["returncode"] = process.returncode
    runner.active_resources.pop(key,None)
    checkpoint()


def run_vm_fault(runner, phase, mode, evidence):
    from _storage_placement_scenarios import CPIRejected
    config = runner.configured_policy()
    args = runner.vm_arguments("fault-"+phase+"-"+mode)
    evidence.update({"agent_id":args[0],"phase":phase,"mode":mode})
    before = runner.audit()
    before_volumes = runner.verification.volume_inventory(runner.verifier)
    receipt_directory = runner.report_path.resolve().with_name(runner.report_path.stem+"-"+phase+"-"+mode+"-response-evidence")
    receipt_directory.mkdir(mode=0o700)
    descriptor = os.open(receipt_directory.parent,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    receipt_path = receipt_directory/"original-response.json"
    with JournalVMFaultProxy(config,args[0],phase,mode,receipt_path=receipt_path) as proxy:
        config.update(proxy.config_override)
        path = runner.derived_config(config,"fault-"+phase+"-"+mode)
        if mode == "crash":
            request = {"method":"create_vm","arguments":args,"context":{},"api_version":2}
            process = subprocess.Popen([runner.cpi_bin,"--config",path],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
            requested = confirmed = False
            try:
                requested = True
                process.stdin.write(json.dumps(request)+"\n")
                process.stdin.flush()
                deadline = time.monotonic()+120
                while not proxy.first_success.wait(0.1):
                    if proxy.evidence.get("injection_aborted") or process.poll() is not None or time.monotonic() >= deadline:
                        supervise_fault_process(runner,process,evidence)
                        raise RuntimeError("VM fault barrier was not established; CPI completion was supervised without injection")
                confirmed = True
                process.kill()
                process.communicate(timeout=30)
            finally:
                if process.poll() is None:
                    if confirmed or not requested:
                        process.kill()
                        process.communicate(timeout=30)
                    else:
                        supervise_fault_process(runner,process,evidence)
                proxy.release.set()
        else:
            try:
                runner.call_once("create_vm",args,path)
            except CPIRejected:
                pass
            else:
                raise RuntimeError("lost VM mutation response returned success")
        evidence["proxy"] = proxy.evidence
        fault = proxy.evidence["fault_step"]
        if not fault or not proxy.evidence["response_withheld"] or proxy.evidence["blocked_mutations"]:
            raise RuntimeError("VM fault did not isolate an exact planned submission")
        record = proxy.current_record()
        steps = [step for step in record.get("steps",[]) if step.get("id") == fault["step_id"]]
        if len(steps) != 1 or steps[0].get("state") == "observed" or record.get("cid"):
            raise RuntimeError("unknown VM mutation was implicitly settled or returned")
        evidence["allocation_id"] = record["id"]
        evidence["vmid"] = fault["vmid"]
    upid = fault.get("upid")
    if upid:
        node = upid.split(":")[1]
        deadline = time.monotonic()+180
        while True:
            task = runner.verifier._get("/nodes/"+quote(node,safe="")+"/tasks/"+quote(upid,safe="")+"/status")
            if isinstance(task,dict) and task.get("status") == "stopped":
                if task.get("exitstatus") != "OK":
                    raise RuntimeError("forwarded VM task did not actually succeed")
                evidence["task"] = {"upid":upid,"status":"stopped","exitstatus":"OK","journal_settled":False}
                break
            if time.monotonic() >= deadline:
                raise RuntimeError("forwarded VM task remains unresolved")
            time.sleep(1)
    after = vm_fault_audit(runner,record,fault)
    old = {row["ID"]:row for row in before["records"]}
    new = {row["ID"]:row for row in after["records"]}
    if any(new.get(key) != value for key,value in old.items()) or set(new)-set(old) != {record["id"]}:
        raise RuntimeError("VM fault changed unrelated journal identities")
    after_volumes = runner.verification.volume_inventory(runner.verifier)
    if not set(map(tuple,before_volumes)) <= set(map(tuple,after_volumes)):
        raise RuntimeError("VM fault changed existing physical volumes")
    added = {row[2] for row in after_volumes if tuple(row) not in set(map(tuple,before_volumes))}
    artifact_proof = pending_vm_artifacts(runner,record,fault,added)
    evidence.update({"retained_volumes":sorted(added),"artifact_observations":artifact_proof,
                     "ownership_established":False,"audit_conflicts":after["audit"].get("conflicts") or [],
                     "journal":new[record["id"]],"unknown_step_not_reissued":True,"resources_retained_for_reconciliation":True})
    return evidence


def vm_fault_audit(runner,record,fault):
    command=[runner.cpi_bin,"storage-journal","audit","--config",runner.config_path]
    result=subprocess.run(command,capture_output=True,text=True,check=False,timeout=180)
    if result.returncode not in (0,1):
        raise RuntimeError("post-fault audit command failed")
    from _storage_placement_scenarios import _unique_json_object
    try:
        output=json.loads(result.stdout,object_pairs_hook=_unique_json_object)
    except (ValueError,TypeError):
        raise RuntimeError("post-fault audit did not return a valid report") from None
    audit=output.get("audit",{})
    expected="recorded VM target for allocation "+record["id"]+" lacks matching ownership provenance"
    conflicts=set(audit.get("conflicts") or [])
    allowed={expected} if fault.get("kind") == "vm.Nodes.CreateQemuClone" else set()
    if conflicts-allowed:
        raise RuntimeError("post-fault audit has an unrelated ownership conflict")
    if (output.get("generation_index_healthy") is not True or output.get("cluster_continuity") is not True
            or audit.get("vm_scan_complete") is not True or audit.get("issues")
            or (audit.get("complete") is not True and conflicts != {expected})):
        raise RuntimeError("post-fault audit is incomplete or authority changed")
    # A clone response can be lost before its ownership marker is installed.
    # Preserve that exact conflict and Complete=false; this is observation only.
    runner.assert_authority()
    return output


def pending_vm_artifacts(runner,record,fault,added):
    """Observe effects of a captured request without granting ownership authority."""
    if not added:
        raise RuntimeError("VM fault has no actual new artifacts")
    target=fault["target"]
    node,vmid=target["node"],target["vmid"]
    config=runner.verifier.qemu_config(str(vmid),node)
    if not isinstance(config,dict):
        raise RuntimeError("submitted VM target cannot be observed")
    attached={str(value).split(",",1)[0] for key,value in config.items() if re.fullmatch(r"(?:scsi|virtio|sata|ide|efidisk|tpmstate|unused)\d+",key) and ":" in str(value)}
    intended=target.get("intended_volume","")
    receipt=fault.get("response_volume","")
    if fault["kind"] == "vm.Storage.CreateVolume" and (not receipt or receipt != intended or receipt not in added):
        raise RuntimeError("pending ephemeral receipt differs from its exact new intended volume")
    if fault["kind"] == "vm.Storage.Upload" and (not intended or intended not in added):
        raise RuntimeError("pending ISO task lacks its exact new intended volume")
    permitted=attached|{value for value in (intended,receipt) if value}
    if not added <= permitted:
        raise RuntimeError("new artifact is unrelated to the submitted VM or exact volume request")
    plan=(record.get("attempts") or [{"plan":record["intent"]}])[-1]["plan"]["plan"]
    targets=plan.get("Targets",[])
    stores={item["StorageID"] for item in targets}
    observations=[]
    for volume in sorted(added):
        storage,suffix=volume.split(":",1)
        if storage not in stores:
            raise RuntimeError("pending artifact escaped its frozen target storage")
        info=runner.verifier._get("/nodes/"+quote(node,safe="")+"/storage/"+quote(storage,safe="")+"/content/"+quote(suffix,safe=""))
        if not isinstance(info,dict) or type(info.get("size")) is not int or info["size"] <= 0:
            raise RuntimeError("pending artifact lacks an actual virtual size")
        if volume == intended and fault["kind"] in {"vm.Storage.CreateVolume","vm.Storage.Upload"}:
            role="ephemeral" if fault["kind"] == "vm.Storage.CreateVolume" else "iso"
            expected=[item for item in targets if item["Role"] == role and item["StorageID"] == storage]
            if len(expected) != 1 or info["size"] != expected[0]["VirtualBytes"]:
                raise RuntimeError("pending allocation actual size differs from its frozen role")
        observations.append({"volume_id":volume,"node":node,"size_bytes":info["size"],"evidence_kind":"captured submission and direct readback; ownership remains unverified"})
    if fault["kind"] in PHASE_KINDS["ha"]:
        identity=fault.get("request_identity",{})
        if fault["kind"] == "vm.Cluster.CreateHaRules":
            rule=runner.verifier._get("/cluster/ha/rules/"+quote(identity["rule"],safe=""))
            if not isinstance(rule,dict) or rule.get("rule") != identity["rule"]:
                raise RuntimeError("submitted HA rule cannot be observed")
        else:
            resource=runner.verifier._get("/cluster/ha/resources/"+quote("vm:"+str(vmid),safe=""))
            if not isinstance(resource,dict) or resource.get("sid") != "vm:"+str(vmid):
                raise RuntimeError("submitted HA resource cannot be observed")
    return observations
