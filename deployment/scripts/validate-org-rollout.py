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
INIT_TIERS = {'reviewer', 'minimal', 'standard', 'full'}
APPROVED_PILOTS = {1408025382: ('hemsoft-dev/sfl-migration-pilot-private', 'private'),
                   1408029795: ('hemsoft-dev/sfl-migration-pilot-public', 'public')}
FHEMMER_REPOSITORY_ID = 1143951439
LEGACY_UNUSED_RECEIPT = 'https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6028911622'
EXTERNAL_SCOPE_RECEIPT = 'https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6029136048'
PROVIDER_ABSENCE_RECEIPT = 'https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6028753359'
RETAINED_APP_RECEIPT = 'https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6028536416'
RETAINED_APP_DISPOSITION = 'No SFL App dependency; existing SFL credentials unused. Preserve personal hosting.'
PILOT_SCENARIOS = {'findings':'gate_failed', 'pending_request':'gate_blocked', 'malformed_output':'gate_blocked',
                   'revoked_permission':'gate_blocked', 'permission_lookup_failure':'gate_blocked',
                   'forged_registration':'gate_blocked', 'edited_registration':'gate_blocked',
                   'duplicate_delivery':'idempotent', 'new_head':'stale_gate_rejected', 'base_advance':'stale_gate_rejected'}
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


def local_capture(reference, directory, label):
    evidence(reference, directory)
    require(not urllib.parse.urlsplit(reference).scheme, label + ' needs an independent local capture')
    capture = json.loads((directory / reference).read_text())
    require(isinstance(capture, dict), label + ' capture must be an object')
    return capture


def observed_time(value, label):
    require(text(value), label + ' needs an observation timestamp')
    try:
        timestamp = datetime.datetime.fromisoformat(value.replace('Z', '+00:00'))
    except ValueError as exc:
        raise ValueError(label + ' needs a valid observation timestamp') from exc
    require(timestamp.tzinfo is not None, label + ' timestamp needs a timezone')
    return timestamp


def deployed_revision(proof, directory, repository_id, repository, manifest):
    capture = local_capture(proof.get('manifest_evidence_url'), directory, 'Deployed revision')
    require(capture.get('repository_id') == repository_id and capture.get('repository') == repository and
            immutable_sha(capture.get('revision_sha')) and capture.get('manifest') == manifest,
            'Deployed revision capture must match its repository and installed manifest')
    observed_time(capture.get('observed_at'), 'Deployed revision')
    return capture['revision_sha']


def string_list(value):
    return isinstance(value, list) and all(text(item) for item in value) and len(set(value)) == len(value)


def semantic_version(value):
    pattern = r'(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?'
    return isinstance(value, str) and re.fullmatch(pattern, value) is not None


def immutable_sha(value):
    return isinstance(value, str) and re.fullmatch(r'[0-9a-f]{40}', value) is not None


def bound_operation(operation, directory, repository_id, repository, sha, version, url):
    require(isinstance(operation, dict) and operation.get('repository_id') == repository_id and
            operation.get('repository') == repository and operation.get('deployment_sha') == sha and
            operation.get('release_version') == version and operation.get('evidence_url') == url,
            'Operation receipt must bind its repository, deployment and release')
    evidence(url, directory)
    if urllib.parse.urlsplit(url).scheme:
        require(url.startswith('https://github.com/' + repository + '/'),
                'Operation URL must belong to its designated repository')


def deployed_workflow_paths(tier, addons=(), components=None):
    root = pathlib.Path(__file__).resolve().parents[2]
    source = (root / 'gh-sfl/init.go').read_text().split('var tierWorkflows =', 1)[1].split('var tierComponents', 1)[0]
    tiers = {name: re.findall(r'"([^"\n]+)"', body)
             for name, body in re.findall(r'"([^"\n]+)":\s*\{([^}]+)\}', source)}
    files = tiers.get(tier, []) if tier != 'custom' else [
        name for name in tiers['full'] if name.removesuffix('.md').removesuffix('.yml') in (components or [])]
    addon_source = (root / 'gh-sfl/addons.go').read_text().split('var addonWorkflows =', 1)[1].split('var addonDescriptions', 1)[0]
    addon_files = {name: re.findall(r'"([^"\n]+)"', body)
                   for name, body in re.findall(r'"([^"\n]+)":\s*\{([^}]+)\}', addon_source)}
    files = list(files) + [name for addon in addons for name in addon_files.get(addon, [])]
    return {'.github/workflows/' + (name[:-3] + '.lock.yml' if name.endswith('.md') else name)
            for name in files}


def bound_workflow_operation(operation, directory, repository_id, repository, sha, version, url, workflows, run_head):
    bound_operation(operation, directory, repository_id, repository, sha, version, url)
    require(operation.get('conclusion') == 'success' and operation.get('workflow') in workflows and
            immutable_sha(run_head) and operation.get('run_head_sha') == run_head,
            'Workflow operation must identify a successful expected deployed workflow and immutable run head')
    require(re.fullmatch('https://github.com/' + re.escape(repository) + r'/actions/runs/[1-9][0-9]*', url) is not None,
            'Workflow operation must reference its repository Actions run')


def validate_gate_policy(policy, directory, repository_id, repository, target_branch):
    require(isinstance(policy, dict) and policy.get('state') == 'required' and
            policy.get('context') == 'SFL Reviewer Gate Runner' and policy.get('app_id') == 15368 and
            policy.get('strict') is True and policy.get('repository_id') == repository_id and
            policy.get('repository') == repository and text(target_branch) and policy.get('branch') == target_branch,
            'Completed review needs a repository-bound required strict SFL gate bound to Actions')
    reference = policy.get('evidence_url')
    evidence(reference, directory)
    require(not urllib.parse.urlsplit(reference).scheme,
            'Gate policy needs an independent local effective-policy capture')
    capture = json.loads((directory / reference).read_text())
    require(capture.get('repository_id') == repository_id and capture.get('repository') == repository and
            capture.get('branch') == policy['branch'], 'Gate policy capture must match its destination repository and branch')
    observed_at = datetime.datetime.fromisoformat(capture.get('observed_at', '').replace('Z', '+00:00'))
    require(observed_at.tzinfo is not None, 'Gate policy capture needs a timezone')
    rules = capture.get('effective_rules', {})
    require(rules.get('state') == 'observed' and isinstance(rules.get('data'), list),
            'Gate policy capture needs observed effective branch rules')
    required = any(rule.get('type') == 'required_status_checks' and
                   rule.get('parameters', {}).get('strict_required_status_checks_policy') is True and
                   any(check.get('context') == policy['context'] and check.get('integration_id') == 15368
                       for check in rule.get('parameters', {}).get('required_status_checks', []))
                   for rule in rules['data'])
    classic = capture.get('classic_protection', {})
    require(classic.get('state') == 'observed' or
            (classic.get('state') == 'absent' and classic.get('http_status') == 404),
            'Gate policy capture needs resolved classic protection')
    checks = classic.get('data', {}).get('required_status_checks') or {}
    required = required or (checks.get('strict') is True and
                           any(check.get('context') == policy['context'] and check.get('app_id') == 15368
                               for check in checks.get('checks', [])))
    require(required, 'Effective destination policy must actually require the strict Actions-owned SFL gate')


def validate_app_credential(proof, directory):
    require(isinstance(proof, dict) and proof.get('repository_id') == 1169772257 and
            proof.get('repository') == 'HemSoft/set-it-free-loop' and
            proof.get('workflow') == '.github/workflows/verify-sfl-app-credential.yml' and
            proof.get('conclusion') == 'success' and immutable_sha(proof.get('reviewed_sha')) and
            proof.get('app_id') == 4448946 and proof.get('client_id') == 'Iv23liwvwJJUh2bUIKLW' and
            proof.get('owner') == 'HemSoft' and type(proof.get('installation_id')) is int and
            proof['installation_id'] == 150383874 and proof.get('repository_selection') == 'all' and
            proof.get('permission_ceiling_verified') is True,
            'Transfer gates need a successful reviewed owned-App credential workflow receipt')
    require(re.fullmatch(r'https://github.com/HemSoft/set-it-free-loop/actions/runs/[1-9][0-9]*',
                        proof.get('run_url', '')) is not None,
            'Owned-App credential run must belong to the source workflow')
    evidence(proof['run_url'], directory)
    metadata = local_capture(proof.get('credential_metadata_evidence_url'), directory, 'App credential metadata')
    require(all(metadata.get(field) == proof.get(field) for field in
                ('repository_id', 'repository', 'reviewed_sha', 'run_url', 'app_id', 'client_id', 'owner',
                 'installation_id', 'repository_selection', 'permission_ceiling_verified')) and
            metadata.get('credential_verification') == 'success' and
            metadata.get('installation_owner') == 'HemSoft' and metadata.get('target_type') == 'User',
            'App credential assertions must match the uploaded workflow metadata')
    observed_time(metadata.get('observed_at'), 'App credential metadata')
    run = local_capture(proof.get('workflow_run_evidence_url'), directory, 'App credential workflow run')
    require(run.get('repository', {}).get('id') == proof['repository_id'] and
            run.get('repository', {}).get('full_name') == proof['repository'] and
            run.get('html_url') == proof['run_url'] and run.get('head_sha') == proof['reviewed_sha'] and
            run.get('path') == proof['workflow'] and run.get('head_branch') == 'main' and
            run.get('status') == 'completed' and run.get('conclusion') == 'success',
            'App credential capture must come from its successful reviewed main workflow run')


def protection_semantics(value, repo, url_field=False):
    if isinstance(value, dict):
        return {key: protection_semantics(item, repo, key == 'url' or key.endswith('_url')) for key, item in value.items()}
    if isinstance(value, list):
        return [protection_semantics(item, repo, url_field) for item in value]
    if isinstance(value, str) and url_field:
        for prefix in ('https://api.github.com/repos/', 'https://github.com/'):
            source = prefix + repo['full_name'] + '/'
            if value.startswith(source):
                return prefix + repo['destination'] + '/' + value[len(source):]
    return value


def protection_contract(repo, directory=None):
    rulesets = repo['settings']['rulesets']
    if rulesets.get('state') == 'unverified':
        require(repo['id'] == FHEMMER_REPOSITORY_ID, 'Unverified baseline rulesets need independent resolution')
        if directory is None:
            directory = pathlib.Path(__file__).resolve().parents[2] / 'docs/organization-migration'
        capture = json.loads((directory / 'fhemmer-protection-evidence.json').read_text())
        require(capture.get('repository_id') == repo['id'] and capture.get('repository') == repo['full_name'] and
                capture.get('identity') == 'HemSoft authenticated GitHub owner browser' and
                capture.get('classic', {}).get('observation') == 'Classic branch protections have not been configured' and
                capture.get('rulesets', {}).get('observation') ==
                "You haven't created any rulesets; rulesets won't be enforced on this private repository until source organization upgrades to GitHub Team",
                'Unverified fhemmer protection contract needs its independent owner capture')
        rules = []
    else:
        rules = []
        for rule in rulesets.get('data', []):
            detail = rule.get('details', {})
            require(detail.get('state') == 'observed', 'Baseline ruleset details must be observed')
            value = detail['data']
            rules.append({key: value.get(key) for key in
                          ('id', 'name', 'target', 'enforcement', 'conditions', 'rules', 'bypass_actors')})
    branches = repo['settings']['protected_branches']
    require(branches.get('state') == 'observed', 'Baseline protected branches must be observed')
    classic = {}
    for branch in branches.get('data', []):
        protection = branch.get('protection', {})
        if protection.get('state') == 'observed':
            classic[branch['name']] = protection['data']
        else:
            require(protection.get('http_status') == 404, 'Unknown classic protection needs resolution')
    return {'rulesets': rules, 'classic': classic}


def validate_protection_preservation(proof, directory, repo):
    require(isinstance(proof, dict) and proof.get('repository_id') == repo['id'] and
            proof.get('repository') == repo['destination'] and immutable_sha(proof.get('revision_sha')) and
            isinstance(proof.get('rulesets'), list) and isinstance(proof.get('classic'), dict),
            'Completed transfer needs structured destination effective-policy evidence')
    capture = local_capture(proof.get('evidence_url'), directory, 'Destination protections')
    require(capture.get('phase') == 'post_transfer' and
            all(capture.get(field) == proof.get(field) for field in
                ('repository_id', 'repository', 'revision_sha', 'observed_at', 'rulesets', 'classic')),
            'Destination protections must match the independent post-transfer capture')
    observed_time(capture.get('observed_at'), 'Destination protections')
    baseline = protection_contract(repo, directory)
    for rule in baseline['rulesets']:
        require(rule in proof['rulesets'], 'Destination must preserve every unrelated baseline ruleset')
    for name, protection in baseline['classic'].items():
        require(protection_semantics(proof['classic'].get(name), repo) == protection_semantics(protection, repo),
                'Destination must preserve every unrelated classic branch protection')


def validate_release_download(proof, directory, repository_id, repository, source, sha, version):
    release_url = 'https://github.com/' + source + '/releases/tag/v' + version
    download = local_capture(proof.get('release_download_verification_url'), directory, 'Release download')
    require(download.get('release_url') == release_url and
            download.get('source_repository') == source and
            download.get('source_sha') == sha and
            download.get('release_version') == version and
            type(download.get('source_repository_id')) is int and download['source_repository_id'] == 1169772257 and
            download.get('target_repository_id') == repository_id and download.get('target_repository') == repository and
            text(download.get('asset_name')) and '/' not in download['asset_name'] and
            download.get('asset_url') == 'https://github.com/' + source +
                '/releases/download/v' + version + '/' + urllib.parse.quote(download['asset_name']) and
            re.fullmatch(r'[0-9a-f]{64}', download.get('expected_sha256', '')) is not None and
            download.get('actual_sha256') == download['expected_sha256'] and
            download.get('checksum_verified') is True and download.get('attestation_verified') is True,
            'Release download capture must verify its canonical asset, source SHA and matching SHA256')
    observed_time(download.get('observed_at'), 'Release download')


def validate_final_onboarding(proof, directory, expected, organization, app_id):
    require(isinstance(proof, dict) and proof.get('status') == 'verified',
            'Final completion needs the separate post-rollout new-repository onboarding proof')
    repo_id = proof.get('repository_id')
    name = proof.get('repository')
    require(type(repo_id) is int and repo_id > 0 and repo_id not in expected and repo_id not in APPROVED_PILOTS and
            name == organization + '/sfl-migration-new-repository' and proof.get('visibility') in {'public', 'private'},
            'Final onboarding must use a distinct designated organization repository')
    reference = proof.get('metadata_evidence_url')
    evidence(reference, directory)
    require(not urllib.parse.urlsplit(reference).scheme, 'New repository creation needs an independent metadata capture')
    capture = json.loads((directory / reference).read_text())
    metadata = capture.get('metadata', {})
    require(metadata.get('id') == repo_id and metadata.get('full_name') == name and
            metadata.get('private') == (proof['visibility'] == 'private') and metadata.get('archived') is False,
            'Post-rollout onboarding must match independently captured repository identity and state')
    completion_reference = proof.get('rollout_completion_evidence_url')
    evidence(completion_reference, directory)
    require(not urllib.parse.urlsplit(completion_reference).scheme and completion_reference != reference,
            'Rollout completion needs an independent baseline completion capture')
    completion = json.loads((directory / completion_reference).read_text())
    require(completion.get('organization') == organization and
            completion.get('completed_at') == proof.get('rollout_completed_at') and
            completion.get('deployment_sha') == proof.get('deployment_sha') and
            completion.get('release_version') == proof.get('release_version'),
            'Rollout completion timestamp must match its independently captured deployment')
    completed = completion.get('repositories')
    require(isinstance(completed, list) and len(completed) == len(expected) and
            {entry.get('repository_id') for entry in completed} == set(expected),
            'Rollout completion capture must account for every baseline repository')
    for entry in completed:
        baseline = expected[entry['repository_id']]
        name_at_completion = baseline['full_name'] if baseline['id'] in APPROVED_RETAINED_IDS else baseline['destination']
        require(entry.get('repository') == name_at_completion and
                entry.get('health') in {'verified', 'archived_verified', 'scope_exception', 'retained_source', 'source_verified'},
                'Rollout completion capture needs terminal repository-bound outcomes')
        completed_at = datetime.datetime.fromisoformat(entry.get('completed_at', '').replace('Z', '+00:00'))
        cutoff = datetime.datetime.fromisoformat(completion['completed_at'].replace('Z', '+00:00'))
        require(completed_at.tzinfo is not None and cutoff.tzinfo is not None and completed_at <= cutoff,
                'Baseline completion timestamps cannot follow the recorded rollout completion')
    created_at = datetime.datetime.fromisoformat(metadata.get('created_at', '').replace('Z', '+00:00'))
    rollout_at = datetime.datetime.fromisoformat(proof.get('rollout_completed_at', '').replace('Z', '+00:00'))
    require(created_at.tzinfo is not None and rollout_at.tzinfo is not None and created_at > rollout_at,
            'New onboarding repository must be created after the recorded rollout completion')
    require(proof.get('deployment_source') == organization + '/set-it-free-loop' and
            immutable_sha(proof.get('deployment_sha')) and semantic_version(proof.get('release_version')),
            'New onboarding needs the canonical immutable deployment and release')
    manifest = proof.get('manifest_identity')
    addons, _ = workflow_catalog()
    require(isinstance(manifest, dict) and manifest.get('source') == proof['deployment_source'] and
            manifest.get('sourceSha') == proof['deployment_sha'] and manifest.get('version') == proof['release_version'] and
            manifest.get('tier') in {'minimal', 'standard', 'reviewer', 'full'} and
            string_list(manifest.get('addons')) and set(manifest['addons']) <= addons and
            '.github/workflows/sfl-pr-review-auto.yml' in deployed_workflow_paths(manifest['tier'],manifest['addons']),
            'New onboarding must prove its canonical post-status observer manifest')
    reference = proof.get('manifest_evidence_url')
    evidence(reference,directory)
    require(not urllib.parse.urlsplit(reference).scheme,'New onboarding needs an independent post-status manifest capture')
    observed = json.loads((directory/reference).read_text())
    require(observed.get('repository_id') == repo_id and observed.get('repository') == name and
            observed.get('manifest') == manifest and immutable_sha(observed.get('revision_sha')),
            'New onboarding manifest capture must match its repository and deployed configuration')
    require(proof.get('release_url') == 'https://github.com/'+proof['deployment_source']+'/releases/tag/v'+proof['release_version'],
            'New onboarding release verification must use its canonical version')
    validate_release_download(proof, directory, repo_id, name, proof['deployment_source'], proof['deployment_sha'], proof['release_version'])
    operations = proof.get('onboarding_operation_receipts')
    fields = ('init_pr_url', 'repeat_onboarding_url', 'sync_pr_url', 'repeat_sync_url', 'status_url')
    require(isinstance(operations, dict) and set(operations) == set(fields),
            'New onboarding needs initial and repeated onboarding/sync/status operation receipts')
    for field in fields:
        bound_operation(operations[field], directory, repo_id, name, proof['deployment_sha'],
                        proof['release_version'], proof.get(field))
        operation = operations[field]
        command = 'init' if field in {'init_pr_url','repeat_onboarding_url'} else 'status' if field == 'status_url' else 'sync'
        outcomes = {'pull_request_merged'} if field == 'init_pr_url' else {'no_changes','pull_request_merged'} if field == 'sync_pr_url' else {'healthy'} if field == 'status_url' else {'no_changes'}
        require(operation.get('command') == command and operation.get('outcome') in outcomes and
                immutable_sha(operation.get('revision_after')),
                'New onboarding operation must prove successful init/sync/status outcomes at the observed revision')
        if operation['outcome'] == 'pull_request_merged':
            require(operation.get('merged') is True and
                    re.fullmatch('https://github.com/'+re.escape(name)+r'/pull/[1-9][0-9]*',proof[field]) is not None,
                    'New onboarding mutation needs its actual merged deployment PR')
        elif operation['outcome'] == 'no_changes':
            require(operation.get('change_count') == 0 and operation.get('revision_before') == operation['revision_after'],
                    'Repeated onboarding and sync must prove zero changes at the same observed revision')
    require(operations['repeat_onboarding_url']['revision_before'] == operations['init_pr_url']['revision_after'] and
            operations['sync_pr_url']['revision_before'] == operations['repeat_onboarding_url']['revision_after'] and
            operations['repeat_sync_url']['revision_before'] == operations['sync_pr_url']['revision_after'] and
            operations['status_url']['revision_after'] == operations['repeat_sync_url']['revision_after'] == observed['revision_sha'],
            'New onboarding revisions must follow init, repeated init, sync and final status in order')
    validate_app_coverage(proof.get('destination_sfl_app_access'), directory, repo_id, name, app_id, organization)
    validate_app_coverage(proof.get('destination_codex_access'), directory, repo_id, name, 1144995, organization)
    validate_registered_review(proof, directory, name, target_branch=metadata.get('default_branch'), deployment_revision=observed['revision_sha'])
    return proof


def validate_final_inventory(proof, directory, expected, retained, approved_onboarding=None):
    require(isinstance(proof, dict), 'Final completion needs a fresh independent repository inventory')
    reference = proof.get('evidence_url')
    evidence(reference, directory)
    require(not urllib.parse.urlsplit(reference).scheme and reference != 'inventory.json',
            'Final inventory needs a separate captured enumeration artifact')
    capture = json.loads((directory / reference).read_text())
    require(capture.get('observed_at') == proof.get('observed_at'), 'Final inventory timestamp must match its capture')
    captured_at = datetime.datetime.fromisoformat(capture['observed_at'].replace('Z', '+00:00'))
    require(captured_at.tzinfo is not None, 'Final inventory timestamp needs a timezone')
    accounts = capture.get('accounts')
    require(isinstance(accounts, list) and {x.get('owner') for x in accounts} == {'HemSoft', 'fhemmer', 'hemsoft-dev'} and
            len(accounts) == 3, 'Final inventory must enumerate both sources and the destination')
    actual = {}
    for account in accounts:
        require(account.get('state') == 'observed' and account.get('all_pages') is True and
                isinstance(account.get('repositories'), list), 'Final inventory enumeration must be complete')
        for repo in account['repositories']:
            repo_id = repo.get('id')
            require(type(repo_id) is int and repo_id not in actual and
                    text(repo.get('full_name')) and repo['full_name'].startswith(account['owner'] + '/'),
                    'Final inventory repository IDs must be unique and bound to their account')
            actual[repo_id] = repo
    for repo_id, repo in expected.items():
        current = actual.get(repo_id, {})
        require(current.get('full_name') == (repo['full_name'] if repo_id in retained else repo['destination']) and
                current.get('private') == repo['private'] and current.get('archived') == repo['archived'],
                'Final inventory must prove each baseline ID at its actual mapped location and preserve state')
    extras = proof.get('additional_repositories')
    require(isinstance(extras, list), 'Final inventory needs explicit additional-repository accounting')
    allowed = dict(APPROVED_PILOTS)
    if approved_onboarding is not None:
        allowed[approved_onboarding['repository_id']] = (approved_onboarding['repository'], approved_onboarding['visibility'])
    for repo in extras:
        repo_id = repo.get('repository_id')
        require(type(repo_id) is int and repo_id not in expected and repo_id not in allowed and
                repo.get('approved_by') == 'HemSoft' and text(repo.get('repository')) and
                repo.get('visibility') in {'public', 'private'}, 'Additional final repositories need a bound owner decision')
        evidence(repo.get('evidence_url'), directory)
        decision_reference = repo.get('decision_artifact')
        evidence(decision_reference, directory)
        require(not urllib.parse.urlsplit(decision_reference).scheme and decision_reference != reference,
                'Additional repository needs an independent owner decision artifact')
        decision = json.loads((directory / decision_reference).read_text())
        require(all(decision.get(key) == repo.get(key) for key in
                    ('repository_id', 'repository', 'visibility', 'approved_by', 'evidence_url')) and
                decision.get('disposition') == 'include_final_inventory' and text(decision.get('reason')),
                'Additional repository must match its independent owner decision')
        approved_at = datetime.datetime.fromisoformat(decision.get('approved_at', '').replace('Z', '+00:00'))
        require(approved_at.tzinfo is not None and approved_at <= captured_at,
                'Additional repository needs a dated owner approval before final capture')
        require(re.fullmatch(r'https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-[1-9][0-9]*',
                             decision.get('evidence_url', '')) is not None,
                'Additional repository needs an explicit owner issue receipt')
        allowed[repo_id] = (repo['repository'], repo['visibility'])
    require(set(actual) == set(expected) | set(allowed), 'Final inventory contains unaccounted repository IDs')
    for repo_id, (name, visibility) in allowed.items():
        require(actual[repo_id]['full_name'] == name and actual[repo_id].get('private') == (visibility == 'private'),
                'Final inventory additions must match their recorded identity and visibility')


def validate_app_coverage(coverage, directory, repository_id, repository, app_id, owner):
    require(isinstance(coverage, dict) and coverage.get('status') == 'verified' and
            coverage.get('app_id') == app_id and coverage.get('owner') == owner and
            coverage.get('repository_id') == repository_id and coverage.get('repository') == repository and
            type(coverage.get('installation_id')) is int and coverage['installation_id'] > 0,
            'Verified destination needs repository-bound post-transfer SFL App coverage')
    evidence(coverage.get('evidence_url'), directory)
    if app_id == 4448946:
        capture = json.loads((directory/'owned-app-organization-installation-evidence.json').read_text())
        require(capture.get('verification_status') == 'verified' and capture.get('account',{}).get('login') == owner and
                capture.get('installation',{}).get('id') == coverage['installation_id'] and
                capture['installation'].get('app_id') == app_id and capture['installation'].get('repository_selection') == 'all',
                'SFL coverage must match the captured all-repositories destination installation')
    if app_id == 1144995:
        capture = json.loads((directory / 'codex-organization-installation-evidence.json').read_text())
        require(coverage['installation_id'] == capture['installation']['id'] and
                capture['installation']['app_id'] == app_id and capture['account']['login'] == owner,
                'Codex coverage must match the independently captured organization installation')


def validate_registered_review(row, directory, repository, repository_id=None, target_branch=None, deployment_revision=None):
    if repository_id is None:
        repository_id = row.get('repository_id')
    validate_gate_policy(row.get('gate_policy'), directory, repository_id, repository, target_branch)
    require(text(row.get('review_requester')) and immutable_sha(row.get('review_head_sha')) and
            immutable_sha(row.get('review_base_sha')), 'Completed review needs immutable registered review context')
    require(row.get('requester_permission') in {'write','maintain','admin'} and
            re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9-]{0,38}', row['review_requester']) is not None,
            'Completed review needs a write-or-higher human requester')
    permission = local_capture(row.get('requester_permission_evidence_url'), directory, 'Requester permission')
    require(permission.get('repository_id') == repository_id and permission.get('repository') == repository and
            permission.get('actor') == row['review_requester'] and
            permission.get('pr_url') == row.get('review_pr_url') and
            permission.get('head_sha') == row['review_head_sha'] and permission.get('base_sha') == row['review_base_sha'] and
            permission.get('http_status') == 200 and isinstance(permission.get('result'), dict),
            'Requester permission capture must bind its actor, repository and reviewed PR context')
    result = permission['result']
    role = result.get('role_name') or result.get('permission')
    require(role == row['requester_permission'] and role in {'write', 'maintain', 'admin'} and
            result.get('permission') in {'write', 'admin'}, 'Requester permission capture must independently grant write or higher')
    observed_time(permission.get('observed_at'), 'Requester permission')
    require(re.fullmatch('https://github.com/' + re.escape(repository) + r'/pull/[1-9][0-9]*',
                        row.get('review_pr_url','')) is not None,
            'Completed review needs a same-destination-repository PR')
    for field in ('review_pr_url', 'gate_run_url', 'review_registration_url',
                  'review_registry_status_url', 'review_artifact_url'):
        evidence(row.get(field), directory)
    receipts = row.get('review_operation_receipts')
    fields = ('gate_run_url', 'review_registration_url', 'review_registry_status_url', 'review_artifact_url')
    require(isinstance(receipts, dict) and set(receipts) == set(fields),
            'Completed review needs repository/head/base-bound operation receipts')
    for field in fields:
        proof = receipts[field]
        require(isinstance(proof, dict) and proof.get('repository_id') == repository_id and
                proof.get('repository') == repository and proof.get('head_sha') == row['review_head_sha'] and
                proof.get('base_sha') == row['review_base_sha'] and proof.get('pr_url') == row['review_pr_url'] and
                proof.get('evidence_url') == row[field], 'Review operation receipt must bind its repository, PR, head and base')
        require(row[field].startswith('https://github.com/' + repository + '/') or
                row[field].startswith('https://api.github.com/repos/' + repository + '/'),
                'Review operation URL must belong to its designated repository')
    require(immutable_sha(deployment_revision), 'Registered review needs its captured deployed revision')
    ancestry = local_capture(row.get('review_deployment_evidence_url'), directory, 'Review deployment ancestry')
    require(ancestry.get('repository_id') == repository_id and ancestry.get('repository') == repository and
            ancestry.get('deployed_revision') == deployment_revision and ancestry.get('reviewed_base_sha') == row['review_base_sha'] and
            ancestry.get('pr_url') == row['review_pr_url'] and ancestry.get('head_sha') == row['review_head_sha'],
            'Review deployment capture must bind its repository, PR and deployed revision')
    comparison = ancestry.get('comparison', {})
    require(comparison.get('status') in {'ahead', 'identical'} and
            comparison.get('base_commit', {}).get('sha') == deployment_revision and
            comparison.get('merge_base_commit', {}).get('sha') == deployment_revision and
            comparison.get('html_url') == 'https://github.com/' + repository + '/compare/' +
                deployment_revision + '...' + row['review_base_sha'],
            'Reviewed base must contain the captured deployed revision')
    observed_time(ancestry.get('observed_at'), 'Review deployment ancestry')
    identity = row.get('review_artifact_identity')
    require(isinstance(identity, dict) and identity.get('runtime') == 'sfl_registered_codex' and
            identity.get('app_id') == 1144995 and identity.get('bot_user_id') == 199175422 and identity.get('reviewed_head_sha') == row['review_head_sha'] and
            identity.get('reviewed_base_sha') == row['review_base_sha'] and
            identity.get('review_pr_url') == row['review_pr_url'] and
            identity.get('requester') == row['review_requester'],
            'SFL registered Codex immutable artifact must bind its requester, PR, head and base')
    gate = receipts['gate_run_url']
    require(re.fullmatch('https://github.com/' + re.escape(repository) + r'/(?:actions/runs|runs)/[1-9][0-9]*', row['gate_run_url']) is not None,
            'Completed gate needs an actual repository Actions run or check-run URL')
    require(gate.get('status') == 'completed' and gate.get('conclusion') == 'success' and
            gate.get('workflow') == '.github/workflows/sfl-pr-review-auto.yml' and gate.get('app_id') == 15368 and
            gate.get('context') == 'SFL Reviewer Gate Runner',
            'Completed gate must prove a successful terminal Actions-owned observer result')
    captured_gate = local_capture(gate.get('capture_evidence_url'), directory, 'Gate result')
    require(all(captured_gate.get(field) == gate.get(field) for field in
                ('repository_id', 'repository', 'head_sha', 'base_sha', 'pr_url', 'evidence_url',
                 'status', 'conclusion', 'workflow', 'app_id', 'context')) and
            captured_gate.get('artifact_identity') == identity,
            'Gate result capture must match the completed gate and recorded immutable review artifact')
    observed_time(captured_gate.get('observed_at'), 'Gate result')


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
    absence = json.loads((directory / 'provider-absence-owner-evidence.json').read_text())
    external_scope = json.loads((directory / 'external-resource-owner-scope-evidence.json').read_text())
    require(absence.get('evidence_url') == PROVIDER_ABSENCE_RECEIPT and
            absence.get('confirmed_by') == 'Franz (HemSoft owner)' and
            absence.get('scope') == '65 approved transfer targets' and
            set(absence.get('providers', [])) == {'Azure Pipelines', 'Fly.io', 'Railway'} and
            absence.get('disposition') == 'none uses these providers' and
            external_scope.get('evidence_url') == EXTERNAL_SCOPE_RECEIPT and
            external_scope.get('confirmed_by') == 'Franz (HemSoft owner)' and
            external_scope.get('scope') == '65 approved transfer targets' and
            external_scope.get('blacksmith_usage') == 'none' and
            external_scope.get('active_external_resources') ==
            'Only the inventoried Vercel, Cloudflare, Supabase, GitHub Pages and yahtzee runner resources',
            'Provider absence must match the approved owner evidence and exact transfer scope')
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
    vercel_capture = json.loads((directory / 'vercel-provider-evidence.json').read_text())
    vercel_resources = {(project['link']['repoId'], project['id']) for project in vercel_capture['projects']}
    require(all(repo_id in expected for repo_id, _ in vercel_resources), 'Captured Vercel resource targets unknown repository')
    seen_vercel_resources = set()
    workflow_capture = json.loads((directory / 'workflow-reference-evidence.json').read_text())
    workflow_refs = {}
    for record in workflow_capture['records']:
        workflow_refs.setdefault(record['source'], set()).update(record.get('referenced_secret_names', []))
    legacy_names = {'SFL_APP_PRIVATE_KEY', 'OPENROUTER_API_KEY'}
    pages_resources = {(repo_id, str(repo_id)) for repo_id, repo in expected.items() if repo['has_pages']}
    vercel_projects = json.loads((directory / 'vercel-provider-evidence.json').read_text())['projects']
    supabase_resources = set()
    unlinked_supabase = {}
    for project in json.loads((directory / 'supabase-provider-evidence.json').read_text())['projects']:
        connection = project.get('vercel_project_connection')
        if connection:
            linked = [item for item in vercel_projects if item['name'] == connection]
            require(len(linked) == 1, 'Supabase connection must match one captured Vercel project')
            supabase_resources.add((linked[0]['link']['repoId'], project['reference']))
        else:
            unlinked_supabase[project['reference']] = project
    account_resources = matrix.get('account_resource_preservation')
    require(isinstance(account_resources, list) and len(account_resources) == len(unlinked_supabase),
            'Every unlinked Supabase account resource needs explicit preservation accounting')
    seen_account_resources = set()
    supabase_owner = json.loads((directory / 'supabase-provider-evidence.json').read_text())['organization']['slug']
    for resource in account_resources:
        resource_id = resource.get('resource_id')
        require(resource_id in unlinked_supabase and resource_id not in seen_account_resources and
                resource.get('provider') == 'Supabase' and resource.get('resource_owner') == supabase_owner and
                resource.get('association') == 'account_owned_no_repository_link' and
                resource.get('resource_url') == unlinked_supabase[resource_id]['resource_url'] and
                resource.get('status') in {'pending', 'verified'},
                'Unlinked Supabase resource must match its independently captured account ownership')
        seen_account_resources.add(resource_id)
    cloudflare = json.loads((directory / 'now-leadership-live-hosting-evidence.json').read_text())['cloudflare_owner_verification']
    cloudflare_zones = {(1162179521, cloudflare['zone_id'])}
    cloudflare_workers = {(1162179521, app['name']) for app in cloudflare['workers_and_pages']['applications']
                          if app['type'] == 'Worker'}
    other_resources = {'github_pages': ('GitHub Pages', pages_resources),
                       'supabase_project': ('Supabase', supabase_resources),
                       'cloudflare_zone': ('Cloudflare', cloudflare_zones),
                       'cloudflare_worker': ('Cloudflare', cloudflare_workers)}
    seen_other_resources = {kind: set() for kind in other_resources}
    captured_manifests = {}
    for record in workflow_capture.get('records', []):
        if record.get('path') in {'.sfl/sfl.json', 'sfl.json'} and record.get('state') == 'observed':
            manifest = record.get('manifest')
            require(isinstance(manifest, dict), 'Captured installation needs its manifest content')
            repo_id = record['repository_id']
            require(repo_id not in captured_manifests or captured_manifests[repo_id] == manifest,
                    'Captured canonical and legacy manifests disagree')
            captured_manifests[repo_id] = manifest
    expected_unused = {}
    for repo_id, repo in expected.items():
        names = set(repo['settings']['secret_names'].get('data', [])) & legacy_names
        if repo_id not in retained and names and not (legacy_names & workflow_refs.get(repo['full_name'], set())):
            expected_unused[repo_id] = names
    unused_capture = json.loads((directory / 'legacy-unused-credential-owner-evidence.json').read_text())
    require(unused_capture.get('evidence_url') == LEGACY_UNUSED_RECEIPT and
            unused_capture.get('confirmed_by') == 'Franz (HemSoft owner)' and
            unused_capture.get('repository_count') == len(expected_unused) == 42,
            'Unused legacy credential confirmation must match the recorded owner scope')
    captured_unused = {}
    for record in unused_capture.get('repositories', []):
        repo_id = record.get('repository_id')
        names = record.get('unused_repository_secret_names')
        require(repo_id in expected_unused and repo_id not in captured_unused and
                record.get('source') == expected[repo_id]['full_name'] and string_list(names) and
                set(names) == expected_unused[repo_id], 'Unused credential artifact identity or names differ from scanned owner scope')
        captured_unused[repo_id] = set(names)
    require(captured_unused == expected_unused, 'Unused credential artifact must retain the exact 42-repository scope')
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
        if row.get('resource_kind') == 'vercel_project':
            resource = (repo_id, row.get('resource_id'))
            require(resource in vercel_resources and resource not in seen_vercel_resources,
                    'Unexpected or duplicate Vercel project resource')
            require(row.get('provider') == 'Vercel', 'Known Vercel project cannot be recorded as provider absence')
            seen_vercel_resources.add(resource)
        kind = row.get('resource_kind')
        if kind in other_resources:
            provider, resources = other_resources[kind]
            resource = (repo_id, row.get('resource_id'))
            require(resource in resources and resource not in seen_other_resources[kind],
                    'Unexpected or duplicate known Pages/Supabase resource')
            require(row.get('provider') == provider, 'Known Pages/Supabase resource cannot be provider absence')
            seen_other_resources[kind].add(resource)
        names = row.get('unused_repository_credential_names')
        require(isinstance(names, str), 'Ledger needs explicit unused credential names')
        listed_unused = [name.strip() for name in names.split(';') if name.strip()]
        require(len(listed_unused) == len(set(listed_unused)) and set(listed_unused) == expected_unused.get(repo_id, set()),
                'Ledger unused credential names differ from owner-confirmed scope')
        expected_reference = 'legacy-unused-credential-owner-evidence.json' if repo_id in expected_unused else ''
        require(row.get('unused_repository_credential_evidence_url') == expected_reference,
                'Ledger unused credential reference differs from owner evidence')
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
            require(repo_id not in retained and row.get('verified_by') == 'HemSoft' and
                    row.get('evidence_url') == EXTERNAL_SCOPE_RECEIPT and
                    verified_at >= datetime.datetime.fromisoformat(external_scope['confirmed_at'].replace('Z', '+00:00')),
                    'Verified provider absence needs the approved owner receipt for this transfer target')
        else:
            if row.get('resource_kind') == 'supabase_project' and row.get('resource_id') == 'cevpnetigzotgstxxjpm':
                decision = json.loads((directory/'dashboard-database-owner-disposition.json').read_text())
                require(decision.get('repository_id') == repo_id and decision.get('resource_id') == row['resource_id'] and
                        decision.get('disposition') == 'preserve_paused_database_and_configuration' and
                        decision.get('resume_authorized') is False and decision.get('delete_authorized') is False and
                        row.get('transfer_action') == 'Preserve paused database and configuration; no project/database transfer, resume, query or deletion',
                        'Dashboard database action must preserve its approved paused disposition')
            if row.get('resource_kind') == 'vercel_project' and row.get('resource_id') == 'prj_hPjAbxtMlCi3A5waKxQpjATto0ae':
                decision = json.loads((directory/'modern-web-stack-git-retirement-evidence.json').read_text())
                require(decision.get('repository_id') == repo_id and decision.get('project_id') == row['resource_id'] and
                        decision.get('result') == 'disconnected' and decision.get('project_deleted') is False and
                        row.get('transfer_action') == 'Git connection retired before transfer; transfer GitHub repository without reconnecting Vercel',
                        'Retired Vercel project action must preserve its approved disconnected disposition')
            for field in ('resource_owner', 'resource_url', 'billing_dependency', 'credential_source',
                          'affected_reference', 'transfer_action', 'smoke_test', 'recovery_action'):
                require(text(row.get(field)), f'Verified integration needs {field}')
            smoke = local_capture(row.get('smoke_evidence_url'), directory, 'Integration smoke')
            smoke_repository = repo['full_name'] if row.get('smoke_phase') == 'pre_transfer' or repo_id in retained else repo['destination']
            require(row.get('smoke_outcome') in {'success', 'approved_recovery', 'preserved_unused', 'baseline_preserved'} and
                    row.get('smoke_phase') in {'pre_transfer', 'post_transfer'} and
                    smoke.get('repository_id') == repo_id and smoke.get('repository') == smoke_repository and
                    smoke.get('provider') == row['provider'] and smoke.get('resource_kind') == row.get('resource_kind') and
                    smoke.get('resource_id') == row.get('resource_id') and smoke.get('outcome') == row['smoke_outcome'] and
                    smoke.get('phase') == row['smoke_phase'] and smoke.get('destructive_changes') is False,
                    'Integration smoke must match its resource, phase and successful preservation outcome')
            observed_time(smoke.get('observed_at'), 'Integration smoke')
            if row['smoke_outcome'] == 'approved_recovery':
                require(smoke.get('recovery_success') is True and smoke.get('approved_by') == 'HemSoft',
                        'Integration recovery needs owner approval and a successful result')
                evidence(smoke.get('owner_receipt_url'), directory)
            elif row['smoke_outcome'] == 'preserved_unused':
                require(smoke.get('resource_unchanged') is True and smoke.get('runtime_actions') == [],
                        'Unused integration must remain preserved without runtime operations')
                evidence(smoke.get('owner_receipt_url'), directory)
            elif row['smoke_outcome'] == 'baseline_preserved':
                require(isinstance(smoke.get('baseline'), dict) and bool(smoke['baseline']) and
                        smoke.get('observed_resource') == smoke['baseline'],
                        'Baseline preservation needs matching independent resource observations')
            else:
                require(smoke.get('continuity_verified') is True, 'Integration success needs verified continuity')
            evidence(row['resource_url'], directory)
            require(row.get('credential_validity') in {'verified', 'not_required'},
                    'Credential presence alone does not establish validity')
    require(seen_ledger == set(expected), 'Ledger must cover every baseline ID')
    require(seen_runner_resources == runner_resources, 'Ledger must preserve every observed repository runner resource')
    require(seen_vercel_resources == vercel_resources, 'Ledger must preserve every captured Vercel project resource')
    require(all(seen_other_resources[kind] == resources for kind, (_, resources) in other_resources.items()),
            'Ledger must preserve every captured Pages/Supabase/Cloudflare resource')
    all_transfer_gates_verified = all(all(status == 'verified' for status in ledger_statuses[repo_id])
                                      for repo_id in expected if repo_id not in retained)
    if all_transfer_gates_verified:
        validate_app_credential(matrix.get('pre_transfer_credential_verification'), directory)
        source_capture = json.loads((directory / 'source-reference-evidence.json').read_text())
        unresolved = {x['repository_id']: x for x in source_capture['repositories'] if x.get('state') != 'observed'}
        tree_refresh = json.loads((directory / 'source-tree-recheck-evidence.json').read_text())
        resolutions = {x.get('repository_id'): x for x in tree_refresh.get('records', [])}
        for repo_id, original in unresolved.items():
            current = resolutions.get(repo_id, {})
            require(current.get('source') == original['source'] and
                    ((current.get('state') == 'empty_tree' and immutable_sha(current.get('commit_sha')) and
                      current.get('tree_sha') == '4b825dc642cb6eb9a060e54bf8d69288fbee4904') or
                     (current.get('state') == 'uninitialized' and current.get('metadata_size') == 0 and
                      current.get('branches') == [])),
                    'Unverified source trees need a repository-bound independent resolution before transfer')
            evidence(current.get('evidence_url'), directory)

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
        require(not (row['archived'] and health == 'verified'), 'Archived repository needs archive-preserving verification')
        if health in {'verified', 'source_verified', 'archived_verified', 'scope_exception'}:
            validate_protection_preservation(row.get('destination_protections'), directory, repo)
        require((health == 'retained_source') == (repo_id in retained), 'Matrix must honor retained source decisions')
        protected_source = repo['full_name'] == 'HemSoft/set-it-free-loop'
        require((row.get('rollout_action') == 'protected_source_verify_workflows_in_place') == protected_source,
                'Protected source completion mode must match the distribution repository')
        if repo_id not in retained and (health != 'pending_transfer' or text(row.get('transfer_evidence_url'))):
            require(all_transfer_gates_verified,
                    'All transfer-target ledger gates must be verified before any transfer advances')
        if repo_id == FHEMMER_REPOSITORY_ID:
            access = row.get('post_transfer_access')
            require(isinstance(access, dict) and access.get('status') in {'pending', 'verified'},
                    'fhemmer needs an explicit post-transfer access and seat state')
            if health != 'pending_transfer' or text(row.get('transfer_evidence_url')):
                require(access['status'] == 'verified', 'fhemmer transfer needs post-transfer access and seat proof')
            if access['status'] == 'verified':
                require(text(row.get('transfer_evidence_url')) and access.get('repository') == repo['destination'] and
                        access.get('account') == 'fhemmerrelias' and access.get('effective_permission') == 'none' and
                        type(access.get('filled_seats')) is int and access['filled_seats'] == 1 and
                        type(access.get('paid_seats')) is int and access['paid_seats'] == 1,
                        'fhemmer post-transfer receipt must prove no access and the existing one-seat license')
                require(text(access.get('verified_by')), 'Post-transfer access receipt needs its verifier')
                try:
                    captured_at = datetime.datetime.fromisoformat(access.get('verified_at', '').replace('Z', '+00:00'))
                except ValueError as exc:
                    raise ValueError('Post-transfer access receipt needs a verification timestamp') from exc
                require(captured_at.tzinfo is not None, 'Post-transfer access receipt needs a timezone')
                evidence(access.get('permission_evidence_url'), directory)
                evidence(access.get('license_evidence_url'), directory)
        coverage = row.get('destination_sfl_app_access')
        if (health in {'verified', 'source_verified', 'archived_verified', 'scope_exception'} and
                row['source_app_access_in_baseline']):
            require(isinstance(coverage, dict) and coverage.get('status') == 'verified',
                    'Every baseline-covered terminal repository must resolve repository-bound destination App coverage')
        if coverage == 'verified' or (isinstance(coverage, dict) and coverage.get('status') == 'verified'):
            require(app_transfer['status'] == 'verified', 'Destination private SFL App access requires verified App transfer')
            validate_app_coverage(coverage, directory, repo_id, repo['destination'], owned_app_id, inventory['destination_login'])
        if health in {'verified', 'archived_verified', 'scope_exception', 'source_verified'}:
            require(all(status == 'verified' for status in ledger_statuses[repo_id]),
                    'Completed rollout requires every integration row verified')
            for resource_id in (rid for source_id, rid in runner_resources if source_id == repo_id):
                proof = row.get('post_transfer_runner')
                require(isinstance(proof, dict) and proof.get('repository_id') == repo_id and
                        proof.get('repository') == repo['destination'] and str(proof.get('runner_id')) == resource_id and
                        proof.get('online') is True and proof.get('idle') is True and proof.get('isolated') is True and
                        proof.get('service_active') is True and proof.get('run_conclusion') == 'success' and
                        immutable_sha(proof.get('run_head_sha')),
                        'Completed runner transfer needs structured destination continuity and smoke evidence')
                require(re.fullmatch('https://github.com/' + re.escape(repo['destination']) + r'/actions/runs/[1-9][0-9]*',
                                     proof.get('run_url', '')) is not None,
                        'Runner smoke must belong to its destination repository')
                for field in ('registration_evidence_url', 'isolation_evidence_url', 'service_evidence_url', 'run_url'):
                    evidence(proof.get(field), directory)
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
                require(dependency.get('verified_by') == 'HemSoft' and
                        dependency.get('disposition') == RETAINED_APP_DISPOSITION and
                        dependency.get('evidence_url') == RETAINED_APP_RECEIPT,
                        'Verified retained App dependency needs the approved no-dependency disposition and owner receipt')
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
            validate_release_download({'release_download_verification_url': proof.get('release_verification_url')},
                                      directory, repo_id, row['destination'], canonical_source,
                                      proof['source_sha'], proof['release_version'])
            runs = proof.get('workflow_run_urls')
            require(isinstance(runs, list) and bool(runs), 'Protected source needs in-place workflow runs')
            operations = proof.get('workflow_operation_receipts')
            require(isinstance(operations, list) and len(operations) == len(runs),
                    'Protected source needs bound workflow operation receipts')
            for run, operation in zip(runs, operations):
                source_workflows = {'.github/workflows/' + path.name for path in
                                    (pathlib.Path(__file__).resolve().parents[2] / '.github/workflows').glob('*.yml')}
                bound_workflow_operation(operation, directory, repo_id, row['destination'],
                                         proof['source_sha'], proof['release_version'], run, source_workflows, proof['source_sha'])
            for field in ('transfer_evidence_url', 'status_evidence_url'):
                evidence(row.get(field), directory)
            require(row.get('destination_codex_access') == 'verified' and isinstance(coverage, dict) and coverage.get('status') == 'verified',
                    'Protected source needs verified App coverage')
            # The source owns its workflows; it must not acquire a consumer manifest through init/sync.
            require(row['installed_tier'] is None and row['selected_tier'] is None,
                    'Protected source must not be recorded as a deployed consumer')
            validate_registered_review(row, directory, row["destination"], target_branch=repo["default_branch"], deployment_revision=proof["source_sha"])
            verified_rollouts += 1
        elif health == 'scope_exception':
            require(not protected_source, 'Protected source cannot omit in-place verification through an exception')
            decision = row.get('scope_exception_decision')
            require(isinstance(decision, dict) and decision.get('repository_id') == repo_id and
                    decision.get('repository') == row['destination'] and decision.get('approved_by') == 'HemSoft' and
                    decision.get('disposition') == 'exclude_runtime_rollout' and text(decision.get('reason')),
                    'Scope exception needs a repository-bound explicit owner decision')
            try:
                approved_at = datetime.datetime.fromisoformat(decision.get('approved_at', '').replace('Z', '+00:00'))
            except ValueError as exc:
                raise ValueError('Scope exception needs an approval timestamp') from exc
            require(approved_at.tzinfo is not None, 'Scope exception approval needs a timezone')
            require(decision.get('evidence_url') == row.get('exception_evidence_url'),
                    'Scope exception evidence must match its owner decision')
            capture = local_capture(row.get('exception_evidence_url'), directory, 'Scope exception')
            require(all(capture.get(field) == decision.get(field) for field in
                        ('repository_id', 'repository', 'approved_by', 'approved_at', 'reason', 'disposition')),
                    'Scope exception must match its independent owner decision capture')
            receipt = capture.get('owner_comment', {})
            require(receipt.get('author') == 'HemSoft' and receipt.get('created_at') == decision['approved_at'] and
                    re.fullmatch(r'https://github.com/HemSoft/set-it-free-loop/issues/(?:138|139)#issuecomment-[1-9][0-9]*',
                                 receipt.get('url', '')) is not None and receipt.get('decision') == {
                        field: decision[field] for field in ('repository_id', 'repository', 'disposition', 'reason')},
                    'Scope exception needs its captured HemSoft issue decision with matching repository and disposition')
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
            observed = row.get('pre_sync_installation')
            require(isinstance(observed, dict) and observed.get('repository_id') == repo_id and
                    observed.get('repository') == row['destination'] and immutable_sha(observed.get('revision_sha')) and
                    observed.get('manifest_paths') == ['.sfl/sfl.json', 'sfl.json'],
                    'Verified rollout needs repository-bound pre-sync installation evidence')
            evidence(observed.get('evidence_url'), directory)
            require(observed.get('tier') == row['installed_tier'], 'Pre-sync receipt must match the observed tier')
            if row['installed_tier'] == 'not_installed':
                require(observed.get('state') == 'absent' and repo_id not in captured_manifests,
                        'Not-installed requires proven absence and cannot erase a captured installation')
            else:
                require(observed.get('state') == 'present', 'Existing deployment needs a present pre-sync receipt')
                require(string_list(observed.get('addons')) and string_list(observed.get('components')) and
                        observed['addons'] == row['installed_addons'] and
                        observed['components'] == (row['installed_components'] or []),
                        'Existing deployment must preserve its installed tier and addons/components from its pre-sync receipt')
                manifest = captured_manifests.get(repo_id)
                if manifest is not None:
                    require(row['installed_tier'] == manifest.get('tier') and
                            row['installed_addons'] == manifest.get('addons', []) and
                            row['installed_components'] == manifest.get('components', []),
                            'Present installation must preserve its independently captured manifest configuration')
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
            require(string_list(manifest.get('addons')) and
                    set(manifest['addons']) == set(row['selected_addons']),
                    'Verified rollout manifest must match the selected addons')
            if row['selected_tier'] == 'custom':
                require(string_list(manifest.get('components')) and
                        set(manifest['components']) == set(row['selected_components']),
                        'Verified custom manifest must match the selected components')
            require('.github/workflows/sfl-pr-review-auto.yml' in deployed_workflow_paths(
                        row['selected_tier'], row['selected_addons'], row['selected_components']),
                    'Verified consumer configuration must actually deploy the review observer')
            require(row.get('release_url') == 'https://github.com/' + canonical_source + '/releases/tag/v' + row['manifest_version'],
                    'Verified rollout release must match its canonical source and version')
            for field in ('manifest_evidence_url', 'release_url', 'release_download_verification_url'):
                evidence(row.get(field), directory)
            require(row.get('destination_codex_access') == 'verified', 'Codex coverage must be verified')
            validate_app_coverage(coverage, directory, repo_id, repo['destination'], owned_app_id, inventory['destination_login'])
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
            validate_release_download(row, directory, repo_id, row['destination'], canonical_source,
                                      row['deployment_sha'], row['manifest_version'])
            revision = deployed_revision(row, directory, repo_id, row['destination'], manifest)
            operations = row.get('wider_operation_receipts', [])
            require(isinstance(operations, list) and len(operations) == len(runs),
                    'Consumer wider workflows need bound operation receipts')
            for run, operation in zip(runs, operations):
                bound_workflow_operation(operation, directory, repo_id, row['destination'],
                                         row['deployment_sha'], row['manifest_version'], run,
                                         deployed_workflow_paths(row['selected_tier'], row['selected_addons'],
                                                                 row['selected_components']) - {'.github/workflows/sfl-pr-review-auto.yml'}, revision)
            validate_registered_review(row, directory, row['destination'], target_branch=repo['default_branch'], deployment_revision=revision)
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
        require(extra_id in APPROVED_PILOTS and (name, extra.get('visibility')) == APPROVED_PILOTS[extra_id],
                'Disposable pilot ID, repository and visibility must match the designated identities')
        require(extra.get('validation_status') in {'pending', 'failed', 'verified'},
                'Disposable pilot needs an explicit validation status')
        if extra['validation_status'] == 'verified':
            require(app_transfer['status'] == 'verified', 'Verified pilot requires completed owned App transfer')
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
                    manifest.get('version') == receipts['release_version'] and manifest.get('tier') in INIT_TIERS,
                    'Verified pilot manifest must match its deployment source, SHA and version')
            require(string_list(manifest.get('addons')) and set(manifest['addons']) <= supported_addons and
                    '.github/workflows/sfl-pr-review-auto.yml' in deployed_workflow_paths(manifest['tier'], manifest['addons']),
                    'Verified pilot configuration must actually deploy the review observer')
            for field in ('manifest_evidence_url', 'release_download_verification_url'):
                evidence(receipts.get(field), directory)
            validate_release_download(receipts, directory, extra_id, name, canonical_source,
                                      receipts['deployment_sha'], receipts['release_version'])
            revision = deployed_revision(receipts, directory, extra_id, name, manifest)
            validate_app_coverage(receipts.get('destination_sfl_app_access'), directory, extra_id, name,
                                  owned_app_id, inventory['destination_login'])
            operation_fields = ('init_pr_url', 'sync_pr_url', 'repeat_sync_evidence_url',
                                'repeat_onboarding_evidence_url', 'review_registration_url',
                                'review_registry_status_url', 'review_artifact_url', 'gate_run_url',
                                'status_evidence_url', 'gate_uninstall_evidence_url')
            operations = receipts.get('operation_receipts')
            require(isinstance(operations, dict) and set(operations) == set(operation_fields),
                    'Verified pilot needs repository-bound onboarding operation receipts')
            for field in operation_fields:
                operation = operations[field]
                require(isinstance(operation, dict) and operation.get('repository_id') == extra_id and
                        operation.get('repository') == name and operation.get('deployment_sha') == receipts['deployment_sha'] and
                        operation.get('release_version') == receipts['release_version'] and
                        operation.get('evidence_url') == receipts[field],
                        'Pilot operation receipt must bind its repository, deployment and release')
                if urllib.parse.urlsplit(receipts[field]).scheme:
                    require(receipts[field].startswith('https://github.com/' + name + '/'),
                            'Pilot operation URL must belong to its designated repository')
            for field in ('init_pr_url', 'sync_pr_url'):
                require(re.fullmatch('https://github.com/' + re.escape(name) + r'/pull/[1-9][0-9]*', receipts[field]) is not None,
                        'Pilot onboarding PR must belong to its designated repository')
            onboarding = ('init_pr_url', 'repeat_onboarding_evidence_url', 'sync_pr_url',
                          'repeat_sync_evidence_url', 'status_evidence_url', 'gate_uninstall_evidence_url')
            for field in onboarding:
                operation = operations[field]
                command = ('init' if field in {'init_pr_url', 'repeat_onboarding_evidence_url'} else
                           'sync' if field in {'sync_pr_url', 'repeat_sync_evidence_url'} else
                           'status' if field == 'status_evidence_url' else 'uninstall-gate')
                outcome = 'pull_request_merged' if field in {'init_pr_url', 'sync_pr_url'} else 'healthy' if command == 'status' else 'gate_removed' if command == 'uninstall-gate' else 'no_changes'
                require(operation.get('command') == command and operation.get('outcome') == outcome and
                        immutable_sha(operation.get('revision_before')) and immutable_sha(operation.get('revision_after')),
                        'Pilot onboarding operations need successful command-specific terminal outcomes')
                if outcome == 'pull_request_merged':
                    require(operation.get('merged') is True, 'Pilot init and sync must prove merged deployment PRs')
                elif outcome == 'no_changes':
                    require(operation.get('change_count') == 0 and operation['revision_before'] == operation['revision_after'],
                            'Pilot repeated init and sync must prove zero changes at the same revision')
                elif outcome == 'gate_removed':
                    require(operation.get('gate_only') is True and operation.get('unrelated_change_count') == 0,
                            'Pilot gate uninstall must prove safe gate-only removal')
            for previous, following in zip(onboarding, onboarding[1:]):
                require(operations[previous]['revision_after'] == operations[following]['revision_before'],
                        'Pilot onboarding operations must follow their ordered revision chain')
            require(operations['status_evidence_url']['revision_after'] == revision,
                    'Pilot healthy status must match its captured deployed revision')
            scenarios = receipts.get('scenario_receipts')
            require(isinstance(scenarios, dict) and set(scenarios) == set(PILOT_SCENARIOS),
                    'Verified pilot needs every mandatory negative scenario receipt')
            for scenario, outcome in PILOT_SCENARIOS.items():
                result = scenarios[scenario]
                require(isinstance(result, dict) and result.get('outcome') == outcome and
                        result.get('mode') in {'live', 'workflow_fixture'} and
                        result.get('deployment_sha') == receipts['deployment_sha'] and
                        result.get('repository_id') == extra_id and result.get('repository') == name and
                        result.get('release_version') == receipts['release_version'],
                        'Pilot scenario outcome must match the tested deployment and declared execution mode')
                evidence(result.get('evidence_url'), directory)
                if urllib.parse.urlsplit(result['evidence_url']).scheme:
                    require(result['evidence_url'].startswith('https://github.com/' + name + '/'),
                            'Pilot scenario URL must belong to its designated repository')
            identity = receipts.get('review_artifact_identity')
            require(isinstance(identity, dict) and identity.get('runtime') == 'sfl_registered_codex' and
                    identity.get('app_id') == 1144995 and identity.get('bot_user_id') == 199175422 and
                    all(isinstance(identity.get(field), str) and
                        re.fullmatch(r'[0-9a-f]{40}', identity[field])
                        for field in ('reviewed_head_sha', 'reviewed_base_sha')),
                    'Verified pilot needs immutable SFL registered Codex review identity')
            validate_registered_review(receipts, directory, name, extra_id, target_branch="main", deployment_revision=revision)
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
                require(manifest['tier'] in {'standard', 'full'},
                        'Wider pilot must declare a wider deployed configuration')
                for run in runs:
                    evidence(run, directory)
                evidence(receipts.get('auditor_run_url'), directory)
                wider_receipts = receipts.get('wider_operation_receipts')
                require(isinstance(wider_receipts, list) and len(wider_receipts) == len(runs),
                        'Wider pilot needs bound workflow operation receipts')
                for run, operation in zip(runs, wider_receipts):
                    bound_workflow_operation(operation, directory, extra_id, name, receipts['deployment_sha'],
                                             receipts['release_version'], run,
                                             deployed_workflow_paths(manifest['tier'], manifest['addons']) -
                                             {'.github/workflows/sfl-pr-review-auto.yml'}, revision)
                bound_workflow_operation(receipts.get('auditor_operation_receipt'), directory, extra_id, name,
                                         receipts['deployment_sha'], receipts['release_version'], receipts['auditor_run_url'],
                                         {'.github/workflows/sfl-auditor.yml'}, revision)
                require(any(operation['workflow'] != '.github/workflows/sfl-auditor.yml' and
                            run != receipts['auditor_run_url'] for run,operation in zip(runs,wider_receipts)),
                        'Wider pilot must execute a distinct successful non-Auditor workflow')
                verified_wider_pilot = True
            verified_pilot_visibilities.add(extra['visibility'])
        extra_ids.add(extra_id)
        extra_names.add(name.casefold())
    require(extra_ids == set(APPROVED_PILOTS), 'Both designated disposable pilot identities must remain recorded')
    source_complete = all(row['health'] in {'verified', 'archived_verified', 'scope_exception', 'retained_source', 'source_verified'} for row in records)
    if verified_rollouts or source_complete:
        require(all(row['retained_app_dependency']['status'] == 'verified' for row in records
                    if row['health'] == 'retained_source'),
                'Completed rollout requires verified retained App dependencies')
        require(verified_pilot_visibilities == {'public', 'private'},
                'Active rollout requires verified public and private onboarding pilots first')
        require(verified_wider_pilot, 'Active rollout requires a verified wider-workflow and auditor pilot')
    if source_complete:
        require(all(row.get('smoke_phase') == 'post_transfer' for row in rows
                    if row.get('provider') != 'none' and row.get('status') == 'verified'),
                'Final completion needs post-transfer resource continuity, beyond pre-transfer readiness')
        require(all(status == 'verified' for statuses in ledger_statuses.values() for status in statuses),
                'Final completion needs every retained and transferred resource verified')
        for resource in account_resources:
            require(resource['status'] == 'verified', 'Final completion needs verified unlinked Supabase preservation')
            reference = resource.get('post_transfer_evidence_url')
            evidence(reference, directory)
            require(not urllib.parse.urlsplit(reference).scheme and reference != 'supabase-provider-evidence.json',
                    'Unlinked resource needs an independent post-transfer account capture')
            capture = json.loads((directory / reference).read_text())
            require(capture.get('resource_id') == resource['resource_id'] and capture.get('resource_owner') == resource['resource_owner'] and
                    capture.get('state') == unlinked_supabase[resource['resource_id']]['state'] and
                    capture.get('resource_changes_made') is False and capture.get('operation') == 'read_only_preservation',
                    'Unlinked Supabase post-transfer capture must preserve the exact account resource and paused state')
        onboarding = validate_final_onboarding(matrix.get('post_rollout_onboarding'), directory, expected, inventory['destination_login'], owned_app_id)
        proof = matrix.get('final_inventory')
        validate_final_inventory(proof, directory, expected, retained, onboarding)
        require(datetime.datetime.fromisoformat(proof['observed_at'].replace('Z', '+00:00')) >
                datetime.datetime.fromisoformat(inventory['captured_at'].replace('Z', '+00:00')),
                'Final inventory must be fresher than the sealed source baseline')
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
