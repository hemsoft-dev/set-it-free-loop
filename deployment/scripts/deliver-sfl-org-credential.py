#!/usr/bin/env python3
"""Validate a reviewed-main ciphertext artifact; apply only with an explicit flag."""
import argparse
import base64
import datetime
import hashlib
import io
import json
import pathlib
import re
import subprocess
import zipfile

ORG = 'hemsoft-dev'
ORG_ID = 338855369
SOURCE_ID = 1169772257
PILOT_ID = 1408025382
PILOT_NAME = ORG + '/sfl-migration-pilot-private'
WORKFLOW = '.github/workflows/seal-sfl-org-credential.yml'
ARTIFACT = 'sealed-sfl-app-credential'
SECRET = 'SFL_APP_PRIVATE_KEY'
FILES = {'sealed-sfl-app-credential.json', 'sealed-sfl-app-metadata.json'}


class DeliveryError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise DeliveryError(message)


def strict_json(raw):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, 'Duplicate JSON property.')
            result[key] = value
        return result
    try:
        return json.loads(raw, object_pairs_hook=unique)
    except (ValueError, UnicodeError) as exc:
        raise DeliveryError('Invalid JSON capture.') from exc


class GitHub:
    def call(self, endpoint, method='GET', body=None, optional_missing=False, binary=False):
        # gh owns authentication. No token enters arguments, payloads, receipts or logs.
        argv = ['gh', 'api', '--method', method, endpoint]
        if body is not None:
            argv += ['--input', '-']
        result = subprocess.run(argv, input=json.dumps(body).encode() if body is not None else None,
                                capture_output=True)
        if result.returncode:
            if optional_missing and b'(HTTP 404)' in result.stderr:
                return None
            raise DeliveryError('GitHub API operation failed: ' + method + ' ' + endpoint)
        if binary:
            return result.stdout
        return strict_json(result.stdout) if result.stdout.strip() else None

    def pages(self, endpoint):
        # The API total is reconciled below; enumerate explicit pages without auth redirects.
        result = []
        for page in range(1, 101):
            data = self.call(endpoint + ('&' if '?' in endpoint else '?') + 'per_page=100&page=' + str(page))
            rows = data['artifacts'] if isinstance(data, dict) and 'artifacts' in data else data
            require(isinstance(rows, list), 'Invalid repository/artifact enumeration.')
            result.extend(rows)
            if len(rows) < 100:
                return result
        raise DeliveryError('Enumeration exceeded its page limit.')


def timestamp(value):
    try:
        parsed = datetime.datetime.fromisoformat(value.replace('Z', '+00:00'))
    except (AttributeError, ValueError) as exc:
        raise DeliveryError('Invalid observation timestamp.') from exc
    require(parsed.tzinfo is not None, 'Observation timestamp has no timezone.')
    return parsed


def validate_policy(policy):
    fields = {'organization', 'organization_id', 'key_id', 'public_key', 'public_key_sha256',
              'selected_repository_ids', 'source_repository_id', 'app_id', 'client_id'}
    require(isinstance(policy, dict) and set(policy) == fields, 'Unexpected recipient policy fields.')
    require(policy['organization'] == ORG and policy['organization_id'] == ORG_ID and
            policy['selected_repository_ids'] == [PILOT_ID] and policy['source_repository_id'] == SOURCE_ID and
            policy['app_id'] == 4448946 and policy['client_id'] == 'Iv23liwvwJJUh2bUIKLW',
            'Unapproved recipient or App identity.')
    try:
        raw = base64.b64decode(policy['public_key'], validate=True)
    except (ValueError, TypeError) as exc:
        raise DeliveryError('Invalid public recipient key.') from exc
    require(len(raw) == 32 and hashlib.sha256(raw).hexdigest() == policy['public_key_sha256'] and
            re.fullmatch(r'[1-9][0-9]*', policy['key_id']) is not None, 'Invalid recipient key identity.')


def validate_artifact(archive, artifact, run, policy, source, sha):
    require(len(archive) <= 1024 * 1024 and artifact.get('digest') == 'sha256:' + hashlib.sha256(archive).hexdigest(),
            'Artifact bytes differ from the GitHub server digest.')
    try:
        with zipfile.ZipFile(io.BytesIO(archive)) as container:
            entries = container.infolist()
            require(len(entries) == 2 and {entry.filename for entry in entries} == FILES and
                    all(not entry.is_dir() and entry.file_size <= 128 * 1024 for entry in entries),
                    'Artifact has unexpected files or size.')
            envelope = strict_json(container.read('sealed-sfl-app-credential.json'))
            metadata = strict_json(container.read('sealed-sfl-app-metadata.json'))
    except (zipfile.BadZipFile, KeyError, RuntimeError) as exc:
        raise DeliveryError('Invalid sealed artifact archive.') from exc
    require(set(envelope) == {'recipient', 'context', 'schema', 'encryption', 'encrypted_value', 'observed_at'} and
            envelope['recipient'] == policy and envelope['schema'] == 'sealed_sfl_app_credential_v1' and
            envelope['encryption'] == 'libsodium_crypto_box_seal', 'Artifact recipient/encryption does not match reviewed policy.')
    expected_context = {'source_repository': source, 'reviewed_sha': sha, 'run_id': run['id'],
                        'run_attempt': run['run_attempt'], 'workflow_ref': source + '/' + WORKFLOW + '@refs/heads/main'}
    require(envelope['context'] == expected_context, 'Ciphertext context does not match the reviewed run.')
    allowed = {'repository_id', 'repository', 'reviewed_sha', 'run_id', 'run_attempt', 'app_id', 'client_id', 'owner',
               'installation_id', 'repository_selection', 'permission_ceiling_verified', 'credential_verification', 'observed_at'}
    require(set(metadata) == allowed and metadata['repository_id'] == SOURCE_ID and metadata['repository'] == source and
            metadata['reviewed_sha'] == sha and metadata['run_id'] == run['id'] and metadata['run_attempt'] == run['run_attempt'] and
            metadata['app_id'] == policy['app_id'] and metadata['client_id'] == policy['client_id'] and
            metadata['owner'] == source.split('/')[0] and type(metadata['installation_id']) is int and
            metadata['installation_id'] > 0 and metadata['repository_selection'] == 'all' and
            metadata['permission_ceiling_verified'] is True and metadata['credential_verification'] == 'success',
            'App authentication metadata does not match its reviewed run.')
    for value in (envelope['observed_at'], metadata['observed_at']):
        require(timestamp(run['run_started_at']) <= timestamp(value) <= timestamp(run['updated_at']),
                'Artifact observations are outside the completed run attempt.')
    try:
        ciphertext = base64.b64decode(envelope['encrypted_value'], validate=True)
    except (ValueError, TypeError) as exc:
        raise DeliveryError('Invalid sealed ciphertext encoding.') from exc
    require(48 < len(ciphertext) <= 65536 + 48, 'Invalid sealed ciphertext length.')
    return envelope['encrypted_value']


def preflight(client, source, sha, run_id, policy):
    validate_policy(policy)
    require(client.call('user')['login'] == 'HemSoft', 'The authenticated actor must be HemSoft.')
    organization = client.call('orgs/' + ORG)
    require(organization['id'] == ORG_ID and organization['login'] == ORG, 'Wrong organization.')
    membership = client.call('orgs/' + ORG + '/memberships/HemSoft')
    require(membership.get('role') == 'admin' and membership.get('state') == 'active', 'Active owner membership required.')
    repository = client.call('repos/' + source)
    require(repository['id'] == SOURCE_ID and repository['full_name'] == source and repository['default_branch'] == 'main',
            'Wrong source repository identity or default branch.')
    ref = client.call('repos/' + source + '/git/ref/heads/main')
    require(ref.get('ref') == 'refs/heads/main' and ref.get('object', {}).get('type') == 'commit' and
            ref['object']['sha'] == sha, 'Reviewed source main advanced; prepare a fresh artifact.')
    pilot = client.call('repos/' + PILOT_NAME)
    require(pilot['id'] == PILOT_ID and pilot['full_name'] == PILOT_NAME and pilot['private'] is True and
            pilot['visibility'] == 'private' and pilot['archived'] is False, 'The selected private pilot changed.')
    public = client.call('orgs/' + ORG + '/actions/secrets/public-key')
    require(public == {'key_id': policy['key_id'], 'key': policy['public_key']}, 'Organization encryption key changed.')
    run = client.call('repos/' + source + '/actions/runs/' + str(run_id))
    require(run['repository']['id'] == SOURCE_ID and run['repository']['full_name'] == source and run['id'] == run_id and
            run['head_sha'] == sha and run['head_branch'] == 'main' and run['event'] == 'workflow_dispatch' and
            run['path'] == WORKFLOW and run['status'] == 'completed' and run['conclusion'] == 'success' and
            run['actor']['login'] == 'HemSoft' and run['triggering_actor']['login'] == 'HemSoft' and
            type(run['run_attempt']) is int and run['run_attempt'] > 0, 'Run is not a successful reviewed owner dispatch.')
    # Obtain the public policy from the exact executed Git revision, not a caller-supplied substitute.
    contents = client.call('repos/' + source + '/contents/deployment/orgseal/recipient-policy.json?ref=' + sha)
    require(contents.get('type') == 'file' and contents.get('encoding') == 'base64' and
            strict_json(base64.b64decode(contents['content'])) == policy, 'Recipient policy differs from executed source.')
    artifact_list = client.call('repos/' + source + '/actions/runs/' + str(run_id) + '/artifacts?per_page=100')
    artifacts = client.pages('repos/' + source + '/actions/runs/' + str(run_id) + '/artifacts')
    require(len(artifacts) == artifact_list['total_count'], 'Incomplete run artifact enumeration.')
    matched = [item for item in artifacts if item.get('name') == ARTIFACT and item.get('expired') is False]
    require(len(matched) == 1, 'Run needs exactly one unexpired sealed artifact.')
    artifact = matched[0]
    require(artifact.get('workflow_run', {}).get('id') == run_id and artifact['workflow_run'].get('head_sha') == sha and
            timestamp(run['run_started_at']) <= timestamp(artifact['created_at']) <= timestamp(run['updated_at']),
            'Artifact is not bound to the current run attempt.')
    archive = client.call('repos/' + source + '/actions/artifacts/' + str(artifact['id']) + '/zip', binary=True)
    encrypted = validate_artifact(archive, artifact, run, policy, source, sha)
    require(client.call('orgs/' + ORG + '/actions/secrets/' + SECRET, optional_missing=True) is None,
            'An existing organization secret requires separate replacement approval.')
    variables = {'SFL_APP_ID': str(policy['app_id']), 'SFL_APP_CLIENT_ID': policy['client_id']}
    for name, value in variables.items():
        current = client.call('orgs/' + ORG + '/actions/variables/' + name, optional_missing=True)
        if current is not None:
            selected = client.call('orgs/' + ORG + '/actions/variables/' + name + '/repositories?per_page=100')
            require(current.get('value') == value and current.get('visibility') == 'selected' and
                    selected.get('total_count') == 1 and [r['id'] for r in selected.get('repositories', [])] == [PILOT_ID],
                    'Existing App variable differs from selected-pilot policy.')
    return encrypted, artifact, variables


def deliver(client, source, sha, run_id, policy, apply=False):
    encrypted, artifact, variables = preflight(client, source, sha, run_id, policy)
    receipt = {'organization': ORG, 'organization_id': ORG_ID, 'selected_repository_ids': [PILOT_ID],
               'source_repository': source, 'reviewed_sha': sha, 'run_id': run_id, 'artifact_id': artifact['id'],
               'artifact_digest': artifact['digest'], 'key_id': policy['key_id'], 'secret_name': SECRET,
               'variable_names': sorted(variables), 'applied': False}
    if apply:
        # Repeat live preflight immediately before writes; compare artifact and ciphertext again.
        fresh, current_artifact, current_variables = preflight(client, source, sha, run_id, policy)
        require(fresh == encrypted and current_artifact['id'] == artifact['id'] and current_variables == variables,
                'Delivery changed after preparation.')
        for name, value in variables.items():
            if client.call('orgs/' + ORG + '/actions/variables/' + name, optional_missing=True) is None:
                client.call('orgs/' + ORG + '/actions/variables', method='POST', body={
                    'name': name, 'value': value, 'visibility': 'selected', 'selected_repository_ids': [PILOT_ID]})
        client.call('orgs/' + ORG + '/actions/secrets/' + SECRET, method='PUT', body={
            'encrypted_value': encrypted, 'key_id': policy['key_id'], 'visibility': 'selected',
            'selected_repository_ids': [PILOT_ID]})
        secret_metadata = client.call('orgs/' + ORG + '/actions/secrets/' + SECRET)
        require(secret_metadata.get('name') == SECRET and secret_metadata.get('visibility') == 'selected',
                'Organization secret visibility does not match selected-pilot policy.')
        for name, value in variables.items():
            current = client.call('orgs/' + ORG + '/actions/variables/' + name)
            recipients = client.call('orgs/' + ORG + '/actions/variables/' + name + '/repositories?per_page=100')
            require(current.get('value') == value and current.get('visibility') == 'selected' and
                    recipients.get('total_count') == 1 and [r['id'] for r in recipients.get('repositories', [])] == [PILOT_ID],
                    'Public App variable selection does not match the private pilot.')
        selected = client.call('orgs/' + ORG + '/actions/secrets/' + SECRET + '/repositories?per_page=100')
        require(selected.get('total_count') == 1 and [r['id'] for r in selected.get('repositories', [])] == [PILOT_ID],
                'Organization secret selection did not match the private pilot.')
        receipt['applied'] = True
    receipt['observed_at'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', choices=['HemSoft/set-it-free-loop', 'hemsoft-dev/set-it-free-loop'], required=True)
    parser.add_argument('--reviewed-sha', required=True)
    parser.add_argument('--run-id', type=int, required=True)
    parser.add_argument('--apply', action='store_true', help='Requires explicit owner approval for selected-pilot credential delivery.')
    args = parser.parse_args()
    require(re.fullmatch(r'[0-9a-f]{40}', args.reviewed_sha) is not None and args.run_id > 0, 'Invalid reviewed run arguments.')
    policy = strict_json((pathlib.Path(__file__).parents[1] / 'orgseal/recipient-policy.json').read_bytes())
    receipt = deliver(GitHub(), args.source, args.reviewed_sha, args.run_id, policy, args.apply)
    print(json.dumps(receipt, indent=2))


if __name__ == '__main__':
    try:
        main()
    except DeliveryError as exc:
        raise SystemExit(str(exc))
