"""Synthetic ciphertext provenance and selected-recipient API contracts."""
import base64
import copy
import hashlib
import importlib.util
import io
import json
import pathlib
import unittest
import zipfile

ROOT = pathlib.Path(__file__).parents[2]
spec = importlib.util.spec_from_file_location('org_delivery', ROOT / 'deployment/scripts/deliver-sfl-org-credential.py')
delivery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(delivery)
SOURCE = 'HemSoft/set-it-free-loop'
SHA = 'a' * 40


class FakeGitHub:
    def __init__(self):
        self.policy = json.loads((ROOT / 'deployment/orgseal/recipient-policy.json').read_text())
        self.run = {'repository': {'id': delivery.SOURCE_ID, 'full_name': SOURCE}, 'id': 1, 'head_sha': SHA,
                    'head_branch': 'main', 'event': 'workflow_dispatch', 'path': delivery.WORKFLOW,
                    'status': 'completed', 'conclusion': 'success', 'actor': {'login': 'HemSoft'},
                    'triggering_actor': {'login': 'HemSoft'}, 'run_attempt': 1,
                    'run_started_at': '2026-10-07T02:00:00Z', 'updated_at': '2026-10-07T02:02:00Z'}
        self.envelope = {'recipient': self.policy, 'schema': 'sealed_sfl_app_credential_v1',
                         'encryption': 'libsodium_crypto_box_seal', 'encrypted_value': base64.b64encode(b'x' * 100).decode(),
                         'observed_at': '2026-10-07T02:01:00Z', 'context': {'source_repository': SOURCE,
                         'reviewed_sha': SHA, 'run_id': 1, 'run_attempt': 1,
                         'workflow_ref': SOURCE + '/' + delivery.WORKFLOW + '@refs/heads/main'}}
        self.metadata = {'repository_id': delivery.SOURCE_ID, 'repository': SOURCE, 'reviewed_sha': SHA,
                         'run_id': 1, 'run_attempt': 1, 'app_id': 4448946, 'client_id': self.policy['client_id'],
                         'owner': 'HemSoft', 'installation_id': 150383874, 'repository_selection': 'all',
                         'permission_ceiling_verified': True, 'credential_verification': 'success',
                         'observed_at': '2026-10-07T02:01:01Z'}
        self.artifact = {'id': 2, 'name': delivery.ARTIFACT, 'expired': False,
                         'workflow_run': {'id': 1, 'head_sha': SHA}, 'created_at': '2026-10-07T02:01:10Z'}
        self.archive = b''
        self.reseal_archive()
        self.data = {'user': {'login': 'HemSoft'}, 'orgs/hemsoft-dev': {'id': delivery.ORG_ID, 'login': 'hemsoft-dev'},
                     'orgs/hemsoft-dev/memberships/HemSoft': {'state': 'active', 'role': 'admin'},
                     'repos/' + SOURCE: {'id': delivery.SOURCE_ID, 'full_name': SOURCE, 'default_branch': 'main'},
                     'repos/' + SOURCE + '/git/ref/heads/main': {'ref': 'refs/heads/main', 'object': {'type': 'commit', 'sha': SHA}},
                     'repos/' + delivery.PILOT_NAME: {'id': delivery.PILOT_ID, 'full_name': delivery.PILOT_NAME,
                                                    'private': True, 'visibility': 'private', 'archived': False},
                     'orgs/hemsoft-dev/actions/secrets/public-key': {'key_id': self.policy['key_id'], 'key': self.policy['public_key']},
                     'orgs/hemsoft-dev/actions/secrets/SFL_APP_PRIVATE_KEY': None}
        self.calls = []

    def reseal_archive(self):
        stream = io.BytesIO()
        with zipfile.ZipFile(stream, 'w') as container:
            container.writestr('sealed-sfl-app-credential.json', json.dumps(self.envelope))
            container.writestr('sealed-sfl-app-metadata.json', json.dumps(self.metadata))
        self.archive = stream.getvalue()
        self.artifact['digest'] = 'sha256:' + hashlib.sha256(self.archive).hexdigest()

    def call(self, endpoint, method='GET', body=None, optional_missing=False, binary=False):
        self.calls.append((method, endpoint, copy.deepcopy(body)))
        if method == 'POST':
            self.data[endpoint + '/' + body['name']] = body
            return None
        if method == 'PUT':
            self.data[endpoint] = dict(body, name=delivery.SECRET)
            return None
        if endpoint.endswith('/actions/runs/1'):
            return copy.deepcopy(self.run)
        if endpoint.endswith('/contents/deployment/orgseal/recipient-policy.json?ref=' + SHA):
            return {'type': 'file', 'encoding': 'base64', 'content': base64.b64encode(json.dumps(self.policy).encode()).decode()}
        if endpoint.endswith('/actions/runs/1/artifacts?per_page=100'):
            return {'total_count': 1, 'artifacts': [copy.deepcopy(self.artifact)]}
        if endpoint.endswith('/actions/artifacts/2/zip'):
            return self.archive
        if endpoint.endswith('/repositories?per_page=100'):
            return {'total_count': 1, 'repositories': [{'id': delivery.PILOT_ID}]}
        if endpoint in self.data:
            return copy.deepcopy(self.data[endpoint])
        if optional_missing:
            return None
        raise AssertionError('Unexpected synthetic API operation: ' + endpoint)

    def pages(self, endpoint):
        return [copy.deepcopy(self.artifact)]


class DeliveryTests(unittest.TestCase):
    def test_bare_and_main_qualified_run_paths_support_preparation_and_apply(self):
        for path in (delivery.WORKFLOW, delivery.WORKFLOW + '@main'):
            for apply in (False, True):
                with self.subTest(path=path, apply=apply):
                    client = FakeGitHub()
                    client.run['path'] = path
                    result = delivery.deliver(client, SOURCE, SHA, 1, client.policy, apply=apply)
                    self.assertEqual(result['applied'], apply)
                    writes = [method for method, _, _ in client.calls if method != 'GET']
                    self.assertEqual(writes, ['POST', 'POST', 'PUT'] if apply else [])

    def test_other_qualified_refs_fail_before_artifact_download_or_writes(self):
        for path in (delivery.WORKFLOW + '@topic', delivery.WORKFLOW + '@refs/tags/main',
                     delivery.WORKFLOW + '@main@topic', '.github/workflows/other.yml@main'):
            with self.subTest(path=path):
                client = FakeGitHub()
                client.run['path'] = path
                with self.assertRaises(delivery.DeliveryError):
                    delivery.deliver(client, SOURCE, SHA, 1, client.policy, apply=True)
                self.assertTrue(all(method == 'GET' for method, _, _ in client.calls))
                self.assertFalse(any('/artifacts' in endpoint for _, endpoint, _ in client.calls))

    def test_preparation_reads_only_and_omits_ciphertext(self):
        client = FakeGitHub()
        result = delivery.deliver(client, SOURCE, SHA, 1, client.policy)
        self.assertFalse(result['applied'])
        self.assertTrue(all(method == 'GET' for method, _, _ in client.calls))
        self.assertNotIn('encrypted_value', json.dumps(result))
        self.assertEqual(result['selected_repository_ids'], [delivery.PILOT_ID])

    def test_apply_sends_only_ciphertext_and_exact_private_selection(self):
        client = FakeGitHub()
        result = delivery.deliver(client, SOURCE, SHA, 1, client.policy, apply=True)
        self.assertTrue(result['applied'])
        writes = [row for row in client.calls if row[0] != 'GET']
        self.assertEqual([method for method, _, _ in writes], ['POST', 'POST', 'PUT'])
        for _, _, body in writes:
            self.assertEqual(body['visibility'], 'selected')
            self.assertEqual(body['selected_repository_ids'], [delivery.PILOT_ID])
        self.assertEqual(writes[-1][2]['encrypted_value'], client.envelope['encrypted_value'])
        self.assertEqual(set(writes[-1][2]), {'encrypted_value', 'key_id', 'visibility', 'selected_repository_ids'})

    def test_changed_live_context_never_writes(self):
        cases = [('actor', lambda c: c.data['user'].update(login='other')),
                 ('organization', lambda c: c.data['orgs/hemsoft-dev'].update(id=42)),
                 ('role', lambda c: c.data['orgs/hemsoft-dev/memberships/HemSoft'].update(role='member')),
                 ('main', lambda c: c.data['repos/' + SOURCE + '/git/ref/heads/main']['object'].update(sha='b' * 40)),
                 ('public-pilot', lambda c: c.data['repos/' + delivery.PILOT_NAME].update(private=False, visibility='public')),
                 ('recipient-key', lambda c: c.data['orgs/hemsoft-dev/actions/secrets/public-key'].update(key_id='1')),
                 ('existing-secret', lambda c: c.data.update({'orgs/hemsoft-dev/actions/secrets/SFL_APP_PRIVATE_KEY': {'name': delivery.SECRET}}))]
        for name, mutation in cases:
            client = FakeGitHub()
            mutation(client)
            with self.subTest(case=name), self.assertRaises(delivery.DeliveryError):
                delivery.deliver(client, SOURCE, SHA, 1, client.policy, apply=True)
            self.assertTrue(all(method == 'GET' for method, _, _ in client.calls))

    def test_run_and_artifact_provenance_rejects_other_attempts(self):
        for field, value in [('head_sha', 'b' * 40), ('head_branch', 'untrusted'), ('event', 'push'),
                             ('path', '.github/workflows/other.yml'), ('status', 'in_progress'),
                             ('conclusion', 'failure'), ('actor', {'login': 'other'}),
                             ('triggering_actor', {'login': 'other'}), ('run_attempt', 2)]:
            client = FakeGitHub()
            client.run[field] = value
            with self.subTest(field=field), self.assertRaises(delivery.DeliveryError):
                delivery.deliver(client, SOURCE, SHA, 1, client.policy, apply=True)
            self.assertTrue(all(method == 'GET' for method, _, _ in client.calls))

    def test_copied_or_tampered_artifact_metadata_never_writes(self):
        for target, field, value in [('context', 'reviewed_sha', 'b' * 40), ('context', 'run_attempt', 2),
                                     ('recipient', 'selected_repository_ids', [1408029795]),
                                     ('metadata', 'permission_ceiling_verified', False),
                                     ('metadata', 'credential_verification', 'failure'),
                                     ('metadata', 'repository_selection', 'selected'),
                                     ('metadata', 'app_id', 1), ('metadata', 'extra_provider_response', 'unapproved'),
                                     ('envelope', 'encrypted_value', 'not-base64'),
                                     ('envelope', 'observed_at', '2026-10-07T01:00:00Z')]:
            client = FakeGitHub()
            data = client.metadata if target == 'metadata' else client.envelope if target == 'envelope' else client.envelope[target]
            data[field] = value
            client.reseal_archive()
            with self.subTest(target=target, field=field), self.assertRaises(delivery.DeliveryError):
                delivery.deliver(client, SOURCE, SHA, 1, client.policy, apply=True)
            self.assertTrue(all(method == 'GET' for method, _, _ in client.calls))
        client = FakeGitHub()
        client.archive += b'tampered'
        with self.assertRaises(delivery.DeliveryError):
            delivery.deliver(client, SOURCE, SHA, 1, client.policy)

    def test_changed_reviewed_policy_or_existing_variable_is_rejected(self):
        client = FakeGitHub()
        local_policy = copy.deepcopy(client.policy)
        local_policy['public_key'] = base64.b64encode(b'z' * 32).decode()
        local_policy['public_key_sha256'] = hashlib.sha256(b'z' * 32).hexdigest()
        with self.assertRaises(delivery.DeliveryError):
            delivery.deliver(client, SOURCE, SHA, 1, local_policy)
        client = FakeGitHub()
        client.data['orgs/hemsoft-dev/actions/variables/SFL_APP_ID'] = {'name': 'SFL_APP_ID', 'value': 'other', 'visibility': 'all'}
        with self.assertRaises(delivery.DeliveryError):
            delivery.deliver(client, SOURCE, SHA, 1, client.policy, apply=True)
        self.assertTrue(all(method == 'GET' for method, _, _ in client.calls))


if __name__ == '__main__':
    unittest.main()
