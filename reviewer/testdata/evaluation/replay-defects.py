#!/usr/bin/env python3
"""Replay six source-only regressions from immutable Git objects, without fetch."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[3]
WORKFLOW = '.github/workflows/sfl-pr-review-auto.yml'
PRIMARY_TEST = 'deployment/tests/test-final-review-history.cjs'
CASES = [
    (154, '3614b31b5f9f27c2580c024925bb30a5829bdc0a', '73607a1b28beb31e794be6498e0d244866b45ab5', 'local', 'edited-supersession', 'edited newer registration must still supersede'),
    (157, '8fba49c1b67d0d2c80e5bf9d2e5a4c549823d084', '73607a1b28beb31e794be6498e0d244866b45ab5', 'local', 'depart-default', 'departure from default must invalidate'),
    (158, 'd20858e5416974992544ad6fe4ed11c36daf3eea', '8c53c43a860a0471308f11501352a9f2b43c7efd', PRIMARY_TEST, 'probe-retarget', 'nondefault-to-nondefault retarget'),
    (159, 'e3c8bd94c5ba7375231b617bf4013598e9ebc608', '8c53c43a860a0471308f11501352a9f2b43c7efd', PRIMARY_TEST, 'probe-active', 'ordinary administrator comment'),
    (160, 'a293479a7615e7379e4ccc67d95bcabd43b0d802', 'afdf3bca1631b98ad87b9aa96ed87d455917e63b', PRIMARY_TEST, 'probe-active', 'writer request before registry visibility'),
    (161, '31482a869638604a8d6bb52e2c9c24e9d9d51589', '104648b0a9f2709c15d2cfb6c9b7d9dc49e171b3', PRIMARY_TEST, 'probe-active', 'quoted marker'),
]


def git_source(revision, path):
    env = {'PATH': os.environ['PATH'], 'GIT_CONFIG_NOSYSTEM': '1',
           'GIT_CONFIG_GLOBAL': '/dev/null', 'GIT_NO_REPLACE_OBJECTS': '1',
           'GIT_NO_LAZY_FETCH': '1', 'LC_ALL': 'C'}
    return subprocess.check_output(
        ['git', '--no-replace-objects', '--no-lazy-fetch', '-C', str(ROOT),
         'show', revision + ':' + path], env=env)


def main():
    results = []
    with tempfile.TemporaryDirectory(prefix='sfl-source-label-probes-') as temp:
        directory = Path(temp)
        for number, head, fix, test, mode, assertion in CASES:
            script = directory / f'{number}.cjs'
            script.write_bytes((Path(__file__).with_name('source-probes.cjs').read_bytes()
                                if test == 'local' else git_source(fix, test)))
            for phase, revision, expected in [('before', head, 1), ('after', fix, 0)]:
                source = directory / f'{number}-{phase}.yml'
                source.write_bytes(git_source(revision, WORKFLOW))
                run = subprocess.run(['node', str(script), str(source), mode],
                                     capture_output=True, text=True, timeout=30)
                if run.returncode != expected or (phase == 'before' and assertion not in run.stderr):
                    sys.exit(f'PR {number} {phase}: unexpected regression result\n{run.stderr}')
                results.append({'candidate': number, 'phase': phase, 'source': revision,
                                'test_revision': None if test == 'local' else fix,
                                'assertion': assertion, 'exit_code': run.returncode})
    print(json.dumps(results, indent=2))


if __name__ == '__main__':
    main()
