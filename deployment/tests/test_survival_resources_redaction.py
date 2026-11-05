"""The published Windows resource capture keeps its evidence with token values redacted (#298)."""
import hashlib
import importlib.util
import json
import pathlib
import re
import shutil
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
DIRECTORY = ROOT / 'docs' / 'organization-migration'
PATH = DIRECTORY / 'current-survival-resources.json'
SPEC = importlib.util.spec_from_file_location('validate_org_rollout_redaction', ROOT / 'deployment/scripts/validate-org-rollout.py')
VALIDATOR = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VALIDATOR)
TOKEN_FIELDS = re.compile(r'"temp_clone_token":\s*"[^"]+"|[?&]token=[^"&\s]{12,}')
POINTERS = {'/policy_capture/repository_response/data/temp_clone_token',
            '/workflow_capture/contents_response/data/download_url'}


def resolve(document, pointer):
    for part in pointer.strip('/').split('/'):
        document = document[part]
    return document


class SurvivalResourceRedactionTests(unittest.TestCase):
    def setUp(self):
        self.raw = PATH.read_bytes()
        self.capture = json.loads(self.raw)

    def test_committed_capture_matches_the_redacted_pin(self):
        self.assertEqual(hashlib.sha256(self.raw).hexdigest(), VALIDATOR.SURVIVAL_RESOURCES_SHA256)
        self.assertEqual((json.dumps(self.capture, indent=2) + '\n').encode(), self.raw)

    def test_validator_accepts_the_committed_redacted_capture(self):
        inventory = json.loads((DIRECTORY / 'inventory.json').read_text())
        repo = next(row for row in inventory['repositories'] if row['id'] == VALIDATOR.SURVIVAL_REPOSITORY_ID)
        result = VALIDATOR.reviewed_survival_resources(repo, DIRECTORY)
        self.assertEqual(result['runner']['id'], 10)
        self.assertTrue(result['workflow_bytes'])

    def test_no_token_values_and_recorded_placeholders(self):
        self.assertEqual(len(TOKEN_FIELDS.findall(self.raw.decode())), 0)
        record = self.capture['redactions']
        self.assertEqual(record['issue'], 'https://github.com/hemsoft-dev/set-it-free-loop/issues/298')
        self.assertNotEqual(record['unredacted_sha256'], VALIDATOR.SURVIVAL_RESOURCES_SHA256)
        self.assertRegex(record['unredacted_commit'], r'^[0-9a-f]{40}$')
        self.assertEqual({field['pointer'] for field in record['fields']}, POINTERS)
        for field in record['fields']:
            value = resolve(self.capture, field['pointer'])
            self.assertEqual(value, field['replacement'])
            self.assertTrue(field['reason'] and field['original_length'] > 0)
        self.assertEqual(resolve(self.capture, '/policy_capture/repository_response/data/temp_clone_token'), '')
        url = resolve(self.capture, '/workflow_capture/contents_response/data/download_url')
        self.assertTrue(url.endswith('/.github/workflows/ci.yml?token='))

    def test_counter_and_pin_reject_a_reinserted_token(self):
        synthetic = json.loads(self.raw)
        synthetic['policy_capture']['repository_response']['data']['temp_clone_token'] = 'X' * 29
        data = synthetic['workflow_capture']['contents_response']['data']
        data['download_url'] += 'Y' * 31
        body = json.dumps(synthetic, indent=2) + '\n'
        self.assertEqual(len(TOKEN_FIELDS.findall(body)), 2)
        with tempfile.TemporaryDirectory() as temporary:
            directory = pathlib.Path(temporary)
            for name in ('inventory.json', 'evidence-integrity.json', 'fhemmer-protection-evidence.json'):
                shutil.copyfile(DIRECTORY / name, directory / name)
            (directory / PATH.name).write_text(body)
            inventory = json.loads((DIRECTORY / 'inventory.json').read_text())
            repo = next(row for row in inventory['repositories'] if row['id'] == VALIDATOR.SURVIVAL_REPOSITORY_ID)
            with self.assertRaisesRegex(ValueError, 'match their reviewed preparation capture'):
                VALIDATOR.reviewed_survival_resources(repo, directory)


if __name__ == '__main__':
    unittest.main()
