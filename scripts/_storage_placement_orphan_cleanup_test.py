import types,unittest,json,copy
from unittest.mock import Mock
from _storage_placement_director import DirectorScenarios
class OrphanCleanupTests(unittest.TestCase):
 def fixture(self):
  obj=DirectorScenarios.__new__(DirectorScenarios);obj.name='owned-deployment';disk={'cid':'pvz-owned','allocation_uuid':'allocation','stable_token':'bpd-token','volume_id':'p:90000/disk.qcow2','size_bytes':2048*1024*1024};obj.service={'instance':'service/uuid','disks':[disk]};obj.snapshot=Mock(return_value={'records':{'allocation':{'kind':'disk','cid':disk['cid'],'state':'ready_to_return'}}});row={'disk_cid':disk['cid'],'deployment_name':obj.name,'instance_name':'service/uuid','orphaned_at':'today','size':2048};obj.bosh=Mock(side_effect=[{'Tables':[{'Rows':[]}]},[row],[row],[]]);obj.disk_evidence=Mock(return_value=disk);obj.require_detached_or_parked_disk=Mock();obj.operation=Mock(return_value={'command':'delete-disk'});return obj,row
 def test_owned_orphan_uses_exact_global_delete_after_proof(self):
  obj,row=self.fixture();self.assertEqual(obj.delete_owned_orphan_disks(),[{'command':'delete-disk'}]);obj.operation.assert_called_once_with(['delete-disk','pvz-owned'],scoped=False);self.assertEqual(obj.require_detached_or_parked_disk.call_count,2)
 def test_foreign_duplicate_wrong_size_or_instance_refuse(self):
  for mutate in [lambda row:row.update(disk_cid='pvz-foreign'),lambda row:row.update(size=65536),lambda row:row.update(instance_name='other/uuid'),lambda row:row.update(deployment_name='other')]:
   obj,row=self.fixture();mutate(row)
   with self.assertRaises(RuntimeError):obj.delete_owned_orphan_disks()
   obj.operation.assert_not_called()
 def test_changed_orphan_and_attached_disk_refuse(self):
  obj,row=self.fixture();obj.bosh.side_effect=[{'Tables':[{'Rows':[]}]},[row],[]]
  with self.assertRaises(RuntimeError):obj.delete_owned_orphan_disks()
  obj.operation.assert_not_called()
  obj,row=self.fixture();obj.require_detached_or_parked_disk.side_effect=RuntimeError('attached')
  with self.assertRaises(RuntimeError):obj.delete_owned_orphan_disks()
  obj.operation.assert_not_called()
 def test_strict_holder_scan_refuses_workload_and_unreadable_vm(self):
  obj,_=self.fixture();del obj.require_detached_or_parked_disk;v=Mock();obj.runner=types.SimpleNamespace(verifier=v);v._get.return_value=[{'type':'qemu','vmid':100,'node':'n'}];disk=obj.service['disks'][0]
  v.qemu_config.return_value={'scsi1':disk['volume_id']+',serial='+disk['stable_token']}
  with self.assertRaises(RuntimeError):obj.require_detached_or_parked_disk(disk)
  v.qemu_config.side_effect=RuntimeError('read failed')
  with self.assertRaisesRegex(RuntimeError,'read failed'):obj.require_detached_or_parked_disk(disk)
 def production_parker(self):
  obj,_=self.fixture();del obj.require_detached_or_parked_disk;obj.fixture={'namespace':'storage-cert-director-20260909'};v=Mock();obj.runner=types.SimpleNamespace(verifier=v)
  disk=obj.service['disks'][0];disk.update(volume_id='nfs-persistent-cert-2:104166/vm-104166-disk-0.qcow2',backing={'type':'nfs','server':'10.254.0.1','export':'/tank/nfs/labs/pve-cpi/multi-storage/persistent-cert-2'})
  entry={'disk_cid':disk['cid'],'allocation_id':disk['allocation_uuid'],'allocation_namespace':obj.fixture['namespace'],'allocation_backing':'nfs://'+disk['backing']['server']+disk['backing']['export'],'volid':disk['volume_id'],'slot':'scsi0','node':'lab-pve-cpi-0'}
  config={'name':'bosh-parker-104166','tags':'bosh-cpi;bosh-parker;director--uuid','onboot':0,'scsihw':'virtio-scsi-pci','scsi0':disk['volume_id']+',serial='+disk['stable_token']+',size=2G','description':'<!--BOSH:'+json.dumps({'bosh_parked_disks':{disk['stable_token']:entry}})+'-->'}
  v.qemu_config.return_value=config;v.parked_disk_recorded.return_value=True;v._get.side_effect=lambda path:[{'type':'qemu','vmid':104166,'node':'lab-pve-cpi-0'}] if path.startswith('/cluster/') else {'status':'stopped','qmpstatus':'stopped'}
  return obj,v,disk,config,entry
 def test_production_scsi_parker_is_accepted(self):
  obj,v,disk,config,entry=self.production_parker();obj.require_detached_or_parked_disk(disk)
 def test_actual_production_parker_capture(self):
  from _pve_verify import PVEVerifier
  capture={'disk': {'cid': 'pvz-H4sIAAAAAAAC_zTMTW7DIBQE4LvMmtfy4yTAbXgPLFtKjAs0XUS-e2VFWc030mheeCJimzvtpfW1j7INktIG2Wi01eHy_XzQW8S1LyTFFW2SDc5rm26e0v1ehcyUtc_BUL4K0xTmC4XMiawY5yftLbv560fqn4XCA_GFuo9-Zl67pJYRUTcorHUsraSzGyj0_tahkDZZakMc7bcozIj4_K3nhvdMQfyNWSeXp8CsrziO_wEAa9plP-IAAAA', 'allocation_uuid': '14d08d91-d6cb-49f5-9dba-2c1384082b3f', 'stable_token': 'bpd-9c87bb0a3d49bb06', 'volume_id': 'nfs-persistent-cert-2:104166/vm-104166-disk-0.qcow2', 'size_bytes': 2147483648, 'backing': {'type': 'nfs', 'server': '10.254.0.1', 'export': '/tank/nfs/labs/pve-cpi/multi-storage/persistent-cert-2'}}, 'holder': {'vmid': 104166, 'node': 'lab-pve-cpi-0', 'slot': 'scsi0', 'config': {'boot': ' ', 'cores': 1, 'description': '<!--BOSH:{"bosh_parked_disks":{"bpd-9c87bb0a3d49bb06":{"allocation_id":"14d08d91-d6cb-49f5-9dba-2c1384082b3f","allocation_namespace":"storage-cert-director-20260909","allocation_backing":"nfs://10.254.0.1/tank/nfs/labs/pve-cpi/multi-storage/persistent-cert-2","disk_cid":"pvz-H4sIAAAAAAAC_zTMTW7DIBQE4LvMmtfy4yTAbXgPLFtKjAs0XUS-e2VFWc030mheeCJimzvtpfW1j7INktIG2Wi01eHy_XzQW8S1LyTFFW2SDc5rm26e0v1ehcyUtc_BUL4K0xTmC4XMiawY5yftLbv560fqn4XCA_GFuo9-Zl67pJYRUTcorHUsraSzGyj0_tahkDZZakMc7bcozIj4_K3nhvdMQfyNWSeXp8CsrziO_wEAa9plP-IAAAA","source_vm_cid":"100137","parked_at":"2026-09-10T17:48:12Z","node":"lab-pve-cpi-0","director_id":"9c88e707-478d-4895-a708-001403623838","volid":"nfs-persistent-cert-2:104166/vm-104166-disk-0.qcow2","slot":"scsi0","opts":{"discard":"on","iothread":"1","ssd":"1"}}}}-->', 'digest': '64636652b5c004df706b7a8275fd5f04c180b718', 'memory': '16', 'meta': 'creation-qemu=11.0.2,ctime=1789041704', 'name': 'bosh-parker-104166', 'onboot': 0, 'protection': 1, 'scsi0': 'nfs-persistent-cert-2:104166/vm-104166-disk-0.qcow2,serial=bpd-9c87bb0a3d49bb06,size=2G', 'scsihw': 'virtio-scsi-pci', 'smbios1': 'uuid=d88da3bc-6d8d-4c05-98ab-1cc90a2642a0', 'tags': 'bosh-cpi;bosh-parker;director--9c88e707-478d-4895-a708-001403623838', 'vmgenid': '09281bf7-3bc1-4b86-9a35-d8abbd3edd82'}, 'status': {'cpu': 0, 'cpus': 1, 'disk': 0, 'ha': {'managed': 0}, 'maxdisk': 0, 'maxmem': 16777216, 'mem': 0, 'memhost': 0, 'name': 'bosh-parker-104166', 'netin': 0, 'netout': 0, 'qmpstatus': 'stopped', 'status': 'stopped', 'tags': 'bosh-cpi;bosh-parker;director--9c88e707-478d-4895-a708-001403623838', 'uptime': 0, 'vmid': 104166}, 'parked_recorded': True}}
  obj=DirectorScenarios.__new__(DirectorScenarios);obj.fixture={'namespace':'storage-cert-director-20260909'};v=PVEVerifier.__new__(PVEVerifier);obj.runner=types.SimpleNamespace(verifier=v);holder=capture['holder'];v.qemu_config=Mock(return_value=holder['config']);v._get=Mock(side_effect=lambda path:[{'type':'qemu','vmid':holder['vmid'],'node':holder['node']}] if path.startswith('/cluster/') else holder['status']);obj.require_detached_or_parked_disk(capture['disk'])
 def test_running_wrong_slot_serial_and_provenance_refuse(self):
  for change in ['running','unused','serial','volume','onboot','name','allocation','cid','slot','namespace','backing']:
   obj,v,disk,config,entry=self.production_parker()
   if change=='running':v._get.side_effect=lambda path:[{'type':'qemu','vmid':104166,'node':'lab-pve-cpi-0'}] if path.startswith('/cluster/') else {'status':'running','qmpstatus':'running'}
   elif change=='unused':config['unused0']=config.pop('scsi0')
   elif change=='serial':config['scsi0']=disk['volume_id']+',serial=other'
   elif change=='volume':config['scsi0']='other:volume,serial='+disk['stable_token']
   elif change=='onboot':config['onboot']=1
   elif change=='name':config['name']='workload'
   else:
    field={'allocation':'allocation_id','cid':'disk_cid','slot':'slot','namespace':'allocation_namespace','backing':'allocation_backing'}[change];entry[field]='other';config['description']='<!--BOSH:'+json.dumps({'bosh_parked_disks':{disk['stable_token']:entry}})+'-->'
   with self.assertRaises(RuntimeError,msg=change):obj.require_detached_or_parked_disk(disk)
 def test_planning_validates_holder_without_deletion(self):
  obj,row=self.fixture();self.assertEqual(obj.plan_owned_orphan_disks(),[('pvz-owned',row)]);obj.operation.assert_not_called();obj.require_detached_or_parked_disk.assert_called_once()
 def test_global_disk_task_binds_actual_result_and_emitted_id(self):
  for failure in [None,'wrong_cid','wrong_description','deployment','stale','missing_finished','other_stage','wrong_index','duplicate']:
   obj=DirectorScenarios.__new__(DirectorScenarios);obj.name='owned';obj.task_evidence=[];obj.runner=types.SimpleNamespace(active_resources={'director_workflow':{}},checkpoint=Mock());task={'id':11,'deployment':None,'description':'delete orphan disks','result':'orphaned disk(s)...','state':'done'};events=[{'stage':'Deleting orphaned disks','task':'Deleting orphaned disk pvz-owned','total':1,'index':1,'state':'started','progress':0},{'stage':'Deleting orphaned disks','task':'Deleting orphaned disk pvz-owned','total':1,'index':1,'state':'finished','progress':100}]
   if failure=='wrong_cid':events[0]['task']='Deleting orphaned disk pvz-other'
   if failure=='missing_finished':events.pop()
   if failure=='other_stage':events[1]['stage']='Another stage'
   if failure=='wrong_index':events[1]['index']=2
   if failure=='duplicate':events.append(dict(events[1]))
   if failure=='wrong_description':task['description']='delete disk'
   if failure=='deployment':task['deployment']='other'
   obj.tasks=Mock(side_effect=[{'11':task} if failure=='stale' else {},{'11':task}]);obj.bosh=Mock(side_effect=[{'task_ids':['11']},events])
   if failure:
    with self.assertRaises(RuntimeError):obj.operation(['delete-disk','pvz-owned'],scoped=False)
   else:
    result=obj.operation(['delete-disk','pvz-owned'],scoped=False);self.assertEqual(result['tasks'][0]['disk_cid'],'pvz-owned')
 def test_actual_director_truncated_task_and_event_capture(self):
  task={'id': 11, 'state': 'done', 'description': 'delete orphan disks', 'timestamp': 1789085964, 'started_at': 1789085960, 'result': 'orphaned disk(s)...', 'user': 'admin', 'deployment': None, 'context_id': ''}
  events=[{'time': 1789085960, 'stage': 'Deleting orphaned disks', 'tags': [], 'total': 1, 'task': 'Deleting orphaned disk pvz-H4sIAAAAAAAC_zTMTW7DIBQE4LvMmtfy4yTAbXgPLFtKjAs0XUS-e2VFWc030mheeCJimzvtpfW1j7INktIG2Wi01eHy_XzQW8S1LyTFFW2SDc5rm26e0v1ehcyUtc_BUL4K0xTmC4XMiawY5yftLbv560fqn4XCA_GFuo9-Zl67pJYRUTcorHUsraSzGyj0_tahkDZZakMc7bcozIj4_K3nhvdMQfyNWSeXp8CsrziO_wEAa9plP-IAAAA', 'index': 1, 'state': 'started', 'progress': 0}, {'time': 1789085964, 'stage': 'Deleting orphaned disks', 'tags': [], 'total': 1, 'task': 'Deleting orphaned disk pvz-H4sIAAAAAAAC_zTMTW7DIBQE4LvMmtfy4yTAbXgPLFtKjAs0XUS-e2VFWc030mheeCJimzvtpfW1j7INktIG2Wi01eHy_XzQW8S1LyTFFW2SDc5rm26e0v1ehcyUtc_BUL4K0xTmC4XMiawY5yftLbv560fqn4XCA_GFuo9-Zl67pJYRUTcorHUsraSzGyj0_tahkDZZakMc7bcozIj4_K3nhvdMQfyNWSeXp8CsrziO_wEAa9plP-IAAAA', 'index': 1, 'state': 'finished', 'progress': 100}]
  cid=events[0]['task'].removeprefix('Deleting orphaned disk ');obj=DirectorScenarios.__new__(DirectorScenarios);obj.name='owned';obj.task_evidence=[];obj.runner=types.SimpleNamespace(active_resources={'director_workflow':{}},checkpoint=Mock());obj.tasks=Mock(side_effect=[{}, {'11':task}]);obj.bosh=Mock(side_effect=[{'task_ids':['11']},events]);self.assertEqual(obj.operation(['delete-disk',cid],scoped=False)['tasks'][0]['disk_cid'],cid)
 def test_lxc_and_malformed_qemu_slots_refuse(self):
  for row,config in [({'type':'lxc','vmid':100,'node':'n'},{}),({'type':'qemu','vmid':100,'node':'n'},{'scsi1':123})]:
   obj,_=self.fixture();del obj.require_detached_or_parked_disk;v=Mock();obj.runner=types.SimpleNamespace(verifier=v);v._get.return_value=[row];v.qemu_config.return_value=config
   with self.assertRaises(RuntimeError):obj.require_detached_or_parked_disk(obj.service['disks'][0])
if __name__=='__main__':unittest.main()
