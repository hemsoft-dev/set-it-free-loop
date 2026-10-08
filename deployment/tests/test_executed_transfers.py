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
                     lambda d: d['after'].update(observed_at=d['before']['observed_at']), 'chronology')

    def test_unapproved_retention_rejected(self):
        self.rejects('repository-first-final-inventory.json',
                     lambda d: d['retained'].pop(), 'Retained repositories')

    def test_rollout_completion_claim_rejected(self):
        self.rejects('repository-first-final-inventory.json',
                     lambda d: d.update(reviewer_rollout_complete=True), 'cannot claim completed SFL')


if __name__ == '__main__':
    unittest.main()
