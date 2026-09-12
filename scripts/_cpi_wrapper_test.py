"""Execute the CPI wrapper against BOSH CLI's package-location contract."""

import os
import pathlib
import subprocess
import tempfile
import unittest

TEMPLATE = pathlib.Path(__file__).resolve().parents[1] / "jobs/pve_cpi/templates/cpi.erb"


class WrapperContract(unittest.TestCase):
    def test_package_locations_and_request_forwarding(self):
        for mode in ("default", "custom", "missing", "nonexecutable"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory(prefix="CPI wrapper ") as tmp:
                base = pathlib.Path(tmp)
                job = base / "install/jobs/pve_cpi"
                (job / "bin").mkdir(parents=True)
                (job / "config").mkdir()
                launch = job / "bin/cpi"
                launch.write_bytes(TEMPLATE.read_bytes())
                packages = base / "install/packages"
                custom = base / "custom packages"
                chosen = custom if mode in ("custom", "nonexecutable") else packages
                (chosen / "pve_cpi/bin").mkdir(parents=True)
                binary = chosen / "pve_cpi/bin/cpi"
                binary.write_text('#!/bin/bash\nprintf "%s\\n" "$@"\ncat\n')
                binary.chmod(0o600 if mode == "nonexecutable" else 0o700)
                env = dict(os.environ)
                env.pop("BOSH_PACKAGES_DIR", None)
                if mode != "default":
                    env["BOSH_PACKAGES_DIR"] = str(custom)
                result = subprocess.run(
                    ["bash", str(launch)], input="request-body\n", text=True,
                    capture_output=True, env=env, check=False,
                )
                if mode in ("missing", "nonexecutable"):
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn("request-body", result.stdout)
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(
                        result.stdout,
                        "--config\n" + str(job / "config/cpi.json") + "\nrequest-body\n",
                    )


    def test_director_symlinked_job_and_packages_use_installation_root(self):
        for custom_packages in (False, True):
            with self.subTest(custom_packages=custom_packages), tempfile.TemporaryDirectory(prefix="BOSH symlink wrapper ") as tmp:
                base = pathlib.Path(tmp)
                install = base / "vcap"
                real_job = install / "data/jobs/pve_cpi/job-fingerprint"
                (real_job / "bin").mkdir(parents=True)
                (real_job / "config").mkdir()
                (real_job / "bin/cpi").write_bytes(TEMPLATE.read_bytes())
                (install / "jobs").mkdir()
                job = install / "jobs/pve_cpi"
                job.symlink_to(real_job, target_is_directory=True)
                real_package = install / "data/packages/pve_cpi/package-fingerprint"
                (real_package / "bin").mkdir(parents=True)
                binary = real_package / "bin/cpi"
                binary.write_text('#!/bin/bash\nprintf "%s\\n" "$@"\ncat\n')
                binary.chmod(0o700)
                packages = base / "explicit relocated packages" if custom_packages else install / "packages"
                packages.mkdir()
                (packages / "pve_cpi").symlink_to(real_package, target_is_directory=True)
                env = dict(os.environ)
                env.pop("BOSH_PACKAGES_DIR", None)
                if custom_packages:
                    env["BOSH_PACKAGES_DIR"] = str(packages)
                request = '{"method":"info","arguments":[]}\n'
                result = subprocess.run(["bash", str(job / "bin/cpi")], input=request,
                                        text=True, capture_output=True, env=env, check=False)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(result.stdout, "--config\n" + str(job / "config/cpi.json") + "\n" + request)


if __name__ == "__main__":
    unittest.main()
