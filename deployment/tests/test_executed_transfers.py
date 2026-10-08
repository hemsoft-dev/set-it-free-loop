"""Corruption checks for the committed transfer evidence, separate from rollout readiness."""
import copy
import importlib.util
import pathlib
import unittest
from unittest import mock

ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    'executed_transfers', ROOT / 'deployment/scripts/validate-executed-transfers.py')
VALIDATOR = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VALIDATOR)
DIRECTORY = ROOT / 'docs/organization-migration'


class ExecutedTransferTests(unittest.TestCase):
    def test_actual_receipts_verify_identity_without_acceptance_claim(self):
        result = VALIDATOR.validate(DIRECTORY)
        self.assertEqual(result['transferred'], 65)
        self.assertEqual(result['source_protection_evidence'], 'incomplete')
        self.assertFalse(result['migration_acceptance_complete'])

    def rejects(self, filename, mutate, message):
        original = VALIDATOR.read

        def changed(directory, name):
            value = copy.deepcopy(original(directory, name))
            if name == filename:
                mutate(value)
            return value

        with mock.patch.object(VALIDATOR, 'read', side_effect=changed):
            with self.assertRaisesRegex(ValueError, message):
                VALIDATOR.validate(DIRECTORY)

    def test_wrong_destination_identity_rejected(self):
        self.rejects('repository-transfers/repository-transfer-1143951439.json',
                     lambda d: d['after']['data'].update(id=1), 'Captured repository identity')

    def test_changed_ref_rejected(self):
        self.rejects('repository-transfers/repository-transfer-1143951439.json',
                     lambda d: d['after_refs']['refs'].clear(), 'Ref summary must derive')

    def test_changed_primary_ref_rejected(self):
        def change(d):
            d['after_refs']['responses'][0]['data'][0]['object']['sha'] = '1' * 40
            d['after_refs']['refs']['refs/heads/main']['sha'] = '1' * 40
        self.rejects('repository-transfers/repository-transfer-1143951439.json',
                     change, 'Transfer changed refs')

    def test_other_conflict_does_not_prove_empty_repository(self):
        self.rejects('repository-transfers/repository-transfer-996911586.json',
                     lambda d: d['before_refs']['responses'][0]['data'].update(message='Conflict'),
                     'Only the captured empty-repository')

    def test_duplicate_receipt_rejected(self):
        self.rejects('repository-first-final-inventory.json',
                     lambda d: d['receipts'].__setitem__(1, d['receipts'][0]), 'exactly the 65')

    def test_pre_transfer_destination_capture_rejected(self):
        self.rejects('repository-transfers/repository-transfer-1143951439.json',
                     lambda d: d['after'].update(observed_at=d['before']['observed_at']), 'accepted API response')

    def test_unapproved_retention_rejected(self):
        self.rejects('repository-first-final-inventory.json',
                     lambda d: d['retained'].pop(), 'Retained inventory')

    def test_duplicate_retained_row_rejected(self):
        self.rejects('repository-first-final-inventory.json',
                     lambda d: d['retained'].append(d['retained'][0]), 'two unique rows')

    def test_retained_repository_requires_post_transfer_identity(self):
        self.rejects('retained-repository-1162179521.json',
                     lambda d: d.update(observed_at='2026-10-07T00:00:00Z'), 'follow the transfer phase')
        self.rejects('retained-repository-1162179521.json',
                     lambda d: d['data'].update(full_name='hemsoft-dev/now-leadership-group'),
                     'Captured repository identity')

    def test_old_source_metadata_rejected(self):
        self.rejects('repository-transfers/repository-transfer-1143951439.json',
                     lambda d: d['before'].update(observed_at='2000-01-01T00:00:00Z'),
                     'Transfer capture chronology')

    def test_post_hoc_owner_direction_rejected(self):
        self.rejects('owner-repository-first-direction.json',
                     lambda d: d.update(recorded_at='2099-01-01T00:00:00Z'), 'Owner direction must predate')

    def test_pilot_qualification_graph_rejects_corruption(self):
        cases = [
            ('rc21-pilots-qualified-before-wider.json', lambda d: d.update(qualified=False), 'summary'),
            ('rc21-private-fixture-findings.json', lambda d: d.update(outcome='gate_passed'), 'fixture'),
            ('rc21-private-fixture-base_advance.json', lambda d: d.update(workflow_sha256='0' * 64), 'fixture'),
            ('rc21-private-live-guarded-merge.json', lambda d: d['gate'].update(conclusion='failure'), 'gate'),
            ('rc21-private-live-guarded-merge.json', lambda d: d['request'].update(id=1), 'request binding'),
            ('rc21-private-post-live-repeat-sync.json', lambda d: d.update(revision_after='1' * 40), 'terminal repeat'),
            ('rc21-private-completed-live-pilot-validation.json', lambda d: d.update(qualified=False), 'completion summary'),
        ]
        for filename, mutation, message in cases:
            with self.subTest(filename=filename):
                self.rejects(filename, mutation, message)

    def test_pilot_live_run_must_be_successful(self):
        import json
        def corrupt(d):
            run = json.loads(d['stdout'])
            run['conclusion'] = 'failure'
            d['stdout'] = json.dumps(run)
        self.rejects('rc21-public-live-runtime-run.json', corrupt, 'did not complete successfully')

    def test_final_installed_equivalence_requires_matching_bytes_and_interval(self):
        for mutation in (lambda d: d.update(content_equal=False),
                         lambda d: d.update(actual_git_blob_sha='0' * 40),
                         lambda d: d.update(actual_capture_started_at='2000-01-01T00:00:00Z')):
            self.rejects('rc21-private-final-installed-equivalence.json', mutation, 'final installed equivalence')

    def test_cleanup_capture_and_deletion_order_rejected(self):
        self.rejects('rc21-private-post-live-after-cleanup-effective.json',
                     lambda d: d.update(started_at='2000-01-01T00:00:00Z'), 'predates gate removal')
        self.rejects('rc21-private-post-live-gate-only-cleanup.json',
                     lambda d: d.update(started_at='2000-01-01T00:00:00Z'), 'removal did not follow')

    def test_pending_log_command_and_run_identity_rejected(self):
        for mutation in (lambda d: d.update(actor='someone-else'),
                         lambda d: d.update(argv=['gh', 'api', 'rate_limit']),
                         lambda d: d.update(started_at='2000-01-01T00:00:00Z')):
            self.rejects('rc21-private-live-request-failure-log.json', mutation, 'pending-failure primary')
        self.rejects('rc21-private-pending-request-failed-run.json',
                     lambda d: d['data']['repository'].update(id=1), 'pending-failure run')

    def test_required_gate_name_and_completion_time_rejected(self):
        for mutation in (lambda d: d['gate'].update(name='unrelated check'),
                         lambda d: d['gate'].update(completed_at='2099-01-01T00:00:00Z'),
                         lambda d: d['gate'].update(external_id=d['gate']['external_id'].replace('pull:12:', 'pull:999:'))):
            self.rejects('rc21-private-live-guarded-merge.json', mutation,
                         'gate is invalid|request binding is invalid')

    def test_arbitrary_installed_text_cannot_qualify_by_rehashing(self):
        import hashlib
        original = VALIDATOR.read
        fake = 'Arbitrary text @89425320ace3127a829d86e3b642fe2b31fd979e'
        digest = hashlib.sha256(fake.encode()).hexdigest()
        blob = hashlib.sha1(b'blob ' + str(len(fake.encode())).encode() + b'\0' + fake.encode()).hexdigest()
        def changed(directory, name):
            value = copy.deepcopy(original(directory, name))
            if name == 'rc21-private-installed-observer-projected.json':
                value.update(content=fake, blob_sha=blob)
            elif name.startswith('rc21-private-fixture-'):
                value['workflow_sha256'] = digest
            return value
        with mock.patch.object(VALIDATOR, 'read', side_effect=changed):
            with self.assertRaisesRegex(ValueError, 'reviewed source markers'):
                VALIDATOR.validate(DIRECTORY)

    def test_accepted_response_is_bound_to_actual_transfer(self):
        for field, value in [('method', 'DELETE'), ('request_url', 'https://api.github.com/rate_limit'),
                             ('observed_at', '2026-10-06T00:00:00Z')]:
            with self.subTest(field=field):
                self.rejects('repository-transfers/repository-transfer-1143951439.json',
                             lambda d, key=field, v=value: d['transfer_response'].update({key: v}),
                             'accepted API response')
        self.rejects('repository-transfers/repository-transfer-1143951439.json',
                     lambda d: d['transfer_response']['data'].update(id=1), 'accepted API response')

    def test_pending_failure_log_references_exist(self):
        for role in ('private', 'public'):
            binding = VALIDATOR.read(DIRECTORY / 'execution', f'rc21-{role}-live-execution-binding.json')
            # Actual pending failures remain distinct from terminal successful native review runs.
            log = VALIDATOR.read(DIRECTORY / 'execution', binding['pending_failure_primary'])
            self.assertTrue(binding['pending_failure_expected'])
            self.assertIn(f'Registered Codex review request {binding["request_id"]} is pending', log['stdout'])

    def test_rollout_completion_claim_rejected(self):
        self.rejects('repository-first-final-inventory.json',
                     lambda d: d.update(reviewer_rollout_complete=True), 'cannot claim completed SFL')


if __name__ == '__main__':
    unittest.main()
