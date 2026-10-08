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
                require(final_manifest == equivalence['manifest'] and final_manifest['sourceSha'] == source and
                        final_manifest['version'] == version and final_manifest['tier'] == 'reviewer',
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
    require(hashlib.sha256((execution / 'rc21-independent-download-verifier.py').read_bytes()).hexdigest() ==
            '003d0035ac48c149db6d71361b8292ed27291f50a6ce6654684e9db6dd21867f',
            'Independent verifier implementation differs from the executed checksum, signature and installer contract')
    require(invocation['actor'] == 'HemSoft' and invocation['exit_code'] == 0 and
            invocation['argv'] == ['python3', '/home/franz/github/hemsoft/set-it-free-loop/.git/merge-mission/verify-rc21-download.py'] and
            timestamp(release['published_at']) <= timestamp(invocation['started_at']) <= timestamp(proof['observed_at']) <=
            timestamp(invocation['completed_at']), 'Independent release verifier did not complete after publication')
    result = json.loads(invocation['stdout'])
    require(result['release_id'] == proof['release_id'] and result['checksum_verified'] is True and result['default_status'] == 'passed' and
            proof['version_output'] == (execution / 'rc21-version.log').read_text() and
            result['version'] == proof['version_output'].strip() and 'Could not check for updates' not in proof['version_output'] and
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
    dropped = repair['before']['ruleset_details'][0]['data']
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


def validate(directory):
    inventory = read(directory, 'inventory.json')
    execution = directory / 'execution'
    validate_source_gate(execution)
    validate_release(execution)
    validate_dashboard_repair(execution)
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
        for field in ('private', 'archived', 'default_branch'):
            require(actual[field] == expected[repository_id][field],
                    'Retained repository configuration differs from the inventory')
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
        for field in ('private', 'archived', 'default_branch'):
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
        after_refs = refs(receipt['after_refs'], repo['destination'], repo['default_branch'], submitted, verified)
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
    limitation = read(execution, 'fhemmer-protection-verification-limitation.json')
    require(limitation['repository_id'] == 1143951439 and
            timestamp(limitation['older_capture_observed_at']) < timestamp(limitation['actual_transfer_started_at']),
            'Historical protection gap must retain its actual chronology')
    require(final['reviewer_rollout_complete'] is False,
            'This intermediate transfer report cannot claim completed SFL rollout')
    validate_pilots(directory)
    return {'transfer_identity_and_refs': 'verified', 'transferred': 65,
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
