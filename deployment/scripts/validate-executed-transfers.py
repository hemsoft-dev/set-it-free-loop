#!/usr/bin/env python3
"""Validate transfer identity evidence without claiming pre-cutover or SFL completion."""
import argparse
import base64
import datetime
import hashlib
import importlib.util
import json
import pathlib
import re
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[2]
REPLAY_SPEC = importlib.util.spec_from_file_location(
    'executed_observer_replay', ROOT / 'deployment/scripts/validate-org-rollout.py')
REPLAY = importlib.util.module_from_spec(REPLAY_SPEC)
REPLAY_SPEC.loader.exec_module(REPLAY)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read(directory, name):
    return json.loads((directory / name).read_text())


def timestamp(value):
    result = datetime.datetime.fromisoformat(value.replace('Z', '+00:00'))
    require(result.tzinfo is not None, 'Capture timestamps need a timezone')
    return result


def metadata(capture, repository_id, name):
    require(capture['method'] == 'GET' and capture['http_status'] == 200,
            'Repository identity needs a successful GET capture')
    require(capture['request_url'] == 'https://api.github.com/repos/' + name,
            'Identity capture URL must match its repository')
    data = capture['data']
    require(data['id'] == repository_id and data['full_name'] == name,
            'Captured repository identity does not match the inventory')
    return data


def refs(capture, repository, default_branch, earliest, latest):
    responses = capture['responses']
    require(len(responses) == 1, 'Expected the complete matching-refs GET response')
    response = responses[0]
    headers = response.get('response_headers')
    require(isinstance(headers, dict) and any(k.lower() == 'x-github-request-id' for k in headers) and
            not any('rel="next"' in v for k, v in headers.items() if k.lower() == 'link'),
            'Refs capture must include its original terminal pagination headers')
    require(response['method'] == 'GET' and response['http_status'] in {200, 409} and
            response['request_url'] == 'https://api.github.com/repos/' + repository + '/git/matching-refs/',
            'Refs need a successful repository-bound matching-refs capture')
    require(earliest <= timestamp(response['observed_at']) <= latest,
            'Refs capture falls outside its transfer phase')
    if response['http_status'] == 409:
        require(response['data'].get('message') == 'Git Repository is empty.' and
                response['data'].get('status') == '409' and capture['refs'] == {},
                'Only the captured empty-repository response proves an empty ref set')
        return {}
    result = {}
    for row in response['data']:
        require(row['ref'] not in result and re.fullmatch(r'[0-9a-f]{40}', row['object']['sha']) and
                row['object']['type'] in {'commit', 'tag'}, 'Captured refs are malformed or duplicated')
        result[row['ref']] = {key: row['object'][key] for key in ('sha', 'type')}
    require(result == capture['refs'], 'Ref summary must derive from the primary response')
    require('refs/heads/' + default_branch in result,
            'Nonempty repository refs must include the declared default branch')
    return result



def api_receipt(execution, name, endpoint):
    capture = read(execution, name)
    require(capture['actor'] == 'HemSoft' and capture['exit_code'] == 0 and
            capture['argv'] == ['gh', 'api', endpoint], 'Pilot API capture is not bound to its endpoint')
    return json.loads(capture['stdout'])


def verify_retained_release_signature(signed):
    """Verify retained DSSE bytes with the leaf previously trusted by gh release verify.

    The certificate fingerprint is pinned to the committed historical successful
    verifier evidence. This checks payload integrity offline, without asserting a
    new certificate-chain, timestamp or transparency-log verification.
    """
    bundle = signed['attestation']['bundle']
    certificate = base64.b64decode(bundle['verificationMaterial']['certificate']['rawBytes'], validate=True)
    require(hashlib.sha256(certificate).hexdigest() == 'a69a47367524e2ad7b911dc194178e76501b8dd5420fe8332c740157805d332a',
            'Retained release signature certificate differs from the reviewed signer')
    envelope = bundle['dsseEnvelope']
    require(envelope['payloadType'] == 'application/vnd.in-toto+json' and len(envelope['signatures']) == 1,
            'Retained release signature envelope is unsupported')
    payload_type = envelope['payloadType'].encode('utf-8')
    payload = base64.b64decode(envelope['payload'], validate=True)
    signature = base64.b64decode(envelope['signatures'][0]['sig'], validate=True)
    pae = b'DSSEv1 ' + str(len(payload_type)).encode() + b' ' + payload_type + b' ' + str(len(payload)).encode() + b' ' + payload
    public_key = subprocess.run(['openssl', 'x509', '-inform', 'DER', '-pubkey', '-noout'],
                                input=certificate, capture_output=True, check=True).stdout
    with tempfile.TemporaryDirectory(prefix='sfl-release-signature-') as directory:
        root = pathlib.Path(directory)
        (root / 'public-key.pem').write_bytes(public_key)
        (root / 'payload.pae').write_bytes(pae)
        (root / 'signature.der').write_bytes(signature)
        result = subprocess.run(['openssl', 'dgst', '-sha256', '-verify', str(root / 'public-key.pem'),
                                 '-signature', str(root / 'signature.der'), str(root / 'payload.pae')],
                                capture_output=True)
        require(result.returncode == 0, 'Retained release DSSE signature does not authenticate its payload')


def validate_pilots(directory):
    execution = directory / 'execution'
    qualification = read(execution, 'rc21-pilots-qualified-before-wider.json')
    source = '89425320ace3127a829d86e3b642fe2b31fd979e'
    version = '2.1.0-rc.21'
    require(qualification['qualified'] is True and qualification['version'] == version and
            qualification['source_sha'] == source and qualification['release_tag'] == 'v' + version and
            set(qualification['pilots']) == {'private', 'public'} and
            qualification['synthetic_negative_fixtures_distinct_from_live_execution'] is True,
            'Pilot qualification summary is invalid')
    outcomes = dict.fromkeys(['pending_request', 'overlapping_request', 'denied_permission',
                             'revoked_permission', 'permission_lookup_failure', 'malformed_output',
                             'forged_registration', 'edited_registration'], 'gate_blocked')
    outcomes.update(findings='gate_failed', new_head='stale_gate_rejected',
                    base_advance='stale_gate_rejected', unregistered_base_context='stale_gate_rejected',
                    duplicate_delivery='idempotent')
    runner_hash = hashlib.sha256((directory.parents[1] / 'deployment/tests/run-org-observer-fixtures.cjs').read_bytes()).hexdigest()
    for role, repository_id in [('private', 1408025382), ('public', 1408029795)]:
        prefix = 'rc21-' + role
        repo = 'hemsoft-dev/sfl-migration-pilot-' + role
        summary = qualification['pilots'][role]
        installed = read(execution, prefix + '-installed-observer-projected.json')
        content = installed['content'].encode()
        workflow_hash = hashlib.sha256(content).hexdigest()
        blob = hashlib.sha1(b'blob ' + str(len(content)).encode() + b'\0' + content).hexdigest()
        require(installed['repository_id'] == repository_id and installed['repository'] == repo and
                installed['path'] == '.github/workflows/sfl-pr-review-auto.yml' and
                installed['blob_sha'] == blob and ('@' + source) in installed['content'],
                'Installed pilot workflow identity or blob is invalid')
        expected_files = {prefix + '-fixture-' + scenario + '.json' for scenario in outcomes}
        require(set(summary['fixture_receipts']) == expected_files and
                len(summary['fixture_receipts']) == len(expected_files) and
                summary['all_fixture_results_validated'] is True, 'Pilot fixture coverage is invalid')
        for scenario, outcome in outcomes.items():
            fixture = read(execution, prefix + '-fixture-' + scenario + '.json')
            require(fixture['repository_id'] == repository_id and fixture['repository'] == repo and
                    fixture['deployment_sha'] == source and fixture['release_version'] == version and
                    fixture['tested_revision_sha'] == installed['revision_sha'] and
                    fixture['scenario'] == scenario and fixture['mode'] == 'workflow_fixture' and
                    fixture['outcome'] == outcome and fixture['passed'] is True and
                    fixture['runner_sha256'] == runner_hash and fixture['workflow_sha256'] == workflow_hash,
                    'Pilot fixture does not match its installed workflow and expected result')
            replay = REPLAY.replay_observer_fixture(
                json.dumps(installed, sort_keys=True), repository_id, repo, source, version,
                installed['revision_sha'], scenario, runner_hash)
            require(all(replay[k] == fixture[k] for k in ('scenario', 'mode', 'outcome', 'passed',
                    'repository_id', 'repository', 'deployment_sha', 'release_version',
                    'tested_revision_sha', 'runner_sha256', 'workflow_sha256')),
                    'Independent pilot fixture replay disagrees with its receipt')
        require(summary['execution_binding_receipt'] == prefix + '-live-execution-binding.json' and
                summary['installed_terminal_receipt'] == prefix + '-live-runtime-run.json',
                'Pilot qualification references the wrong live evidence')
        binding = read(execution, summary['execution_binding_receipt'])
        merge = read(execution, prefix + '-live-guarded-merge.json')
        merged_capture = read(execution, prefix + '-live-merged-pr-current.json')
        merged = merged_capture['data']
        require(merged_capture['actor'] == 'HemSoft' and merged_capture['exit_code'] == 0 and
                merged_capture['argv'] == ['gh', 'api', 'repos/' + repo + '/pulls/' + str(merge['pr_number'])] and
                merged['number'] == merge['pr_number'] and merged['state'] == 'closed' and merged['merged'] is True and
                merged['head']['repo']['id'] == merged['base']['repo']['id'] == repository_id and
                merged['head']['repo']['full_name'] == merged['base']['repo']['full_name'] == repo and
                merged['head']['sha'] == merge['head'] and merged['base']['sha'] == merge['base'] and
                merged['merge_commit_sha'] == merge['merge_sha'] and merged['merged_at'] == merge['merged_at'] and
                timestamp(merged['merged_at']) <= timestamp(merged_capture['started_at']) <=
                timestamp(merged_capture['completed_at']) <= timestamp(qualification['qualified_at']),
                'Pilot merge requires its authenticated post-merge PR capture')
        gate = merge['gate']
        request, native = merge['request'], merge['native_clean']
        repository_url = 'https://api.github.com/repos/' + repo
        require(all(comment['url'] == repository_url + '/issues/comments/' + str(comment['id']) and
                    comment['issue_url'] == repository_url + '/issues/' + str(merge['pr_number']) and
                    comment['html_url'] == 'https://github.com/' + repo + '/pull/' +
                    str(merge['pr_number']) + '#issuecomment-' + str(comment['id'])
                    for comment in (request, native)), 'Pilot request binding URLs are invalid')
        check_prs = gate['pull_requests']
        require(gate['url'] == repository_url + '/check-runs/' + str(gate['id']) and
                gate['html_url'] == 'https://github.com/' + repo + '/runs/' + str(gate['id']) and
                len(check_prs) == 1 and
                check_prs[0]['number'] == merge['pr_number'] and
                check_prs[0]['url'] == repository_url + '/pulls/' + str(merge['pr_number']) and
                all(check_prs[0][side]['repo']['id'] == repository_id and
                    check_prs[0][side]['repo']['url'] == repository_url and
                    check_prs[0][side]['repo']['name'] == repo.split('/')[1] and
                    check_prs[0][side]['sha'] == merge[side] for side in ('head', 'base')),
                'Pilot gate repository and pull request binding is invalid')
        require(binding['repository_id'] == repository_id and binding['repository'] == repo and
                binding['execution_sha'] == binding['workflow_sha'] == installed['revision_sha'] == merge['base'] and
                binding['event'] == 'issue_comment' and binding['workflow_path'] == installed['path'] and
                binding['check_run_id'] == gate['id'] and binding['external_id'] == gate['external_id'] and
                binding['request_id'] == request['id'] and binding['native_comment_id'] == native['id'] and
                binding['native_app_id'] == native['performed_via_github_app']['id'] == 1144995 and
                native['user']['id'] == 199175422 and request['user']['id'] == 8227352 and
                request['created_at'] == request['updated_at'] <= native['created_at'] and
                native['body'].startswith("Codex Review: Didn't find any major issues.") and
                any(merge['head'].startswith(v) for v in re.findall(r'Reviewed commit:\*\* `([0-9a-f]{7,40})`', native['body'])) and
                f'head={merge["head"]};base={merge["base"]};context=' in request['body'],
                'Pilot live review identity or request binding is invalid')
        marker = re.search(r'<!-- sfl-gate-execution:([A-Za-z0-9+/=]+) -->', gate['output']['summary'])
        require(marker is not None, 'Pilot gate has no execution binding')
        decoded = json.loads(base64.b64decode(marker[1]))
        require(set(decoded) == {'repository_id', 'repository', 'run_id', 'run_attempt',
                                 'execution_sha', 'workflow_sha', 'workflow_path', 'event',
                                 'check_run_id', 'external_id'} and
                all(decoded[k] == binding[k] for k in decoded) and gate['head_sha'] == merge['head'] and
                gate['name'] == 'SFL Reviewer Gate Runner' and
                gate['external_id'].startswith(f'sfl-codex-review:pull:{merge["pr_number"]}:base:{merge["base"]}:context:') and
                timestamp(request['created_at']) < timestamp(native['created_at']) <=
                timestamp(gate['started_at']) <= timestamp(gate['completed_at']) <=
                timestamp(merge['observed_at']) <= timestamp(merge['merged_at']) and
                gate['app']['id'] == 15368 and gate['status'] == 'completed' and gate['conclusion'] == 'success' and
                gate['external_id'].endswith(':artifact:c' + str(native['id'])) and
                f':request:{request["id"]}:' in gate['external_id'] and
                f':base:{merge["base"]}:' in gate['external_id'] and
                merge['source_sha'] == source and merge['release_version'] == version and
                all(c['bucket'] in {'pass', 'skipping'} for c in merge['checks']),
                'Pilot guarded merge or gate is invalid')
        endpoint = 'repos/' + repo + '/actions/runs/' + str(binding['run_id'])
        run = api_receipt(execution, summary['installed_terminal_receipt'], endpoint)
        require(run['id'] == binding['run_id'] and run['repository']['id'] == repository_id and
                run['head_sha'] == installed['revision_sha'] and run['path'] == installed['path'] and
                run['event'] == 'issue_comment' and run['run_attempt'] == binding['run_attempt'] and
                run['status'] == 'completed' and run['conclusion'] == 'success',
                'Pilot installed live run did not complete successfully')
        jobs = api_receipt(execution, prefix + '-live-runtime-jobs.json', endpoint + '/jobs?per_page=100')
        run_capture = read(execution, summary['installed_terminal_receipt'])
        jobs_capture = read(execution, prefix + '-live-runtime-jobs.json')
        require(timestamp(run['created_at']) <= timestamp(run['run_started_at']) <= timestamp(run['updated_at']) <=
                timestamp(run_capture['started_at']) <= timestamp(run_capture['completed_at']) <=
                timestamp(jobs_capture['started_at']) <= timestamp(jobs_capture['completed_at']) <=
                timestamp(qualification['qualified_at']) and
                jobs['total_count'] == len(jobs['jobs']) and
                all(job['status'] == 'completed' and
                    timestamp(job['completed_at']) <= timestamp(jobs_capture['started_at']) for job in jobs['jobs']),
                'Pilot live run and jobs capture must follow terminal completion')
        observers = [j for j in jobs['jobs'] if j['name'] == 'Observe authenticated Codex review']
        require(len(observers) == 1 and observers[0]['run_id'] == run['id'] and
                observers[0]['head_sha'] == run['head_sha'] and observers[0]['conclusion'] == 'success',
                'Pilot installed observer job did not pass')
        log = read(execution, binding['pending_failure_primary'])
        pending_capture = read(execution, prefix + '-pending-request-failed-run.json')
        pending = pending_capture['data']
        require(pending_capture['method'] == 'GET' and pending_capture['http_status'] == 200 and
                pending_capture['actor'] == 'HemSoft' and pending_capture['request_url'] ==
                'https://api.github.com/repos/' + repo + '/actions/runs/' + str(pending['id']) and
                pending['repository']['id'] == repository_id and pending['repository']['full_name'] == repo and
                pending['head_sha'] == installed['revision_sha'] and pending['path'] == installed['path'] and
                pending['event'] == 'issue_comment' and pending['status'] == 'completed' and
                pending['conclusion'] == 'failure' and
                timestamp(request['created_at']) <= timestamp(pending['created_at']) <=
                timestamp(pending['run_started_at']) <= timestamp(pending['updated_at']) <=
                timestamp(native['created_at']) and
                timestamp(pending['updated_at']) <= timestamp(pending_capture['observed_at']),
                'Pilot pending-failure run is not bound to its registered request')
        require(binding['pending_failure_expected'] is True and log['exit_code'] == 0 and
                log['actor'] == 'HemSoft' and log['argv'] == ['gh', 'run', 'view', str(pending['id']),
                    '--repo', repo, '--log-failed'] and
                timestamp(pending['updated_at']) <= timestamp(log['started_at']) <= timestamp(log['completed_at']) and
                f'Registered Codex review request {request["id"]} is pending' in log['stdout'],
                'Pilot pending-failure primary is missing or mismatched')
        completion = read(execution, prefix + '-completed-live-pilot-validation.json')
        require(summary['completion'] == completion and completion['qualified'] is True and
                completion['repository'] == repo and completion['live_merge_sha'] == merge['merge_sha'] and
                completion['unrelated_policy_preserved'] is True and completion['after_terminal_captures'] is True,
                'Pilot completion summary disagrees with its primary evidence')
        require(len(merge['rules']) == 1 and merge['protection'] is None and
                merge['rules'][0]['ruleset_id'] == completion['owned_gate_id'] and
                merge['rules'][0]['parameters']['strict_required_status_checks_policy'] is True and
                merge['rules'][0]['parameters']['required_status_checks'] ==
                [{'context': 'SFL Reviewer Gate Runner', 'integration_id': 15368}],
                'Pilot live merge did not use its strict required Actions gate')
        for operation in ('init', 'sync', 'status'):
            repeat = read(execution, prefix + '-post-live-repeat-' + operation + '.json')
            require(repeat['repository_id'] == repository_id and repeat['repository'] == repo and
                    repeat['cli_version'] == repeat['release_version'] == version and repeat['deployment_sha'] == source and
                    repeat['exit_code'] == repeat['execution']['exit_code'] == 0 and repeat['actor'] == 'HemSoft' and
                    repeat['revision_before'] == repeat['revision_after'] == merge['merge_sha'] and
                    repeat['execution']['command'] == 'gh sfl ' + operation and
                    repeat['execution']['argv'] == ['gh', 'sfl', operation, '--repo', repo] +
                    (['--tier', 'reviewer', '--source-ref', 'v' + version] if operation == 'init' else []) +
                    (['--pr'] if operation != 'status' else []) and
                    timestamp(merge['merged_at']) <= timestamp(repeat['started_at']) <=
                    timestamp(repeat['execution']['completed_at']) <= timestamp(completion['observed_at']),
                    'Pilot terminal repeat is invalid or changed the default revision')
            if operation != 'status':
                require('no pull request needed' in repeat['stdout'], 'Pilot repeat created a pull request')
        equivalence = read(execution, prefix + '-final-installed-equivalence.json')
        require(equivalence['repository_id'] == repository_id and equivalence['repository'] == repo and
                equivalence['tested_revision_sha'] == installed['revision_sha'] and
                equivalence['final_default_revision_sha'] == merge['merge_sha'] and
                equivalence['actual_git_blob_sha'] == installed['blob_sha'] and
                equivalence['workflow_sha256'] == workflow_hash and equivalence['content_equal'] is True and
                equivalence['fixture_revisions_unmodified'] is True and
                timestamp(completion['completed_at']) <= timestamp(equivalence['actual_capture_started_at']) <=
                timestamp(equivalence['actual_capture_completed_at']) <= timestamp(equivalence['observed_at']),
                'Pilot final installed equivalence is not bound to its tested workflow')
        for kind, path in [('workflow', installed['path']), ('manifest', '.sfl/sfl.json')]:
            capture = read(execution, prefix + '-final-installed-' + kind + '-capture.json')
            require(capture['actor'] == 'HemSoft' and capture['exit_code'] == 0 and
                    capture['argv'] == ['gh', 'api', 'repos/' + repo + '/contents/' + path + '?ref=' + merge['merge_sha']] and
                    timestamp(completion['completed_at']) <= timestamp(capture['started_at']) <=
                    timestamp(capture['completed_at']) <= timestamp(equivalence['observed_at']) and
                    capture['data']['type'] == 'file' and capture['data']['encoding'] == 'base64' and
                    capture['data']['path'] == path,
                    'Pilot final installed capture is not bound to its immutable revision')
            captured_content = base64.b64decode(capture['data']['content'])
            if kind == 'workflow':
                require(captured_content == content and capture['data']['sha'] == blob and
                        capture['started_at'] == equivalence['actual_capture_started_at'] and
                        capture['completed_at'] == equivalence['actual_capture_completed_at'],
                        'Pilot final installed bytes differ from the tested workflow')
            else:
                final_manifest = json.loads(captured_content)
                manifest_blob = hashlib.sha1(b'blob ' + str(len(captured_content)).encode() +
                                             b'\0' + captured_content).hexdigest()
                require(capture['data']['sha'] == manifest_blob and
                        final_manifest == equivalence['manifest'] and final_manifest['sourceSha'] == source and
                        final_manifest['version'] == version and final_manifest['tier'] == 'reviewer' and
                        final_manifest['source'] == 'hemsoft-dev/set-it-free-loop' and
                        final_manifest['components'] == ['sfl-pr-review-auto'] and
                        final_manifest.get('addons', []) == [] and
                        final_manifest['enginePolicy'] == {'defaultProfile': 'codex-gpt-55-high', 'workflows': []},
                        'Pilot final installed manifest differs from its canonical pin')
        deletion = read(execution, prefix + '-post-live-gate-only-cleanup.json')
        require(deletion['actor'] == 'HemSoft' and deletion['exit_code'] == 0 and
                deletion['argv'] == ['gh', 'api', '--method', 'DELETE',
                    'repos/' + repo + '/rulesets/' + str(completion['owned_gate_id'])] and
                timestamp(completion['observed_at']) <= timestamp(deletion['started_at']) <=
                timestamp(deletion['completed_at']) <= timestamp(completion['completed_at']),
                'Pilot owned gate removal did not follow terminal repeats')
        for kind in ('effective', 'rulesets'):
            capture = read(execution, prefix + '-post-live-after-cleanup-' + kind + '.json')
            require(timestamp(deletion['completed_at']) <= timestamp(capture['started_at']) <=
                    timestamp(capture['completed_at']) <= timestamp(completion['completed_at']),
                    'Pilot post-cleanup policy capture predates gate removal')
        effective = api_receipt(execution, prefix + '-post-live-after-cleanup-effective.json',
                                'repos/' + repo + '/rules/branches/main')
        rulesets = api_receipt(execution, prefix + '-post-live-after-cleanup-rulesets.json',
                               'repos/' + repo + '/rulesets?includes_parents=true&per_page=100')
        require(effective == [r for r in merge['rules'] if r['ruleset_id'] != completion['owned_gate_id']] and
                rulesets == [r for r in merge['rulesets'] if r['id'] != completion['owned_gate_id']] and
                timestamp(completion['completed_at']) <= timestamp(qualification['qualified_at']),
                'Pilot cleanup did not preserve unrelated policy')


def validate_source_gate(execution):
    created = read(execution, 'rc21-source-only-org-review-gate-create.json')
    require(created['actor'] == 'HemSoft' and created['exit_code'] == 0 and
            created['argv'][:6] == ['gh', 'api', '--include', '--method', 'POST', 'orgs/hemsoft-dev/rulesets'] and
            created['stdout'].startswith('HTTP/2.0 201 Created\n'), 'Source gate creation needs its successful organization POST')
    gate = json.loads(created['stdout'].split('\n\n', 1)[1])
    require(gate == read(execution, 'rc21-source-only-org-review-gate-created-primary.json') and
            gate['id'] == 24716278 and gate['source_type'] == 'Organization' and gate['source'] == 'hemsoft-dev' and
            gate['target'] == 'branch' and gate['enforcement'] == 'active' and gate['bypass_actors'] == [] and
            gate['conditions'] == {'repository_id': {'repository_ids': [1169772257]},
                                  'ref_name': {'exclude': [], 'include': ['~DEFAULT_BRANCH']}} and
            gate['rules'] == [{'type': 'required_status_checks', 'parameters': {
                'strict_required_status_checks_policy': True, 'do_not_enforce_on_create': False,
                'required_status_checks': [{'context': 'SFL Reviewer Gate Runner', 'integration_id': 15368}]}}],
            'Source organization gate changed its scope, integration or bypass policy')
    before_name, after_name = 'rc21-before-source-governance-unrelated-detail.json', 'rc21-source-org-gate-after-unrelated.json'
    endpoint = 'orgs/hemsoft-dev/rulesets/24698223'
    before, after = api_receipt(execution, before_name, endpoint), api_receipt(execution, after_name, endpoint)
    require(before == after and before['id'] == 24698223 and
            timestamp(read(execution, before_name)['completed_at']) <= timestamp(created['started_at']) <=
            timestamp(created['completed_at']) <= timestamp(read(execution, after_name)['started_at']),
            'Source gate creation must preserve the unrelated organization rule')


def validate_release(execution):
    proof = read(execution, 'rc21-independent-download-proof.json')
    source, version, repo = '89425320ace3127a829d86e3b642fe2b31fd979e', '2.1.0-rc.21', 'hemsoft-dev/set-it-free-loop'
    tag = 'v' + version
    require(proof['repository'] == repo and proof['source_sha'] == source and proof['version'] == version and
            proof['release_id'] == 406645777 and proof['immutable'] is True and proof['signed_release_verified'] is True and
            proof['default_status_exit_code'] == 0 and proof['installer_default_source'] == repo,
            'Independent release summary has an invalid identity or result')
    release_capture = read(execution, 'pr156-rc21-release-metadata-primary.json')
    tag_capture = read(execution, 'pr156-rc21-release-tag-primary.json')
    for capture, endpoint in [(release_capture, 'repos/' + repo + '/releases/tags/' + tag),
                              (tag_capture, 'repos/' + repo + '/git/ref/tags/' + tag)]:
        require(capture['actor'] == 'HemSoft' and capture['exit_code'] == 0 and capture['argv'] == ['gh', 'api', endpoint],
                'Independent release needs its repository-bound API captures')
    release, ref = release_capture['data'], tag_capture['data']
    require(release['id'] == proof['release_id'] and release['tag_name'] == tag and
            release['immutable'] is True and release['prerelease'] is True and release['draft'] is False and
            ref['ref'] == 'refs/tags/' + tag and ref['object']['type'] == 'commit' and ref['object']['sha'] == source,
            'Immutable release API identity differs from its proof')
    signed = read(execution, 'rc21-signed-release-verification-primary.json')
    envelope = signed['attestation']['bundle']['dsseEnvelope']
    statement = signed['verificationResult']['statement']
    require(json.loads(base64.b64decode(envelope['payload'])) == statement and bool(envelope['signatures']) and
            signed['verificationResult']['signature']['certificate']['subjectAlternativeName'] == 'https://dotcom.releases.github.com',
            'Release signature verification primary does not bind its signed statement')
    verify_retained_release_signature(signed)
    predicate = statement['predicate']
    require(statement['_type'] == 'https://in-toto.io/Statement/v1' and
            statement['predicateType'] == 'https://in-toto.io/attestation/release/v0.2' and
            predicate['repository'] == repo and predicate['repositoryId'] == predicate['packageId'] == '1169772257' and
            predicate['ownerId'] == '338855369' and predicate['databaseId'] == str(proof['release_id']) and predicate['tag'] == tag,
            'Release signature statement belongs to another repository or release')
    subjects = statement['subject']
    require(subjects[0] == {'uri': 'pkg:github/' + repo + '@' + tag, 'digest': {'sha1': source}},
            'Release signature source SHA differs from its proof')
    sums_bytes = (execution / 'rc21-SHA256SUMS').read_bytes()
    sums = dict((line.split()[1], line.split()[0]) for line in sums_bytes.decode().splitlines())
    signed_assets = {item['name']: item['digest']['sha256'] for item in subjects[1:]}
    require(proof['digests'] == sums and signed_assets == {**sums, 'SHA256SUMS': hashlib.sha256(sums_bytes).hexdigest()} and
            {item['name']: item['digest'] for item in release['assets']} ==
            {name: 'sha256:' + digest for name, digest in signed_assets.items()},
            'Release asset digests do not match the signed checksum primary')
    invocation = read(execution, 'rc21-independent-default-download-verification.json')
    verifier_bytes = (execution / 'rc21-independent-download-verifier.py').read_bytes()
    verifier_capture = read(execution, 'pr156-rc21-executed-verifier-path-capture.json')
    require(verifier_capture['captured_path'] == invocation['argv'][1] and
            verifier_capture['committed_copy'] == 'rc21-independent-download-verifier.py' and
            verifier_capture['invocation_primary'] == 'rc21-independent-default-download-verification.json' and
            verifier_capture['captured_bytes'] == len(verifier_bytes) and
            verifier_capture['captured_sha256'] == hashlib.sha256(verifier_bytes).hexdigest() ==
            '003d0035ac48c149db6d71361b8292ed27291f50a6ce6654684e9db6dd21867f' and
            verifier_capture['capture_timing'] == 'retrospective_after_execution' and
            timestamp(invocation['completed_at']) <= timestamp(verifier_capture['observed_at']) and
            verifier_capture['limitation'].startswith('Original invocation did not record a script hash at execution time;'),
            'Retrospective verifier path capture does not bind actual argv, retained bytes or its timing limitation')
    require(invocation['actor'] == 'HemSoft' and invocation['exit_code'] == 0 and
            invocation['argv'] == ['python3', '/home/franz/github/hemsoft/set-it-free-loop/.git/merge-mission/verify-rc21-download.py'] and
            timestamp(release['published_at']) <= timestamp(invocation['started_at']) <= timestamp(proof['observed_at']) <=
            timestamp(invocation['completed_at']), 'Independent release verifier did not complete after publication')
    result = json.loads(invocation['stdout'])
    require(result['release_id'] == proof['release_id'] and result['checksum_verified'] is True and result['default_status'] == 'passed' and
            proof['version_output'] == (execution / 'rc21-version.log').read_text() and
            result['version'] == proof['version_output'].strip() and
            re.fullmatch(r'gh-sfl ' + re.escape(version) + r' \([0-9]{4}-[0-9]{2}-[0-9]{2}\) HemSoft',
                         proof['version_output'].splitlines()[0]) is not None and
            'Could not check for updates' not in proof['version_output'] and
            'Verified SHA-256 for gh-sfl_' + version + '_linux_amd64.' in (execution / 'rc21-installer.log').read_text() and
            'Could not resolve latest synchronized release' not in (execution / 'rc21-default-status.log').read_text(),
            'Independent installer or isolated CLI logs disagree with the successful verifier')


def validate_dashboard_repair(execution):
    repair = read(execution, 'dashboard-policy-repair.json')
    receipt = read(execution, 'repository-transfers/repository-transfer-1120402599.json')
    require(repair['repository_id'] == 1120402599 and
            repair['original_primary_capture'] == 'repository-transfers/repository-transfer-1120402599.json',
            'Dashboard repair must bind its original repository receipt')
    response = repair['response']
    require(response['method'] == 'PUT' and response['http_status'] == 200 and
            response['request_url'] == 'https://api.github.com/repos/hemsoft-dev/dashboard/rulesets/11400445' and
            timestamp(receipt['submitted_at']) <= timestamp(repair['observed_at']) <= timestamp(response['observed_at']) <=
            timestamp(repair['repaired_at']) <= timestamp(receipt['verified_at']),
            'Dashboard repair needs its post-transfer repository-bound PUT response')
    original = receipt['before_policy']['ruleset_details'][0]['data']
    dropped_capture = repair['before']['ruleset_details'][0]
    require(dropped_capture['method'] == 'GET' and dropped_capture['http_status'] == 200 and
            dropped_capture['request_url'] ==
            'https://api.github.com/repos/hemsoft-dev/dashboard/rulesets/11400445' and
            timestamp(receipt['transfer_response']['observed_at']) <=
            timestamp(dropped_capture['observed_at']) <= timestamp(repair['observed_at']),
            'Dropped dashboard policy needs its successful post-transfer ruleset GET')
    dropped = dropped_capture['data']
    final = receipt['after_policy']['ruleset_details'][0]['data']
    def policy(value):
        # GitHub adds this disabled schema field during transfer; it has no enforcement effect.
        if isinstance(value, list): return [policy(item) for item in value]
        if isinstance(value, dict): return {k: policy(v) for k, v in value.items()
            if not (k == 'dismissal_restriction' and v == {'enabled': False, 'allowed_actors': []})}
        return value
    fields = ('name', 'target', 'enforcement', 'conditions', 'rules', 'bypass_actors')
    expected = policy({key: original[key] for key in fields})
    require(original['id'] == dropped['id'] == final['id'] == response['data']['id'] == 11400445 and
            expected['bypass_actors'] == [{'actor_id': 5, 'actor_type': 'RepositoryRole', 'bypass_mode': 'always'}] and
            dropped['bypass_actors'] == [] and
            policy({key: dropped[key] for key in fields if key != 'bypass_actors'}) ==
            {key: expected[key] for key in fields if key != 'bypass_actors'} and
            policy(repair['payload']) == expected and
            policy({key: response['data'][key] for key in fields}) == expected and
            policy({key: final[key] for key in fields}) == expected,
            'Dashboard repair changed another rule or failed to restore its original administrator bypass')


def validate_app_phase(directory):
    execution = directory / 'execution'
    proof = read(execution, 'organization-app-coverage-verified.json')
    capture = read(execution, 'pr156-app-owned-metadata-current.json')
    app = capture['data']
    require(capture['actor'] == 'HemSoft' and capture['exit_code'] == 0 and
            capture['argv'] == ['gh', 'api', 'apps/sfl-app'] and app['id'] == 4448946 and app['slug'] == 'sfl-app' and
            app['client_id'] == 'Iv23liwvwJJUh2bUIKLW' and
            app['owner'] == {'id': 338855369, 'login': 'hemsoft-dev', 'type': 'Organization'} and
            app['permissions'] == {'actions': 'write', 'checks': 'write', 'contents': 'read',
                                   'issues': 'write', 'metadata': 'read', 'pull_requests': 'write'},
            'Executed App transfer needs its owned organization registration capture')
    run_capture = read(execution, 'organization-app-coverage-run-primary.json')
    run = run_capture['data']
    require(run_capture['actor'] == 'HemSoft' and run_capture['exit_code'] == 0 and
            run_capture['argv'] == ['gh', 'api', 'repos/hemsoft-dev/set-it-free-loop/actions/runs/37731379724'] and
            run['id'] == proof['run_id'] == 37731379724 and run['repository'] ==
            {'id': 1169772257, 'full_name': 'hemsoft-dev/set-it-free-loop'} and
            run['head_sha'] == proof['reviewed_sha'] == 'ee75f09992d7b54b5f9f89dc4ab59caa055e0f05' and
            run['head_branch'] == 'main' and run['event'] == 'workflow_dispatch' and
            run['path'] == '.github/workflows/verify-sfl-app-credential.yml' and
            run['status'] == 'completed' and run['conclusion'] == 'success',
            'App coverage requires its actual successful reviewed main workflow')
    artifact_capture = read(execution, 'organization-app-coverage-artifact-primary.json')
    require(artifact_capture['actor'] == 'HemSoft' and artifact_capture['exit_code'] == 0 and
            artifact_capture['argv'] == ['gh', 'api', 'repos/hemsoft-dev/set-it-free-loop/actions/runs/37731379724/artifacts?per_page=100'],
            'App coverage artifact list belongs to another workflow run')
    artifacts = [a for a in artifact_capture['data']['artifacts'] if a['name'] == 'sfl-app-repository-coverage']
    require(len(artifacts) == 1 and artifacts[0]['id'] == 11530141241 and
            artifacts[0]['workflow_run']['id'] == run['id'] and
            artifacts[0]['workflow_run']['repository_id'] == 1169772257 and
            artifacts[0]['workflow_run']['head_sha'] == run['head_sha'], 'App coverage artifact lacks its reviewed run binding')
    download = read(execution, 'pr156-app-coverage-independent-artifact-download.json')
    require(download['actor'] == 'HemSoft' and download['exit_code'] == 0 and
            download['argv'] == ['gh', 'run', 'download', '37731379724', '--repo', 'hemsoft-dev/set-it-free-loop',
                '--name', 'sfl-app-repository-coverage', '--dir',
                '/home/franz/github/hemsoft/set-it-free-loop/.git/merge-mission/pr156-app-coverage-primary'] and
            timestamp(run['updated_at']) <= timestamp(download['started_at']),
            'App coverage independent artifact download did not succeed')
    content = (execution / 'organization-app-repository-coverage-primary.json').read_bytes()
    require(hashlib.sha256(content).hexdigest() == proof['artifact_json_sha256'], 'App coverage artifact bytes differ from the original proof')
    archive = read(execution, 'pr156-app-coverage-archive-primary.json')
    archive_bytes = base64.b64decode(archive['archive_base64'], validate=True)
    require(archive['actor'] == 'HemSoft' and archive['exit_code'] == 0 and
            archive['argv'] == ['gh', 'api', '--allow-escape-sequences',
                'repos/hemsoft-dev/set-it-free-loop/actions/artifacts/11530141241/zip'] and
            archive['artifact_id'] == artifacts[0]['id'] and archive['run_id'] == run['id'] and
            archive['repository_id'] == 1169772257 and
            timestamp(run['updated_at']) <= timestamp(archive['started_at']) <= timestamp(archive['completed_at']) and
            artifacts[0]['digest'] == 'sha256:' + hashlib.sha256(archive_bytes).hexdigest() and
            archive['archive_sha256'] == hashlib.sha256(archive_bytes).hexdigest() and
            archive['json_sha256'] == hashlib.sha256(content).hexdigest(),
            'App coverage archive does not match its authenticated artifact digest')
    import io
    import zipfile
    with zipfile.ZipFile(io.BytesIO(archive_bytes)) as downloaded:
        require(downloaded.namelist() == [archive['member']] and
                downloaded.read(archive['member']) == content,
                'App coverage JSON differs from the digest-bound downloaded archive')
    coverage = json.loads(content)
    require(coverage['run_id'] == run['id'] and coverage['reviewed_sha'] == run['head_sha'] and
            coverage['app_id'] == proof['app_id'] == app['id'] and
            coverage['installation_id'] == proof['installation_id'] == 169090497 and
            coverage['target_count'] == proof['verified_target_count'] == len(coverage['repositories']) == 68 and
            proof['original_transfer_targets'] == 65 and proof['supplemental_validation_targets'] == 3 and
            proof['retained_personal_ids_excluded'] == [1162179521, 1169698740] and
            proof['repository_identity_binding_verified'] is True and proof['permission_ceiling_verified'] is True and
            proof['metadata_projection_verified'] is True and proof['credential_changes'] is False,
            'App coverage summary does not match its actual artifact')
    expected = {row['id']: row['destination'] for row in read(directory, 'inventory.json')['repositories']
                if row['id'] not in proof['retained_personal_ids_excluded']}
    expected.update({1408025382: 'hemsoft-dev/sfl-migration-pilot-private',
                     1408029795: 'hemsoft-dev/sfl-migration-pilot-public',
                     1409692659: 'hemsoft-dev/sfl-migration-onboarding-proof'})
    require({row['repository_id']: row['repository'] for row in coverage['repositories']} == expected,
            'App coverage must reconcile all transferred IDs and the three validation repositories')
    for row in coverage['repositories']:
        repo = row['repository']
        identity, installation = row['identity'], row['installation']
        require(row['method'] == identity['method'] == 'GET' and row['http_status'] == identity['http_status'] == 200 and
                row['request_url'] == 'https://api.github.com/repos/' + repo + '/installation' and
                identity['request_url'] == 'https://api.github.com/repos/' + repo and
                identity['repository']['id'] == row['repository_id'] and identity['repository']['full_name'] == repo and
                identity['repository']['owner'] == app['owner'] and
                installation['id'] == 169090497 and installation['app_id'] == app['id'] and
                installation['client_id'] == app['client_id'] and installation['repository_selection'] == 'all' and
                installation['account']['id'] == 338855369 and installation['account']['login'] == 'hemsoft-dev' and
                installation['target_type'] == 'Organization' and installation['suspended_at'] is None and
                installation['permissions'] == app['permissions'] and
                timestamp(run['created_at']) <= timestamp(row['observed_at']) <= timestamp(coverage['observed_at']) <=
                timestamp(run['updated_at']) <= timestamp(run_capture['started_at']),
                'App coverage repository identity, installation, permission or chronology is invalid')
    phase = read(execution, 'executed-phase-status.json')
    require(phase['owned_app_transfer'] == {'status': 'verified', 'app_id': 4448946, 'owner': 'hemsoft-dev',
            'installation_id': 169090497, 'coverage_targets': 68, 'evidence': 'organization-app-repository-coverage-primary.json'} and
            phase['transfer_identity_targets'] == 65 and phase['retained_sources'] == 2 and
            phase['migration_acceptance_complete'] is False and phase['sfl_rollout_complete'] is False and
            timestamp(capture['completed_at']) <= timestamp(phase['observed_at']),
            'Executed phase status must report completed App transfer separately from pending migration acceptance')


def validate_protection_limitation(execution):
    limitation = read(execution, 'fhemmer-protection-verification-limitation.json')
    receipt = read(execution, limitation['primary_transfer_capture'])
    browser = read(execution, limitation['older_owner_browser_capture'])
    audit = read(execution, limitation['retrospective_supporting_capture'])
    require(limitation['repository_id'] == receipt['repository_id'] == browser['repository_id'] == 1143951439 and
            limitation['source'] == receipt['source'] == browser['source'] == 'fhemmer/hs-cli-confluence-search' and
            limitation['destination'] == receipt['destination'] == 'hemsoft-dev/hs-cli-confluence-search-fhemmer' and
            limitation['actual_transfer_started_at'] == receipt['started_at'] and
            limitation['older_capture_observed_at'] == browser['observed_at'] and browser['owner_login'] == 'HemSoft' and
            browser['source_transferred'] is False and browser['classic']['state'] == browser['rulesets']['state'] == 'absent' and
            browser['classic']['url'] == 'https://github.com/fhemmer/hs-cli-confluence-search/settings/branches' and
            browser['rulesets']['url'] == 'https://github.com/fhemmer/hs-cli-confluence-search/settings/rules' and
            timestamp(browser['observed_at']) < timestamp(receipt['started_at']),
            'Historical protection limitation must derive from its actual owner browser and transfer captures')
    response = receipt['before_policy']['ruleset_pages'][0]
    require(response['method'] == 'GET' and response['http_status'] == 403 and response['request_url'] ==
            'https://api.github.com/repos/fhemmer/hs-cli-confluence-search/rulesets?includes_parents=true&per_page=100' and
            response['data']['message'] == 'Upgrade to GitHub Pro or make this repository public to enable this feature.' and
            response['data']['status'] == '403' and timestamp(browser['observed_at']) <= timestamp(response['observed_at']) <=
            timestamp(receipt['submitted_at']), 'Historical protection gap requires its actual cutover API refusal')
    events = re.findall(r'(?m)^\s*[^\r\n]+?\s+[–-]\s+([A-Za-z_]+\.[A-Za-z_]+)\s*$', audit['text'])
    require(audit['url'] == 'https://github.com/organizations/fhemmer/settings/audit-log?q=created%3A%3E%3D2026-10-07' and
            audit['capture_source'] == 'Shared owner browser visible fhemmer organization audit log' and
            set(events) == {'repo.transfer_outgoing', 'integration_installation.repositories_removed'} and
            audit['pagination'] == [] and
            timestamp(receipt['verified_at']) <= timestamp(audit['observed_at']),
            'Retrospective protection audit must retain its actual limited event capture')
    return limitation


def validate(directory):
    inventory = read(directory, 'inventory.json')
    execution = directory / 'execution'
    validate_source_gate(execution)
    validate_release(execution)
    validate_dashboard_repair(execution)
    validate_app_phase(directory)
    final = read(execution, 'repository-first-final-inventory.json')
    direction = read(execution, 'owner-repository-first-direction.json')
    require(direction['repository_transfer_first'] is True and
            direction['non_sfl_resource_preservation_still_required'] is True,
            'Executed phase must preserve the recorded owner scope')
    expected = {row['id']: row for row in inventory['repositories']}
    require(len(expected) == 67 and len(inventory['repositories']) == 67,
            'Original inventory must contain 67 unique identities')
    require(len(final['retained']) == 2 and len({row['id'] for row in final['retained']}) == 2,
            'Retained inventory must contain exactly two unique rows')
    retained = {row['id']: row['source'] for row in final['retained']}
    require(retained == {1162179521: 'HemSoft/now-leadership-group',
                         1169698740: 'HemSoft/set-it-free-loop-site'},
            'Retained repositories must match the owner decision')
    require(set(direction['retained_sources']) == set(retained.values()),
            'Retention differs from the recorded owner direction')
    for repository_id, name in retained.items():
        capture = read(execution, f'retained-repository-{repository_id}.json')
        actual = metadata(capture, repository_id, name)
        require(timestamp(capture['observed_at']) >= timestamp(final['observed_at']),
                'Retained repository capture must follow the transfer phase')
        for field in ('private', 'visibility', 'archived', 'default_branch'):
            require(actual[field] == expected[repository_id][field],
                    'Retained repository configuration differs from the inventory')
        primary_name = {1162179521: 'pr156-sixth-retained-nlg-current-primary.json',
                        1169698740: 'pr156-sixth-retained-sfl-site-current-primary.json'}[repository_id]
        require(capture['primary_receipt'] == primary_name,
                'Retained repository primary receipt reference differs from its capture')
        primary = read(execution, primary_name)
        require(primary['actor'] == capture['actor'] == 'HemSoft' and primary['exit_code'] == 0 and
                primary['argv'] == ['gh', 'api', 'repos/' + name, '--jq',
                                    '{id,full_name,private,visibility,archived,default_branch}'] and
                timestamp(final['observed_at']) <= timestamp(primary['started_at']) ==
                timestamp(capture['capture_started_at']) <= timestamp(primary['completed_at']) ==
                timestamp(capture['observed_at']) and json.loads(primary['stdout']) == actual,
                'Retained repository projection differs from its successful command receipt')
    receipts = final['receipts']
    require(len(receipts) == 65 and len({row['repository_id'] for row in receipts}) == 65 and
            {row['repository_id'] for row in receipts} == set(expected) - set(retained),
            'Transfer receipts must cover exactly the 65 destination identities')
    private_count = archive_count = 0
    for row in receipts:
        repo = expected[row['repository_id']]
        path = pathlib.PurePosixPath(row['receipt_file'])
        require(path.parts == ('repository-transfers', f'repository-transfer-{repo["id"]}.json'),
                'Receipt path must bind the expected immutable repository ID')
        receipt = read(execution, str(path))
        require(receipt['repository_id'] == repo['id'] and
                receipt['source'] == row['source'] == repo['full_name'] and
                receipt['destination'] == row['destination'] == repo['destination'],
                'Receipt mapping must match the original inventory')
        before = metadata(receipt['before'], repo['id'], repo['full_name'])
        after = metadata(receipt['after'], repo['id'], repo['destination'])
        for field in ('private', 'visibility', 'archived', 'default_branch'):
            require(before[field] == after[field] == repo[field],
                    f'Transfer changed {field} for repository {repo["id"]}')
        require(row['private'] == after['private'] and row['archived'] == after['archived'],
                'Final inventory disagrees with captured visibility or archive state')
        submitted = timestamp(receipt['submitted_at'])
        verified = timestamp(receipt['verified_at'])
        require(timestamp(direction['recorded_at']) <= submitted,
                'Owner direction must predate every transfer submission')
        accepted = receipt['transfer_response']
        require(accepted['method'] == 'POST' and accepted['http_status'] == 202 and
                accepted['request_url'] == 'https://api.github.com/repos/' + repo['full_name'] + '/transfer' and
                accepted['data']['id'] == repo['id'] and
                submitted <= timestamp(accepted['observed_at']) <= timestamp(receipt['after']['observed_at']) and
                receipt['transfer_payload']['new_owner'] == inventory['destination_login'] and
                receipt['transfer_payload'].get('new_name', repo['full_name'].split('/')[1]) ==
                repo['destination'].split('/')[1],
                'Transfer needs its accepted API response and destination organization')
        before_refs = refs(receipt['before_refs'], repo['full_name'], repo['default_branch'],
                           timestamp(inventory['captured_at']), submitted)
        after_refs = refs(receipt['after_refs'], repo['destination'], repo['default_branch'],
                          timestamp(accepted['observed_at']), verified)
        require(before_refs == after_refs,
                f'Transfer changed refs for repository {repo["id"]}')
        require(timestamp(inventory['captured_at']) <= timestamp(receipt['before']['observed_at']) <= timestamp(receipt['submitted_at']) <
                timestamp(receipt['after']['observed_at']) <= timestamp(receipt['verified_at']) <=
                timestamp(final['observed_at']), 'Transfer capture chronology is invalid')
        require(timestamp(row['verified_at']) == timestamp(receipt['verified_at']),
                'Final inventory must retain the actual receipt verification time')
        private_count += after['private']
        archive_count += after['archived']
    require((final['transferred'], final['private_transferred'], final['archived_transferred']) ==
            (65, private_count, archive_count) and archive_count == 13,
            'Final transfer counts do not derive from the actual receipts')
    limitation = validate_protection_limitation(execution)
    require(final['reviewer_rollout_complete'] is False,
            'This intermediate transfer report cannot claim completed SFL rollout')
    validate_pilots(directory)
    return {'owned_app_transfer': 'verified', 'app_coverage_targets': 68,
            'transfer_identity_and_refs': 'verified', 'transferred': 65,
            'retained': 2, 'archived_transferred': archive_count,
            'source_protection_evidence': 'incomplete',
            'source_protection_gap_repository_id': limitation['repository_id'],
            'sfl_rollout': 'pending', 'migration_acceptance_complete': False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('directory', type=pathlib.Path)
    args = parser.parse_args()
    try:
        print(json.dumps(validate(args.directory), sort_keys=True))
    except (ValueError, KeyError) as exc:
        parser.error(str(exc))


if __name__ == '__main__':
    main()
