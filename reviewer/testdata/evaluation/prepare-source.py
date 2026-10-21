#!/usr/bin/env python3
"""Provision a new offline fixture store from trusted local Git plus a bundle."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


def fail(message):
    raise ValueError(message)


def read_json(path):
    with path.open('rb') as stream:
        raw = stream.read((16 << 20) + 1)
    if len(raw) > 16 << 20:
        fail('fixture JSON exceeds limit')
    return json.loads(raw)


def git(*args, capture=True):
    # Only local file transport is available for this explicit provisioning
    # step. No inherited credential helpers, traces or user config are used.
    env = {'PATH': os.environ['PATH'], 'GIT_CONFIG_NOSYSTEM': '1',
           'GIT_CONFIG_GLOBAL': '/dev/null', 'GIT_ALLOW_PROTOCOL': 'file',
           'GIT_NO_REPLACE_OBJECTS': '1', 'GIT_NO_LAZY_FETCH': '1', 'LC_ALL': 'C'}
    command = ['git', '--no-replace-objects', '--no-lazy-fetch',
               '-c', 'core.hooksPath=/dev/null', *args]
    result = subprocess.run(command, env=env, capture_output=True, text=True, timeout=60)
    if result.returncode:
        fail('local Git prerequisite/provisioning failed; Git 2.46+ and full base history are required')
    return result.stdout


def run():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-git-dir', type=Path, required=True)
    parser.add_argument('--output-dir', type=Path, required=True)
    parser.add_argument('--evaluation-dir', type=Path, default=Path(__file__).resolve().parent)
    args = parser.parse_args()
    source, output, directory = args.source_git_dir, args.output_dir, args.evaluation_dir
    if not source.is_absolute() or not source.is_dir() or not output.is_absolute():
        fail('use an absolute trusted local Git directory and a new absolute output directory')
    if output.exists():
        fail('output directory must not already exist')
    manifest = read_json(directory / 'source-objects.json')
    if set(manifest) != {'Version', 'BasePrerequisite', 'Bundle', 'BundleSHA256', 'BundleBytes', 'Heads'}:
        fail('unknown/missing source manifest fields')
    if manifest['Version'] != 1 or manifest['Bundle'] != 'source-objects.bundle':
        fail('unsupported source fixture manifest')
    if not re.fullmatch('[a-f0-9]{64}', manifest['BundleSHA256']):
        fail('invalid bundle digest')
    if not re.fullmatch('[a-f0-9]{40}', manifest['BasePrerequisite']):
        fail('invalid immutable base prerequisite')
    with (directory / manifest['Bundle']).open('rb') as stream:
        bundle_bytes = stream.read((4 << 20) + 1)
    if len(bundle_bytes) > 4 << 20 or len(bundle_bytes) != manifest['BundleBytes'] or hashlib.sha256(bundle_bytes).hexdigest() != manifest['BundleSHA256']:
        fail('source bundle digest/size mismatch')
    original = read_json(directory / 'labels.json')['Cases']
    replacement = read_json(directory / 'replacement-148.json')
    expected = {x['Snapshot']['pr']: x['Snapshot']['head'] for x in original + [replacement]}
    if len(expected) != 21 or len(manifest['Heads']) != 21:
        fail('source fixture must retain the original twenty heads plus the replacement')
    refs = {}
    for head in manifest['Heads']:
        if set(head) != {'PR', 'Head', 'Ref'} or type(head['PR']) is not int or head['PR'] <= 0:
            fail('invalid source head record')
        ref = 'refs/merge-mission/evaluation/pr-' + str(head['PR'])
        if head['Ref'] != ref or not re.fullmatch('[a-f0-9]{40}', head['Head']) or expected.get(head['PR']) != head['Head'] or ref in refs:
            fail('source head differs from retained label provenance')
        refs[ref] = head['Head']
    bundle = (directory / manifest['Bundle']).resolve()
    actual = {}
    for line in git('bundle', 'list-heads', str(bundle)).splitlines():
        oid, ref = line.split(' ', 1)
        if ref in actual:
            fail('duplicate bundled source ref')
        actual[ref] = oid
    if actual != refs:
        fail('bundled heads differ from the declared source set')
    git('--git-dir=' + str(source), 'cat-file', '-e', manifest['BasePrerequisite'] + '^{commit}')
    # Only this new private store is mutated. The supplied source is read-only.
    output.mkdir(mode=0o700)
    git('init', '--bare', '--quiet', str(output))
    git('--git-dir=' + str(output), 'fetch', '--quiet', '--no-tags', '--no-write-fetch-head',
        '--no-recurse-submodules', str(source), manifest['BasePrerequisite'] + ':refs/heads/fixture-base')
    git('--git-dir=' + str(output), 'bundle', 'verify', str(bundle))
    git('--git-dir=' + str(output), 'fetch', '--quiet', '--no-tags', '--no-write-fetch-head',
        '--no-recurse-submodules', str(bundle), *[ref + ':' + ref for ref in sorted(refs)])
    for ref, oid in refs.items():
        if git('--git-dir=' + str(output), 'rev-parse', '--verify', ref).strip() != oid:
            fail('imported source head mismatch')
    print(json.dumps({'GitDir': str(output), 'SourceHeads': len(refs),
                      'BundleSHA256': manifest['BundleSHA256'], 'NetworkFetch': False}))


if __name__ == '__main__':
    try:
        run()
    except (ValueError, OSError, KeyError, TypeError, subprocess.TimeoutExpired) as error:
        sys.exit(str(error))
