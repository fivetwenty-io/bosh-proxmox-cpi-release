import base64,json,os,pathlib,struct,tempfile,types,unittest
from unittest.mock import Mock,patch
import _storage_placement_rollout_trust as trust
class ReplacementTrustTests(unittest.TestCase):
 def test_replacement_trust_bound_to_qga_pve_state_and_strict_ssh(self):
  for failure in [None,'director','physical','ssh','state','oldtrust']:
   with self.subTest(failure=failure),tempfile.TemporaryDirectory() as directory:
    root=pathlib.Path(directory);previous=root/'old-known-hosts';previous.write_text('old pinned host');previous.chmod(0o600);key=root/'key';key.write_text('private');key.chmod(0o600);pmx=root/'pmx';pmx.write_text('pinnedpmx');state=root/'state';state.write_text(json.dumps({'current_vm_cid':'100072','disks':[{'cid':'disk'}]}))
    product='11111111-2222-3333-4444-555555555555';uuid='9c88e707-478d-4895-a708-001403623838';wire=struct.pack('>I',11)+b'ssh-ed25519'+struct.pack('>I',32)+b'x'*32;public='ssh-ed25519 '+base64.b64encode(wire).decode();payload={'public_key':public,'product_uuid':product,'director_uuid':uuid}
    verifier=Mock();verifier._transport='pmx';verifier.verify_ssl=True;verifier.host='pve';verifier.port=8006;verifier._ca='pinnedCA';verifier.base='https://pve:8006';verifier._get.return_value=[{'type':'qemu','vmid':100072,'node':'node'}];verifier.qemu_config.return_value={'smbios1':'uuid='+product}
    fixture={'ssh':{'host':'10.254.23.245','user':'jumpbox','known_hosts_file':str(previous),'identity_file':str(key)},'environment':'https://10.254.23.245:25555'};runner=types.SimpleNamespace(base_config={'host':'pve','verify_ssl':True,'pve_ca_cert':'pinnedCA','api_token':'user!token=secret'},verifier=verifier,report={},checkpoint=Mock());director=types.SimpleNamespace(fixture=fixture,observer=types.SimpleNamespace(fixture=fixture),runner=runner,command_evidence_dir=root)
    if failure=='director':payload['director_uuid']='another'
    if failure=='physical':payload['product_uuid']='another'
    def ssh_call(argv,**kwargs):
     self.assertIn('StrictHostKeyChecking=yes',argv);self.assertIn('IdentitiesOnly=yes',argv)
     if failure=='state':state.write_text('{}')
     if failure=='oldtrust':previous.write_text('changed')
     return types.SimpleNamespace(returncode=0,stdout=json.dumps({**payload,'public_key':'wrong'} if failure=='ssh' else payload))
    with patch.dict(os.environ,{'PVE_VERIFY_PMX_BIN':str(pmx)}),patch.object(trust,'successful_update',return_value={'path':'result','sha256':'sha'}),patch.object(trust,'qga_identity',return_value=(payload,123)),patch.object(trust.subprocess,'run',side_effect=ssh_call):
     if failure:
      with self.assertRaises((RuntimeError,KeyError)):trust.refresh_rollout_trust(director,state,uuid,'candidate_manifest')
      self.assertEqual(director.fixture['ssh']['known_hosts_file'],str(previous));self.assertFalse((root/'host-trust-candidate_manifest/completion.json').exists())
     else:
      proof=trust.refresh_rollout_trust(director,state,uuid,'candidate_manifest');self.assertTrue(proof['strict_ssh_passed']);self.assertNotEqual(director.fixture['ssh']['known_hosts_file'],str(previous));self.assertEqual(previous.read_text(),'old pinned host');self.assertEqual(director.observer.fixture,director.fixture)
      with self.assertRaisesRegex(RuntimeError,"already attempted"):trust.refresh_rollout_trust(director,state,uuid,'candidate_manifest')
 def test_missing_or_malformed_qga_credentials_refuse(self):
  import _storage_placement_rollout as rollout
  for token in [None,'','user=secret','!token=secret','user!=secret','user!token=','user!bad!token=secret']:
   with tempfile.TemporaryDirectory() as directory:
    root=pathlib.Path(directory)
    for name in ['known','key','pmx']:(root/name).write_text(name);(root/name).chmod(0o600)
    director=types.SimpleNamespace(command_evidence_dir=root,fixture={'ssh':{'host':'10.254.23.245','user':'jumpbox','known_hosts_file':str(root/'known'),'identity_file':str(root/'key')},'environment':'https://10.254.23.245:25555'},runner=types.SimpleNamespace(base_config={'verify_ssl':True,'pve_ca_cert':'CA','api_token':token},verifier=Mock()),write_json=Mock())
    scenario=rollout.RolloutScenarios.__new__(rollout.RolloutScenarios);scenario.director=director;scenario.remote_before={'director_uuid':'9c88e707-478d-4895-a708-001403623838'};scenario.candidate={'path':'candidate','sha256':'sha'}
    with patch.dict(os.environ,{'PVE_VERIFY_PMX_BIN':str(root/'pmx')}),patch('_storage_placement_director.archive_fingerprints',return_value=scenario.candidate),patch.object(rollout.subprocess,'run') as process:
     with self.assertRaisesRegex(RuntimeError,'PVE token unavailable'):scenario.create_env('candidate_manifest')
     process.assert_not_called();director.write_json.assert_not_called();self.assertFalse((root/'create-env-candidate_manifest').exists())
 def test_update_receipt_preserves_transport_and_rejects_changed_evidence(self):
  for failure in [None,'oldtrust','pmx','vars','stdout']:
   with self.subTest(failure=failure),tempfile.TemporaryDirectory() as directory:
    root=pathlib.Path(directory)
    for name in ['oldtrust','key','pmx','state','vars','manifest','archive']:(root/name).write_text(name)
    director=types.SimpleNamespace(command_evidence_dir=root,fixture={'ssh':{'known_hosts_file':str(root/'oldtrust'),'identity_file':str(root/'key')}},runner=types.SimpleNamespace(base_config={'verify_ssl':True}))
    with patch.dict(os.environ,{'PVE_VERIFY_PMX_BIN':str(root/'pmx')}):
     out=trust.begin_update(director,root/'state',root/'vars',root/'manifest',{'path':str(root/'archive'),'sha256':trust.digest(root/'archive')},'candidate_manifest');(root/'state').write_text('replacement');trust.finish_update(out,root/'state',result=types.SimpleNamespace(returncode=0,stdout='success',stderr=''))
     if failure:(out/'stdout' if failure=='stdout' else root/failure).write_text('changed')
     if failure:
      with self.assertRaises(RuntimeError):trust.successful_update(director,root/'state','candidate_manifest')
     else:self.assertEqual(trust.successful_update(director,root/'state','candidate_manifest')['sha256'],trust.digest(out/'result.json'))
     with self.assertRaises(FileExistsError):trust.begin_update(director,root/'state',root/'vars',root/'manifest',{'path':str(root/'archive'),'sha256':trust.digest(root/'archive')},'candidate_manifest')
 def test_phase_snapshots_remain_verifiable_after_next_phase_advances_state(self):
  with tempfile.TemporaryDirectory() as directory:
   root=pathlib.Path(directory)
   for name in ['oldtrust','key','pmx','state','vars','manifest','archive']:(root/name).write_text(name)
   director=types.SimpleNamespace(command_evidence_dir=root,fixture={'ssh':{'known_hosts_file':str(root/'oldtrust'),'identity_file':str(root/'key')}},runner=types.SimpleNamespace(base_config={'verify_ssl':True}))
   archive={'path':str(root/'archive'),'sha256':trust.digest(root/'archive')}
   with patch.dict(os.environ,{'PVE_VERIFY_PMX_BIN':str(root/'pmx')}):
    first=trust.begin_update(director,root/'state',root/'vars',root/'manifest',archive,'scalar_manifest')
    (root/'state').write_text('scalar');(root/'vars').write_text('scalar vars')
    trust.finish_update(first,root/'state',result=types.SimpleNamespace(returncode=0,stdout='first',stderr=''))
    old=trust.immutable_update(first)
    second=trust.begin_update(director,root/'state',root/'vars',root/'manifest',archive,'baseline_manifest')
    (root/'state').write_text('baseline');(root/'vars').write_text('baseline vars')
    trust.finish_update(second,root/'state',result=types.SimpleNamespace(returncode=0,stdout='second',stderr=''))
    self.assertEqual(trust.immutable_update(first),old);trust.immutable_update(second)
    with self.assertRaises(RuntimeError):trust.successful_update(director,root/'state','scalar_manifest')
    trust.successful_update(director,root/'state','baseline_manifest')
    for name in ('state-before.json','vars-before.json','state-after.json','vars-after.json'):
     path=first/name;raw=path.read_bytes();self.assertEqual(path.stat().st_mode & 0o777,0o600);path.write_bytes(b'changed')
     with self.assertRaises(RuntimeError):trust.immutable_update(first)
     path.write_bytes(raw)
 def test_native_qga_uses_pinned_tls_context_and_bounds_output(self):
  cfg={'host':'pve','verify_ssl':True,'pve_ca_cert':'CA','api_token':'user@pve!token=secret'};seen=[]
  def run(argv,**kwargs):
   context=json.loads(pathlib.Path(argv[argv.index('--config')+1]).read_text())['contexts']['rollout-trust'];self.assertFalse(context['tls']['insecure']);self.assertEqual(pathlib.Path(context['tls']['ca-cert']).read_text(),'CA');self.assertEqual(context['auth']['secret'],'${STORAGE_ROLLOUT_PMX_SECRET}');self.assertEqual(kwargs['env']['STORAGE_ROLLOUT_PMX_SECRET'],'secret');self.assertLessEqual(kwargs['timeout'],45);seen.append(argv)
   value={'pid':123} if 'exec-status' not in argv else {'exited':True,'exitcode':0,'out-truncated':False,'out-data':json.dumps({'director_uuid':'uuid'})}
   return types.SimpleNamespace(returncode=0,stdout=json.dumps(value))
  with patch.object(trust.subprocess,'run',side_effect=run):self.assertEqual(trust.qga_identity(cfg,'node','100072',pathlib.Path('/bound/pmx')),({'director_uuid':'uuid'},123))
  self.assertEqual(len(seen),2);self.assertIn('--no-log',seen[0]);self.assertIn(trust.GUEST_IDENTITY,seen[0])
 def test_exact_guest_program_uses_installed_config_and_loopback_identity(self):
  import contextlib,io,urllib.request
  files={'/etc/ssh/ssh_host_ed25519_key.pub':'ssh-ed25519 fixture','/sys/class/dmi/id/product_uuid':'11111111-2222-3333-4444-555555555555','/var/vcap/jobs/director/config/director.yml':json.dumps({'port':25556})}
  response=Mock();response.read.return_value=json.dumps({'uuid':'director'}).encode();response.__enter__=Mock(return_value=response);response.__exit__=Mock(return_value=False);opener=Mock();opener.open.return_value=response;out=io.StringIO()
  with patch.object(pathlib.Path,'read_text',lambda p:files[str(p)]),patch.object(urllib.request,'build_opener',return_value=opener),contextlib.redirect_stdout(out):exec(trust.GUEST_IDENTITY,{})
  self.assertEqual(json.loads(out.getvalue())['director_uuid'],'director');opener.open.assert_called_once_with('http://127.0.0.1:25556/info',timeout=10)
 def test_rollout_preflight_refuses_before_create_env_submission(self):
  import _storage_placement_rollout as rollout
  scenario=rollout.RolloutScenarios.__new__(rollout.RolloutScenarios);scenario.candidate={'path':'candidate','sha256':'sha'};scenario.director=Mock();scenario.remote_before={'director_uuid':'uuid'}
  with patch('_storage_placement_director.archive_fingerprints',return_value=scenario.candidate),patch.object(trust,'preflight_rollout_trust',side_effect=RuntimeError('TLS required')),patch.object(rollout.subprocess,'run') as process:
   with self.assertRaisesRegex(RuntimeError,'TLS required'):scenario.create_env('candidate_manifest')
   process.assert_not_called();scenario.director.write_json.assert_not_called()
 def test_malformed_public_key_and_private_file_refuse(self):
  for key in ['ssh-rsa xxx','ssh-ed25519 !!!!','ssh-ed25519 AAAA','ssh-ed25519 x\n']:
   with self.assertRaises(RuntimeError):trust.public_key(key)
  with tempfile.TemporaryDirectory() as d:
   p=pathlib.Path(d)/'key';p.write_text('private');p.chmod(0o644)
   with self.assertRaises(RuntimeError):trust.private_file(p)
   p.chmod(0o600);link=pathlib.Path(d)/'link';link.symlink_to(p)
   with self.assertRaises(RuntimeError):trust.private_file(link)
if __name__=='__main__':unittest.main()
