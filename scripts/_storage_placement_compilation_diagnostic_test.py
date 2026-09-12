import json,pathlib,tempfile,types,unittest
from unittest.mock import patch
from _storage_placement_director import DirectorScenarios,failure_evidence
class Diagnostics(unittest.TestCase):
 def test_failed_observations_remain_private_and_linked(self):
  with tempfile.TemporaryDirectory() as td:
   obj=DirectorScenarios.__new__(DirectorScenarios);obj.command_evidence_dir=pathlib.Path(td)
   sample={'successful_snapshots':2,'scanned':{'id':{'state':'ready_to_return','cid':'100'}},'observed':{},'failures_by_type':{'RuntimeError':2},'first_failure_samples':[{'error':'private detail'}]}
   obj.retain_compilation_observation(sample)
   path=pathlib.Path(td)/obj.compilation_observation_diagnostic
   self.assertEqual(json.loads(path.read_text()),sample);self.assertEqual(path.stat().st_mode & 0o777,0o600)
   evidence=failure_evidence(RuntimeError('failed'),obj)
   self.assertEqual(evidence['compilation_observation_diagnostic'],path.name);self.assertNotIn('private detail',json.dumps(evidence))
 def test_retention_failure_does_not_replace_original_failure(self):
  obj=DirectorScenarios.__new__(DirectorScenarios);obj.command_evidence_dir=pathlib.Path('/unused')
  with patch.object(pathlib.Path,'open',side_effect=OSError('ENOSPC')):obj.retain_compilation_observation({'sample':'value'})
  self.assertIsNone(obj.compilation_observation_diagnostic)
if __name__=='__main__':unittest.main()
