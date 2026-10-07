#!/usr/bin/env python3
"""Validate offline migration work records without treating pending work as complete."""

import argparse
import csv
import datetime
import json
import pathlib
import re
import urllib.parse

TIERS = {'review', 'reviewer', 'minimal', 'standard', 'full', 'custom'}
HEALTH = {'pending_transfer', 'pending_rollout', 'failed', 'verified', 'archived_verified', 'scope_exception', 'retained_source'}
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


def workflow_catalog():
    root = pathlib.Path(__file__).resolve().parents[2]
    addons = (root / 'gh-sfl/addons.go').read_text()
    addon_map = addons.split('var addonWorkflows =', 1)[1].split('\n}', 1)[0]
    supported_addons = set(re.findall(r'^\s*"([^"\n]+)":\s*\{', addon_map, re.MULTILINE))
    init = (root / 'gh-sfl/init.go').read_text()
    workflow_map = init.split('var tierWorkflows =', 1)[1].split('\n}', 1)[0]
    full = re.search(r'"full":\s*\{([^}]+)\}', workflow_map)
    require(full is not None and supported_addons, 'Cannot establish authoritative workflow catalog')
    components = {name.removesuffix('.md').removesuffix('.yml').removesuffix('.lock')
                  for name in re.findall(r'"([^"\n]+)"', full.group(1))}
    return supported_addons, components


def string_list(value):
    return isinstance(value, list) and all(text(item) for item in value) and len(set(value)) == len(value)


def validate(inventory, rows, matrix, directory, scope_decisions=None):
    expected = {repo['id']: repo for repo in inventory['repositories']}
    require(len(expected) == len(inventory['repositories']), 'Duplicate baseline ID')
    require(matrix.get('schema_version') == 1, 'Unsupported matrix schema')
    require(matrix.get('organization') == inventory['destination_login'], 'Wrong matrix organization')
    require(matrix.get('based_on_inventory_capture') == inventory['captured_at'], 'Wrong baseline capture')
    owner = json.loads((directory / 'owner-verification.json').read_text())
    apps = owner['source_personal_apps']['installations']
    source_apps = [app for app in apps if app['name'] == 'SFL App']
    require(len(source_apps) == 1, 'Cannot establish baseline SFL App coverage')
    source_app_repos = set(source_apps[0]['repositories'])
    supported_addons, supported_components = workflow_catalog()
    owned_app_id = inventory['known_owned_app']['data']['id']
    if scope_decisions is None:
        scope_decisions = json.loads((directory / 'scope-decisions.json').read_text())
    require(scope_decisions.get('schema_version') == 1, 'Unsupported scope decision schema')
    retained = {}
    for decision in scope_decisions.get('retained_repositories', []):
        repo_id = decision.get('repository_id')
        require(type(repo_id) is int and repo_id in expected and repo_id not in retained,
                'Invalid or duplicate retained source ID')
        repo = expected[repo_id]
        require(decision.get('source') == repo['full_name'] and
                decision.get('retained_repository') == repo['full_name'] and
                decision.get('disposition') == 'retain_source' and decision.get('decision_owner') == 'HemSoft',
                'Retained source needs explicit owner scope decision')
        evidence(decision.get('decision_evidence_url'), directory)
        evidence(decision.get('status_evidence_url'), directory)
        metadata = decision.get('observed_metadata', {})
        require(metadata.get('id') == repo_id and metadata.get('full_name') == repo['full_name'] and
                metadata.get('private') == (repo['visibility'] == 'private') and
                metadata.get('archived') == repo['archived'], 'Retained source metadata differs from baseline')
        retained[repo_id] = decision
    seen_ledger = set()
    ledger_statuses = {}
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
        ledger_statuses.setdefault(repo_id, []).append(row['status'])
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
        require(type(row.get('source_app_access_in_baseline')) is bool and
                row['source_app_access_in_baseline'] == (repo['full_name'] in source_app_repos),
                'Matrix source App access differs from owner-verified baseline')
        for field in ('installed_tier', 'selected_tier'):
            require(field in row and (row[field] is None or row[field] in TIERS), f'Invalid {field}')
        for field in ('installed_addons', 'selected_addons'):
            require(field in row, f'Matrix needs {field}')
            addons = row[field]
            require(addons is None or string_list(addons), f'Invalid {field}')
            if field == 'selected_addons' and addons is not None:
                require(set(addons) <= supported_addons, 'Unsupported selected addon')
        for field in ('installed_components', 'selected_components'):
            require(field in row and (row[field] is None or string_list(row[field])), f'Invalid {field}')
        health = row.get('health')
        require(health in HEALTH, 'Invalid matrix health')
        require((health == 'retained_source') == (repo_id in retained), 'Matrix must honor retained source decisions')
        if health in {'verified', 'archived_verified', 'scope_exception'}:
            require(all(status == 'verified' for status in ledger_statuses[repo_id]),
                    'Completed rollout requires every integration row verified')
        if health == 'retained_source':
            decision = retained[repo_id]
            require(row.get('actual_repository') == decision['retained_repository'] and
                    row.get('retention_evidence_url') == decision['decision_evidence_url'] and
                    row.get('status_evidence_url') == decision['status_evidence_url'],
                    'Retained source needs matching owner and current source receipts')
        elif health == 'scope_exception':
            evidence(row.get('exception_evidence_url'), directory)
            evidence(row.get('transfer_evidence_url'), directory)
            evidence(row.get('status_evidence_url'), directory)
        elif health == 'archived_verified':
            require(row['archived'] is True, 'Active repository cannot use archived completion')
            evidence(row.get('transfer_evidence_url'), directory)
            evidence(row.get('status_evidence_url'), directory)
        elif health == 'verified':
            require(row['archived'] is False, 'Archived repository needs archive-preserving verification')
            for field in ('selected_tier', 'manifest_version', 'review_requester'):
                require(text(row.get(field)), f'Verified rollout needs {field}')
            require(row['installed_addons'] is not None and row['selected_addons'] is not None,
                    'Verified rollout needs observed and selected addon lists')
            require(row['selected_tier'] != 'review', 'Verified selected tier must use canonical reviewer spelling')
            if row['selected_tier'] == 'custom':
                components = row['selected_components']
                require(components is not None and bool(set(components) & supported_components) and
                        set(components) <= supported_components | {'labels', 'governance'},
                        'Custom tier needs recognized selected workflow components')
            policy = row.get('gate_policy')
            require(isinstance(policy, dict) and policy.get('state') == 'required' and
                    policy.get('context') == 'SFL Reviewer Gate Runner' and
                    policy.get('app_id') == 15368 and policy.get('strict') is True,
                    'Verified rollout needs a required strict SFL gate bound to GitHub Actions')
            evidence(policy.get('evidence_url'), directory)
            require(row.get('deployment_source') == inventory['destination_login'] + '/set-it-free-loop',
                    'Verified rollout must use the transferred canonical source')
            for field in ('deployment_sha', 'review_head_sha', 'review_base_sha'):
                require(isinstance(row.get(field), str) and re.fullmatch(r'[0-9a-f]{40}', row[field]),
                        f'Verified rollout needs immutable {field}')
            require(row.get('destination_codex_access') == 'verified', 'Codex coverage must be verified')
            require(row.get('destination_sfl_app_access') == 'verified', 'SFL App coverage must be verified')
            for field in ('transfer_evidence_url', 'review_pr_url', 'gate_run_url', 'status_evidence_url'):
                evidence(row.get(field), directory)
            for field in ('review_registration_url', 'review_registry_status_url', 'review_artifact_url'):
                evidence(row.get(field), directory)
            identity = row.get('review_artifact_identity')
            require(isinstance(identity, dict) and identity.get('runtime') == 'sfl_owned' and
                    identity.get('app_id') == owned_app_id and
                    identity.get('reviewed_head_sha') == row['review_head_sha'] and
                    identity.get('reviewed_base_sha') == row['review_base_sha'],
                    'Verified rollout needs an SFL-owned immutable review artifact identity')
            runs = row.get('wider_workflow_run_urls')
            require(isinstance(runs, list), 'Wider workflow evidence must be a list')
            wider = row['selected_tier'] not in {'reviewer', 'custom'} or (
                row['selected_tier'] == 'custom' and
                bool(set(row['selected_components']) & (supported_components - {'sfl-pr-review-auto'})))
            require(not wider or bool(runs), 'Wider tier needs workflow evidence')
            for run in runs:
                evidence(run, directory)
            verified_rollouts += 1
    require(seen_matrix == set(expected), 'Matrix must cover every baseline ID exactly once')
    summary = {'repositories': len(expected), 'active': sum(not repo['archived'] for repo in expected.values()),
               'archived': sum(repo['archived'] for repo in expected.values()), 'verified_rollouts': verified_rollouts,
               'planned_transfers': len(expected) - len(retained), 'retained_sources': len(retained)}
    require(matrix.get('summary') == summary, 'Matrix summary is stale')
    extras = matrix.get('disposable_validation_repositories')
    require(isinstance(extras, list) and bool(extras), 'Disposable validation inventory must remain present')
    extra_ids, extra_names, verified_pilot_visibilities = set(), set(), set()
    for extra in extras:
        extra_id, name = extra.get('repository_id'), extra.get('repository')
        require(type(extra_id) is int and extra_id > 0 and extra_id not in expected and
                extra.get('source_inventory_member') is False,
                'Disposable validation repository cannot count as a source transfer')
        require(isinstance(name, str) and re.fullmatch(re.escape(inventory['destination_login']) +
                r'/[A-Za-z0-9_.-]+', name) and name.split('/')[1] not in {'.', '..'},
                'Invalid disposable repository identity')
        require(extra_id not in extra_ids and name.casefold() not in extra_names,
                'Duplicate disposable repository identity')
        require(extra.get('visibility') in {'public', 'private'}, 'Invalid disposable repository visibility')
        require(extra.get('validation_status') in {'pending', 'failed', 'verified'},
                'Disposable pilot needs an explicit validation status')
        if extra['validation_status'] == 'verified':
            receipts = extra.get('validation_evidence')
            require(isinstance(receipts, dict), 'Verified pilot needs onboarding and SFL receipts')
            for field in ('init_pr_url', 'sync_pr_url', 'repeat_sync_evidence_url',
                          'repeat_onboarding_evidence_url', 'review_registration_url',
                          'review_registry_status_url', 'review_artifact_url', 'gate_run_url',
                          'status_evidence_url', 'gate_uninstall_evidence_url'):
                evidence(receipts.get(field), directory)
            identity = receipts.get('review_artifact_identity')
            require(isinstance(identity, dict) and identity.get('runtime') == 'sfl_owned' and
                    identity.get('app_id') == owned_app_id and
                    all(isinstance(identity.get(field), str) and
                        re.fullmatch(r'[0-9a-f]{40}', identity[field])
                        for field in ('reviewed_head_sha', 'reviewed_base_sha')),
                    'Verified pilot needs immutable SFL-owned review identity')
            verified_pilot_visibilities.add(extra['visibility'])
        extra_ids.add(extra_id)
        extra_names.add(name.casefold())
    source_complete = all(row['health'] in {'verified', 'archived_verified', 'scope_exception', 'retained_source'} for row in records)
    if verified_rollouts or source_complete:
        require(verified_pilot_visibilities == {'public', 'private'},
                'Active rollout requires verified public and private onboarding pilots first')
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
