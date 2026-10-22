#!/usr/bin/env python3
"""Opt-in inert ELF execution diagnostic, never aggregate qualification.

A zero exit means the comparison was collected and its unit stopped. Inspect
exec_compatible separately. qualified and noswap_qualified always remain false.
No credential, provider client, Git parser or host policy change is involved.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import runpy
import shutil
import struct
import subprocess
import time

FixtureUnit = runpy.run_path(str(Path(__file__).resolve().parents[2]/"fixture-unit.py"))["FixtureUnit"]

PARENT = r'''
import fcntl,json,os,pathlib,subprocess,sys
root=pathlib.Path(sys.argv[1]);program=root/'probe';data=program.read_bytes();payload=(root/'payload-filter.bin').read_bytes()
image=os.memfd_create('sfl-inert-sealed-elf',1|2|16);os.write(image,data);os.fchmod(image,0o500);fcntl.fcntl(image,1033,63);os.lseek(image,0,0)
ordinary=os.open(program,os.O_RDONLY|os.O_CLOEXEC)
results=[]
for confined in (False,True):
 for kind,fd in [('ordinary',ordinary),('sealed',image)]:
  for method in (('proc','direct','named') if confined else ('proc','direct')):
   mountfd=os.open('/proc/self/fd/'+str(fd),os.O_RDONLY|os.O_CLOEXEC)
   filterfd=os.memfd_create('sfl-inert-payload-filter',1|2|8);os.write(filterfd,payload);os.fchmod(filterfd,0o400);fcntl.fcntl(filterfd,1033,63);os.lseek(filterfd,0,0)
   if confined:
    args=[sys.argv[2],'--unshare-all','--unshare-user','--disable-userns','--die-with-parent','--new-session','--cap-drop','ALL','--clearenv','--ro-bind',str(program),'/trampoline']
    if method=='named':args+=['--ro-bind-fd',str(mountfd),'/bound-worker']
    args+=['--proc','/proc','--dev','/dev','--tmpfs','/tmp','--chdir','/','--seccomp',str(filterfd),'--','/trampoline',method,str(fd)]
   else:args=[str(program),method,str(fd)]
   try:
    result=subprocess.run(args,env={},pass_fds=(fd,filterfd,mountfd),capture_output=True,text=True,timeout=3)
    if len(result.stdout)>8192 or len(result.stderr)>2048:raise RuntimeError('diagnostic output bound')
    frames=[json.loads(line) for line in result.stdout.splitlines()]
    results.append({'confined':confined,'kind':kind,'method':method,'exit_code':result.returncode,'frames':frames,'stderr':result.stderr})
   finally:os.close(filterfd);os.close(mountfd)
os.close(ordinary);os.close(image)
print(json.dumps({'matrix':results}),flush=True)
'''


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def artifact(directory, name, value):
    temporary = directory/('.report-'+secrets.token_hex(16))
    raw = (json.dumps(value, indent=2)+'\n').encode()
    with temporary.open('xb') as stream:
        os.chmod(temporary, 0o600)
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, directory/name)
    fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def tracked(module, name):
    actual = (module/name).read_bytes()
    expected = subprocess.check_output(['/usr/bin/git', '-C', str(module.parent),
                                        'show', 'HEAD:reviewer/'+name])
    if actual != expected:
        raise RuntimeError('production filter/module inputs must match their committed source')
    return actual


def build(args, output, module):
    producer = output/'filter-producer'
    producer.mkdir(mode=0o700)
    source = tracked(module, 'git_payload_seccomp_linux.go')
    selected = source.decode().split('// A sealed, non-executable, close-on-exec input.', 1)
    if len(selected) != 2:
        raise RuntimeError('production filter source layout changed')
    main = selected[0].replace('package reviewer', 'package main', 1)
    main += '''
var errGitUnit = os.ErrInvalid
func main(){f,e:=gitPayloadSeccomp();if e!=nil{os.Exit(1)};var b bytes.Buffer;if binary.Write(&b,binary.LittleEndian,f)!=nil{os.Exit(1)};if _,e=os.Stdout.Write(b.Bytes());e!=nil{os.Exit(1)}}
'''
    (producer/'main.go').write_text(main)
    for name in ['go.mod', 'go.sum']:
        (producer/name).write_bytes(tracked(module, name))
    environment = {'PATH': '/usr/bin:/bin', 'GOTOOLCHAIN': 'local', 'GOWORK': 'off',
                   'GOPROXY': 'off', 'GOSUMDB': 'off', 'CGO_ENABLED': '0',
                   'GOMODCACHE': str(args.module_cache.resolve()),
                   'GOCACHE': str(output/'go-cache')}
    program = output/'filter-producer.bin'
    with (output/'filter-build.log').open('xb') as log:
        subprocess.run([args.go, '-C', str(producer), 'build', '-mod=readonly',
                        '-trimpath', '-o', str(program), '.'], env=environment,
                       stdout=log, stderr=subprocess.STDOUT, check=True, timeout=120)
    payload = subprocess.check_output([str(program)], env={'PATH': '/usr/bin:/bin'}, timeout=5)
    if not payload or len(payload) > 4096 or len(payload) % 8:
        raise RuntimeError('invalid produced filter')
    (output/'payload-filter.bin').write_bytes(payload)
    probe_source = Path(__file__).with_name('probe.c')
    shutil.copyfile(probe_source, output/'probe.c')
    with (output/'cc-build.log').open('xb') as log:
        subprocess.run([args.cc, '-static', '-Os', '-Wall', '-Wextra', '-Werror',
                        '-o', str(output/'probe'), str(output/'probe.c')],
                       env={'PATH': '/usr/bin:/bin'}, stdout=log,
                       stderr=subprocess.STDOUT, check=True, timeout=60)
    binary = (output/'probe').read_bytes()
    if len(binary) < 64 or binary[:6] != b'\x7fELF\x02\x01':
        raise RuntimeError('expected static little-endian ELF64 fixture')
    offset = struct.unpack_from('<Q', binary, 32)[0]
    size, count = struct.unpack_from('<HH', binary, 54)
    if size != 56 or count > 64 or offset+size*count > len(binary):
        raise RuntimeError('invalid ELF fixture headers')
    if any(struct.unpack_from('<I', binary, offset+size*i)[0] == 3 for i in range(count)):
        raise RuntimeError('fixture unexpectedly has an ELF interpreter')
    (output/'probe').chmod(0o500)
    head = subprocess.check_output(['/usr/bin/git', '-C', str(module.parent),
                                    'rev-parse', 'HEAD'], text=True).strip()
    return {'production_source_head': head, 'filter_source_sha256': hashlib.sha256(source).hexdigest(),
            'filter_producer_sha256': digest(program), 'filter_sha256': hashlib.sha256(payload).hexdigest(),
            'filter_bytes': len(payload), 'probe_source_sha256': digest(probe_source),
            'probe_sha256': digest(output/'probe'), 'driver_sha256': digest(Path(__file__)),
            'cc_sha256': digest(Path(args.cc).resolve()), 'bwrap_sha256': digest(Path(args.bwrap).resolve()),
            'go_sha256': digest(Path(args.go).resolve()), 'scope': 'inert static ELF comparison only'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run-inert-only', action='store_true', required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--go', required=True)
    parser.add_argument('--module-cache', type=Path, required=True)
    parser.add_argument('--cc', default='/usr/bin/cc')
    parser.add_argument('--bwrap', default='/usr/bin/bwrap')
    args = parser.parse_args()
    if any(not Path(path).is_absolute() for path in [args.go, args.cc, args.bwrap]):
        parser.error('tool paths must be absolute')
    output = args.output.absolute()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    module = Path(__file__).resolve().parents[3]
    report = {'purpose': 'inert-sealed-elf-diagnostic', 'qualified': False,
              'noswap_qualified': False, 'comparison_collected': False, 'settled': False,
              'started_at': time.time()}
    artifact(output, 'result.json', report)
    report['build'] = build(args, output, module)
    unit = 'sfl-alpha-git-exec-probe-'+secrets.token_hex(8)+'.service'
    properties = ['RuntimeMaxSec=10s', 'TimeoutStopSec=3s', 'MemoryMax=256M',
                  'MemorySwapMax=0', 'TasksMax=32', 'CPUQuota=100%',
                  'NoNewPrivileges=yes', 'LimitCORE=0',
                  'RestrictAddressFamilies=AF_UNIX AF_NETLINK',
                  'SystemCallArchitectures=native',
                  'SystemCallFilter=~io_uring_setup io_uring_enter io_uring_register',
                  'SystemCallErrorNumber=EPERM']
    report.update(unit=unit, properties=properties, intent_recorded=True)
    artifact(output, 'result.json', report)
    environment = {key: os.environ[key] for key in ['XDG_RUNTIME_DIR', 'DBUS_SESSION_BUS_ADDRESS']
                   if key in os.environ}
    environment.update(PATH='/usr/bin:/bin', LANG='C.UTF-8')
    command = ['/usr/bin/systemd-run', '--user', '--quiet', '--wait', '--pipe', '--collect',
               '--unit='+unit]+['--property='+value for value in properties]
    command += ['--', '/usr/bin/python3', '-I', '-S', '-c', PARENT, str(output), args.bwrap]
    owner = FixtureUnit(unit, 'SFL inert sealed-exec diagnostic '+unit, output, environment)
    try:
        process = owner.start(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        stdout, stderr = process.communicate(timeout=20)
        result = subprocess.CompletedProcess(command, process.returncode, stdout, stderr)
        if len(result.stdout) > 65536 or len(result.stderr) > 4096:
            raise RuntimeError('unit diagnostic output exceeds bound')
        report.update(unit_exit=result.returncode, unit_stderr=result.stderr)
        if result.returncode == 0:
            report['matrix'] = json.loads(result.stdout)['matrix']
            cells = report['matrix']
            controls = [cell for cell in cells if not cell['confined'] or cell['kind'] == 'ordinary']
            def reached_target(cell):
                return (cell['exit_code'] == 0 and bool(cell['frames'])
                        and cell['frames'][-1].get('event') == 'target'
                        and cell['frames'][-1].get('ok') is True)
            control_ok = all(reached_target(cell) for cell in controls)
            baseline = [cell for cell in cells if cell['method'] != 'named']
            observations = [frame for cell in baseline for frame in cell['frames']
                            if frame.get('event') == 'before']
            observed_ok = (len(observations) == 8 and all(
                frame['image_mode'] == 0o500 and frame['no_new_privs'] == 1
                and frame['image_uid'] == frame['uid'][1] for frame in observations))
            sealed = [cell['frames'][0] for cell in baseline
                      if cell['kind'] == 'sealed' and cell['frames']]
            sealed_ok = (len(sealed) == 4 and len({frame['image_inode'] for frame in sealed}) == 1
                         and all(frame['image_seals'] == 63 for frame in sealed))
            report['comparison_collected'] = (len(cells) == 10 and len(controls) == 7
                                               and control_ok and observed_ok and sealed_ok)
            actual = [cell for cell in cells if cell['confined'] and cell['kind'] == 'sealed'
                      and cell['method'] in ['proc', 'direct']]
            report['exec_compatible'] = len(actual) == 2 and all(reached_target(cell) for cell in actual)
    finally:
        try:
            owner.stop()
            if owner.process is not None:
                owner.process.communicate(timeout=5)
            report.update(settled=True, original_unit_stop_receipt=str(output/'unit-stopped.json'),
                          final_status=owner.observe(), finished_at=time.time())
        finally:
            owner.close()
            artifact(output, 'result.json', report)
    print(json.dumps({key: report.get(key) for key in ['comparison_collected', 'exec_compatible',
                     'settled', 'qualified', 'noswap_qualified', 'unit']}))
    if not report['comparison_collected'] or not report['settled']:
        raise SystemExit(1)


if __name__ == '__main__':
    main()
