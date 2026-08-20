#!/usr/bin/env python3
"""Offline unit tests for _integration helper functions.

Run with:
    python3 scripts/_integration_test.py

Uses only stdlib; no PVE network connection required.
"""

from __future__ import annotations

import importlib.machinery as _ilm
import importlib.util as _ilu
import sys
import tempfile
import types as _types
import unittest
import unittest.mock
from pathlib import Path

# Mirror the import pattern used by scripts/test so the module resolves
# regardless of the working directory the test is invoked from.
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _integration

# Import the scripts/cf module under test.  The file has no .py extension so
# we load it via importlib with an explicit SourceFileLoader.  The scripts/cf
# file declares `requests` as a uv inline dependency; we stub it at import
# time so unit tests run without the package installed.
import importlib.util as _ilu
import importlib.machinery as _ilm
import types as _types

def _load_cf_module():
    _cf_path = str(Path(__file__).resolve().parent / "cf")
    # Stub `requests` if not installed (uv provides it at runtime).
    _requests_stub = _types.ModuleType("requests")
    _requests_stub.get = lambda *a, **kw: None  # type: ignore[attr-defined]
    import sys as _sys
    _had_requests = "requests" in _sys.modules
    if not _had_requests:
        _sys.modules["requests"] = _requests_stub
    try:
        loader = _ilm.SourceFileLoader("cf_script", _cf_path)
        spec = _ilu.spec_from_loader("cf_script", loader, origin=_cf_path)
        mod = _ilu.module_from_spec(spec)
        mod.__file__ = _cf_path  # required for Path(__file__) at module level
        loader.exec_module(mod)
    finally:
        if not _had_requests:
            _sys.modules.pop("requests", None)
    return mod


class TestSelectLocalDiskPools(unittest.TestCase):
    """Tests for _integration.select_local_disk_pools."""

    def _call(self, entries: list[dict]) -> list[str]:
        return _integration.select_local_disk_pools(entries)

    def test_lvm_with_images_kept(self) -> None:
        entries = [
            {"storage": "local-lvm", "type": "lvm", "content": "images,rootdir"},
        ]
        self.assertEqual(self._call(entries), ["local-lvm"])

    def test_lvmthin_with_images_kept(self) -> None:
        entries = [
            {"storage": "local-lvm-thin", "type": "lvmthin", "content": "images"},
        ]
        self.assertEqual(self._call(entries), ["local-lvm-thin"])

    def test_zfspool_with_images_kept(self) -> None:
        entries = [
            {"storage": "tank-zfs", "type": "zfspool", "content": "images,rootdir"},
        ]
        self.assertEqual(self._call(entries), ["tank-zfs"])

    def test_dir_with_images_and_other_content_kept(self) -> None:
        entries = [
            {"storage": "local", "type": "dir", "content": "images,iso,backup"},
        ]
        self.assertEqual(self._call(entries), ["local"])

    def test_dir_with_iso_only_excluded(self) -> None:
        entries = [
            {"storage": "iso-store", "type": "dir", "content": "iso"},
        ]
        self.assertEqual(self._call(entries), [])

    def test_nfs_with_images_excluded_wrong_type(self) -> None:
        entries = [
            {"storage": "nfs-store", "type": "nfs", "content": "images"},
        ]
        self.assertEqual(self._call(entries), [])

    def test_cephfs_excluded_wrong_type(self) -> None:
        entries = [
            {"storage": "ceph-store", "type": "cephfs", "content": "images"},
        ]
        self.assertEqual(self._call(entries), [])

    def test_disabled_lvm_int_excluded(self) -> None:
        entries = [
            {"storage": "disabled-lvm", "type": "lvm", "content": "images", "disable": 1},
        ]
        self.assertEqual(self._call(entries), [])

    def test_disabled_lvm_str_excluded(self) -> None:
        entries = [
            {"storage": "disabled-lvm", "type": "lvm", "content": "images", "disable": "1"},
        ]
        self.assertEqual(self._call(entries), [])

    def test_disabled_lvm_bool_excluded(self) -> None:
        entries = [
            {"storage": "disabled-lvm", "type": "lvm", "content": "images", "disable": True},
        ]
        self.assertEqual(self._call(entries), [])

    def test_full_representative_list(self) -> None:
        """Combined fixture covering all keep/exclude cases from the spec."""
        entries = [
            # Keep: lvm w/ images
            {"storage": "local-lvm", "type": "lvm", "content": "images,rootdir"},
            # Keep: lvmthin w/ images
            {"storage": "local-lvm-thin", "type": "lvmthin", "content": "images"},
            # Keep: zfspool w/ images
            {"storage": "tank-zfs", "type": "zfspool", "content": "images,rootdir"},
            # Keep: dir w/ images + iso + backup
            {"storage": "local", "type": "dir", "content": "images,iso,backup"},
            # Exclude: dir w/ iso only (no images)
            {"storage": "iso-store", "type": "dir", "content": "iso"},
            # Exclude: nfs w/ images (wrong type)
            {"storage": "nfs-data", "type": "nfs", "content": "images"},
            # Exclude: cephfs (wrong type)
            {"storage": "ceph-fs", "type": "cephfs", "content": "images"},
            # Exclude: lvm w/ images but disabled (int 1)
            {"storage": "old-lvm", "type": "lvm", "content": "images", "disable": 1},
        ]
        result = self._call(entries)
        self.assertEqual(result, ["local", "local-lvm", "local-lvm-thin", "tank-zfs"])

    def test_empty_entries_returns_empty(self) -> None:
        self.assertEqual(self._call([]), [])

    def test_result_is_sorted(self) -> None:
        entries = [
            {"storage": "zz-pool", "type": "lvm", "content": "images"},
            {"storage": "aa-pool", "type": "lvmthin", "content": "images"},
            {"storage": "mm-pool", "type": "zfspool", "content": "images"},
        ]
        result = self._call(entries)
        self.assertEqual(result, sorted(result))

    def test_duplicates_deduplicated(self) -> None:
        entries = [
            {"storage": "mypool", "type": "lvm", "content": "images"},
            {"storage": "mypool", "type": "lvm", "content": "images"},
        ]
        self.assertEqual(self._call(entries), ["mypool"])

    def test_content_whitespace_stripped(self) -> None:
        """Comma-split parts are stripped so ' images' still matches."""
        entries = [
            {"storage": "my-dir", "type": "dir", "content": "iso, images , backup"},
        ]
        self.assertEqual(self._call(entries), ["my-dir"])

    def test_missing_content_field_excluded(self) -> None:
        entries = [
            {"storage": "bare-lvm", "type": "lvm"},
        ]
        self.assertEqual(self._call(entries), [])

    def test_disable_zero_not_excluded(self) -> None:
        """disable=0 means enabled — entry should be kept."""
        entries = [
            {"storage": "enabled-lvm", "type": "lvm", "content": "images", "disable": 0},
        ]
        self.assertEqual(self._call(entries), ["enabled-lvm"])

    def test_disable_false_not_excluded(self) -> None:
        """disable=False means enabled — entry should be kept."""
        entries = [
            {"storage": "enabled-lvm2", "type": "lvm", "content": "images", "disable": False},
        ]
        self.assertEqual(self._call(entries), ["enabled-lvm2"])

    def test_custom_types_parameter(self) -> None:
        """Caller can restrict accepted types via the types parameter."""
        entries = [
            {"storage": "my-lvm", "type": "lvm", "content": "images"},
            {"storage": "my-dir", "type": "dir", "content": "images"},
        ]
        result = _integration.select_local_disk_pools(entries, types=("lvm",))
        self.assertEqual(result, ["my-lvm"])

    def test_widened_types_include_rbd(self) -> None:
        """A types tuple naming rbd keeps rbd pools that the local-only
        default excludes."""
        entries = [
            {"storage": "ceph-rbd", "type": "rbd", "content": "images"},
            {"storage": "local-lvm", "type": "lvm", "content": "images"},
            {"storage": "nfs-data", "type": "nfs", "content": "images"},
        ]
        result = _integration.select_local_disk_pools(
            entries, types=("lvm", "rbd")
        )
        self.assertEqual(result, ["ceph-rbd", "local-lvm"])


class TestDiskStorageTypes(unittest.TestCase):
    """Tests for the tier1.disk_storage_types override plumbing."""

    def _cfg(self, tier1: dict) -> dict:
        return {"tier1": tier1}

    def test_absent_key_returns_default(self) -> None:
        self.assertEqual(
            _integration.disk_storage_types(self._cfg({})),
            _integration._LOCAL_DISK_TYPES,
        )

    def test_empty_list_returns_default(self) -> None:
        self.assertEqual(
            _integration.disk_storage_types(self._cfg({"disk_storage_types": []})),
            _integration._LOCAL_DISK_TYPES,
        )

    def test_default_excludes_shared_types(self) -> None:
        """The default stays local-only: nfs, cephfs, and rbd are not in it."""
        types = _integration.disk_storage_types(self._cfg({}))
        for shared_type in ("nfs", "cephfs", "rbd"):
            self.assertNotIn(shared_type, types)

    def test_override_returned_lowercased(self) -> None:
        cfg = self._cfg({"disk_storage_types": ["LVMThin", " rbd "]})
        self.assertEqual(_integration.disk_storage_types(cfg), ("lvmthin", "rbd"))

    def test_non_list_raises(self) -> None:
        with self.assertRaises(RuntimeError):
            _integration.disk_storage_types(
                self._cfg({"disk_storage_types": "rbd"})
            )

    def test_non_string_entry_raises(self) -> None:
        with self.assertRaises(RuntimeError):
            _integration.disk_storage_types(
                self._cfg({"disk_storage_types": ["lvm", 3]})
            )

    def test_empty_string_entry_raises(self) -> None:
        with self.assertRaises(RuntimeError):
            _integration.disk_storage_types(
                self._cfg({"disk_storage_types": ["lvm", "  "]})
            )

    def test_detect_pools_uses_override(self) -> None:
        """detect_disk_storage_pools must thread the configured types into the
        pool filter — the caller-side plumbing the default tuple bypassed."""
        entries = [
            {"storage": "ceph-rbd", "type": "rbd", "content": "images"},
            {"storage": "local-lvm", "type": "lvm", "content": "images"},
        ]
        cfg = self._cfg({"disk_storage_types": ["rbd"]})
        with unittest.mock.patch.object(
            _integration, "build_cpi_config", return_value={}
        ), unittest.mock.patch.object(
            _integration, "fetch_storage_index", return_value=entries
        ):
            self.assertEqual(_integration.detect_disk_storage_pools(cfg), ["ceph-rbd"])

    def test_detect_pools_default_stays_local(self) -> None:
        entries = [
            {"storage": "ceph-rbd", "type": "rbd", "content": "images"},
            {"storage": "local-lvm", "type": "lvm", "content": "images"},
        ]
        cfg = self._cfg({})
        with unittest.mock.patch.object(
            _integration, "build_cpi_config", return_value={}
        ), unittest.mock.patch.object(
            _integration, "fetch_storage_index", return_value=entries
        ):
            self.assertEqual(_integration.detect_disk_storage_pools(cfg), ["local-lvm"])


class TestSelectNetworkModes(unittest.TestCase):
    """Tests for _integration.select_network_modes (pure decision logic)."""

    SDN = {"vnet": "itvnet", "range": "10.250.0.0/24", "gateway": "10.250.0.1", "ip": "10.250.0.10"}

    def _call(self, **kw):
        defaults = dict(
            sdn_installed=False,
            existing_zones=[],
            sdn_cfg={},
            bridge_iface="",
            bridge_exists=False,
        )
        defaults.update(kw)
        return _integration.select_network_modes(**defaults)

    def _modes(self, passes):
        return [p["mode"] for p in passes]

    def test_nothing_capable_returns_empty(self) -> None:
        self.assertEqual(self._call(), [])

    def test_bridge_included_when_iface_free(self) -> None:
        passes = self._call(bridge_iface="vmbr9", bridge_exists=False)
        self.assertEqual(self._modes(passes), ["bridge"])
        self.assertEqual(passes[0]["cpi"], {})

    def test_bridge_skipped_when_iface_exists(self) -> None:
        passes = self._call(bridge_iface="vmbr9", bridge_exists=True)
        self.assertEqual(passes, [])

    def test_bridge_skipped_when_no_iface_configured(self) -> None:
        passes = self._call(bridge_iface="", bridge_exists=False)
        self.assertEqual(passes, [])

    def test_sdn_skipped_when_not_installed(self) -> None:
        passes = self._call(sdn_installed=False, existing_zones=["it"], sdn_cfg={**self.SDN, "zone": "it"})
        self.assertEqual(passes, [])

    def test_sdn_skipped_when_params_missing(self) -> None:
        # vnet/range/gateway/ip absent -> cannot run sdn even if installed + zone.
        passes = self._call(sdn_installed=True, existing_zones=["it"], sdn_cfg={"zone": "it"})
        self.assertEqual(passes, [])

    def test_sdn_reuse_existing_configured_zone(self) -> None:
        passes = self._call(
            sdn_installed=True, existing_zones=["it", "other"], sdn_cfg={**self.SDN, "zone": "it"}
        )
        self.assertEqual(self._modes(passes), ["sdn"])
        p = passes[0]
        self.assertEqual(p["env"], {"SDN_ZONE": "it"})
        # Reuse must pin the zone and keep auto-manage OFF (never delete it).
        self.assertEqual(p["cpi"], {"sdn_zone": "it", "sdn_auto_manage_zone": False})

    def test_sdn_adopts_first_existing_zone_when_unconfigured(self) -> None:
        passes = self._call(
            sdn_installed=True, existing_zones=["zb", "za"], sdn_cfg={**self.SDN, "zone": ""}
        )
        p = passes[0]
        self.assertEqual(p["env"], {"SDN_ZONE": "za"})  # sorted-first
        self.assertEqual(p["cpi"], {"sdn_zone": "za", "sdn_auto_manage_zone": False})

    def test_sdn_creates_configured_zone_when_absent(self) -> None:
        passes = self._call(
            sdn_installed=True, existing_zones=["other"], sdn_cfg={**self.SDN, "zone": "it"}
        )
        # Create path runs twice: vxlan default first, then the simple opt-in.
        self.assertEqual(self._modes(passes), ["sdn", "sdn-simple"])
        vxlan = passes[0]
        # SDN_ZONE_TYPE pinned empty so nothing overrides the CPI's vxlan default;
        # auto-manage ON, sdn_zone left unset so teardown isn't pinned.
        self.assertEqual(vxlan["env"], {"SDN_ZONE": "it", "SDN_ZONE_TYPE": ""})
        self.assertEqual(vxlan["cpi"], {"sdn_auto_manage_zone": True})

    def test_sdn_simple_optin_pass_shape(self) -> None:
        passes = self._call(
            sdn_installed=True, existing_zones=[], sdn_cfg={**self.SDN, "zone": "it"}
        )
        simple = passes[1]
        self.assertEqual(simple["mode"], "sdn-simple")
        self.assertEqual(simple["env"], {"SDN_ZONE": "it", "SDN_ZONE_TYPE": "simple"})
        self.assertEqual(
            simple["cpi"], {"sdn_zone_type": "simple", "sdn_auto_manage_zone": True}
        )

    def test_sdn_reuse_emits_single_pass_no_zone_type(self) -> None:
        # Pre-existing zone: one pass only; the zone's actual type governs, so
        # no zone_type override may be injected.
        passes = self._call(
            sdn_installed=True, existing_zones=["it"], sdn_cfg={**self.SDN, "zone": "it"}
        )
        self.assertEqual(self._modes(passes), ["sdn"])
        self.assertNotIn("sdn_zone_type", passes[0]["cpi"])
        self.assertNotIn("SDN_ZONE_TYPE", passes[0]["env"])

    def test_sdn_skipped_when_no_zone_and_none_exist(self) -> None:
        passes = self._call(
            sdn_installed=True, existing_zones=[], sdn_cfg={**self.SDN, "zone": ""}
        )
        self.assertEqual(passes, [])

    def test_both_modes_when_all_capable(self) -> None:
        passes = self._call(
            sdn_installed=True,
            existing_zones=["it"],
            sdn_cfg={**self.SDN, "zone": "it"},
            bridge_iface="vmbr9",
            bridge_exists=False,
        )
        self.assertEqual(sorted(self._modes(passes)), ["bridge", "sdn"])


class TestCfScripts_UnstickAgent_RejectsNonIntCID(unittest.TestCase):
    """_coerce_vmid must reject non-integer CIDs before any subprocess call."""

    def setUp(self):
        self._cf = _load_cf_module()

    # -- parametrized rejection cases ----------------------------------------

    def test_invalid_cid_values_raise_value_error(self) -> None:
        """All invalid VMID strings raise ValueError; subprocess is never called."""
        cases = [
            # (label, cid, required_fragment_in_message)
            ("pure alpha", "abc", "integer"),
            ("shell injection", "abc; rm -rf /", "integer"),
            ("empty string", "", None),
            ("float string", "1.5", None),
            ("zero", "0", "positive"),
            ("negative", "-1", "positive"),
        ]
        for label, cid, required_fragment in cases:
            with self.subTest(cid=repr(cid)):
                with self.assertRaises(ValueError) as ctx:
                    self._cf._coerce_vmid(cid)
                if required_fragment:
                    self.assertIn(
                        required_fragment,
                        str(ctx.exception).lower(),
                        msg=f"{label!r}: expected {required_fragment!r} in exception message",
                    )

    def test_invalid_cid_subprocess_never_called(self) -> None:
        """subprocess.run is never invoked for any invalid VMID."""
        invalid_cids = ["abc; rm -rf /", "", "1.5", "0", "-1", "abc"]
        for cid in invalid_cids:
            with self.subTest(cid=repr(cid)):
                with unittest.mock.patch("subprocess.run") as mock_run:
                    with self.assertRaises(ValueError):
                        self._cf._coerce_vmid(cid)
                    mock_run.assert_not_called()

    # -- valid VMID ----------------------------------------------------------

    def test_valid_integer_returns_int(self) -> None:
        """A valid positive integer string should pass through as int."""
        result = self._cf._coerce_vmid("101")
        self.assertEqual(result, 101)
        self.assertIsInstance(result, int)

    def test_cmd_unstick_agent_non_int_cid_exits_without_subprocess(self) -> None:
        """cmd_unstick_agent exits cleanly when bosh vms returns a non-integer CID.

        subprocess.run must never be called with the malformed CID value.
        """
        cf = self._cf

        def _fake_capture(*cmd):
            # Simulate 'bosh vms' returning a line with a non-integer CID.
            if "vms" in cmd:
                return "uaa/0  abc; rm -rf /"
            # 'bosh int' for pve_host — should not be reached.
            return "pve.example.com"  # pragma: no cover

        with unittest.mock.patch.object(cf, "capture", side_effect=_fake_capture):
            with unittest.mock.patch("subprocess.run") as mock_run:
                with self.assertRaises(SystemExit) as ctx:
                    cf.cmd_unstick_agent(["uaa/0"])
                # Exit must be non-zero (string message triggers sys.exit(msg)).
                self.assertNotEqual(ctx.exception.code, 0)
                # subprocess.run must never have been called with the raw CID.
                for call in mock_run.call_args_list:
                    args = call.args[0] if call.args else []
                    joined = " ".join(str(a) for a in args)
                    self.assertNotIn("rm -rf", joined)


class TestCfScripts_CheckUaaDb_RaisesOnSSHFailure(unittest.TestCase):
    """cmd_check_uaa_db must raise RuntimeError when bosh ssh returns rc != 0."""

    def setUp(self):
        self._cf = _load_cf_module()
        # cmd_check_uaa_db gates the probe behind _cf_has_database_vm(), which
        # runs its own `bosh vms`. These tests exercise the probe's SSH-failure
        # handling, so force the precondition True; otherwise the globally
        # mocked subprocess.run feeds the gate a fake result, it reads "no
        # database VM", and the probe path under test is skipped entirely.
        gate = unittest.mock.patch.object(
            self._cf, "_cf_has_database_vm", return_value=True
        )
        gate.start()
        self.addCleanup(gate.stop)

    def _make_completed_process(self, returncode: int, stdout: str = "", stderr: str = ""):
        import subprocess as _sp
        result = _sp.CompletedProcess(args=[], returncode=returncode)
        result.stdout = stdout
        result.stderr = stderr
        return result

    def test_nonzero_rc_raises_runtime_error(self) -> None:
        """Any non-zero returncode from bosh ssh raises RuntimeError."""
        cases = [
            # (label, returncode, stderr, expected_in_msg)
            (
                "rc=255 ssh unreachable",
                255,
                "ssh: connect to host database.example.com port 22: Connection refused",
                ["255", "bosh ssh"],
            ),
            (
                "rc=1 deployment not found",
                1,
                "Error: deployment 'cf' not found",
                ["1"],
            ),
            (
                "rc=2 general failure",
                2,
                "general error",
                ["2"],
            ),
        ]
        for label, rc, stderr_text, expected_fragments in cases:
            with self.subTest(label=label):
                fake = self._make_completed_process(returncode=rc, stderr=stderr_text)
                with unittest.mock.patch("subprocess.run", return_value=fake):
                    with self.assertRaises(RuntimeError) as ctx:
                        self._cf.cmd_check_uaa_db([])
                    msg = str(ctx.exception).lower()
                    for frag in expected_fragments:
                        self.assertIn(frag.lower(), msg, msg=f"{label}: expected {frag!r} in error message")

    def test_zero_rc_recognized_tokens_return_normally(self) -> None:
        """rc=0 with recognized output tokens must not raise."""
        cases = [
            ("SKIP_NO_PXC token", "SKIP_NO_PXC\n"),
            ("OK_FRESH token", "OK_FRESH\n"),
        ]
        for label, stdout_text in cases:
            with self.subTest(label=label):
                fake = self._make_completed_process(returncode=0, stdout=stdout_text)
                with unittest.mock.patch("subprocess.run", return_value=fake):
                    # Must return without raising.
                    self._cf.cmd_check_uaa_db([])

    def test_zero_rc_error_conditions_raise_runtime_error(self) -> None:
        """rc=0 with error indicators in output must raise RuntimeError."""
        cases = [
            ("PROBE_ERROR token", "PROBE_ERROR: Access denied\n", "PROBE_ERROR"),
            ("unparseable output", "completely unexpected output\n", "parsed"),
        ]
        for label, stdout_text, expected_fragment in cases:
            with self.subTest(label=label):
                fake = self._make_completed_process(returncode=0, stdout=stdout_text)
                with unittest.mock.patch("subprocess.run", return_value=fake):
                    with self.assertRaises(RuntimeError) as ctx:
                        self._cf.cmd_check_uaa_db([])
                    self.assertIn(
                        expected_fragment.lower(),
                        str(ctx.exception).lower(),
                        msg=f"{label}: expected {expected_fragment!r} in error message",
                    )


def _load_module_no_ext(name: str, filepath: str):
    """Load a Python script that has no .py extension via SourceFileLoader."""
    loader = _ilm.SourceFileLoader(name, filepath)
    spec = _ilu.spec_from_loader(name, loader, origin=filepath)
    mod = _ilu.module_from_spec(spec)
    mod.__file__ = filepath
    loader.exec_module(mod)
    return mod


def _load_bosh_module():
    bosh_path = str(Path(__file__).resolve().parent / "bosh")
    return _load_module_no_ext("bosh_script", bosh_path)


def _load_test_module():
    test_path = str(Path(__file__).resolve().parent / "test")
    return _load_module_no_ext("test_script", test_path)


class TestBoshStateIsEmpty(unittest.TestCase):
    """Unit tests for scripts/bosh _state_is_empty.

    Covers:
      - absent file             -> True
      - empty / whitespace-only -> True
      - bare '{}'               -> True
      - JSON without bosh/deployments keys -> True
      - populated with 'bosh' key          -> False
      - populated with 'deployments' key   -> False
      - invalid JSON                       -> False (conservative: don't skip teardown)
      - JSON array                         -> False
    """

    def setUp(self):
        self._bosh = _load_bosh_module()

    def _write_state(self, content: str, tmpdir: Path) -> None:
        state_file = tmpdir / "state.json"
        state_file.write_text(content, encoding="utf-8")
        self._bosh.STATE = state_file

    def test_absent_file_returns_true(self) -> None:
        """Non-existent state file is empty."""
        with tempfile.TemporaryDirectory() as d:
            self._bosh.STATE = Path(d) / "state.json"
            self.assertTrue(self._bosh._state_is_empty())

    def test_file_contents_classified_as_empty(self) -> None:
        """File contents that represent an absent/empty BOSH state return True."""
        cases = [
            ("empty string", ""),
            ("whitespace only", "   \n  "),
            ("bare empty object", "{}"),
            ("json with unrelated key", '{"other": "data"}'),
        ]
        for label, content in cases:
            with self.subTest(label=label):
                with tempfile.TemporaryDirectory() as d:
                    self._write_state(content, Path(d))
                    self.assertTrue(
                        self._bosh._state_is_empty(),
                        msg=f"{label!r}: expected _state_is_empty()=True",
                    )

    def test_file_contents_classified_as_non_empty(self) -> None:
        """File contents that indicate real BOSH state return False."""
        cases = [
            (
                "populated bosh key",
                '{"bosh": {"type": "create-env"}, "current_manifest_sha1": "abc"}',
            ),
            (
                "populated deployments key",
                '{"deployments": [{"name": "bosh"}]}',
            ),
            (
                "invalid json",
                "{not valid json",
            ),
            (
                "json array not dict",
                "[]",
            ),
        ]
        for label, content in cases:
            with self.subTest(label=label):
                with tempfile.TemporaryDirectory() as d:
                    self._write_state(content, Path(d))
                    self.assertFalse(
                        self._bosh._state_is_empty(),
                        msg=f"{label!r}: expected _state_is_empty()=False",
                    )


class TestRunWithRetries(unittest.TestCase):
    """Unit tests for scripts/test run_with_retries.

    run_with_retries wraps run() which in turn calls subprocess.run.
    We patch run() directly on the loaded module to control return codes
    without spawning real subprocesses.
    """

    def setUp(self):
        self._test_mod = _load_test_module()

    def test_retry_count_and_return_code_matrix(self) -> None:
        """Parametrized: verify call count and final rc for various retry/rc combinations."""
        cases = [
            # (label, run_returns_sequence, retries, expected_rc, expected_call_count)
            ("success on first attempt", [0], 2, 0, 1),
            ("success after one failure", [1, 0], 1, 0, 2),
            ("all attempts fail rc=42", [42, 42, 42], 2, 42, 3),
            ("no retries returns immediately", [7], 0, 7, 1),
        ]
        for label, run_returns, retries, expected_rc, expected_calls in cases:
            with self.subTest(label=label):
                mod = _load_test_module()
                with unittest.mock.patch.object(mod, "run", side_effect=run_returns) as mock_run:
                    with unittest.mock.patch.object(mod, "_dry_run", False):
                        with unittest.mock.patch("time.sleep"):
                            rc = mod.run_with_retries(["cmd"], retries=retries, delay=0)
                self.assertEqual(rc, expected_rc, msg=f"{label}: rc mismatch")
                self.assertEqual(mock_run.call_count, expected_calls, msg=f"{label}: call_count mismatch")

    def test_dry_run_mode_returns_zero_without_calling_run(self) -> None:
        """When _dry_run is True, run() prints and returns 0 without executing."""
        test_mod = _load_test_module()
        test_mod._dry_run = True
        with unittest.mock.patch.object(test_mod, "run", wraps=test_mod.run) as mock_run:
            rc = test_mod.run_with_retries(["echo", "dry"], retries=1, delay=0)
        # run() is still called (it handles dry_run internally), but returns 0
        # without spawning a subprocess; rc must be 0.
        self.assertEqual(rc, 0)

    def test_label_used_in_retry_message(self) -> None:
        """label= kwarg appears in the stderr retry message instead of the raw cmd."""
        import io
        buf = io.StringIO()
        with unittest.mock.patch.object(
            self._test_mod, "run", side_effect=[1, 0]
        ):
            with unittest.mock.patch.object(self._test_mod, "_dry_run", False):
                with unittest.mock.patch("time.sleep"):
                    with unittest.mock.patch("sys.stderr", buf):
                        self._test_mod.run_with_retries(
                            ["my-cmd", "arg"], retries=1, delay=0, label="my-label"
                        )
        self.assertIn("my-label", buf.getvalue())


if __name__ == "__main__":
    unittest.main()
