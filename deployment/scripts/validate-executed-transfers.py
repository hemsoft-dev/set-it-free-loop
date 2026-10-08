#!/usr/bin/env python3
"""Validate transfer identity evidence without claiming pre-cutover or SFL completion."""
import argparse
import base64
import datetime
import hashlib
import json
import pathlib
import re


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


def refs(capture, repository, earliest, latest):
    responses = capture['responses']
    require(len(responses) == 1, 'Expected the complete matching-refs GET response')
    response = responses[0]
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
        require(summary['execution_binding_receipt'] == prefix + '-live-execution-binding.json' and
                summary['installed_terminal_receipt'] == prefix + '-live-runtime-run.json',
                'Pilot qualification references the wrong live evidence')
        binding = read(execution, summary['execution_binding_receipt'])
        merge = read(execution, prefix + '-live-guarded-merge.json')
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
        require(binding['pending_failure_expected'] is True and log['exit_code'] == 0 and
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
                    timestamp(merge['merged_at']) <= timestamp(repeat['started_at']) <=
                    timestamp(repeat['execution']['completed_at']) <= timestamp(completion['observed_at']),
                    'Pilot terminal repeat is invalid or changed the default revision')
            if operation != 'status':
                require('no pull request needed' in repeat['stdout'], 'Pilot repeat created a pull request')
        effective = api_receipt(execution, prefix + '-post-live-after-cleanup-effective.json',
                                'repos/' + repo + '/rules/branches/main')
        rulesets = api_receipt(execution, prefix + '-post-live-after-cleanup-rulesets.json',
                               'repos/' + repo + '/rulesets?includes_parents=true&per_page=100')
        require(effective == [r for r in merge['rules'] if r['ruleset_id'] != completion['owned_gate_id']] and
                rulesets == [r for r in merge['rulesets'] if r['id'] != completion['owned_gate_id']] and
                timestamp(completion['completed_at']) <= timestamp(qualification['qualified_at']),
                'Pilot cleanup did not preserve unrelated policy')


def validate(directory):
    inventory = read(directory, 'inventory.json')
    execution = directory / 'execution'
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
        before_refs = refs(receipt['before_refs'], repo['full_name'],
                           timestamp(inventory['captured_at']), submitted)
        after_refs = refs(receipt['after_refs'], repo['destination'], submitted, verified)
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
