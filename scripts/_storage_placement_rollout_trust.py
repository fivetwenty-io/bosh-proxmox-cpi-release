"""Bind replacement Director SSH trust through the existing verified PVE endpoint."""
from __future__ import annotations
import base64,copy,hashlib,ipaddress,json,os,re,shlex,stat,struct,subprocess,tempfile,time
from pathlib import Path
from urllib.parse import urlsplit

GUEST_IDENTITY = r'''import pathlib,json,urllib.request
key=pathlib.Path('/etc/ssh/ssh_host_ed25519_key.pub').read_text().strip()
product=pathlib.Path('/sys/class/dmi/id/product_uuid').read_text().strip().lower()
config=json.loads(pathlib.Path('/var/vcap/jobs/director/config/director.yml').read_text())
port=config.get('port')
if type(port) is not int or not 1<=port<=65535:raise RuntimeError('Director port invalid')
with urllib.request.build_opener(urllib.request.ProxyHandler({})).open('http://127.0.0.1:'+str(port)+'/info',timeout=10) as response:raw=response.read(1024*1024+1)
if len(raw)>1024*1024:raise RuntimeError('Director identity response exceeds bound')
print(json.dumps({'public_key':key,'product_uuid':product,'director_uuid':json.loads(raw).get('uuid')}))
'''

def require(ok,message):
    if not ok:raise RuntimeError(message)
def digest(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()
def sync_directory(path):
    fd=os.open(path,os.O_RDONLY)
    try:os.fsync(fd)
    finally:os.close(fd)
def retain(path,value):
    with path.open('x') as stream:json.dump(value,stream,indent=2);stream.flush();os.fsync(stream.fileno())
    sync_directory(path.parent)
def private_file(path):
    require(path.is_absolute() and not path.is_symlink(), 'Private SSH path must be an absolute regular file')
    metadata=path.stat();require(stat.S_ISREG(metadata.st_mode) and metadata.st_mode & 0o077 == 0, 'SSH file permissions are not private')
def transport_binding(director):
    ssh=director.fixture['ssh'];config=director.runner.base_config;pmx=Path(os.environ['PVE_VERIFY_PMX_BIN'])
    return {'known_hosts_path':ssh['known_hosts_file'],'known_hosts_sha256':digest(ssh['known_hosts_file']),'identity_path':ssh['identity_file'],'identity_sha256':digest(ssh['identity_file']),'pmx_path':str(pmx),'pmx_sha256':digest(pmx),'config_sha256':hashlib.sha256(json.dumps(config,sort_keys=True,separators=(',',':')).encode()).hexdigest(),'ssh':dict(ssh)}

def retain_snapshot(source,target):
    raw=Path(source).read_bytes()
    with target.open('xb') as stream:
        os.chmod(target,0o600);stream.write(raw);stream.flush();os.fsync(stream.fileno())
    sync_directory(target.parent)
    sha=hashlib.sha256(raw).hexdigest()
    require(digest(source)==sha,'State or variables changed during snapshot')
    return {'path':str(target),'sha256':sha}

def immutable_update(out):
    out=Path(out);attempt=json.loads((out/'attempt.json').read_text());result=json.loads((out/'result.json').read_text())
    require(result.get('attempt_sha256')==digest(out/'attempt.json'),'Create-env attempt changed')
    for container,phase in ((attempt,'before'),(result,'after')):
        for kind in ('state','vars'):
            item=container[kind+'_'+phase+'_snapshot']
            require(item=={'path':str(out/(kind+'-'+phase+'.json')),'sha256':container[kind+'_'+phase+'_sha256']} and digest(item['path'])==item['sha256'],'Retained phase snapshot changed')
    require(digest(attempt['manifest_path'])==attempt['manifest_sha256'] and digest(attempt['archive_path'])==attempt['archive_sha256'] and digest(out/'stdout')==result['stdout_sha256'] and digest(out/'stderr')==result['stderr_sha256'],'Create-env input or output evidence changed')
    return attempt,result

def begin_update(director,state_path,vars_path,manifest_path,archive,phase):
    out=Path(director.command_evidence_dir)/('create-env-'+phase);out.mkdir(mode=0o700);sync_directory(out.parent)
    state=retain_snapshot(state_path,out/'state-before.json');variables=retain_snapshot(vars_path,out/'vars-before.json')
    retain(out/'attempt.json',{'phase':phase,'transport':transport_binding(director),'vars_path':str(vars_path),'vars_before_sha256':variables['sha256'],'state_before_sha256':state['sha256'],'state_before_snapshot':state,'vars_before_snapshot':variables,'manifest_path':str(manifest_path),'manifest_sha256':digest(manifest_path),'archive_path':archive['path'],'archive_sha256':archive['sha256']})
    return out

def finish_update(out,state_path,result=None,error=None):
    def text_file(name,value):
        value=value.decode('utf-8','replace') if isinstance(value,bytes) else str(value or '')
        require(len(value.encode())<=16*1024*1024,'Create-env diagnostic exceeds bound')
        path=out/name
        with path.open('x') as stream:os.chmod(path,0o600);stream.write(value);stream.flush();os.fsync(stream.fileno())
        sync_directory(out);return digest(path)
    source=result if result is not None else error
    try:
        attempt=json.loads((out/'attempt.json').read_text())
        stdout=text_file('stdout',getattr(source,'stdout',''));stderr=text_file('stderr',getattr(source,'stderr',''))
        state=retain_snapshot(state_path,out/'state-after.json');variables=retain_snapshot(attempt['vars_path'],out/'vars-after.json')
        retain(out/'result.json',{'attempt_sha256':digest(out/'attempt.json'),'returncode':getattr(result,'returncode',None),'error_type':type(error).__name__ if error else None,'state_after_sha256':state['sha256'],'vars_after_sha256':variables['sha256'],'state_after_snapshot':state,'vars_after_snapshot':variables,'stdout_sha256':stdout,'stderr_sha256':stderr})
    except (OSError,RuntimeError,ValueError,KeyError):
        if error is None and getattr(result,'returncode',None)==0:raise

def successful_update(director,state_path,phase):
    out=Path(director.command_evidence_dir)/('create-env-'+phase);attempt,result=immutable_update(out)
    require(attempt.get('transport')==transport_binding(director),'Transport identity changed during create-env')
    require(result.get('vars_after_sha256')==digest(attempt['vars_path']),'Create-env vars changed after successful result')
    require(attempt.get('phase')==phase and result.get('returncode')==0 and result.get('error_type') is None and result.get('attempt_sha256')==digest(out/'attempt.json') and result.get('state_after_sha256')==digest(state_path),'Successful create-env receipt missing or changed')
    require(digest(attempt['manifest_path'])==attempt['manifest_sha256'] and digest(attempt['archive_path'])==attempt['archive_sha256'] and digest(out/'stdout')==result['stdout_sha256'] and digest(out/'stderr')==result['stderr_sha256'],'Create-env input or output evidence changed')
    return {'path':str(out/'result.json'),'sha256':digest(out/'result.json')}

def state_identity(path):
    value=json.loads(Path(path).read_text());vmid=str(value.get('current_vm_cid',''))
    require(re.fullmatch(r'[1-9][0-9]{0,8}',vmid),'Replacement state VM identity invalid')
    disks=value.get('disks');require(isinstance(disks,list) and disks and all(isinstance(x,dict) and isinstance(x.get('cid'),str) and x['cid'] for x in disks),'Replacement state disks unavailable')
    cids=sorted(x['cid'] for x in disks);require(len(set(cids))==len(cids),'Replacement state disk identity repeated')
    return {'vm_cid':vmid,'disk_cids':cids,'state_sha256':digest(path)}
def vm_node(verifier,vmid):
    rows=verifier._get('/cluster/resources?type=vm');require(isinstance(rows,list) and len(rows)<=10000,'PVE replacement inventory incomplete')
    seen=set();matches=[]
    for row in rows:
        require(isinstance(row,dict) and row.get('type') in ('qemu','lxc'),'PVE replacement inventory malformed')
        identity=str(row.get('vmid',''));node=row.get('node')
        require(re.fullmatch(r'[1-9][0-9]{0,8}',identity) and identity not in seen and isinstance(node,str) and re.fullmatch(r'[A-Za-z0-9_.-]+',node),'PVE replacement identity malformed or repeated');seen.add(identity)
        if identity==vmid:
            require(row['type']=='qemu','Director replacement is not a QEMU VM');matches.append(node)
    require(len(matches)==1,'Director replacement VM is not unique');return matches[0]
def product_uuid(config):
    require(isinstance(config,dict) and isinstance(config.get('smbios1'),str),'PVE SMBIOS identity missing')
    values=[v.split('=',1)[1].lower() for v in config['smbios1'].split(',') if v.startswith('uuid=')]
    require(len(values)==1 and re.fullmatch(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}',values[0]) and values[0]!='00000000-0000-0000-0000-000000000000','PVE SMBIOS identity invalid');return values[0]
def public_key(value):
    require(isinstance(value,str) and len(value)<=1024 and '\n' not in value and '\r' not in value,'SSH public key malformed');parts=value.split();require(len(parts)>=2 and parts[0]=='ssh-ed25519','Expected Ed25519 host identity')
    try:raw=base64.b64decode(parts[1],validate=True)
    except ValueError:raise RuntimeError('SSH public key encoding invalid') from None
    require(raw[:4]==struct.pack('>I',11) and raw[4:15]==b'ssh-ed25519' and raw[15:19]==struct.pack('>I',32) and len(raw)==51,'SSH public key wire identity invalid')
    return parts[0]+' '+parts[1]
def qga_credentials(config):
    require(config.get('verify_ssl') is True and isinstance(config.get('pve_ca_cert'),str) and config['pve_ca_cert'],'Verified PVE TLS with pinned CA is required')
    value=config.get('api_token');require(isinstance(value,str),'PVE token unavailable for native pmx capture')
    identity,separator,secret=value.partition('=');user,bang,token=identity.partition('!');require(separator and bang and re.fullmatch(r'[^!\s=]+',user) and re.fullmatch(r'[^!\s=]+',token) and secret and not any(c.isspace() for c in secret),'PVE token unavailable for native pmx capture')
    return user,token,secret

def qga_identity(config,node,vmid,pmx):
    user,token,secret=qga_credentials(config)
    with tempfile.TemporaryDirectory(prefix='rollout-host-trust-') as directory:
        root=Path(directory);ca=root/'ca.pem';ca.write_text(config['pve_ca_cert']);ca.chmod(0o600)
        context={'host':config['host'],'port':config.get('port',8006),'protocol':'https','auth':{'type':'token','username':user,'token-id':token,'secret':'${STORAGE_ROLLOUT_PMX_SECRET}'},'tls':{'insecure':False,'ca-cert':str(ca)}}
        path=root/'pmx.json';path.write_text(json.dumps({'current-context':'rollout-trust','contexts':{'rollout-trust':context}}));path.chmod(0o600)
        env={k:v for k,v in os.environ.items() if not k.startswith(('PMX_','PVE_','STORAGE_CERT_'))};env['STORAGE_ROLLOUT_PMX_SECRET']=secret
        base=[str(pmx),'--config',str(path),'--context','rollout-trust','--output','json','--no-log','--node',node,'pve','qemu','agent']
        deadline=time.monotonic()+90
        def call(args):
            remaining=deadline-time.monotonic();require(remaining>0,'QGA identity deadline exceeded')
            q=subprocess.run(base+args,env=env,capture_output=True,text=True,timeout=min(45,remaining),check=False);require(q.returncode==0 and len(q.stdout)<=1024*1024,'Native pmx QGA identity inspection failed');return json.loads(q.stdout)
        receipt=call(['exec',vmid,'--','python3','-c',GUEST_IDENTITY]);require(type(receipt.get('pid')) is int and receipt['pid']>0,'QGA identity process receipt invalid')
        for _ in range(20):
            status=call(['exec-status',vmid,'--pid',str(receipt['pid'])])
            if status.get('exited') is True:break
            time.sleep(1)
        require(status.get('exited') is True and status.get('exitcode')==0 and status.get('out-truncated',False) is False and isinstance(status.get('out-data'),str) and len(status['out-data'])<=8192,'QGA identity read did not complete')
        return json.loads(status['out-data']),receipt['pid']

def preflight_rollout_trust(director,expected_uuid,phase,after_update=False):
    require(phase in ["candidate_manifest", "scalar_manifest", "baseline_manifest"], "Unexpected rollout phase")
    require(not (Path(director.command_evidence_dir)/("host-trust-"+phase)).exists(), "Rollout trust phase already attempted")
    require(after_update or not (Path(director.command_evidence_dir)/("create-env-"+phase)).exists(), 'Rollout update phase already attempted')
    fixture=director.fixture;ssh=copy.deepcopy(fixture['ssh']);host=ssh['host'];ipaddress.ip_address(host)
    require(urlsplit(fixture['environment']).hostname==host,'SSH endpoint differs from authenticated Director endpoint')
    require(isinstance(expected_uuid,str) and re.fullmatch(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}',expected_uuid),'Preserved Director UUID invalid')
    previous=Path(ssh['known_hosts_file']);key=Path(ssh['identity_file']);require(previous.is_absolute() and key.is_absolute() and previous.is_file() and key.is_file(),'Existing private SSH identities required')
    private_file(previous);private_file(key);require(re.fullmatch(r'[a-z_][a-z0-9_-]*',ssh['user']),'SSH username invalid')
    previous_sha=digest(previous);key_sha=digest(key);config=director.runner.base_config;verifier=director.runner.verifier
    qga_credentials(config)
    require(verifier._transport=='pmx' and verifier.verify_ssl is True and verifier.host==config['host'] and verifier.port==int(config.get('port',8006)) and verifier._ca==config.get('pve_ca_cert'),'PVE verifier transport or endpoint differs')
    pmx=Path(os.environ.get('PVE_VERIFY_PMX_BIN',''));require(pmx.is_absolute() and pmx.is_file(),'Explicit bound pmx binary required');pmx_sha=digest(pmx)
    return fixture,ssh,host,previous,key,previous_sha,key_sha,config,verifier,pmx,pmx_sha

def refresh_rollout_trust(director,state_path,expected_uuid,phase):
    from _storage_placement_director import observed_director_vmid
    require(phase in ['candidate_manifest','scalar_manifest','baseline_manifest'],'Unexpected rollout phase')
    fixture,ssh,host,previous,key,previous_sha,key_sha,config,verifier,pmx,pmx_sha=preflight_rollout_trust(director,expected_uuid,phase,after_update=True)
    update=successful_update(director,state_path,phase)
    state=state_identity(state_path);node=vm_node(verifier,state['vm_cid']);product=product_uuid(verifier.qemu_config(state['vm_cid'],node))
    out=Path(director.command_evidence_dir)/('host-trust-'+phase);out.mkdir(mode=0o700);sync_directory(out.parent);retain(out/'attempt.json',{'state':state,'previous_known_hosts':str(previous),'previous_known_hosts_sha256':previous_sha,'ssh_identity_sha256':key_sha,'pmx_sha256':pmx_sha})
    payload,pid=qga_identity(config,node,state['vm_cid'],pmx)
    require(isinstance(payload,dict) and payload.get('product_uuid')==product and payload.get('director_uuid')==expected_uuid,'Replacement guest physical or Director identity differs')
    canonical=public_key(payload.get('public_key'));require(observed_director_vmid(verifier,product)==state['vm_cid'],'Guest SMBIOS does not uniquely identify replacement VM')
    known=out/'known_hosts'
    with known.open('x') as stream:stream.write(host+' '+canonical+'\n');stream.flush();os.fsync(stream.fileno())
    known.chmod(0o600);sync_directory(out)
    new_ssh={**ssh,'known_hosts_file':str(known)}
    argv=['ssh','-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o','IdentitiesOnly=yes','-o','ConnectTimeout=10','-o','UserKnownHostsFile='+str(known),'-i',str(key),ssh['user']+'@'+host,'sudo -n python3 -c '+shlex.quote(GUEST_IDENTITY)]
    result=subprocess.run(argv,capture_output=True,text=True,timeout=45,check=False);require(result.returncode==0 and len(result.stdout)<=8192,'Strict replacement SSH verification failed');observed=json.loads(result.stdout)
    require(observed==payload,'SSH guest differs from the pmx/QGA identity')
    require(state_identity(state_path)==state and vm_node(verifier,state['vm_cid'])==node and product_uuid(verifier.qemu_config(state['vm_cid'],node))==product,'Replacement state changed during trust capture')
    require(digest(previous)==previous_sha and digest(key)==key_sha and digest(pmx)==pmx_sha,'Bound transport identity changed during trust capture')
    require(successful_update(director,state_path,phase)==update,'Create-env receipt changed during trust capture')
    proof={'create_env_result':update,'state':state,'node':node,'product_uuid':product,'director_uuid':expected_uuid,'pmx_tls_verified':True,'strict_ssh_passed':True,'qga_pid':pid,'known_hosts_sha256':digest(known),'previous_known_hosts_sha256':previous_sha,'pve_endpoint':verifier.base,'pve_ca_sha256':hashlib.sha256(config['pve_ca_cert'].encode()).hexdigest(),'pmx_sha256':pmx_sha,'ssh':new_ssh}
    retain(out/'completion.json',proof)
    director.fixture={**fixture,'ssh':new_ssh};director.observer.fixture=copy.deepcopy(director.fixture)
    director.runner.report.setdefault('rollout_host_trust',[]).append({'phase':phase,'completion':str(out/'completion.json'),'sha256':digest(out/'completion.json')});director.runner.checkpoint()
    return proof
