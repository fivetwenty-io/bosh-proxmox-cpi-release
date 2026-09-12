import json,pathlib,tempfile,types,unittest
from unittest.mock import Mock,patch
import _storage_placement_director as subject
class CommandTests(unittest.TestCase):
 def director(self):
  value=subject.DirectorScenarios.__new__(subject.DirectorScenarios);value.fixture={'environment':'https://dedicated'};value.name='storage-cert-unit';value.command_evidence_dir=pathlib.Path(self.enterContext(tempfile.TemporaryDirectory()));value.last_command_diagnostic=None;return value
 def test_private_json_inputs_survive_temporary_directory_cleanup_and_refuse_overwrite(self):
  value=self.director();value.directory=tempfile.TemporaryDirectory()
  path=pathlib.Path(value.write_json('rollout-candidate_manifest.json',{'private':'value'}))
  value.directory.cleanup()
  self.assertEqual(path.parent,value.command_evidence_dir);self.assertEqual(json.loads(path.read_text()),{'private':'value'});self.assertEqual(path.stat().st_mode & 0o777,0o600)
  with self.assertRaises(FileExistsError):value.write_json(path.name,{'private':'replacement'})
  self.assertEqual(json.loads(path.read_text()),{'private':'value'})
  for name in ('../outside.json','/absolute.json','nested/name.json'):
   with self.assertRaises(RuntimeError):value.write_json(name,{})
 def test_raw_info_tasks_and_events_omit_cli_json_envelope(self):
  for command,raw,wanted in [(['curl','/info'],'{"uuid":"real-director"}',{'uuid':'real-director'}),(['curl','/tasks?limit=100'],'[]',[]),(['curl','/tasks/12/output?type=event'],'{"stage":"one"}\n{"stage":"two"}\n',[{'stage':'one'},{'stage':'two'}])]:
   value=self.director()
   with patch.object(subject.subprocess,'run',return_value=types.SimpleNamespace(returncode=0,stdout=raw,stderr='')) as run:
    self.assertEqual(value.bosh(command,scoped=False,as_json=True),wanted)
    self.assertNotIn('--json',run.call_args.args[0])
 def test_tables_keep_cli_json_mode(self):
  value=self.director();table={'Tables':[{'Rows':[{'vm_cid':'100'}]}]}
  with patch.object(subject.subprocess,'run',return_value=types.SimpleNamespace(returncode=0,stdout=json.dumps(table),stderr='')) as run:
   self.assertEqual(value.bosh(['vms','--details'],as_json=True),table);self.assertIn('--json',run.call_args.args[0])
 def test_malformed_enveloped_and_oversized_curl_fail_with_fixed_reasons(self):
  for raw in ['not json',json.dumps({'Tables':None,'Blocks':['{"uuid":"hidden"}'],'Lines':[]}),json.dumps(['x'])*20]:
   value=self.director()
   with patch.object(subject,'MAX_COMMAND_BYTES',64),patch.object(subject.subprocess,'run',return_value=types.SimpleNamespace(returncode=0,stdout=raw,stderr='')):
    with self.assertRaises(subject.DirectorFailure):value.bosh(['curl','/info'],as_json=True)
 def test_event_scalar_or_cli_envelope_is_refused(self):
  for raw in ['1\n',json.dumps({'Tables':None,'Blocks':['{}'],'Lines':[]})]:
   value=self.director()
   with patch.object(subject.subprocess,'run',return_value=types.SimpleNamespace(returncode=0,stdout=raw,stderr='')):
    with self.assertRaises(subject.DirectorFailure):value.bosh(['curl','/tasks/1/output?type=event'],as_json=True)
 def test_private_command_output_is_not_exposed_in_failure_evidence(self):
  value=self.director();secret='credential-must-stay-private'
  with patch.object(subject.subprocess,'run',return_value=types.SimpleNamespace(returncode=1,stdout='',stderr=secret)):
   with self.assertRaises(subject.DirectorFailure) as caught:value.bosh(['curl','/info'],as_json=True)
  evidence=subject.failure_evidence(caught.exception,value);self.assertNotIn(secret,json.dumps(evidence))
  path=value.command_evidence_dir/evidence['command_diagnostic'];self.assertIn(secret,path.read_text());self.assertEqual(path.stat().st_mode&0o777,0o600)
  self.assertNotIn('BOSH_CLIENT_SECRET',path.read_text())
 def test_private_retention_failure_does_not_replace_command_failure(self):
  value=self.director()
  with patch.object(pathlib.Path,'open',side_effect=OSError('full')),patch.object(subject.subprocess,'run',return_value=types.SimpleNamespace(returncode=1,stdout='',stderr='secret')):
   with self.assertRaisesRegex(subject.DirectorFailure,'Fixed BOSH command failed'):value.bosh(['curl','/info'],as_json=True)
 def test_missing_wrong_or_ambiguous_stemcell_stops_before_workflow_mutation(self):
  required={'alias':'default','os':'ubuntu-noble','version':'1.484.cert'}
  valid={'name':'any-compatible-name','operating_system':'ubuntu-noble','version':'1.484.cert','cid':'owned-cid'}
  for available in [[],[{**valid,'version':'other'}],[{**valid,'operating_system':'other'}],[valid,{**valid,'cid':'second-cid'}]]:
   value=self.director();value.deployment={'stemcells':[required]};value.baseline_cloud={};value.read_cloud_config=lambda:{};value.snapshot=Mock(side_effect=AssertionError('must not reach snapshot'))
   value.bosh=Mock(side_effect=[{'Tables':[{'Rows':[]}]},available])
   with self.assertRaisesRegex(subject.DirectorFailure,'missing or ambiguous'):value.compilation()
   self.assertEqual([call.args[0][0] for call in value.bosh.call_args_list],['deployments','curl']);value.snapshot.assert_not_called()
 def test_manifest_os_and_version_do_not_impose_unrequested_name(self):
  value=self.director();value.deployment={'stemcells':[{'alias':'default','os':'ubuntu-noble','version':'1'}]};value.bosh=Mock(return_value=[{'name':'provider-specific-name','operating_system':'ubuntu-noble','version':'1'}]);value.require_workload_stemcells()
  value.deployment['stemcells'][0]['name']='explicit-other-name'
  with self.assertRaisesRegex(subject.DirectorFailure,'missing or ambiguous'):value.require_workload_stemcells()
 def test_final_report_preserves_internally_caught_module_resources_before_close(self):
  import _storage_placement_scenarios as scenarios
  with tempfile.TemporaryDirectory() as td:
   root=pathlib.Path(td);config=root/'config';manifest=root/'manifest';report=root/'report';config.write_text('{}');manifest.write_text('{}')
   active={'director_workflow':{'deployment':'storage-cert-unit','phase':'prepared'}}
   runner=types.SimpleNamespace(report={'rows':[]},active_resources=active,preflight=Mock(),close=lambda:active.clear())
   def caught_failure(*args):
    scenarios.write_report(report,{'retained_resources':active,'rows':[]})
    return [{'scenario_id':'director_compilation','status':'failed','evidence':{'reason':'fixed failure'}}]
   with patch.object(scenarios,'validate_manifest'),patch.object(scenarios,'ScenarioRunner',return_value=runner),patch.object(subject,'run_director_scenarios',side_effect=caught_failure):
    self.assertEqual(scenarios.main(['--config',str(config),'--manifest',str(manifest),'--report',str(report),'--cpi-bin','unused','--suite','director']),1)
   saved=json.loads(report.read_text());self.assertEqual(saved['retained_resources'],{'director_workflow':{'deployment':'storage-cert-unit','phase':'prepared'}});self.assertEqual(active,{})
 def test_final_report_survives_local_close_failure(self):
  import _storage_placement_scenarios as scenarios
  with tempfile.TemporaryDirectory() as td:
   root=pathlib.Path(td);config=root/'config';manifest=root/'manifest';report=root/'report';config.write_text('{}');manifest.write_text('{}')
   runner=types.SimpleNamespace(report={'rows':[]},active_resources={'pending':'preserved'},preflight=Mock(),close=Mock(side_effect=OSError('private detail')))
   with patch.object(scenarios,'validate_manifest'),patch.object(scenarios,'ScenarioRunner',return_value=runner),patch.object(subject,'run_director_scenarios',return_value=[{'status':'passed'}]):
    self.assertEqual(scenarios.main(['--config',str(config),'--manifest',str(manifest),'--report',str(report),'--cpi-bin','unused','--suite','director']),1)
   saved=json.loads(report.read_text());self.assertEqual(saved['retained_resources'],{'pending':'preserved'});self.assertNotIn('private detail',json.dumps(saved))
 def test_human_task_commands_force_tty_without_color(self):
  value=self.director()
  def process(argv,**kwargs):
   self.assertIn('--tty',argv);self.assertIn('--no-color',argv);self.assertNotIn('--json',argv)
   return types.SimpleNamespace(returncode=0,stdout='Using environment dedicated\n\nTask 17\nTask 17 done\nSucceeded\n',stderr='')
  with patch.object(subject.subprocess,'run',side_effect=process):
   self.assertEqual(value.bosh(['deploy','/private/deployment.json'])['task_ids'],['17'])
 def test_ambient_tty_cannot_corrupt_raw_curl_or_json_tables(self):
  value=self.director()
  with patch.dict(subject.os.environ,{'BOSH_TTY':'true','BOSH_CLIENT_SECRET':'kept-private'}):
   for command,expected in [(['curl','/info'],{'uuid':'correct'}),(['instances','--details'],{'Tables':[]})]:
    with patch.object(subject.subprocess,'run',return_value=types.SimpleNamespace(returncode=0,stdout=json.dumps(expected),stderr='')) as call:
     self.assertEqual(value.bosh(command,as_json=True),expected)
     self.assertNotIn('--tty',call.call_args.args[0]);self.assertNotIn('BOSH_TTY',call.call_args.kwargs['env'])
     self.assertEqual(call.call_args.kwargs['env']['BOSH_CLIENT_SECRET'],'kept-private')
     self.assertEqual('--json' in call.call_args.args[0],command[0]!='curl')
 def test_raw_cloud_config_sanitizes_ambient_tty(self):
  value=self.director()
  responses=[types.SimpleNamespace(returncode=0,stdout='vm_types: []\n'),types.SimpleNamespace(returncode=0,stdout='{"vm_types":[]}')]
  with patch.dict(subject.os.environ,{'BOSH_TTY':'true'}),patch.object(subject.subprocess,'run',side_effect=responses) as call:
   self.assertEqual(value.read_cloud_config(),{'vm_types':[]})
   self.assertNotIn('BOSH_TTY',call.call_args_list[0].kwargs['env']);self.assertNotIn('--tty',call.call_args_list[0].args[0])
 def test_raw_curl_without_json_parsing_still_never_forces_tty(self):
  value=self.director()
  with patch.object(subject.subprocess,'run',return_value=types.SimpleNamespace(returncode=0,stdout='{}',stderr='')) as call:
   value.bosh(['curl','/info']);self.assertNotIn('--tty',call.call_args.args[0])
 def test_no_event_task_completion_still_identifies_invocation(self):
  value=self.director()
  with patch.object(subject.subprocess,'run',return_value=types.SimpleNamespace(returncode=0,stdout='Task 19. Done\n',stderr='')):
   self.assertEqual(value.bosh(['run-errand','check'])['task_ids'],['19'])
if __name__=='__main__':unittest.main()
