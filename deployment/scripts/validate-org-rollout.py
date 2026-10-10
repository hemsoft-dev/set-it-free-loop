#!/usr/bin/env python3
"""Validate migration records and independently verify completed release downloads."""

import argparse
import base64
import contextlib
import copy
import csv
import datetime
import hashlib
import gzip
import functools
import io
import json
import ntpath
import os
import pathlib
import re
import subprocess
import tempfile
import urllib.parse
import zipfile

# Immutable source deployment paths at baseline 7a15964. These historical
# operation receipts must not depend on current CLI tiers or installed files.
HISTORICAL_SOURCE_WORKFLOWS = frozenset('.github/workflows/' + name for name in (
    'daily-repo-status.lock.yml', 'issue-processor.lock.yml',
    'pr-analyzer-general.lock.yml', 'pr-analyzer-quality.lock.yml',
    'pr-analyzer-security.lock.yml', 'pr-analyzer-testing.lock.yml',
    'pr-fixer.lock.yml', 'pr-promoter.lock.yml', 'repo-audit.lock.yml',
    'sfl-auditor.lock.yml', 'sfl-dispatcher.yml', 'sfl-pr-review-auto.yml',
    'simplisticate.lock.yml',
))

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
                   'duplicate_delivery':'idempotent', 'new_head':'stale_gate_rejected', 'base_advance':'stale_gate_rejected',
                   'overlapping_request':'gate_blocked', 'unregistered_base_context':'stale_gate_rejected'}
# Franz's recorded October 6 decision. Expanding this set requires a new owner decision.
APPROVED_RETAINED_IDS = {1162179521, 1169698740}
RETENTION_RECEIPT = 'https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6028207635'
HEALTH = {'pending_transfer', 'pending_rollout', 'failed', 'verified', 'archived_verified', 'scope_exception', 'retained_source', 'source_verified'}
LEDGER_STATUS = {'owner_verification_pending', 'partial_provider_verified', 'verified'}
SFL_APP_PERMISSIONS = {'actions':'write', 'checks':'write', 'contents':'read',
                       'issues':'write', 'metadata':'read', 'pull_requests':'write'}
GHX_HISTORY_CATALOG_SHA256 = 'a95712b0121ab3c4d0b4b985727eff0dd82eb0ca4081218733ac39bb388c8c78'
SURVIVAL_REPOSITORY_ID = 1399232444
SURVIVAL_RESOURCES_SHA256 = 'a53f01ba88db330c6e370a0c959c1c993bebe3b37e1abae68b7fa987c01b880a'


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


def terminal_run_time(run, observed_at, label):
    created = observed_time(run.get('created_at'), label + ' creation')
    completed = observed_time(run.get('updated_at'), label + ' completion')
    require(created <= completed <= observed_at,
            label + ' capture must follow actual run completion')
    return completed


def validate_actions_run_response(capture, run, repository, observed_at, label):
    require(type(run.get('id')) is int and run['id'] > 0,
            label + ' needs its actual Actions run ID')
    response = capture.get('run_response', {})
    validate_resource_response(response, 'https://api.github.com/repos/' + repository +
                               '/actions/runs/' + str(run['id']), run)
    completed = terminal_run_time(run, observed_at, label)
    require(completed <= observed_time(response.get('observed_at'), label + ' run GET') <= observed_at,
            label + ' primary run GET must follow completion and precede its capture')
    return completed


def validate_run_artifact(reference, directory, repository, run, artifact_name, filename, payload, observed_at):
    capture = local_capture(reference, directory, 'Actions output artifact')
    artifact = capture.get('artifact', {})
    artifact_id = artifact.get('id')
    require(type(artifact_id) is int and artifact_id > 0 and type(run.get('id')) is int and run['id'] > 0 and
            artifact.get('name') == artifact_name and artifact.get('expired') is False and
            artifact.get('workflow_run', {}).get('id') == run['id'] and
            artifact['workflow_run'].get('head_sha') == run['head_sha'] and
            capture.get('request_url') == 'https://api.github.com/repos/' + repository + '/actions/artifacts/' + str(artifact_id) and
            artifact.get('archive_download_url') == capture.get('download_url') ==
                'https://api.github.com/repos/' + repository + '/actions/artifacts/' + str(artifact_id) + '/zip',
            'Actions output artifact must belong to the exact repository run and named output')
    validate_resource_response(capture.get('artifact_response', {}), capture['request_url'], artifact)
    require(observed_time(artifact.get('updated_at'), 'Artifact completion') <=
            observed_time(capture['artifact_response'].get('observed_at'), 'Artifact metadata GET') <=
            observed_time(capture.get('observed_at'), 'Artifact capture'),
            'Artifact metadata GET must follow completion and precede the archive capture')
    require(observed_time(run['created_at'], 'Artifact workflow creation') <=
            observed_time(run.get('run_started_at'), 'Artifact run attempt start') <=
            observed_time(payload.get('observed_at'), 'Artifact output creation') <=
            observed_time(artifact.get('created_at'), 'Artifact creation') <=
            observed_time(artifact.get('updated_at'), 'Artifact completion') <=
            observed_time(run['updated_at'], 'Artifact workflow completion') and
            observed_time(run['updated_at'], 'Artifact workflow completion') <=
            observed_time(capture.get('observed_at'), 'Artifact capture') <= observed_at,
            'Actions output artifact capture must follow its completed run')
    try:
        archive = base64.b64decode(capture.get('archive_base64', ''), validate=True)
        require(artifact.get('digest') == 'sha256:' + hashlib.sha256(archive).hexdigest(),
                'Actions output archive must match its GitHub digest')
        with zipfile.ZipFile(io.BytesIO(archive)) as files:
            require(files.namelist() == [filename], 'Actions output artifact must contain only its named public output')
            require(json.loads(files.read(filename).decode('utf-8-sig')) == payload,
                    'Actions output must equal the downloaded immutable artifact contents')
    except (ValueError, zipfile.BadZipFile, KeyError) as exc:
        raise ValueError('Actions artifact must contain the independently downloaded output') from exc


def deployed_revision(proof, directory, repository_id, repository, manifest):
    capture = local_capture(proof.get('manifest_evidence_url'), directory, 'Deployed revision')
    require(capture.get('repository_id') == repository_id and capture.get('repository') == repository and
            immutable_sha(capture.get('revision_sha')) and capture.get('manifest') == manifest,
            'Deployed revision capture must match its repository and installed manifest')
    observed_time(capture.get('observed_at'), 'Deployed revision')
    validate_manifest_contents(capture, repository_id, repository, manifest)
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


def validate_terminal_operation(operation, directory, cutover=None):
    capture = local_capture(operation.get('capture_evidence_url'), directory, 'Terminal CLI operation')
    fields = ('repository_id', 'repository', 'deployment_sha', 'release_version', 'evidence_url',
              'command', 'outcome', 'revision_before', 'revision_after')
    require(all(capture.get(field) == operation.get(field) for field in fields) and
            capture.get('status') == 'completed' and type(capture.get('exit_code')) is int and
            capture['exit_code'] == 0, 'Terminal operation capture must bind successful CLI execution and revisions')
    timestamp = observed_time(capture.get('observed_at'), 'Terminal CLI operation')
    result = capture.get('result', {})
    if operation['outcome'] == 'pull_request_merged':
        pr = result.get('pull_request', {})
        require(pr.get('html_url') == operation['evidence_url'] and pr.get('merged') is True and
                pr.get('state') == 'closed' and pr.get('merge_commit_sha') == operation['revision_after'] and
                pr.get('base', {}).get('repo', {}).get('id') == operation['repository_id'] and
                pr['base']['repo'].get('full_name') == operation['repository'],
                'Terminal mutation needs an independently captured merged repository PR and resulting revision')
        number = operation['evidence_url'].rsplit('/', 1)[-1]
        require(number.isdigit() and int(number) > 0 and pr.get('number') == int(number),
                'Terminal mutation needs its actual repository PR number')
        response = capture.get('pull_request_response', {})
        validate_resource_response(response, 'https://api.github.com/repos/' + operation['repository'] +
                                   '/pulls/' + number, pr)
        created = observed_time(pr.get('created_at'), 'Deployment PR creation')
        completed = observed_time(pr.get('merged_at'), 'Deployment PR merge')
        require(created <= completed <= observed_time(response.get('observed_at'), 'Deployment PR GET') <= timestamp and
                (cutover is None or created >= cutover),
                'CLI terminal capture must follow its actual PR merge after cutover')
        return completed
    require(operation['outcome'] in {'no_changes', 'healthy', 'gate_removed'},
            'Unsupported terminal CLI operation outcome')
    execution = capture.get('execution', {})
    started = observed_time(execution.get('started_at'), 'Terminal execution start')
    completed = observed_time(execution.get('completed_at'), 'Terminal execution completion')
    require(started == observed_time(capture.get('started_at'), 'Terminal capture start') and
            started <= completed <= timestamp and completed - started <= datetime.timedelta(minutes=15) and
            (cutover is None or started >= cutover),
            'Terminal execution must finish within its bounded capture after cutover')
    require(text(execution.get('command')) and isinstance(execution.get('argv'), list) and
            execution['argv'] and all(text(arg) for arg in execution['argv']) and
            type(execution.get('exit_code')) is int and execution['exit_code'] == 0,
            'Terminal execution needs its actual successful command and arguments')
    if operation['outcome'] == 'no_changes':
        execution = capture.get('execution', {})
        argv = execution.get('argv')
        require(operation.get('command') in {'init','sync'} and isinstance(argv, list) and string_list(argv) and
                argv[:3] == ['gh','sfl',operation['command']] and argv.count('--repo') == 1 and
                argv.index('--repo') + 1 < len(argv) and argv[argv.index('--repo') + 1] == operation['repository'] and
                '--pr' in argv and execution.get('command') == 'gh sfl ' + operation['command'] and
                type(execution.get('exit_code')) is int and execution['exit_code'] == 0,
                'No-op needs its successful concrete CLI command and target arguments')
        stdout = execution.get('stdout')
        require(isinstance(stdout, str) and
                execution.get('stdout_sha256') == hashlib.sha256(stdout.encode()).hexdigest() and
                any(line.strip() == 'SFL ' + operation['command'] + ' is already up to date; no pull request needed'
                    for line in stdout.splitlines()), 'No-op result must derive from the actual successful CLI output')
        require(type(result.get('change_count')) is int and result['change_count'] == 0 and
                operation['revision_before'] == operation['revision_after'] and
                result.get('revision_before') == operation['revision_before'] and
                result.get('revision_after') == operation['revision_after'],
                'No-op needs captured zero changes at the observed unchanged revision')
    elif operation['outcome'] == 'healthy':
        argv = execution['argv']
        require(execution['command'] == 'gh sfl status' and argv[:3] == ['gh', 'sfl', 'status'] and
                argv.count('--repo') == 1 and argv.index('--repo') + 1 < len(argv) and
                argv[argv.index('--repo') + 1] == operation['repository'],
                'Status must execute its concrete CLI command against its designated repository')
        require(operation['command'] == 'status' and operation['revision_before'] == operation['revision_after'] and
                result.get('health') == 'healthy' and result.get('revision_sha') == operation['revision_after'] and
                result.get('missing_files') == [] and result.get('drifted_files') == [],
                'Status needs captured healthy deployment checks at its revision')
    elif operation['outcome'] == 'gate_removed':
        require(operation['command'] == 'uninstall-gate' and result.get('gate_required') is False and result.get('unrelated_change_count') == 0 and
                result.get('preserved_unrelated_rules') is True,
                'Gate uninstall needs captured absence and preservation of unrelated policy')
    return completed


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


def bound_workflow_operation(operation, directory, repository_id, repository, sha, version, url, workflows, run_head, cutover=None):
    bound_operation(operation, directory, repository_id, repository, sha, version, url)
    require(operation.get('conclusion') == 'success' and operation.get('workflow') in workflows and
            immutable_sha(run_head) and operation.get('run_head_sha') == run_head,
            'Workflow operation must identify a successful expected deployed workflow and immutable run head')
    require(re.fullmatch('https://github.com/' + re.escape(repository) + r'/actions/runs/[1-9][0-9]*', url) is not None,
            'Workflow operation must reference its repository Actions run')
    capture = local_capture(operation.get('capture_evidence_url'), directory, 'Workflow execution')
    run = capture.get('run', {})
    require(run.get('repository',{}).get('id') == repository_id and run['repository'].get('full_name') == repository and
            run.get('html_url') == url and run.get('head_sha') == run_head and
            run.get('path') == operation['workflow'] and run.get('status') == 'completed' and
            run.get('conclusion') == 'success', 'Workflow execution capture must prove the successful expected Actions run')
    timestamp = observed_time(capture.get('observed_at'), 'Workflow execution')
    created = observed_time(run.get('created_at'), 'Workflow creation')
    terminal_run_time(run, timestamp, 'Workflow execution')
    run_id = int(url.rsplit('/', 1)[1])
    require(run.get('id') == run_id, 'Workflow execution needs its actual run ID')
    validate_actions_run_response(capture, run, repository, timestamp, 'Workflow execution')
    if cutover is not None:
        require(created >= cutover, 'Workflow must execute after its independent destination/App cutover capture')



def validate_branch_policy_responses(capture, repository, target_branch, earliest=None):
    observed_at = observed_time(capture.get('observed_at'), 'Effective branch policy')
    rules = capture.get('effective_rules', {})
    classic = capture.get('classic_protection', {})
    base = 'https://api.github.com/repos/' + repository
    branch = urllib.parse.quote(target_branch, safe='')
    require(rules.get('state') == 'observed' and isinstance(rules.get('data'), list) and
            rules.get('method') == 'GET' and rules.get('http_status') == 200 and
            rules.get('request_url') == base + '/rules/branches/' + branch,
            'Branch policy needs its exact successful effective-rules GET')
    require(classic.get('method') == 'GET' and
            classic.get('request_url') == base + '/branches/' + branch + '/protection' and
            ((classic.get('state') == 'observed' and classic.get('http_status') == 200 and isinstance(classic.get('data'), dict)) or
             (classic.get('state') == 'absent' and classic.get('http_status') == 404 and
              classic.get('data', {}).get('message') == 'Branch not protected')),
            'Branch policy needs its exact protection GET or explicit unprotected-branch404')
    for response in (rules, classic):
        at = observed_time(response.get('observed_at'), 'Effective policy response')
        require(at <= observed_at and (earliest is None or at >= earliest),
                'Effective policy primary responses are outside the terminal freshness boundary')
    return rules, classic


def validate_gate_policy(policy, directory, repository_id, repository, target_branch, earliest=None):
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
    if earliest is not None:
        require(observed_at >= earliest, 'Effective gate policy must follow cutover and the completed review run')
    rules, classic = validate_branch_policy_responses(capture, repository, target_branch, earliest)
    required = any(rule.get('type') == 'required_status_checks' and
                   rule.get('parameters', {}).get('strict_required_status_checks_policy') is True and
                   any(check.get('context') == policy['context'] and check.get('integration_id') == 15368
                       for check in rule.get('parameters', {}).get('required_status_checks', []))
                   for rule in rules['data'])
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
    run_capture = local_capture(proof.get('workflow_run_evidence_url'), directory, 'App credential workflow run')
    run = {key: value for key, value in run_capture.items() if key not in {'captured_at', 'run_response'}}
    require(run.get('repository', {}).get('id') == proof['repository_id'] and
            run.get('repository', {}).get('full_name') == proof['repository'] and
            run.get('html_url') == proof['run_url'] and run.get('head_sha') == proof['reviewed_sha'] and
            run.get('path') == proof['workflow'] and run.get('head_branch') == 'main' and run.get('event') == 'workflow_dispatch' and
            run.get('status') == 'completed' and run.get('conclusion') == 'success',
            'App credential capture must come from its successful reviewed main workflow run')
    captured_at = observed_time(run_capture.get('captured_at'), 'App credential run capture')
    validate_actions_run_response(run_capture, run, proof['repository'], captured_at, 'App credential workflow')
    implementations = proof.get('implementation_evidence')
    paths = (proof['workflow'], 'deployment/scripts/SflGitHubAppBootstrap.psm1')
    require(isinstance(implementations, dict) and set(implementations) == set(paths),
            'App credential proof needs both reviewed canonical implementations')
    for path in paths:
        capture = local_capture(implementations[path], directory, 'App credential implementation')
        actual = immutable_contents(capture, proof['repository_id'], proof['repository'], proof['reviewed_sha'], path)
        source_root = pathlib.Path(__file__).resolve().parents[2]
        # The source is frozen; this exact historical workflow is an inactive sample.
        implementation_path = (source_root / 'deployment/tests/fixtures/retired-source-workflows/verify-sfl-app-credential.yml'
                               if path == '.github/workflows/verify-sfl-app-credential.yml' else source_root / path)
        require(actual == implementation_path.read_bytes(),
                'App credential implementation must equal the reviewed canonical source bytes')
        require(observed_time(capture.get('observed_at'), 'App credential implementation capture') <=
                captured_at,
                'App credential implementation must be captured with its reviewed run')
    validate_run_artifact(proof.get('credential_artifact_evidence_url'), directory, proof['repository'], run,
                          'sfl-app-credential-metadata', 'sfl-app-credential-metadata.json', metadata,
                          captured_at)


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


def original_protection_contract(repo, directory=None):
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


def reviewed_survival_resources(repo, directory):
    """An additive current-resource contract; the original inventory seal remains intact."""
    path = directory / 'current-survival-resources.json'
    require(path.is_file() and hashlib.sha256(path.read_bytes()).hexdigest() == SURVIVAL_RESOURCES_SHA256,
            'Current Windows resources must match their reviewed preparation capture')
    capture = json.loads(path.read_bytes())
    require(repo['id'] == SURVIVAL_REPOSITORY_ID and repo['full_name'] == 'HemSoft/survival-shelter-opus55' and
            capture.get('repository_id') == repo['id'] and capture.get('repository') == repo['full_name'] and
            capture.get('phase') == 'preparation' and capture.get('transfer_gate_cleared') is False and
            capture.get('baseline_inventory_sha256') == hashlib.sha256((directory / 'inventory.json').read_bytes()).hexdigest() and
            capture.get('baseline_integrity_sha256') == hashlib.sha256((directory / 'evidence-integrity.json').read_bytes()).hexdigest(),
            'Current Windows resources must preserve the sealed source identity and inventory')
    derived_at = observed_time(capture.get('derived_at'), 'Current resource preparation')
    policy = capture.get('policy_capture', {})
    policy_at = observed_time(policy.get('observed_at'), 'Current Windows policy')
    require(policy.get('repository_id') == repo['id'] and policy.get('repository') == repo['full_name'] and
            immutable_sha(policy.get('revision_sha')) and policy_at <= derived_at,
            'Current Windows policy needs its actual source revision and primary capture')
    contract = validate_protection_responses(policy, repo, repo['full_name'], policy['revision_sha'], policy_at)
    original = original_protection_contract(repo, directory)
    require(all(rule in contract['rulesets'] for rule in original['rulesets']) and
            all(contract['classic'].get(name) == value for name, value in original['classic'].items()),
            'Current Windows reconciliation must preserve every original protection')
    runners = validate_raw_page_chain(capture.get('runner_pages'),
        'https://api.github.com/repos/' + repo['full_name'] + '/actions/runners?per_page=100',
        derived_at, 'Reviewed Windows runner inventory', field='runners')
    require(len(runners) == 1 and runners[0].get('id') == 10 and runners[0].get('name') == 'DESKTOP-7ES73Q4' and
            runners[0].get('os') == 'Windows' and
            {label.get('name') for label in runners[0].get('labels', [])} == {'self-hosted', 'Windows', 'X64', 'ue5.7'},
            'Current Windows reconciliation needs its exact runner identity and labels')
    variable = capture.get('variable_response', {})
    validate_resource_response(variable, 'https://api.github.com/repos/' + repo['full_name'] +
                               '/actions/variables/UE_RUNNER_ENABLED', variable.get('data'))
    require(variable['data'].get('name') == 'UE_RUNNER_ENABLED' and variable['data'].get('value') == 'true' and
            observed_time(variable.get('observed_at'), 'Reviewed runner variable') <= derived_at,
            'Current Windows reconciliation must retain its enabled CI variable')
    workflow = capture.get('workflow_capture', {})
    workflow_bytes = immutable_contents(workflow, repo['id'], repo['full_name'], policy['revision_sha'], '.github/workflows/ci.yml')
    require(observed_time(workflow.get('observed_at'), 'Reviewed Windows workflow') <= derived_at,
            'Windows workflow must retain its immutable source capture')
    validate_windows_startup(capture['host_capture'], capture['startup_capture'], repo, runners[0], derived_at)
    return {'capture':capture, 'policy':contract, 'runner':runners[0], 'workflow_bytes':workflow_bytes,
            'observed_at':policy_at}


def protection_contract(repo, directory=None):
    if directory is None:
        directory = pathlib.Path(__file__).resolve().parents[2] / 'docs/organization-migration'
    if repo['id'] == SURVIVAL_REPOSITORY_ID:
        return reviewed_survival_resources(repo, directory)['policy']
    return original_protection_contract(repo, directory)


def validate_windows_startup(host_capture, task_capture, repo, runner, latest, earliest=None):
    for capture in (host_capture, task_capture):
        at = observed_time(capture.get('observed_at'), 'Windows startup capture')
        require(capture.get('host_alias') == 'laptop' and at <= latest and (earliest is None or at >= earliest),
                'Windows startup must be captured on its existing host after cutover')
    host, task = host_capture.get('data', {}), task_capture.get('data', {})
    require(host.get('computer') == task.get('computer') == runner['name'] and host.get('services') == [],
            'Windows continuity must preserve the observed foreground startup mechanism')
    configurations = host.get('configurations', [])
    require(len(configurations) == 1 and configurations[0].get('agentId') == runner['id'] and
            configurations[0].get('agentName') == runner['name'] and configurations[0].get('workFolder') == '_work' and
            configurations[0].get('gitHubUrl') in {'https://github.com/' + repo['full_name'],
                                                   'https://github.com/' + repo['destination']},
            'Windows host registration must preserve its repository-bound agent')
    root = ntpath.normcase(ntpath.normpath(configurations[0].get('directory', '')))
    expected_root = ntpath.normcase(r'C:\Users\User\actions-runner\survival-shelter')
    require(root == expected_root, 'Windows runner must preserve its existing work directory')
    processes = host.get('processes', [])
    require(len(processes) == 1 and processes[0].get('Name') == 'Runner.Listener.exe' and
            type(processes[0].get('ProcessId')) is int and processes[0]['ProcessId'] > 0 and
            ntpath.normcase(ntpath.normpath(processes[0].get('ExecutablePath', ''))) ==
                ntpath.join(root, 'bin', 'runner.listener.exe'),
            'Windows startup must observe the existing runner listener')
    actions, triggers = task.get('actions', []), task.get('triggers', [])
    require(task.get('name') == 'GitHub Actions runner - HemSoft-survival-shelter-opus55' and
            task.get('path') in {'\\', '\\\\'} and task.get('enabled') is True and task.get('state') == 'Running' and
            task.get('principal') == {'user':'User', 'logonType':'Interactive', 'runLevel':'Limited'} and
            len(actions) == 1 and actions[0].get('Arguments') in {None, ''} and
            ntpath.normcase(ntpath.normpath(actions[0].get('Execute', ''))) == ntpath.join(root, 'run.cmd') and
            ntpath.normcase(ntpath.normpath(actions[0].get('WorkingDirectory', ''))) == root and
            len(triggers) == 1 and triggers[0].get('Enabled') is True and
            triggers[0].get('CimClass', {}).get('CimSystemProperties', {}).get('ClassName') == 'MSFT_TaskLogonTrigger',
            'Windows continuity must preserve the exact enabled logon task, action and limited principal')


def validate_source_browser_protection(capture, repo, repository, timestamp, earliest, directory):
    """Keep the Free-plan API refusal visible and verify the source owner DOM separately."""
    require(repo['id'] == FHEMMER_REPOSITORY_ID and repository == 'fhemmer/hs-cli-confluence-search' and
            repository == repo['full_name'] and capture.get('phase') in {'pre_cutover','pre_transfer'} and earliest is not None,
            'Owner-browser protection fallback is limited to the inaccessible fhemmer source')
    pages = capture.get('ruleset_pages')
    require(isinstance(pages, list) and len(pages) == 1, 'Source plan refusal needs its exact primary GET')
    response = pages[0]
    require(response.get('method') == 'GET' and response.get('http_status') == 403 and
            response.get('request_url') == 'https://api.github.com/repos/' + repository +
                '/rulesets?includes_parents=true&per_page=100' and
            response.get('data', {}).get('message') ==
                'Upgrade to GitHub Pro or make this repository public to enable this feature.' and
            earliest <= observed_time(response.get('observed_at'), 'Source plan refusal') <= timestamp and
            capture.get('ruleset_responses') == {},
            'Only the observed plan refusal permits independent source owner verification')
    browser = local_capture(capture.get('owner_browser_evidence_url'), directory, 'Fresh source owner protection')
    require(browser.get('phase') == capture['phase'], 'Source owner DOM needs its current cutover phase')
    for page, suffix, phrase in [('classic', 'branches', 'Classic branch protections have not been configured'),
                                 ('rulesets', 'rules', "You haven't created any rulesets")]:
        result = browser.get(page, {})
        value = result.get('value', {})
        url = 'https://github.com/' + repository + '/settings/' + suffix
        require(result.get('toolIcon', {}).get('pageUrl') == url and value.get('url') == url and
                value.get('login') == 'HemSoft' and
                'Settings: ' + repository in value.get('visible_text', '') and phrase in value.get('visible_text', '') and
                earliest <= observed_time(value.get('observed_at'), 'Fresh source owner DOM') <= timestamp,
                'Source owner DOM must independently prove current authenticated repository protection absence')
        if page == 'classic':
            require(value.get('repository') == repository and value.get('repository_id') == str(repo['id']),
                    'Classic owner DOM must bind the immutable repository identity')


def validate_protection_responses(capture, repo, repository, revision, timestamp, earliest=None, strict_after=False, directory=None):
    base = 'https://api.github.com/repos/' + repository
    metadata = capture.get('repository_response', {})
    validate_resource_response(metadata, base, metadata.get('data'))
    require(all(metadata['data'].get(field) == value for field, value in
        (('id', repo['id']), ('full_name', repository), ('default_branch', repo['default_branch']),
         ('private', repo['private']), ('visibility', repo['visibility']), ('archived', repo['archived']))),
        'Destination protection metadata must derive from its actual preserved repository identity')
    ref = capture.get('revision_response', {})
    ref_url = base + '/git/ref/heads/' + urllib.parse.quote(repo['default_branch'], safe='')
    if revision is None:
        require(repo['id'] == 996911586 and ref.get('method') == 'GET' and ref.get('request_url') == ref_url and
                ref.get('http_status') in {404,409} and ref.get('data',{}).get('message') in {'Not Found','Git Repository is empty.'},
                'Only the sealed uninitialized source may have an unavailable protection revision')
    else:
        validate_resource_response(ref, ref_url, ref.get('data'))
        require(ref['data'].get('ref') == 'refs/heads/' + repo['default_branch'] and
                ref['data'].get('object', {}).get('sha') == revision,
                'Protection responses need their current default branch revision')
    blocked_source = repository == 'fhemmer/hs-cli-confluence-search' and any(
        page.get('http_status') == 403 for page in capture.get('ruleset_pages', []))
    if blocked_source:
        require(directory is not None, 'Source owner DOM needs its migration evidence directory')
        validate_source_browser_protection(capture, repo, repository, timestamp, earliest, directory)
        rules = []
    else:
        rules = validate_raw_page_chain(capture.get('ruleset_pages'), base + '/rulesets?includes_parents=true&per_page=100',
            timestamp, 'Destination rulesets', earliest=earliest)
    details = capture.get('ruleset_responses', {})
    ids = [rule.get('id') for rule in rules]
    require(all(type(rule_id) is int and rule_id > 0 for rule_id in ids) and len(ids) == len(set(ids)) and
            isinstance(details, dict) and set(details) == {str(rule_id) for rule_id in ids},
            'Destination rulesets need complete unique detail responses')
    actual_rules = []
    for rule_id in ids:
        response = details[str(rule_id)];value = response.get('data')
        validate_resource_response(response, base + '/rulesets/' + str(rule_id), value)
        require(isinstance(value, dict) and value.get('id') == rule_id, 'Destination ruleset detail identity mismatch')
        actual_rules.append({key: value.get(key) for key in
            ('id', 'name', 'target', 'enforcement', 'conditions', 'rules', 'bypass_actors')})
    branches = validate_raw_page_chain(capture.get('protected_branch_pages'), base + '/branches?protected=true&per_page=100',
        timestamp, 'Destination protected branches', earliest=earliest)
    require(not blocked_source or (branches == [] and capture.get('classic_responses') == {}),
            'Source owner protection absence must agree with the complete protected branch GETs')
    names = [branch.get('name') for branch in branches]
    classic = capture.get('classic_responses', {})
    require(string_list(names) and len(names) == len(set(names)) and isinstance(classic, dict) and set(classic) == set(names),
            'Destination classic protection needs every protected branch response')
    actual_classic = {}
    for name in names:
        response = classic[name];value = response.get('data')
        require(response.get('method') == 'GET' and response.get('request_url') ==
            base + '/branches/' + urllib.parse.quote(name, safe='') + '/protection' and
            ((response.get('http_status') == 200 and isinstance(value, dict)) or
             (response.get('http_status') == 404 and isinstance(value, dict) and value.get('message') == 'Branch not protected')),
            'Destination classic protection needs exact successful GETs or explicit unprotected responses')
        if response['http_status'] == 200:actual_classic[name] = value
    for response in [metadata, ref, *details.values(), *classic.values()]:
        at = observed_time(response.get('observed_at'), 'Destination preservation response')
        require(at <= timestamp and (earliest is None or (at > earliest if strict_after else at >= earliest)),
                'Destination preservation responses must follow the earliest boundary')
    return {'rulesets':actual_rules, 'classic':actual_classic}


def validate_protection_preservation(proof, directory, repo, cutover=None):
    require(isinstance(proof, dict) and proof.get('repository_id') == repo['id'] and
            proof.get('repository') == repo['destination'] and immutable_sha(proof.get('revision_sha')) and
            isinstance(proof.get('rulesets'), list) and isinstance(proof.get('classic'), dict),
            'Completed transfer needs structured destination effective-policy evidence')
    capture = local_capture(proof.get('evidence_url'), directory, 'Destination protections')
    require(capture.get('phase') == 'post_transfer' and
            all(capture.get(field) == proof.get(field) for field in
                ('repository_id', 'repository', 'revision_sha', 'observed_at', 'rulesets', 'classic')),
            'Destination protections must match the independent post-transfer capture')
    timestamp = observed_time(capture.get('observed_at'), 'Destination protections')
    if cutover is not None:
        require(timestamp > cutover, 'Destination protections must be captured after source/App cutover')
    actual = validate_protection_responses(capture, repo, repo['destination'], proof['revision_sha'], timestamp, cutover, True)
    require(actual == {'rulesets':proof['rulesets'], 'classic':proof['classic']},
            'Destination preservation collections must derive from complete primary API responses')
    baseline = protection_contract(repo, directory)
    for rule in baseline['rulesets']:
        require(rule in proof['rulesets'], 'Destination must preserve every unrelated baseline ruleset')
    for name, protection in baseline['classic'].items():
        require(protection_semantics(proof['classic'].get(name), repo) == protection_semantics(protection, repo),
                'Destination must preserve every unrelated classic branch protection')


def validate_terminal_protections(row, directory, repo, revision):
    policy = local_capture(row['gate_policy']['evidence_url'], directory, 'Terminal gate policy')
    proof = row.get('terminal_protections')
    validate_protection_preservation(proof, directory, repo,
                                     observed_time(policy['observed_at'], 'Terminal gate policy'))
    require(proof['revision_sha'] == revision,
            'Terminal protection preservation must bind the actual verified deployment revision')
    validate_gate_policy(dict(row['gate_policy'], evidence_url=proof['evidence_url']), directory,
                         repo['id'], repo['destination'], repo['default_branch'],
                         observed_time(policy['observed_at'], 'Terminal gate policy'))


def validate_archived_status(row, directory, repo, cutoff):
    capture = local_capture(row.get('status_evidence_url'), directory, 'Archived destination status')
    metadata = capture.get('metadata', {})
    require(capture.get('phase') == 'post_transfer' and capture.get('repository_id') == repo['id'] and
            capture.get('repository') == repo['destination'] and capture.get('http_status') == 200 and
            capture.get('request_url') == 'https://api.github.com/repos/' + repo['destination'] and
            metadata.get('id') == repo['id'] and metadata.get('full_name') == repo['destination'] and
            all(metadata.get(field) == repo[field] for field in ('private', 'visibility', 'default_branch')) and
            metadata.get('archived') is True,
            'Archived completion needs actual destination metadata proving preserved archived state')
    require(observed_time(capture.get('observed_at'), 'Archived destination status') >= cutoff,
            'Archived destination status must follow transfer and destination preservation')


def run_release_verification(argv):
    """Execute the verifier; a stored success receipt is not signature proof."""
    try:
        result = subprocess.run(argv, capture_output=True, text=True, timeout=60, check=False)
        require(result.returncode == 0, 'Independent release signature verification failed')
        return json.loads(result.stdout)
    except (OSError, subprocess.TimeoutExpired, json.JSONDecodeError) as exc:
        raise ValueError('Independent release signature verification could not complete') from exc


@contextlib.contextmanager
def canonical_release_asset(path, source, version, name):
    asset = pathlib.Path(path)
    if asset.is_file():
        yield asset
        return
    with tempfile.TemporaryDirectory(prefix='sfl-canonical-release-') as directory:
        argv = ['gh', 'release', 'download', 'v' + version, '--repo', source,
                '--pattern', name, '--dir', directory]
        try:
            result = subprocess.run(argv, capture_output=True, text=True, timeout=60, check=False)
            require(result.returncode == 0, 'Canonical release asset download failed')
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise ValueError('Canonical release asset download could not complete') from exc
        asset = pathlib.Path(directory) / name
        require(asset.is_file(), 'Canonical release download did not produce the exact asset')
        yield asset


def validate_release_download(proof, directory, repository_id, repository, source, sha, version):
    release_url = 'https://github.com/' + source + '/releases/tag/v' + version
    download = local_capture(proof.get('release_download_verification_url'), directory, 'Release download')
    assets = {'linux_amd64': 'gh-sfl_' + version + '_linux_amd64',
              'windows_amd64': 'gh-sfl_' + version + '_windows_amd64.exe'}
    require(download.get('platform') in assets and download.get('asset_name') == assets[download['platform']],
            'Release download must verify the versioned installable CLI asset for its platform')
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
    downloaded_at = observed_time(download.get('observed_at'), 'Release download')
    attestation = local_capture(download.get('attestation_evidence_url'), directory, 'Release asset attestation')
    argv = ['gh', 'release', 'verify-asset', 'v' + version, attestation.get('asset_path'), '--repo', source, '--format', 'json']
    require(text(attestation.get('asset_path')) and pathlib.PurePosixPath(attestation['asset_path']).name == download['asset_name'] and
            attestation.get('argv') == argv and type(attestation.get('exit_code')) is int and attestation['exit_code'] == 0 and
            attestation.get('command') == 'gh release verify-asset' and attestation.get('status') == 'completed' and
            observed_time(attestation.get('observed_at'), 'Release asset attestation') <= downloaded_at,
            'Release asset attestation needs the actual successful canonical asset verification command')
    result = attestation.get('result', {})
    verified = result.get('verificationResult', {})
    statement = verified.get('statement', {})
    envelope = result.get('attestation', {}).get('bundle', {}).get('dsseEnvelope', {})
    try:
        signed = json.loads(base64.b64decode(envelope.get('payload', ''), validate=True))
    except (ValueError, TypeError) as exc:
        raise ValueError('Release asset attestation needs its signed statement payload') from exc
    require(signed == statement and statement.get('_type') == 'https://in-toto.io/Statement/v1' and
            statement.get('predicateType') == 'https://in-toto.io/attestation/release/v0.2' and
            envelope.get('payloadType') == 'application/vnd.in-toto+json' and bool(envelope.get('signatures')) and
            verified.get('signature', {}).get('certificate', {}).get('subjectAlternativeName') == 'https://dotcom.releases.github.com' and
            bool(verified.get('verifiedTimestamps')),
            'Release asset attestation must contain the GitHub-verified signed release statement')
    predicate = statement.get('predicate', {})
    purl = 'pkg:github/' + source + '@v' + version
    require(predicate.get('repository') == source and predicate.get('repositoryId') == '1169772257' and
            predicate.get('tag') == 'v' + version and predicate.get('purl') == purl and
            re.fullmatch(r'[1-9][0-9]*', predicate.get('databaseId', '')) is not None,
            'Signed release statement must bind its canonical repository ID, release and tag')
    subjects = statement.get('subject', [])
    source_subjects = [x for x in subjects if x.get('uri') == purl]
    asset_subjects = [x for x in subjects if x.get('name') == download['asset_name']]
    require(len(source_subjects) == len(asset_subjects) == 1 and source_subjects[0].get('digest', {}).get('sha1') == sha and
            asset_subjects[0].get('digest', {}).get('sha256') == download['expected_sha256'],
            'Downloaded digest must match the independently verified signed asset and source commit')
    with canonical_release_asset(attestation['asset_path'], source, version, download['asset_name']) as asset:
        with asset.open('rb') as stream:
            actual = hashlib.file_digest(stream, 'sha256').hexdigest()
        require(actual == download['actual_sha256'], 'Actual downloaded file bytes must match the claimed release digest')
        independent_argv = list(argv)
        independent_argv[4] = str(asset)
        independent = run_release_verification(independent_argv)
        require(independent.get('verificationResult', {}).get('statement') == statement,
                'Independent cryptographic verification must confirm the captured release statement and actual asset')


def validate_final_onboarding(proof, directory, expected, organization, app_id, cutover=None, terminal_times=None):
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
    require(capture.get('request_url') == 'https://api.github.com/repos/' + name and capture.get('http_status') == 200 and
            metadata.get('id') == repo_id and metadata.get('full_name') == name and
            metadata.get('private') == (proof['visibility'] == 'private') and metadata.get('visibility') == proof['visibility'] and metadata.get('archived') is False,
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
        require(terminal_times is not None and entry['repository_id'] in terminal_times and
                completed_at >= terminal_times[entry['repository_id']],
                'Repository completion must follow its latest independently validated terminal evidence')
        require(completed_at.tzinfo is not None and cutoff.tzinfo is not None and completed_at <= cutoff,
                'Baseline completion timestamps cannot follow the recorded rollout completion')
    created_at = datetime.datetime.fromisoformat(metadata.get('created_at', '').replace('Z', '+00:00'))
    rollout_at = datetime.datetime.fromisoformat(proof.get('rollout_completed_at', '').replace('Z', '+00:00'))
    require(created_at.tzinfo is not None and rollout_at.tzinfo is not None and created_at > rollout_at,
            'New onboarding repository must be created after the recorded rollout completion')
    require(observed_time(capture.get('observed_at'), 'New repository creation capture') >= created_at,
            'New repository metadata GET must be observed after its actual creation')
    require(proof.get('deployment_source') == organization + '/set-it-free-loop' and
            immutable_sha(proof.get('deployment_sha')) and semantic_version(proof.get('release_version')),
            'New onboarding needs the canonical immutable deployment and release')
    manifest = proof.get('manifest_identity')
    addons, _ = workflow_catalog()
    require(isinstance(manifest, dict) and manifest.get('source') == proof['deployment_source'] and
            manifest.get('sourceSha') == proof['deployment_sha'] and manifest.get('version') == proof['release_version'] and
            manifest.get('tier') == 'reviewer' and manifest.get('addons') == [] and
            string_list(manifest.get('addons')) and set(manifest['addons']) <= addons and
            '.github/workflows/sfl-pr-review-auto.yml' in deployed_workflow_paths(manifest['tier'],manifest['addons']),
            'Designated new onboarding must prove the default reviewer tier and canonical post-status observer manifest')
    reference = proof.get('manifest_evidence_url')
    evidence(reference,directory)
    require(not urllib.parse.urlsplit(reference).scheme,'New onboarding needs an independent post-status manifest capture')
    observed = json.loads((directory/reference).read_text())
    require(observed.get('repository_id') == repo_id and observed.get('repository') == name and
            observed.get('manifest') == manifest and immutable_sha(observed.get('revision_sha')),
            'New onboarding manifest capture must match its repository and deployed configuration')
    validate_manifest_contents(observed, repo_id, name, manifest)
    require(proof.get('release_url') == 'https://github.com/'+proof['deployment_source']+'/releases/tag/v'+proof['release_version'],
            'New onboarding release verification must use its canonical version')
    validate_release_download(proof, directory, repo_id, name, proof['deployment_source'], proof['deployment_sha'], proof['release_version'])
    operations = proof.get('onboarding_operation_receipts')
    fields = ('init_pr_url', 'repeat_onboarding_url', 'sync_pr_url', 'repeat_sync_url', 'status_url')
    require(isinstance(operations, dict) and set(operations) == set(fields),
            'New onboarding needs initial and repeated onboarding/sync/status operation receipts')
    operation_times = []
    for field in fields:
        bound_operation(operations[field], directory, repo_id, name, proof['deployment_sha'],
                        proof['release_version'], proof.get(field))
        operation = operations[field]
        command = 'init' if field in {'init_pr_url','repeat_onboarding_url'} else 'status' if field == 'status_url' else 'sync'
        outcomes = {'pull_request_merged'} if field == 'init_pr_url' else {'no_changes','pull_request_merged'} if field == 'sync_pr_url' else {'healthy'} if field == 'status_url' else {'no_changes'}
        require(operation.get('command') == command and operation.get('outcome') in outcomes and
                immutable_sha(operation.get('revision_after')),
                'New onboarding operation must prove successful init/sync/status outcomes at the observed revision')
        operation_times.append(validate_terminal_operation(operation, directory,
            max(created_at, cutover) if cutover is not None else created_at))
        if operation['outcome'] == 'pull_request_merged':
            require(operation.get('merged') is True and
                    re.fullmatch('https://github.com/'+re.escape(name)+r'/pull/[1-9][0-9]*',proof[field]) is not None,
                    'New onboarding mutation needs its actual merged deployment PR')
        elif operation['outcome'] == 'no_changes':
            require(operation.get('change_count') == 0 and operation.get('revision_before') == operation['revision_after'],
                    'Repeated onboarding and sync must prove zero changes at the same observed revision')
    require(all(before <= after for before, after in zip(operation_times, operation_times[1:])),
            'New onboarding terminal operations must follow init, repeated init, sync, repeated sync and status chronology')
    require(operations['repeat_onboarding_url']['revision_before'] == operations['init_pr_url']['revision_after'] and
            operations['sync_pr_url']['revision_before'] == operations['repeat_onboarding_url']['revision_after'] and
            operations['repeat_sync_url']['revision_before'] == operations['sync_pr_url']['revision_after'] and
            operations['status_url']['revision_before'] == operations['repeat_sync_url']['revision_after'] and
            operations['status_url']['revision_after'] == operations['repeat_sync_url']['revision_after'] == observed['revision_sha'],
            'New onboarding revisions must follow init, repeated init, sync and final status in order')
    validate_app_coverage(proof.get('destination_sfl_app_access'), directory, repo_id, name, app_id, organization,
                          cutover, repository_created_at=created_at)
    validate_app_coverage(proof.get('destination_codex_access'), directory, repo_id, name, 1144995, organization)
    validate_registered_review(proof, directory, name, target_branch=metadata.get('default_branch'), deployment_revision=observed['revision_sha'], cutover=max(created_at, cutover) if cutover is not None else created_at)
    codex_policy = local_capture(proof.get('codex_installation_policy_evidence_url'), directory, 'New onboarding Codex policy')
    validate_codex_installation_policy(codex_policy, created_at)
    repo = {'id':repo_id, 'destination':name, 'default_branch':metadata['default_branch']}
    validate_consumer_default_head(proof, directory, repo, observed['revision_sha'],
        final_onboarding_terminal_time(proof, directory, include_head=False))
    return proof


def final_onboarding_terminal_time(proof, directory, include_head=True):
    references = [op['capture_evidence_url'] for op in proof['onboarding_operation_receipts'].values()]
    references += [proof['gate_policy']['evidence_url'], proof['requester_permission_evidence_url'],
                   proof['review_deployment_evidence_url'],
                   proof['manifest_evidence_url'], proof['release_download_verification_url']]
    references += [op['capture_evidence_url'] for op in proof['review_operation_receipts'].values()]
    references.append(proof['destination_sfl_app_access']['evidence_url'])
    references.append(proof['codex_installation_policy_evidence_url'])
    if include_head:
        references.append(proof['default_branch_evidence_url'])
    return max(observed_time(local_capture(ref, directory, 'Final onboarding terminal evidence').get('observed_at'),
                             'Final onboarding terminal evidence') for ref in references)


def validate_consumer_status(row, directory, revision, cutover):
    capture = local_capture(row.get('status_evidence_url'), directory, 'Consumer status')
    require(capture.get('repository_id') == row['repository_id'] and capture.get('repository') == row['destination'] and
            capture.get('revision_sha') == revision and capture.get('command') == 'status' and
            capture.get('status') == 'completed' and type(capture.get('exit_code')) is int and capture['exit_code'] == 0 and
            capture.get('health') == 'healthy' and capture.get('manifest_identity') == row['manifest_identity'],
            'Consumer status must prove a successful healthy command at the deployed manifest revision')
    timestamp = observed_time(capture.get('observed_at'), 'Consumer status')
    manifest = local_capture(row['manifest_evidence_url'], directory, 'Consumer manifest')
    require(timestamp >= max(cutover, observed_time(manifest['observed_at'], 'Consumer manifest')),
            'Consumer status must follow cutover and deployed manifest observation')
    checks = capture.get('file_checks')
    paths = deployed_workflow_paths(row['selected_tier'], row['selected_addons'], row['selected_components'])
    require(isinstance(checks, list) and len(checks) == len(paths) and {c.get('path') for c in checks} == paths and
            all(c.get('present') is True and re.fullmatch(r'[0-9a-f]{64}', c.get('expected_sha256', '')) and
                c.get('actual_sha256') == c['expected_sha256'] for c in checks) and
            capture.get('missing_files') == [] and capture.get('drifted_files') == [],
            'Consumer status must corroborate every installed workflow without missing or drifted files')
    for check in checks:
        validate_installation_file(check, row, directory, revision, timestamp)


def immutable_contents(capture, repository_id, repository, revision, path):
    response = capture.get('contents_response', {})
    body = response.get('data', {})
    require(capture.get('repository_id') == repository_id and capture.get('repository') == repository and
            capture.get('revision_sha') == revision and response.get('http_status') == 200 and
            response.get('request_url') == 'https://api.github.com/repos/' + repository + '/contents/' + path + '?ref=' + revision and
            body.get('path') == path and body.get('type') == 'file' and body.get('encoding') == 'base64',
            'Installed file needs repository/revision-bound immutable contents GETs')
    try:
        content = base64.b64decode(''.join(body.get('content', '').split()), validate=True)
    except (ValueError, TypeError) as exc:
        raise ValueError('Installed file contents need valid encoded source bytes') from exc
    blob = hashlib.sha1(b'blob ' + str(len(content)).encode() + b'\0' + content).hexdigest()
    require(body.get('sha') == blob and body.get('size') == len(content) and
            body.get('git_url') == 'https://api.github.com/repos/' + repository + '/git/blobs/' + blob,
            'Installed file bytes must match their independent immutable Git blob identity')
    return content


def validate_manifest_contents(capture, repository_id, repository, manifest):
    path = capture.get('manifest_path')
    require(path in {'.sfl/sfl.json', 'sfl.json'}, 'Installed manifest needs its actual repository path')
    content = immutable_contents(capture, repository_id, repository, capture['revision_sha'], path)
    require(json.loads(content) == manifest, 'Installed manifest identity must derive from immutable repository bytes')


def validate_installation_file(check, row, directory, revision, status_at):
    name = check['path'].removeprefix('.github/workflows/')
    source_path = ('.github/workflows/' + name if name.endswith('.lock.yml') else
                   ('deployment/infrastructure/' if name in {'sfl-pr-review-auto.yml', 'sfl-dispatcher.yml', 'sfl-auditor.yml'}
                    else 'deployment/workflows/') + name)
    source = local_capture(check.get('source_contents_evidence_url'), directory, 'Canonical installed workflow')
    deployed = local_capture(check.get('deployed_contents_evidence_url'), directory, 'Actual installed workflow')
    source_bytes = immutable_contents(source, 1169772257, row['deployment_source'], row['deployment_sha'], source_path)
    actual = immutable_contents(deployed, row['repository_id'], row['destination'], revision, check['path'])
    require(observed_time(source.get('observed_at'), 'Canonical installed workflow') <= status_at and
            observed_time(deployed.get('observed_at'), 'Actual installed workflow') <= status_at,
            'Consumer status must follow independent canonical and actual file captures')
    expected = source_bytes.decode('utf-8')
    if name == 'sfl-pr-review-auto.yml':
        placeholders = ('# Source: HemSoft/set-it-free-loop/deployment/infrastructure/sfl-pr-review-auto.yml@main',
                        '    branches: [main]', '  SFL_REVIEW_BASE_BRANCH: main')
        require(all(value in expected for value in placeholders), 'Canonical reviewer source needs supported deployment placeholders')
        source_ref = row['deployment_source'] + '/' + source_path + '@' + row['deployment_sha']
        branch = row['gate_policy']['branch'].replace("'", "''")
        for old, new in zip(placeholders, ('# Source: ' + source_ref, "    branches: ['" + branch + "']",
                                           "  SFL_REVIEW_BASE_BRANCH: '" + branch + "'")):
            expected = expected.replace(old, new, 1)
        expected = '# Deployed from: ' + source_ref + '\n# To upgrade: re-run deploy-workflow.ps1 at the desired SHA\n' + expected
    expected = expected.replace('__SFL_VERSION__', row['manifest_version']).encode('utf-8')
    require(actual == expected and check['expected_sha256'] == hashlib.sha256(expected).hexdigest() and
            check['actual_sha256'] == hashlib.sha256(actual).hexdigest(),
            'Installed workflow hashes must derive from the pinned canonical source and actual deployed bytes')


def validate_workflow_contents(workflow, repository, revision):
    response = workflow.get('contents_response', {})
    body = response.get('data', {})
    path = '.github/workflows/sfl-pr-review-auto.yml'
    require(response.get('http_status') == 200 and response.get('request_url') ==
            'https://api.github.com/repos/' + repository + '/contents/' + path + '?ref=' + revision and
            body.get('path') == path and body.get('type') == 'file' and body.get('encoding') == 'base64',
            'Fixture source needs the captured immutable repository contents response')
    try:
        content = base64.b64decode(''.join(body.get('content', '').split()), validate=True)
    except (ValueError, TypeError) as exc:
        raise ValueError('Fixture contents response needs valid encoded source bytes') from exc
    blob_sha = hashlib.sha1(b'blob ' + str(len(content)).encode() + b'\0' + content).hexdigest()
    require(content == workflow['content'].encode() and body.get('sha') == blob_sha and body.get('size') == len(content) and
            body.get('git_url') == 'https://api.github.com/repos/' + repository + '/git/blobs/' + blob_sha,
            'Fixture source must match its independently captured immutable Git blob identity')


def validate_raw_page_chain(pages, url, captured_at, label, field=None, earliest=None):
    require(isinstance(pages, list) and bool(pages), label + ' needs raw complete response pages')
    parsed = urllib.parse.urlsplit(url)
    endpoint, filters = parsed.path, urllib.parse.parse_qs(parsed.query)
    rows, totals, seen = [], set(), set()
    page_number = 1
    for page in pages:
        require(url is not None and url not in seen and page.get('request_url') == url and
                page.get('method') == 'GET' and page.get('http_status') == 200 and
                isinstance(page.get('response_headers'), dict), label + ' needs exact successful GET page responses')
        seen.add(url)
        at = observed_time(page.get('observed_at'), label + ' page')
        require(at <= captured_at and (earliest is None or at >= earliest), label + ' page is outside its freshness boundary')
        data = page.get('data')
        if field is not None:
            require(isinstance(data, dict) and type(data.get('total_count')) is int and
                    data['total_count'] >= 0, label + ' needs raw response totals')
            totals.add(data['total_count'])
            data = data.get(field)
        require(isinstance(data, list), label + ' needs raw response arrays')
        rows.extend(data)
        headers = {key.lower(): value for key, value in page['response_headers'].items()}
        links = re.findall(r'<([^>]+)>;\s*rel="next"', headers.get('link', ''))
        require(len(links) <= 1, label + ' has ambiguous pagination')
        url = links[0] if links else None
        if url:
            next_page = urllib.parse.urlsplit(url)
            query = urllib.parse.parse_qs(next_page.query)
            page_number += 1
            require(next_page.scheme == 'https' and next_page.netloc == 'api.github.com' and
                    next_page.path == endpoint and query == dict(filters, page=[str(page_number)]),
                    label + ' pagination must preserve its endpoint, filters and consecutive pages')
    require(url is None, label + ' omitted a captured next page')
    if field is not None:
        require(totals == {len(rows)}, label + ' response total differs from all captured pages')
    return rows


def validate_codex_installation_policy(capture, earliest=None):
    observed = observed_time(capture.get('observed_at'), 'Codex installation policy')
    url = 'https://api.github.com/orgs/hemsoft-dev/installations?per_page=100'
    installations = validate_raw_page_chain(capture.get('installation_pages'), url, observed,
        'Codex installation policy', field='installations', earliest=earliest)
    matched = [item for item in installations if item.get('app_id') == 1144995]
    require(len(matched) == 1, 'Codex policy needs its unique actual organization installation')
    actual = matched[0]
    require(actual.get('id') == 168678981 and actual.get('app_slug') == 'chatgpt-codex-connector' and
            actual.get('repository_selection') == 'all' and actual.get('account', {}).get('id') == 338855369 and
            actual['account'].get('login') == 'hemsoft-dev' and actual['account'].get('type') == 'Organization' and
            'suspended_at' in actual and actual['suspended_at'] is None and
            'suspended_by' in actual and actual['suspended_by'] is None,
            'Codex policy needs actual all-current-and-future repository coverage and unsuspended organization identity')
    require(all(capture.get('installation', {}).get(key) == actual.get(key) for key in
                ('id','app_id','app_slug','repository_selection')) and
            all(capture.get('account', {}).get(key) == actual['account'].get(key) for key in ('id','login','type')),
            'Codex installation summary must derive from its raw API response')
    return observed


def validate_repository_enumeration(account, captured_at, earliest=None):
    owner = account['owner']
    url = ('https://api.github.com/user/repos?affiliation=owner&per_page=100' if owner == 'HemSoft' else
           'https://api.github.com/orgs/' + owner + '/repos?type=all&per_page=100')
    pages = account.get('pages')
    require(isinstance(pages, list) and bool(pages), 'Final inventory needs raw complete repository enumeration pages')
    repositories = []
    for page in pages:
        require(url is not None and page.get('request_url') == url and page.get('http_status') == 200 and
                page.get('method') == 'GET' and isinstance(page.get('data'), list) and
                isinstance(page.get('response_headers'), dict) and
                observed_time(page.get('observed_at'), 'Final inventory page') <= captured_at,
                'Final inventory pages need successful exact account GETs, response headers and capture times')
        if earliest is not None:
            require(observed_time(page['observed_at'], 'Repository page') >= earliest,
                    'Repository enumeration pages must follow the independent freshness boundary')
        repositories.extend(page['data'])
        headers = {key.lower(): value for key, value in page['response_headers'].items()}
        links = re.findall(r'<([^>]+)>;\s*rel="next"', headers.get('link', ''))
        require(len(links) <= 1, 'Final inventory pagination has ambiguous next links')
        url = links[0] if links else None
        if url is not None:
            parsed = urllib.parse.urlsplit(url)
            query = urllib.parse.parse_qs(parsed.query)
            require(parsed.scheme == 'https' and parsed.netloc == 'api.github.com' and
                    parsed.path == ('/user/repos' if owner == 'HemSoft' else '/orgs/' + owner + '/repos') and
                    query == dict({'affiliation':['owner']} if owner == 'HemSoft' else {'type':['all']},
                                  per_page=['100'], page=query.get('page')) and
                    isinstance(query.get('page'), list) and len(query['page']) == 1 and
                    re.fullmatch(r'[1-9][0-9]*', query['page'][0]) is not None,
                    'Final inventory pagination must stay on its exact account endpoint')
    require(url is None, 'Final inventory omitted a captured next repository page')
    fields = ('id', 'full_name', 'private', 'visibility', 'archived', 'default_branch')
    project = lambda rows: [{key: row[key] for key in fields if key in row} for row in rows]
    require(project(repositories) == project(account['repositories']),
            'Final inventory list must derive from all raw account repository pages')


def validate_wider_pilot_ordering(disposable_wider_verified, existing_wider_id, existing_completed_at, rollout_starts):
    require(disposable_wider_verified or existing_completed_at is not None,
            'Active rollout requires a verified wider-workflow and auditor pilot')
    if not disposable_wider_verified:
        require(all(existing_completed_at < started for rid, started in rollout_starts if rid != existing_wider_id),
                'Existing wider pilot must finish validation before other active consumers start')


def validate_existing_wider_pilot(row, directory, revision, cutover):
    """Qualify the existing full consumer only after its normal rollout contracts pass."""
    require(row.get('repository_id') == 1229335234 and
            row.get('source') == 'HemSoft/hs-buddy' and row.get('destination') == 'hemsoft-dev/hs-buddy' and
            row.get('health') == 'verified' and row.get('installed_tier') == row.get('selected_tier') == 'full',
            'Existing wider pilot must preserve the designated hs-buddy full deployment')
    runs, operations = row.get('wider_workflow_run_urls'), row.get('wider_operation_receipts')
    require(isinstance(runs, list) and isinstance(operations, list) and len(runs) == len(operations) and len(runs) >= 2,
            'Existing wider pilot needs distinct successful Auditor and non-Auditor runs')
    allowed = deployed_workflow_paths('full', row['selected_addons']) - {'.github/workflows/sfl-pr-review-auto.yml'}
    for run, operation in zip(runs, operations):
        bound_workflow_operation(operation, directory, row['repository_id'], row['destination'],
            row['deployment_sha'], row['manifest_version'], run, allowed, revision, cutover)
    auditor = [(run, operation) for run, operation in zip(runs, operations)
               if operation['workflow'] == '.github/workflows/sfl-auditor.yml']
    require(len(auditor) == 1 and any(operation['workflow'] != '.github/workflows/sfl-auditor.yml' and
            run != auditor[0][0] for run, operation in zip(runs, operations)),
            'Existing wider pilot must execute one Auditor and a distinct non-Auditor workflow')


def validate_final_inventory(proof, directory, expected, retained, approved_onboarding=None, pilot_branches=None):
    require(isinstance(proof, dict), 'Final completion needs a fresh independent repository inventory')
    reference = proof.get('evidence_url')
    evidence(reference, directory)
    require(not urllib.parse.urlsplit(reference).scheme and reference != 'inventory.json',
            'Final inventory needs a separate captured enumeration artifact')
    capture = json.loads((directory / reference).read_text())
    require(capture.get('observed_at') == proof.get('observed_at'), 'Final inventory timestamp must match its capture')
    captured_at = datetime.datetime.fromisoformat(capture['observed_at'].replace('Z', '+00:00'))
    require(captured_at.tzinfo is not None, 'Final inventory timestamp needs a timezone')
    if approved_onboarding is not None:
        metadata = local_capture(approved_onboarding['metadata_evidence_url'], directory, 'New repository metadata')['metadata']
        status = local_capture(approved_onboarding['manifest_evidence_url'], directory, 'New repository status')
        require(captured_at >= max(observed_time(approved_onboarding['rollout_completed_at'], 'Rollout completion'),
                                  observed_time(metadata['created_at'], 'New repository creation'),
                                  observed_time(status.get('observed_at'), 'New repository status'),
                                  final_onboarding_terminal_time(approved_onboarding, directory)),
                'Final inventory must follow rollout completion, new repository creation and final status')
    accounts = capture.get('accounts')
    require(isinstance(accounts, list) and {x.get('owner') for x in accounts} == {'HemSoft', 'fhemmer', 'hemsoft-dev'} and
            len(accounts) == 3, 'Final inventory must enumerate both sources and the destination')
    actual = {}
    for account in accounts:
        require(account.get('state') == 'observed' and account.get('all_pages') is True and
                isinstance(account.get('repositories'), list), 'Final inventory enumeration must be complete')
        validate_repository_enumeration(account, captured_at,
            final_onboarding_terminal_time(approved_onboarding, directory) if approved_onboarding is not None else None)
        for repo in account['repositories']:
            repo_id = repo.get('id')
            require(type(repo_id) is int and repo_id not in actual and
                    text(repo.get('full_name')) and repo['full_name'].startswith(account['owner'] + '/'),
                    'Final inventory repository IDs must be unique and bound to their account')
            actual[repo_id] = repo
    for repo_id, repo in expected.items():
        current = actual.get(repo_id, {})
        require(current.get('full_name') == (repo['full_name'] if repo_id in retained else repo['destination']) and
                current.get('private') == repo['private'] and current.get('visibility') == repo['visibility'] and current.get('archived') == repo['archived'] and
                current.get('default_branch') == repo['default_branch'],
                'Final inventory must prove each baseline ID at its actual mapped location and preserve state')
    extras = proof.get('additional_repositories')
    require(isinstance(extras, list), 'Final inventory needs explicit additional-repository accounting')
    if approved_onboarding is not None:
        metadata = local_capture(approved_onboarding['metadata_evidence_url'], directory, 'New repository metadata')['metadata']
        require(actual.get(approved_onboarding['repository_id'], {}).get('default_branch') == metadata['default_branch'] ==
                approved_onboarding['gate_policy']['branch'], 'Final onboarding must preserve its actually gated default branch')
        require(actual.get(approved_onboarding['repository_id'], {}).get('archived') is False,
                'Final onboarding repository must remain unarchived in the final inventory')
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
        receipt_match = re.fullmatch(r'https://github.com/(HemSoft|hemsoft-dev)/set-it-free-loop/issues/138#issuecomment-([1-9][0-9]*)',
                                    decision.get('evidence_url', ''))
        require(receipt_match is not None,
                'Additional repository needs an explicit owner issue receipt')
        validate_owner_approval_comment(decision.get('owner_comment_evidence_url'), directory,
            decision['evidence_url'], approved_at,
            {key: decision[key] for key in ('repository_id', 'repository', 'visibility', 'disposition', 'reason')}, captured_at)
        allowed[repo_id] = (repo['repository'], repo['visibility'])
    require(set(actual) == set(expected) | set(allowed), 'Final inventory contains unaccounted repository IDs')
    if pilot_branches is not None:
        require(set(pilot_branches) == set(APPROVED_PILOTS) and
                all(actual[rid].get('default_branch') == branch for rid, branch in pilot_branches.items()),
                'Final pilot default branches must match their independently captured and gated branches')
    for repo_id, (name, visibility) in allowed.items():
        require(actual[repo_id]['full_name'] == name and actual[repo_id].get('private') == (visibility == 'private') and actual[repo_id].get('visibility') == visibility,
                'Final inventory additions must match their recorded identity and visibility')


def validate_owner_approval_comment(reference, directory, receipt_url, approved_at, decision, latest):
    match = re.fullmatch(r'https://github.com/(HemSoft|hemsoft-dev)/set-it-free-loop/issues/(138|139)#issuecomment-([1-9][0-9]*)', receipt_url)
    require(match is not None, 'Owner approval needs an explicit migration issue receipt')
    receipt = local_capture(reference, directory, 'Owner approval comment')
    comment = receipt.get('comment', {})
    require(receipt.get('method') == 'GET' and receipt.get('http_status') == 200 and receipt.get('request_url') ==
            'https://api.github.com/repos/' + match[1] + '/set-it-free-loop/issues/comments/' + match[3] and
            comment.get('id') == int(match[3]) and comment.get('user', {}).get('login') == 'HemSoft' and
            comment['user'].get('id') == 8227352 and comment['user'].get('type') == 'User' and
            comment.get('html_url') in {'https://github.com/' + owner + '/set-it-free-loop/issues/' + match[2] + '#issuecomment-' + match[3]
                                        for owner in ('HemSoft', 'hemsoft-dev')} and
            observed_time(comment.get('created_at'), 'Owner approval creation') <= approved_at and
            observed_time(comment.get('updated_at'), 'Owner effective approval') == approved_at and
            approved_at <=
            observed_time(receipt.get('observed_at'), 'Owner approval capture') <= latest,
            'Owner approval must match the actual HemSoft comment identity and chronology')
    markers = re.findall(r'<!-- sfl-migration-approval:(.*?) -->', comment.get('body', ''), re.DOTALL)
    require(len(markers) == 1, 'Owner comment needs one explicit structured approval')
    try:
        approval = json.loads(markers[0])
    except ValueError as exc:
        raise ValueError('Owner approval must contain valid JSON') from exc
    require(approval == decision, 'Owner comment must approve this exact repository decision')


def validate_owner_scope(record, decision, directory):
    """Authenticate recorded global facts using the same owner-comment contract."""
    capture = local_capture(record.get('owner_comment_evidence_url'), directory, 'Owner scope comment')
    recorded_at = observed_time(record.get('decision_recorded_at'), 'Owner scope decision')
    captured_at = observed_time(capture.get('observed_at'), 'Owner scope capture')
    validate_owner_approval_comment(record['owner_comment_evidence_url'], directory, record['evidence_url'],
                                    recorded_at, decision, captured_at)
    require(observed_time(record.get('confirmed_at'), 'Original owner scope confirmation') <= recorded_at,
            'Structured owner scope cannot precede its original confirmation')
    return captured_at


def validate_app_coverage(coverage, directory, repository_id, repository, app_id, owner, cutover=None,
                          repository_created_at=None):
    require(isinstance(coverage, dict) and coverage.get('status') == 'verified' and
            coverage.get('app_id') == app_id and coverage.get('owner') == owner and
            coverage.get('repository_id') == repository_id and coverage.get('repository') == repository and
            type(coverage.get('installation_id')) is int and coverage['installation_id'] > 0,
            'Verified destination needs repository-bound post-transfer SFL App coverage')
    evidence(coverage.get('evidence_url'), directory)
    if app_id == 4448946:
        access = local_capture(coverage['evidence_url'], directory, 'Repository App installation')
        installation = access.get('installation', {})
        require(access.get('repository_id') == repository_id and access.get('repository') == repository and
                access.get('http_status') == 200 and
                access.get('request_url') == 'https://api.github.com/repos/' + repository + '/installation' and
                installation.get('id') == coverage['installation_id'] and installation.get('app_id') == app_id and
                installation.get('account', {}).get('login') == owner and
                installation['account'].get('id') == 338855369 and installation['account'].get('type') == 'Organization' and
                installation.get('target_type') == 'Organization' and installation.get('repository_selection') == 'all' and
                installation.get('permissions') == SFL_APP_PERMISSIONS and
                'suspended_at' in installation and installation['suspended_at'] is None,
                'SFL coverage needs an unsuspended repository-specific destination installation GET')
        cutoffs = [time for time in (cutover, repository_created_at) if time is not None]
        access_at = observed_time(access.get('observed_at'), 'Repository App installation')
        if cutoffs:
            require(access_at >= max(cutoffs),
                    'Repository App installation GET must follow repository creation and App cutover')
        capture = local_capture('owned-app-organization-installation-evidence.json', directory, 'Destination App installation')
        require(capture.get('phase') == 'post_transfer', 'Destination App installation must be captured after transfer')
        installation_at = observed_time(capture.get('observed_at'), 'Destination App installation')
        if cutover is not None:
            require(installation_at >= cutover, 'Destination App installation capture must follow App ownership transfer')
        require(installation_at <= access_at,
                'Repository App access must follow the shared destination installation capture')
        require(capture.get('verification_status') == 'verified' and capture.get('account',{}).get('login') == owner and
                capture.get('installation',{}).get('id') == coverage['installation_id'] and
                capture['installation'].get('app_id') == app_id and capture['installation'].get('repository_selection') == 'all' and
                capture['installation'].get('permissions') == SFL_APP_PERMISSIONS,
                'SFL coverage must match the captured all-repositories destination installation')
    if app_id == 1144995:
        capture = json.loads((directory / 'codex-organization-installation-evidence.json').read_text())
        require(coverage['installation_id'] == capture['installation']['id'] and
                capture['installation']['app_id'] == app_id and capture['account']['login'] == owner,
                'Codex coverage must match the independently captured organization installation')


def registered_review_external_id(row, directory, repository_id, repository):
    captures = {}
    for field in ('review_registration_url', 'review_registry_status_url', 'review_artifact_url'):
        receipt = row['review_operation_receipts'][field]
        capture = local_capture(receipt.get('capture_evidence_url'), directory, 'Registered review artifact')
        require(all(capture.get(key) == receipt.get(key) for key in
                    ('repository_id', 'repository', 'head_sha', 'base_sha', 'pr_url', 'evidence_url')),
                'Registered review capture must bind the exact request and review context')
        observed_time(capture.get('observed_at'), 'Registered review artifact')
        captures[field] = capture
    request = captures['review_registration_url'].get('comment', {})
    request_id = request.get('id')
    request_capture = captures['review_registration_url']
    validate_resource_response(request_capture, 'https://api.github.com/repos/' + repository +
                               '/issues/comments/' + str(request_id), request)
    request_at = observed_time(request.get('created_at'), 'Registered request creation')
    require(type(request_id) is int and request_id > 0 and request.get('user', {}).get('login') == row['review_requester'] and
            request.get('html_url') == row['review_registration_url'] == row['review_pr_url'] + '#issuecomment-' + str(request_id) and
            request.get('updated_at') == request.get('created_at'),
            'Registered request must be the actual unedited requester-authored PR comment')
    marker = re.search(r'<!-- sfl-codex-review:head=([0-9a-f]{40});base=([0-9a-f]{40});context=([^\s]+) -->',
                       request.get('body', ''))
    require(marker is not None and marker.group(1) == row['review_head_sha'] and marker.group(2) == row['review_base_sha'] and
            '@codex review' in request.get('body', ''), 'Registered request must carry its exact head, base and context marker')
    number = row['review_pr_url'].rsplit('/', 1)[1]
    registry = captures['review_registry_status_url'].get('commit_status', {})
    registry_capture = captures['review_registry_status_url']
    statuses = validate_raw_page_chain(registry_capture.get('status_pages'),
        'https://api.github.com/repos/' + repository + '/commits/' + row['review_head_sha'] + '/statuses?per_page=100',
        observed_time(registry_capture['observed_at'], 'Registry status capture'), 'Registered commit statuses')
    require([status for status in statuses if status.get('id') == registry.get('id')] == [registry],
            'Registered status must derive from the complete head-bound status GETs')
    require(type(registry.get('id')) is int and registry['id'] > 0 and registry.get('state') == 'success' and
            registry.get('context') == 'SFL Codex Review Request Registry' and
            registry.get('target_url') == row['review_registration_url'] and
            registry.get('creator', {}).get('login') == row['review_requester'] and
            registry.get('description') == 'SFL Codex request comment ' + str(request_id) + ' for PR #' + number + ' base ' + row['review_base_sha'],
            'Captured registry status must register that exact requester, request ID, PR and base')
    artifact_capture = captures['review_artifact_url']
    artifact = artifact_capture.get('artifact', {})
    kind = artifact_capture.get('kind')
    artifact_id = artifact.get('id')
    validate_resource_response(artifact_capture, 'https://api.github.com/repos/' + repository +
        ('/issues/comments/' + str(artifact_id) if kind == 'issue_comment' else
         '/pulls/' + row['review_pr_url'].rsplit('/', 1)[1] + '/reviews/' + str(artifact_id)), artifact)
    require(type(artifact_id) is int and artifact_id > 0 and kind in {'issue_comment', 'pull_request_review'} and
            artifact.get('user', {}).get('id') == 199175422 and artifact.get('performed_via_github_app', {}).get('id') == 1144995 and
            artifact.get('html_url') == row['review_artifact_url'] == row['review_pr_url'] +
                ('#issuecomment-' if kind == 'issue_comment' else '#pullrequestreview-') + str(artifact_id),
            'Captured native Codex artifact must prove its immutable ID, App, author and PR')
    if kind == 'issue_comment':
        reviewed = re.search(r'\*\*Reviewed commit:\*\*\s+`([0-9a-f]{7,40})`', artifact.get('body', ''), re.I)
        require(reviewed is not None and row['review_head_sha'].startswith(reviewed.group(1).lower()),
                'Native comment must name the actual reviewed head')
        artifact_at = observed_time(artifact.get('created_at'), 'Native Codex artifact creation')
    else:
        require(artifact.get('commit_id') == row['review_head_sha'], 'Native review must name the actual reviewed head')
        artifact_at = observed_time(artifact.get('submitted_at'), 'Native Codex artifact creation')
    require(artifact_at >= request_at and all(observed_time(c['observed_at'], 'Registered review artifact') >= request_at for c in captures.values()) and
            observed_time(artifact_capture['observed_at'], 'Native artifact observation') >= artifact_at,
            'Registered review captures must follow their actual request and native artifact')
    context = urllib.parse.quote(marker.group(3), safe="-_.!~*'()")
    external_id = 'sfl-codex-review:pull:' + number + ':base:' + row['review_base_sha'] + ':context:' + context + \
        ':request:' + str(request_id) + ':at:' + str(int(request_at.timestamp() * 1000)) + \
        ':artifact:' + ('c' if kind == 'issue_comment' else 'r') + str(artifact_id)
    return external_id, request_at, artifact_at


def validate_resource_response(capture, url, expected):
    require(isinstance(capture, dict) and isinstance(expected, (dict, list)) and
            capture.get('method') == 'GET' and capture.get('http_status') == 200 and
            capture.get('request_url') == url and capture.get('data') == expected,
            'Resource capture must derive from its exact successful API GET response')


def validate_observer_execution(capture, row, directory, repository_id, repository, deployment_revision):
    run, check = capture['run'], capture['check_run']
    summaries = re.findall(r'<!-- sfl-gate-execution:([A-Za-z0-9+/=]+) -->', check.get('output', {}).get('summary', ''))
    require(len(summaries) == 1, 'Successful gate needs its durable observer execution identity')
    try:
        execution = json.loads(base64.b64decode(summaries[0], validate=True))
    except (ValueError, TypeError) as exc:
        raise ValueError('Observer execution identity needs valid encoded JSON') from exc
    require(type(run.get('id')) is int and run['id'] > 0 and type(check.get('id')) is int and check['id'] > 0 and
            run['html_url'] == 'https://github.com/' + repository + '/actions/runs/' + str(run['id']) and
            run.get('event') in {'issue_comment', 'pull_request_review'} and
            execution.get('repository_id') == repository_id and execution.get('repository') == repository and
            execution.get('run_id') == run['id'] and execution.get('run_attempt') == run.get('run_attempt') and
            type(execution.get('run_attempt')) is int and execution['run_attempt'] > 0 and
            execution.get('check_run_id') == check['id'] and execution.get('external_id') == check['external_id'] and
            execution.get('execution_sha') == run['head_sha'] and execution.get('event') == run['event'] and
            execution.get('workflow_path') == run['path'] and immutable_sha(execution.get('workflow_sha')),
            'Successful check must bind its exact observer run, attempt, check ID and executed workflow')
    proof = local_capture(capture.get('execution_evidence_url'), directory, 'Executed observer workflow')
    require(proof.get('repository_id') == repository_id and proof.get('repository') == repository and
            proof.get('run_id') == run['id'] and proof.get('run_attempt') == run['run_attempt'] and
            proof.get('workflow_sha') == execution['workflow_sha'] and proof.get('deployment_revision') == deployment_revision,
            'Executed workflow proof must bind its actual run and deployed revision')
    require(observed_time(run.get('created_at'), 'Observer run creation') <=
            observed_time(proof.get('observed_at'), 'Executed observer workflow') <=
            observed_time(capture.get('observed_at'), 'Observer gate observation'),
            'Executed workflow capture must follow its run and precede the completed gate observation')
    for key, revision in [('executed_workflow', execution['workflow_sha']), ('deployed_workflow', deployment_revision)]:
        workflow = proof.get(key, {})
        require(text(workflow.get('content')), 'Executed observer needs actual workflow bytes')
        validate_workflow_contents(workflow, repository, revision)
    comparison = proof.get('comparison', {})
    require(comparison.get('status') in {'ahead', 'identical'} and comparison.get('base_commit', {}).get('sha') == deployment_revision and
            comparison.get('merge_base_commit', {}).get('sha') == deployment_revision and
            comparison.get('html_url') == 'https://github.com/' + repository + '/compare/' + deployment_revision + '...' + execution['workflow_sha'] and
            proof['executed_workflow']['content'] == proof['deployed_workflow']['content'],
            'Actually executed observer must preserve the deployed workflow bytes and descend from its revision')


def validate_registered_review(row, directory, repository, repository_id=None, target_branch=None, deployment_revision=None, cutover=None):
    if repository_id is None:
        repository_id = row.get('repository_id')

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
    validate_resource_response(permission, 'https://api.github.com/repos/' + repository + '/collaborators/' +
        row['review_requester'] + '/permission', permission['result'])
    result = permission['result']
    role = result.get('role_name') or result.get('permission')
    require(role == row['requester_permission'] and role in {'write', 'maintain', 'admin'} and
            result.get('permission') in {'write', 'admin'}, 'Requester permission capture must independently grant write or higher')
    observed_time(permission.get('observed_at'), 'Requester permission')
    require(re.fullmatch('https://github.com/' + re.escape(repository) + r'/pull/[1-9][0-9]*',
                        row.get('review_pr_url','')) is not None,
            'Completed review needs a same-destination-repository PR')
    pr_capture = local_capture(row.get('review_pr_metadata_evidence_url'), directory, 'Reviewed PR metadata')
    pr = pr_capture.get('pull_request', {})
    number = int(row['review_pr_url'].rsplit('/', 1)[1])
    require(pr_capture.get('request_url') == 'https://api.github.com/repos/' + repository + '/pulls/' + str(number) and
            pr.get('number') == number and pr.get('html_url') == row['review_pr_url'] and
            pr.get('base', {}).get('ref') == target_branch and pr['base'].get('sha') == row['review_base_sha'] and
            pr['base'].get('repo', {}).get('id') == repository_id and pr['base']['repo'].get('full_name') == repository and
            pr.get('head', {}).get('sha') == row['review_head_sha'] and
            pr['head'].get('repo', {}).get('id') == repository_id and pr['head']['repo'].get('full_name') == repository,
            'Reviewed PR metadata must target the gated branch and bind its actual repository/head/base')
    validate_resource_response(pr_capture, 'https://api.github.com/repos/' + repository + '/pulls/' + str(number), pr)
    pr_at = observed_time(pr_capture.get('observed_at'), 'Reviewed PR metadata')
    if cutover is not None:
        require(pr_at >= cutover, 'Reviewed PR metadata must follow destination/App cutover')
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
    compare_response = ancestry.get('compare_response', {})
    validate_resource_response(compare_response, 'https://api.github.com/repos/' + repository + '/compare/' +
        deployment_revision + '...' + row['review_base_sha'], comparison)
    compare_at = observed_time(compare_response.get('observed_at'), 'Review ancestry compare GET')
    require(compare_at <= observed_time(ancestry.get('observed_at'), 'Review deployment ancestry') and
            (cutover is None or compare_at >= cutover),
            'Review ancestry comparison GET must follow cutover and precede its capture')
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
    gate_at = observed_time(captured_gate.get('observed_at'), 'Gate result')
    require(pr_at <= gate_at, 'Gate observation must follow its current PR metadata capture')
    run = captured_gate.get('run', {})
    require(run.get('repository', {}).get('id') == repository_id and
            run['repository'].get('full_name') == repository and immutable_sha(run.get('head_sha')) and
            run.get('path') == gate['workflow'] and run.get('status') == 'completed' and
            run.get('conclusion') == 'success' and
            re.fullmatch('https://github.com/' + re.escape(repository) + r'/actions/runs/[1-9][0-9]*',
                         run.get('html_url', '')) is not None,
            'Gate capture must include its actual successful repository-bound Actions workflow run')
    check = captured_gate.get('check_run', {})
    external_id, request_at, artifact_at = registered_review_external_id(row, directory, repository_id, repository)
    require(check.get('head_sha') == row['review_head_sha'] and check.get('name') == gate['context'] and
            check.get('app', {}).get('id') == 15368 and check.get('status') == 'completed' and
            check.get('conclusion') == 'success' and check.get('html_url') == row['gate_run_url'] and
            check.get('external_id') == external_id,
            'Actions-owned check capture must prove the actual reviewed PR head and successful gate')
    validate_observer_execution(captured_gate, row, directory, repository_id, repository, deployment_revision)
    validate_resource_response(captured_gate.get('run_response', {}), 'https://api.github.com/repos/' + repository +
        '/actions/runs/' + str(run.get('id')), run)
    validate_resource_response(captured_gate.get('check_response', {}), 'https://api.github.com/repos/' + repository +
        '/check-runs/' + str(check.get('id')), check)
    created = observed_time(run.get('created_at'), 'Registered review run creation')
    completed = observed_time(run.get('updated_at'), 'Registered review run completion')
    check_started = observed_time(check.get('started_at'), 'Gate check creation')
    check_completed = observed_time(check.get('completed_at'), 'Gate check completion')
    require(request_at <= check_started and artifact_at <= check_completed,
            'Successful gate must follow its exact registered request and native artifact')
    require(created <= check_started <= check_completed <= gate_at and created <= completed <= gate_at,
            'Gate capture must follow actual workflow and check completion')
    if cutover is not None:
        require(created >= cutover and check_started >= cutover and request_at >= cutover,
                'Registered review must execute after destination/App cutover')
    validate_gate_policy(row.get('gate_policy'), directory, repository_id, repository, target_branch,
                         max(completed, check_completed, cutover) if cutover is not None else max(completed, check_completed))



def source_tag_heads(row, repo, earliest=None):
    base = 'https://api.github.com/repos/' + repo['full_name']
    response = row.get('tag_refs_response', {})
    timestamp = observed_time(row.get('observed_at'), 'Source tag capture')
    at = observed_time(response.get('observed_at'), 'Source tag refs GET')
    require(response.get('method') == 'GET' and response.get('request_url') == base + '/git/matching-refs/tags/' and
            at <= timestamp and (earliest is None or at >= earliest),
            'Source tags need their exact current matching-refs GET')
    if response.get('http_status') == 409:
        require(repo['id'] == 996911586 and row.get('state') == 'uninitialized' and
                response.get('data', {}).get('message') == 'Git Repository is empty.',
                'Only the sealed empty repository may have an unavailable tag namespace')
        refs = []
    else:
        refs = response.get('data')
        validate_resource_response(response, base + '/git/matching-refs/tags/', refs)
        require(isinstance(refs, list) and 'rel="next"' not in response.get('response_headers', {}).get('Link', ''),
                'Source tag namespace must be the complete matching-refs array')
    objects = row.get('tag_object_responses', {})
    require(isinstance(objects, dict), 'Annotated source tags need their immutable object responses')
    tags, used_objects = {}, set()
    for ref in refs:
        name, obj = ref.get('ref'), ref.get('object', {})
        require(text(name) and name.startswith('refs/tags/') and len(name) > len('refs/tags/') and
                name not in tags and obj.get('type') in {'commit','tag','tree','blob'} and immutable_sha(obj.get('sha')),
                'Source tags need unique complete names and immutable object identities')
        original = dict(obj)
        visited = set()
        while obj['type'] == 'tag':
            sha = obj['sha']
            require(sha not in visited and len(visited) < 64, 'Annotated tag resolution must terminate without cycles')
            visited.add(sha);used_objects.add(sha)
            primary = objects.get(sha, {});value = primary.get('data')
            validate_resource_response(primary, base + '/git/tags/' + sha, value)
            object_at = observed_time(primary.get('observed_at'), 'Annotated tag object GET')
            require(at <= object_at <= timestamp and isinstance(value, dict) and value.get('sha') == sha and
                    value.get('object', {}).get('type') in {'commit','tag','tree','blob'} and
                    immutable_sha(value.get('object', {}).get('sha')),
                    'Annotated tag objects must match their exact immutable identities and capture interval')
            obj = value['object']
        tags[name] = {'type':original['type'], 'sha':original['sha'], 'target_type':obj['type'],
                      'commit_sha':obj['sha'] if obj['type'] == 'commit' else None}
    require(set(objects) == used_objects, 'Tag object captures must cover exactly every traversed annotation')
    return tags


def source_revision(row, repo, earliest=None):
    """Bind a complete branch list and default head to raw immutable Git data."""
    base = 'https://api.github.com/repos/' + repo['full_name']
    branches = row.get('branches_response', {})
    require(branches.get('request_url') == base + '/branches?per_page=100' and
            branches.get('http_status') == 200 and branches.get('all_pages') is True and
            isinstance(branches.get('data'), list), 'Source heads need complete repository-bound branches GETs')
    actual_branches = validate_raw_page_chain(branches.get('pages'), base + '/branches?per_page=100',
        observed_time(row.get('observed_at'), 'Source branch capture'), 'Source branches', earliest=earliest)
    require(actual_branches == branches['data'], 'Source branch list must derive from every raw API page')
    branch_heads = {}
    for branch in actual_branches:
        name, sha = branch.get('name'), branch.get('commit', {}).get('sha')
        require(text(name) and name not in branch_heads and immutable_sha(sha),
                'Source branches need unique names and immutable heads')
        branch_heads[name] = sha
    tags = source_tag_heads(row, repo, earliest)
    ref = row.get('ref_response', {})
    require(ref.get('request_url') == base + '/git/ref/heads/' + repo['default_branch'],
            'Source default head needs its exact Git reference GET')
    if not branch_heads:
        require(repo['id'] == 996911586 and row.get('state') == 'uninitialized' and
                ref.get('http_status') in {404, 409} and row.get('head_sha') is None and row.get('tree_sha') is None,
                'Only the sealed uninitialized repository may have no source revision')
        require(not tags, 'Uninitialized source cannot hide an existing tag namespace')
        return None, None, branch_heads, tags
    head, tree = row.get('head_sha'), row.get('tree_sha')
    commit = row.get('commit_response', {})
    require(immutable_sha(head) and immutable_sha(tree) and row.get('state') == 'observed' and
            branch_heads.get(repo['default_branch']) == head and ref.get('http_status') == 200 and
            ref.get('data', {}).get('ref') == 'refs/heads/' + repo['default_branch'] and
            ref['data'].get('object', {}).get('type') == 'commit' and
            ref['data']['object'].get('sha') == head and
            commit.get('request_url') == base + '/git/commits/' + head and commit.get('http_status') == 200 and
            commit.get('data', {}).get('sha') == head and commit['data'].get('tree', {}).get('sha') == tree,
            'Source default branch must match its immutable commit and tree')
    return head, tree, branch_heads, tags


def validate_reference_scan(reference, directory, inventory, latest_at=None):
    scan = local_capture(reference, directory, 'Fresh complete source reference scan')
    timestamp = observed_time(scan.get('observed_at'), 'Fresh source scan')
    expected = {repo['id']: repo for repo in inventory['repositories']}
    rows = scan.get('repositories')
    require(scan.get('phase') == 'pre_cutover' and isinstance(rows, list) and len(rows) == len(expected),
            'Fresh reference scan must cover every sealed repository')
    revisions, manifests = {}, {}
    unused = local_capture('legacy-unused-credential-owner-evidence.json', directory, 'Unused legacy credential scope')
    for row in rows:
        repo_id = row.get('repository_id')
        require(repo_id in expected and repo_id not in revisions, 'Fresh source scan has unexpected or duplicate IDs')
        repo = expected[repo_id]
        require(row.get('source') == repo['full_name'] and
                observed_time(row.get('observed_at'), 'Repository reference scan') <= timestamp,
                'Fresh reference scan needs exact source identities and capture times')
        revision = source_revision(row, repo)
        revisions[repo_id] = revision
        if revision[0] is None:
            require(row.get('files') == [], 'Uninitialized source cannot claim scanned files')
            continue
        branch_scans = row.get('branch_scans')
        other_heads = (set(revision[2].values()) | {tag['commit_sha'] for tag in revision[3].values() if tag['commit_sha']}) - {revision[0]}
        require(isinstance(branch_scans, list) and len(branch_scans) == len(other_heads) and
                {branch.get('head_sha') for branch in branch_scans} == other_heads,
                'Fresh scan must inspect every distinct non-default branch and tagged commit')
        references, unresolved_secret_scope = set(), False
        waived = next((r['unused_repository_secret_names'] for r in unused['repositories'] if r['repository_id'] == repo_id), [])
        conflicts = []
        current_heads = set(revision[2].values())
        for branch in [row] + branch_scans:
            head, tree_sha = branch.get('head_sha'), branch.get('tree_sha')
            commit = branch.get('commit_response', {})
            require(immutable_sha(tree_sha) and commit.get('http_status') == 200 and
                    commit.get('request_url') == 'https://api.github.com/repos/' + repo['full_name'] + '/git/commits/' + head and
                    commit.get('data', {}).get('sha') == head and commit['data'].get('tree', {}).get('sha') == tree_sha,
                    'Scanned branch must match its immutable commit and tree')
            branch_references, branch_unresolved, branch_manifests = validate_branch_reference_files(
                branch, repo, observed_time(row['observed_at'], 'Repository reference scan'),
                require_consistent_manifests=head in current_heads)
            references.update(branch_references)
            unresolved_secret_scope |= branch_unresolved
            if branch_references.intersection(name.upper() for name in waived):
                conflicts.append(branch)
            if head == revision[0]:
                manifests.update({repo_id: manifest for manifest in branch_manifests})
        require({name.upper() for name in row.get('referenced_secret_names', [])} == references,
                'Fresh credential reference conclusions must derive from every captured workflow')
        require(not waived or not unresolved_secret_scope,
                'Inherited or dynamic secret scope needs reconciliation before an unused-credential waiver')
        if conflicts:
            validate_inactive_historical_references(row, repo, revision, conflicts, waived, directory, timestamp, latest_at=latest_at)
    return revisions, timestamp, manifests


def secret_references(source):
    names = {name.upper() for name in re.findall(r'secrets\s*\.\s*([A-Za-z_][A-Za-z0-9_]*)', source, re.I)}
    names.update(name.upper() for name in re.findall(r"secrets\s*\[\s*['\"]([^'\"]+)['\"]\s*\]", source, re.I))
    return names


def validate_inactive_historical_references(row, repo, revision, conflicts, waived, directory, scanned_at, latest_at=None):
    """Reconcile only the reviewed retired gh-x bytes, never current branch references."""
    require(repo['id'] == 1262580000 and repo['full_name'] == 'HemSoft/gh-x' and
            set(waived) == {'OPENROUTER_API_KEY', 'SFL_APP_PRIVATE_KEY'},
            'A newly referenced legacy credential needs reconciliation and fresh validation')
    catalog_path = directory / 'historical-workflow-reconciliation.json'
    require(catalog_path.is_file() and hashlib.sha256(catalog_path.read_bytes()).hexdigest() == GHX_HISTORY_CATALOG_SHA256,
            'Historical credential treatment must match the reviewed immutable catalog')
    catalog = json.loads(catalog_path.read_bytes())
    require(catalog.get('repository_id') == repo['id'] and catalog.get('repository') == repo['full_name'] and
            set(catalog.get('secret_names', [])) == set(waived) and catalog.get('owner_unused_receipt') == LEGACY_UNUSED_RECEIPT,
            'Historical reconciliation needs its exact repository and existing owner-unused scope')
    current_heads = set(revision[2].values())
    allowed_heads = set(catalog['tag_only_revisions'])
    require(all(branch['head_sha'] not in current_heads and branch['head_sha'] in allowed_heads for branch in conflicts),
            'Unused credentials cannot be referenced on any current branch or an unreconciled tagged revision')
    allowed = {(entry['path'], entry['blob_sha']): entry for entry in catalog['blobs']}
    retired_paths = {entry['path'] for entry in catalog['blobs'] if entry['path'].startswith('.github/workflows/')}
    for branch in [row] + row['branch_scans']:
        if branch['head_sha'] in current_heads:
            require(not retired_paths.intersection(file['path'] for file in branch['files']),
                    'A restored historical SFL workflow invalidates its inactive credential treatment')
    for branch in conflicts:
        for file in branch['files']:
            body = immutable_contents(file, repo['id'], repo['full_name'], branch['head_sha'], file['path'])
            if not secret_references(body.decode('utf-8')).intersection(waived):
                continue
            entry = allowed.get((file['path'], file['contents_response']['data']['sha']))
            require(entry is not None and hashlib.sha256(body).hexdigest() == entry['sha256'],
                    'Historical credential reference differs from its reviewed exact bytes')

    capture = local_capture(row.get('inactive_historical_workflows_evidence_url'), directory, 'Fresh inactive workflow metadata')
    at = observed_time(capture.get('observed_at'), 'Inactive workflow metadata')
    require(capture.get('phase') == 'pre_cutover' and capture.get('repository_id') == repo['id'] and
            capture.get('repository') == repo['full_name'] and capture.get('head_sha') == revision[0] and at >= scanned_at and (latest_at is None or at <= latest_at),
            'Inactive workflow evidence must follow the complete scan at its current source head and precede source refresh')
    base = 'https://api.github.com/repos/' + repo['full_name']
    for name, url in [('metadata_response', base),
                      ('default_ref_response', base + '/git/ref/heads/' + urllib.parse.quote(repo['default_branch'], safe=''))]:
        response = capture.get(name, {})
        validate_resource_response(response, url, response.get('data'))
        require(scanned_at <= observed_time(response.get('observed_at'), 'Inactive workflow identity') <= at,
                'Inactive workflow identity GETs must follow the complete scan')
    require(capture['metadata_response']['data'].get('id') == repo['id'] and
            capture['metadata_response']['data'].get('full_name') == repo['full_name'] and
            capture['default_ref_response']['data'].get('ref') == 'refs/heads/' + repo['default_branch'] and
            capture['default_ref_response']['data'].get('object', {}).get('sha') == revision[0],
            'Inactive workflow metadata needs the independently captured source repository and head')
    workflows = validate_raw_page_chain(capture.get('workflow_pages'), base + '/actions/workflows?per_page=100',
        at, 'Current registered workflows', field='workflows', earliest=scanned_at)
    require(not retired_paths.intersection(workflow.get('path') for workflow in workflows),
            'A registered historical workflow invalidates its inactive credential treatment')
    expected = {'.github/workflows/sfl-pr-review-auto.yml': 337272622,
                '.github/workflows/sfl-pr-review.lock.yml': 325052102,
                '.github/workflows/sfl-pr-review-recovery.yml': None}
    responses = capture.get('historical_workflow_responses')
    require(isinstance(responses, dict) and set(responses) == set(expected),
            'Inactive history needs each exact retired workflow GET')
    for path, workflow_id in expected.items():
        response = responses[path]
        require(response.get('method') == 'GET' and response.get('request_url') ==
                base + '/actions/workflows/' + path.rsplit('/', 1)[1] and
                scanned_at <= observed_time(response.get('observed_at'), 'Retired workflow GET') <= at,
                'Retired workflow GET must be fresh and resource-bound')
        data = response.get('data', {})
        if workflow_id is None:
            require(response.get('http_status') == 404 and data.get('message') == 'Not Found',
                    'Retired recovery workflow must be actually unavailable')
        else:
            require(response.get('http_status') == 200 and data.get('id') == workflow_id and
                    data.get('path') == path and data.get('state') == 'deleted',
                    'Historical SFL workflow must remain deleted at its exact observed identity')


def validate_branch_reference_files(branch, repo, captured_at, require_consistent_manifests=True):
    repo_id = repo['id']
    head, tree_sha = branch['head_sha'], branch['tree_sha']
    tree = branch.get('tree_response', {})
    empty_tree = tree_sha == hashlib.sha1(b'tree 0\0').hexdigest() and tree.get('http_status') == 404 and \
        tree.get('data', {}).get('message') == 'Not Found'
    require(tree.get('request_url') == 'https://api.github.com/repos/' + repo['full_name'] +
            '/git/trees/' + tree_sha + '?recursive=1' and (empty_tree or
            (tree.get('http_status') == 200 and tree.get('data', {}).get('sha') == tree_sha and
             tree['data'].get('truncated') is False and isinstance(tree['data'].get('tree'), list))),
            'Fresh reference scan needs the complete immutable Git tree or its committed canonical empty tree')
    paths = {}
    for entry in ([] if empty_tree else tree['data']['tree']):
        path = entry.get('path')
        require(text(path) and path not in paths, 'Fresh source tree paths must be unique')
        paths[path] = entry
    relevant = {p for p, entry in paths.items() if entry.get('type') == 'blob' and
                ((p.startswith('.github/workflows/') and p.endswith(('.yml', '.yaml', '.md'))) or
                 p in {'.sfl/sfl.json', 'sfl.json', 'vercel.json', 'fly.toml', 'railway.json',
                       'railway.toml', 'wrangler.toml', 'wrangler.json', 'wrangler.jsonc',
                       'azure-pipelines.yml', 'azure-pipelines.yaml', 'supabase/config.toml'})}
    files = branch.get('files')
    require(isinstance(files, list) and len(files) == len(relevant) and
            {f.get('path') for f in files} == relevant, 'Fresh scan must read every workflow and installation manifest')
    references = set()
    unresolved_secret_scope = False
    manifests = []
    for file in files:
        path = file['path']
        body = immutable_contents(file, repo_id, repo['full_name'], head, path)
        require(file['contents_response']['data']['sha'] == paths[path].get('sha') and
                observed_time(file.get('observed_at'), 'Source file capture') <=
                captured_at,
                'Scanned file must match the complete Git tree blob')
        source = body.decode('utf-8')
        references.update(secret_references(source))
        unresolved_secret_scope |= bool(re.search(r"(?im)^\s*secrets\s*:\s*(['\"]?)inherit\1\s*(?:#.*)?$", source))
        unresolved_secret_scope |= any(re.fullmatch(r"\s*['\"][A-Za-z_][A-Za-z0-9_]*['\"]\s*", match[1]) is None
            for match in re.finditer(r'\bsecrets\s*\[([^\]]*)\]', source, re.I))
        unresolved_secret_scope |= any(re.search(r'\bsecrets\b(?!\s*(?:\.[A-Za-z_]|\[))', expression, re.I)
            for expression in re.findall(r'\$\{\{(.*?)\}\}', source, re.S))
        if path in {'.sfl/sfl.json', 'sfl.json'}:
            require(file.get('manifest') == json.loads(body), 'Fresh installation manifest must derive from immutable bytes')
            require(not require_consistent_manifests or not manifests or manifests[0] == file['manifest'],
                    'Fresh canonical and legacy installation manifests disagree')
            manifests.append(file['manifest'])
    return references, unresolved_secret_scope, manifests


def reviewed_environment_secrets(repo, directory):
    historical = local_capture('runtime-metadata.json', directory, 'Reviewed environment inventory')
    entries = [row for row in historical['repositories'] if row.get('source') == repo['full_name']]
    require(len(entries) == 1 and isinstance(entries[0].get('environments'), list),
            'Every source needs a reviewed environment inventory')
    expected = {}
    for environment in entries[0]['environments']:
        name, secrets = environment.get('name'), environment.get('secret_names', {})
        names = secrets.get('data')
        require(text(name) and name not in expected and secrets.get('state') == 'observed' and
                string_list(names) and len(names) == len(set(names)),
                'Reviewed environments need unique names and observed secret-name inventories')
        expected[name] = set(names)
    refreshed = local_capture('runtime-refresh-evidence.json', directory, 'Reviewed environment secret refresh')
    seen = set()
    for record in refreshed['records']:
        if not (record.get('repository_id') == repo['id'] or record.get('repository') == repo['full_name']):
            continue
        kind = record.get('kind', '')
        if not kind.startswith('environment_secrets:'):
            continue
        name, data = kind.split(':', 1)[1], record.get('data', {})
        names = data.get('names')
        require(text(name) and name not in seen and record.get('state') == 'observed' and
                string_list(names) and len(names) == len(set(names)) and
                type(data.get('total_count')) is int and data['total_count'] == len(names),
                'Reviewed environment refresh needs complete unique observed secret names')
        seen.add(name)
        expected[name] = set(names)
    return expected


def validate_environment_secret_refresh(current, repo, directory, timestamp, scanned_at):
    base = 'https://api.github.com/repos/' + repo['full_name']
    environments = validate_raw_page_chain(current.get('environment_pages'), base + '/environments?per_page=100',
        timestamp, 'Fresh environment inventory', field='environments', earliest=scanned_at)
    names = [environment.get('name') for environment in environments]
    expected = reviewed_environment_secrets(repo, directory)
    require(string_list(names) and len(names) == len(set(names)) and set(names) == set(expected),
            'Current environment names changed; reconcile credential scopes before cutover')
    pages = current.get('environment_secret_pages')
    require(isinstance(pages, dict) and set(pages) == set(names),
            'Every current environment needs its complete fresh secret-name GET pages')
    for name in names:
        secrets = validate_raw_page_chain(pages[name], base + '/environments/' +
            urllib.parse.quote(name, safe='') + '/secrets?per_page=100', timestamp,
            'Fresh environment secret names', field='secrets', earliest=scanned_at)
        actual = [secret.get('name') for secret in secrets]
        require(string_list(actual) and len(actual) == len(set(actual)) and set(actual) == expected[name],
                'Current environment secret names changed; reconcile credentials and owner waivers before cutover')


def validate_source_refresh(proof, directory, inventory, credential):
    capture = local_capture(proof, directory, 'Pre-cutover source refresh')
    require(capture.get('phase') == 'pre_cutover', 'Source refresh must precede cutover')
    timestamp = observed_time(capture.get('observed_at'), 'Source refresh')
    metadata = local_capture(credential.get('credential_metadata_evidence_url'), directory, 'App credential metadata')
    require(timestamp >= observed_time(metadata.get('observed_at'), 'App credential metadata'),
            'Source refresh must follow the credential check')
    credential_run = local_capture(credential.get('workflow_run_evidence_url'), directory, 'App credential workflow run')
    require(timestamp >= observed_time(credential_run.get('captured_at'), 'App credential run capture'),
            'Source refresh must follow the captured completed credential workflow')
    expected = {r['id']: r for r in inventory['repositories']}
    revisions, scanned_at, manifests = validate_reference_scan(capture.get('reference_scan_evidence_url'), directory, inventory, latest_at=timestamp)
    run = credential_run.get('run', credential_run)
    require(scanned_at <= observed_time(run.get('created_at'), 'Fresh credential run creation') and
            timestamp - observed_time(run.get('updated_at'), 'Fresh credential run completion') <= datetime.timedelta(minutes=15),
            'Pre-cutover credentials need a new successful run after the fresh scan and within 15 minutes of source refresh')
    accounts = capture.get('accounts')
    require(isinstance(accounts, list) and len(accounts) == 2 and
            {a.get('owner') for a in accounts} == {'HemSoft', 'fhemmer'}, 'Source refresh must enumerate both owners')
    actual = {}
    for account in accounts:
        require(account.get('all_pages') is True and account.get('state') == 'observed' and
                isinstance(account.get('repositories'), list), 'Source refresh needs complete owner enumerations')
        validate_repository_enumeration(account, timestamp, scanned_at)
        for current in account['repositories']:
            repo_id = current.get('id')
            require(repo_id in expected and repo_id not in actual, 'Source refresh contains a new or duplicate repository')
            baseline = expected[repo_id]
            require(current.get('full_name', '').startswith(account['owner'] + '/') and
                    all(current.get(k) == baseline[k] for k in ('full_name','private','visibility','archived','default_branch')),
                    'Source repository metadata changed; reconcile the transfer baseline')
            head = current.get('source_head', {})
            require(scanned_at <= observed_time(head.get('observed_at'), 'Current source head') <= timestamp and
                    source_revision(head, baseline, scanned_at) == revisions[repo_id],
                    'Source head, tree, branch or tag refs changed after reference scan; rescan and reconcile before transfer')
            policy = local_capture(current.get('protection_evidence_url'), directory, 'Fresh source protection')
            policy_at = observed_time(policy.get('observed_at'), 'Fresh source protection')
            require(policy.get('phase') == 'pre_cutover' and policy.get('repository_id') == repo_id and
                    policy.get('repository') == baseline['full_name'] and policy.get('revision_sha') == head.get('head_sha') and
                    scanned_at <= policy_at <= timestamp,
                    'Fresh source policy must bind its repository revision and current refresh boundary')
            actual_policy = validate_protection_responses(policy, baseline, baseline['full_name'], head.get('head_sha'), policy_at, scanned_at,
                                                         directory=directory)
            require(current.get('protections') == actual_policy == protection_contract(baseline, directory),
                    'Source protections must derive from complete current primary GETs and match the reviewed preservation baseline')
            secret_rows = validate_raw_page_chain(current.get('secret_pages'),
                'https://api.github.com/repos/' + baseline['full_name'] + '/actions/secrets?per_page=100',
                timestamp, 'Fresh repository secret names', field='secrets', earliest=scanned_at)
            names = [secret.get('name') for secret in secret_rows]
            require(string_list(names) and len(names) == len(set(names)) and
                    baseline['settings']['secret_names']['state'] == 'observed' and
                    set(names) == set(baseline['settings']['secret_names']['data']),
                    'Current repository secret names changed; reconcile credentials and owner waivers before cutover')
            validate_environment_secret_refresh(current, baseline, directory, timestamp, scanned_at)
            runner_rows = validate_raw_page_chain(current.get('runner_pages'),
                'https://api.github.com/repos/' + baseline['full_name'] + '/actions/runners?per_page=100',
                timestamp, 'Fresh repository runner inventory', field='runners', earliest=scanned_at)
            runner_ids = [runner.get('id') for runner in runner_rows]
            historical = local_capture('runtime-metadata.json', directory, 'Historical runners')
            refreshed = local_capture('runtime-refresh-evidence.json', directory, 'Reviewed runner inventory')
            expected_runners = {runner['id'] for repo in historical['repositories'] if repo['source'] == baseline['full_name']
                for runner in (repo['repository_runners'].get('data') or [])}
            expected_runners.update(runner['id'] for record in refreshed['records'] if
                (record.get('repository_id') == repo_id or record.get('repository') == baseline['full_name']) and
                record['kind'] == 'runners' and record.get('state') == 'observed'
                for runner in record.get('data', {}).get('runners', []))
            if repo_id == SURVIVAL_REPOSITORY_ID:
                survival = reviewed_survival_resources(baseline, directory)
                scan = local_capture(capture['reference_scan_evidence_url'], directory, 'Fresh complete source reference scan')
                scanned_repository = next(row for row in scan['repositories'] if row['repository_id'] == repo_id)
                workflows = [file for file in scanned_repository['files'] if file['path'] == '.github/workflows/ci.yml']
                require(len(workflows) == 1 and
                        immutable_contents(workflows[0], repo_id, baseline['full_name'], head['head_sha'],
                                           '.github/workflows/ci.yml') == survival['workflow_bytes'],
                        'Current Windows workflow must match its reviewed pinned bytes before cutover')
                expected_runners.add(survival['runner']['id'])
                require(scanned_at >= survival['observed_at'],
                        'Source scan must follow the reviewed current Windows resource observations')
                actual_runner = next((runner for runner in runner_rows if runner.get('id') == 10), {})
                require(actual_runner.get('name') == survival['runner']['name'] and actual_runner.get('os') == 'Windows' and
                        actual_runner.get('status') == 'online' and actual_runner.get('busy') is False and
                        {label.get('name') for label in actual_runner.get('labels', [])} ==
                            {label['name'] for label in survival['runner']['labels']},
                        'Current Windows runner must preserve its online idle identity and labels')
                variable = current.get('variable_responses', {}).get('UE_RUNNER_ENABLED', {})
                validate_resource_response(variable, 'https://api.github.com/repos/' + baseline['full_name'] +
                                           '/actions/variables/UE_RUNNER_ENABLED', variable.get('data'))
                require(variable['data'].get('name') == 'UE_RUNNER_ENABLED' and variable['data'].get('value') == 'true' and
                        scanned_at <= observed_time(variable.get('observed_at'), 'Fresh runner variable') <= timestamp,
                        'Current Windows CI needs its freshly preserved enabled runner variable')
            require(all(type(runner_id) is int and runner_id > 0 for runner_id in runner_ids) and
                    len(runner_ids) == len(set(runner_ids)) and set(runner_ids) == expected_runners,
                    'Current repository runner registrations changed; reconcile the runner ledger before cutover')
            if repo_id == 1169772257:
                require(head.get('head_sha') == credential['reviewed_sha'] == run.get('head_sha'),
                        'Fresh credential proof must execute the current source main revision')
            actual[repo_id] = current
    require(set(actual) == set(expected), 'Source refresh must cover every sealed repository ID')
    destination = capture.get('destination_account', {})
    login = inventory['destination_login']
    require(destination.get('owner') == login and destination.get('state') == 'observed' and
            destination.get('all_pages') is True and isinstance(destination.get('repositories'), list) and
            destination.get('request_url') == 'https://api.github.com/orgs/' + login + '/repos?type=all&per_page=100',
            'Source refresh must include a complete destination repository enumeration')
    validate_repository_enumeration(destination, timestamp, scanned_at)
    mapped_names = {repo['destination'].casefold() for repo in expected.values()
                    if repo['id'] not in APPROVED_RETAINED_IDS}
    destination_ids, destination_names = set(), set()
    for current in destination['repositories']:
        repo_id, name = current.get('id'), current.get('full_name')
        require(type(repo_id) is int and repo_id > 0 and repo_id not in destination_ids and
                repo_id not in expected and text(name) and name.startswith(login + '/') and
                name.casefold() not in destination_names,
                'Destination refresh identities must be unique, account-bound and separate from source IDs')
        require(name.casefold() not in mapped_names,
                'Destination refresh contains a newly occupied mapped transfer name')
        destination_ids.add(repo_id)
        destination_names.add(name.casefold())
    app = capture.get('owned_app', {})
    require(app.get('id') == 4448946 and app.get('client_id') == 'Iv23liwvwJJUh2bUIKLW' and
            app.get('owner') == {'login':'HemSoft','type':'User'} and
            app.get('permissions') == inventory['known_owned_app']['data']['permissions'] and
            capture.get('source_installation') == {'id':150383874,'app_id':4448946,'owner':'HemSoft','repository_selection':'all'} and
            capture.get('source_organization_installations') == inventory['source_organization_apps'],
            'Source refresh must reconcile owned and installed App settings')
    return timestamp, scanned_at, manifests


def ledger_digest(rows):
    return hashlib.sha256(json.dumps(rows, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def validate_tree_refresh(reference, directory, inventory, source_refreshed_at):
    original = json.loads((directory / 'source-tree-recheck-evidence.json').read_text())
    unresolved = {row['repository_id']: row for row in original['records']}
    capture = local_capture(reference, directory, 'Pre-cutover source tree refresh')
    require(capture.get('phase') == 'pre_cutover' and isinstance(capture.get('records'), list) and
            len(capture['records']) == len(unresolved) and
            {row.get('repository_id') for row in capture['records']} == set(unresolved),
            'Source tree refresh must resolve every originally unavailable tree exactly once')
    timestamp = observed_time(capture.get('observed_at'), 'Source tree refresh')
    require(timestamp >= source_refreshed_at, 'Source tree refresh must follow the complete source refresh')
    expected = {repo['id']: repo for repo in inventory['repositories']}
    for row in capture['records']:
        repo = expected[row['repository_id']]
        baseline = unresolved[row['repository_id']]
        observed = observed_time(row.get('observed_at'), 'Source tree observation')
        require(source_refreshed_at <= observed <= timestamp,
                'Each source branch/head recheck must follow source refresh and precede its completed capture')
        metadata = row.get('metadata', {})
        branches = row.get('branches_response', {})
        require(row.get('source') == repo['full_name'] and row.get('state') == baseline['state'] and
                row.get('metadata_request_url') == 'https://api.github.com/repos/' + repo['full_name'] and
                row.get('metadata_http_status') == 200 and metadata.get('id') == repo['id'] and
                metadata.get('full_name') == repo['full_name'] and metadata.get('size') == 0 and
                metadata.get('default_branch') == repo['default_branch'] and
                branches.get('request_url') == 'https://api.github.com/repos/' + repo['full_name'] + '/branches?per_page=100' and
                branches.get('http_status') == 200 and branches.get('all_pages') is True and
                branches.get('data') == baseline['branches'],
                'Source tree recheck must bind current canonical metadata and unchanged complete branches')
        raw_branches = validate_raw_page_chain(branches.get('pages'), branches['request_url'], timestamp,
            'Source tree branches', earliest=source_refreshed_at)
        require(raw_branches == branches['data'], 'Source tree branch list must derive from raw pages')
        if baseline['state'] == 'empty_tree':
            commit = row.get('commit_response', {})
            require(commit.get('request_url') == 'https://api.github.com/repos/' + repo['full_name'] +
                    '/git/commits/' + baseline['commit_sha'] and commit.get('http_status') == 200 and
                    commit.get('data', {}).get('sha') == baseline['commit_sha'] and
                    commit['data'].get('tree', {}).get('sha') == '4b825dc642cb6eb9a060e54bf8d69288fbee4904' and
                    len(branches['data']) == 1 and branches['data'][0]['commit']['sha'] == baseline['commit_sha'],
                    'Empty source tree needs its unchanged current branch head and raw empty-tree Git commit')
        else:
            require(baseline['state'] == 'uninitialized' and branches['data'] == [],
                    'Uninitialized source must still have no current branches')
    return timestamp


def validate_ledger_readiness(reference, current_rows, directory, context, source_refreshed_at, scanned_at):
    capture = local_capture(reference, directory, 'Pre-cutover ledger readiness')
    timestamp = observed_time(capture.get('observed_at'), 'Ledger readiness')
    rows = capture.get('rows')
    require(capture.get('phase') == 'pre_cutover' and capture.get('verified_by') == 'HemSoft' and
            capture.get('organization_id') == 338855369 and isinstance(rows, list) and
            capture.get('ledger_sha256') == ledger_digest(rows) and timestamp <= source_refreshed_at,
            'Ledger readiness must preserve an owner-verified snapshot before source refresh and App transfer')
    statuses, _ = validate_ledger_rows(rows, *context)
    retained = context[3]
    require(all(all(status == 'verified' for status in values) for repo_id, values in statuses.items() if repo_id not in retained),
            'Immutable pre-cutover ledger must verify every transfer-target gate')
    changing = {'status', 'verified_at', 'evidence_url', 'smoke_outcome', 'smoke_phase', 'smoke_evidence_url'}
    def identities(values):
        return sorted(json.dumps({k:v for k,v in row.items() if k not in changing}, sort_keys=True) for row in values)
    require(identities(rows) == identities(current_rows),
            'Pre-cutover ledger must retain the current provider, resource, credential and transfer identities')
    pinned = {}
    for row in rows:
        if row['status'] != 'verified':
            continue
        require(scanned_at <= observed_time(row['verified_at'], 'Ledger verification') <= timestamp,
                'Ledger gates must be reconciled after the fresh source scan and before the readiness snapshot')
        if row['provider'] != 'none':
            require(row.get('smoke_phase') == 'pre_transfer', 'Ledger readiness needs pre-transfer provider evidence')
        for key, reference in row.items():
            if key.endswith('evidence_url') and text(reference) and not urllib.parse.urlsplit(reference).scheme:
                evidence(reference, directory)
                path = directory / reference
                pinned[reference] = hashlib.sha256(path.read_bytes()).hexdigest()
                data = json.loads(path.read_text())
                for field in ('observed_at', 'confirmed_at', 'approved_at'):
                    if data.get(field):
                        require(observed_time(data[field], 'Readiness evidence') <= timestamp,
                                'Ledger readiness must follow all pinned pre-transfer evidence')
    require(capture.get('evidence_sha256') == pinned,
            'Immutable ledger readiness must pin every local pre-transfer evidence capture')
    return timestamp


def validate_app_transfer(proof, directory, inventory, earliest):
    before = local_capture(proof.get('pre_transfer_owner_evidence_url'), directory, 'Pre-transfer App owner')
    before_at = observed_time(before.get('observed_at'), 'Pre-transfer App owner')
    app_before = before.get('app', {})
    require(before.get('phase') == 'pre_transfer' and before_at > earliest and
            before.get('request_url') == 'https://api.github.com/apps/sfl-app' and before.get('http_status') == 200 and
            app_before.get('id') == 4448946 and app_before.get('client_id') == 'Iv23liwvwJJUh2bUIKLW' and
            app_before.get('owner', {}).get('login') == 'HemSoft' and app_before['owner'].get('type') == 'User' and
            app_before['owner'].get('id') == 8227352 and
            app_before.get('permissions') == inventory['known_owned_app']['data']['permissions'],
            'App transfer needs a still-personally-owned registration GET after every pre-transfer gate')
    capture = local_capture(proof.get('evidence_url'), directory, 'App ownership transfer')
    timestamp = observed_time(capture.get('observed_at'), 'App ownership transfer')
    app = capture.get('app', {})
    require(capture.get('phase') == 'post_transfer' and timestamp > before_at and
            capture.get('request_url') == 'https://api.github.com/apps/sfl-app' and capture.get('http_status') == 200 and
            app.get('id') == 4448946 and app.get('client_id') == 'Iv23liwvwJJUh2bUIKLW' and
            app.get('owner', {}).get('id') == 338855369 and app['owner'].get('login') == 'hemsoft-dev' and
            app['owner'].get('type') == 'Organization' and
            app.get('permissions') == inventory['known_owned_app']['data']['permissions'],
            'App ownership transfer needs post-transfer registration metadata with the original permission ceiling')
    return timestamp


def validate_post_transfer_access(access, directory, repo, earliest):
    permission = local_capture(access.get('permission_evidence_url'), directory, 'Post-transfer permission')
    license = local_capture(access.get('license_evidence_url'), directory, 'Post-transfer license')
    require(permission.get('phase') == 'post_transfer' and permission.get('repository_id') == repo['id'] and
            permission.get('repository') == repo['destination'] and permission.get('account') == 'fhemmerrelias' and
            permission.get('effective_permission') == 'none' and
            permission.get('request_url') == 'https://api.github.com/repos/' + repo['destination'] +
            '/collaborators/fhemmerrelias/permission' and permission.get('http_status') == 200 and
            permission.get('result',{}).get('permission') == 'none' and
            permission['result'].get('user', {}).get('login') == 'fhemmerrelias',
            'Post-transfer permission capture must prove the specified account has no access')
    require(license.get('phase') == 'post_transfer' and license.get('organization_id') == 338855369 and
            license.get('organization') == 'hemsoft-dev' and
            license.get('request_url') == 'https://api.github.com/orgs/hemsoft-dev' and license.get('http_status') == 200 and
            license.get('data', {}).get('id') == 338855369 and license['data'].get('login') == 'hemsoft-dev' and
            license.get('plan') == {'name':'team','filled_seats':1,'seats':1} and
            all(license['data'].get('plan', {}).get(k) == v for k, v in license['plan'].items()),
            'Post-transfer license capture must prove the existing one-seat organization plan')
    for capture in (permission, license):
        require(observed_time(capture.get('observed_at'), 'Post-transfer access') > earliest and
                observed_time(capture['observed_at'], 'Post-transfer access') <=
                observed_time(access.get('verified_at'), 'Access verification'),
                'Access and license captures must follow repository transfer and precede verification')


def validate_source_governance(proof, directory, repo, cutover):
    capture = local_capture(proof.get('governance_evidence_url'), directory, 'Protected source governance')
    require(capture.get('phase') == 'post_transfer' and capture.get('repository_id') == repo['id'] and
            capture.get('repository') == repo['destination'] and capture.get('revision_sha') == proof['source_sha'],
            'Source governance capture must match its transferred repository and release revision')
    observed_at = observed_time(capture.get('observed_at'), 'Source governance')
    require(observed_at > cutover,
            'Source governance must be captured after App cutover')
    base = 'https://api.github.com/repos/' + repo['destination']
    for field, suffix in (('actions_policy', '/actions/permissions'),
                          ('workflow_permissions', '/actions/permissions/workflow')):
        response = capture.get(field + '_response', {})
        require(response.get('method') == 'GET' and response.get('http_status') == 200 and
                response.get('request_url') == base + suffix and response.get('data') == capture.get(field) and
                cutover < observed_time(response.get('observed_at'), 'Source governance GET') <= observed_at,
                'Source governance policy must derive from successful repository-specific API responses')
    raw_labels = validate_raw_page_chain(capture.get('labels_pages'), base + '/labels?per_page=100',
        observed_at, 'Source governance labels', earliest=cutover)
    require(raw_labels == capture.get('labels'), 'Source governance labels must derive from complete raw API pages')
    codeowners = capture.get('codeowners_contents', {})
    content = immutable_contents(codeowners, repo['id'], repo['destination'], proof['source_sha'], '.github/CODEOWNERS')
    require(content.decode('utf-8') == capture.get('codeowners') and
            cutover < observed_time(codeowners.get('observed_at'), 'Source CODEOWNERS GET') <= observed_at,
            'Source CODEOWNERS must derive from current revision-bound repository contents')
    root = pathlib.Path(__file__).resolve().parents[2]
    labels = json.loads((root / 'deployment/governance/labels.json').read_text())
    actual = capture.get('labels')
    require(isinstance(actual, list), 'Source governance needs captured labels')
    by_name = {label.get('name'):label for label in actual}
    require(all(all(by_name.get(label['name'],{}).get(field) == label[field]
                    for field in ('name','color','description')) for label in labels),
            'Source governance must preserve every authoritative label')
    require(capture.get('codeowners') == (root / 'deployment/governance/CODEOWNERS').read_text() and
            capture.get('actions_policy') == repo['settings']['actions_policy'].get('data') and
            capture.get('workflow_permissions') == repo['settings']['workflow_permissions'].get('data'),
            'Source governance must match authoritative CODEOWNERS and baseline Actions policies')


def validate_source_default_head(proof, directory, repo, latest):
    capture = local_capture(proof.get('default_branch_evidence_url'), directory, 'Source default-branch head')
    require(capture.get('phase') == 'post_transfer' and capture.get('repository_id') == repo['id'] and
            capture.get('repository') == repo['destination'] and capture.get('branch') == repo['default_branch'] and
            capture.get('http_status') == 200 and capture.get('request_url') ==
                'https://api.github.com/repos/' + repo['destination'] + '/git/ref/heads/' + repo['default_branch'] and
            capture.get('data', {}).get('ref') == 'refs/heads/' + repo['default_branch'] and
            capture['data'].get('object', {}).get('type') == 'commit' and
            capture['data']['object'].get('sha') == proof['source_sha'],
            'Protected source proof must match its independently captured current default-branch head')
    require(observed_time(capture.get('observed_at'), 'Source current head') >= latest,
            'Source default-branch head capture must follow all validated source evidence')


def validate_consumer_default_head(row, directory, repo, revision, latest):
    capture = local_capture(row.get('default_branch_evidence_url'), directory, 'Consumer default-branch head')
    require(capture.get('phase') == 'post_transfer' and capture.get('repository_id') == repo['id'] and
            capture.get('repository') == repo['destination'] and capture.get('branch') == repo['default_branch'] and
            capture.get('http_status') == 200 and capture.get('method') == 'GET' and capture.get('request_url') ==
                'https://api.github.com/repos/' + repo['destination'] + '/git/ref/heads/' + repo['default_branch'] and
            capture.get('data', {}).get('ref') == 'refs/heads/' + repo['default_branch'] and
            capture['data'].get('object', {}).get('type') == 'commit' and
            capture['data']['object'].get('sha') == revision,
            'Consumer proof must match its independently captured current default-branch head')
    require(observed_time(capture.get('observed_at'), 'Consumer current head') >= latest,
            'Consumer default-branch head capture must follow all validated terminal evidence')


def validate_runner_captures(proof, directory, earliest):
    registration = local_capture(proof.get('registration_evidence_url'), directory, 'Destination runner')
    runner = registration.get('runner', {})
    windows = proof['repository_id'] == SURVIVAL_REPOSITORY_ID
    if windows:
        inventory = json.loads((directory / 'inventory.json').read_text())
        repo = next(repo for repo in inventory['repositories'] if repo['id'] == SURVIVAL_REPOSITORY_ID)
        survival = reviewed_survival_resources(repo, directory)
        baseline = survival['runner']
        fields = ('registration_evidence_url', 'startup_evidence_url', 'run_evidence_url')
        workflow_path, expected_workflow = '.github/workflows/ci.yml', survival['workflow_bytes']
        require(proof['repository'] == repo['destination'] and proof['runner_id'] == 10,
                'Windows continuity must bind the exact destination and preserved registration')
    else:
        require(proof['repository_id'] == 1188676172 and proof['runner_id'] == 21,
                'Linux runner continuity must preserve its sealed repository registration')
        sealed = local_capture('yahtzee-runner-owner-evidence.json', directory, 'Sealed runner')
        baseline = sealed['runner']
        fields = ('registration_evidence_url', 'isolation_evidence_url', 'service_evidence_url', 'run_evidence_url')
        workflow_path = '.github/workflows/self-hosted-smoke.yml'
        expected_workflow = (directory / 'yahtzee-smoke-workflow.yml').read_bytes()
    labels = {label['name'] if isinstance(label, dict) else label for label in runner.get('labels', [])}
    baseline_labels = {label['name'] if isinstance(label, dict) else label for label in baseline['labels']}
    require(runner.get('id') == baseline['id'] and runner.get('name') == baseline['name'] and
            labels == baseline_labels, 'Runner registration must preserve its sealed identity and labels')
    for field in fields:
        capture = local_capture(proof.get(field), directory, 'Destination runner')
        require(capture.get('phase') == 'post_transfer' and
                capture.get('repository_id') == proof['repository_id'] and capture.get('repository') == proof['repository'] and
                capture.get('runner_id') == proof['runner_id'] and
                observed_time(capture.get('observed_at'), 'Destination runner') >= earliest,
                'Runner captures must match the destination identity after actual destination/App cutover')
        if field == 'registration_evidence_url':
            require(capture.get('runner',{}).get('id') == proof['runner_id'] and
                    capture['runner'].get('status') == 'online' and capture['runner'].get('busy') is False,
                    'Runner registration capture must prove online and idle state')
            if windows:
                response = capture.get('runner_response', {})
                validate_resource_response(response, 'https://api.github.com/repos/' + proof['repository'] +
                                           '/actions/runners/10', runner)
                require(runner.get('os') == 'Windows' and earliest <=
                        observed_time(response.get('observed_at'), 'Windows runner GET') <=
                        observed_time(capture['observed_at'], 'Windows registration capture'),
                        'Windows registration must derive from its fresh destination runner GET')
        elif field == 'startup_evidence_url':
            validate_windows_startup(capture.get('host_capture', {}), capture.get('startup_capture', {}),
                                     repo, runner, observed_time(capture['observed_at'], 'Windows startup'), earliest)
        elif field == 'isolation_evidence_url':
            checks = capture.get('isolation_checks')
            require(capture.get('tailscale_present') is False and isinstance(checks,list) and len(checks) == 6 and
                    {c.get('target') for c in checks} == {'100.101.122.39:22','100.117.202.124:22','100.69.182.27:22',
                        '192.168.1.1:80','10.0.0.1:443','172.16.0.1:443'} and
                    all(c.get('blocked') is True for c in checks) and capture.get('public_dns_https') == 'passed',
                    'Runner isolation capture must preserve the six blocked private routes and absence of Tailscale')
        elif field == 'service_evidence_url':
            unit = sealed['guest']['service']
            require(capture.get('unit') == unit and capture.get('active_state') == 'active' and
                    capture.get('argv') == ['systemctl', 'show', unit, '--property=Id,ActiveState', '--no-pager'] and
                    capture.get('systemctl_show') == {'Id':unit, 'ActiveState':'active'},
                    'Runner service capture must prove the guest service is active')
        else:
            run = capture.get('run', {})
            require(run.get('repository',{}).get('id') == proof['repository_id'] and
                    run['repository'].get('full_name') == proof['repository'] and run.get('html_url') == proof['run_url'] and
                    run.get('head_sha') == proof['run_head_sha'] and run.get('status') == 'completed' and
                    run.get('conclusion') == 'success' and
                    (run.get('path') == workflow_path or (windows and
                     run.get('path') == workflow_path + '@' + repo['default_branch'])) and
                    run.get('event') == 'workflow_dispatch' and
                    ((windows and run.get('head_branch') == repo['default_branch'] and
                      capture.get('verification_mode') == 'existing_build_and_simulation_tests') or
                     (not windows and capture.get('read_only') is True)) and
                    observed_time(run.get('created_at'), 'Runner smoke creation') >= earliest,
                    'Runner smoke capture must prove a completed destination run at the recorded revision')
            captured_at = observed_time(capture['observed_at'], 'Runner capture')
            validate_actions_run_response(capture, run, proof['repository'], captured_at, 'Runner smoke')
            workflow = local_capture(proof.get('workflow_evidence_url'), directory, 'Runner smoke workflow')
            actual_workflow = immutable_contents(workflow, proof['repository_id'], proof['repository'],
                                                run['head_sha'], workflow_path)
            require(actual_workflow == expected_workflow and
                    observed_time(run['created_at'], 'Runner smoke creation') <=
                    observed_time(workflow.get('observed_at'), 'Runner smoke workflow') <= captured_at,
                    'Runner smoke must execute the reviewed read-only workflow bytes at its actual run head')
            jobs = local_capture(proof.get('jobs_evidence_url'), directory, 'Runner smoke jobs')
            require(type(run.get('id')) is int and run['id'] > 0 and
                    type(run.get('run_attempt')) is int and run['run_attempt'] > 0 and
                    jobs.get('request_url') == 'https://api.github.com/repos/' + proof['repository'] +
                        '/actions/runs/' + str(run['id']) + '/attempts/' + str(run['run_attempt']) + '/jobs?per_page=100' and
                    jobs.get('all_pages') is True and isinstance(jobs.get('jobs'), list) and
                    jobs.get('total_count') == len(jobs['jobs']) and
                    observed_time(run['updated_at'], 'Runner smoke completion') <=
                    observed_time(jobs.get('observed_at'), 'Runner jobs capture') <=
                    observed_time(capture['observed_at'], 'Runner capture'),
                    'Runner jobs must be captured from the completed current run attempt')
            raw_jobs = validate_raw_page_chain(jobs.get('pages'), jobs['request_url'],
                observed_time(jobs['observed_at'], 'Runner jobs capture'), 'Runner jobs', field='jobs',
                earliest=observed_time(run['updated_at'], 'Runner smoke completion'))
            require(raw_jobs == jobs['jobs'], 'Runner job conclusions must derive from complete successful API pages')
            matches = [job for job in raw_jobs if job.get('id') == proof.get('smoke_job_id')]
            require(type(proof.get('smoke_job_id')) is int and proof['smoke_job_id'] > 0 and len(matches) == 1,
                    'Runner smoke needs its unique executed job')
            job = matches[0]
            require(job.get('run_id') == run['id'] and job.get('run_attempt') == run['run_attempt'] and
                    job.get('head_sha') == run['head_sha'] and job.get('runner_id') == proof['runner_id'] and
                    job.get('runner_name') == runner['name'] and job.get('status') == 'completed' and
                    job.get('conclusion') == 'success' and string_list(job.get('labels')) and
                    'self-hosted' in job['labels'] and set(job['labels']) <= labels and
                    observed_time(run['created_at'], 'Runner smoke creation') <=
                    observed_time(job.get('started_at'), 'Runner job start') <=
                    observed_time(job.get('completed_at'), 'Runner job completion') <=
                    observed_time(run['updated_at'], 'Runner smoke completion'),
                    'Runner smoke job must execute successfully on the preserved self-hosted runner and labels')
            require(not windows or job.get('name') == 'Build and simulation tests',
                    'Windows continuity must execute the existing build and simulation job')


def validate_transfer_audit_export(export, organization, captured_at):
    require(isinstance(export, dict) and export.get('organization') == organization and
            export.get('organization_id') == 338855369, 'Transfer chronology needs its actual organization audit export')
    request = export.get('request', {})
    require(request.get('method') == 'POST' and request.get('http_status') == 201 and
            request.get('request_url') == 'https://github.com/orgs/' + organization + '/audit-log/export.json' and
            request.get('parameters') == {'q':'action:repo.transfer', 'format':'json'},
            'Transfer audit export needs its successful resource-bound JSON export request')
    previous = observed_time(request.get('observed_at'), 'Audit export request')
    paths = [('status_response', 'status_url_sha256', 'export_status', ['export_id']),
             ('verification_response', 'verify_url_sha256', 'export', ['export_id','verify_truncate']),
             ('download_response', 'export_url_sha256', 'export', ['export_id'])]
    for field, digest, suffix, parameters in paths:
        response = export.get(field, {})
        expected_digest = request.get('response', {}).get(digest)
        require(isinstance(expected_digest, str) and re.fullmatch(r'[0-9a-f]{64}', expected_digest) and
                response.get('method') == 'GET' and response.get('http_status') == 200 and
                response.get('request_origin') == 'https://github.com' and
                response.get('request_path') == '/orgs/' + organization + '/audit-log/' + suffix and
                sorted(response.get('query_parameter_names', [])) == sorted(parameters) and
                response.get('request_url_sha256') == expected_digest,
                'Transfer audit export responses must match the successful request and exact GitHub routes')
        at = observed_time(response.get('observed_at'), 'Audit export response')
        require(previous <= at <= captured_at, 'Audit export responses must follow request, readiness, verification and download order')
        previous = at
    require(export['status_response'].get('body') == '' and
            export['verification_response'].get('data', {}).get('truncated') is False,
            'Transfer audit export must finish without truncation')
    download = export['download_response']
    try:
        archive = base64.b64decode(download.get('body_base64', ''), validate=True)
        require(0 < len(archive) <= 1024 * 1024 and download.get('content_type') == 'application/gzip' and
                hashlib.sha256(archive).hexdigest() == download.get('body_sha256'),
                'Transfer audit export must retain its actual downloaded bytes and digest')
        with gzip.GzipFile(fileobj=io.BytesIO(archive)) as stream:
            raw = stream.read(10 * 1024 * 1024 + 1)
        require(len(raw) <= 10 * 1024 * 1024, 'Transfer audit export exceeds its bounded evidence size')
        try:
            events = json.loads(raw or b'[]')
        except ValueError:
            events = [json.loads(line) for line in raw.splitlines() if line.strip()]
        if isinstance(events, dict):
            events = [events]
        require(isinstance(events, list) and all(isinstance(event, dict) and event.get('action') == 'repo.transfer'
                for event in events), 'Transfer audit export must decode every filtered event from its raw JSON bytes')
    except (ValueError, TypeError, OSError, EOFError) as exc:
        raise ValueError('Transfer audit export needs valid complete raw gzip JSON bytes') from exc
    return events


def validate_repository_transfer(reference, directory, repo, cutoff, scanned_revision):
    capture = local_capture(reference, directory, 'Repository transfer')
    event = capture.get('event', {})
    organization = repo['destination'].split('/')[0]
    captured_at = observed_time(capture.get('observed_at'), 'Repository transfer capture')
    events = validate_transfer_audit_export(capture.get('audit_export'), organization, captured_at)
    require(capture.get('phase') == 'post_transfer' and capture.get('source') == repo['full_name'] and
            capture.get('repository_id') == repo['id'] and capture.get('repository') == repo['destination'] and
            capture.get('audit_log_url') == 'https://github.com/organizations/' + organization + '/settings/audit-log' and
            event.get('action') == 'repo.transfer' and text(event.get('_document_id')) and
            event.get('repo_id') == repo['id'] and event.get('repo') == repo['destination'] and
            event.get('org') == organization and event.get('org_id') == 338855369 and
            event.get('actor') == 'HemSoft' and
            ('repo_was' not in event or event['repo_was'] == repo['full_name']),
            'Repository transfer must match its captured GitHub acceptance event and source/destination identities')
    require(sum(actual == event for actual in events) == 1,
            'Repository transfer event and timestamp must derive from the successful raw audit export')
    milliseconds = event.get('@timestamp')
    require(type(milliseconds) is int and milliseconds > 0 and
            ('created_at' not in event or event['created_at'] == milliseconds),
            'Repository transfer needs the actual audit event timestamp')
    transferred_at = datetime.datetime.fromtimestamp(milliseconds / 1000, datetime.timezone.utc)
    require(cutoff < transferred_at <= observed_time(capture['audit_export']['request']['observed_at'], 'Audit export request') <= captured_at,
            'Repository transfer must follow immutable ledger readiness and source recheck')
    policy = local_capture(capture.get('source_protection_evidence_url'), directory, 'Immediate pre-transfer source policy')
    policy_at = observed_time(policy.get('observed_at'), 'Immediate source policy')
    require(policy.get('phase') == 'pre_transfer' and policy.get('repository_id') == repo['id'] and
            policy.get('repository') == repo['full_name'] and policy.get('revision_sha') == scanned_revision[0] and
            cutoff <= policy_at <= transferred_at and transferred_at - policy_at <= datetime.timedelta(seconds=60),
            'Each transfer needs its source policy within 60 seconds before acceptance and after the final cutoff')
    actual_policy = validate_protection_responses(policy, repo, repo['full_name'], scanned_revision[0], policy_at,
        max(cutoff, transferred_at - datetime.timedelta(seconds=60)), directory=directory)
    require(actual_policy == protection_contract(repo, directory),
            'Late source protection changes must be reconciled with the reviewed preservation baseline before transfer')
    heads = local_capture(capture.get('destination_heads_evidence_url'), directory, 'Transferred branch heads')
    require(heads.get('repository_id') == repo['id'] and heads.get('repository') == repo['destination'] and
            heads.get('phase') == 'post_transfer' and
            transferred_at <= observed_time(heads.get('observed_at'), 'Transferred branch heads') <= captured_at,
            'Transfer needs repository-bound branch observations after its acceptance event')
    response = heads.get('repository_response', {})
    validate_resource_response(response, 'https://api.github.com/repos/' + repo['destination'], response.get('data'))
    require(response['data'].get('id') == repo['id'] and response['data'].get('full_name') == repo['destination'],
            'Transferred branch observations need their actual destination repository identity')
    destination = dict(repo, full_name=repo['destination'])
    require(source_revision(heads, destination, transferred_at) == scanned_revision,
            'Transferred branch heads or tag refs changed after the credential/reference scan; reconcile before proceeding')
    return transferred_at


def validate_pre_sync_installation(proof, directory, row):
    capture = local_capture(proof.get('evidence_url'), directory, 'Pre-sync installation')
    require(capture.get('phase') == 'pre_sync' and all(capture.get(field) == proof.get(field) for field in
            ('repository_id','repository','revision_sha','manifest_paths','state','tier','addons','components')),
            'Pre-sync capture must match the repository revision and observed installation configuration')
    timestamp = observed_time(capture.get('observed_at'), 'Pre-sync installation')
    destination = local_capture(row['destination_protections']['evidence_url'], directory, 'Destination metadata')
    deployed = local_capture(row['manifest_evidence_url'], directory, 'Deployed manifest')
    require(observed_time(destination['observed_at'], 'Destination metadata') <= timestamp <=
            observed_time(deployed['observed_at'], 'Deployed manifest'),
            'Pre-sync capture must follow destination verification and precede deployed status')
    require(isinstance(row.get('gate_policy'), dict) and text(row['gate_policy'].get('branch')),
            'Completed review needs a required strict SFL gate policy')
    head = capture.get('default_branch_head', {})
    require(head.get('http_status') == 200 and
            head.get('request_url') == 'https://api.github.com/repos/' + row['destination'] + '/git/ref/heads/' + row['gate_policy']['branch'] and
            head.get('data', {}).get('object', {}).get('sha') == proof['revision_sha'],
            'Pre-sync input must match the independently read current default-branch ref')
    operation = local_capture(capture.get('deployment_input_evidence_url'), directory, 'Deployment input')
    require(head.get('branch') == row['gate_policy']['branch'] and head.get('sha') == proof['revision_sha'] and
            operation.get('repository_id') == row['repository_id'] and operation.get('repository') == row['destination'] and
            operation.get('input_revision_sha') == proof['revision_sha'] and
            operation.get('result_revision_sha') == deployed['revision_sha'],
            'Pre-sync revision must be the observed default branch and actual deployment input')
    comparison = operation.get('comparison', {})
    require(comparison.get('status') in {'ahead', 'identical'} and
            comparison.get('base_commit', {}).get('sha') == proof['revision_sha'] and
            comparison.get('merge_base_commit', {}).get('sha') == proof['revision_sha'] and
            comparison.get('html_url') == 'https://github.com/' + row['destination'] + '/compare/' +
                proof['revision_sha'] + '...' + deployed['revision_sha'],
            'Deployed revision must contain the independently captured pre-sync input')
    input_at = observed_time(operation.get('observed_at'), 'Deployment input')
    started_at = observed_time(operation.get('started_at'), 'Deployment operation start')
    require(timestamp <= started_at <= input_at <= observed_time(deployed['observed_at'], 'Deployed manifest'),
            'Deployment input proof must follow pre-sync inspection and precede final deployment status')
    files = capture.get('manifest_files')
    require(isinstance(files, dict) and set(files) == {'.sfl/sfl.json','sfl.json'} and
            set(proof['manifest_paths']) == set(files), 'Pre-sync capture must inspect both manifest locations')
    for path, value in files.items():
        require(isinstance(value, dict) and value.get('repository_id') == row['repository_id'] and
                value.get('repository') == row['destination'] and value.get('revision_sha') == proof['revision_sha'] and
                value.get('path') == path and value.get('state') in {'absent','observed'},
                'Manifest location capture must bind its path and pre-sync revision')
        require(value.get('http_status') == (404 if value['state']=='absent' else 200),
                'Manifest absence needs a captured 404; presence needs a successful contents read')
        response = value.get('contents_response', {})
        require(response.get('request_url') == 'https://api.github.com/repos/' + row['destination'] +
                '/contents/' + path + '?ref=' + proof['revision_sha'] and
                response.get('http_status') == value['http_status'],
                'Pre-sync manifest needs its exact immutable contents response')
        if value['state'] == 'absent':
            require(response.get('data', {}).get('message') == 'Not Found',
                    'Pre-sync absence must derive from the actual contents 404')
        else:
            content = immutable_contents(value, row['repository_id'], row['destination'], proof['revision_sha'], path)
            require(value.get('manifest') == json.loads(content),
                    'Pre-sync configuration must derive from immutable captured manifest contents')
    primary = files['.sfl/sfl.json'] if files['.sfl/sfl.json']['state']=='observed' else files['sfl.json']
    if proof['state'] == 'absent':
        require(all(value['state']=='absent' for value in files.values()) and
                proof['tier']=='not_installed' and proof['addons']==[] and proof['components']==[],
                'Absent installation needs independent absence of both manifests')
    else:
        manifest = primary.get('manifest', {})
        require(primary['state']=='observed' and manifest.get('tier') == proof['tier'] and
                manifest.get('addons',[]) == proof['addons'] and manifest.get('components',[]) == proof['components'],
                'Present installation must match independently captured manifest contents')
    return started_at


def validate_pilot_scenario(result, scenario, directory, repository_id, repository, sha, version, revision, cutover=None):
    capture = local_capture(result.get('capture_evidence_url'), directory, 'Pilot scenario execution')
    require(capture.get('repository_id') == repository_id and capture.get('repository') == repository and
            capture.get('deployment_sha') == sha and capture.get('release_version') == version and
            capture.get('tested_revision_sha') == revision and capture.get('scenario') == scenario and
            capture.get('mode') == result['mode'] and capture.get('outcome') == result['outcome'] and
            capture.get('status') == 'completed', 'Pilot scenario capture must match the executed observer and terminal result')
    scenario_at = observed_time(capture.get('observed_at'), 'Pilot scenario execution')
    if cutover is not None:
        require(scenario_at >= cutover, 'Pilot scenario must execute after App cutover and deployed observer capture')
    output = local_capture(capture.get('output_evidence_url'), directory, 'Pilot scenario output')
    require(output.get('repository_id') == repository_id and output.get('repository') == repository and
            output.get('deployment_sha') == sha and output.get('tested_revision_sha') == revision and
            output.get('scenario') == scenario and output.get('mode') == result['mode'] and
            output.get('outcome') == PILOT_SCENARIOS[scenario] and output.get('passed') is True,
            'Pilot scenario output must prove the expected result for the tested observer revision')
    if result['mode'] == 'workflow_fixture':
        workflow = local_capture(capture.get('workflow_evidence_url'), directory, 'Fixture observer source')
        require(workflow.get('repository_id') == repository_id and workflow.get('repository') == repository and
                workflow.get('revision_sha') == revision and workflow.get('path') == '.github/workflows/sfl-pr-review-auto.yml' and
                text(workflow.get('content')), 'Fixture observer source must bind its actual deployed workflow and revision')
        require(observed_time(output.get('observed_at'), 'Fixture output') <= scenario_at,
                'Fixture execution capture must follow generated output')
        validate_workflow_contents(workflow, repository, revision)
        runner = pathlib.Path(__file__).resolve().parents[2] / 'deployment/tests/run-org-observer-fixtures.cjs'
        argv = ['node', 'deployment/tests/run-org-observer-fixtures.cjs', '--workflow', 'docs/organization-migration/' + capture['workflow_evidence_url'],
                '--repository-id', str(repository_id), '--repository', repository, '--deployment-sha', sha,
                '--release-version', version, '--revision', revision, '--scenario', scenario,
                '--output', 'docs/organization-migration/' + capture['output_evidence_url']]
        require(capture.get('argv') == argv and capture.get('command') == 'run-org-observer-fixtures' and
                output.get('runner_sha256') == hashlib.sha256(runner.read_bytes()).hexdigest() and
                output.get('workflow_sha256') == hashlib.sha256(workflow['content'].encode()).hexdigest() and
                capture.get('output_sha256') == hashlib.sha256((directory/capture['output_evidence_url']).read_bytes()).hexdigest(),
                'Fixture execution must use the approved runner and exact arguments, with matching generated output and observer digests')
        require(type(capture.get('exit_code')) is int and capture['exit_code'] == 0 and
                text(capture.get('command')) and capture.get('conclusion') == 'success' and
                result['evidence_url'] == result['capture_evidence_url'],
                'Fixture scenario needs its successful executed command and captured output')
        reproduced = replay_observer_fixture(json.dumps(workflow, sort_keys=True), repository_id, repository,
            sha, version, revision, scenario, hashlib.sha256(runner.read_bytes()).hexdigest())
        require(all(reproduced.get(key) == output.get(key) for key in (
            'repository_id', 'repository', 'deployment_sha', 'release_version', 'tested_revision_sha',
            'scenario', 'mode', 'outcome', 'passed', 'runner_sha256', 'workflow_sha256')),
            'Recorded fixture output must equal the independently executed observer result')
    else:
        run = capture.get('run', {})
        require(re.fullmatch('https://github.com/' + re.escape(repository) + r'/actions/runs/[1-9][0-9]*',
                             result.get('evidence_url','')) is not None and
                run.get('html_url') == result['evidence_url'] and run.get('repository',{}).get('id') == repository_id and
                run['repository'].get('full_name') == repository and run.get('head_sha') == revision and
                run.get('path') == '.github/workflows/sfl-pr-review-auto.yml' and
                run.get('status') == 'completed' and run.get('conclusion') in {'success','failure'},
                'Live scenario needs its terminal deployed-observer Actions run')
        validate_resource_response(capture.get('run_response', {}), 'https://api.github.com/repos/' + repository +
            '/actions/runs/' + str(run.get('id')), run)
        require(observed_time(run.get('updated_at'), 'Live scenario completion') <=
                observed_time(capture['run_response'].get('observed_at'), 'Live scenario run GET') <= scenario_at,
                'Live scenario run GET must follow completion and precede its capture')
        terminal_run_time(run, scenario_at, 'Live scenario')
        if cutover is not None:
            require(observed_time(run.get('created_at'), 'Live scenario creation') >= cutover and
                    observed_time(run.get('run_started_at'), 'Live scenario attempt start') >= cutover,
                    'Live scenario run and attempt must start after App cutover and deployed observer capture')
        run_id = int(result['evidence_url'].rsplit('/', 1)[1])
        require(type(run.get('id')) is int and run['id'] == run_id and
                type(run.get('run_attempt')) is int and run['run_attempt'] > 0 and
                output.get('run_id') == run_id and output.get('run_attempt') == run['run_attempt'],
                'Live scenario output must identify its exact run and attempt')
        validate_run_artifact(capture.get('artifact_evidence_url'), directory, repository, run,
                              'sfl-observer-scenario-' + scenario, 'scenario.json', output, scenario_at)


@functools.lru_cache(maxsize=128)
def replay_observer_fixture(workflow_json, repository_id, repository, sha, version, revision, scenario, runner_digest):
    root = pathlib.Path(__file__).resolve().parents[2]
    runner = root / 'deployment/tests/run-org-observer-fixtures.cjs'
    require(hashlib.sha256(runner.read_bytes()).hexdigest() == runner_digest, 'Fixture runner changed before replay')
    source = json.loads(workflow_json)['content']
    # Historical installed receipts must be compared with their reviewed release,
    # rather than whichever observer happens to be current when CI replays them.
    snapshots = {'2.1.0-rc.21': ('deployment/tests/observer-snapshots/2.1.0-rc.21.yml',
                              '86833629e9557bbe658147a190ae8b77b8166e6fb2919f2902b5e862930f556e')}
    if version in snapshots:
        snapshot_path, snapshot_digest = snapshots[version]
        canonical_bytes = (root / snapshot_path).read_bytes()
        require(hashlib.sha256(canonical_bytes).hexdigest() == snapshot_digest,
                'Historical reviewed observer snapshot changed before replay')
        canonical = canonical_bytes.decode()
    else:
        canonical = (root / 'deployment/infrastructure/sfl-pr-review-auto.yml').read_text()
    markers = [('// BEGIN TESTABLE ' + name, '// END TESTABLE ' + name) for name in
        ('CODEX OBSERVER', 'REQUESTER AUTHORIZATION', 'REQUESTER REFRESH', 'REQUEST REGISTRATION',
         'REQUEST ELIGIBILITY', 'REQUIRED GATE REPAIR', 'REVIEW CONTEXT PROVENANCE')]
    markers.append(('const existing = latestRequestChecks.find(', 'const detailsURL = artifact.html_url'))
    for begin, end in markers:
        require(source.count(begin) == canonical.count(begin) > 0 and
                source.count(end) == canonical.count(end) > 0,
                'Independent observer fixture execution requires unique reviewed source markers')
        start = source.index(begin) + len(begin)
        reviewed_start = canonical.index(begin) + len(begin)
        finish, reviewed_finish = source.find(end, start), canonical.find(end, reviewed_start)
        require(start <= finish and reviewed_start <= reviewed_finish and
                source[start:finish] == canonical[reviewed_start:reviewed_finish],
                'Independent observer fixture execution requires reviewed canonical code blocks')
    with tempfile.TemporaryDirectory(prefix='sfl-observer-replay-') as folder:
        workflow, output = pathlib.Path(folder) / 'workflow.json', pathlib.Path(folder) / 'output.json'
        workflow.write_text(workflow_json)
        argv = ['node', str(runner), '--workflow', str(workflow), '--repository-id', str(repository_id),
            '--repository', repository, '--deployment-sha', sha, '--release-version', version,
            '--revision', revision, '--scenario', scenario, '--output', str(output)]
        try:
            environment = {key: os.environ[key] for key in ('PATH', 'SystemRoot', 'WINDIR') if key in os.environ}
            completed = subprocess.run(argv, cwd=folder, env=environment,
                capture_output=True, text=True, timeout=30, check=False)
            require(completed.returncode == 0 and output.is_file(), 'Independent observer fixture execution failed')
            return json.loads(output.read_text())
        except (OSError, subprocess.TimeoutExpired, ValueError) as exc:
            raise ValueError('Independent observer fixture execution could not verify the result') from exc


def provider_preservation_baseline(row, inventory, directory):
    kind, resource = row.get('resource_kind'), row.get('resource_id')
    if kind == 'vercel_project':
        capture = local_capture('vercel-provider-evidence.json', directory, 'Vercel baseline')
        project = next((p for p in capture['projects'] if p['id'] == resource), None)
        require(project is not None, 'Unknown sealed Vercel resource')
        return {key: project.get(key) for key in ('id', 'accountId', 'framework', 'name', 'targets')}
    if kind == 'github_pages':
        repo = next(r for r in inventory['repositories'] if r['id'] == int(row['repository_id']))
        pages = repo['settings']['pages']
        require(pages['state'] == 'observed', 'Pages preservation needs a sealed observed baseline')
        return {key: pages['data'].get(key) for key in ('cname', 'build_type', 'source', 'public', 'https_enforced')}
    if kind == 'supabase_project':
        capture = local_capture('supabase-provider-evidence.json', directory, 'Supabase baseline')
        project = next((p for p in capture['projects'] if p['reference'] == resource), None)
        require(project is not None, 'Unknown sealed Supabase resource')
        return dict(project, organization=capture['organization']['slug'])
    cloudflare = local_capture('now-leadership-live-hosting-evidence.json', directory, 'Cloudflare baseline')['cloudflare_owner_verification']
    if kind == 'cloudflare_zone':
        require(resource == cloudflare['zone_id'], 'Unknown sealed Cloudflare zone')
        return {key: cloudflare[key] for key in ('account_id', 'zone_id', 'plan', 'website_records', 'workers_routes')}
    if kind == 'cloudflare_worker':
        worker = next((p for p in cloudflare['workers_and_pages']['applications'] if p['name'] == resource), None)
        require(worker is not None, 'Unknown sealed Cloudflare Worker')
        return dict(worker, account_id=cloudflare['account_id'], workers_routes=cloudflare['workers_routes'])
    raise ValueError('Baseline preservation requires a supported sealed provider resource')


def validate_provider_success(smoke, row, inventory, directory, repository):
    """Check provider state from successful, resource-bound read-only responses."""
    observed = observed_time(smoke.get('observed_at'), 'Provider smoke')
    responses = smoke.get('provider_responses', {})
    times = []

    def response(name, url):
        capture = responses.get(name, {})
        at = observed_time(capture.get('observed_at'), 'Provider response')
        urls = {url}
        if url.startswith('https://api.cloudflare.com/client/v4/'):
            urls.add(url.replace('https://api.cloudflare.com/client/v4/', 'https://dash.cloudflare.com/api/v4/', 1))
        if url.startswith('https://api.vercel.com/'):
            urls.add(url.replace('https://api.vercel.com/', 'https://vercel.com/api/', 1))
        if url.startswith('https://api.supabase.com/v1/projects/') or url.startswith('https://api.supabase.com/v1/organizations/'):
            urls.add(url.replace('https://api.supabase.com/v1/', 'https://api.supabase.com/platform/', 1))
        require(capture.get('request_url') in urls and capture.get('http_status') == 200 and
                capture.get('method') == 'GET' and at <= observed,
                'Provider success needs successful exact resource GETs before its capture')
        times.append(at)
        return capture.get('data')

    kind, resource = row.get('resource_kind'), row.get('resource_id')
    if kind == 'vercel_project':
        baseline = provider_preservation_baseline(row, inventory, directory)
        project = response('project', 'https://api.vercel.com/v9/projects/' + resource +
                           '?teamId=' + baseline['accountId'])
        require(isinstance(project, dict) and all(project.get(k) == baseline[k] for k in
                ('id', 'accountId', 'framework', 'name')), 'Vercel success must preserve the sealed project identity')
        original = next(p for p in local_capture('vercel-provider-evidence.json', directory, 'Vercel baseline')['projects']
                        if p['id'] == resource)
        if resource == 'prj_hPjAbxtMlCi3A5waKxQpjATto0ae':
            require(project.get('link') is None, 'Retired Vercel Git integration must remain disconnected')
        else:
            owner, name = repository.split('/')
            link = project.get('link') or {}
            require(link.get('type') == 'github' and link.get('org') == owner and link.get('repo') == name and
                    link.get('repoId') == int(row['repository_id']) and
                    link.get('productionBranch') == original['link']['productionBranch'],
                    'Vercel success needs the actual expected repository binding and production branch')
        production = project.get('targets', {}).get('production') or {}
        aliases = production.get('alias')
        require(production.get('readyState') == 'READY' and isinstance(aliases, list) and
                set(baseline['targets']['production'].get('alias', [])).issubset(aliases),
                'Vercel success needs READY production and preserved production aliases')
    elif kind == 'github_pages':
        baseline = provider_preservation_baseline(row, inventory, directory)
        pages = response('pages', 'https://api.github.com/repos/' + repository + '/pages')
        require(isinstance(pages, dict) and all(pages.get(k) == v for k, v in baseline.items()),
                'Pages success must preserve the sealed HTTPS, domain and build configuration')
        owner, name = repository.split('/')
        site = 'https://' + (baseline['cname'] + '/' if baseline['cname'] else owner.lower() + '.github.io/' + name + '/')
        require(pages.get('html_url') == site and response('site', site).get('final_url') == site,
                'Pages success needs the actual destination Pages URL and successful site GET')
    elif kind == 'cloudflare_zone':
        baseline = provider_preservation_baseline(row, inventory, directory)
        base = 'https://api.cloudflare.com/client/v4/zones/' + resource
        zone = response('zone', base)
        require(zone.get('success') is True and zone.get('result', {}).get('id') == resource and
                zone['result'].get('account', {}).get('id') == baseline['account_id'] and
                zone['result'].get('plan', {}).get('name') in {baseline['plan'], 'Free Website'} and
                baseline['plan'] == 'Free',
                'Cloudflare success must preserve zone ownership and plan')
        dns, routes = response('dns', base + '/dns_records?per_page=100'), response('routes', base + '/workers/routes')
        require(responses['dns'].get('all_pages') is True and dns.get('success') is True and
                isinstance(dns.get('result'), list) and routes.get('success') is True and
                routes.get('result') == baseline['workers_routes'], 'Cloudflare success needs complete DNS and unchanged Worker routes')
        for record in baseline['website_records']:
            require(any(all(current.get(k) == v for k, v in record.items()) for current in dns['result']),
                    'Cloudflare website DNS content and proxy state must be preserved')
    elif kind == 'cloudflare_worker':
        baseline = provider_preservation_baseline(row, inventory, directory)
        base = 'https://api.cloudflare.com/client/v4/accounts/' + baseline['account_id'] + '/workers/scripts'
        scripts = response('scripts', base)
        require(responses['scripts'].get('all_pages') is True and scripts.get('success') is True and
                any(script.get('id') == resource for script in scripts.get('result', [])),
                'Cloudflare Worker success must observe the actual script in its owner account')
        subdomain = response('subdomain', base + '/' + resource + '/subdomain')
        account_domain = response('account_subdomain', 'https://api.cloudflare.com/client/v4/accounts/' +
                                  baseline['account_id'] + '/workers/subdomain')
        zone = local_capture('now-leadership-live-hosting-evidence.json', directory, 'Cloudflare zone')['cloudflare_owner_verification']['zone_id']
        routes = response('routes', 'https://api.cloudflare.com/client/v4/zones/' + zone + '/workers/routes')
        require(subdomain.get('success') is True and subdomain.get('result', {}).get('enabled') is True and
                account_domain.get('success') is True and
                'https://' + resource + '.' + account_domain.get('result', {}).get('subdomain', '') + '.workers.dev' == baseline['url'] and
                routes.get('success') is True and routes.get('result') == baseline['workers_routes'],
                'Cloudflare Worker success needs its enabled preserved workers.dev URL and route configuration')
    elif kind == 'repository_runner':
        runner = response('runner', 'https://api.github.com/repos/' + repository + '/actions/runners/' + str(resource))
        if int(row['repository_id']) == SURVIVAL_REPOSITORY_ID:
            repo = next(repo for repo in inventory['repositories'] if repo['id'] == SURVIVAL_REPOSITORY_ID)
            baseline = reviewed_survival_resources(repo, directory)['runner']
            require(str(baseline['id']) == resource and runner.get('os') == 'Windows',
                    'Windows runner resource must preserve its observed OS and registration')
            expected_name, expected_labels = baseline['name'], {label['name'] for label in baseline['labels']}
        else:
            require(int(row['repository_id']) == 1188676172 and resource == '21',
                    'Runner provider observation needs a reviewed repository and registration')
            expected_name, expected_labels = 'mini-github-runner-01', {'self-hosted', 'Linux', 'X64', 'mini', 'yahtzee'}
        require(runner.get('id') == int(resource) and runner.get('name') == expected_name and
                runner.get('status') == 'online' and runner.get('busy') is False and
                {label.get('name') for label in runner.get('labels', [])} == expected_labels,
                'Runner success needs its actual online idle repository registration and preserved labels')
    elif kind == 'supabase_project':
        baseline = provider_preservation_baseline(row, inventory, directory)
        require(resource == 'cevpnetigzotgstxxjpm' and row.get('smoke_outcome') in {'baseline_preserved', 'preserved_unused'},
                'Supabase runtime success cannot replace the approved unused paused disposition')
        validate_supabase_project(response, responses, baseline, resource)
    else:
        raise ValueError('Provider success needs a supported provider-specific observation; preserve unused resources separately')
    return min(times)


def repository_terminal_times(records, ledger_rows, directory):
    """Collect timestamped local captures only after their contracts passed."""
    def references(value):
        if isinstance(value, dict):
            for key, item in value.items():
                if key.endswith(('evidence_url', 'verification_url')) and isinstance(item, str) and not urllib.parse.urlsplit(item).scheme:
                    yield item
                elif isinstance(item, (dict, list)):
                    yield from references(item)
        elif isinstance(value, list):
            for item in value:
                yield from references(item)
    times = {}
    for row in records:
        relevant = [row] + [r for r in ledger_rows if int(r['repository_id']) == row['repository_id']]
        observations = []
        for reference in set(references(relevant)):
            if not reference:
                continue
            capture = local_capture(reference, directory, 'Terminal repository evidence')
            timestamp = capture.get('observed_at') or capture.get('approved_at')
            if timestamp:
                observations.append(observed_time(timestamp, 'Terminal repository evidence'))
            run = capture.get('run', {})
            if run.get('status') == 'completed':
                observations.append(observed_time(run.get('updated_at'), 'Terminal workflow completion'))
        require(bool(observations), 'Terminal repository needs independently timestamped completion evidence')
        times[row['repository_id']] = max(observations)
    return times


def validate_supabase_project(response, responses, baseline, resource):
    project = response('project', 'https://api.supabase.com/v1/projects/' + resource)
    require(isinstance(project, dict), 'Supabase preservation needs actual project metadata')
    organization_matches = project.get('organization_slug') == baseline['organization']
    if responses['project']['request_url'].startswith('https://api.supabase.com/platform/'):
        organization = response('organization', 'https://api.supabase.com/v1/organizations/' + baseline['organization'])
        organization_matches = isinstance(organization, dict) and organization.get('slug') == baseline['organization'] and \
            project.get('organization_id') is not None and organization.get('id') == project['organization_id']
    require(project.get('ref') == resource and project.get('name') == baseline['name'] and
            project.get('region') == baseline['region'] and organization_matches and project.get('status') == 'INACTIVE',
            'Supabase preservation needs the actual owner project metadata and paused state')


def validate_unlinked_supabase(resource, baseline, directory, cutoff):
    require(resource['status'] == 'verified', 'Final completion needs verified unlinked Supabase preservation')
    reference = resource.get('post_transfer_evidence_url')
    evidence(reference, directory)
    require(not urllib.parse.urlsplit(reference).scheme and reference != 'supabase-provider-evidence.json',
            'Unlinked resource needs an independent post-transfer account capture')
    capture = local_capture(reference, directory, 'Unlinked Supabase preservation')
    validate_preservation_time(capture, cutoff)
    require(capture.get('resource_id') == resource['resource_id'] and capture.get('resource_owner') == resource['resource_owner'] and
            capture.get('state') == baseline['state'] and capture.get('project') == baseline and
            capture.get('resource_changes_made') is False and capture.get('operation') == 'read_only_preservation',
            'Unlinked Supabase post-transfer capture must preserve the exact account project configuration and paused state')
    responses = capture.get('provider_responses', {})
    observed = observed_time(capture['observed_at'], 'Unlinked Supabase capture')

    def response(name, url):
        raw = responses.get(name, {})
        at = observed_time(raw.get('observed_at'), 'Unlinked Supabase response')
        require(raw.get('request_url') in {url, url.replace('https://api.supabase.com/v1/', 'https://api.supabase.com/platform/', 1)} and
                raw.get('method') == 'GET' and raw.get('http_status') == 200 and cutoff <= at <= observed,
                'Unlinked Supabase preservation needs successful post-rollout resource-specific metadata GETs')
        return raw.get('data')

    organization = local_capture('supabase-provider-evidence.json', directory, 'Supabase account baseline')['organization']['slug']
    validate_supabase_project(response, responses, dict(baseline, organization=organization), resource['resource_id'])



def without_sfl_gate_rules(rules):
    result = []
    for original in rules:
        rule = copy.deepcopy(original)
        if rule.get('type') == 'required_status_checks':
            checks = rule.get('parameters', {}).get('required_status_checks', [])
            rule['parameters']['required_status_checks'] = [check for check in checks if not
                (check.get('context') == 'SFL Reviewer Gate Runner' and check.get('integration_id') == 15368)]
            if not rule['parameters']['required_status_checks']:
                continue
        result.append(rule)
    return sorted(json.dumps(rule, sort_keys=True) for rule in result)


def without_sfl_gate_classic(response):
    if response['state'] == 'absent':
        return None
    policy = copy.deepcopy(response['data'])
    checks = policy.get('required_status_checks')
    if isinstance(checks, dict):
        checks['contexts'] = [context for context in checks.get('contexts', []) if context != 'SFL Reviewer Gate Runner']
        checks['checks'] = [check for check in checks.get('checks', []) if not
            (check.get('context') == 'SFL Reviewer Gate Runner' and check.get('app_id') == 15368)]
        if not checks['contexts'] and not checks['checks']:
            policy['required_status_checks'] = None
    return policy


def validate_pilot_cleanup(receipts, directory, repository_id, repository, branch, operations, operation_times):
    ordered = ('init_pr_url', 'repeat_onboarding_evidence_url', 'sync_pr_url',
               'repeat_sync_evidence_url', 'status_evidence_url', 'gate_uninstall_evidence_url')
    require(all(operation_times[a] <= operation_times[b] for a,b in zip(ordered, ordered[1:])),
            'Pilot terminal operations must follow execution time as well as revision order')
    prior = {k:v for k,v in receipts.items() if k not in {
        'gate_uninstall_evidence_url', 'pre_cleanup_gate_policy_evidence_url', 'final_gate_policy_evidence_url'}}
    prior['operation_receipts'] = {k:v for k,v in operations.items() if k != 'gate_uninstall_evidence_url'}
    latest = repository_terminal_times([{'repository_id':repository_id, 'validation':prior}], [], directory)[repository_id]
    removed_at = operation_times['gate_uninstall_evidence_url']
    require(removed_at >= latest, 'Pilot gate removal must follow the latest completed validation evidence')
    before = local_capture(receipts.get('pre_cleanup_gate_policy_evidence_url'), directory, 'Pre-cleanup pilot gate policy')
    execution = local_capture(operations['gate_uninstall_evidence_url']['capture_evidence_url'], directory,
                              'Pilot gate removal execution')['execution']
    require(before.get('repository_id') == repository_id and before.get('repository') == repository and
            before.get('branch') == branch and latest <= observed_time(before.get('observed_at'), 'Pre-cleanup pilot policy') <=
            observed_time(execution.get('started_at'), 'Pilot gate removal start'),
            'Pre-cleanup policy must follow completed validation and precede the actual gate removal execution')
    before_rules, before_classic = validate_branch_policy_responses(before, repository, branch, latest)
    policy = local_capture(receipts.get('final_gate_policy_evidence_url'), directory, 'Final pilot gate policy')
    require(policy.get('repository_id') == repository_id and policy.get('repository') == repository and
            policy.get('branch') == branch and observed_time(policy.get('observed_at'), 'Final pilot policy') >= removed_at,
            'Final pilot policy must follow gate removal on the actual default branch')
    rules, classic = validate_branch_policy_responses(policy, repository, branch, removed_at)
    checks = classic.get('data', {}).get('required_status_checks') or {}
    contexts = list(checks.get('contexts', [])) + [c.get('context') for c in checks.get('checks', [])]
    contexts += [c.get('context') for r in rules['data'] if r.get('type') == 'required_status_checks'
                 for c in r.get('parameters', {}).get('required_status_checks', [])]
    require('SFL Reviewer Gate Runner' not in contexts, 'Final pilot policy must prove the SFL gate is absent')
    require(without_sfl_gate_rules(before_rules['data']) == without_sfl_gate_rules(rules['data']) and
            without_sfl_gate_classic(before_classic) == without_sfl_gate_classic(classic),
            'Pilot gate cleanup must preserve all unrelated effective and classic policy')


def validate_ledger_rows(rows, inventory, directory, expected, retained, candidates, expected_unused,
                         runner_resources, vercel_resources, other_resources, external_scope):
    seen_vercel_resources = set()
    seen_other_resources = {kind: set() for kind in other_resources}
    seen_runner_resources = set()
    seen_ledger = set()
    ledger_statuses = {}
    post_transfer_smokes = {}
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
            smoke_observed_at = observed_time(smoke.get('observed_at'), 'Integration smoke')
            if row['smoke_outcome'] == 'approved_recovery':
                require(smoke.get('recovery_success') is True and smoke.get('approved_by') == 'HemSoft',
                        'Integration recovery needs owner approval and a successful result')
                approval = local_capture(smoke.get('owner_approval_evidence_url'), directory, 'Provider recovery approval')
                decision = {'repository_id':repo_id, 'repository':smoke_repository, 'provider':row['provider'],
                            'resource_id':row['resource_id'], 'disposition':'approved_recovery',
                            'reason':approval.get('reason'), 'recovery_action':row['recovery_action'],
                            'operation':smoke.get('recovery_operation')}
                operation = decision['operation']
                require(isinstance(operation, dict) and set(operation) == {'command','argv'} and text(operation.get('command')) and
                        string_list(operation.get('argv')) and bool(operation['argv']),
                        'Provider recovery needs its exact approved executable action')
                require(text(decision['reason']) and all(approval.get(k) == v for k, v in decision.items()),
                        'Provider recovery approval must identify this exact resource and action')
                validate_owner_approval_comment(approval.get('owner_comment_evidence_url'), directory,
                    smoke.get('owner_receipt_url'), observed_time(approval.get('approved_at'), 'Provider recovery approval'),
                    decision, smoke_observed_at)
                execution = local_capture(smoke.get('recovery_execution_evidence_url'), directory, 'Provider recovery execution')
                require(all(execution.get(key) == decision[key] for key in
                            ('repository_id','repository','provider','resource_id','recovery_action','operation')) and
                        type(execution.get('exit_code')) is int and execution['exit_code'] == 0 and
                        execution.get('status') == 'completed' and execution.get('conclusion') == 'success',
                        'Provider recovery execution must bind the exact owner-approved action and successful terminal result')
                started = observed_time(execution.get('started_at'), 'Recovery execution start')
                completed = observed_time(execution.get('completed_at'), 'Recovery execution completion')
                require(observed_time(approval['approved_at'], 'Recovery approval') <= started <= completed <=
                        observed_time(execution.get('observed_at'), 'Recovery execution capture') <= smoke_observed_at,
                        'Recovery execution must follow approval and precede its captured smoke')
                provider_at = validate_provider_success(smoke, row, inventory, directory, smoke_repository)
                require(completed <= provider_at, 'Provider recovery metadata GETs must follow its exact executed action')
                smoke_observed_at = min(started, provider_at)
            elif row['smoke_outcome'] in {'baseline_preserved', 'preserved_unused'}:
                approvals = {('vercel_project', 'prj_hPjAbxtMlCi3A5waKxQpjATto0ae'):
                             'https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6029136048',
                             ('supabase_project', 'cevpnetigzotgstxxjpm'):
                             'https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6030145370'}
                require((kind, row.get('resource_id')) in approvals and
                        smoke.get('owner_receipt_url') == approvals[(kind, row.get('resource_id'))] and
                        smoke.get('runtime_actions') == [] and smoke.get('resource_unchanged') is True,
                        'Preservation-only outcomes require an explicitly approved unused resource and no runtime actions')
                baseline = provider_preservation_baseline(row, inventory, directory)
                require(smoke.get('baseline') == baseline and smoke.get('observed_resource') == baseline,
                        'Baseline preservation must match the sealed provider resource and independent observation')
                smoke_observed_at = validate_provider_success(smoke, row, inventory, directory, smoke_repository)
            else:
                require(smoke.get('continuity_verified') is True, 'Integration success needs verified continuity')
                smoke_observed_at = validate_provider_success(smoke, row, inventory, directory, smoke_repository)
            if row['smoke_phase'] == 'post_transfer':
                post_transfer_smokes.setdefault(repo_id, []).append(smoke_observed_at)
            evidence(row['resource_url'], directory)
            require(row.get('credential_validity') in {'verified', 'not_required'},
                    'Credential presence alone does not establish validity')
    require(seen_ledger == set(expected), 'Ledger must cover every baseline ID')
    require(seen_runner_resources == runner_resources, 'Ledger must preserve every observed repository runner resource')
    require(seen_vercel_resources == vercel_resources, 'Ledger must preserve every captured Vercel project resource')
    require(all(seen_other_resources[kind] == resources for kind, (_, resources) in other_resources.items()),
            'Ledger must preserve every captured Pages/Supabase/Cloudflare resource')
    return ledger_statuses, post_transfer_smokes


def validate_preservation_time(capture, cutoff):
    require(capture.get('phase') == 'post_transfer' and
            observed_time(capture.get('observed_at'), 'Account preservation') > cutoff,
            'Account preservation must be observed after rollout and App transfer')


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
    validate_codex_installation_policy(captured_codex)
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
    owner_scope_times = [validate_owner_scope(absence, {
        'scope': absence['scope'], 'providers': sorted(absence['providers']), 'disposition': absence['disposition']}, directory),
        validate_owner_scope(external_scope, {field: external_scope[field] for field in
            ('scope', 'active_external_resources', 'blacksmith_usage', 'modern_web_stack_poc_usage', 'database_treatment')}, directory)]
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
    survival = reviewed_survival_resources(expected[SURVIVAL_REPOSITORY_ID], directory)
    runner_resources.add((SURVIVAL_REPOSITORY_ID, str(survival['runner']['id'])))
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
    owner_scope_times.append(validate_owner_scope(unused_capture, {
        'repository_count': 42, 'disposition': 'Unused by external clients',
        'repositories': unused_capture['repositories']}, directory))
    ledger_context = (inventory, directory, expected, retained, candidates, expected_unused,
                      runner_resources, vercel_resources, other_resources, external_scope)
    ledger_statuses, post_transfer_smokes = validate_ledger_rows(rows, *ledger_context)
    all_transfer_gates_verified = all(all(status == 'verified' for status in ledger_statuses[repo_id])
                                      for repo_id in expected if repo_id not in retained)
    if all_transfer_gates_verified:
        validate_app_credential(matrix.get('pre_transfer_credential_verification'), directory)
        source_refreshed_at, scanned_at, captured_manifests = validate_source_refresh(matrix.get('pre_cutover_source_evidence_url'), directory, inventory,
                                                       matrix['pre_transfer_credential_verification'])
        refreshed_sources = local_capture(matrix['pre_cutover_source_evidence_url'], directory, 'Verified source heads')
        scanned_revisions = {current['id']: source_revision(current['source_head'], expected[current['id']], scanned_at)
            for account in refreshed_sources['accounts'] for current in account['repositories']}
        require(all(at <= source_refreshed_at for at in owner_scope_times),
                'Global owner scope must be authenticated before the pre-cutover source refresh')
        validate_ledger_readiness(matrix.get('pre_cutover_ledger_evidence_url'), rows, directory,
                                 ledger_context, source_refreshed_at, scanned_at)
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
        tree_refreshed_at = validate_tree_refresh(matrix.get('pre_cutover_tree_evidence_url'), directory,
                                                  inventory, source_refreshed_at)

    app_transfer = matrix.get('owned_app_transfer')
    require(isinstance(app_transfer, dict) and app_transfer.get('app_id') == owned_app_id and
            app_transfer.get('status') in {'pending', 'verified'}, 'Owned App needs an explicit transfer state')
    if app_transfer['status'] == 'verified':
        require(all_transfer_gates_verified, 'All transfer-target ledger gates must be verified before App transfer')
        require(app_transfer.get('owner') == inventory['destination_login'], 'Transferred App needs its canonical organization owner')
        app_transferred_at = validate_app_transfer(app_transfer, directory, inventory, tree_refreshed_at)
    if all_transfer_gates_verified:
        cutoff = app_transferred_at if app_transfer['status'] == 'verified' else source_refreshed_at
        require(all(timestamp > cutoff for times in post_transfer_smokes.values() for timestamp in times),
                'Post-transfer smoke observations must follow the independently captured source/App cutover')

    records = matrix.get('repositories')
    require(isinstance(records, list), 'Matrix needs repository records')
    seen_matrix = set()
    verified_rollouts = 0
    rollout_start_times = []
    existing_wider_id = matrix.get('existing_wider_validation_repository_id')
    require(existing_wider_id is None or type(existing_wider_id) is int and existing_wider_id == 1229335234,
            'Existing wider validation must name the designated baseline hs-buddy repository')
    existing_wider_completed_at = None
    protected_source_completed_at = None
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
            validate_protection_preservation(row.get('destination_protections'), directory, repo,
                                             app_transferred_at if app_transfer['status'] == 'verified' else source_refreshed_at)
            destination = local_capture(row['destination_protections']['evidence_url'], directory, 'Destination protections')
            require(all(timestamp >= observed_time(destination['observed_at'], 'Destination protections')
                        for timestamp in post_transfer_smokes.get(repo_id, [])),
                    'Post-transfer smoke observations must follow captured destination metadata')
        require((health == 'retained_source') == (repo_id in retained), 'Matrix must honor retained source decisions')
        protected_source = repo['full_name'] == 'HemSoft/set-it-free-loop'
        require((row.get('rollout_action') == 'protected_source_verify_workflows_in_place') == protected_source,
                'Protected source completion mode must match the distribution repository')
        if repo_id not in retained and (health != 'pending_transfer' or text(row.get('transfer_evidence_url'))):
            require(all_transfer_gates_verified,
                    'All transfer-target ledger gates must be verified before any transfer advances')
            transferred_at = validate_repository_transfer(row.get('transfer_evidence_url'), directory, repo, tree_refreshed_at,
                scanned_revisions[repo_id])
            if health in {'verified', 'source_verified', 'archived_verified', 'scope_exception'}:
                require(transferred_at <= observed_time(destination['observed_at'], 'Destination protections'),
                        'Destination protections must be observed after actual repository transfer')
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
                validate_post_transfer_access(access, directory, repo, transferred_at)
        coverage = row.get('destination_sfl_app_access')
        if (health in {'verified', 'source_verified', 'archived_verified', 'scope_exception'} and
                row['source_app_access_in_baseline']):
            require(isinstance(coverage, dict) and coverage.get('status') == 'verified',
                    'Every baseline-covered terminal repository must resolve repository-bound destination App coverage')
        if coverage == 'verified' or (isinstance(coverage, dict) and coverage.get('status') == 'verified'):
            require(app_transfer['status'] == 'verified', 'Destination private SFL App access requires verified App transfer')
            validate_app_coverage(coverage, directory, repo_id, repo['destination'], owned_app_id, inventory['destination_login'], app_transferred_at)
        if health in {'verified', 'archived_verified', 'scope_exception', 'source_verified'}:
            require(all(status == 'verified' for status in ledger_statuses[repo_id]),
                    'Completed rollout requires every integration row verified')
            for resource_id in (rid for source_id, rid in runner_resources if source_id == repo_id):
                proof = row.get('post_transfer_runner')
                require(isinstance(proof, dict) and proof.get('repository_id') == repo_id and
                        proof.get('repository') == repo['destination'] and str(proof.get('runner_id')) == resource_id and
                        proof.get('online') is True and proof.get('idle') is True and proof.get('run_conclusion') == 'success' and
                        immutable_sha(proof.get('run_head_sha')),
                        'Completed runner transfer needs structured destination continuity and smoke evidence')
                if repo_id == SURVIVAL_REPOSITORY_ID:
                    require(proof.get('startup_model') == 'windows_logon_task' and proof.get('startup_active') is True,
                            'Windows completion needs its preserved active logon startup task')
                    runner_fields = ('registration_evidence_url', 'startup_evidence_url', 'run_url')
                else:
                    require(proof.get('isolated') is True and proof.get('service_active') is True,
                            'Linux structured destination continuity needs its existing isolation and active service')
                    runner_fields = ('registration_evidence_url', 'isolation_evidence_url', 'service_evidence_url', 'run_url')
                require(re.fullmatch('https://github.com/' + re.escape(repo['destination']) + r'/actions/runs/[1-9][0-9]*',
                                     proof.get('run_url', '')) is not None,
                        'Runner smoke must belong to its destination repository')
                for field in runner_fields:
                    evidence(proof.get(field), directory)
                validate_runner_captures(proof, directory, max(source_refreshed_at,
                    app_transferred_at if app_transfer['status'] == 'verified' else source_refreshed_at,
                    observed_time(row['destination_protections']['observed_at'], 'Runner destination transfer')))
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
                decision_at = observed_time(dependency.get('decision_recorded_at'), 'Retained App owner decision')
                validate_owner_approval_comment(dependency.get('owner_comment_evidence_url'), directory,
                    RETAINED_APP_RECEIPT, decision_at,
                    {'repository_ids': sorted(APPROVED_RETAINED_IDS), 'disposition': RETAINED_APP_DISPOSITION}, verified_at)
                if app_transfer['status'] == 'verified':
                    require(verified_at < app_transferred_at,
                            'Retained App dependency verification must precede App transfer')
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
            validate_source_governance(proof, directory, repo, app_transferred_at)
            runs = proof.get('workflow_run_urls')
            require(isinstance(runs, list) and bool(runs), 'Protected source needs in-place workflow runs')
            operations = proof.get('workflow_operation_receipts')
            require(isinstance(operations, list) and len(operations) == len(runs),
                    'Protected source needs bound workflow operation receipts')
            for run, operation in zip(runs, operations):
                # These receipts prove execution at proof['source_sha'], not the
                # current checkout. Retirement must not invalidate immutable history.
                source_workflows = HISTORICAL_SOURCE_WORKFLOWS
                bound_workflow_operation(operation, directory, repo_id, row['destination'],
                                         proof['source_sha'], proof['release_version'], run, source_workflows, proof['source_sha'],
                                         max(app_transferred_at, observed_time(row['destination_protections']['observed_at'], 'Source transfer')))
                execution = local_capture(operation['capture_evidence_url'], directory, 'Source workflow execution')
                require(execution['run'].get('head_branch') == repo['default_branch'],
                        'Protected source workflow must execute on its actual default branch')
            for field in ('transfer_evidence_url', 'status_evidence_url'):
                evidence(row.get(field), directory)
            require(row.get('destination_codex_access') == 'verified' and isinstance(coverage, dict) and coverage.get('status') == 'verified',
                    'Protected source needs verified App coverage')
            # The source owns its workflows; it must not acquire a consumer manifest through init/sync.
            require(row['installed_tier'] is None and row['selected_tier'] is None,
                    'Protected source must not be recorded as a deployed consumer')
            validate_registered_review(row, directory, row["destination"], target_branch=repo["default_branch"], deployment_revision=proof["source_sha"], cutover=max(app_transferred_at, observed_time(row["destination_protections"]["observed_at"], "Source transfer")))
            validate_terminal_protections(row, directory, repo, proof['source_sha'])
            validate_source_default_head(proof, directory, repo,
                repository_terminal_times([row], rows, directory)[repo_id])
            protected_source_completed_at = repository_terminal_times([row], rows, directory)[repo_id]
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
            require(receipt.get('author') == 'HemSoft' and
                    observed_time(receipt.get('updated_at', receipt.get('created_at')), 'Effective scope approval') == approved_at and
                    re.fullmatch(r'https://github.com/(?:HemSoft|hemsoft-dev)/set-it-free-loop/issues/(?:138|139)#issuecomment-[1-9][0-9]*',
                                 receipt.get('url', '')) is not None and receipt.get('decision') == {
                        field: decision[field] for field in ('repository_id', 'repository', 'disposition', 'reason')},
                    'Scope exception needs its captured HemSoft issue decision with matching repository and disposition')
            validate_owner_approval_comment(capture.get('owner_comment_evidence_url'), directory, receipt['url'],
                approved_at, receipt['decision'], observed_time(capture.get('observed_at'), 'Scope exception capture'))
            evidence(row.get('transfer_evidence_url'), directory)
            evidence(row.get('status_evidence_url'), directory)
        elif health == 'archived_verified':
            require(row['archived'] is True, 'Active repository cannot use archived completion')
            evidence(row.get('transfer_evidence_url'), directory)
            validate_archived_status(row, directory, repo,
                max(transferred_at, observed_time(destination['observed_at'], 'Destination protections')))
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
            rollout_start_times.append((repo_id, validate_pre_sync_installation(observed, directory, row)))
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
            validate_app_coverage(coverage, directory, repo_id, repo['destination'], owned_app_id, inventory['destination_login'], app_transferred_at)
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
            validate_consumer_status(row, directory, revision, max(app_transferred_at,
                observed_time(row['destination_protections']['observed_at'], 'Consumer transfer')))
            operations = row.get('wider_operation_receipts', [])
            require(isinstance(operations, list) and len(operations) == len(runs),
                    'Consumer wider workflows need bound operation receipts')
            for run, operation in zip(runs, operations):
                bound_workflow_operation(operation, directory, repo_id, row['destination'],
                                         row['deployment_sha'], row['manifest_version'], run,
                                         deployed_workflow_paths(row['selected_tier'], row['selected_addons'],
                                                                 row['selected_components']) - {'.github/workflows/sfl-pr-review-auto.yml'}, revision,
                                         max(app_transferred_at, observed_time(row['destination_protections']['observed_at'], 'Consumer transfer')))
            validate_registered_review(row, directory, row['destination'], target_branch=repo['default_branch'], deployment_revision=revision, cutover=max(app_transferred_at, observed_time(row['destination_protections']['observed_at'], 'Consumer transfer')))
            validate_terminal_protections(row, directory, repo, revision)
            validate_consumer_default_head(row, directory, repo, revision,
                repository_terminal_times([row], rows, directory)[repo_id])
            if repo_id == existing_wider_id:
                validate_existing_wider_pilot(row, directory, revision,
                    max(app_transferred_at, observed_time(row['destination_protections']['observed_at'], 'Consumer transfer')))
                existing_wider_completed_at = repository_terminal_times([row], rows, directory)[repo_id]
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
    pilot_branches = {}
    pilot_terminal_times = []
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
            metadata_capture = local_capture(receipts.get('metadata_evidence_url'), directory, 'Pilot metadata')
            metadata = metadata_capture.get('metadata', {})
            require(metadata.get('id') == extra_id and metadata.get('full_name') == name and
                    metadata.get('private') == (extra['visibility'] == 'private') and metadata.get('visibility') == extra['visibility'] and metadata.get('archived') is False and
                    text(metadata.get('default_branch')) and
                    observed_time(metadata_capture.get('observed_at'), 'Pilot metadata') >= app_transferred_at,
                    'Pilot metadata must prove its current destination identity and actual default branch after cutover')
            pilot_branches[extra_id] = metadata['default_branch']
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
                                  owned_app_id, inventory['destination_login'], app_transferred_at)
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
                    require(receipts[field].startswith('https://github.com/' + name + '/') or
                            receipts[field].startswith('https://api.github.com/repos/' + name + '/'),
                            'Pilot operation URL must belong to its designated repository')
            for field in ('init_pr_url', 'sync_pr_url'):
                require(re.fullmatch('https://github.com/' + re.escape(name) + r'/pull/[1-9][0-9]*', receipts[field]) is not None,
                        'Pilot onboarding PR must belong to its designated repository')
            onboarding = ('init_pr_url', 'repeat_onboarding_evidence_url', 'sync_pr_url',
                          'repeat_sync_evidence_url', 'status_evidence_url', 'gate_uninstall_evidence_url')
            operation_times = {}
            for field in onboarding:
                operation = operations[field]
                command = ('init' if field in {'init_pr_url', 'repeat_onboarding_evidence_url'} else
                           'sync' if field in {'sync_pr_url', 'repeat_sync_evidence_url'} else
                           'status' if field == 'status_evidence_url' else 'uninstall-gate')
                outcome = 'pull_request_merged' if field in {'init_pr_url', 'sync_pr_url'} else 'healthy' if command == 'status' else 'gate_removed' if command == 'uninstall-gate' else 'no_changes'
                require(operation.get('command') == command and operation.get('outcome') == outcome and
                        immutable_sha(operation.get('revision_before')) and immutable_sha(operation.get('revision_after')),
                        'Pilot onboarding operations need successful command-specific terminal outcomes')
                operation_times[field] = validate_terminal_operation(operation, directory, app_transferred_at)
                if outcome == 'pull_request_merged':
                    require(operation.get('merged') is True, 'Pilot init and sync must prove merged deployment PRs')
                elif outcome == 'no_changes':
                    require(operation.get('change_count') == 0 and operation['revision_before'] == operation['revision_after'],
                            'Pilot repeated init and sync must prove zero changes at the same revision')
                elif outcome == 'gate_removed':
                    require(operation.get('gate_only') is True and operation.get('unrelated_change_count') == 0,
                            'Pilot gate uninstall must prove safe gate-only removal')
            init_capture = local_capture(operations['init_pr_url']['capture_evidence_url'], directory, 'Pilot init execution')
            pilot_started_at = observed_time(init_capture.get('started_at'), 'Pilot init execution start')
            require(app_transferred_at <= pilot_started_at <= operation_times['init_pr_url'],
                    'Pilot init execution start must follow App cutover and precede its successful observation')
            require(protected_source_completed_at is not None and protected_source_completed_at < pilot_started_at,
                    'Protected source must finish all verification before pilot execution starts')
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
                validate_pilot_scenario(result, scenario, directory, extra_id, name, receipts['deployment_sha'],
                                        receipts['release_version'], revision,
                                        max(app_transferred_at, observed_time(local_capture(receipts['manifest_evidence_url'], directory, 'Pilot manifest')['observed_at'], 'Pilot manifest')))
            identity = receipts.get('review_artifact_identity')
            require(isinstance(identity, dict) and identity.get('runtime') == 'sfl_registered_codex' and
                    identity.get('app_id') == 1144995 and identity.get('bot_user_id') == 199175422 and
                    all(isinstance(identity.get(field), str) and
                        re.fullmatch(r'[0-9a-f]{40}', identity[field])
                        for field in ('reviewed_head_sha', 'reviewed_base_sha')),
                    'Verified pilot needs immutable SFL registered Codex review identity')
            validate_registered_review(receipts, directory, name, extra_id, target_branch=metadata['default_branch'], deployment_revision=revision, cutover=app_transferred_at)
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
                                             {'.github/workflows/sfl-pr-review-auto.yml'}, revision, app_transferred_at)
                bound_workflow_operation(receipts.get('auditor_operation_receipt'), directory, extra_id, name,
                                         receipts['deployment_sha'], receipts['release_version'], receipts['auditor_run_url'],
                                         {'.github/workflows/sfl-auditor.yml'}, revision, app_transferred_at)
                require(any(operation['workflow'] != '.github/workflows/sfl-auditor.yml' and
                            run != receipts['auditor_run_url'] for run,operation in zip(runs,wider_receipts)),
                        'Wider pilot must execute a distinct successful non-Auditor workflow')
                verified_wider_pilot = True
            validate_pilot_cleanup(receipts, directory, extra_id, name, metadata['default_branch'], operations, operation_times)
            pilot_terminal_times.append(repository_terminal_times(
                [{'repository_id':extra_id, 'validation':receipts}], [], directory)[extra_id])
            verified_pilot_visibilities.add(extra['visibility'])
        extra_ids.add(extra_id)
        extra_names.add(name.casefold())
    require(extra_ids == set(APPROVED_PILOTS), 'Both designated disposable pilot identities must remain recorded')
    source_complete = all(row['health'] in {'verified', 'archived_verified', 'scope_exception', 'retained_source', 'source_verified'} for row in records)
    if rollout_start_times or source_complete:
        require(all(row['retained_app_dependency']['status'] == 'verified' for row in records
                    if row['health'] == 'retained_source'),
                'Completed rollout requires verified retained App dependencies')
        require(verified_pilot_visibilities == {'public', 'private'},
                'Active rollout requires verified public and private onboarding pilots first')
        validate_wider_pilot_ordering(verified_wider_pilot, existing_wider_id,
                                     existing_wider_completed_at, rollout_start_times)
        require(all(finished < started for finished in pilot_terminal_times for _, started in rollout_start_times),
                'Both pilots must finish all validation before the first active rollout deployment')
    if source_complete:
        require(all(row.get('smoke_phase') == 'post_transfer' for row in rows
                    if row.get('provider') != 'none' and row.get('status') == 'verified'),
                'Final completion needs post-transfer resource continuity, beyond pre-transfer readiness')
        require(all(status == 'verified' for statuses in ledger_statuses.values() for status in statuses),
                'Final completion needs every retained and transferred resource verified')
        onboarding = validate_final_onboarding(matrix.get('post_rollout_onboarding'), directory, expected, inventory['destination_login'], owned_app_id, app_transferred_at,
                                            repository_terminal_times(records, rows, directory))
        rollout_completed_at = observed_time(onboarding['rollout_completed_at'], 'Rollout completion')
        for resource in account_resources:
            validate_unlinked_supabase(resource, unlinked_supabase[resource['resource_id']], directory,
                                       max(rollout_completed_at, app_transferred_at))
        proof = matrix.get('final_inventory')
        validate_final_inventory(proof, directory, expected, retained, onboarding, pilot_branches)
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
