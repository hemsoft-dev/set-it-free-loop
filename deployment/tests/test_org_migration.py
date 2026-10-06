"""Offline safety checks for the read-only migration inventory."""

import importlib.util
import json
import pathlib
import subprocess
import unittest
from unittest.mock import patch

SCRIPT = pathlib.Path(__file__).parents[1] / 'scripts' / 'capture-org-migration.py'
spec = importlib.util.spec_from_file_location('capture_org_migration', SCRIPT)
capture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(capture)


class MigrationTests(unittest.TestCase):
    def snapshot(self):
        return {
            'source_owners': ['HemSoft', 'fhemmer'], 'destination_login': 'hemsoft-dev',
            'repositories': [
                {'id': 1, 'full_name': 'HemSoft/hs-cli-confluence-search',
                 'destination': 'hemsoft-dev/hs-cli-confluence-search',
                 'private': False, 'archived': False, 'settings': {}},
                {'id': 2, 'full_name': 'fhemmer/hs-cli-confluence-search',
                 'destination': 'hemsoft-dev/hs-cli-confluence-search-fhemmer',
                 'private': True, 'archived': False,
                 'settings': {'app_installation': {'state': 'unverified', 'http_status': 404}}},
            ],
        }

    def test_preserve_collision_and_visibility(self):
        result = capture.validate(self.snapshot())
        self.assertEqual(result, {'repositories': 2, 'private': 1, 'archived': 0, 'unverified_endpoints': 1})

    def test_collector_maps_both_colliding_repositories(self):
        for owner, private, expected in [
            ('HemSoft', False, 'hemsoft-dev/hs-cli-confluence-search'),
            ('fhemmer', True, 'hemsoft-dev/hs-cli-confluence-search-fhemmer'),
        ]:
            repo = {'id': 1 if owner == 'HemSoft' else 2, 'name': 'hs-cli-confluence-search',
                    'full_name': owner + '/hs-cli-confluence-search', 'private': private}
            with patch.object(capture, 'api', return_value={'state': 'unverified'}):
                record = capture.capture_repo(repo, 'hemsoft-dev')
            self.assertEqual(record['destination'], expected)
            self.assertEqual(record['private'], private)
            self.assertEqual(record['id'], repo['id'])

    def test_reject_missing_source_owner_population(self):
        snapshot = self.snapshot()
        snapshot['repositories'] = snapshot['repositories'][:1]
        with self.assertRaisesRegex(ValueError, 'both source owners'):
            capture.validate(snapshot)

    def test_reject_case_insensitive_destination_collision(self):
        snapshot = self.snapshot()
        snapshot['repositories'][1]['destination'] = 'hemsoft-dev/HS-CLI-CONFLUENCE-SEARCH'
        with self.assertRaises(ValueError):
            capture.validate(snapshot)

    def test_reject_duplicate_repository_identity(self):
        snapshot = self.snapshot()
        snapshot['repositories'][1]['id'] = 1
        with self.assertRaises(ValueError):
            capture.validate(snapshot)

    def test_reject_unexpected_source_or_destination(self):
        for field, value in [('full_name', 'another-owner/repo'), ('destination', 'another-owner/repo')]:
            snapshot = self.snapshot()
            snapshot['repositories'][0][field] = value
            with self.assertRaises(ValueError):
                capture.validate(snapshot)

    def test_reject_destination_name_typo_and_wrong_collision_rename(self):
        for index in (0, 1):
            snapshot = self.snapshot()
            snapshot['repositories'][index]['destination'] = 'hemsoft-dev/not-the-source-name'
            with self.assertRaisesRegex(ValueError, 'reviewed mapping'):
                capture.validate(snapshot)

    def test_count_nested_protection_and_ruleset_failures(self):
        snapshot = self.snapshot()
        snapshot['repositories'][0]['settings'] = {
            'protected_branches': {'state': 'observed', 'data': [
                {'name': 'main', 'protection': {'state': 'unverified', 'http_status': 404}}]},
            'rulesets': {'state': 'observed', 'data': [
                {'id': 1, 'details': {'state': 'unverified', 'http_status': 403}}]},
        }
        self.assertEqual(capture.validate(snapshot)['unverified_endpoints'], 3)

    def test_reject_invalid_destination_logins(self):
        for login in ('hemsoft--dev', '-hemsoft', 'hemsoft-', 'x' * 40):
            snapshot = self.snapshot()
            snapshot['destination_login'] = login
            with self.assertRaisesRegex(ValueError, 'Invalid destination login'):
                capture.validate(snapshot)
        for login in ('hemsoft-dev', 'HemSoft', 'x', 'x' * 39):
            self.assertTrue(capture.valid_login(login))

    def test_reject_repository_limited_credentials(self):
        for headers in ('HTTP/2.0 200 OK\n\n{}', 'HTTP/2.0 200 OK\nX-OAuth-Scopes: public_repo\n\n{}'):
            response = subprocess.CompletedProcess([], 0, headers, '')
            with patch.object(capture.subprocess, 'run', return_value=response):
                with self.assertRaisesRegex(RuntimeError, 'repo scope'):
                    capture.verify_source_access()

    def test_require_active_source_org_ownership(self):
        response = subprocess.CompletedProcess([], 0, 'HTTP/2.0 200 OK\nX-OAuth-Scopes: repo, read:org\n\n{}', '')
        for membership in ({'state': 'observed', 'data': {'state': 'active', 'role': 'member'}},
                           {'state': 'unverified', 'http_status': 403}):
            with patch.object(capture.subprocess, 'run', return_value=response), \
                 patch.object(capture, 'api', return_value=membership):
                with self.assertRaisesRegex(RuntimeError, 'ownership'):
                    capture.verify_source_access()
        with patch.object(capture.subprocess, 'run', return_value=response), \
             patch.object(capture, 'api', return_value={'state': 'observed', 'data': {
                 'state': 'active', 'role': 'admin', 'organization': {}}}):
            self.assertTrue(capture.verify_source_access()['fhemmer_owner'])

    def test_reconcile_against_existing_population(self):
        previous = self.snapshot()
        with self.assertRaisesRegex(RuntimeError, 'previously recorded IDs'):
            capture.reconcile_population([{'id': 1}], previous)
        capture.reconcile_population([{'id': 1}, {'id': 2}, {'id': 3}], previous)

    def test_collection_pagination_and_get_only(self):
        response = subprocess.CompletedProcess([], 0, json.dumps([[{'id': 1}], [{'id': 2}]]), '')
        with patch.object(capture.subprocess, 'run', return_value=response) as run:
            result = capture.api('repos/owner/repo/hooks?per_page=100', '')
        self.assertEqual(result['data'], [{'id': 1}, {'id': 2}])
        self.assertIn('--paginate', run.call_args.args[0])
        self.assertIn('--slurp', run.call_args.args[0])
        self.assertEqual(run.call_args.args[0][2:4], ['--method', 'GET'])

    def test_wrapped_collection_pagination(self):
        response = subprocess.CompletedProcess([], 0, json.dumps([
            {'workflows': [{'id': 1}]}, {'workflows': [{'id': 2}]}]), '')
        with patch.object(capture.subprocess, 'run', return_value=response):
            self.assertEqual(capture.api('repos/owner/repo/actions/workflows', 'workflows')['data'],
                             [{'id': 1}, {'id': 2}])

    def test_failed_collection_is_not_empty_or_serialized(self):
        response = subprocess.CompletedProcess([], 1, '{"token":"not-for-output"}', 'gh: Not Found (HTTP 404)')
        with patch.object(capture.subprocess, 'run', return_value=response):
            result = capture.api('repos/owner/repo/installation')
        self.assertEqual(result, {'state': 'unverified', 'http_status': 404})
        self.assertNotIn('not-for-output', json.dumps(result))

    def test_fail_closed_on_incomplete_source_enumeration(self):
        with patch.object(capture, 'api', side_effect=[{'state': 'observed', 'data': []},
                                                     {'state': 'unverified', 'http_status': 403}]):
            with self.assertRaises(RuntimeError):
                capture.source_repositories()

    def test_values_and_hook_credentials_are_not_persisted(self):
        repo = {'id': 1, 'name': 'repo', 'full_name': 'HemSoft/repo'}
        def endpoint(path, collection=None):
            if '/actions/variables' in path:
                return {'state': 'observed', 'data': [{'name': 'VAR', 'value': 'hidden-value'}]}
            if '/hooks?' in path:
                return {'state': 'observed', 'data': [
                    {'id': 1, 'active': True, 'events': ['push'],
                     'config': {'url': 'https://user:password@example.com/token-path?key=hidden'}}]}
            return {'state': 'observed', 'data': []} if collection is not None else {'state': 'unverified'}
        with patch.object(capture, 'api', side_effect=endpoint):
            record = capture.capture_repo(repo, 'hemsoft-dev')
        text = json.dumps(record)
        self.assertEqual(record['settings']['variable_names']['data'], ['VAR'])
        self.assertEqual(record['settings']['webhooks']['data'][0]['delivery_host'], 'example.com')
        for forbidden in ('hidden-value', 'password', 'token-path', 'key=hidden'):
            self.assertNotIn(forbidden, text)


if __name__ == '__main__':
    unittest.main()
