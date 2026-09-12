"""Executable, opt-in faults confined to declared disposable storage fixtures."""
from __future__ import annotations

import copy
import hmac
import secrets
import http.client
import hashlib
import http.server
import json
import os
import stat
import re
import shlex
import socket
import ssl
import subprocess
import tempfile
import threading
import time
from pathlib import Path
from urllib.parse import unquote, urlsplit, urlencode

BACKEND_IDS = ("backend_export_outage", "backend_read_only", "backend_quota_exhaustion", "backend_inode_exhaustion")
PROXY_IDS = ("response_lost_after_allocation", "crash_after_submission", "ambiguous_backend_error")
VM_IDS = tuple("vm_"+phase+"_"+mode for phase in ("root","ephemeral","iso","ha") for mode in ("response_loss","crash"))
from _storage_fault_transport import PMXBinding, validate_transport

FIXTURE_KEYS = {"storage_id", "nfs_server", "ssh_host", "ssh_user", "mount_path", "filesystem_uuid", "marker_nonce", "export_client", "quota_uid", "inode_limit"}


def validate_fault_manifest(manifest, base_config):
    faults = manifest.get("faults", {})
    if not isinstance(faults, dict) or set(faults) - {"namespace", "backends", "proxy_storage_id", "disk_size_mib", "vm_phases"}:
        raise ValueError("unknown or malformed faults manifest fields")
    if not faults:
        return faults
    namespace = faults.get("namespace")
    if not isinstance(namespace, str) or not namespace or namespace != base_config.get("storage_placement_namespace"):
        raise ValueError("fault namespace must exactly match configured disposable authority")
    size = faults.get("disk_size_mib", 64)
    if type(size) is not int or not 1 <= size <= 1024:
        raise ValueError("fault allocation size must be 1..1024 MiB")
    fixtures = faults.get("backends", [])
    if not isinstance(fixtures, list):
        raise ValueError("backends must be a list")
    seen = set()
    for item in fixtures:
        if not isinstance(item, dict) or not FIXTURE_KEYS <= set(item) or set(item) - FIXTURE_KEYS - {"transport"}:
            raise ValueError("backend fixture fields must match the fixed control schema")
        if "transport" in item:
            if item["transport"] is None:
                raise ValueError("backend transport must not be null")
            validate_transport(item["transport"])
        for key in ("storage_id", "filesystem_uuid", "marker_nonce"):
            if not isinstance(item[key], str) or not re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", item[key]):
                raise ValueError("backend fixture identity is malformed")
        if len(item["marker_nonce"]) < 16 or len(item["filesystem_uuid"]) < 8:
            raise ValueError("backend marker and filesystem UUID must be explicit identities")
        if not isinstance(item["nfs_server"], str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.-]*", item["nfs_server"]):
            raise ValueError("NFS server must be an explicit host identity")
        if item["ssh_user"] != "root" or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.-]*", item["ssh_host"]):
            raise ValueError("backend control requires a declared root SSH host")
        path = Path(item["mount_path"])
        if not path.is_absolute() or str(path) != item["mount_path"] or path == Path("/") or ".." in path.parts or len(path.parts) < 3:
            raise ValueError("backend mount must be an exact dedicated absolute path")
        if not re.fullmatch(r"[A-Za-z0-9*.:/_-]+", item["export_client"]):
            raise ValueError("backend export client is malformed")
        if type(item["quota_uid"]) is not int or not 1 <= item["quota_uid"] <= 2147483647:
            raise ValueError("quota UID must be a non-root numeric identity")
        if type(item["inode_limit"]) is not int or not 1 <= item["inode_limit"] <= 200000:
            raise ValueError("inode fault requires a bounded limit of at most 200000")
        key = (item["ssh_host"], item["mount_path"])
        if key in seen:
            raise ValueError("duplicate backend filesystem declaration")
        seen.add(key)
    proxy = faults.get("proxy_storage_id", "")
    if proxy and (not isinstance(proxy, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+", proxy)):
        raise ValueError("proxy storage identity is malformed")
    phases = faults.get("vm_phases", [])
    if not isinstance(phases, list) or any(not isinstance(phase,str) for phase in phases) or len(set(phases)) != len(phases) or not set(phases) <= {"root","ephemeral","iso","ha"}:
        raise ValueError("VM fault phases must be an explicit unique supported list")
    return faults


class BackendController:
    def __init__(self, fixture, namespace, run_id=None, evidence_directory=None):
        self.evidence_directory = Path(evidence_directory) if evidence_directory else None
        self.run_id = run_id or secrets.token_hex(16)
        if not re.fullmatch(r"[0-9a-f]{32}", self.run_id):
            raise ValueError("fault run ID is malformed")
        self.fixture = {key: value for key, value in fixture.items() if key not in ("ssh_host", "ssh_user", "storage_id", "transport")}
        self.fixture["namespace"] = namespace
        self.fixture["run_id"] = self.run_id
        self.host = fixture["ssh_user"] + "@" + fixture["ssh_host"]
        if "transport" in fixture and fixture["transport"] is None:
            raise ValueError("backend transport must not be null")
        self.transport = PMXBinding(fixture["transport"]) if "transport" in fixture else None
        if self.transport:
            self.fixture["transport_binding"] = self.transport.fingerprint

    def retain(self, path, value):
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(descriptor, "w") as stream:
            json.dump(value, stream)
            stream.flush()
            os.fsync(stream.fileno())
        descriptor = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)

    def invoke(self, action, fault=None):
        source = Path(__file__).with_name("_storage_fault_backend.py").read_text()
        request = {"action": action, "fixture": self.fixture}
        if fault:
            request["fault"] = fault
        receipt = None
        if self.evidence_directory:
            self.evidence_directory.mkdir(mode=0o700, exist_ok=True)
            info = self.evidence_directory.lstat()
            if not stat.S_ISDIR(info.st_mode) or info.st_mode & 0o077:
                raise RuntimeError("backend evidence directory must be private")
            descriptor = os.open(self.evidence_directory.parent, os.O_RDONLY)
            try:
                os.fsync(descriptor)
            finally:
                os.close(descriptor)
            receipt = self.evidence_directory / (self.run_id + "-" + secrets.token_hex(16))
            self.retain(receipt.with_suffix(".attempt.json"), {
                "action": action, "fault": fault, "run_id": self.run_id,
                "source_sha256": hashlib.sha256(source.encode()).hexdigest(),
                "request_sha256": hashlib.sha256(json.dumps(request).encode()).hexdigest(),
                "started_at_unix": time.time()})
        arguments = (self.transport.command(shlex.quote(source)) if self.transport else
                     ["ssh", "-oBatchMode=yes", "-oStrictHostKeyChecking=yes", "-oConnectTimeout=15", "--", self.host,
                      "python3", "-c", shlex.quote(source)])
        options = {"env": self.transport.environment()} if self.transport else {}
        try:
            process = subprocess.run(arguments, input=json.dumps(request), text=True,
                                     capture_output=True, timeout=180, check=False, **options)
        except (OSError, subprocess.TimeoutExpired) as error:
            if receipt:
                try:
                    self.retain(receipt.with_suffix(".result.json"), {"error_class": type(error).__name__, "completed_at_unix": time.time()})
                except OSError:
                    pass
            raise RuntimeError("disposable backend transport failed; retained remote state requires inspection") from None
        if receipt:
            try:
                self.retain(receipt.with_suffix(".result.json"), {
                    "returncode": process.returncode, "completed_at_unix": time.time(),
                    "stdout": process.stdout[:1024 * 1024], "stderr": process.stderr[:1024 * 1024],
                    "stdout_truncated": len(process.stdout) > 1024 * 1024,
                    "stderr_truncated": len(process.stderr) > 1024 * 1024})
            except OSError:
                if process.returncode:
                    raise RuntimeError("disposable backend control failed and private evidence retention failed") from None
                raise RuntimeError("backend private evidence retention failed; inspect remote state") from None
        if process.returncode:
            raise RuntimeError("disposable backend control failed; retained remote state requires inspection")
        try:
            if len(process.stdout) > 1024 * 1024:
                raise ValueError("response bound")
            response = json.loads(process.stdout)
        except (TypeError, ValueError):
            raise RuntimeError("backend control returned malformed evidence") from None
        if not isinstance(response, dict):
            raise RuntimeError("backend control returned malformed evidence")
        return response


class LoopbackPVEFaultProxy:
    """Forward to one pinned PVE endpoint and lose one real allocation response."""
    def __init__(self, base_config, storage_ids, mode):
        if mode not in PROXY_IDS + ("observe_allocation",) or not storage_ids:
            raise ValueError("proxy requires an explicit allocation fault and target")
        self.config = copy.deepcopy(base_config)
        self.storage_ids = set(storage_ids)
        self.mode = mode
        self.first_success = threading.Event()
        self.release = threading.Event()
        self.lock = threading.Lock()
        self.auth_lock = threading.Lock()
        self.capability = "fault-proxy@pve!run=" + secrets.token_hex(32)
        self.ticket_headers = None
        self.evidence = {"attempted_allocations": 0, "forwarded_allocations": 0, "response_withheld": False, "blocked_mutations": 0, "allocation_status": None}
        self.directory = None
        self.server = None
        self.thread = None

    def __enter__(self):
        self.directory = tempfile.TemporaryDirectory(prefix="cpi-fault-proxy-")
        root = Path(self.directory.name)
        cert, key = root / "public.pem", root / "private.pem"
        result = subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-subj", "/CN=localhost", "-keyout", str(key), "-out", str(cert)], capture_output=True, timeout=30, check=False)
        if result.returncode:
            self.directory.cleanup()
            raise RuntimeError("local fault proxy certificate generation failed")
        key.chmod(0o600)
        proxy = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def setup(self):
                super().setup()
                self.connection.settimeout(120)

            def log_message(self, *_args):
                pass

            def do_GET(self):
                proxy.forward(self)

            def do_POST(self):
                proxy.forward(self)

            def do_PUT(self):
                proxy.forward(self)

            def do_DELETE(self):
                proxy.forward(self)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(cert, key)
        self.server.socket = context.wrap_socket(self.server.socket, server_side=True)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        return self

    @property
    def config_override(self):
        if self.server is None:
            raise RuntimeError("proxy is not running")
        # Disable direct node hops so every mutation traverses the fault boundary.
        return {"api_token": self.capability, "password": "", "user": "fault-proxy", "realm": "pve",
                "host": "127.0.0.1", "port": self.server.server_address[1], "verify_ssl": False,
                "node_endpoints": {}, "node_endpoints_discovery": False}

    def upstream_context(self):
        ca = self.config.get("pve_ca_cert") if self.config.get("verify_ssl") is not False else None
        # A configured CA replaces the system trust pool, matching the CPI SDK.
        context = ssl.create_default_context(cadata=ca) if ca else ssl.create_default_context()
        if self.config.get("verify_ssl") is False:
            context.check_hostname = False
            context.verify_mode = ssl.CERT_NONE
        return context

    def upstream_auth(self, connection):
        token = self.config.get("api_token")
        if token:
            return {"Authorization": token if token.startswith("PVEAPIToken=") else "PVEAPIToken=" + token}
        with self.auth_lock:
            if self.ticket_headers is None:
                user = self.config["user"]
                if "@" not in user:
                    user += "@" + self.config.get("realm", "pam")
                connection.request("POST", "/api2/json/access/ticket", urlencode({"username": user, "password": self.config["password"]}), {"Content-Type": "application/x-www-form-urlencoded"})
                response = connection.getresponse()
                data = json.loads(response.read()).get("data", {})
                if response.status != 200 or not data.get("ticket") or not data.get("CSRFPreventionToken"):
                    raise RuntimeError("upstream fault proxy authentication failed")
                self.ticket_headers = {"Cookie": "PVEAuthCookie=" + data["ticket"], "CSRFPreventionToken": data["CSRFPreventionToken"]}
            return dict(self.ticket_headers)

    def planned_allocation(self, allocation_id, node, storage, vmid, filename):
        directory = Path(self.config["storage_allocation_journal_dir"])
        namespace = self.config["storage_placement_namespace"]
        path = directory / hashlib.sha256(namespace.encode()).hexdigest() / ("allocation-" + allocation_id + ".json")
        if path.resolve() != path:
            return False
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(descriptor, "rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_size > 16 * 1024 * 1024:
                return False
            from _storage_placement_scenarios import ScenarioFailure, verified_record_envelope
            try:
                record = verified_record_envelope(stream.read(16 * 1024 * 1024 + 1))
            except ScenarioFailure as error:
                raise ValueError("allocation journal envelope is invalid") from error
        if (record.get("version") != 1 or record.get("kind") != "disk"
                or record.get("id") != allocation_id or record.get("namespace") != namespace):
            return False
        active_attempt = max(0, len(record.get("attempts", [])) - 1)
        matches = []
        for step in record.get("steps", []):
            target = step.get("target", {})
            if (step.get("kind") == "create_persistent_volume" and step.get("state") == "planned"
                    and step.get("attempt", 0) == active_attempt
                    and target.get("node") == node and target.get("storage") == storage
                    and target.get("vmid") == vmid and not target.get("external")
                    and target.get("intended_volume") == f"{storage}:{vmid}/{filename}"):
                matches.append(step)
        return len(matches) == 1

    def forward(self, handler):
        authorization = handler.headers.get("Authorization", "")
        if not hmac.compare_digest(authorization, "PVEAPIToken=" + self.capability):
            handler.send_error(403)
            return
        path = urlsplit(handler.path)
        if path.scheme or path.netloc or not path.path.startswith("/api2/json/"):
            handler.send_error(400)
            return
        match = re.fullmatch(r"/api2/json/nodes/[^/]+/storage/([^/]+)/content", path.path)
        allocation = handler.command == "POST" and match is not None and unquote(match[1]) in self.storage_ids
        if handler.command != "GET" and not allocation:
            with self.lock:
                self.evidence["blocked_mutations"] += 1
            handler.send_error(503)
            return
        if allocation:
            with self.lock:
                self.evidence["attempted_allocations"] += 1
                if self.evidence["forwarded_allocations"]:
                    handler.send_error(503)
                    return
        try:
            length = int(handler.headers.get("Content-Length", 0))
        except (ValueError, TypeError):
            handler.send_error(400)
            return
        if length < 0 or length > 4 * 1024 * 1024:
            handler.send_error(413)
            return
        body = handler.rfile.read(length)
        if allocation:
            from urllib.parse import parse_qs
            try:
                fields = json.loads(body) if "application/json" in handler.headers.get("Content-Type", "") else {key: values[0] for key, values in parse_qs(body.decode()).items() if len(values) == 1}
                if (set(fields) - {"filename", "vmid", "size", "format"}
                        or fields.get("size") != "1G"):
                    raise ValueError("allocation differs from the bounded fault fixture")
                filename = fields.get("filename", "")
                if fields.get("format", "") not in ("", filename.rsplit(".", 1)[-1]):
                    raise ValueError("allocation format differs from its recorded filename")
                locator = hashlib.sha256(self.config["storage_placement_namespace"].encode()).hexdigest()[:16]
                valid = re.fullmatch(r"vm-([1-9][0-9]*)-bosh-" + locator + r"-alloc-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.(?:raw|qcow2|vmdk)", filename)
                if valid and str(fields.get("vmid", "")) != valid[1]:
                    valid = False
                if valid:
                    allocation_id = filename.split("-alloc-", 1)[1].rsplit(".", 1)[0]
                    valid = self.planned_allocation(allocation_id, unquote(path.path.split("/")[4]),
                                                    unquote(match[1]), int(fields["vmid"]), filename)
            except (ValueError, KeyError, TypeError, OSError, AttributeError):
                valid = False
            if not valid:
                with self.lock:
                    self.evidence["blocked_mutations"] += 1
                handler.send_error(503)
                return
        if allocation:
            with self.lock:
                if self.evidence["forwarded_allocations"]:
                    handler.send_error(503)
                    return
                self.evidence["forwarded_allocations"] += 1
                self.evidence["allocation_id"] = allocation_id
                self.evidence["vmid"] = int(fields["vmid"])
                self.evidence["storage_id"] = unquote(match[1])
        connection = http.client.HTTPSConnection(self.config["host"], int(self.config.get("port", 8006)), timeout=90, context=self.upstream_context())
        try:
            headers = {key: value for key, value in handler.headers.items() if key.lower() not in ("host", "connection", "transfer-encoding", "accept-encoding", "authorization", "cookie", "csrfpreventiontoken")}
            headers.update(self.upstream_auth(connection))
            connection.request(handler.command, handler.path, body, headers)
            response = connection.getresponse()
            payload = response.read()
            if allocation:
                with self.lock:
                    self.evidence["allocation_status"] = response.status
            if allocation and self.mode != "observe_allocation" and 200 <= response.status < 300:
                with self.lock:
                    self.evidence["response_withheld"] = True
                if self.mode == "ambiguous_backend_error":
                    handler.send_response(500)
                    payload = b'{"data":null,"message":"injected unknown allocation result"}'
                    handler.send_header("Content-Type", "application/json")
                    handler.send_header("Content-Length", str(len(payload)))
                    handler.end_headers()
                    handler.wfile.write(payload)
                    self.first_success.set()
                    return
                self.first_success.set()
                if self.mode == "crash_after_submission":
                    self.release.wait(120)
                handler.close_connection = True
                try:
                    handler.connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
                return
            handler.send_response(response.status)
            handler.send_header("Content-Type", "application/json")
            handler.send_header("Content-Length", str(len(payload)))
            handler.end_headers()
            handler.wfile.write(payload)
        except (OSError, http.client.HTTPException, RuntimeError, ValueError, KeyError):
            handler.close_connection = True
        finally:
            connection.close()

    def __exit__(self, *_args):
        self.release.set()
        if self.server:
            self.server.shutdown()
            self.server.server_close()
        if self.thread:
            self.thread.join(10)
        if self.directory:
            self.directory.cleanup()


def _snapshot(runner):
    return {"journal": runner.verification._journal_records(), "volumes": runner.verification.volume_inventory(runner.verifier)}


def _config(runner):
    config = copy.deepcopy(runner.base_config)
    config["detached_disk_strategy"] = "free"
    return config


def _verify_single_attempt(before, after):
    original_rows = {row.get("ID", row.get("allocation_id", row.get("id"))): row for row in before["journal"]["records"]}
    original = set(original_rows)
    current_rows = {row.get("ID", row.get("allocation_id", row.get("id"))): row for row in after["journal"]["records"]}
    if any(current_rows.get(key) != row for key, row in original_rows.items()):
        raise RuntimeError("fault changed an existing allocation record")
    created = [row for row in after["journal"]["records"] if row.get("ID", row.get("allocation_id", row.get("id"))) not in original]
    if len(created) > 1:
        raise RuntimeError("fault created more than one allocation identity")
    old_volumes = {tuple(row) for row in before["volumes"]}
    if not old_volumes <= {tuple(row) for row in after["volumes"]}:
        raise RuntimeError("fault changed or deleted an existing storage volume")
    additions = [row for row in after["volumes"] if tuple(row) not in old_volumes]
    actual = {row[2] for row in additions}
    if len(actual) > 1:
        raise RuntimeError("fault allocated more than one actual disk")
    if actual and not created:
        raise RuntimeError("fault left an unrecorded storage artifact")
    evidence = after["journal"]["evidence"]
    created_ids = {row.get("ID", row.get("allocation_id", row.get("id"))) for row in created}
    if any(not any(item.get("volume_id") == volume and item.get("allocation_id") in created_ids for item in evidence) for volume in actual):
        raise RuntimeError("fault artifact is not retained by the allocation audit")
    return {"new_allocations": created, "retained_volumes": sorted(actual)}


def _verify_submission(proof, result, require_volume=False):
    created = result["new_allocations"]
    if len(created) != 1 or created[0].get("ID") != proof.get("allocation_id"):
        raise RuntimeError("submitted allocation lacks its exact retained journal identity")
    if created[0].get("State") not in ("planned", "submitted", "observed", "reconciliation_required"):
        raise RuntimeError("unknown allocation was returned or disposed without settlement")
    if created[0].get("CID"):
        raise RuntimeError("unknown allocation acquired a returnable CID")
    if require_volume and len(result["retained_volumes"]) != 1:
        raise RuntimeError("successful backend submission did not retain one actual volume")


def _backend_case(runner, fixture, namespace, scenario, size, evidence):
    from _storage_placement_scenarios import CPIRejected
    storage = fixture["storage_id"]
    if storage not in runner.verification.members["persistent"]:
        raise RuntimeError("backend fault storage is outside the observed persistent set")
    expected_backing = "nfs://" + fixture["nfs_server"] + fixture["mount_path"]
    pairs = [row["Pair"] for row in runner.verification.diagnostic.get("capacities", []) if row["Pair"].get("StorageID") == storage]
    if not pairs or any(pair.get("BackingKey") != expected_backing for pair in pairs):
        raise RuntimeError("declared export does not match observed actual storage backing")
    report_path = getattr(runner, "report_path", None)
    controller = BackendController(fixture, namespace, evidence_directory=(
        report_path.with_name(report_path.name + ".backend-control") if report_path else None))
    before = _snapshot(runner)
    evidence["before"] = before
    observed = controller.invoke("inspect")
    evidence["backend_before"] = observed
    evidence["fixture"] = {"storage_id": storage, "mount_path": fixture["mount_path"], "filesystem_uuid": fixture["filesystem_uuid"], "run_id": controller.run_id}
    restoration = None
    outcome = "rejected"
    try:
        evidence["apply"] = controller.invoke("apply", scenario)
        config = _config(runner)
        with LoopbackPVEFaultProxy(config, {storage}, "observe_allocation") as proxy:
            config.update(proxy.config_override)
            path = runner.derived_config(config, scenario)
            try:
                cid = runner.call_once("create_disk", [size, {"storage_pool": storage}, ""], path)
            except CPIRejected:
                cid = None
            evidence["proxy"] = dict(proxy.evidence)
        if cid is not None:
            if isinstance(cid, str) and cid:
                # The exact returned allocation is authorized cleanup, never a guessed volume.
                evidence["unexpected_disk_cid"] = cid
                restoration = controller.invoke("restore")
                evidence["restoration"] = restoration
                cleanup_path = runner.derived_config(_config(runner), scenario + "-cleanup")
                runner.call_once("delete_disk", [cid], cleanup_path)
            raise RuntimeError("backend fault unexpectedly allowed allocation")
        proof = evidence["proxy"]
        if scenario != "backend_export_outage":
            if (proof["attempted_allocations"] != 1 or proof["forwarded_allocations"] != 1
                    or proof["blocked_mutations"] or not isinstance(proof["allocation_status"], int)
                    or not 500 <= proof["allocation_status"] < 600):
                raise RuntimeError("backend fault did not reach one actual failed allocation")
        elif proof["forwarded_allocations"]:
            if (proof["attempted_allocations"] != 1 or proof["forwarded_allocations"] != 1
                    or proof["blocked_mutations"] or not isinstance(proof["allocation_status"], int)
                    or not 500 <= proof["allocation_status"] < 600):
                raise RuntimeError("outage did not isolate an actual failed allocation")
        else:
            unavailable = []
            for node in sorted({pair["Node"] for pair in pairs}):
                statuses = runner.verifier._get("/nodes/" + node + "/storage")
                if not isinstance(statuses, list):
                    raise RuntimeError("outage target status is unreadable")
                target = [row for row in statuses if isinstance(row, dict) and row.get("storage") == storage]
                if len(target) != 1 or target[0].get("active") not in (False, 0, "0"):
                    raise RuntimeError("outage did not establish selected target unavailability")
                unavailable.append(node)
            evidence["unavailable_nodes"] = unavailable
    finally:
        if restoration is None:
            restoration = controller.invoke("restore")
        evidence["restoration"] = restoration
    if restoration.get("restored") is not True or restoration.get("after") != observed:
        raise RuntimeError("backend restoration was not independently verified")
    after = _snapshot(runner)
    evidence["after"] = after
    result = _verify_single_attempt(before, after)
    if proof["forwarded_allocations"]:
        _verify_submission(proof, result)
    return {"before": before, "after": after, "restore_verified": True, "request_outcome": outcome,
            **result}


def _proxy_case(runner, storage, scenario, size, evidence):
    from _storage_placement_scenarios import CPIRejected
    if storage not in runner.verification.members["persistent"]:
        raise RuntimeError("proxy target is outside the observed persistent set")
    before = _snapshot(runner)
    evidence["before"] = before
    config = _config(runner)
    with LoopbackPVEFaultProxy(config, {storage}, scenario) as proxy:
        config.update(proxy.config_override)
        path = runner.derived_config(config, scenario)
        if scenario == "crash_after_submission":
            request = {"method": "create_disk", "arguments": [size, {"storage_pool": storage}, ""], "context": {}, "api_version": 2}
            process = subprocess.Popen([runner.cpi_bin, "--config", path], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            from _storage_placement_vm_faults import supervise_fault_process
            evidence["method"] = "create_disk"
            requested = confirmed = False
            try:
                requested = True
                process.stdin.write(json.dumps(request) + "\n")
                process.stdin.flush()
                deadline = time.monotonic()+120
                while not proxy.first_success.wait(0.1):
                    if process.poll() is not None or time.monotonic() >= deadline:
                        supervise_fault_process(runner,process,evidence)
                        raise RuntimeError("Disk fault barrier was not established; CPI completion was supervised without injection")
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
                runner.call_once("create_disk", [size, {"storage_pool": storage}, ""], path)
            except CPIRejected:
                pass
            else:
                raise RuntimeError("lost allocation response unexpectedly returned success")
        proof = dict(proxy.evidence)
        evidence["proxy"] = proof
    if proof["attempted_allocations"] != 1 or proof["forwarded_allocations"] != 1 or not proof["response_withheld"] or proof["blocked_mutations"]:
        raise RuntimeError("fault did not isolate one actual allocation submission")
    after = _snapshot(runner)
    evidence["after"] = after
    result = _verify_single_attempt(before, after)
    _verify_submission(proof, result, require_volume=True)
    return {"before": before, "after": after, "proxy": proof, **result}


def run_fault_scenarios(manifest, runner):
    faults = validate_fault_manifest(manifest, runner.base_config)
    rows = []
    fixture = (faults.get("backends") or [None])[0]
    scenarios = BACKEND_IDS + PROXY_IDS + VM_IDS
    for index, scenario in enumerate(scenarios):
        storage = faults.get("proxy_storage_id")
        if scenario in VM_IDS:
            _, phase, mode = scenario.split("_", 2)
            if phase not in faults.get("vm_phases", []):
                rows.append({"scenario_id":scenario,"status":"missing","evidence":{"reason":"VM fault phase is not declared"}})
                continue
        if (scenario in BACKEND_IDS and fixture is None) or (scenario in PROXY_IDS and not storage):
            rows.append({"scenario_id": scenario, "status": "missing", "evidence": {"reason": "explicit disposable fixture is not declared"}})
            continue
        evidence = {}
        try:
            if scenario in BACKEND_IDS:
                evidence.update(_backend_case(runner, fixture, faults["namespace"], scenario, faults.get("disk_size_mib", 64), evidence))
            elif scenario in VM_IDS:
                from _storage_placement_vm_faults import run_vm_fault
                evidence.update(run_vm_fault(runner, phase, mode, evidence))
            else:
                evidence.update(_proxy_case(runner, storage, scenario, faults.get("disk_size_mib", 64), evidence))
            rows.append({"scenario_id": scenario, "status": "passed", "evidence": evidence})
        except (RuntimeError, ValueError, OSError, subprocess.SubprocessError):
            evidence["reason"] = "fault or verified restoration failed; inspect retained journal and fixture state"
            rows.append({"scenario_id": scenario, "status": "failed", "evidence": evidence})
            # Never stack another fault on an uncertain backend or allocation.
            rows.extend({"scenario_id": remaining, "status": "missing",
                         "evidence": {"reason": "prior scenario requires reconciliation"}}
                        for remaining in scenarios[index + 1:])
            break
    return rows


def restore_backend_fixture(manifest, base_config, storage_id, run_id):
    """Explicitly restore one declared backend using its retained fault run ID."""
    faults = validate_fault_manifest(manifest, base_config)
    fixtures = [entry for entry in faults.get("backends", []) if entry["storage_id"] == storage_id]
    if len(fixtures) != 1:
        raise ValueError("restoration requires one exact declared backend")
    return BackendController(fixtures[0], faults["namespace"], run_id).invoke("restore")
