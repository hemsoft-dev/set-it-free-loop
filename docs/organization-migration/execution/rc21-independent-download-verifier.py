import subprocess,os,json,hashlib,pathlib,datetime
root=pathlib.Path('/home/franz/github/hemsoft/set-it-free-loop/.git/merge-mission')
directory=root/'rc21-clean-download';directory.mkdir(exist_ok=True)
assets=directory/'assets';assets.mkdir(exist_ok=True)
repo='hemsoft-dev/set-it-free-loop';tag='v2.1.0-rc.21';sha=json.loads((root/'pr155-main-delivery.json').read_text())['merge_sha']
def run(argv,env=None):
    result=subprocess.run(argv,env=env,capture_output=True,text=True)
    assert result.returncode==0, result.stderr
    return result.stdout
assert run(['gh','api','user','--jq','.login']).strip()=='HemSoft'
release=json.loads(run(['gh','api',f'repos/{repo}/releases/tags/{tag}']))
assert release['immutable'] and release['prerelease'] and not release['draft']
ref=json.loads(run(['gh','api',f'repos/{repo}/git/ref/tags/{tag}']))
assert ref['object']['type']=='commit' and ref['object']['sha']==sha
run(['gh','release','download',tag,'--repo',repo,'--dir',str(assets)])
sums={line.split()[1]:line.split()[0] for line in (assets/'SHA256SUMS').read_text().splitlines()}
for name,digest in sums.items():
    assert '/' not in name and '\\' not in name
    assert hashlib.sha256((assets/name).read_bytes()).hexdigest()==digest
attestation=json.loads(run(['gh','release','verify',tag,'--repo',repo,'--format','json']))
(directory/'signed-release-verification.json').write_text(json.dumps(attestation,indent=2)+'\n')
env=os.environ.copy();env['XDG_DATA_HOME']=str(directory/'isolated-data')
installed=directory/'isolated-data/gh/extensions/gh-sfl/gh-sfl'
stdout=run(['pwsh','-NoProfile','-File',str(assets/'install-gh-sfl-hemsoft.ps1'),'-ReleaseVersion','2.1.0-rc.21','-WorkDir',str(directory/'download')],env)
(directory/'installer.log').write_text(stdout)
assert hashlib.sha256(installed.read_bytes()).hexdigest()==sums['gh-sfl_2.1.0-rc.21_linux_amd64']
version=run(['gh','sfl','version'],env)
assert '2.1.0-rc.21' in version and 'Could not check for updates' not in version
(directory/'version.log').write_text(version)
status=run(['gh','sfl','status','--repo','hemsoft-dev/sfl-migration-pilot-private'],env)
assert 'Could not resolve latest synchronized release' not in status
(directory/'default-status.log').write_text(status)
receipt={'observed_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'repository':repo,'source_sha':sha,'version':'2.1.0-rc.21','release_id':release['id'],'immutable':True,'digests':sums,'signed_release_verified':True,'version_output':version,'default_status_exit_code':0,'installer_default_source':repo,'isolated_data_home':env['XDG_DATA_HOME'],'pilot_sync_complete':False}
(root/'rc21-independent-download-proof.json').write_text(json.dumps(receipt,indent=2)+'\n')
print(json.dumps({'release_id':release['id'],'checksum_verified':True,'version':version.strip(),'default_status':'passed'}))
