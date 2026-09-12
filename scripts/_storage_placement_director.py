"""Fixed Director workflows with independent remote CPI journal observations."""
from __future__ import annotations

import base64
import copy
import hashlib
import json
import os
import traceback
import re
import shlex
import subprocess
import tarfile
import time
import tempfile
import threading
import uuid
from pathlib import Path
from typing import Any
from urllib.parse import quote, urlsplit

from _release_artifact import release_identity

DIRECTOR_IDS = (
    "director_compilation", "director_errand", "director_recreate", "director_resurrection",
    "director_resource_policy_removal", "director_global_policy_removal",
    "director_create_env_upgrade", "director_binary_downgrade",
)
REMOTE_CPI = "/var/vcap/packages/pve_cpi/bin/cpi"
REMOTE_CONFIG = "/var/vcap/jobs/pve_cpi/config/cpi.json"
REMOTE_SPEC = "/var/vcap/bosh/spec.json"
# This fixed program reads an existing authority. It never repairs/enrolls it,
# edits configuration, or accepts a shell command or an arbitrary file to read.
REMOTE_AUDIT = r'''
import base64,hashlib,json,pathlib,pwd,stat,subprocess,sys
request=json.loads(sys.stdin.read())
config_path=pathlib.Path(request.get("audit_config") or "/var/vcap/jobs/pve_cpi/config/cpi.json")
import re
if str(config_path) != "/var/vcap/jobs/pve_cpi/config/cpi.json" and not re.fullmatch(r"/var/vcap/store/pve_cpi/storage-certification/[0-9a-f]{32}/candidate-config.json", str(config_path)): raise RuntimeError("audit config identity")
binary=pathlib.Path("/var/vcap/packages/pve_cpi/bin/cpi")
audit_binary=request.get("audit_binary") or str(binary)
import re
if audit_binary != str(binary) and not re.fullmatch(r"/var/vcap/store/pve_cpi/storage-certification/[0-9a-f]{32}/candidate-cpi",audit_binary): raise RuntimeError("audit binary identity")
with pathlib.Path(audit_binary).open("rb") as stream: audit_bytes=stream.read(256*1024*1024+1)
if len(audit_bytes)>256*1024*1024 or hashlib.sha256(audit_bytes).hexdigest()!=request["audit_sha256"]: raise RuntimeError("candidate audit executable checksum")
config=json.loads(config_path.read_text())
namespace=config.get("storage_placement_namespace")
if namespace != request["namespace"]: raise RuntimeError("namespace mismatch")
runtime=pwd.getpwnam("vcap")
journal=pathlib.Path(config["storage_allocation_journal_dir"])
st=journal.lstat()
if not stat.S_ISDIR(st.st_mode) or st.st_uid!=runtime.pw_uid or st.st_gid!=runtime.pw_gid or stat.S_IMODE(st.st_mode)!=0o700: raise RuntimeError("journal differs from provisioned CPI runtime identity")
root=journal/hashlib.sha256(namespace.encode()).hexdigest()
result=subprocess.run([audit_binary,"storage-journal","audit","--config",str(config_path)],capture_output=True,text=True,timeout=180,user=runtime.pw_uid,group=runtime.pw_gid,extra_groups=[])
if result.returncode: raise RuntimeError("audit did not certify complete authority")
audit=json.loads(result.stdout)
records=audit.get("records") or []
if not isinstance(records,list) or len(records)>10000: raise RuntimeError("record bound")
envelopes={}
total_bytes=0
for row in records:
    identity=row["ID"]
    if identity in envelopes: raise RuntimeError("duplicate record identity")
    if len(identity)!=36 or any(c not in "0123456789abcdef-" for c in identity): raise RuntimeError("record identity")
    with (root/("allocation-"+identity+".json")).open("rb") as stream: raw=stream.read(16*1024*1024+1)
    if len(raw)>16*1024*1024: raise RuntimeError("record bound")
    total_bytes += len(raw)
    if total_bytes > 64*1024*1024: raise RuntimeError("aggregate record bound")
    envelopes[identity]=base64.b64encode(raw).decode()
spec=json.loads(pathlib.Path("/var/vcap/bosh/spec.json").read_text())
templates=spec.get("job",{}).get("templates",[])
job=[entry for entry in templates if entry.get("name")=="pve_cpi"]
if len(job)!=1: raise RuntimeError("CPI job identity is ambiguous")
package=spec.get("packages",{}).get("pve_cpi",{})
keys=("storage_sets","root_storage_set","ephemeral_storage_set","persistent_storage_set","storage_capacity_domains","iso_storage","iso_storage_follow_vm_storage","root_disk_bus","clone_mode","vm_storage","disk_storage")
def bounded_json(path):
    with pathlib.Path(path).open("rb") as stream: data=stream.read(8*1024*1024+1)
    if len(data)>8*1024*1024: raise RuntimeError("configuration size bound")
    return json.loads(data)
settings=bounded_json("/var/vcap/bosh/settings.json")
with pathlib.Path("/sys/class/dmi/id/product_uuid").open("rb") as stream: product_uuid=stream.read(128).decode("ascii").strip().lower()
if not re.fullmatch(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}",product_uuid) or product_uuid=="00000000-0000-0000-0000-000000000000": raise RuntimeError("guest SMBIOS identity")
director_config=bounded_json("/var/vcap/jobs/director/config/director.yml")
hm_config=bounded_json("/var/vcap/jobs/health_monitor/config/health_monitor.yml")
import urllib.request
port=director_config.get("port")
if type(port) is not int or not 1<=port<=65535: raise RuntimeError("Director backend port")
with urllib.request.build_opener(urllib.request.ProxyHandler({})).open("http://127.0.0.1:"+str(port)+"/info",timeout=10) as response:
    raw_info=response.read(1024*1024+1)
if len(raw_info)>1024*1024: raise RuntimeError("Director info bound")
director_uuid=json.loads(raw_info).get("uuid")
if not isinstance(director_uuid,str) or not re.fullmatch(r"[0-9a-f-]{36}",director_uuid): raise RuntimeError("Director UUID")
resurrectors=[item for item in hm_config.get("plugins",[]) if item.get("name")=="resurrector"]
deployed_config=json.loads(pathlib.Path("/var/vcap/jobs/pve_cpi/config/cpi.json").read_text())
authority=json.loads((root/"authority.json").read_text())["payload"]
print(json.dumps({"audit_runtime_uid":runtime.pw_uid,"audit_runtime_gid":runtime.pw_gid,"director_uuid":director_uuid,"auto_fix_stateful_nodes":director_config.get("scan_and_fix",{}).get("auto_fix_stateful_nodes") is True,"resurrector_enabled":len(resurrectors)==1,"product_uuid":product_uuid,"agent_id":settings.get("agent_id"),"cluster_id":authority["enrollment"]["cluster_id"],"audit":audit,"records":envelopes,"namespace":namespace,"journal_dir":str(root.parent),"authority_sha256":hashlib.sha256((root/"authority.json").read_bytes()).hexdigest(),"binary_sha256":hashlib.sha256(binary.read_bytes()).hexdigest(),"job_version":job[0].get("version"),"package_version":package.get("version"),"policy":{key:config[key] for key in keys if key in config},"deployed_policy":{key:deployed_config[key] for key in keys if key in deployed_config}}))
'''


def require(condition: Any, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


def _path(value: Any, label: str) -> str:
    require(isinstance(value, str) and Path(value).is_absolute() and "\n" not in value and "\r" not in value, label + " must be an absolute path")
    return value


DIRECTOR_POLICY_KEYS = {"vm_cloud_properties", "ephemeral_storage_ids", "root_storage_ids", "persistent_storage_ids",
                        "fixed_iso_storage_id", "ephemeral_size_mib", "disk_size_mib", "expected_root_mechanism",
                        "encrypted_persistent_storage_ids", "escaping_tier_criteria"}

def director_runner_view(runner: Any, fixture: dict[str, Any]) -> Any:
    policy = fixture.get("observer_policy")
    require(isinstance(policy, dict) and set(policy) == DIRECTOR_POLICY_KEYS, "Director observer policy must declare exactly its supported expectations")
    view = copy.copy(runner)
    view.policy = copy.deepcopy(runner.policy)
    view.policy.update(copy.deepcopy(policy))
    return view


def validate_director_manifest(manifest: dict[str, Any], base_config: dict[str, Any]) -> dict[str, Any]:
    value = manifest.get("director", {})
    keys = {"dedicated_director", "environment", "namespace", "ssh", "candidate_archive", "candidate_sha256", "linux_cpi_sha256", "compilation_release", "deployment_manifest", "cloud_config", "baseline_cloud_config", "compilation_vm_type", "workload_vm_type", "errand_vm_type", "errand_name", "resurrection_timeout_seconds", "rollout", "expected_root_mechanism", "observer_policy"}
    require(isinstance(value, dict) and not set(value)-keys, "director fixture contains unsupported fields")
    if not value:
        return value
    required = keys - {"rollout", "resurrection_timeout_seconds"}
    require(required <= set(value) and value["dedicated_director"] is True, "Director scenarios require an explicit dedicated Director fixture")
    for key in ("environment", "namespace", "compilation_vm_type", "workload_vm_type", "errand_vm_type", "errand_name"):
        require(isinstance(value[key], str) and value[key] and not value[key].startswith("-") and "\n" not in value[key], "Director identity is malformed")
    require(urlsplit(value["environment"]).username is None and urlsplit(value["environment"]).password is None, "Director endpoint must not embed credentials")
    require(value["namespace"] != base_config["storage_placement_namespace"], "Director and create-env authorities must use distinct namespaces")
    require(len({value[key] for key in ("compilation_vm_type", "workload_vm_type", "errand_vm_type")}) == 3, "workflow VM types must be distinct")
    for key in ("candidate_archive", "compilation_release", "deployment_manifest", "cloud_config", "baseline_cloud_config"):
        _path(value[key], key)
    require(all(isinstance(value[key], str) and re.fullmatch(r"[0-9a-f]{64}", value[key]) for key in ("candidate_sha256", "linux_cpi_sha256")), "candidate archive and Linux binary checksums must be explicit")
    require(isinstance(value["observer_policy"], dict) and set(value["observer_policy"]) == DIRECTOR_POLICY_KEYS, "Director observer policy must declare exactly its supported expectations")
    require(value["expected_root_mechanism"] in {"import", "full_clone", "linked_clone", "auto"}, "Director root mechanism expectation must be explicit")
    ssh = value["ssh"]
    require(isinstance(ssh, dict) and set(ssh) <= {"host", "user", "identity_file", "known_hosts_file"} and {"host", "user"} <= set(ssh), "SSH fixture is malformed")
    require(isinstance(ssh["host"], str) and isinstance(ssh["user"], str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.-]*", ssh["host"]) and re.fullmatch(r"[a-z_][a-z0-9_-]*", ssh["user"]), "SSH host/user is malformed")
    if "identity_file" in ssh:
        _path(ssh["identity_file"], "SSH identity")
    if "known_hosts_file" in ssh:
        _path(ssh["known_hosts_file"], "SSH known hosts")
    timeout = value.get("resurrection_timeout_seconds", 1200)
    require(type(timeout) is int and 60 <= timeout <= 3600, "resurrection deadline must be 60..3600 seconds")
    if "rollout" in value:
        from _storage_placement_rollout import validate_rollout_manifest
        validate_rollout_manifest(value["rollout"], value)
    return copy.deepcopy(value)


def director_policy_requirements(cloud: dict[str, Any], deployment: dict[str, Any], fixture: dict[str, Any], deployed: dict[str, Any]) -> dict[str, Any]:
    """Resolve this workflow's declared roles instead of borrowing standalone cases."""
    sets = deployed.get("storage_sets")
    require(isinstance(sets, dict), "Director storage sets are absent")
    def members(name):
        value = sets.get(name, {})
        names = value.get("names")
        require(isinstance(names, list) and names and all(isinstance(x, str) and x for x in names)
                and len(set(names)) == len(names), "Director workflow requires explicit unique set members")
        return set(names)
    vm_types = cloud.get("vm_types", [])
    expected_e, expected_root, ephemeral_sizes = set(), set(), set()
    for role in ("compilation", "workload", "errand"):
        matches = [x for x in vm_types if x.get("name") == fixture[role+"_vm_type"]]
        require(len(matches) == 1, "Director role VM type is missing or ambiguous")
        props = matches[0].get("cloud_properties", {})
        require(isinstance(props, dict) and not set(props) & {"vm_storage", "storage", "storage_selector", "storage_tier"}, "Director role has an unsupported competing storage selector")
        e = props.get("ephemeral_storage_set", deployed.get("ephemeral_storage_set"))
        root = props.get("root_storage_set", deployed.get("root_storage_set", e))
        expected_e |= members(e)
        if root != e:
            expected_root |= members(root)
        size = props.get("ephemeral_disk_size_mb")
        require(type(size) is int and size > 0, "Director role must declare a positive dedicated ephemeral size")
        ephemeral_sizes.add(size)
    require(len(ephemeral_sizes) == 1, "Director role ephemeral sizes differ from the shared observer expectation")
    groups = [x for x in deployment.get("instance_groups", []) if x.get("lifecycle", "service") == "service"]
    require(len(groups) == 1, "Director workflow must have one service group")
    disk_type = groups[0].get("persistent_disk_type")
    disks = [x for x in cloud.get("disk_types", []) if x.get("name") == disk_type]
    require(len(disks) == 1, "Director workflow requires a unique persistent disk type")
    disk = disks[0];props = disk.get("cloud_properties", {})
    require(isinstance(props, dict) and not set(props) & {"storage", "storage_selector", "storage_tier", "encrypted"}, "Director persistent type has unsupported selector or encryption assertions")
    persistent = members(props.get("storage_set", deployed.get("persistent_storage_set")))
    size = disk.get("disk_size");require(type(size) is int and size > 0, "Director persistent size is invalid")
    require(deployed.get("iso_storage_follow_vm_storage") is False and isinstance(deployed.get("iso_storage"), str), "Director workflow requires its explicit fixed ISO policy")
    mechanism = fixture.get("expected_root_mechanism")
    require(mechanism in {"import", "full_clone", "linked_clone", "auto"}, "Director root mechanism expectation is absent")
    clone_mode = deployed.get("clone_mode", "auto")
    require(mechanism != "auto" or clone_mode == "auto", "Director automatic clone expectation requires automatic clone policy")
    require(clone_mode != "full" or mechanism != "linked_clone", "Director full-clone policy contradicts its fixture")
    require(clone_mode != "linked" or mechanism == "linked_clone", "Director linked-clone policy contradicts its fixture")
    compiler = next(x["cloud_properties"] for x in vm_types if x.get("name") == fixture["compilation_vm_type"])
    diagnostic_props = {k: v for k, v in compiler.items() if k not in {"ephemeral_disk_size_mb", "ephemeral_storage_set", "root_storage_set", "tags", "retain_ephemeral_on_delete"}}
    return {"vm_cloud_properties": diagnostic_props, "ephemeral_storage_ids": sorted(expected_e), "root_storage_ids": sorted(expected_root),
            "persistent_storage_ids": sorted(persistent), "fixed_iso_storage_id": deployed["iso_storage"],
            "ephemeral_size_mib": next(iter(ephemeral_sizes)), "disk_size_mib": size,
            "expected_root_mechanism": mechanism, "encrypted_persistent_storage_ids": [], "escaping_tier_criteria": {}}


def validate_director_policy(policy: dict[str, Any], cloud: dict[str, Any], deployment: dict[str, Any], fixture: dict[str, Any], deployed: dict[str, Any]) -> dict[str, Any]:
    expected = director_policy_requirements(cloud, deployment, fixture, deployed)
    for key, value in expected.items():
        actual = policy.get(key)
        if isinstance(value, list):
            require(isinstance(actual, list) and len(actual) == len(set(actual)) and set(actual) == set(value), "Director observer fixture differs for " + key)
        else:
            require(actual == value, "Director observer fixture differs for " + key)
    return expected


def archive_fingerprints(path: str, expected: str) -> dict[str, str]:
    identity = release_identity(Path(path), expected)
    with tarfile.open(path, "r:gz") as archive:
        entries = [entry for entry in archive if entry.name in {"release.MF", "./release.MF"}]
        require(len(entries) == 1 and entries[0].isfile(), "candidate release manifest is ambiguous")
        stream = archive.extractfile(entries[0])
        require(stream is not None, "candidate release manifest is unreadable")
        with stream:
            manifest = stream.read(1024*1024+1).decode()
    require(len(manifest.encode()) <= 1024*1024, "candidate manifest exceeds bound")
    section, current = "", ""
    found: dict[str, list[str]] = {"jobs": [], "packages": []}
    for line in manifest.splitlines():
        heading = re.fullmatch(r"([A-Za-z_]+):(?:.*)", line)
        if heading:
            section, current = heading.group(1), ""
        item = re.fullmatch(r"- name: ['\"]?([A-Za-z0-9_.-]+)['\"]?", line)
        if item:
            current = item.group(1)
        version = re.fullmatch(r"  version: ['\"]?([0-9a-f]{40}|[0-9a-f]{64})['\"]?", line)
        if section in found and current == "pve_cpi" and version:
            found[section].append(version.group(1))
    require(all(len(entries) == 1 for entries in found.values()), "candidate lacks unique CPI job/package fingerprints")
    return {**identity, "job_version": found["jobs"][0], "package_version": found["packages"][0]}


def observed_director_vmid(verifier: Any, product_uuid: Any) -> str:
    """Bind the authenticated guest observation to one fresh PVE VM config."""
    require(isinstance(product_uuid, str) and re.fullmatch(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}", product_uuid)
            and product_uuid != "00000000-0000-0000-0000-000000000000", "guest SMBIOS identity is invalid")
    # Do not use cluster_vms(), whose node-local fallback cannot prove uniqueness.
    rows = verifier._get("/cluster/resources?type=vm")
    require(isinstance(rows, list) and len(rows) <= 10000, "Director VM inventory is incomplete or exceeds bound")
    matches = []
    seen = set()
    for row in rows:
        require(isinstance(row, dict), "Director VM inventory row is malformed")
        if row.get("type") != "qemu":
            require(row.get("type") == "lxc", "Director VM inventory type is unknown")
            continue
        vmid, node = str(row.get("vmid", "")), row.get("node")
        require(re.fullmatch(r"[1-9][0-9]{0,8}", vmid) and isinstance(node, str) and node and vmid not in seen,
                "Director VM inventory identity is malformed or repeated")
        seen.add(vmid)
        config = verifier.qemu_config(vmid, node)
        require(isinstance(config, dict), "PVE VM configuration is malformed")
        smbios = config.get("smbios1", "")
        require(isinstance(smbios, str), "PVE SMBIOS configuration is malformed")
        fields = smbios.split(",")
        identities = [field.split("=", 1)[1].lower() for field in fields if field.startswith("uuid=")]
        require(len(identities) <= 1, "PVE SMBIOS identity is ambiguous")
        require(all(re.fullmatch(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}", value) for value in identities),
                "PVE SMBIOS UUID is malformed")
        if identities == [product_uuid]:
            matches.append(vmid)
    require(len(matches) == 1, "guest SMBIOS identity does not identify exactly one PVE VM")
    return matches[0]


class DirectorObserver:
    def __init__(self, fixture: dict[str, Any], runner: Any) -> None:
        self.fixture, self.runner = fixture, runner
        self.artifact = archive_fingerprints(fixture["candidate_archive"], fixture["candidate_sha256"])
        self.authority: tuple[str, str, str, str] | None = None

    def snapshot(self, expected_artifact: dict[str, str] | None = None, expected_linux_sha: str | None = None, audit_binary: str | None = None, audit_config: str | None = None) -> dict[str, Any]:
        from _storage_placement_scenarios import verified_record_payload
        artifact = expected_artifact or self.artifact
        linux_sha = expected_linux_sha or self.fixture["linux_cpi_sha256"]
        require(audit_binary is None or re.fullmatch(r"/var/vcap/store/pve_cpi/storage-certification/[0-9a-f]{32}/candidate-cpi", audit_binary), "retained audit binary path is malformed")
        require(audit_config is None or re.fullmatch(r"/var/vcap/store/pve_cpi/storage-certification/[0-9a-f]{32}/candidate-config.json", audit_config), "retained audit config path is malformed")
        if audit_config is not None:
            require(audit_binary is not None and Path(audit_config).parent == Path(audit_binary).parent, "retained audit binary/config identities differ")
        ssh = self.fixture["ssh"]
        argv = ["ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10"]
        if ssh.get("identity_file"):
            argv += ["-o", "IdentitiesOnly=yes", "-i", ssh["identity_file"]]
        if ssh.get("known_hosts_file"):
            argv += ["-o", "UserKnownHostsFile=" + json.dumps(ssh["known_hosts_file"])]
        # OpenSSH sends a remote shell command; quote the fixed Python source
        # explicitly. Fixture values travel only as JSON on standard input.
        argv += [ssh["user"]+"@"+ssh["host"], "sudo -n python3 -c " + shlex.quote(REMOTE_AUDIT)]
        response = subprocess.run(argv, input=json.dumps({"namespace": self.fixture["namespace"], "audit_sha256": self.fixture["linux_cpi_sha256"], "audit_binary": audit_binary, "audit_config": audit_config}), capture_output=True, text=True, timeout=240, check=False)
        require(response.returncode == 0, "remote Director audit or identity read failed")
        require(len(response.stdout.encode()) <= 96*1024*1024, "remote evidence exceeds aggregate bound")
        result = json.loads(response.stdout)
        require(isinstance(result, dict), "remote Director evidence is malformed")
        audit = result["audit"]
        require(audit.get("generation_index_healthy") is True and audit.get("cluster_continuity") is True, "remote Director authority is unhealthy")
        facts = audit.get("audit", {})
        require(facts.get("complete") is True and facts.get("vm_scan_complete") is True and not facts.get("issues") and not facts.get("conflicts"), "remote Director audit is incomplete")
        require(result.get("binary_sha256") == linux_sha, "deployed CPI binary differs from the candidate")
        require(result.get("job_version") == artifact["job_version"], "deployed CPI job differs from candidate archive")
        package_version = result.get("package_version")
        require(isinstance(package_version, str) and re.fullmatch(re.escape(artifact["package_version"]) + r"(?:\.[0-9]+)?", package_version), "deployed CPI compiled package differs from candidate source archive")
        identity = (result["namespace"], result["journal_dir"], result["authority_sha256"], result["director_uuid"])
        require(identity[0] == self.fixture["namespace"], "remote Director namespace changed")
        if self.authority is None:
            self.authority = identity
        require(identity == self.authority, "remote Director authority changed across workflow")
        records = {}
        for summary in audit.get("records") or []:
            require(summary["ID"] not in records, "remote audit repeated a record identity")
            raw = base64.b64decode(result["records"][summary["ID"]], validate=True)
            record = verified_record_payload(raw, summary)
            require(record["namespace"] == identity[0], "remote record belongs to another namespace")
            records[record["id"]] = record
        result["records"] = records
        result["vm_cid"] = observed_director_vmid(self.runner.verifier, result.get("product_uuid"))
        return result


def active_plan(record: dict[str, Any]) -> dict[str, Any]:
    attempts = record.get("attempts") or []
    return (attempts[-1]["plan"] if attempts else record["intent"])["plan"]


def table_rows(payload: Any) -> list[dict[str, Any]]:
    require(isinstance(payload, dict) and isinstance(payload.get("Tables"), list), "BOSH did not return a JSON table")
    rows = []
    for table in payload["Tables"]:
        require(isinstance(table, dict) and isinstance(table.get("Rows"), list), "BOSH table rows are malformed")
        for row in table["Rows"]:
            require(isinstance(row, dict), "BOSH table row is malformed")
            rows.append({str(key).lower().replace(" ", "_"): value for key, value in row.items()})
    return rows


MAX_COMMAND_BYTES = 8 * 1024 * 1024

def bosh_output_environment() -> dict[str, str]:
    # BOSH_TTY would add banners to raw curl/cloud-config responses even when
    # stdout is a pipe. Human task output is enabled explicitly per command.
    environment = dict(os.environ)
    environment.pop("BOSH_TTY", None)
    return environment


class DirectorFailure(RuntimeError):
    """A fixed public reason; raw command output stays in private diagnostics."""


def failure_evidence(error: Exception, director: Any = None) -> dict[str, Any]:
    reason = str(error) if isinstance(error, DirectorFailure) else "Director workflow assertion failed; retained resources require inspection"
    evidence = {"reason": reason}
    if director is not None:
        evidence["command_diagnostic"] = getattr(director, "last_command_diagnostic", None)
        evidence["compilation_observation_diagnostic"] = getattr(director, "compilation_observation_diagnostic", None)
        directory = getattr(director, "command_evidence_dir", None)
        if directory is not None:
            name = "failure-" + uuid.uuid4().hex + ".json"
            try:
                path = Path(directory)/name
                with path.open("x") as stream:
                    os.chmod(path, 0o600)
                    json.dump({"error_type": type(error).__name__, "error": str(error)[:MAX_COMMAND_BYTES], "traceback": traceback.format_exc()[:MAX_COMMAND_BYTES]}, stream)
                evidence["private_failure_diagnostic"] = name
            except OSError:
                evidence["private_failure_diagnostic_retained"] = False
    return evidence


class DirectorScenarios:
    def __init__(self, fixture: dict[str, Any], runner: Any) -> None:
        self.fixture = fixture
        self.runner = director_runner_view(runner, fixture)
        self.observer = DirectorObserver(fixture, self.runner)
        self.run_id = uuid.uuid4().hex
        self.command_evidence_dir = Path(runner.report_path).parent / ("director-private-" + self.run_id)
        self.command_evidence_dir.mkdir(mode=0o700)
        self.last_command_diagnostic = None
        self.directory = tempfile.TemporaryDirectory(prefix="cpi-director-scenarios-")
        self.deployment = json.loads(Path(fixture["deployment_manifest"]).read_text())
        self.cloud = json.loads(Path(fixture["cloud_config"]).read_text())
        self.baseline_cloud = json.loads(Path(fixture["baseline_cloud_config"]).read_text())
        self.name = self.deployment.get("name")
        require(isinstance(self.name, str) and re.fullmatch(r"storage-cert-[a-z0-9-]+", self.name), "workflow deployment must have an explicit storage-cert- name")
        groups = self.deployment.get("instance_groups")
        require(isinstance(groups, list) and all(isinstance(row, dict) for row in groups), "workflow deployment requires instance-group objects")
        self.workload = [row for row in groups if row.get("lifecycle", "service") == "service"]
        self.errands = [row for row in groups if row.get("lifecycle") == "errand"]
        require(len(self.workload) == 1 and self.workload[0].get("instances") == 1 and self.workload[0].get("vm_type") == fixture["workload_vm_type"], "workflow fixture requires one dedicated service instance")
        require(len(self.errands) == 1 and self.errands[0].get("instances") == 1 and self.errands[0].get("vm_type") == fixture["errand_vm_type"], "workflow fixture requires one disposable errand instance")
        require(self.workload[0].get("persistent_disk_type") or self.workload[0].get("persistent_disk"), "workflow service must retain a persistent disk")
        require(any(job.get("name") == fixture["errand_name"] for job in self.errands[0].get("jobs", [])), "declared errand job is absent")
        vm_types = self.cloud.get("vm_types")
        require(isinstance(vm_types, list) and all(isinstance(row, dict) for row in vm_types), "cloud config requires VM-type objects")
        self.tags = {}
        for role in ("compilation", "workload", "errand"):
            name = fixture[role+"_vm_type"]
            found = [entry for entry in vm_types if entry.get("name") == name]
            require(len(found) == 1, "workflow VM type must occur exactly once")
            properties = found[0].setdefault("cloud_properties", {})
            require(isinstance(properties, dict), "workflow VM properties are malformed")
            require(isinstance(properties.setdefault("tags", {}), dict), "workflow tags must be an object")
            properties["tags"]["storagecert"] = self.run_id+"-"+role
            properties["retain_ephemeral_on_delete"] = False
            self.tags[role] = "storagecert--"+self.run_id+"-"+role
        require(self.cloud.get("compilation", {}).get("vm_type") == fixture["compilation_vm_type"], "compilation must select the declared tagged VM type")
        self.cloud["compilation"]["reuse_compilation_vms"] = False
        self.cloud_path = self.write_json("cloud.json", self.cloud)
        self.deployment_path = self.write_json("deployment.json", self.deployment)
        self.results: list[dict[str, Any]] = []
        self.task_evidence: list[dict[str, Any]] = []
        self.last_snapshot: dict[str, Any] = {}
        self.runner.active_resources["director_workflow"] = {"deployment": self.name, "run_id": self.run_id, "phase": "prepared", "namespace": self.fixture["namespace"]}
        self.runner.checkpoint()

    def write_json(self, name: str, value: Any) -> str:
        require(isinstance(name, str) and re.fullmatch(r"[A-Za-z0-9_-]+\.json", name), "private input name is invalid")
        path = Path(self.command_evidence_dir)/name
        with path.open("x") as stream:
            os.chmod(path, 0o600)
            json.dump(value, stream)
            stream.flush()
            os.fsync(stream.fileno())
        descriptor = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
        return str(path)

    def retain_command(self, command: list[str], result: Any = None, error: Exception | None = None) -> None:
        directory = getattr(self, "command_evidence_dir", None)
        if directory is None:
            return
        name = "command-" + uuid.uuid4().hex + ".json"
        def bounded(value):
            if isinstance(value, bytes):
                return value[:MAX_COMMAND_BYTES].decode("utf-8", "replace")
            return str(value or "")[:MAX_COMMAND_BYTES]
        payload = {"command": command, "returncode": getattr(result, "returncode", None),
                   "stdout": bounded(getattr(result, "stdout", getattr(error, "stdout", None))),
                   "stderr": bounded(getattr(result, "stderr", getattr(error, "stderr", None)))}
        try:
            path = Path(directory)/name
            with path.open("x") as stream:
                os.chmod(path, 0o600)
                json.dump(payload, stream)
            self.last_command_diagnostic = name
        except OSError:
            self.last_command_diagnostic = None

    def bosh(self, command: list[str], scoped: bool = True, as_json: bool = False) -> Any:
        argv = ["bosh", "-n", "-e", self.fixture["environment"]]
        if scoped:
            argv += ["-d", self.name]
        # curl already returns the API body. --json wraps it in CLI Blocks.
        if as_json and command[0] != "curl":
            argv += ["--json"]
        elif not as_json and command[0] != "curl":
            argv += ["--tty", "--no-color"]
        try:
            result = subprocess.run(argv+command, capture_output=True, text=True, timeout=3600, check=False, env=bosh_output_environment())
        except (OSError, subprocess.SubprocessError) as error:
            self.retain_command(command, error=error)
            raise DirectorFailure("Fixed BOSH command could not complete; inspect private command diagnostics") from None
        self.retain_command(command, result=result)
        if result.returncode != 0:
            raise DirectorFailure("Fixed BOSH command failed; inspect private command diagnostics")
        if len(result.stdout.encode()) > MAX_COMMAND_BYTES:
            raise DirectorFailure("BOSH command output exceeds the parsing limit")
        if as_json:
            try:
                if command[0] == "curl" and command[-1].endswith("?type=event"):
                    lines = [line for line in result.stdout.splitlines() if line.strip()]
                    if len(lines) > 10000:
                        raise DirectorFailure("Director event output exceeds the event limit")
                    events = [json.loads(line) for line in lines]
                    if not all(isinstance(event, dict) and not {"Tables", "Blocks", "Lines"} <= set(event) for event in events):
                        raise DirectorFailure("Director event output contains a non-object event")
                    return events
                value = json.loads(result.stdout)
                if command[0] == "curl" and (not isinstance(value, (dict, list)) or isinstance(value, dict) and {"Tables", "Blocks", "Lines"} <= set(value)):
                    raise DirectorFailure("BOSH curl did not return a raw Director API object or array")
                return value
            except (ValueError, TypeError):
                raise DirectorFailure("BOSH command returned malformed JSON; inspect private command diagnostics") from None
        return {"stdout_sha256": hashlib.sha256(result.stdout.encode()).hexdigest(), "task_ids": sorted(set(re.findall(r"(?m)^Task ([0-9]+)(?:\.\s|\s|$)", result.stdout)), key=int)}

    def require_workload_stemcells(self) -> None:
        required = self.deployment.get("stemcells")
        if not isinstance(required, list) or not required or not all(isinstance(row, dict) for row in required):
            raise DirectorFailure("Workload manifest must declare its required stemcells")
        available = self.bosh(["curl", "/stemcells"], scoped=False, as_json=True)
        if not isinstance(available, list) or len(available) > 10000 or not all(isinstance(row, dict) for row in available):
            raise DirectorFailure("Director stemcell inventory is malformed")
        for wanted in required:
            version = wanted.get("version")
            os_name, name = wanted.get("os"), wanted.get("name")
            if not isinstance(version, str) or not version or version == "latest" or not (isinstance(os_name, str) and os_name or isinstance(name, str) and name):
                raise DirectorFailure("Workload stemcell prerequisites require an explicit version and OS or name")
            matches = [row for row in available if str(row.get("version", "")) == version
                       and (not os_name or row.get("operating_system") == os_name)
                       and (not name or row.get("name") == name)]
            if len(matches) != 1:
                raise DirectorFailure("Required workload stemcell is missing or ambiguous; upload the matching stemcell before workflow mutation")

    def tasks(self, scoped: bool = True) -> dict[str, dict[str, Any]]:
        endpoint = "/tasks?verbose=2&limit=100"
        if scoped:
            endpoint += "&deployment="+quote(self.name, safe="")
        rows = self.bosh(["curl", endpoint], scoped=False, as_json=True)
        require(isinstance(rows, list) and len(rows) <= 100, "Director task response is malformed")
        values = {}
        for row in rows:
            require(isinstance(row, dict), "Director task row is malformed")
            identity = str(row.get("id", ""))
            require(identity.isdecimal() and identity not in values and (not scoped or row.get("deployment") == self.name), "BOSH task identity or deployment is missing or ambiguous")
            values[identity] = row
        return values

    def operation(self, command: list[str], scoped: bool = True) -> dict[str, Any]:
        global_delete = not scoped
        global_disk = global_delete and len(command) == 2 and command[0] == "delete-disk" and isinstance(command[1], str) and command[1].startswith(("pvz-", "pvd-"))
        require(not global_delete or global_disk or (len(command) == 2 and command[0] == "delete-vm" and isinstance(command[1], str) and command[1].isdecimal()), "unscoped workflow must delete one explicit VM or disk CID")
        before = self.tasks(scoped=False) if global_delete else self.tasks()
        self.runner.active_resources["director_workflow"]["phase"] = command[0]
        self.runner.checkpoint()
        output = self.bosh(command, scoped=scoped)
        after = self.tasks(scoped=False) if global_delete else self.tasks()
        identities = output["task_ids"]
        require(not global_delete or len(identities) == 1, "global deletion must emit exactly one task")
        require(identities, "workflow output did not identify its Director task")
        completed = []
        for identity in identities:
            require(identity not in before and identity in after, "workflow task is stale or missing from Director history")
            row = after[identity]
            if global_delete:
                require("deployment" in row and row["deployment"] is None, "global deletion task unexpectedly belongs to a deployment")
                if global_disk:
                    require(row.get("description") == "delete orphan disks", "global orphan deletion description differs")
                    events = self.bosh(["curl", "/tasks/" + identity + "/output?type=event"], scoped=False, as_json=True)
                    require(isinstance(events, list) and events and all(isinstance(event, dict) for event in events), "orphan deletion event stream is malformed")
                    task_events = [event for event in events if event.get("stage") == "Deleting orphaned disks"]
                    require(len(task_events) == len(events) == 2 and sorted(event.get("state", "") for event in task_events) == ["finished", "started"] and all(event.get("task") == "Deleting orphaned disk " + command[1] and event.get("total") == 1 and event.get("index") == 1 for event in task_events) and any(event.get("state") == "finished" and event.get("progress") == 100 for event in task_events), "global orphan deletion event does not identify the exact completed disk CID")
                else:
                    require(row.get("description") == "delete vm "+command[1], "global deletion task does not identify the exact unscoped VM deletion")
            require(row.get("state") == "done", "workflow task did not complete successfully")
            # The CLI emitted this ID for this exact synchronous invocation;
            # unrelated tasks listed concurrently cannot certify the workflow.
            completed.append({"id": identity, "state": row["state"], "description": row.get("description", ""), "invocation": command[0], "deployment": None if global_delete else self.name, **({("disk_cid" if global_disk else "vm_cid"): command[1], "workflow_deployment": self.name} if global_delete else {})})
        self.task_evidence.extend(completed)
        self.runner.active_resources["director_workflow"]["tasks"] = copy.deepcopy(self.task_evidence)
        self.runner.checkpoint()
        return {"command": command[0], "tasks": completed, **output}

    def instances(self) -> dict[str, Any]:
        rows = table_rows(self.bosh(["instances", "--details"], as_json=True))
        service = [row for row in rows if str(row.get("instance", "")).split("/", 1)[0] == self.workload[0]["name"]]
        require(len(service) == 1, "Director did not report exactly one workflow service instance")
        row = service[0]
        vmid = str(row.get("vm_cid", ""))
        disks = row.get("disk_cids", row.get("disk_cid", ""))
        disks = disks if isinstance(disks, list) else [item.strip() for item in str(disks).split(",") if item.strip()]
        require(vmid.isdecimal() and disks and row.get("process_state") == "running", "workflow service VM, disk, or running state is unproven")
        return {"vm_cid": vmid, "disk_cids": sorted(disks), "instance": row["instance"], "process_state": "running"}

    def snapshot(self) -> dict[str, Any]:
        from _storage_placement_scenarios import verified_record_envelope
        value = self.observer.snapshot()
        endpoint = self.bosh(["curl", "/info"], scoped=False, as_json=True)
        require(endpoint.get("uuid") == value.get("director_uuid") and bool(value.get("director_uuid")), "BOSH environment differs from SSH-observed Director")
        root = Path(self.runner.base_config["storage_allocation_journal_dir"])/hashlib.sha256(self.runner.base_config["storage_placement_namespace"].encode()).hexdigest()/"authority.json"
        local = verified_record_envelope(root.read_bytes())
        require(value["cluster_id"] == local["enrollment"]["cluster_id"], "Director and independent PVE observer target different clusters")
        self.last_snapshot = value
        return value

    def disk_evidence(self, snapshot: dict[str, Any], cid: str, previous: dict[str, Any] | None = None) -> dict[str, Any]:
        from _storage_placement_scenarios import disk_correlation_token
        records = [row for row in snapshot["records"].values() if row.get("kind") == "disk" and row.get("cid") == cid]
        require(len(records) == 1, "Director disk lacks a unique full journal UUID")
        record = records[0]
        token = disk_correlation_token(cid)
        evidence = snapshot["audit"]["audit"].get("evidence") or []
        volumes = {row.get("volume_id") for row in evidence if row.get("allocation_id") == record["id"] and row.get("volume_id")}
        require(len(volumes) == 1, "Director disk actual volume identity is ambiguous")
        volume = next(iter(volumes))
        require(volume.split(":", 1)[0] in self.runner.policy["persistent_storage_ids"], "Director disk escaped declared P members")
        size = self.runner.observed_volume(volume)["size"]
        definition = self.runner.verifier.storage_entry(volume.split(":", 1)[0])
        require(isinstance(definition, dict) and definition.get("type") == "nfs", "Director persistent backing is not independently observable")
        result = {"cid": cid, "allocation_uuid": record["id"], "stable_token": token, "volume_id": volume, "size_bytes": size,
                  "backing": {key: definition.get(key) for key in ("type", "server", "export")}}
        if previous:
            require(all(result[key] == previous[key] for key in ("cid", "allocation_uuid", "stable_token", "size_bytes", "backing")), "Director workflow changed persistent identity, backing, or size")
        return result

    def service_evidence(self, snapshot: dict[str, Any], previous: dict[str, Any] | None = None) -> dict[str, Any]:
        state = self.instances()
        records = [row for row in snapshot["records"].values() if row.get("kind") == "vm" and row.get("cid") == state["vm_cid"]]
        require(len(records) == 1, "Director service lacks a unique managed VM generation")
        record = records[0]
        require(record.get("state") in {"ready_to_return", "adopted"}, "Director service generation is not ready")
        disks = [self.disk_evidence(snapshot, cid, next((old for old in previous["disks"] if old["cid"] == cid), None) if previous else None) for cid in state["disk_cids"]]
        if previous:
            require(state["disk_cids"] == previous["disk_cids"], "Director workflow replaced persistent CIDs")
        summary = next(row for row in snapshot["audit"]["records"] if row["ID"] == record["id"])
        plan = active_plan(record)
        execution = plan["VMExecution"]
        policy = copy.deepcopy(snapshot["policy"])
        policy["root_disk_bus"] = "scsi" if execution["RootDevice"] == "scsi0" else "virtio"
        root = self.runner.observe_root_record(record, summary, policy)
        root["external_volumes"] = [disk["volume_id"] for disk in disks]
        vm = self.runner.observed_vm(state["vm_cid"], policy, execution["EphemeralGiB"] > 0, root)
        actual = self.runner.verifier.qemu_config(state["vm_cid"])
        for disk in disks:
            entries = [str(value) for slot, value in actual.items() if re.fullmatch(r"(?:scsi|virtio|sata|ide)\d+", slot) and str(value).split(",", 1)[0] == disk["volume_id"]]
            require(len(entries) == 1 and "serial="+disk["stable_token"] in entries[0].split(","), "Director disk is not attached with its exact stable token")
        return {**state, "allocation_uuid": record["id"], "disks": disks, "vm": vm, "root_execution": root}

    def records_with_tag(self, snapshot: dict[str, Any], role: str) -> list[dict[str, Any]]:
        return [record for record in snapshot["records"].values() if record.get("kind") == "vm"
                and self.tags[role] in active_plan(record).get("VMExecution", {}).get("Tags", "").split(";")]

    def live_vm_record(self, snapshot: dict[str, Any], record: dict[str, Any]) -> dict[str, Any]:
        from _storage_placement_scenarios import guest_disk_mapping
        require(record.get("state") in {"ready_to_return", "adopted"} and str(record.get("cid", "")).isdecimal(), "workflow VM has not reached a proven ready generation")
        plan = active_plan(record)
        execution = plan["VMExecution"]
        policy = copy.deepcopy(snapshot["policy"])
        policy["root_disk_bus"] = "scsi" if execution["RootDevice"] == "scsi0" else "virtio"
        summary = next(row for row in snapshot["audit"]["records"] if row["ID"] == record["id"])
        root = self.runner.observe_root_record(record, summary, policy)
        vmid = record["cid"]
        node = self.runner.verifier._node_hosting(vmid)
        payload = self.runner.verifier._get(f"/nodes/{quote(node, safe='')}/qemu/{vmid}/agent/get-fsinfo")
        mappings = guest_disk_mapping(payload, execution["EphemeralGiB"] > 0)
        volumes = self.runner.observed_vm(vmid, policy, execution["EphemeralGiB"] > 0, root, mappings)
        return {"allocation_uuid": record["id"], "vm_cid": vmid, "root_execution": root, "vm": volumes}

    def retain_compilation_observation(self, value: dict[str, Any]) -> None:
        self.compilation_observation_diagnostic = None
        try:
            name = "compilation-observation-" + uuid.uuid4().hex + ".json"
            path = Path(self.command_evidence_dir)/name
            with path.open("x") as stream:
                os.chmod(path, 0o600)
                json.dump(value, stream)
            self.compilation_observation_diagnostic = name
        except OSError:
            # Diagnostic retention must not replace a CPI or workflow failure.
            pass

    def compilation(self) -> dict[str, Any]:
        require(not table_rows(self.bosh(["deployments"], scoped=False, as_json=True)), "compilation requires an otherwise empty dedicated Director")
        require(self.read_cloud_config() == self.baseline_cloud, "supplied baseline cloud config differs from the actual initial Director configuration")
        self.require_workload_stemcells()
        before = self.snapshot()
        validate_director_policy(self.runner.policy, self.cloud, self.deployment, self.fixture, before["policy"])
        self.bosh(["update-cloud-config", self.cloud_path], scoped=False)
        self.bosh(["upload-release", self.fixture["compilation_release"]], scoped=False)
        observed: dict[str, dict[str, Any]] = {}
        observation_failures: dict[str, int] = {}
        samples: list[dict[str, Any]] = []
        scanned: dict[str, dict[str, Any]] = {}
        successful_snapshots = 0
        stopped = threading.Event()
        def observe():
            nonlocal successful_snapshots
            while not stopped.is_set():
                try:
                    snapshot = self.snapshot()
                    successful_snapshots += 1
                    for record in self.records_with_tag(snapshot, "compilation"):
                        scanned[record["id"]] = {"cid": record.get("cid"), "state": record.get("state")}
                        if stopped.is_set():
                            break
                        if record["id"] not in before["records"] and record["id"] not in observed:
                            observed[record["id"]] = self.live_vm_record(snapshot, record)
                except (RuntimeError, OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
                    # A transient VM may not yet have mounted its data disk.
                    # Keep bounded classifications; missing proof still fails.
                    name = type(error).__name__
                    observation_failures[name] = observation_failures.get(name, 0) + 1
                    if len(samples) < 16:
                        samples.append({"error_type": name, "error": str(error)[:4096], "traceback": traceback.format_exc()[:16384]})
                stopped.wait(1)
        observer = threading.Thread(target=observe, name="storage-compilation-observer", daemon=True)
        observer.start()
        try:
            operation = self.operation(["deploy", self.deployment_path])
        finally:
            stopped.set()
            observer.join(timeout=300)
            if not observer.is_alive():
                self.retain_compilation_observation({"successful_snapshots": successful_snapshots, "scanned": scanned, "observed": observed, "failures_by_type": observation_failures, "first_failure_samples": samples})
        require(not observer.is_alive(), "compilation observer did not stop within its bounded read deadline")
        after = self.snapshot()
        compiled = [record for record in self.records_with_tag(after, "compilation") if record["id"] not in before["records"]]
        require(compiled and set(record["id"] for record in compiled) <= set(observed), "compilation did not expose every fresh VM's actual storage and guest mounts")
        inventory = self.runner.verification.volume_inventory(self.runner.verifier)
        for record in compiled:
            require(record.get("state") in {"deleted", "cleaned"}, "compilation VM lacks final journal disposition")
            require(not self.runner.verifier.vm_exists(record["cid"]), "compilation VM remains after task completion")
            volumes = set(observed[record["id"]]["vm"]["devices"].values()) | set(observed[record["id"]]["vm"]["iso"].values())
            require(not any(row[2] in volumes for row in inventory), "compilation left an owned actual volume")
        self.service = self.service_evidence(after)
        return {"operation": operation, "compilation_vms": list(observed.values()), "service": self.service,
                "all_compilation_resources_absent": True, "candidate_archive": self.observer.artifact, "observation_failures_by_type": observation_failures}

    def errand(self) -> dict[str, Any]:
        before = self.snapshot()
        operation = self.operation(["run-errand", "--keep-alive", self.fixture["errand_name"]])
        after = self.snapshot()
        records = [record for record in self.records_with_tag(after, "errand") if record["id"] not in before["records"]]
        require(len(records) == 1, "errand did not create exactly one tagged fresh VM")
        observed = self.live_vm_record(after, records[0])
        self.service = self.service_evidence(after, self.service)
        return {"operation": operation, "errand_vm": observed, "service": self.service, "errand_kept_for_final_audited_deletion": True}

    def recreate(self) -> dict[str, Any]:
        old = self.service
        operation = self.operation(["recreate", old["instance"]])
        after = self.snapshot()
        current = self.service_evidence(after, old)
        require(current["vm_cid"] != old["vm_cid"] and current["allocation_uuid"] != old["allocation_uuid"], "recreate did not replace the VM generation")
        require(after["records"][old["allocation_uuid"]].get("state") in {"deleted", "cleaned"}, "recreate did not dispose of the original managed VM")
        require(not self.runner.verifier.vm_exists(old["vm_cid"]), "recreate left its original VM present")
        self.service = current
        return {"operation": operation, "before": old, "after": current, "persistent_uuid_backing_preserved": True}

    def resurrection(self) -> dict[str, Any]:
        prerequisites = self.snapshot()
        require(prerequisites.get("auto_fix_stateful_nodes") is True and prerequisites.get("resurrector_enabled") is True, "stateful resurrection requires observed Director auto_fix_stateful_nodes and health-monitor resurrector plugin")
        old = self.service
        before_tasks = self.tasks()
        prior = self.bosh(["curl", "/resurrection"], scoped=False, as_json=True)
        require(isinstance(prior, dict) and type(prior.get("resurrection")) is bool, "Director resurrection state is unproven")
        self.resurrection_prior = prior["resurrection"]
        self.runner.report["director_recovery"] = {"resurrection_prior": self.resurrection_prior, "restored": False}
        self.runner.checkpoint()
        self.bosh(["update-resurrection", "on"], scoped=False)
        # The documented delete-vm command removes the VM through the Director's
        # own CPI. The instance remains desired, so automatic resurrection must
        # create the replacement. No direct PVE mutation bypasses its journal.
        self.operation(["delete-vm", old["vm_cid"]], scoped=False)
        deadline = time.monotonic()+self.fixture.get("resurrection_timeout_seconds", 1200)
        current = None
        while time.monotonic() < deadline:
            try:
                snapshot = self.snapshot()
                candidate = self.service_evidence(snapshot, old)
                if candidate["vm_cid"] != old["vm_cid"]:
                    current = candidate
                    break
            except (RuntimeError, OSError, ValueError, KeyError, subprocess.SubprocessError):
                pass
            time.sleep(2)
        require(current is not None and current["allocation_uuid"] != old["allocation_uuid"], "automatic resurrection did not produce a new observed VM generation")
        after_tasks = self.tasks()
        tasks = [{"id": identity, "state": row.get("state"), "description": row.get("description", ""), "user": row.get("user"), "deployment": row.get("deployment")}
                 for identity, row in after_tasks.items() if identity not in before_tasks]
        matching = [row for row in tasks if row["state"] == "done" and row["description"] == "scan and fix" and row.get("user") == "hm" and row.get("deployment") == self.name]
        proven = []
        for task in matching:
            events = self.bosh(["curl", "/tasks/"+task["id"]+"/output?type=event"], scoped=False, as_json=True)
            if any(isinstance(event, dict) and event.get("stage") == "Applying problem resolutions" and event.get("state") == "finished" and old["instance"] in str(event.get("task", "")) for event in events):
                proven.append(task)
        require(proven, "replacement lacks an automatic resurrection task naming the exact instance")
        tasks = proven
        self.bosh(["update-resurrection", "on" if self.resurrection_prior else "off"], scoped=False)
        restored = self.bosh(["curl", "/resurrection"], scoped=False, as_json=True)
        require(restored.get("resurrection") is self.resurrection_prior, "Director resurrection setting was not restored")
        self.runner.report["director_recovery"]["restored"] = True
        self.runner.checkpoint()
        require(snapshot["records"][old["allocation_uuid"]].get("state") in {"deleted", "cleaned"} and not self.runner.verifier.vm_exists(old["vm_cid"]), "resurrection left its original VM generation")
        self.service = current
        return {"before": old, "after": current, "tasks": tasks, "persistent_uuid_backing_preserved": True, "resurrection_setting_restored": True}

    def resource_policy_removal(self) -> dict[str, Any]:
        old = self.service
        changed = copy.deepcopy(self.cloud)
        removed = []
        for category in ("vm_types", "disk_types"):
            for entry in changed.get(category, []):
                properties = entry.get("cloud_properties", {})
                for key in ("root_storage_set", "ephemeral_storage_set", "persistent_storage_set", "storage_set"):
                    if key in properties:
                        removed.append(category+"/"+entry["name"]+"/"+key)
                        del properties[key]
        require(removed, "resource policy removal fixture has no explicit set selectors")
        before = self.snapshot()
        require(before["deployed_policy"].get("ephemeral_storage_set") and before["deployed_policy"].get("persistent_storage_set"), "resource removal requires global E and P defaults")
        path = self.write_json("resource-policy-removed.json", changed)
        self.bosh(["update-cloud-config", path], scoped=False)
        operation = self.operation(["recreate", old["instance"]])
        after = self.snapshot()
        current = self.service_evidence(after, old)
        require(current["allocation_uuid"] != old["allocation_uuid"] and after["records"][old["allocation_uuid"]]["state"] in {"deleted", "cleaned"}, "resource removal did not complete VM replacement")
        self.service = current
        self.cloud, self.cloud_path = changed, path
        return {"operation": operation, "removed_selectors": removed, "before": old, "after": current, "authority_preserved": True}

    def read_cloud_config(self) -> dict[str, Any]:
        result = subprocess.run(["bosh", "-n", "-e", self.fixture["environment"], "cloud-config"], capture_output=True, text=True, timeout=180, check=False, env=bosh_output_environment())
        require(result.returncode == 0 and len(result.stdout.encode()) <= 8*1024*1024, "cloud-config readback failed or exceeded bound")
        # BOSH renders evaluated cloud config as YAML. Ruby/Psych is used only
        # as a safe data decoder; no fixture code or YAML object is executed.
        decoded = subprocess.run(["ruby", "-ryaml", "-rjson", "-e", "puts JSON.generate(YAML.safe_load(STDIN.read, permitted_classes: [], permitted_symbols: [], aliases: false))"], input=result.stdout, capture_output=True, text=True, timeout=30, check=False)
        require(decoded.returncode == 0, "cloud-config semantic decoding failed")
        value = json.loads(decoded.stdout)
        require(isinstance(value, dict), "cloud config is not an object")
        return value

    def plan_owned_orphan_disks(self) -> list[tuple[str, dict[str, Any]]]:
        require(not table_rows(self.bosh(["deployments"], scoped=False, as_json=True)), "owned orphan cleanup requires no deployed workloads")
        rows = self.bosh(["curl", "/disks"], scoped=False, as_json=True)
        require(isinstance(rows, list) and all(isinstance(row, dict) for row in rows), "orphan disk inventory is malformed")
        owned = {disk["cid"]: disk for disk in self.service["disks"]}
        require(len(owned) == len(self.service["disks"]) and owned, "owned disk inventory is empty or duplicated")
        cids = [row.get("disk_cid") for row in rows]
        require(all(isinstance(cid, str) and cid in owned for cid in cids) and len(set(cids)) == len(cids), "orphan inventory contains an unowned or repeated disk")
        snapshot = self.snapshot()
        pending = []
        for cid, previous in owned.items():
            record = snapshot["records"][previous["allocation_uuid"]]
            require(record.get("cid") == cid and record.get("kind") == "disk", "orphan disk journal identity changed")
            selected = [row for row in rows if row["disk_cid"] == cid]
            if record.get("state") in {"deleted", "cleaned"}:
                require(not selected, "terminal disk is still registered as orphaned")
                continue
            require(record.get("state") in {"ready_to_return", "adopted"} and len(selected) == 1, "owned disk is not a settled BOSH orphan")
            row = selected[0]
            require(row.get("deployment_name") == self.name and row.get("instance_name") == self.service["instance"] and isinstance(row.get("orphaned_at"), str) and row["orphaned_at"] and type(row.get("size")) is int and row["size"] * 1024 * 1024 == previous["size_bytes"], "orphan registration differs from the owned service disk")
            actual = self.disk_evidence(snapshot, cid, previous)
            self.require_detached_or_parked_disk(actual)
            pending.append((cid, row))
        return pending

    def delete_owned_orphan_disks(self) -> list[dict[str, Any]]:
        pending = self.plan_owned_orphan_disks()
        owned = {disk["cid"]: disk for disk in self.service["disks"]}
        operations = []
        for cid, row in pending:
            current = self.bosh(["curl", "/disks"], scoped=False, as_json=True)
            require(isinstance(current, list) and all(isinstance(entry, dict) for entry in current) and [entry for entry in current if entry.get("disk_cid") == cid] == [row], "owned orphan changed before deletion")
            self.require_detached_or_parked_disk(self.disk_evidence(self.snapshot(), cid, owned[cid]))
            operations.append(self.operation(["delete-disk", cid], scoped=False))
        after = self.bosh(["curl", "/disks"], scoped=False, as_json=True)
        require(isinstance(after, list) and all(isinstance(row, dict) for row in after) and not any(row.get("disk_cid") in owned for row in after), "owned orphan remains registered after deletion")
        return operations

    def require_detached_or_parked_disk(self, disk: dict[str, Any]) -> None:
        verifier = self.runner.verifier
        rows = verifier._get("/cluster/resources?type=vm")
        require(isinstance(rows, list) and len(rows) <= 10000, "orphan attachment inventory is incomplete")
        seen, holders = set(), []
        for row in rows:
            require(isinstance(row, dict) and row.get("type") in {"qemu", "lxc"}, "orphan attachment inventory is malformed")
            require(row["type"] == "qemu", "orphan attachment scan does not support LXC holders")
            vmid, node = str(row.get("vmid", "")), row.get("node")
            require(re.fullmatch(r"[1-9][0-9]{0,8}", vmid) and vmid not in seen and isinstance(node, str) and node, "orphan attachment VM identity is invalid")
            seen.add(vmid)
            config = verifier.qemu_config(vmid, node)
            require(isinstance(config, dict), "orphan attachment VM configuration is malformed")
            for slot, raw in config.items():
                if re.fullmatch(r"(?:scsi|virtio|sata|ide|unused)[0-9]+", slot):
                    require(isinstance(raw, str) and raw, "orphan attachment disk slot is malformed")
                if re.fullmatch(r"(?:scsi|virtio|sata|ide|unused)[0-9]+", slot) and isinstance(raw, str) and (raw.split(",", 1)[0] == disk["volume_id"] or "serial="+disk["stable_token"] in raw.split(",")):
                    tags = config.get("tags")
                    require(isinstance(tags, str) and {"bosh-cpi", "bosh-parker"}.issubset(set(re.split(r"[;, ]+", tags))) and config.get("name") == "bosh-parker-" + vmid and type(config.get("onboot")) is int and config["onboot"] == 0 and config.get("scsihw") == "virtio-scsi-pci" and re.fullmatch(r"scsi(?:[0-9]|[12][0-9]|30)", slot), "owned orphan remains attached to a workload or unproven holder")
                    require(raw.split(",", 1)[0] == disk["volume_id"] and raw.split(",").count("serial=" + disk["stable_token"]) == 1, "parked disk slot identity differs")
                    status = verifier._get(f"/nodes/{node}/qemu/{vmid}/status/current")
                    require(isinstance(status, dict) and status.get("status") == "stopped" and status.get("qmpstatus") == "stopped", "owned disk parker is not stopped")
                    description = config.get("description")
                    matches = re.findall(r"<!--BOSH:(.*?)-->", description, re.DOTALL) if isinstance(description, str) else []
                    require(len(matches) == 1, "parked disk provenance is absent or ambiguous")
                    try:
                        sentinel = json.loads(matches[0])
                        parked = sentinel["bosh_parked_disks"][disk["stable_token"]]
                    except (ValueError, KeyError, TypeError) as exc:
                        raise RuntimeError("parked disk provenance is malformed") from exc
                    backing = disk["backing"]
                    expected = {"disk_cid": disk["cid"], "allocation_id": disk["allocation_uuid"], "allocation_namespace": self.fixture["namespace"], "allocation_backing": "nfs://" + backing["server"] + backing["export"], "volid": disk["volume_id"], "slot": slot, "node": node}
                    require(isinstance(parked, dict) and all(parked.get(key) == value for key, value in expected.items()) and verifier.parked_disk_recorded(vmid, disk["cid"]), "parked disk allocation provenance differs")
                    holders.append((vmid, slot))
        require(len(holders) <= 1, "owned orphan has multiple holders")

    def cleanup(self) -> dict[str, Any]:
        before = self.snapshot()
        owned = {record["id"]: record for role in self.tags for record in self.records_with_tag(before, role)}
        disk_ids = {row["allocation_uuid"] for row in self.service["disks"]}
        volumes = {disk["volume_id"] for disk in self.service["disks"]}
        vmids = {self.service["vm_cid"]}
        for record in owned.values():
            if record.get("cid"):
                vmids.add(record["cid"])
            for step in record.get("steps", []):
                volumes.update(step.get("volids") or [])
        volumes.update(self.service["vm"]["devices"].values())
        iso = self.service["vm"]["iso"]
        volumes.update(iso.values() if isinstance(iso, dict) else iso)
        operation = self.operation(["delete-deployment"])
        orphan_cleanup = self.delete_owned_orphan_disks()
        after = self.snapshot()
        for identity in set(owned) | disk_ids:
            require(after["records"][identity].get("state") in {"deleted", "cleaned"}, "Director workflow cleanup left an allocation unresolved")
        require(not any(self.runner.verifier.vm_exists(vmid) for vmid in vmids), "workflow cleanup left an actual VM")
        inventory = self.runner.verification.volume_inventory(self.runner.verifier)
        require(not any(row[2] in volumes for row in inventory), "workflow cleanup left an actual owned volume")
        require(not table_rows(self.bosh(["deployments"], scoped=False, as_json=True)), "workflow deployment remains after cleanup")
        restoration = self.write_json("baseline-cloud-restoration.json", self.baseline_cloud)
        self.bosh(["update-cloud-config", restoration], scoped=False)
        require(self.read_cloud_config() == self.baseline_cloud, "baseline cloud-config restoration readback differs")
        self.runner.active_resources.pop("director_workflow", None)
        self.runner.checkpoint()
        return {"operation": operation, "orphan_disk_cleanup": orphan_cleanup, "allocation_ids": sorted(set(owned) | disk_ids), "all_owned_records_terminal": True, "baseline_cloud_config_restored": True}


def run_director_scenarios(manifest: dict[str, Any], runner: Any) -> list[dict[str, Any]]:
    fixture = validate_director_manifest(manifest, runner.base_config)
    if not fixture:
        return [{"scenario_id": identity, "status": "missing", "reason": "dedicated Director fixture absent"} for identity in DIRECTOR_IDS]
    try:
        director = DirectorScenarios(fixture, runner)
    except (RuntimeError, ValueError, KeyError, TypeError, OSError, tarfile.TarError):
        return [{"scenario_id": identity, "status": "failed" if index == 0 else "missing", "evidence": {"reason": "Director fixture or candidate artifact initialization failed before workflow mutation"}} for index, identity in enumerate(DIRECTOR_IDS)]
    rows = []
    methods = (("director_compilation", director.compilation), ("director_errand", director.errand),
               ("director_recreate", director.recreate), ("director_resurrection", director.resurrection),
               ("director_resource_policy_removal", director.resource_policy_removal))
    for index, (identity, method) in enumerate(methods):
        try:
            rows.append({"scenario_id": identity, "status": "passed", "evidence": method()})
        except (RuntimeError, ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError) as error:
            rows.append({"scenario_id": identity, "status": "failed", "error_type": type(error).__name__, "evidence": {**failure_evidence(error, director), "tasks": director.task_evidence, "allocation_ids": sorted(director.last_snapshot.get("records", {}))}})
            rows.extend({"scenario_id": remaining, "status": "missing", "reason": "prior Director workflow failed; resources retained"} for remaining in DIRECTOR_IDS[index+1:])
            return rows
    from _storage_placement_rollout import run_rollout_scenarios
    rows.extend(run_rollout_scenarios(director, fixture.get("rollout")))
    return rows
