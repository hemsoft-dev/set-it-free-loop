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
# Franz's recorded October 6 decision. Expanding this set requires a new owner decision.
APPROVED_RETAINED_IDS = {1162179521, 1169698740}
RETENTION_RECEIPT = 'https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6028207635'
HEALTH = {'pending_transfer', 'pending_rollout', 'failed', 'verified', 'archived_verified', 'scope_exception', 'retained_source', 'source_verified'}
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


def semantic_version(value):
    pattern = r'(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?'
    return isinstance(value, str) and re.fullmatch(pattern, value) is not None


def immutable_sha(value):
    return isinstance(value, str) and re.fullmatch(r'[0-9a-f]{40}', value) is not None


def validate_registered_review(row, directory, repository):
    policy = row.get('gate_policy')
    require(isinstance(policy, dict) and policy.get('state') == 'required' and
            policy.get('context') == 'SFL Reviewer Gate Runner' and policy.get('app_id') == 15368 and
            policy.get('strict') is True, 'Completed review needs a required strict SFL gate bound to Actions')
    evidence(policy.get('evidence_url'), directory)
    require(text(row.get('review_requester')) and immutable_sha(row.get('review_head_sha')) and
            immutable_sha(row.get('review_base_sha')), 'Completed review needs immutable registered review context')
    require(row.get('requester_permission') in {'write','maintain','admin'} and
            re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9-]{0,38}', row['review_requester']) is not None,
            'Completed review needs a write-or-higher human requester')
    evidence(row.get('requester_permission_evidence_url'), directory)
    require(re.fullmatch('https://github.com/' + re.escape(repository) + r'/pull/[1-9][0-9]*',
                        row.get('review_pr_url','')) is not None,
            'Completed review needs a same-destination-repository PR')
    for field in ('review_pr_url', 'gate_run_url', 'review_registration_url',
                  'review_registry_status_url', 'review_artifact_url'):
        evidence(row.get(field), directory)
    identity = row.get('review_artifact_identity')
    require(isinstance(identity, dict) and identity.get('runtime') == 'sfl_registered_codex' and
            identity.get('app_id') == 1144995 and identity.get('bot_user_id') == 199175422 and identity.get('reviewed_head_sha') == row['review_head_sha'] and
            identity.get('reviewed_base_sha') == row['review_base_sha'] and
            identity.get('review_pr_url') == row['review_pr_url'] and
            identity.get('requester') == row['review_requester'],
            'SFL registered Codex immutable artifact must bind its requester, PR, head and base')


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
    canonical_source = inventory['destination_login'] + '/set-it-free-loop'
    codex = matrix.get('destination_codex_installation')
    captured_codex = json.loads((directory / 'codex-organization-installation-evidence.json').read_text())
    installation = captured_codex['installation']
    smoke = captured_codex['smoke_review']
    require(isinstance(codex, dict) and codex.get('id') == installation['id'] and
            codex.get('app_id') == installation['app_id'] == 1144995 and
            codex.get('slug') == installation['app_slug'] == 'chatgpt-codex-connector' and
            codex.get('repository_selection') == installation['repository_selection'] == 'all' and
            captured_codex['account']['login'] == inventory['destination_login'] and
            captured_codex['account']['type'] == 'Organization',
            'Destination Codex installation must match its independent captured identity and coverage')
    require(codex.get('connection_status') == 'verified_by_real_private_organization_review' and
            codex.get('smoke_review_url') == smoke['url'] and
            codex.get('smoke_review_head') == smoke['head_sha'] and immutable_sha(smoke['head_sha']) and
            smoke.get('bot_user_id') == 199175422 and smoke.get('app_id') == 1144995 and
            smoke['url'].startswith('https://github.com/' + inventory['destination_login'] + '/') and
            smoke.get('result','').startswith("Codex Review: Didn't find any major issues."),
            'Destination Codex connection needs its immutable authenticated organization smoke review')
    evidence(codex.get('smoke_review_url'), directory)

    if scope_decisions is None:
        scope_decisions = json.loads((directory / 'scope-decisions.json').read_text())
    require(scope_decisions.get('schema_version') == 1, 'Unsupported scope decision schema')
    decisions = scope_decisions.get('retained_repositories')
    require(isinstance(decisions, list) and len(decisions) == len(APPROVED_RETAINED_IDS),
            'Retention must match the fixed owner-approved two-repository set')
    retained = {}
    for decision in decisions:
        repo_id = decision.get('repository_id')
        require(type(repo_id) is int and repo_id in expected and repo_id in APPROVED_RETAINED_IDS and repo_id not in retained,
                'Invalid or duplicate retained source ID')
        repo = expected[repo_id]
        require(decision.get('source') == repo['full_name'] and
                decision.get('retained_repository') == repo['full_name'] and
                decision.get('disposition') == 'retain_source' and decision.get('decision_owner') == 'HemSoft',
                'Retained source needs explicit owner scope decision')
        require(decision.get('decision_evidence_url') == RETENTION_RECEIPT,
                'Retention must reference the recorded owner decision')
        evidence(decision.get('decision_evidence_url'), directory)
        reference = decision.get('status_evidence_url')
        evidence(reference, directory)
        require(not urllib.parse.urlsplit(reference).scheme and reference != 'scope-decisions.json',
                'Retention needs a separate captured API status artifact')
        status_capture = json.loads((directory / reference).read_text())
        require(status_capture.get('request_url') == 'https://api.github.com/repos/' + repo['full_name'],
                'Retained status artifact targets another repository')
        try:
            observed_at = datetime.datetime.fromisoformat(status_capture.get('observed_at', '').replace('Z', '+00:00'))
        except ValueError as exc:
            raise ValueError('Retained status artifact needs a capture timestamp') from exc
        require(observed_at.tzinfo is not None, 'Retained status capture needs a timezone')
        require(status_capture.get('metadata') == decision.get('observed_metadata'),
                'Retained status metadata differs from independent API capture')
        metadata = decision.get('observed_metadata', {})
        require(metadata.get('id') == repo_id and metadata.get('full_name') == repo['full_name'] and
                metadata.get('private') == (repo['visibility'] == 'private') and
                metadata.get('archived') == repo['archived'], 'Retained source metadata differs from baseline')
        retained[repo_id] = decision
    require(set(retained) == APPROVED_RETAINED_IDS, 'Retention differs from fixed owner-approved set')
    provider_apps = {'Azure Pipelines', 'Railway App', 'Fly.io', 'Vercel'}
    candidates = {repo_id: set() for repo_id in expected}
    for repo_id, repo in expected.items():
        if repo['full_name'].startswith('HemSoft/'):
            for app in apps:
                if app['name'] in provider_apps and (app['selection'] == 'all' or
                        repo['full_name'] in app['repositories']):
                    candidates[repo_id].add(app['name'])
        else:
            org_apps = inventory['source_organization_apps']
            require(org_apps.get('state') == 'observed', 'Organization App candidates are unverified')
            for app in org_apps['data']:
                if app['app_slug'] == 'blacksmith-sh':
                    require(app['repository_selection'] == 'all',
                            'Cannot establish selected organization provider coverage')
                    candidates[repo_id].add('Blacksmith')
    # Reconcile every observed repository runner, including the independent refresh.
    runner_resources = set()
    runtime = json.loads((directory / 'runtime-metadata.json').read_text())
    refresh = json.loads((directory / 'runtime-refresh-evidence.json').read_text())
    by_source = {repo['full_name']: repo_id for repo_id, repo in expected.items()}
    for capture in runtime['repositories']:
        for runner in capture['repository_runners'].get('data', []) or []:
            runner_resources.add((by_source[capture['source']], str(runner['id'])))
    for capture in refresh['records']:
        if capture['kind'] == 'runners' and capture.get('state') == 'observed':
            for runner in capture.get('data', {}).get('runners', []):
                runner_resources.add((by_source[capture['repository']], str(runner['id'])))
    seen_runner_resources = set()
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
        if row.get('resource_kind') == 'repository_runner':
            resource = (repo_id, row.get('resource_id'))
            require(resource in runner_resources and resource not in seen_runner_resources,
                    'Unexpected or duplicate repository runner resource')
            require(row.get('provider') == 'GitHub Actions', 'Known runner cannot be recorded as provider absence')
            seen_runner_resources.add(resource)
        captured = row.get('provider_candidates_from_app_access', '')
        require(isinstance(captured, str), 'Invalid provider candidate list')
        listed = [name.strip() for name in captured.split(';') if name.strip()]
        require(len(listed) == len(set(listed)) and set(listed) == candidates[repo_id],
                'Provider candidates differ from captured App selections')
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
    require(seen_runner_resources == runner_resources, 'Ledger must preserve every observed repository runner resource')
    all_transfer_gates_verified = all(all(status == 'verified' for status in ledger_statuses[repo_id])
                                      for repo_id in expected if repo_id not in retained)

    app_transfer = matrix.get('owned_app_transfer')
    require(isinstance(app_transfer, dict) and app_transfer.get('app_id') == owned_app_id and
            app_transfer.get('status') in {'pending', 'verified'}, 'Owned App needs an explicit transfer state')
    if app_transfer['status'] == 'verified':
        require(all_transfer_gates_verified, 'All transfer-target ledger gates must be verified before App transfer')
        require(app_transfer.get('owner') == inventory['destination_login'], 'Transferred App needs its canonical organization owner')
        evidence(app_transfer.get('evidence_url'), directory)

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
            require(field in row and (row[field] is None or row[field] in (TIERS | {'not_installed'} if field == 'installed_tier' else TIERS)), f'Invalid {field}')
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
        protected_source = repo['full_name'] == 'HemSoft/set-it-free-loop'
        require((row.get('rollout_action') == 'protected_source_verify_workflows_in_place') == protected_source,
                'Protected source completion mode must match the distribution repository')
        if repo_id not in retained and (health != 'pending_transfer' or text(row.get('transfer_evidence_url'))):
            require(all_transfer_gates_verified,
                    'All transfer-target ledger gates must be verified before any transfer advances')
        if row.get('destination_sfl_app_access') == 'verified':
            require(app_transfer['status'] == 'verified', 'Destination private SFL App access requires verified App transfer')
        if health in {'verified', 'archived_verified', 'scope_exception', 'source_verified'}:
            require(all(status == 'verified' for status in ledger_statuses[repo_id]),
                    'Completed rollout requires every integration row verified')
        if health == 'retained_source':
            decision = retained[repo_id]
            require(row.get('actual_repository') == decision['retained_repository'] and
                    row.get('retention_evidence_url') == decision['decision_evidence_url'] and
                    row.get('status_evidence_url') == decision['status_evidence_url'],
                    'Retained source needs matching owner and current source receipts')
            dependency = row.get('retained_app_dependency')
            require(isinstance(dependency, dict) and dependency.get('status') in {'pending', 'verified'},
                    'Retained source needs an explicit App dependency status')
            if dependency['status'] == 'verified':
                require(text(dependency.get('verified_by')) and text(dependency.get('disposition')),
                        'Verified retained App dependency needs owner and disposition')
                evidence(dependency.get('evidence_url'), directory)
                try:
                    verified_at = datetime.datetime.fromisoformat(dependency.get('verified_at', '').replace('Z', '+00:00'))
                except ValueError as exc:
                    raise ValueError('Retained App dependency needs a verification timestamp') from exc
                require(verified_at.tzinfo is not None, 'Retained App dependency needs a timezone')
        elif health == 'source_verified':
            require(protected_source and not row['archived'], 'Only the protected distribution source can verify in place')
            proof = row.get('in_place_evidence')
            require(isinstance(proof, dict), 'Protected source needs in-place workflow, release and governance evidence')
            require(semantic_version(proof.get('release_version')) and immutable_sha(proof.get('source_sha')),
                    'Protected source needs an immutable semantic release identity')
            require(proof.get('release_url') == 'https://github.com/' + canonical_source + '/releases/tag/v' + proof['release_version'],
                    'Protected source release must belong to its canonical destination')
            for field in ('release_url', 'release_verification_url', 'governance_evidence_url'):
                evidence(proof.get(field), directory)
            runs = proof.get('workflow_run_urls')
            require(isinstance(runs, list) and bool(runs), 'Protected source needs in-place workflow runs')
            for run in runs:
                evidence(run, directory)
            for field in ('transfer_evidence_url', 'status_evidence_url'):
                evidence(row.get(field), directory)
            require(row.get('destination_codex_access') == 'verified' and row.get('destination_sfl_app_access') == 'verified',
                    'Protected source needs verified App coverage')
            # The source owns its workflows; it must not acquire a consumer manifest through init/sync.
            require(row['installed_tier'] is None and row['selected_tier'] is None,
                    'Protected source must not be recorded as a deployed consumer')
            validate_registered_review(row, directory, row["destination"])
            verified_rollouts += 1
        elif health == 'scope_exception':
            require(not protected_source, 'Protected source cannot omit in-place verification through an exception')
            evidence(row.get('exception_evidence_url'), directory)
            evidence(row.get('transfer_evidence_url'), directory)
            evidence(row.get('status_evidence_url'), directory)
        elif health == 'archived_verified':
            require(row['archived'] is True, 'Active repository cannot use archived completion')
            evidence(row.get('transfer_evidence_url'), directory)
            evidence(row.get('status_evidence_url'), directory)
        elif health == 'verified':
            require(not protected_source, 'Protected source must use in-place source verification')
            require(row['archived'] is False, 'Archived repository needs archive-preserving verification')
            for field in ('selected_tier', 'manifest_version', 'review_requester'):
                require(text(row.get(field)), f'Verified rollout needs {field}')
            require(semantic_version(row.get('manifest_version')), 'Verified rollout needs a semantic manifest release version')
            require(text(row.get('installed_tier')), 'Verified rollout needs the observed pre-sync installed tier')
            if row['installed_tier'] == 'custom':
                require(string_list(row['installed_components']) and bool(row['installed_components']),
                        'Installed custom tier needs observed components')
            require(row['installed_addons'] is not None and row['selected_addons'] is not None,
                    'Verified rollout needs observed and selected addon lists')
            require(row['selected_tier'] != 'review', 'Verified selected tier must use canonical reviewer spelling')
            if row['selected_tier'] == 'custom':
                require(row['installed_tier'] == 'custom', 'Selected custom tier requires an existing custom installation')
                components = row['selected_components']
                require(components is not None and bool(set(components) & supported_components) and
                        set(components) <= supported_components | {'labels', 'governance'},
                        'Custom tier needs recognized selected workflow components')
            if row['installed_tier'] != 'not_installed':
                require(row['selected_tier'] == ('reviewer' if row['installed_tier'] == 'review' else row['installed_tier']) and
                        set(row['selected_addons']) == set(row['installed_addons']),
                        'Existing deployment must preserve its installed tier and addons')
                if row['installed_tier'] == 'custom':
                    require(set(row['selected_components']) == set(row['installed_components']),
                            'Custom deployment must preserve its observed components')
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
            manifest = row.get('manifest_identity')
            require(isinstance(manifest, dict) and manifest.get('source') == canonical_source and
                    manifest.get('sourceSha') == row['deployment_sha'] and
                    manifest.get('version') == row['manifest_version'] and
                    manifest.get('tier') == row['selected_tier'],
                    'Verified rollout manifest must bind source, release version, SHA and selected tier')
            require(row.get('release_url') == 'https://github.com/' + canonical_source + '/releases/tag/v' + row['manifest_version'],
                    'Verified rollout release must match its canonical source and version')
            for field in ('manifest_evidence_url', 'release_url', 'release_download_verification_url'):
                evidence(row.get(field), directory)
            require(row.get('destination_codex_access') == 'verified', 'Codex coverage must be verified')
            require(row.get('destination_sfl_app_access') == 'verified', 'SFL App coverage must be verified')
            for field in ('transfer_evidence_url', 'review_pr_url', 'gate_run_url', 'status_evidence_url'):
                evidence(row.get(field), directory)
            for field in ('review_registration_url', 'review_registry_status_url', 'review_artifact_url'):
                evidence(row.get(field), directory)
            identity = row.get('review_artifact_identity')
            require(isinstance(identity, dict) and identity.get('runtime') == 'sfl_registered_codex' and
                    identity.get('app_id') == 1144995 and identity.get('bot_user_id') == 199175422 and
                    identity.get('reviewed_head_sha') == row['review_head_sha'] and
                    identity.get('reviewed_base_sha') == row['review_base_sha'],
                    'Verified rollout needs an SFL registered Codex immutable review artifact identity')
            runs = row.get('wider_workflow_run_urls')
            require(isinstance(runs, list), 'Wider workflow evidence must be a list')
            wider = row['selected_tier'] not in {'reviewer', 'custom'} or (
                row['selected_tier'] == 'custom' and
                bool(set(row['selected_components']) & (supported_components - {'sfl-pr-review-auto'})))
            require(not wider or bool(runs), 'Wider tier needs workflow evidence')
            for run in runs:
                evidence(run, directory)
            validate_registered_review(row, directory, row['destination'])
            verified_rollouts += 1
    if app_transfer['status'] == 'verified':
        require(all(row.get('retained_app_dependency', {}).get('status') == 'verified'
                    for row in records if row['repository_id'] in retained),
                'App transfer requires verified retained App dependencies')
    require(seen_matrix == set(expected), 'Matrix must cover every baseline ID exactly once')
    summary = {'repositories': len(expected), 'active': sum(not repo['archived'] for repo in expected.values()),
               'archived': sum(repo['archived'] for repo in expected.values()), 'verified_rollouts': verified_rollouts,
               'planned_transfers': len(expected) - len(retained), 'retained_sources': len(retained)}
    require(matrix.get('summary') == summary, 'Matrix summary is stale')
    extras = matrix.get('disposable_validation_repositories')
    require(isinstance(extras, list) and bool(extras), 'Disposable validation inventory must remain present')
    extra_ids, extra_names, verified_pilot_visibilities = set(), set(), set()
    verified_wider_pilot = False
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
            require(receipts.get('deployment_source') == canonical_source and
                    semantic_version(receipts.get('release_version')) and immutable_sha(receipts.get('deployment_sha')),
                    'Verified pilot needs canonical immutable deployment release identity')
            manifest = receipts.get('manifest_identity')
            require(isinstance(manifest, dict) and manifest.get('source') == receipts['deployment_source'] and
                    manifest.get('sourceSha') == receipts['deployment_sha'] and
                    manifest.get('version') == receipts['release_version'] and manifest.get('tier') in TIERS,
                    'Verified pilot manifest must match its deployment source, SHA and version')
            for field in ('manifest_evidence_url', 'release_download_verification_url'):
                evidence(receipts.get(field), directory)
            identity = receipts.get('review_artifact_identity')
            require(isinstance(identity, dict) and identity.get('runtime') == 'sfl_registered_codex' and
                    identity.get('app_id') == 1144995 and identity.get('bot_user_id') == 199175422 and
                    all(isinstance(identity.get(field), str) and
                        re.fullmatch(r'[0-9a-f]{40}', identity[field])
                        for field in ('reviewed_head_sha', 'reviewed_base_sha')),
                    'Verified pilot needs immutable SFL registered Codex review identity')
            validate_registered_review(receipts, directory, name)
            require(receipts.get('requester_permission') in {'write', 'maintain', 'admin'},
                    'Verified pilot needs an authorized human requester')
            require(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9-]{0,38}', receipts['review_requester']) is not None,
                    'Verified pilot requester must be a human GitHub login')
            evidence(receipts.get('requester_permission_evidence_url'), directory)
            require(re.fullmatch('https://github.com/' + re.escape(name) + r'/pull/[1-9][0-9]*', receipts['review_pr_url']) is not None,
                    'Verified pilot review must use a same-repository PR')
            require(identity.get('review_pr_url') == receipts['review_pr_url'] and
                    identity.get('requester') == receipts['review_requester'],
                    'Pilot artifact must bind its recorded requester and reviewed PR')
            runs = receipts.get('wider_workflow_run_urls')
            require(isinstance(runs, list), 'Pilot needs an explicit wider workflow receipt list')
            if runs:
                require(manifest['tier'] in {'minimal', 'standard', 'full'},
                        'Wider pilot must declare a wider deployed configuration')
                for run in runs:
                    evidence(run, directory)
                evidence(receipts.get('auditor_run_url'), directory)
                verified_wider_pilot = True
            verified_pilot_visibilities.add(extra['visibility'])
        extra_ids.add(extra_id)
        extra_names.add(name.casefold())
    source_complete = all(row['health'] in {'verified', 'archived_verified', 'scope_exception', 'retained_source', 'source_verified'} for row in records)
    if verified_rollouts or source_complete:
        require(all(row['retained_app_dependency']['status'] == 'verified' for row in records
                    if row['health'] == 'retained_source'),
                'Completed rollout requires verified retained App dependencies')
        require(verified_pilot_visibilities == {'public', 'private'},
                'Active rollout requires verified public and private onboarding pilots first')
        require(verified_wider_pilot, 'Active rollout requires a verified wider-workflow and auditor pilot')
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
