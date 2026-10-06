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
