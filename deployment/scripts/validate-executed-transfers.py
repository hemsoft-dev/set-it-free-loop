#!/usr/bin/env python3
"""Validate transfer identity evidence without claiming pre-cutover or SFL completion."""
import argparse
import datetime
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
    retained = {row['id']: row['source'] for row in final['retained']}
    require(retained == {1162179521: 'HemSoft/now-leadership-group',
                         1169698740: 'HemSoft/set-it-free-loop-site'},
            'Retained repositories must match the owner decision')
    require(set(direction['retained_sources']) == set(retained.values()),
            'Retention differs from the recorded owner direction')
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
        require(receipt['transfer_response']['http_status'] == 202 and
                receipt['transfer_payload']['new_owner'] == inventory['destination_login'],
                'Transfer needs its accepted API response and destination organization')
        before_refs = refs(receipt['before_refs'], repo['full_name'],
                           timestamp(inventory['captured_at']), submitted)
        after_refs = refs(receipt['after_refs'], repo['destination'], submitted, verified)
        require(before_refs == after_refs,
                f'Transfer changed refs for repository {repo["id"]}')
        require(timestamp(receipt['before']['observed_at']) <= timestamp(receipt['submitted_at']) <
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
