"""Fixed, artifact-pinned Director updates with retained independent audit tooling."""
from __future__ import annotations

import copy
import hashlib
import json
import os
import re
import shlex
import stat
import subprocess
import tarfile
from pathlib import Path

ROLLOUT_IDS = ("director_global_policy_removal", "director_binary_downgrade", "director_create_env_upgrade")
SET_KEYS = {"root_storage_set", "ephemeral_storage_set", "persistent_storage_set", "storage_set"}
TERMINAL_OR_READY = {"ready_to_return", "adopted", "deleted", "cleaned", "vm_deleted_retained"}
RETAIN_AUDITOR = r'''
import hashlib,json,os,pathlib,pwd,stat,sys
r=json.load(sys.stdin)
if len(r['run_id'])!=32 or any(c not in '0123456789abcdef' for c in r['run_id']): raise RuntimeError('run identity')
runtime=pwd.getpwnam('vcap')
base=pathlib.Path('/var/vcap/store/pve_cpi')
if base.parent.resolve()!=base.parent: raise RuntimeError('redirected persistent directory')
st=base.lstat()
if not stat.S_ISDIR(st.st_mode) or st.st_uid not in (0,runtime.pw_uid) or st.st_mode&0o022: raise RuntimeError('unsafe persistent directory')
retained=[]
for name,source,mode in [('candidate-cpi','/var/vcap/packages/pve_cpi/bin/cpi',0o700),('candidate-config.json','/var/vcap/jobs/pve_cpi/config/cpi.json',0o600)]:
    fd=os.open(source,os.O_RDONLY|os.O_NOFOLLOW)
    with os.fdopen(fd,'rb') as stream:
        st=os.fstat(stream.fileno())
        if not stat.S_ISREG(st.st_mode) or st.st_uid not in (0,runtime.pw_uid) or st.st_mode&0o022: raise RuntimeError('unsafe audit source')
        limit=256*1024*1024 if name=='candidate-cpi' else 16*1024*1024
        raw=stream.read(limit+1)
        if len(raw)>limit: raise RuntimeError('audit source bound')
    if name=='candidate-cpi' and hashlib.sha256(raw).hexdigest()!=r['sha256']: raise RuntimeError('candidate binary differs')
    if name=='candidate-config.json':
        cfg=json.loads(raw)
        journal=pathlib.Path(cfg['storage_allocation_journal_dir'])
        st=journal.lstat()
        if not stat.S_ISDIR(st.st_mode) or st.st_uid!=runtime.pw_uid or st.st_gid!=runtime.pw_gid or stat.S_IMODE(st.st_mode)!=0o700: raise RuntimeError('journal differs from provisioned CPI runtime identity')
    retained.append((name,raw,mode))
# Root reads deployed inputs, then creates only new artifacts as the CPI owner.
os.setgroups([])
os.setgid(runtime.pw_gid)
os.setuid(runtime.pw_uid)
directory=base/'storage-certification'
try: directory.mkdir(mode=0o700)
except FileExistsError: pass
st=directory.lstat()
if not stat.S_ISDIR(st.st_mode) or st.st_uid!=runtime.pw_uid or st.st_gid!=runtime.pw_gid or stat.S_IMODE(st.st_mode)!=0o700: raise RuntimeError('private runtime-owned directory required')
root=directory/r['run_id']
root.mkdir(mode=0o700)
for name,raw,mode in retained:
    fd=os.open(root/name,os.O_CREAT|os.O_EXCL|os.O_WRONLY,mode)
    with os.fdopen(fd,'wb') as stream: stream.write(raw);stream.flush();os.fsync(stream.fileno())
for directory in (root,root.parent):
    fd=os.open(directory,os.O_RDONLY|os.O_DIRECTORY)
    try: os.fsync(fd)
    finally: os.close(fd)
print(json.dumps({'binary':str(root/'candidate-cpi'),'config':str(root/'candidate-config.json'),'binary_sha256':r['sha256']}))
'''


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def private_file(value, label):
    require(isinstance(value, str) and Path(value).is_absolute(), label + " must be an absolute file")
    path = Path(value)
    require(path.resolve(strict=True) == path, label + " must not use symlinks or path aliases")
    info = path.stat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and not info.st_mode & 0o077, label + " must be an existing private owned regular file")
    return path


def validate_rollout_manifest(value, director_fixture):
    if value is None or value == {}:
        return {}
    keys = {"candidate_manifest", "scalar_manifest", "baseline_manifest", "state_file", "vars_store", "baseline_archive", "baseline_sha256", "baseline_linux_cpi_sha256"}
    require(isinstance(value, dict) and set(value) == keys, "rollout requires exactly the declared manifest/state/artifact fields")
    for key in ("candidate_manifest", "scalar_manifest", "baseline_manifest", "state_file", "vars_store"):
        require(isinstance(value[key], str) and Path(value[key]).is_absolute() and "\n" not in value[key], "rollout path must be absolute")
    require(len({value[key] for key in ("candidate_manifest", "scalar_manifest", "baseline_manifest", "state_file", "vars_store")}) == 5, "rollout inputs must use distinct paths")
    require(isinstance(value["baseline_archive"], str) and Path(value["baseline_archive"]).is_absolute(), "baseline archive must be explicit")
    for key in ("baseline_sha256", "baseline_linux_cpi_sha256"):
        require(isinstance(value[key], str) and re.fullmatch(r"[0-9a-f]{64}", value[key]), "baseline archive and binary checksums must be explicit")
    require(value["baseline_sha256"] != director_fixture["candidate_sha256"] and value["baseline_linux_cpi_sha256"] != director_fixture["linux_cpi_sha256"], "upgrade/downgrade requires two distinct artifacts and binaries")
    return copy.deepcopy(value)


def manifest_cpi(document):
    groups = document.get("instance_groups")
    require(isinstance(groups, list), "Director manifest lacks instance groups")
    jobs = [job for group in groups for job in group.get("jobs", []) if job.get("name") == "pve_cpi"]
    require(len(jobs) == 1 and isinstance(jobs[0].get("properties", {}).get("pve"), dict), "Director manifest lacks one explicit CPI job policy")
    provider = document.get("cloud_provider", {})
    require(provider.get("template", {}).get("name") == "pve_cpi" and isinstance(provider.get("properties", {}).get("pve"), dict), "bootstrap CPI policy must be explicit")
    return jobs[0], provider


def contains_selector(value):
    if isinstance(value, dict):
        if any(key in SET_KEYS and item not in (None, "") for key, item in value.items()):
            return True
        return any(contains_selector(item) for item in value.values())
    return isinstance(value, list) and any(contains_selector(item) for item in value)


def validate_manifest_artifact(document, artifact):
    job, provider = manifest_cpi(document)
    require(job["release"] == artifact["name"] and provider["template"]["release"] == artifact["name"], "manifest job/provider release differs from artifact")
    releases = [row for row in document.get("releases", []) if row.get("name") == artifact["name"]]
    require(len(releases) == 1, "manifest CPI release identity is ambiguous")
    release = releases[0]
    require(release.get("url") == Path(artifact["path"]).as_uri(), "manifest must reference the exact checksummed local CPI archive")
    require(str(release.get("version", artifact["version"])) == artifact["version"], "manifest release version differs from archive")
    return job, provider


def rollout_shape(document):
    """Only CPI release identity and placement bindings may vary by phase."""
    value = copy.deepcopy(document)
    job, provider = manifest_cpi(value)
    names = {job["release"], provider["template"]["release"]}
    value["releases"] = [release for release in value["releases"] if release.get("name") not in names]
    job["release"] = provider["template"]["release"] = "CPI_PHASE_ARTIFACT"
    for properties in (job["properties"]["pve"], provider["properties"]["pve"]):
        for key in SET_KEYS:
            properties.pop(key, None)
    def strip_resource_selectors(item):
        if isinstance(item, dict):
            if isinstance(item.get("cloud_properties"), dict):
                for key in SET_KEYS:
                    item["cloud_properties"].pop(key, None)
            for child in item.values():
                strip_resource_selectors(child)
        elif isinstance(item, list):
            for child in item:
                strip_resource_selectors(child)
    strip_resource_selectors(value)
    return value


def require_scalar_vm_boundary(remote_records, local_records):
    """An old CPI cannot dispose of managed VM journal generations."""
    for records in (remote_records, local_records):
        require(isinstance(records, dict), "downgrade journal records are unavailable")
        for record in records.values():
            require(record.get("state") in TERMINAL_OR_READY, "downgrade requires settled allocation outcomes")
            if record.get("kind") == "vm":
                require(record.get("state") in {"deleted", "cleaned", "vm_deleted_retained"},
                        "downgrade requires candidate-disposed managed VM generations")


def state_identity(path):
    value = json.loads(private_file(path, "create-env state").read_text())
    vm = str(value.get("current_vm_cid", ""))
    disks = value.get("disks")
    require(vm.isdecimal() and isinstance(disks, list) and disks, "create-env state lacks an existing Director VM and persistent disks")
    cids = [row.get("cid") for row in disks]
    require(all(isinstance(cid, str) and cid for cid in cids) and len(set(cids)) == len(cids), "bootstrap disk identities are missing or ambiguous")
    return {"vm_cid": vm, "disk_cids": sorted(cids)}


def preserve_history(previous, current):
    require(set(previous) <= set(current), "rollout lost historical journal records")
    for identity, before in previous.items():
        after = current[identity]
        for field in ("id", "namespace", "kind", "agent_id", "intent"):
            require(before.get(field) == after.get(field), "rollout changed immutable allocation identity or intent")
        for field in ("steps", "verifications"):
            old, new = before.get(field) or [], after.get(field) or []
            require(new[:len(old)] == old, "rollout erased or rewrote prior journal evidence")
        attempts = before.get("attempts") or []
        current_attempts = after.get("attempts") or []
        require(len(current_attempts) >= len(attempts), "rollout dropped attempt history")
        for old, new in zip(attempts, current_attempts):
            require(old.get("plan") == new.get("plan"), "rollout rewrote a frozen attempt plan")
            if old.get("completion") is not None:
                require(old["completion"] == new.get("completion"), "rollout rewrote prior attempt disposition")


class RolloutScenarios:
    def __init__(self, director, value):
        from _storage_placement_director import archive_fingerprints
        self.director, self.runner, self.value = director, director.runner, value
        self.candidate = director.observer.artifact
        self.baseline = archive_fingerprints(value["baseline_archive"], value["baseline_sha256"])
        require(self.baseline["version"] != self.candidate["version"], "release pair must have distinct embedded versions")
        self.documents = {}
        for key, artifact in (("candidate_manifest", self.candidate), ("scalar_manifest", self.candidate), ("baseline_manifest", self.baseline)):
            document = json.loads(private_file(value[key], key).read_text())
            require(isinstance(document, dict) and "((" not in json.dumps(document), "rollout requires a fully interpolated JSON manifest")
            job, provider = validate_manifest_artifact(document, artifact)
            remote, local = job["properties"]["pve"], provider["properties"]["pve"]
            require(remote.get("storage_placement_namespace") == director.fixture["namespace"], "Director manifest changes its journal namespace")
            for field in ("storage_placement_namespace", "storage_allocation_journal_dir"):
                require(local.get(field) == self.runner.base_config[field], "bootstrap manifest changes its independent journal authority")
            require(local["storage_placement_namespace"] != remote["storage_placement_namespace"], "bootstrap and Director authorities overlap")
            if key != "candidate_manifest":
                require(isinstance(remote.get("vm_storage"), str) and remote["vm_storage"] and isinstance(remote.get("iso_storage"), str) and remote["iso_storage"] not in ("", "local") and remote.get("iso_storage_follow_vm_storage") is False, "scalar rollout requires explicit VM and fixed ISO pools")
                require(not any(properties.get(key) for properties in (remote, local) for key in SET_KEYS), "scalar/downgrade manifest retains global set bindings")
                require(not contains_selector(document.get("resource_pools", [])) and not contains_selector(document.get("disk_pools", [])), "scalar bootstrap manifest retains resource selectors")
            self.documents[key] = document
        require(len({document.get("name") for document in self.documents.values()}) == 1 and self.documents["candidate_manifest"].get("name"), "rollout manifests name different Directors")
        shapes = [rollout_shape(document) for document in self.documents.values()]
        require(all(shape == shapes[0] for shape in shapes), "rollout manifests change fields outside CPI release and placement bindings")
        private_file(value["vars_store"], "create-env variables store")
        self.bootstrap = state_identity(value["state_file"])
        self.retained = None
        self.remote_before = None
        self.local_before = None
        self.disk_before = None
        self.remote_history = None
        self.local_history = None

    def retain_auditor(self):
        ssh = self.director.fixture["ssh"]
        command = ["ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10"]
        if ssh.get("identity_file"):
            command += ["-o", "IdentitiesOnly=yes", "-i", ssh["identity_file"]]
        if ssh.get("known_hosts_file"):
            command += ["-o", "UserKnownHostsFile=" + json.dumps(ssh["known_hosts_file"])]
        command += [ssh["user"] + "@" + ssh["host"], "sudo -n python3 -c " + shlex.quote(RETAIN_AUDITOR)]
        result = subprocess.run(command, input=json.dumps({"run_id": self.director.run_id, "sha256": self.director.fixture["linux_cpi_sha256"]}), capture_output=True, text=True, timeout=180, check=False)
        require(result.returncode == 0, "private candidate audit tool retention failed")
        retained = json.loads(result.stdout)
        prefix = "/var/vcap/store/pve_cpi/storage-certification/" + self.director.run_id
        require(retained == {"binary": prefix + "/candidate-cpi", "config": prefix + "/candidate-config.json", "binary_sha256": self.director.fixture["linux_cpi_sha256"]}, "retained audit identity differs from the declared candidate")
        self.retained = retained
        self.runner.report["retained_director_auditor"] = {"binary": retained["binary"], "config": retained["config"], "sha256": retained["binary_sha256"]}
        self.runner.checkpoint()

    def snapshot(self, baseline=False):
        observed = self.director.observer.snapshot(expected_artifact=self.baseline if baseline else self.candidate,
            expected_linux_sha=self.value["baseline_linux_cpi_sha256"] if baseline else self.director.fixture["linux_cpi_sha256"],
            audit_binary=self.retained["binary"] if baseline else None, audit_config=self.retained["config"] if baseline else None)
        api = self.director.bosh(["curl", "/info"], scoped=False, as_json=True)
        require(isinstance(api, dict) and observed.get("director_uuid") and api.get("uuid") == observed["director_uuid"], "BOSH endpoint differs from the independently observed Director")
        require(self.remote_before is not None and observed["director_uuid"] == self.remote_before["director_uuid"], "Director identity changed during rollout")
        preserve_history(self.remote_history, observed["records"])
        self.remote_history = copy.deepcopy(observed["records"])
        local = self.local_records()
        preserve_history(self.local_history, local)
        self.local_history = copy.deepcopy(local)
        return observed

    def local_records(self):
        from _storage_placement_scenarios import verified_record_payload
        audit = self.runner.audit()
        namespace = hashlib.sha256(self.runner.base_config["storage_placement_namespace"].encode()).hexdigest()
        root = Path(self.runner.base_config["storage_allocation_journal_dir"]) / namespace
        values = {}
        for summary in audit["records"]:
            require(re.fullmatch(r"[0-9a-f-]{36}", summary["ID"]) and summary["ID"] not in values, "bootstrap journal identity is ambiguous")
            path = private_file(str(root / ("allocation-" + summary["ID"] + ".json")), "bootstrap journal record")
            values[summary["ID"]] = verified_record_payload(path.read_bytes(), summary)
        return values

    def prove_registered_returnable_records(self):
        from _storage_placement_director import table_rows
        rows = table_rows(self.director.bosh(["vms", "--details"], as_json=True))
        vmids = {str(row.get("vm_cid", "")) for row in rows}
        require(vmids and all(value.isdecimal() for value in vmids), "Director cannot prove registered workflow VM CIDs")
        registered_disks = set(self.director.instances()["disk_cids"])
        for record in self.remote_before["records"].values():
            if record.get("state") not in {"ready_to_return", "adopted"}:
                continue
            cid = record.get("cid")
            require(cid in (vmids if record["kind"] == "vm" else registered_disks), "returnable allocation has no registered Director owner")
        for record in self.local_history.values():
            if record.get("state") in {"ready_to_return", "adopted"}:
                cids = {self.bootstrap["vm_cid"]} if record["kind"] == "vm" else set(self.bootstrap["disk_cids"])
                require(record.get("cid") in cids, "bootstrap returnable allocation has no matching create-env owner")

    def bootstrap_disks(self):
        state = state_identity(self.value["state_file"])
        require(state["disk_cids"] == self.bootstrap["disk_cids"], "create-env replaced the Director's persistent CIDs")
        records = self.runner.audit()["records"]
        proof = []
        for cid in state["disk_cids"]:
            volume = self.runner.verifier.current_disk_volid(cid)
            content = self.runner.verifier.volume_entry(cid)
            require(isinstance(content, dict) and type(content.get("size")) is int and content["size"] > 0, "bootstrap persistent volume is absent")
            definition = self.runner.verifier.storage_entry(volume.split(":", 1)[0])
            require(isinstance(definition, dict), "bootstrap persistent backing is absent")
            owned = [item for item in records if item.get("Kind") == "disk" and item.get("CID") == cid]
            require(len(owned) <= 1, "bootstrap disk has ambiguous journal ownership")
            proof.append({"cid": cid, "allocation_uuid": owned[0]["ID"] if owned else None, "size_bytes": content["size"],
                          "backing": {key: definition.get(key) for key in ("type", "server", "export", "path")}})
        return proof

    def preflight(self):
        self.remote_before = self.director.snapshot()
        require(self.remote_before.get("vm_cid") == self.bootstrap["vm_cid"], "create-env state belongs to a different VM than the observed Director")
        self.local_before = self.runner.audit()
        require(all(record.get("state") in TERMINAL_OR_READY for record in self.remote_before["records"].values()), "downgrade requires every remote allocation outcome to be settled")
        require(all(record.get("State") in TERMINAL_OR_READY for record in self.local_before["records"]), "bootstrap update requires settled local allocations")
        for document in self.documents.values():
            job, _ = manifest_cpi(document)
            require(job["properties"]["pve"].get("storage_allocation_journal_dir") == self.remote_before["journal_dir"], "rollout manifest changes remote journal directory")
        self.remote_history = copy.deepcopy(self.remote_before["records"])
        self.local_history = self.local_records()
        self.prove_registered_returnable_records()
        self.disk_before = self.bootstrap_disks()
        self.retain_auditor()
        self.runner.active_resources["director_rollout"] = {"phase": "prepared", "state_file": self.value["state_file"]}
        self.runner.checkpoint()

    def create_env(self, key, baseline=False):
        # Archive and manifest identities are rechecked immediately before use.
        from _storage_placement_director import archive_fingerprints
        expected = self.baseline if baseline else self.candidate
        require(archive_fingerprints(expected["path"], expected["sha256"]) == expected, "release artifact changed before create-env")
        from _storage_placement_rollout_trust import preflight_rollout_trust
        preflight_rollout_trust(self.director, self.remote_before["director_uuid"], key)
        path = self.director.write_json("rollout-" + key + ".json", self.documents[key])
        self.runner.active_resources["director_rollout"] = {"phase": key, "state_file": self.value["state_file"], "release_sha256": expected["sha256"]}
        self.runner.checkpoint()
        command = ["bosh", "-n", "--tty", "create-env", path, "--state=" + self.value["state_file"], "--vars-store=" + self.value["vars_store"]]
        previous_bootstrap = None
        if key == "scalar_manifest":
            # The candidate must dispose of its managed bootstrap VM before an
            # older CPI can handle it. Recreate only the VM, retaining its disks.
            previous_bootstrap = state_identity(self.value["state_file"])["vm_cid"]
            command.append("--recreate")
        from _storage_placement_rollout_trust import begin_update, finish_update
        attempt = begin_update(self.director, self.value["state_file"], self.value["vars_store"], path, expected, key)
        try:
            result = subprocess.run(command, capture_output=True, text=True, timeout=7200, check=False)
        except (OSError, subprocess.SubprocessError) as error:
            finish_update(attempt, self.value["state_file"], error=error)
            raise
        finish_update(attempt, self.value["state_file"], result=result)
        require(result.returncode == 0, "fixed create-env update failed; retained state and audit tooling require inspection")
        from _storage_placement_rollout_trust import refresh_rollout_trust
        refresh_rollout_trust(self.director, self.value["state_file"], self.remote_before["director_uuid"], key)
        observed = self.snapshot(baseline)
        require(observed.get("vm_cid") == state_identity(self.value["state_file"])["vm_cid"], "updated Director differs from the create-env state VM")
        if previous_bootstrap is not None:
            require(observed["vm_cid"] != previous_bootstrap and not self.runner.verifier.vm_exists(previous_bootstrap),
                    "scalar transition did not replace the managed bootstrap VM")
        require(self.bootstrap_disks() == self.disk_before, "create-env changed persistent bootstrap UUID, backing or size")
        require(set(self.remote_before["records"]) <= set(observed["records"]), "Director update lost historical allocation records")
        return observed

    def service_disks(self, snapshot):
        previous = self.director.service
        state = self.director.instances()
        require(state["disk_cids"] == previous["disk_cids"], "rollout replaced workload persistent CIDs")
        disks = [self.director.disk_evidence(snapshot, cid, next(disk for disk in previous["disks"] if disk["cid"] == cid)) for cid in state["disk_cids"]]
        config = self.runner.verifier.qemu_config(state["vm_cid"])
        for disk in disks:
            fields = [str(value).split(",") for key, value in config.items() if re.fullmatch(r"(?:scsi|virtio|sata|ide)\d+", key) and str(value).split(",", 1)[0] == disk["volume_id"]]
            require(len(fields) == 1 and "serial=" + disk["stable_token"] in fields[0], "rollout lost actual attachment or stable disk provenance")
        policy = snapshot.get("deployed_policy", snapshot["policy"])
        vm_types = [entry for entry in self.director.cloud["vm_types"] if entry["name"] == self.director.fixture["workload_vm_type"]]
        require(len(vm_types) == 1, "scalar VM type is ambiguous")
        properties = vm_types[0].get("cloud_properties", {})
        root_storage = properties.get("storage_pool") or policy.get("vm_storage")
        ephemeral_storage = properties.get("ephemeral_storage_pool") or root_storage
        root_slot = "scsi0" if policy.get("root_disk_bus", "").lower() == "scsi" else "virtio0"
        persistent = {disk["volume_id"] for disk in disks}
        volumes = {slot: str(value).split(",", 1)[0] for slot, value in config.items()
                   if re.fullmatch(r"(?:scsi|virtio|sata|ide|unused)\d+", slot) and ":" in str(value) and "media=cdrom" not in str(value) and str(value).split(",", 1)[0] not in persistent}
        dedicated = "scsi1" in previous["vm"]["devices"]
        expected = {root_slot, "scsi1"} if dedicated else {root_slot}
        expected |= set(previous["root_execution"]["auxiliary_devices"])
        require(set(volumes) == expected and len(set(volumes.values())) == len(volumes), "scalar VM has extra, missing or duplicate role volumes")
        require(volumes[root_slot].split(":", 1)[0] == root_storage, "scalar root ignored its actual configured target")
        if dedicated:
            require(volumes["scsi1"].split(":", 1)[0] == ephemeral_storage, "scalar ephemeral disk ignored its configured target")
        isos = [str(value).split(",", 1)[0] for value in config.values() if "media=cdrom" in str(value) and ":" in str(value)]
        require(len(isos) == 1 and isos[0].split(":", 1)[0] == policy.get("iso_storage"), "scalar VM has an unexpected configdrive target")
        for volume in list(volumes.values()) + isos:
            require(self.runner.observed_volume(volume)["size"] > 0, "scalar VM actual role volume is absent")
        from _storage_placement_scenarios import guest_disk_mapping
        from urllib.parse import quote
        node = self.runner.verifier._node_hosting(state["vm_cid"])
        mappings = guest_disk_mapping(self.runner.verifier._get("/nodes/" + quote(node, safe="") + "/qemu/" + state["vm_cid"] + "/agent/get-fsinfo"), dedicated)
        return {**state, "disks": disks, "allocation_uuid": None, "allocation_mode": "legacy_scalar", "root_execution": {"auxiliary_devices": previous["root_execution"]["auxiliary_devices"]}, "vm": {"devices": volumes, "iso": isos, "guest_mounts": mappings}}

    def old_workload_absent(self, previous, current):
        require(current["vm_cid"] != previous["vm_cid"] and not self.runner.verifier.vm_exists(previous["vm_cid"]), "rollout recreation left its previous VM")
        volumes = set(previous["vm"]["devices"].values())
        isos = previous["vm"]["iso"]
        volumes.update(isos.values() if isinstance(isos, dict) else isos)
        inventory = self.runner.verification.volume_inventory(self.runner.verifier)
        require(not any(row[2] in volumes for row in inventory), "rollout recreation left a previous root, ephemeral or ISO volume")

    def scalar_cloud(self):
        cloud = copy.deepcopy(self.director.cloud)
        def strip(value):
            if isinstance(value, dict):
                for key in SET_KEYS:
                    value.pop(key, None)
                for item in value.values():
                    strip(item)
            elif isinstance(value, list):
                for item in value:
                    strip(item)
        strip(cloud)
        require(not contains_selector(cloud), "resource selector removal was incomplete")
        path = self.director.write_json("rollout-scalar-cloud.json", cloud)
        self.director.bosh(["update-cloud-config", path], scoped=False)
        self.director.cloud = cloud

    def dispose_kept_errands(self):
        before = self.snapshot()
        records = self.director.records_with_tag(before, "errand")
        active = [record for record in records if record.get("state") in {"ready_to_return", "adopted"}]
        require(active, "scalar transition lacks its declared kept errand")
        # The core workflow deliberately kept this disposable errand for guest
        # inspection. Omit --keep-alive so the candidate completes its cleanup.
        operation = self.director.operation(["run-errand", self.director.fixture["errand_name"]])
        after = self.snapshot()
        current = self.director.records_with_tag(after, "errand")
        require({record["id"] for record in records} <= {record["id"] for record in current}, "errand cleanup lost journal history")
        volumes = set()
        for record in current:
            require(record.get("state") in {"deleted", "cleaned"}, "kept errand lacks candidate journal disposition")
            require(not self.runner.verifier.vm_exists(record["cid"]), "kept errand VM remains")
            for step in record.get("steps", []):
                volumes.update(step.get("volids") or [])
        inventory = self.runner.verification.volume_inventory(self.runner.verifier)
        require(not any(row[2] in volumes for row in inventory), "kept errand volume remains")
        return {"operation": operation, "allocation_ids": sorted(record["id"] for record in current), "all_errand_resources_absent": True}

    def global_removal(self):
        errand_cleanup = self.dispose_kept_errands()
        self.scalar_cloud()
        observed = self.create_env("scalar_manifest")
        require(not any(observed.get("deployed_policy", observed["policy"]).get(key) for key in SET_KEYS), "actual deployed configuration retains a global set binding")
        previous = copy.deepcopy(self.director.service)
        before = previous["vm_cid"]
        operation = self.director.operation(["recreate", self.director.service["instance"]])
        after = self.snapshot()
        service = self.service_disks(after)
        require(service["vm_cid"] != before, "global policy removal did not exercise a fresh scalar VM")
        require(not any(record.get("kind") == "vm" and record.get("cid") == service["vm_cid"] for record in after["records"].values()), "removed bindings unexpectedly created a managed VM allocation")
        self.old_workload_absent(previous, service)
        require_scalar_vm_boundary(after["records"], self.local_history)
        self.director.service = {**self.director.service, **service}
        return {"operation": operation, "service": service, "global_and_resource_selectors_removed": True, "kept_errand_cleanup": errand_cleanup,
                "candidate_sha256": self.candidate["sha256"], "journal_authority_sha256": after["authority_sha256"]}

    def downgrade(self):
        previous = copy.deepcopy(self.director.service)
        before = self.snapshot()
        require_scalar_vm_boundary(before["records"], self.local_history)
        observed = self.create_env("baseline_manifest", baseline=True)
        # Exercise real old-binary lifecycle through the Director while the
        # retained candidate independently audits full managed disk provenance.
        operation = self.director.operation(["recreate", self.director.service["instance"]])
        after = self.snapshot(baseline=True)
        service = self.service_disks(after)
        self.old_workload_absent(previous, service)
        self.director.service = {**self.director.service, **service}
        return {"operation": operation, "release_pair": {"candidate": self.candidate, "baseline": self.baseline}, "service": service,
                "retained_audit_binary": self.retained["binary"], "journal_authority_sha256": observed["authority_sha256"]}

    def upgrade(self):
        previous = copy.deepcopy(self.director.service)
        observed = self.create_env("candidate_manifest")
        operation = self.director.operation(["recreate", self.director.service["instance"]])
        after = self.snapshot()
        service = self.director.service_evidence(after, self.director.service)
        self.old_workload_absent(previous, service)
        self.director.service = service
        self.runner.active_resources.pop("director_rollout", None)
        self.runner.checkpoint()
        return {"operation": operation, "release_pair": {"baseline": self.baseline, "candidate": self.candidate}, "service": service,
                "bootstrap_disks": self.bootstrap_disks(), "journal_authority_sha256": observed["authority_sha256"]}


def run_rollout_scenarios(director, value):
    from _storage_placement_director import failure_evidence
    value = validate_rollout_manifest(value, director.fixture)
    if not value:
        try:
            cleanup = director.cleanup()
        except (RuntimeError, OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
            return [{"scenario_id": identity, "status": "failed" if index == 0 else "missing", "evidence": {**failure_evidence(error, director), "reason": "core Director workflow cleanup failed; resources retained"}} for index, identity in enumerate(ROLLOUT_IDS)]
        return [{"scenario_id": identity, "status": "missing", "evidence": {"reason": "explicit rollout manifests and baseline artifact are absent", "core_workflow_cleanup": cleanup}} for identity in ROLLOUT_IDS]
    rows = []
    try:
        scenarios = RolloutScenarios(director, value)
        scenarios.preflight()
        for identity, execute in zip(ROLLOUT_IDS, (scenarios.global_removal, scenarios.downgrade, scenarios.upgrade)):
            evidence = execute()
            if identity == ROLLOUT_IDS[-1]:
                evidence["cleanup"] = director.cleanup()
            rows.append({"scenario_id": identity, "status": "passed", "evidence": evidence})
    except (RuntimeError, OSError, ValueError, KeyError, TypeError, StopIteration, tarfile.TarError, subprocess.SubprocessError) as error:
        identity = ROLLOUT_IDS[len(rows)]
        rows.append({"scenario_id": identity, "status": "failed", "evidence": {**failure_evidence(error, director), "reason": "artifact-pinned rollout assertion failed; retained state requires inspection"}})
        rows.extend({"scenario_id": pending, "status": "missing", "evidence": {"reason": "prior rollout failed", "blocked_by": identity}} for pending in ROLLOUT_IDS[len(rows):])
    return rows
