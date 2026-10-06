#!/usr/bin/env python3
"""Unit tests for scripts/_upgrade_checks.py and scripts/bosh check-vars.

Run with:
    python3 scripts/_upgrade_checks_test.py

Pure parsing tests use only stdlib. The interpolation tests run `bosh int`
offline against manifests written to a temporary directory, and skip when the
bosh CLI is not on PATH. Shell checks of the marker scripts skip without bash
or shellcheck.
"""

from __future__ import annotations

import base64
import importlib.machinery as _ilm
import importlib.util as _ilu
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

SCRIPTS = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS))
import _upgrade_checks as uc  # noqa: E402

NONCE = "0123456789abcdef0123456789abcdef"
SHA = "a" * 64


class ParseMissingVarsTest(unittest.TestCase):
    def test_list_form(self) -> None:
        stderr = (
            "Evaluating manifest:\n"
            "  - Expected to find variables:\n"
            "      - pve_parker_pool\n"
            "      - pve_parker_prefix\n"
            "\n"
            "Exit code 1\n"
        )
        self.assertEqual(uc.parse_missing_vars(stderr),
                         ["pve_parker_pool", "pve_parker_prefix"])

    def test_inline_form(self) -> None:
        stderr = "Expected to find variables: pve_parker_pool, pve_parker_prefix\n"
        self.assertEqual(uc.parse_missing_vars(stderr),
                         ["pve_parker_pool", "pve_parker_prefix"])

    def test_other_errors_report_nothing(self) -> None:
        self.assertEqual(uc.parse_missing_vars("Opening file ops.yml: no such file\n"), [])

    def test_names_are_deduplicated(self) -> None:
        stderr = "Expected to find variables:\n    - a\n    - a\n"
        self.assertEqual(uc.parse_missing_vars(stderr), ["a"])


class ExclusionTest(unittest.TestCase):
    def test_declared_names(self) -> None:
        variables = [{"name": "admin_password", "type": "password"},
                     {"name": "director_ssl", "type": "certificate"}, "junk"]
        self.assertEqual(uc.declared_variable_names(variables),
                         {"admin_password", "director_ssl"})
        self.assertEqual(uc.declared_variable_names(None), set())

    def test_self_supplied_and_declared_are_not_missing(self) -> None:
        reported = ["director_name", "pve_cpi_release_path", "pve_cpi_release_name",
                    "admin_password", "pve_parker_pool"]
        self.assertEqual(uc.truly_missing(reported, {"admin_password"}),
                         ["pve_parker_pool"])


class MarkerTest(unittest.TestCase):
    def test_write_script_embeds_the_nonce(self) -> None:
        script = uc.write_marker_script(NONCE)
        self.assertIn(NONCE, script)
        self.assertIn("mountpoint -q", script)
        self.assertIn("sync", script)

    def test_write_script_rejects_a_bad_nonce(self) -> None:
        with self.assertRaises(ValueError):
            uc.write_marker_script("not-hex; rm -rf /")

    def test_remote_command_round_trips(self) -> None:
        script = uc.read_marker_script()
        cmd = uc.remote_command(script)
        self.assertTrue(cmd.startswith("echo "))
        self.assertTrue(cmd.endswith(" | base64 -d | sudo bash"))
        encoded = cmd.split()[1]
        self.assertEqual(base64.b64decode(encoded).decode(), script)

    def test_parse_marker_from_prefixed_bosh_ssh_output(self) -> None:
        out = (
            "Using deployment 'certification'\n"
            f"simple/abc: stdout | MARKER nonce={NONCE} sha256={SHA}\r\n"
            "simple/abc: stderr | Connection to 192.0.2.30 closed.\n"
            "Succeeded\n"
        )
        self.assertEqual(uc.parse_marker(out), (NONCE, SHA))

    def test_parse_marker_absent(self) -> None:
        self.assertIsNone(uc.parse_marker("Succeeded\n"))

    def test_marker_error(self) -> None:
        out = "simple/abc: stdout | MARKER-ERROR /var/vcap/store is not a mounted persistent disk\n"
        self.assertEqual(uc.marker_error(out), "/var/vcap/store is not a mounted persistent disk")

    @unittest.skipUnless(shutil.which("bash"), "bash is not on PATH")
    def test_scripts_parse_under_bash(self) -> None:
        for script in (uc.write_marker_script(NONCE), uc.read_marker_script()):
            proc = subprocess.run(["bash", "-n"], input=script, text=True,
                                  capture_output=True)
            self.assertEqual(proc.returncode, 0, proc.stderr)

    @unittest.skipUnless(shutil.which("shellcheck"), "shellcheck is not on PATH")
    def test_scripts_pass_shellcheck(self) -> None:
        for script in (uc.write_marker_script(NONCE), uc.read_marker_script()):
            proc = subprocess.run(["shellcheck", "-s", "bash", "-"], input=script,
                                  text=True, capture_output=True)
            self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)

    @unittest.skipUnless(shutil.which("bash") and shutil.which("sha256sum")
                         and shutil.which("mountpoint"), "needs bash, sha256sum, mountpoint")
    def test_scripts_refuse_an_unmounted_store(self) -> None:
        proc = subprocess.run(["bash"], input=uc.read_marker_script(), text=True,
                              capture_output=True)
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("MARKER-ERROR", proc.stdout)


class SerialTest(unittest.TestCase):
    CONFIG = {
        "scsi0": "local-lvm:vm-101-disk-0,size=8G",
        "scsi1": "local-lvm:vm-101-disk-1,serial=bpd-00112233aabbccdd,size=10G",
        "unused0": "local-lvm:vm-101-disk-9,serial=bpd-ffff000011112222",
        "net0": "virtio=AA:BB:CC:DD:EE:FF,bridge=cpitest0",
        "virtio2": "ceph:vm-101-disk-3,serial=bpd-1234123412341234",
    }

    def test_drive_serials_skip_unused_and_non_drives(self) -> None:
        self.assertEqual(uc.drive_serials(self.CONFIG), {
            "scsi1": "bpd-00112233aabbccdd",
            "virtio2": "bpd-1234123412341234",
        })

    def _section(self, vmid: str, drive: "str | None") -> dict:
        ids = {"pvd-cid": "bpd-00112233aabbccdd"}
        serials = {vmid: {drive: "bpd-00112233aabbccdd"}} if drive else {vmid: {}}
        return {"stable_ids": ids, "locations": uc.serial_locations(ids, serials)}

    def test_locations(self) -> None:
        section = self._section("101", "scsi1")
        self.assertEqual(section["locations"], {"pvd-cid": "101/scsi1"})
        self.assertEqual(uc.describe_serials(section["stable_ids"], section["locations"]),
                         "bpd-00112233aabbccdd on 101/scsi1")

    def test_stable_across_a_vm_rebuild(self) -> None:
        held, why = uc.serials_stable(self._section("101", "scsi1"),
                                      self._section("202", "scsi2"))
        self.assertTrue(held, why)

    def test_serial_lost_after_upgrade(self) -> None:
        held, why = uc.serials_stable(self._section("101", "scsi1"),
                                      self._section("202", None))
        self.assertFalse(held)
        self.assertIn("after the upgrade", why)

    def test_non_bpd_stable_id_fails(self) -> None:
        pre = {"stable_ids": {"pvd-cid": ""}, "locations": {"pvd-cid": ""}}
        held, why = uc.serials_stable(pre, pre)
        self.assertFalse(held)
        self.assertIn("bpd-", why)

    def test_nothing_captured_fails(self) -> None:
        self.assertFalse(uc.serials_stable({}, {})[0])

    def test_changed_disk_set_fails(self) -> None:
        post = self._section("202", "scsi2")
        post["stable_ids"] = {"pvd-other": "bpd-00112233aabbccdd"}
        self.assertFalse(uc.serials_stable(self._section("101", "scsi1"), post)[0])


@unittest.skipUnless(shutil.which("bosh"), "bosh CLI is not on PATH")
class BoshIntOutputTest(unittest.TestCase):
    """parse_missing_vars reads what this bosh CLI actually prints."""

    def test_real_var_errs_output(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            m = Path(tmp) / "m.yml"
            m.write_text("a: ((alpha))\nb: ((beta))\nc: ((gamma))\n")
            v = Path(tmp) / "v.yml"
            v.write_text("alpha: 1\n")
            proc = subprocess.run(["bosh", "int", str(m), "-l", str(v), "--var-errs"],
                                  capture_output=True, text=True)
        self.assertNotEqual(proc.returncode, 0)
        self.assertEqual(uc.parse_missing_vars(proc.stderr + proc.stdout), ["beta", "gamma"])


def _load_bosh_script(environ: dict) -> object:
    path = str(SCRIPTS / "bosh")
    with mock.patch.dict(os.environ, environ):
        if "BOSH_STATE_DIR" not in environ:
            os.environ.pop("BOSH_STATE_DIR", None)
        loader = _ilm.SourceFileLoader("bosh_check_vars_under_test", path)
        spec = _ilu.spec_from_file_location("bosh_check_vars_under_test", path, loader=loader)
        module = _ilu.module_from_spec(spec)
        loader.exec_module(module)
    return module


@unittest.skipUnless(shutil.which("bosh"), "bosh CLI is not on PATH")
class CheckVarsTest(unittest.TestCase):
    """scripts/bosh check-vars against fixture manifests, fully offline."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.director = root / "bosh.yml"
        self.director.write_text(
            "name: ((director_name))\n"
            "release: ((pve_cpi_release_path))\n"
            "cpi_name: ((pve_cpi_release_name))\n"
            "ip: ((internal_ip))\n"
            "parker_prefix: ((pve_parker_prefix))\n"
            "parker_pool: ((pve_parker_pool))\n"
            "admin: ((admin_password))\n"
            "variables:\n"
            "- name: admin_password\n"
            "  type: password\n"
        )
        self.cloud = root / "cloud-config.yml"
        self.cloud.write_text("bridge: ((pve_network_bridge))\ncores: ((certification_vm_cores))\n")
        self.stale = root / "vars.yml"
        self.stale.write_text("internal_ip: 192.0.2.10\npve_network_bridge: cpitest0\n")
        self.fresh = root / "fresh.yml"
        self.fresh.write_text(
            "internal_ip: 192.0.2.10\npve_network_bridge: cpitest0\n"
            "pve_parker_prefix: ''\npve_parker_pool: '{prefix}-parker'\n"
        )

    def _run(self, vars_file: Path, ucc_args: list[str]) -> "tuple[int, str]":
        mod = _load_bosh_script({})
        mod.VARS = vars_file
        mod.CLOUD_CONFIG = self.cloud
        mod.bosh_deployment_dir = lambda: Path(self.tmp.name)
        mod._director_manifest_argv = lambda bd: [str(self.director), "-l", str(mod.VARS)]
        mod.env_network_ops = lambda: []
        mod.env_cc_reserved_ops = lambda: []
        mod.env_vars_layer = lambda: []
        mod.env_vars_file = lambda: None
        mod.require_slot = lambda action: None
        err = []
        with mock.patch("sys.stderr") as fake_err, mock.patch("sys.stdout"):
            fake_err.write.side_effect = err.append
            rc = mod.cmd_check_vars(ucc_args)
        return rc, "".join(err)

    def test_stale_vars_name_each_missing_variable(self) -> None:
        rc, err = self._run(self.stale, ["-v", "certification_vm_cores=2"])
        self.assertEqual(rc, 1)
        self.assertIn("- pve_parker_pool", err)
        self.assertIn("- pve_parker_prefix", err)
        for supplied in ("director_name", "pve_cpi_release_path",
                         "pve_cpi_release_name", "admin_password"):
            self.assertNotIn(f"- {supplied}\n", err)
        self.assertIn("LAB_BOSH_VARS_YML", err)

    def test_fresh_vars_pass(self) -> None:
        rc, err = self._run(self.fresh, ["-v", "certification_vm_cores=2"])
        self.assertEqual(rc, 0, err)

    def test_cloud_config_flags_count(self) -> None:
        rc, err = self._run(self.fresh, [])
        self.assertEqual(rc, 1)
        self.assertIn("- certification_vm_cores", err)


if __name__ == "__main__":
    unittest.main(verbosity=2)
