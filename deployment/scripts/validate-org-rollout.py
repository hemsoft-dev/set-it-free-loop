#!/usr/bin/env python3
"""Validate offline migration work records without treating pending work as complete."""

import argparse
import csv
import datetime
import json
import pathlib
import re
import urllib.parse

TIERS = {'reviewer', 'minimal', 'standard', 'full'}
HEALTH = {'pending_transfer', 'pending_rollout', 'failed', 'verified', 'archived_verified', 'scope_exception'}
LEDGER_STATUS = {'owner_verification_pending', 'partial_provider_verified', 'verified'}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def text(value):
    return isinstance(value, str) and bool(value.strip())


def evidence(value, directory):
    require(text(value), 'Missing evidence reference')
    parsed = urllib.parse.urlsplit(value)
    if parsed.scheme:
        require(parsed.scheme == 'https' and bool(parsed.hostname) and not parsed.username
                and not parsed.password, 'Evidence must be an HTTPS reference without credentials')
    else:
        target = (directory / value).resolve()
        require(target.is_relative_to(directory.resolve()) and target.is_file(),
                'Local evidence must name a file inside the migration directory')


def validate(inventory, rows, matrix, directory):
    expected = {repo['id']: repo for repo in inventory['repositories']}
    require(len(expected) == len(inventory['repositories']), 'Duplicate baseline ID')
    require(matrix.get('schema_version') == 1, 'Unsupported matrix schema')
    require(matrix.get('organization') == inventory['destination_login'], 'Wrong matrix organization')
    require(matrix.get('based_on_inventory_capture') == inventory['captured_at'], 'Wrong baseline capture')
    seen_ledger = set()
    for row in rows:
        try:
            repo_id = int(row['repository_id'])
        except (ValueError, TypeError, KeyError) as exc:
            raise ValueError('Invalid ledger repository ID') from exc
        require(repo_id in expected, 'Unexpected ledger repository ID')
        repo = expected[repo_id]
        require(row.get('source') == repo['full_name'] and row.get('destination') == repo['destination'],
                'Ledger identity or collision mapping mismatch')
        require(row.get('status') in LEDGER_STATUS, 'Invalid ledger status')
        seen_ledger.add(repo_id)
        if row['status'] != 'verified':
            continue
        require(text(row.get('verified_by')), 'Verified ledger row needs owner identity')
        try:
            verified_at = datetime.datetime.fromisoformat(row.get('verified_at', '').replace('Z', '+00:00'))
        except ValueError as exc:
            raise ValueError('Verified ledger row needs an ISO timestamp') from exc
        require(verified_at.tzinfo is not None, 'Verification timestamp needs a timezone')
        evidence(row.get('evidence_url'), directory)
        require(text(row.get('provider')), 'Verified ledger row needs a provider or explicit none')
        if row['provider'] == 'none':
            require(text(row.get('absence_reason')), 'Verified absence needs an evidence-backed reason')
        else:
            for field in ('resource_owner', 'resource_url', 'billing_dependency', 'credential_source',
                          'affected_reference', 'transfer_action', 'smoke_test', 'recovery_action'):
                require(text(row.get(field)), f'Verified integration needs {field}')
            evidence(row['resource_url'], directory)
            require(row.get('credential_validity') in {'verified', 'not_required'},
                    'Credential presence alone does not establish validity')
    require(seen_ledger == set(expected), 'Ledger must cover every baseline ID')

    records = matrix.get('repositories')
    require(isinstance(records, list), 'Matrix needs repository records')
    seen_matrix = set()
    verified_rollouts = 0
    for row in records:
        repo_id = row.get('repository_id')
        require(type(repo_id) is int and repo_id in expected and repo_id not in seen_matrix,
                'Unexpected or duplicate matrix ID')
        seen_matrix.add(repo_id)
        repo = expected[repo_id]
        for field, baseline in (('source', 'full_name'), ('destination', 'destination'),
                                ('visibility', 'visibility'), ('archived', 'archived')):
            require(row.get(field) == repo[baseline], f'Matrix {field} differs from baseline')
        for field in ('installed_tier', 'selected_tier'):
            require(field in row and (row[field] is None or row[field] in TIERS), f'Invalid {field}')
        for field in ('installed_addons', 'selected_addons'):
            require(field in row, f'Matrix needs {field}')
            addons = row[field]
            require(addons is None or (isinstance(addons, list) and all(text(addon) for addon in addons)
                                      and len(set(addons)) == len(addons)), f'Invalid {field}')
        health = row.get('health')
        require(health in HEALTH, 'Invalid matrix health')
        if health == 'scope_exception':
            evidence(row.get('exception_evidence_url'), directory)
        elif health == 'archived_verified':
            require(row['archived'] is True, 'Active repository cannot use archived completion')
            evidence(row.get('transfer_evidence_url'), directory)
            evidence(row.get('status_evidence_url'), directory)
        elif health == 'verified':
            require(row['archived'] is False, 'Archived repository needs archive-preserving verification')
            for field in ('selected_tier', 'manifest_version', 'gate_policy', 'review_requester'):
                require(text(row.get(field)), f'Verified rollout needs {field}')
            require(row['installed_addons'] is not None and row['selected_addons'] is not None,
                    'Verified rollout needs observed and selected addon lists')
            require(row.get('deployment_source') == inventory['destination_login'] + '/set-it-free-loop',
                    'Verified rollout must use the transferred canonical source')
            for field in ('deployment_sha', 'review_head_sha', 'review_base_sha'):
                require(isinstance(row.get(field), str) and re.fullmatch(r'[0-9a-f]{40}', row[field]),
                        f'Verified rollout needs immutable {field}')
            require(row.get('destination_codex_access') == 'verified', 'Codex coverage must be verified')
            require(row.get('destination_sfl_app_access') == 'verified', 'SFL App coverage must be verified')
            for field in ('transfer_evidence_url', 'review_pr_url', 'gate_run_url', 'status_evidence_url'):
                evidence(row.get(field), directory)
            runs = row.get('wider_workflow_run_urls')
            require(isinstance(runs, list), 'Wider workflow evidence must be a list')
            require(row['selected_tier'] == 'reviewer' or bool(runs), 'Wider tier needs workflow evidence')
            for run in runs:
                evidence(run, directory)
            verified_rollouts += 1
    require(seen_matrix == set(expected), 'Matrix must cover every baseline ID exactly once')
    summary = {'repositories': len(expected), 'active': sum(not repo['archived'] for repo in expected.values()),
               'archived': sum(repo['archived'] for repo in expected.values()), 'verified_rollouts': verified_rollouts}
    require(matrix.get('summary') == summary, 'Matrix summary is stale')
    for extra in matrix.get('disposable_validation_repositories', []):
        require(extra.get('repository_id') not in expected and extra.get('source_inventory_member') is False,
                'Disposable validation repository cannot count as a source transfer')
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('directory', type=pathlib.Path)
    args = parser.parse_args()
    directory = args.directory
    inventory = json.loads((directory / 'inventory.json').read_text())
    matrix = json.loads((directory / 'rollout-matrix.json').read_text())
    with (directory / 'integration-ledger.csv').open(newline='') as stream:
        rows = list(csv.DictReader(stream))
    try:
        print(json.dumps(validate(inventory, rows, matrix, directory), sort_keys=True))
    except ValueError as exc:
        parser.error(str(exc))


if __name__ == '__main__':
    main()
