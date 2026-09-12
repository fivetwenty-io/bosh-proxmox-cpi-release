"""VMID reuse must resolve through live ownership and current journal authority."""
import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock
import _storage_placement_scenarios as scenarios

OLD='12345678-1234-4234-8234-123456789abc'
NEW='c13bd2b4-8539-415e-9565-0e76e8f79e88'
AGENT='cbde0b51-e383-4170-8424-3cf2d85d045f'

def persist(path,payload):
    raw=scenarios.go_canonical_json(payload)
    digest=hashlib.sha256(raw.encode()).hexdigest()
    path.write_text('{"version":1,"sha256":"'+digest+'","payload":'+raw+'}')
    path.chmod(0o600)
    return digest

class VMGenerationTests(unittest.TestCase):
    def test_reused_cid_resolves_only_current_marker_record_and_index(self):
        for mode in ('valid','ambiguous live','wrong marker','missing marker','duplicate marker field','wrong index','wrong agent','incomplete audit','unhealthy index','changed record'):
            with self.subTest(mode=mode),tempfile.TemporaryDirectory() as tmp:
                runner=scenarios.ScenarioRunner.__new__(scenarios.ScenarioRunner)
                runner.base_config={'storage_placement_namespace':'cert','storage_allocation_journal_dir':tmp}
                root=Path(tmp)/hashlib.sha256(b'cert').hexdigest();root.mkdir()
                agent_hash=hashlib.sha256(AGENT.encode()).hexdigest()
                old={'id':OLD,'cid':'8009','kind':'vm','namespace':'cert','agent_id':'old-agent','state':'cleaned'}
                new={**old,'id':NEW,'agent_id':AGENT,'state':'ready_to_return'}
                rows=[]
                for record in (old,new):
                    digest=persist(root/('allocation-'+record['id']+'.json'),record)
                    rows.append({'ID':record['id'],'CID':'8009','Kind':'vm','State':record['state'],'SHA256':digest})
                index={'version':1,'active_vms':{agent_hash:OLD if mode=='wrong index' else NEW}}
                persist(root/'index.json',index)
                marker={'version':1,'namespace':'cert','kind':'vm','allocation_id':OLD if mode=='wrong marker' else NEW,'agent_sha256':'f'*64 if mode=='wrong agent' else agent_hash}
                payload=json.dumps(marker)
                if mode=='duplicate marker field':payload=payload[:-1]+',"kind":"vm"}'
                description='[bosh_storage_allocation]\n'+payload+'\n[/bosh_storage_allocation]'
                if mode=='missing marker':description='operator metadata only'
                runner.verifier=SimpleNamespace(qemu_config=Mock(return_value={'description':description}))
                audit={'records':rows,'generation_index_healthy':mode!='unhealthy index','cluster_continuity':True,'audit':{'complete':mode!='incomplete audit','vm_scan_complete':True,'issues':[],'conflicts':[],'evidence':[{'kind':'vm','vmid':8009,'allocation_id':NEW,'agent_sha256':agent_hash}]}}
                if mode=='ambiguous live':rows[0]['State']='ready_to_return'
                if mode=='changed record':persist(root/('allocation-'+NEW+'.json'),{**new,'agent_id':'changed'})
                before={p.name:p.read_bytes() for p in root.iterdir()}
                if mode=='valid':self.assertEqual(runner.record_for(audit,'8009','vm')['ID'],NEW)
                else:
                    with self.assertRaises((scenarios.ScenarioFailure,ValueError)):
                        runner.record_for(audit,'8009','vm')
                self.assertEqual(before,{p.name:p.read_bytes() for p in root.iterdir()})

    def test_terminal_lookup_uses_exact_known_allocation_without_live_marker(self):
        runner=scenarios.ScenarioRunner.__new__(scenarios.ScenarioRunner)
        audit={'records':[{'ID':OLD,'CID':'8009','Kind':'vm','State':'cleaned'}, {'ID':NEW,'CID':'8009','Kind':'vm','State':'deleted'}],'audit':{'evidence':[]}}
        runner.audit=Mock(return_value=audit)
        runner.verifier=SimpleNamespace(qemu_config=Mock())
        runner.verification=SimpleNamespace(volume_inventory=Mock(return_value=[]))
        runner.assert_deleted('8009','vm',NEW,'config',{'e:iso/vm-8009-config.iso'})
        runner.verifier.qemu_config.assert_not_called()
        with self.assertRaises(scenarios.ScenarioFailure):runner.record_for(audit,'8009','vm','00000000-0000-4000-8000-000000000000')

if __name__=='__main__':unittest.main()
