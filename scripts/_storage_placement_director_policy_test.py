import copy,types,unittest
from unittest.mock import Mock
from _storage_placement_director import director_policy_requirements,validate_director_policy,DirectorScenarios,director_runner_view
from _storage_placement_scenarios import policy_capabilities,ScenarioRunner
class PolicyTests(unittest.TestCase):
 def setUp(self):
  self.fixture={'compilation_vm_type':'compile','workload_vm_type':'service','errand_vm_type':'errand','expected_root_mechanism':'linked_clone'}
  self.cloud={'vm_types':[{'name':n,'cloud_properties':{'ephemeral_storage_set':'e','ephemeral_disk_size_mb':2048}} for n in ['compile','service','errand']],'disk_types':[{'name':'p','disk_size':2048,'cloud_properties':{'storage_set':'p'}}]}
  self.deployment={'instance_groups':[{'name':'service','persistent_disk_type':'p'}]}
  self.deployed={'storage_sets':{'e':{'names':['E1','E2','E3']},'p':{'names':['P1','P2']}},'ephemeral_storage_set':'e','persistent_storage_set':'p','iso_storage_follow_vm_storage':False,'iso_storage':'images'}
  self.policy=director_policy_requirements(self.cloud,self.deployment,self.fixture,self.deployed)
 def test_director_does_not_require_split_encryption_or_plural_roles(self):
  self.assertEqual(policy_capabilities(suite='director'),{'ephemeral':1,'persistent':1,'root':0,'encrypted':0})
  self.assertEqual(self.policy['root_storage_ids'],[]);self.assertEqual(self.policy['encrypted_persistent_storage_ids'],[])
  validate_director_policy(self.policy,self.cloud,self.deployment,self.fixture,self.deployed)
 def test_every_inherited_supplemental_assumption_is_rejected(self):
  wrong={'vm_cloud_properties':{'cores':1},'expected_root_mechanism':'full_clone','root_storage_ids':['E4'],'fixed_iso_storage_id':'E3','ephemeral_size_mib':4096,'disk_size_mib':1024,'encrypted_persistent_storage_ids':['P2'],'escaping_tier_criteria':{'types':['lvmthin']},'ephemeral_storage_ids':['E1'],'persistent_storage_ids':['P1']}
  for key,value in wrong.items():
   with self.subTest(key=key),self.assertRaisesRegex(RuntimeError,key):validate_director_policy({**self.policy,key:value},self.cloud,self.deployment,self.fixture,self.deployed)
 def test_bad_fixture_fails_before_cloud_update_release_or_deploy(self):
  obj=DirectorScenarios.__new__(DirectorScenarios);obj.runner=types.SimpleNamespace(policy={**self.policy,'expected_root_mechanism':'full_clone'});obj.cloud=self.cloud;obj.deployment=self.deployment;obj.fixture=self.fixture
  obj.baseline_cloud={};obj.read_cloud_config=lambda:{};obj.require_workload_stemcells=lambda:None;obj.snapshot=lambda:{'policy':self.deployed};obj.bosh=Mock(return_value={'Tables':[{'Rows':[]}]})
  with self.assertRaisesRegex(RuntimeError,'expected_root_mechanism'):obj.compilation()
  self.assertEqual(obj.bosh.call_args_list[0].args[0],['deployments']);self.assertEqual(obj.bosh.call_count,1)
 def test_director_policy_view_preserves_combined_suite_expectations(self):
  original=types.SimpleNamespace(policy={'expected_root_mechanism':'full_clone','encrypted_persistent_storage_ids':['P2'],'stemcell_cid':'bound'},report={},active_resources={})
  view=director_runner_view(original,{'observer_policy':self.policy})
  self.assertEqual(view.policy['expected_root_mechanism'],'linked_clone');self.assertEqual(original.policy['expected_root_mechanism'],'full_clone')
  self.assertEqual(original.policy['encrypted_persistent_storage_ids'],['P2']);self.assertEqual(view.policy['encrypted_persistent_storage_ids'],[])
  self.assertIs(view.report,original.report);self.assertIs(view.active_resources,original.active_resources);self.assertEqual(view.policy['stemcell_cid'],'bound')
  with self.assertRaises(RuntimeError):director_runner_view(original,{'observer_policy':{**self.policy,'stemcell_cid':'other'}})
 def test_auto_expectation_tracks_auto_policy_without_pinning_one_past_mechanism(self):
  auto={**self.fixture,'expected_root_mechanism':'auto'}
  self.assertEqual(director_policy_requirements(self.cloud,self.deployment,auto,self.deployed)['expected_root_mechanism'],'auto')
  for mode in ['full','linked']:
   with self.assertRaises(RuntimeError):director_policy_requirements(self.cloud,self.deployment,auto,{**self.deployed,'clone_mode':mode})
 def test_auto_still_requires_actual_source_provenance(self):
  for mechanism in ['linked_clone','full_clone']:
   query=Mock(side_effect=RuntimeError('must observe source'))
   runner=types.SimpleNamespace(policy={'expected_root_mechanism':'auto'},verifier=types.SimpleNamespace(_get=query))
   record={'intent':{'plan':{'Targets':[{'Role':'root','Mechanism':mechanism,'Source':{'VolumeID':'pool:base','Node':'node'}}]}}}
   with self.assertRaisesRegex(RuntimeError,'must observe source'):ScenarioRunner.observe_root_record(runner,record,{}, {'clone_mode':'auto'})
   query.assert_called_once()
   query.reset_mock()
   with self.assertRaisesRegex(RuntimeError,'required root mechanism'):ScenarioRunner.observe_root_record(runner,record,{}, {'clone_mode':'full'})
   query.assert_not_called()
 def test_explicit_root_split_and_selector_conflicts(self):
  split=copy.deepcopy(self.deployed);split['root_storage_set']='r';split['storage_sets']['r']={'names':['E4']}
  self.assertEqual(director_policy_requirements(self.cloud,self.deployment,self.fixture,split)['root_storage_ids'],['E4'])
  bad=copy.deepcopy(self.cloud);bad['vm_types'][0]['cloud_properties']['storage_selector']='other'
  with self.assertRaises(RuntimeError):director_policy_requirements(bad,self.deployment,self.fixture,self.deployed)
if __name__=='__main__':unittest.main()
