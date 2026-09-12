import hashlib,io,json,os,pathlib,tempfile,types,unittest
from unittest.mock import patch
import _storage_placement_rollout as rollout
class RuntimeOwnerTests(unittest.TestCase):
 def setup_fixture(self,td):
  root=pathlib.Path(td).resolve();base=root/'pve_cpi';base.mkdir(mode=0o700);journal=base/'allocation-journal';journal.mkdir(mode=0o700)
  binary=root/'cpi';binary.write_bytes(b'candidate');binary.chmod(0o755)
  config=root/'config.json';config.write_text(json.dumps({'storage_allocation_journal_dir':str(journal)}));config.chmod(0o640)
  source=rollout.RETAIN_AUDITOR.replace('/var/vcap/store/pve_cpi',str(base)).replace('/var/vcap/packages/pve_cpi/bin/cpi',str(binary)).replace('/var/vcap/jobs/pve_cpi/config/cpi.json',str(config))
  return base,journal,source
 def test_retention_drops_identity_before_writes_without_chown_existing(self):
  with tempfile.TemporaryDirectory() as td:
   base,journal,source=self.setup_fixture(td);before=(base.stat(),journal.stat());events=[];real_open=os.open
   def opened(path,flags,*args,**kwargs):
    if flags&os.O_CREAT:self.assertEqual(events,['groups','gid','uid'])
    return real_open(path,flags,*args,**kwargs)
   user=types.SimpleNamespace(pw_uid=os.getuid(),pw_gid=os.getgid());output=io.StringIO();run_id='a'*32
   with patch('pwd.getpwnam',return_value=user),patch('os.setgroups',side_effect=lambda groups:events.append('groups')),patch('os.setgid',side_effect=lambda gid:events.append('gid')),patch('os.setuid',side_effect=lambda uid:events.append('uid')),patch('os.chown') as chown,patch('os.open',side_effect=opened),patch('sys.stdin',io.StringIO(json.dumps({'run_id':run_id,'sha256':hashlib.sha256(b'candidate').hexdigest()}))),patch('sys.stdout',output):
    exec(compile(source,'retention','exec'),{})
   chown.assert_not_called();self.assertEqual(events,['groups','gid','uid'])
   for path,prior in zip((base,journal),before):self.assertEqual((path.stat().st_uid,path.stat().st_gid,path.stat().st_mode),(prior.st_uid,prior.st_gid,prior.st_mode))
   self.assertEqual((base/'storage-certification'/run_id/'candidate-cpi').stat().st_mode&0o777,0o700)
   self.assertEqual((base/'storage-certification'/run_id/'candidate-config.json').stat().st_mode&0o777,0o600)
 def test_wrong_candidate_never_drops_identity_or_creates_artifacts(self):
  with tempfile.TemporaryDirectory() as td:
   base,journal,source=self.setup_fixture(td)
   with patch('pwd.getpwnam',return_value=types.SimpleNamespace(pw_uid=os.getuid(),pw_gid=os.getgid())),patch('os.setuid') as drop,patch('sys.stdin',io.StringIO(json.dumps({'run_id':'a'*32,'sha256':'0'*64}))):
    with self.assertRaisesRegex(RuntimeError,'candidate binary differs'):exec(compile(source,'retention','exec'),{})
   drop.assert_not_called();self.assertFalse((base/'storage-certification').exists())
if __name__=='__main__':unittest.main()
