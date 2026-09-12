import pathlib,runpy,tempfile,types,unittest
from unittest.mock import Mock,patch
BATS=runpy.run_path(str(pathlib.Path(__file__).with_name('bats')),run_name='bats_test_module')['BATS']
class BaselineTests(unittest.TestCase):
 def instance(self,root):
  value=BATS.__new__(BATS);value.dry_run=False;value.run_dir=root;value.rep=Mock();value.args=types.SimpleNamespace(skip_doc=True);value._director_env=lambda:{'BOSH_ENVIRONMENT':'https://dedicated','BOSH_CLIENT_SECRET':'private'};return value
 def test_captures_actual_cloud_and_restores_same_authenticated_endpoint(self):
  with tempfile.TemporaryDirectory() as td:
   v=self.instance(pathlib.Path(td));v.runner=Mock();v.runner.capture.return_value=(True,'vm_types: []\n');v.runner.step.return_value=(True,'')
   self.assertTrue(v.capture_baseline_cloud());self.assertEqual(v.baseline_cloud,{'vm_types':[]});self.assertEqual(v.baseline_cloud_path.stat().st_mode&0o777,0o600)
   self.assertTrue(v.finalize(True));args=v.runner.step.call_args
   self.assertEqual(args.args[1],['bosh','-n','update-cloud-config',str(v.baseline_cloud_path)])
   self.assertEqual(args.kwargs['env'],v._director_env())
 def test_absent_or_malformed_baseline_prevents_rspec(self):
  with tempfile.TemporaryDirectory() as td:
   for output in [(False,''),(True,'null'),(True,'- list'),(True,'[malformed')]:
    v=self.instance(pathlib.Path(td));v.runner=Mock();v.runner.capture.return_value=output;v.preflight=Mock(return_value=True);v.prepare=Mock(return_value=True);v.run_rspec=Mock()
    self.assertFalse(v.dispatch());v.run_rspec.assert_not_called()
 def test_restore_readback_mismatch_fails(self):
  with tempfile.TemporaryDirectory() as td:
   v=self.instance(pathlib.Path(td));v.runner=Mock();v.runner.capture.return_value=(True,'vm_types: []\n');v.runner.step.return_value=(True,'');self.assertTrue(v.capture_baseline_cloud());v.runner.capture.return_value=(True,'vm_types: [changed]\n');self.assertFalse(v.finalize(True))
 def test_light_stemcell_uses_explicit_vars_instead_of_default(self):
  with tempfile.TemporaryDirectory() as td:
   root=pathlib.Path(td);explicit=root/'explicit.yml';default=root/'default.yml';explicit.write_text('selected');default.write_text('other')
   v=self.instance(root);v.cfg={'bosh_vars':str(explicit)};v.env_name='storage-cert'
   values={'/stemcell_url':'explicit-image','/stemcell_sha1':'explicit-hash','/pve_host':'explicit-pve','/pve_port':'8006','/pve_api_token':'private'}
   def read(path,key):
    self.assertEqual(path,explicit);return values.get(key,'')
   globals_=BATS._ensure_light_stemcell.__globals__;light=globals_['_lightstemcell']
   with patch.dict(globals_,{'BOSH_VARS':default,'_bosh_int_opt':read,'layered_var':Mock(side_effect=AssertionError('default/env variables must not override explicit input'))}),patch.object(light,'parse_os_version',return_value=('noble','1')),patch.object(light,'ensure_light_stemcell',return_value='ready') as ensure:
    self.assertEqual(v._ensure_light_stemcell(),'ready')
    self.assertEqual(ensure.call_args.kwargs['cpi_cfg']['host'],'explicit-pve')
    self.assertEqual(ensure.call_args.kwargs['source_url'],'explicit-image')
    self.assertEqual(ensure.call_args.kwargs['source_sha1'],'explicit-hash')
 def test_light_stemcell_retains_env_layering_for_default_vars(self):
  with tempfile.TemporaryDirectory() as td:
   root=pathlib.Path(td);default=root/'default.yml';v=self.instance(root);v.cfg={'bosh_vars':str(default)};v.env_name='selected-env'
   values={'/stemcell_url':'layered-image','/stemcell_sha1':'layered-hash','/pve_host':'layered-pve','/pve_port':'8006'}
   def layered(env,key):self.assertEqual(env,'selected-env');return values.get(key,'')
   globals_=BATS._ensure_light_stemcell.__globals__;light=globals_['_lightstemcell']
   with patch.dict(globals_,{'BOSH_VARS':default,'layered_var':layered,'_bosh_int_opt':Mock(side_effect=AssertionError('layering bypassed'))}),patch.object(light,'parse_os_version',return_value=('noble','1')),patch.object(light,'ensure_light_stemcell',return_value='ready') as ensure:
    self.assertEqual(v._ensure_light_stemcell(),'ready');self.assertEqual(ensure.call_args.kwargs['cpi_cfg']['host'],'layered-pve')
if __name__=='__main__':unittest.main()
