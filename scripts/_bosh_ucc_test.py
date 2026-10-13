#!/usr/bin/env python3
"""Tests for the cpi-config AZ binding that `scripts/bosh ucc` layers on.

Offline: subprocess.run and the module's run are mocked, so no test reaches a
Director. The last test runs `bosh int` on the real manifests and is skipped
when the bosh CLI is not installed.
"""

from __future__ import annotations

import importlib.machinery
import importlib.util
import os
import shutil
import subprocess
import unittest
from pathlib import Path
from unittest import mock

SCRIPTS = Path(__file__).resolve().parent
REPO_ROOT = SCRIPTS.parent
OPS_FILE = REPO_ROOT / "manifests" / "bosh" / "multi-cpi-azs.yml"
CLOUD_CONFIG = REPO_ROOT / "manifests" / "bosh" / "cloud-config.yml"


def load_bosh() -> object:
    path = str(SCRIPTS / "bosh")
    with mock.patch.dict(os.environ):
        os.environ.pop("BOSH_STATE_DIR", None)
        loader = importlib.machinery.SourceFileLoader("bosh_ucc_under_test", path)
        spec = importlib.util.spec_from_loader("bosh_ucc_under_test", loader)
        module = importlib.util.module_from_spec(spec)
        loader.exec_module(module)
    return module


def completed(returncode: int, stdout: str = "") -> subprocess.CompletedProcess:
    return subprocess.CompletedProcess([], returncode, stdout=stdout, stderr="")


class MultiCpiAzsOpsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.bosh = load_bosh()
        patcher = mock.patch.object(self.bosh, "director_env", return_value={})
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_layer_returned_when_cpi_config_has_cpis(self) -> None:
        with mock.patch.object(
            self.bosh.subprocess, "run", return_value=completed(0, "cpis:\n- name: pve-az1\n")
        ) as run, mock.patch("builtins.print") as printed:
            ops = self.bosh.multi_cpi_azs_ops()
        printed.assert_called_once()
        self.assertEqual(ops, ["-o", str(OPS_FILE)])
        self.assertEqual(run.call_args.args[0][-1], "cpi-config")

    def test_empty_on_nonzero_exit(self) -> None:
        with mock.patch.object(self.bosh.subprocess, "run", return_value=completed(1, "cpis:\n")):
            self.assertEqual(self.bosh.multi_cpi_azs_ops(), [])

    def test_empty_when_output_lacks_cpis(self) -> None:
        with mock.patch.object(self.bosh.subprocess, "run", return_value=completed(0, "No CPI config\n")):
            self.assertEqual(self.bosh.multi_cpi_azs_ops(), [])


class CmdUccTest(unittest.TestCase):
    def test_layer_precedes_vars_and_extra_args_stay_last(self) -> None:
        bosh = load_bosh()
        layer = ["-o", str(OPS_FILE)]
        with mock.patch.object(bosh, "director_env", return_value={}), \
                mock.patch.object(bosh, "env_network_ops", return_value=[]), \
                mock.patch.object(bosh, "env_cc_reserved_ops", return_value=["-o", "reserved.yml"]), \
                mock.patch.object(bosh, "env_vars_layer", return_value=[]), \
                mock.patch.object(bosh, "SLOT", mock.Mock(vars_layer=lambda: [])), \
                mock.patch.object(bosh, "multi_cpi_azs_ops", return_value=layer), \
                mock.patch.object(bosh, "run", return_value=0) as run:
            rc = bosh.cmd_ucc(["--dry-run"])
        self.assertEqual(rc, 0)
        cmd = list(run.call_args.args)
        self.assertLess(cmd.index("reserved.yml"), cmd.index(str(OPS_FILE)))
        self.assertLess(cmd.index(str(OPS_FILE)), cmd.index("-l"))
        self.assertEqual(cmd[-1], "--dry-run")


class CloudConfigBindingTest(unittest.TestCase):
    @unittest.skipUnless(shutil.which("bosh"), "bosh CLI not installed")
    def test_bosh_int_binds_z1_to_pve_az1(self) -> None:
        res = subprocess.run(
            [
                "bosh", "int", str(CLOUD_CONFIG), "-o", str(OPS_FILE),
                "--path", "/azs/name=z1/cpi",
            ],
            capture_output=True, text=True,
        )
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(res.stdout.strip(), "pve-az1")


if __name__ == "__main__":
    unittest.main()
