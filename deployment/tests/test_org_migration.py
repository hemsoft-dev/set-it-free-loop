"""Offline safety checks for the read-only migration inventory."""

import importlib.util
import json
import pathlib
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = pathlib.Path(__file__).parents[1] / 'scripts' / 'capture-org-migration.py'
spec = importlib.util.spec_from_file_location('capture_org_migration', SCRIPT)
capture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(capture)


class MigrationTests(unittest.TestCase):
    def snapshot(self):
        snapshot = {
            'source_owners': ['HemSoft', 'fhemmer'], 'destination_login': 'hemsoft-dev',
            'expected_repository_ids': [1, 2],
            'expected_repository_sources': {'1': 'HemSoft/hs-cli-confluence-search', '2': 'fhemmer/hs-cli-confluence-search'},
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

        snapshot.update(schema_version=1, captured_at='2026-10-06T21:41:09Z', requester='HemSoft',
            source_personal_plan='pro', coverage_limits=[],
            source_access={'credential': 'classic/OAuth', 'repo_scope': True, 'fhemmer_owner': True})
        for field in ('destination_organization', 'source_organization', 'known_owned_app', 'destination_actions_policy', 'source_organization_apps'):
            snapshot[field] = {'state': 'unverified'}
        snapshot['destination_organization'] = {'state': 'observed', 'data': {'id': 42, 'login': 'hemsoft-dev',
            'type': 'Organization', 'plan': {'name': 'team'}, 'default_repository_permission': 'read'}}
        snapshot['destination_teams'] = {'state': 'observed', 'data': []}
        for field in ('destination_owner_membership', 'source_organization_membership'):
            snapshot[field] = {'state': 'observed', 'data': {'state': 'active', 'role': 'admin'}}
        for repo in snapshot['repositories']:
            for field, data_type in capture.SETTING_TYPES.items():
                repo['settings'].setdefault(field, {'state': 'observed', 'data': data_type()})
        return snapshot

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
        with self.assertRaises(ValueError):
            capture.validate(snapshot)

    def test_reject_repository_deletion_even_when_both_owners_remain(self):
        snapshot = self.snapshot()
        snapshot['expected_repository_ids'].append(3)
        snapshot['expected_repository_sources']['3'] = 'HemSoft/another'
        snapshot['repositories'].append({**snapshot['repositories'][0], 'id': 3,
            'full_name': 'HemSoft/another', 'destination': 'hemsoft-dev/another'})
        snapshot['summary'] = capture.validate(snapshot)
        snapshot['repositories'].pop()
        with self.assertRaisesRegex(ValueError, 'expected IDs'):
            capture.validate(snapshot)

    def test_reject_stale_summary(self):
        snapshot = self.snapshot()
        snapshot['summary'] = {**capture.validate(snapshot), 'repositories': 67}
        with self.assertRaisesRegex(ValueError, 'Stored summary'):
            capture.validate(snapshot)

    def test_require_summary_for_offline_check_but_allow_live_computation(self):
        snapshot = self.snapshot()
        self.assertEqual(capture.validate(snapshot)['repositories'], 2)
        with self.assertRaisesRegex(ValueError, 'requires a stored summary'):
            capture.validate(snapshot, require_summary=True)

    def bundle(self):
        snapshot = self.snapshot()
        for repo in snapshot['repositories']:
            repo['settings']['environments'] = {'state': 'observed', 'data': []}
        snapshot['summary'] = capture.validate(snapshot)
        owner = {'verifier_account': 'HemSoft', 'destination': {'login': 'hemsoft-dev', 'id': 42,
            'plan': 'team', 'members': ['HemSoft'], 'teams': [],
            'actions': {'state': 'observed_in_owner_browser'}},
            'source_personal_apps': {'state': 'observed_in_owner_browser', 'expected_installation_ids': [10],
                'expected_repository_selections': {'10': {'selection': 'selected',
                    'repositories': [snapshot['repositories'][0]['full_name']]}},
                'installations': [{'installation_id': 10, 'selection': 'selected',
                                  'repositories': [snapshot['repositories'][0]['full_name']]}]},
            'effective_branch_rules': [], 'expected_effective_branch_rule_hashes': {},
            'source_fhemmer_rulesets': {'state': 'observed_in_owner_browser', 'configured_rulesets': []}}
        runtime = {'expected_repository_runner_ids': {}, 'repositories': [
            {'source': repo['full_name'], 'repository_runners': {'state': 'observed', 'data': []},
             'environments': []} for repo in snapshot['repositories']]}
        return snapshot, owner, runtime

    def test_reconcile_supplemental_repository_and_runner_coverage(self):
        snapshot, owner, runtime = self.bundle()
        capture.validate_supplemental(snapshot, owner, runtime)
        runtime['repositories'].pop()
        with self.assertRaisesRegex(ValueError, 'repository coverage'):
            capture.validate_supplemental(snapshot, owner, runtime)
        snapshot, owner, runtime = self.bundle()
        runtime['expected_repository_runner_ids'] = {runtime['repositories'][0]['source']: [12]}
        with self.assertRaisesRegex(ValueError, 'runner identity'):
            capture.validate_supplemental(snapshot, owner, runtime)

    def test_reconcile_supplemental_environment_coverage_and_names_only(self):
        snapshot, owner, runtime = self.bundle()
        snapshot['repositories'][0]['settings']['environments']['data'] = [{'name': 'Production'}]
        with self.assertRaisesRegex(ValueError, 'environment coverage'):
            capture.validate_supplemental(snapshot, owner, runtime)
        env = {'name': 'Production', 'secret_names': {'state': 'observed', 'data': []},
               'variable_names': {'state': 'observed', 'data': []}}
        runtime['repositories'][0]['environments'] = [env]
        capture.validate_supplemental(snapshot, owner, runtime)
        env['variable_names']['data'] = [{'name': 'KEY', 'value': 'sensitive'}]
        with self.assertRaisesRegex(ValueError, 'names only'):
            capture.validate_supplemental(snapshot, owner, runtime)

    def test_reconcile_supplemental_app_identity_and_selection(self):
        snapshot, owner, runtime = self.bundle()
        owner['source_personal_apps']['installations'] = []
        with self.assertRaisesRegex(ValueError, 'App identity'):
            capture.validate_supplemental(snapshot, owner, runtime)
        snapshot, owner, runtime = self.bundle()
        owner['source_personal_apps']['installations'][0]['repositories'] = ['outside/repo']
        with self.assertRaisesRegex(ValueError, 'App repository selection'):
            capture.validate_supplemental(snapshot, owner, runtime)

    def test_reconcile_supplemental_protection_and_owner_identity(self):
        snapshot, owner, runtime = self.bundle()
        snapshot['repositories'][0]['settings']['protected_branches'] = {'state': 'observed', 'data': [
            {'name': 'main', 'protection': {'state': 'unverified', 'http_status': 404}}]}
        with self.assertRaisesRegex(ValueError, 'coverage mismatch'):
            capture.validate_supplemental(snapshot, owner, runtime)
        owner['effective_branch_rules'] = [{'source': snapshot['repositories'][0]['full_name'],
                                            'branch': 'main', 'state': 'observed', 'rules': []}]
        owner['expected_effective_branch_rule_hashes'] = {snapshot['repositories'][0]['full_name'] + '/main':
            capture.digest(owner['effective_branch_rules'][0])}
        capture.validate_supplemental(snapshot, owner, runtime)
        owner['verifier_account'] = 'another-user'
        with self.assertRaisesRegex(ValueError, 'identity mismatch'):
            capture.validate_supplemental(snapshot, owner, runtime)

    def test_offline_bundle_parses_both_supplemental_files(self):
        snapshot, owner, runtime = self.bundle()
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / 'inventory.json').write_text(json.dumps(snapshot))
            (root / 'evidence-integrity.json').write_text(json.dumps({name: capture.digest(value)
                for name, value in [('inventory.json', snapshot), ('owner-verification.json', owner),
                                    ('runtime-metadata.json', runtime)]}))
            for filename in ('owner-verification.json', 'runtime-metadata.json'):
                (root / 'owner-verification.json').write_text(json.dumps(owner))
                (root / 'runtime-metadata.json').write_text(json.dumps(runtime))
                self.assertEqual(capture.validate_bundle(root / 'inventory.json')['repositories'], 2)
                (root / filename).write_text('{malformed')
                with self.assertRaises(json.JSONDecodeError):
                    capture.validate_bundle(root / 'inventory.json')
            (root / 'owner-verification.json').write_text(json.dumps(owner))
            (root / 'runtime-metadata.json').write_text(json.dumps(runtime))
            snapshot.pop('summary')
            (root / 'inventory.json').write_text(json.dumps(snapshot))
            with self.assertRaisesRegex(ValueError, 'requires a stored summary'):
                capture.validate_bundle(root / 'inventory.json')

    def test_reject_missing_or_malformed_repository_settings(self):
        for field, data_type in capture.SETTING_TYPES.items():
            for malformed in (None, {'state': 'observed'}, {'state': 'invalid'},
                              {'state': 'observed', 'data': 'wrong-type'}):
                snapshot = self.snapshot()
                snapshot['repositories'][1]['settings'][field] = malformed
                with self.assertRaises(ValueError):
                    capture.validate(snapshot)
            snapshot = self.snapshot()
            del snapshot['repositories'][1]['settings'][field]
            with self.assertRaisesRegex(ValueError, 'Missing captured repository setting'):
                capture.validate(snapshot)
        snapshot = self.snapshot()
        snapshot['repositories'][1]['settings']['collaborators']['data'] = [{'login': 'fhemmerrelias'}]
        with self.assertRaisesRegex(ValueError, 'Incomplete captured setting item'):
            capture.validate(snapshot)

    def test_reject_truncated_or_changed_selected_app_access(self):
        snapshot, owner, runtime = self.bundle()
        app = owner['source_personal_apps']['installations'][0]
        app['repositories'].pop()
        with self.assertRaisesRegex(ValueError, 'App repository selection'):
            capture.validate_supplemental(snapshot, owner, runtime)
        snapshot, owner, runtime = self.bundle()
        owner['source_personal_apps']['installations'][0].update(selection='all', repositories=[])
        with self.assertRaisesRegex(ValueError, 'App repository selection'):
            capture.validate_supplemental(snapshot, owner, runtime)
        snapshot, owner, runtime = self.bundle()
        owner['source_personal_apps']['expected_repository_selections'] = {}
        with self.assertRaisesRegex(ValueError, 'App selection manifest'):
            capture.validate_supplemental(snapshot, owner, runtime)

    def test_require_top_level_capture_evidence(self):
        fields = ('source_organization_apps', 'known_owned_app', 'destination_owner_membership',
                  'destination_organization', 'destination_teams', 'destination_actions_policy',
                  'source_organization', 'source_organization_membership')
        for field in fields:
            snapshot = self.snapshot()
            del snapshot[field]
            with self.assertRaisesRegex(ValueError, 'Missing top-level'):
                capture.validate(snapshot)
            snapshot = self.snapshot()
            snapshot[field] = {'state': 'observed', 'data': 'invalid'}
            with self.assertRaisesRegex(ValueError, 'setting payload'):
                capture.validate(snapshot)

    def test_reject_swapped_repository_id_source_pairs(self):
        snapshot = self.snapshot()
        left, right = snapshot['repositories']
        left['id'], right['id'] = right['id'], left['id']
        with self.assertRaisesRegex(ValueError, 'ID/source mapping'):
            capture.validate(snapshot)

    def test_reject_payload_on_unverified_environment_credentials(self):
        for field in ('secret_names', 'variable_names'):
            snapshot, owner, runtime = self.bundle()
            snapshot['repositories'][0]['settings']['environments']['data'] = [{'name': 'Production'}]
            env = {'name': 'Production', 'secret_names': {'state': 'observed', 'data': []},
                   'variable_names': {'state': 'observed', 'data': []}}
            runtime['repositories'][0]['environments'] = [env]
            env[field] = {'state': 'unverified', 'data': [{'name': 'TOKEN', 'value': 'supersecret'}]}
            with self.assertRaisesRegex(ValueError, 'unverified setting'):
                capture.validate_supplemental(snapshot, owner, runtime)

    def test_reject_truncated_branch_rule_payload(self):
        snapshot, owner, runtime = self.bundle()
        source = snapshot['repositories'][0]['full_name']
        snapshot['repositories'][0]['settings']['protected_branches']['data'] = [
            {'name': 'main', 'protection': {'state': 'unverified'}}]
        rule = {'source': source, 'branch': 'main', 'state': 'observed',
                'rules': [{'type': 'required_status_checks'}, {'type': 'pull_request'}]}
        owner['effective_branch_rules'] = [rule]
        owner['expected_effective_branch_rule_hashes'] = {source + '/main': capture.digest(rule)}
        capture.validate_supplemental(snapshot, owner, runtime)
        rule['rules'].pop()
        with self.assertRaisesRegex(ValueError, 'Branch rule payload'):
            capture.validate_supplemental(snapshot, owner, runtime)

    def test_integrity_manifest_rejects_any_evidence_payload_change(self):
        snapshot, owner, runtime = self.bundle()
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            files = {'inventory.json': snapshot, 'owner-verification.json': owner, 'runtime-metadata.json': runtime}
            for name, value in files.items():
                (root / name).write_text(json.dumps(value))
            (root / 'evidence-integrity.json').write_text(json.dumps({name: capture.digest(value) for name, value in files.items()}))
            capture.validate_bundle(root / 'inventory.json')
            for name, value in files.items():
                changed = {**value, 'unexpected_evidence': 'changed'}
                (root / name).write_text(json.dumps(changed))
                with self.assertRaisesRegex(ValueError, 'integrity manifest'):
                    capture.validate_bundle(root / 'inventory.json')
                (root / name).write_text(json.dumps(value))

    def test_capture_custom_environment_patterns_with_encoded_name(self):
        repo = {'id': 1, 'name': 'repo', 'full_name': 'HemSoft/repo'}
        paths = []
        def endpoint(path, collection=None):
            paths.append((path, collection))
            if '/environments?' in path:
                return {'state': 'observed', 'data': [{'id': 1, 'name': 'release /pages',
                    'deployment_branch_policy': {'custom_branch_policies': True}}]}
            if '/deployment-branch-policies?' in path:
                return {'state': 'observed', 'data': [{'id': 2, 'name': 'release/*', 'type': 'branch'},
                                                     {'id': 3, 'name': 'v*', 'type': 'tag'}]}
            return {'state': 'observed', 'data': []} if collection is not None else {'state': 'unverified'}
        with patch.object(capture, 'api', side_effect=endpoint):
            record = capture.capture_repo(repo, 'hemsoft-dev')
        patterns = record['settings']['environments']['data'][0]['deployment_branch_patterns']['data']
        self.assertEqual([(item['name'], item['type']) for item in patterns], [('release/*', 'branch'), ('v*', 'tag')])
        self.assertIn(('repos/HemSoft/repo/environments/release%20%2Fpages/deployment-branch-policies?per_page=100',
                       'branch_policies'), paths)

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
        snapshot['repositories'][0]['settings'].update({
            'protected_branches': {'state': 'observed', 'data': [
                {'name': 'main', 'protection': {'state': 'unverified', 'http_status': 404}}]},
            'rulesets': {'state': 'observed', 'data': [
                {'id': 1, 'details': {'state': 'unverified', 'http_status': 403}}]},
        })
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
