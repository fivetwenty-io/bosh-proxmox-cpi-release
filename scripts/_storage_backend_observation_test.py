import copy
import json
import pathlib
import tempfile
import unittest
from _storage_backend_observation import same_backend_observation, retain_backend_observation

class BackendObservationTests(unittest.TestCase):
    def setUp(self):
        self.base = {"backend": {"mount": {"target": "/dedicated", "source": "/dev/loop0", "uuid": "uuid", "fstype": "ext4", "options": "quota,rw"}, "exports": [{"client": "10.254.0.0/24", "options": "all_squash,rw,sync"}]}, "quota_limits": {"block_hard": 0, "block_soft": 0, "inode_hard": 0, "inode_soft": 0}, "active_fault_state_absent": True, "storage_definition": {"storage": "owned", "server": "10.254.0.1", "export": "/dedicated", "type": "nfs", "content": "images,iso,import", "nodes": "n0,n1,n2", "options": "vers=4.1,hard", "format": "qcow2"}}
    def test_only_membership_order_is_ignored(self):
        value = copy.deepcopy(self.base)
        value["storage_definition"].update(content="import,images,iso", nodes="n2,n0,n1")
        before = copy.deepcopy(value)
        self.assertTrue(same_backend_observation(self.base, value))
        self.assertEqual(value, before)
    def test_membership_changes_and_malformed_values_refuse(self):
        for field, value in [("content", "images,iso"), ("nodes", "n0,n1"), ("content", "images,images,iso,import"), ("nodes", "n0,,n1,n2"), ("nodes", ["n0", "n1", "n2"]), ("content", "images, iso,import")]:
            changed = copy.deepcopy(self.base); changed["storage_definition"][field] = value
            with self.subTest(field=field, value=value): self.assertFalse(same_backend_observation(self.base, changed))
    def test_all_identity_and_restoration_fields_remain_exact(self):
        mutations = [("storage_definition", "server", "elsewhere"), ("storage_definition", "export", "/other"), ("storage_definition", "storage", "foreign"), ("storage_definition", "options", "hard,vers=4.1"), ("storage_definition", "format", "raw"), ("quota_limits", "block_hard", 1), ("quota_limits", "inode_hard", 1)]
        for group, field, value in mutations:
            changed = copy.deepcopy(self.base); changed[group][field] = value
            with self.subTest(group=group, field=field): self.assertFalse(same_backend_observation(self.base, changed))
        for field, value in [("uuid", "other"), ("source", "/dev/loop1"), ("options", "quota,ro"), ("target", "/other"), ("fstype", "xfs")]:
            changed = copy.deepcopy(self.base); changed["backend"]["mount"][field] = value
            self.assertFalse(same_backend_observation(self.base, changed))
        changed = copy.deepcopy(self.base); changed["backend"]["exports"][0]["client"] = "*"
        self.assertFalse(same_backend_observation(self.base, changed))
    def test_unknown_fields_absence_and_types_are_not_discarded(self):
        for change in [{"active_fault_state_absent": False}, {"active_fault_state_absent": 1}, {"unexpected": "changed"}]:
            self.assertFalse(same_backend_observation(self.base, {**self.base, **change}))
        changed = copy.deepcopy(self.base); del changed["quota_limits"]
        self.assertFalse(same_backend_observation(self.base, changed))
    def test_raw_mismatch_retained_before_refusal(self):
        changed = copy.deepcopy(self.base); changed["quota_limits"]["block_hard"] = 42
        with tempfile.TemporaryDirectory() as d:
            path = pathlib.Path(d)/"backend-ready.json"
            def write(p, data):
                with p.open("x") as f: json.dump(data, f)
            with self.assertRaisesRegex(RuntimeError, "raw observation retained"):
                retain_backend_observation(path, self.base, changed, write)
            self.assertEqual(json.loads(path.read_text()), changed)
            with self.assertRaises(FileExistsError): retain_backend_observation(path, self.base, self.base, write)
            self.assertEqual(json.loads(path.read_text()), changed)
    def test_retention_failure_cannot_certify(self):
        def fail(path, value): raise OSError("full")
        with self.assertRaises(OSError): retain_backend_observation(None, self.base, self.base, fail)

if __name__ == "__main__": unittest.main()
