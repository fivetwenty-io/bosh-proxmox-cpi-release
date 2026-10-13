#!/usr/bin/env python3
"""Offline unit tests for the release-sourcing order and manifest preflight in scripts/cf.

Run with:
    python3 scripts/_cf_deploy_test.py

Uses only stdlib. Every subprocess call and stemcell lookup is mocked, so no test
calls bosh, the Director, or a lab.
"""

from __future__ import annotations

import importlib.machinery as _ilm
import importlib.util as _ilu
import sys
import tempfile
import types as _types
import unittest
import unittest.mock as mock
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _cfcompile


def _load_cf_module():
    cf_path = str(Path(__file__).resolve().parent / "cf")
    stub = _types.ModuleType("requests")
    stub.get = lambda *a, **kw: None  # type: ignore[attr-defined]
    had = "requests" in sys.modules
    if not had:
        sys.modules["requests"] = stub
    try:
        loader = _ilm.SourceFileLoader("cf_script_deploy", cf_path)
        spec = _ilu.spec_from_loader("cf_script_deploy", loader, origin=cf_path)
        mod = _ilu.module_from_spec(spec)
        mod.__file__ = cf_path
        loader.exec_module(mod)
    finally:
        if not had:
            sys.modules.pop("requests", None)
    return mod


cf = _load_cf_module()

CFD = Path("/cfd")
UCR = str(CFD / "operations/use-compiled-releases.yml")
PG = str(CFD / "operations/use-postgres.yml")


class OrderTests(unittest.TestCase):
    def _ops(self, state: str, database: str, require: str | None = None) -> list[str]:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        compiled = Path(tmp.name) / "compiled-releases.yml"
        compiled.write_text("x")
        self.compiled = str(compiled)
        env = {} if require is None else {"CF_REQUIRE_PRECOMPILE": require}
        with mock.patch.object(cf, "stemcell_os_version", return_value=("ubuntu-noble", "1.2")), \
                mock.patch.object(cf, "COMPILED_OPS_CF", compiled), \
                mock.patch.object(cf, "CF_DATABASE", database), \
                mock.patch.object(cf, "cf_deployment_dir", return_value=CFD), \
                mock.patch.object(cf._cfcompile, "precompile_state", return_value=state), \
                mock.patch.dict(cf.os.environ, env, clear=False):
            if require is None:
                cf.os.environ.pop("CF_REQUIRE_PRECOMPILE", None)
            return cf._cf_db_and_release_ops()

    def test_fallback_goes_before_postgres(self) -> None:
        ops = self._ops("absent", "postgres", "0")
        self.assertEqual(ops, ["-o", UCR, "-o", PG])

    def test_fallback_with_pxc_adds_only_stock_file(self) -> None:
        self.assertEqual(self._ops("absent", "pxc", "0"), ["-o", UCR])

    def test_store_ops_goes_after_postgres(self) -> None:
        ops = self._ops("store-ops", "postgres")
        self.assertEqual(ops, ["-o", PG, "-o", self.compiled])

    def test_store_ops_with_pxc(self) -> None:
        self.assertEqual(self._ops("store-ops", "pxc"), ["-o", self.compiled])

    def test_director_cache_adds_nothing_for_releases(self) -> None:
        self.assertEqual(self._ops("director-cache", "postgres"), ["-o", PG])
        self.assertEqual(self._ops("director-cache", "pxc"), [])

    def test_fallback_still_exits_when_precompile_required(self) -> None:
        with self.assertRaises(SystemExit):
            self._ops("absent", "postgres")
        with self.assertRaises(SystemExit):
            self._ops("mismatch", "pxc", "1")


class PreflightTests(unittest.TestCase):
    CMD = ["bosh", "-e", "e", "-n", "-d", "cf", "deploy", "/cfd/cf-deployment.yml",
           "-o", "a.yml", "-l", "v.yml", "-v", "k=v"]

    def test_only_allowed_flags_are_carried(self) -> None:
        cmd = self.CMD + ["--recreate", "--fix", "--skip-drain", "web", "-o", "x.yml", "-o=y.yml",
                          "--var=a=b", "--vars-file", "f.yml", "--var-file=g=h", "--ops-file", "z.yml"]
        got = cf._manifest_eval_args(cmd)
        self.assertEqual(got, ["/cfd/cf-deployment.yml", "-o", "a.yml", "-l", "v.yml", "-v", "k=v",
                               "-o", "x.yml", "-o=y.yml", "--var=a=b", "--vars-file", "f.yml",
                               "--var-file=g=h", "--ops-file", "z.yml"])
        for dropped in ("--recreate", "--fix", "--skip-drain", "-n", "-d", "-e"):
            self.assertNotIn(dropped, got)

    def test_preflight_runs_bosh_int_without_var_errs(self) -> None:
        ok = _types.SimpleNamespace(returncode=0, stdout="", stderr="")
        with mock.patch.object(cf.subprocess, "run", return_value=ok) as run:
            cf._preflight_manifest(self.CMD + ["--recreate"])
        argv = run.call_args.args[0]
        self.assertEqual(argv[:3], ["bosh", "int", "/cfd/cf-deployment.yml"])
        self.assertNotIn("--var-errs", argv)
        self.assertNotIn("--recreate", argv)

    def test_failing_preflight_exits_with_error(self) -> None:
        bad = _types.SimpleNamespace(returncode=1, stdout="", stderr="Expected to find exactly one")
        with mock.patch.object(cf.subprocess, "run", return_value=bad):
            with self.assertRaises(SystemExit) as ctx:
                cf._preflight_manifest(self.CMD)
        self.assertIn("Expected to find exactly one", str(ctx.exception))

    def test_failing_preflight_never_enters_retry_loop(self) -> None:
        with mock.patch.object(cf, "stemcell_os_version", return_value=("ubuntu-noble", "1.2")), \
                mock.patch.object(cf, "wait_agents_responsive") as wait, \
                mock.patch.object(cf, "cf_deployment_dir", return_value=CFD), \
                mock.patch.object(cf, "_cf_db_and_release_ops", return_value=[]), \
                mock.patch.object(cf, "_env_vars_layer", return_value=[]), \
                mock.patch.object(cf, "_preflight_manifest", side_effect=SystemExit("bad")), \
                mock.patch.object(cf, "_run_deploy_with_watchdog") as dog, \
                mock.patch.object(cf.subprocess, "run") as run, \
                mock.patch.object(cf.time, "sleep") as sleep, \
                mock.patch.dict(cf.os.environ, {"SCRIPTS_CF_AGENT_WAIT": "0"}):
            with self.assertRaises(SystemExit):
                cf.cmd_bosh_deploy(["--recreate"])
        dog.assert_not_called()
        run.assert_not_called()
        sleep.assert_not_called()
        wait.assert_not_called()


if __name__ == "__main__":
    unittest.main()
