"""Regression tests for offline ledger and rollout completion gates."""

import copy
import csv
import importlib.util
import json
import pathlib
import unittest

ROOT = pathlib.Path(__file__).parents[2]
DIRECTORY = ROOT / 'docs' / 'organization-migration'
spec = importlib.util.spec_from_file_location('validate_org_rollout', ROOT / 'deployment/scripts/validate-org-rollout.py')
validator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validator)


class RolloutTests(unittest.TestCase):
    def setUp(self):
        self.inventory = json.loads((DIRECTORY / 'inventory.json').read_text())
        self.matrix = json.loads((DIRECTORY / 'rollout-matrix.json').read_text())
        self.scope = json.loads((DIRECTORY / 'scope-decisions.json').read_text())
        with (DIRECTORY / 'integration-ledger.csv').open(newline='') as stream:
            self.rows = list(csv.DictReader(stream))

    def check(self):
        return validator.validate(self.inventory, self.rows, self.matrix, DIRECTORY, self.scope)

    def complete_provider(self):
        row = self.rows[0]
        row.update(status='verified', provider='example', resource_owner='HemSoft',
                   resource_url='https://example.com/resource', billing_dependency='No purchase required',
                   credential_source='Provider-owned integration', credential_validity='verified',
                   affected_reference='Git repository link', transfer_action='Reconnect exact repository ID',
                   smoke_test='Expected response verified', recovery_action='Restore previous repository link',
                   verified_by='HemSoft', verified_at='2026-10-06T20:00:00-04:00',
                   evidence_url='https://github.com/HemSoft/set-it-free-loop/issues/138')
        return row

    def verify_source_ledger(self, repo_id):
        for row in self.rows:
            if int(row['repository_id']) == repo_id:
                row.update(status='verified', provider='none' if row.get('resource_kind') != 'repository_runner' else 'GitHub Actions', absence_reason='Synthetic owner absence receipt',
                           verified_by='HemSoft', verified_at='2026-10-06T20:00:00-04:00',
                           evidence_url='https://github.com/HemSoft/set-it-free-loop/issues/138')
                if row.get('resource_kind') == 'repository_runner':
                    row.update(resource_owner='HemSoft', resource_url='https://example.com/runner',
                               billing_dependency='Existing VM',credential_source='Existing registration',
                               credential_validity='verified',affected_reference='Runner 21',
                               transfer_action='Verify registration continuity',smoke_test='Manual smoke succeeds',
                               recovery_action='Restore exact registration if needed')

    def complete_transfer_gates(self):
        for repo in self.inventory['repositories']:
            self.verify_source_ledger(repo['id'])

    def complete_app_transfer(self):
        self.matrix['owned_app_transfer'].update(status='verified',owner='hemsoft-dev',evidence_url='https://example.com/app-transfer')

    def complete_rollout(self):
        self.complete_pilots()
        self.complete_transfer_gates()
        self.complete_app_transfer()
        row = next(row for row in self.matrix['repositories'] if not row['archived'])
        row.update(health='verified', selected_tier='reviewer', installed_tier='not_installed', installed_addons=[], selected_addons=[],
                   manifest_version='2.1.0-rc.14', review_requester='HemSoft',
                   deployment_source='hemsoft-dev/set-it-free-loop', deployment_sha='a'*40,
                   review_head_sha='b'*40, review_base_sha='c'*40, destination_codex_access='verified',
                   destination_sfl_app_access='verified', transfer_evidence_url='https://example.com/transfer',
                   review_pr_url='https://example.com/review', gate_run_url='https://example.com/gate',
                   status_evidence_url='https://example.com/status')
        row['gate_policy'] = {'state': 'required', 'context': 'SFL Reviewer Gate Runner', 'app_id': 15368,
                              'strict': True, 'evidence_url': 'https://example.com/rule'}
        row['review_registration_url'] = 'https://example.com/registration'
        row['review_registry_status_url'] = 'https://example.com/registry-status'
        row['review_artifact_url'] = 'https://example.com/sfl-review-artifact'
        row['review_artifact_identity'] = {'runtime': 'sfl_registered_codex', 'app_id': 1144995, 'bot_user_id': 199175422,
                                           'reviewed_head_sha': 'b'*40, 'reviewed_base_sha': 'c'*40}
        row.update(requester_permission='admin',requester_permission_evidence_url='https://example.com/permission')
        row['review_pr_url']='https://github.com/'+row['destination']+'/pull/1'
        row['review_artifact_identity'].update(requester=row['review_requester'],review_pr_url=row['review_pr_url'])
        self.verify_source_ledger(row['repository_id'])
        row.update(manifest_identity={'source':row['deployment_source'],'sourceSha':row['deployment_sha'],
                                      'version':row['manifest_version'],'tier':row['selected_tier']},
                   manifest_evidence_url='https://example.com/manifest',
                   release_url='https://github.com/hemsoft-dev/set-it-free-loop/releases/tag/v'+row['manifest_version'],
                   release_download_verification_url='https://example.com/checksum')
        self.matrix['summary']['verified_rollouts'] += 1
        return row

    def complete_pilots(self):
        for pilot in self.matrix['disposable_validation_repositories']:
            pilot['validation_status'] = 'verified'
            pilot['validation_evidence'] = {field: 'https://example.com/' + field for field in
                ('init_pr_url', 'sync_pr_url', 'repeat_sync_evidence_url',
                 'repeat_onboarding_evidence_url', 'review_registration_url',
                 'review_registry_status_url', 'review_artifact_url', 'gate_run_url',
                 'status_evidence_url', 'gate_uninstall_evidence_url')}
            pilot['validation_evidence'].update(deployment_source='hemsoft-dev/set-it-free-loop',
                release_version='2.1.0-rc.14', deployment_sha='a'*40,
                manifest_identity={'source':'hemsoft-dev/set-it-free-loop','sourceSha':'a'*40,
                                   'version':'2.1.0-rc.14','tier':'reviewer'},
                manifest_evidence_url='https://example.com/manifest',
                release_download_verification_url='https://example.com/checksum')
            receipts=pilot['validation_evidence']
            receipts.update(review_requester='HemSoft',review_pr_url='https://github.com/'+pilot['repository']+'/pull/1',
                review_head_sha='b'*40,review_base_sha='c'*40,requester_permission='admin',
                requester_permission_evidence_url='https://example.com/permission',
                gate_policy={'state':'required','context':'SFL Reviewer Gate Runner','app_id':15368,
                             'strict':True,'evidence_url':'https://example.com/rule'},wider_workflow_run_urls=[])
            if pilot['visibility']=='private':
                receipts['manifest_identity']['tier']='full'
                receipts['wider_workflow_run_urls']=['https://example.com/workflow']
                receipts['auditor_run_url']='https://example.com/auditor'
            pilot['validation_evidence']['review_artifact_identity'] = {
                'runtime': 'sfl_registered_codex', 'app_id': 1144995, 'bot_user_id': 199175422,
                'reviewed_head_sha': 'b'*40, 'reviewed_base_sha': 'c'*40,
                'review_pr_url':receipts['review_pr_url'],'requester':receipts['review_requester']}

    def complete_source(self):
        self.complete_pilots()
        self.complete_transfer_gates()
        self.complete_app_transfer()
        row = next(row for row in self.matrix['repositories'] if row['source'] == 'HemSoft/set-it-free-loop')
        row.update(health='source_verified', transfer_evidence_url='https://example.com/transfer',
                   status_evidence_url='https://example.com/status', destination_codex_access='verified',
                   destination_sfl_app_access='verified', review_requester='HemSoft',
                   review_head_sha='b'*40, review_base_sha='c'*40,
                   gate_policy={'state':'required','context':'SFL Reviewer Gate Runner','app_id':15368,
                                'strict':True,'evidence_url':'https://example.com/rule'})
        for field in ('review_pr_url','gate_run_url','review_registration_url',
                      'review_registry_status_url','review_artifact_url'):
            row[field] = 'https://example.com/' + field
        row['review_artifact_identity']={'runtime':'sfl_registered_codex','app_id':1144995,'bot_user_id':199175422,
                                        'reviewed_head_sha':'b'*40,'reviewed_base_sha':'c'*40}
        row.update(requester_permission='admin',requester_permission_evidence_url='https://example.com/permission')
        row['review_pr_url']='https://github.com/'+row['destination']+'/pull/1'
        row['review_artifact_identity'].update(requester=row['review_requester'],review_pr_url=row['review_pr_url'])
        row['in_place_evidence']={'workflow_run_urls':['https://example.com/run'],
            'release_version':'2.1.0-rc.14','source_sha':'a'*40,
            'release_url':'https://github.com/hemsoft-dev/set-it-free-loop/releases/tag/v2.1.0-rc.14',
            'release_verification_url':'https://example.com/checksum',
            'governance_evidence_url':'https://example.com/governance'}
        self.verify_source_ledger(row['repository_id'])
        self.matrix['summary']['verified_rollouts'] += 1
        return row

    def test_protected_source_can_complete_without_consumer_manifest(self):
        row = self.complete_source()
        self.assertIsNone(row['installed_tier'])
        self.assertIsNone(row['manifest_version'])
        self.assertEqual(self.check()['verified_rollouts'], 1)

    def test_protected_source_requires_in_place_proof_and_owned_review(self):
        row = self.complete_source()
        original = copy.deepcopy(row)
        for field in row['in_place_evidence']:
            row.clear(); row.update(copy.deepcopy(original))
            del row['in_place_evidence'][field]
            with self.subTest(field=field), self.assertRaises(ValueError): self.check()
        for field in ('gate_policy','review_artifact_identity','review_registration_url'):
            row.clear(); row.update(copy.deepcopy(original)); row[field]=None
            with self.subTest(field=field), self.assertRaises(ValueError): self.check()
        row.clear(); row.update(original); row['health']='scope_exception'
        with self.assertRaisesRegex(ValueError, 'cannot omit in-place'): self.check()

    def test_protected_mode_cannot_be_applied_to_another_repository(self):
        row=self.complete_rollout()
        row['rollout_action']='protected_source_verify_workflows_in_place'
        with self.assertRaisesRegex(ValueError, 'completion mode'): self.check()

    def test_completed_rollout_requires_retained_app_dependencies_verified(self):
        self.complete_rollout()
        row=next(row for row in self.matrix['repositories'] if row['health']=='retained_source')
        row['retained_app_dependency']['status']='pending'
        with self.assertRaisesRegex(ValueError, 'verified retained App dependencies'): self.check()

    def test_pilot_requires_immutable_matching_deployment_and_manifest(self):
        self.complete_pilots()
        pilot=self.matrix['disposable_validation_repositories'][0]
        original=copy.deepcopy(pilot['validation_evidence'])
        for field in ('deployment_source','release_version','deployment_sha','manifest_identity',
                      'manifest_evidence_url','release_download_verification_url'):
            pilot['validation_evidence']=copy.deepcopy(original)
            del pilot['validation_evidence'][field]
            with self.subTest(field=field), self.assertRaises(ValueError): self.check()
        for field,value in (('source','HemSoft/set-it-free-loop'),('sourceSha','d'*40),
                            ('version','2.1.0-rc.13'),('tier','unknown')):
            pilot['validation_evidence']=copy.deepcopy(original)
            pilot['validation_evidence']['manifest_identity'][field]=value
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, 'manifest must match'): self.check()
        pilot['validation_evidence']=original
        self.check()

    def test_manifest_release_version_must_be_semantic(self):
        row=self.complete_rollout()
        for value in ('unknown','main','v2.1.0','01.2.3','2.1.0-rc.01','2.1.0-'):
            row['manifest_version']=value
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, 'semantic manifest'): self.check()
        for value in ('2.1.0','2.1.0-rc.14','2.1.0-rc.14+build.1'):
            row['manifest_version']=value
            row['manifest_identity']['version']=value
            row['release_url']='https://github.com/hemsoft-dev/set-it-free-loop/releases/tag/v'+value
            self.check()

    def test_retained_status_cannot_reference_scope_decision_itself(self):
        self.scope['retained_repositories'][0]['status_evidence_url']='scope-decisions.json'
        with self.assertRaisesRegex(ValueError, 'separate captured API'): self.check()
        decision=self.scope['retained_repositories'][0]
        decision['status_evidence_url']=self.scope['retained_repositories'][1]['status_evidence_url']
        with self.assertRaisesRegex(ValueError, 'another repository'): self.check()

    def test_pending_records_do_not_claim_completion(self):
        self.assertEqual(self.check()['verified_rollouts'], 0)
        self.assertEqual(self.check()['repositories'], 67)

    def test_missing_matrix_repository_rejected(self):
        self.matrix['repositories'].pop()
        with self.assertRaisesRegex(ValueError, 'every baseline ID'):
            self.check()

    def test_duplicate_matrix_id_rejected(self):
        self.matrix['repositories'].append(copy.deepcopy(self.matrix['repositories'][0]))
        with self.assertRaisesRegex(ValueError, 'duplicate matrix ID'):
            self.check()

    def test_missing_ledger_repository_rejected(self):
        self.rows.pop(0)
        with self.assertRaisesRegex(ValueError, 'every baseline ID'):
            self.check()

    def test_changed_collision_mapping_rejected(self):
        self.rows[0]['destination'] = 'hemsoft-dev/hs-cli-confluence-search'
        with self.assertRaisesRegex(ValueError, 'mapping mismatch'):
            self.check()

    def test_second_provider_resource_per_repository_allowed(self):
        self.rows.append(copy.deepcopy(self.rows[0]))
        self.rows[-1]['provider'] = 'another pending provider'
        self.check()

    def test_verified_provider_requires_each_completion_field(self):
        complete = copy.deepcopy(self.complete_provider())
        fields = ('resource_owner', 'resource_url', 'billing_dependency', 'credential_source',
                  'credential_validity', 'affected_reference', 'transfer_action', 'smoke_test',
                  'recovery_action', 'verified_by', 'verified_at', 'evidence_url')
        for field in fields:
            with self.subTest(field=field):
                self.rows[0] = copy.deepcopy(complete)
                self.rows[0][field] = ''
                with self.assertRaises(ValueError):
                    self.check()
        self.rows[0] = complete
        self.check()

    def test_verified_absence_requires_reason(self):
        row = self.complete_provider()
        row['provider'] = 'none'
        row['absence_reason'] = ''
        with self.assertRaisesRegex(ValueError, 'absence'):
            self.check()
        row['absence_reason'] = 'Owner dashboard and source references verified absent'
        self.check()

    def test_credential_presence_is_not_validity(self):
        self.complete_provider()['credential_validity'] = 'present'
        with self.assertRaisesRegex(ValueError, 'validity'):
            self.check()

    def test_verified_rollout_requires_each_receipt(self):
        row = self.complete_rollout()
        complete = copy.deepcopy(row)
        for field in ('installed_tier', 'installed_addons', 'selected_addons', 'deployment_sha', 'review_head_sha',
                      'review_base_sha', 'gate_run_url', 'review_pr_url', 'transfer_evidence_url',
                      'status_evidence_url', 'selected_tier', 'destination_codex_access',
                      'destination_sfl_app_access', 'review_registration_url', 'review_registry_status_url',
                      'review_artifact_url', 'review_artifact_identity', 'gate_policy'):
            with self.subTest(field=field):
                row.clear()
                row.update(complete)
                row[field] = None
                with self.assertRaises(ValueError):
                    self.check()
        row.clear()
        row.update(complete)
        self.check()

    def test_legacy_source_cannot_claim_completed_rollout(self):
        self.complete_rollout()['deployment_source'] = 'HemSoft/set-it-free-loop'
        with self.assertRaisesRegex(ValueError, 'canonical source'):
            self.check()

    def test_wider_tier_needs_workflow_evidence(self):
        row = self.complete_rollout()
        row['selected_tier'] = 'full'
        row['manifest_identity']['tier'] = 'full'
        with self.assertRaisesRegex(ValueError, 'Wider tier'):
            self.check()
        row['wider_workflow_run_urls'] = ['https://example.com/run']
        self.check()

    def test_stale_summary_rejected(self):
        self.matrix['summary']['verified_rollouts'] = 67
        with self.assertRaisesRegex(ValueError, 'summary'):
            self.check()

    def test_archived_repository_not_treated_as_active_rollout(self):
        row = next(row for row in self.matrix['repositories'] if row['archived'])
        row['health'] = 'verified'
        self.complete_transfer_gates()
        with self.assertRaisesRegex(ValueError, 'archive-preserving'):
            self.check()
        row.update(health='archived_verified', transfer_evidence_url='https://example.com/transfer',
                   status_evidence_url='https://example.com/settings')
        self.check()

    def test_scope_exception_needs_owner_receipt(self):
        row = self.matrix['repositories'][0]
        row['health'] = 'scope_exception'
        self.complete_transfer_gates()
        row['transfer_evidence_url'] = 'https://example.com/transfer'
        row['status_evidence_url'] = 'https://example.com/settings'
        with self.assertRaises(ValueError):
            self.check()
        row['exception_evidence_url'] = 'https://github.com/HemSoft/set-it-free-loop/issues/139'
        self.check()

    def test_invalid_addon_lists_rejected(self):
        for addons in (['pr-review', 'pr-review'], [''], 'pr-review', [123]):
            self.matrix['repositories'][0]['selected_addons'] = addons
            with self.assertRaisesRegex(ValueError, 'selected_addons'):
                self.check()

    def test_disposable_repo_cannot_count_as_source_transfer(self):
        self.matrix['disposable_validation_repositories'][0]['repository_id'] = self.inventory['repositories'][0]['id']
        with self.assertRaisesRegex(ValueError, 'Disposable'):
            self.check()

    def test_local_evidence_cannot_escape_migration_directory(self):
        self.complete_provider()['evidence_url'] = '../ORGANIZATION-DEPLOYMENT.md'
        with self.assertRaisesRegex(ValueError, 'inside the migration'):
            self.check()

    def test_custom_tier_requires_recognized_component_evidence(self):
        row = self.complete_rollout()
        row['selected_tier'] = 'custom'
        row['manifest_identity']['tier'] = 'custom'
        row['installed_tier'] = 'custom'
        row['installed_components'] = ['sfl-auditor']
        row['wider_workflow_run_urls'] = ['https://example.com/auditor-run']
        for components in (None, [], ['unknown-workflow']):
            row['selected_components'] = components
            with self.assertRaisesRegex(ValueError, 'Custom tier'):
                self.check()
        row['selected_components'] = ['sfl-auditor']
        self.check()

    def test_legacy_installed_review_tier_recorded_truthfully(self):
        row = self.complete_rollout()
        row['installed_tier'] = 'review'
        self.check()
        row['selected_tier'] = 'review'
        with self.assertRaisesRegex(ValueError, 'canonical reviewer'):
            self.check()

    def test_unknown_selected_addon_rejected(self):
        row = self.complete_rollout()
        row['selected_addons'] = ['auditor']
        with self.assertRaisesRegex(ValueError, 'Unsupported selected addon'):
            self.check()
        row['selected_addons'] = ['pr-review']
        self.check()

    def test_review_only_custom_tier_needs_no_unrelated_workflow_run(self):
        row = self.complete_rollout()
        row.update(selected_tier='custom', installed_tier='custom', installed_components=['sfl-pr-review-auto'],selected_components=['sfl-pr-review-auto'])
        row['manifest_identity']['tier']='custom'
        row['wider_workflow_run_urls'] = []
        self.check()
        row['selected_components'].append('sfl-auditor')
        row['installed_components'].append('sfl-auditor')
        with self.assertRaisesRegex(ValueError, 'Wider tier'):
            self.check()

    def test_active_rollout_requires_both_verified_onboarding_pilots(self):
        self.complete_rollout()
        for pilot in self.matrix['disposable_validation_repositories']:
            pilot['validation_status'] = 'pending'
            with self.assertRaisesRegex(ValueError, 'public and private onboarding pilots'):
                self.check()
            pilot['validation_status'] = 'verified'
        self.check()

    def test_verified_pilot_requires_all_onboarding_and_sfl_receipts(self):
        self.complete_pilots()
        pilot = self.matrix['disposable_validation_repositories'][0]
        complete = copy.deepcopy(pilot['validation_evidence'])
        for field in complete:
            pilot['validation_evidence'] = copy.deepcopy(complete)
            del pilot['validation_evidence'][field]
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.check()
        pilot['validation_evidence'] = complete
        self.check()

    def test_pilot_status_required_even_during_preparation(self):
        del self.matrix['disposable_validation_repositories'][0]['validation_status']
        with self.assertRaisesRegex(ValueError, 'explicit validation status'):
            self.check()

    def test_terminal_exceptions_cannot_skip_new_repository_onboarding(self):
        for row in self.matrix['repositories']:
            if row['health'] == 'retained_source' or row['source'] == 'HemSoft/set-it-free-loop':
                continue
            self.verify_source_ledger(row['repository_id'])
            row.update(health='scope_exception', exception_evidence_url='https://example.com/exception',
                       transfer_evidence_url='https://example.com/transfer',
                       status_evidence_url='https://example.com/settings')
        self.complete_source()
        for pilot in self.matrix['disposable_validation_repositories']:
            pilot['validation_status'] = 'pending'
        with self.assertRaisesRegex(ValueError, 'public and private onboarding pilots'):
            self.check()

    def test_retained_sources_preserve_original_inventory_and_owner(self):
        summary = self.check()
        self.assertEqual(summary['repositories'], 67)
        self.assertEqual(summary['planned_transfers'], 65)
        self.assertEqual(summary['retained_sources'], 2)
        for decision in self.scope['retained_repositories']:
            row = next(row for row in self.matrix['repositories'] if row['repository_id'] == decision['repository_id'])
            self.assertEqual(row['actual_repository'], row['source'])
            self.assertEqual(row['health'], 'retained_source')

    def test_retained_source_cannot_be_transferred_or_waived_without_decision(self):
        decision = self.scope['retained_repositories'][0]
        row = next(row for row in self.matrix['repositories'] if row['repository_id'] == decision['repository_id'])
        row['health'] = 'pending_transfer'
        with self.assertRaisesRegex(ValueError, 'honor retained source'):
            self.check()

    def test_retention_needs_owner_receipt_and_exact_metadata(self):
        original = copy.deepcopy(self.scope['retained_repositories'][0])
        for field in ('decision_evidence_url', 'status_evidence_url', 'decision_owner', 'observed_metadata'):
            self.scope['retained_repositories'][0] = copy.deepcopy(original)
            del self.scope['retained_repositories'][0][field]
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.check()

    def test_completed_rollout_requires_all_provider_rows_verified(self):
        row = self.complete_rollout()
        pending = copy.deepcopy(next(item for item in self.rows if int(item['repository_id']) == row['repository_id']))
        pending['status'] = 'partial_provider_verified'
        pending['provider'] = 'additional provider'
        self.rows.append(pending)
        with self.assertRaisesRegex(ValueError, 'All transfer-target ledger gates'):
            self.check()

    def test_exception_does_not_waive_transfer_or_settings_evidence(self):
        row = self.matrix['repositories'][0]
        self.complete_transfer_gates()
        row.update(health='scope_exception', exception_evidence_url='https://example.com/owner-exception')
        with self.assertRaises(ValueError):
            self.check()
        row['transfer_evidence_url'] = 'https://example.com/transfer'
        with self.assertRaises(ValueError):
            self.check()
        row['status_evidence_url'] = 'https://example.com/settings'
        self.check()

    def test_optional_or_unbound_gate_cannot_claim_verified_rollout(self):
        row = self.complete_rollout()
        policy = copy.deepcopy(row['gate_policy'])
        for invalid in ('disabled', 'optional', {}, dict(policy, state='optional'),
                        dict(policy, strict=False), dict(policy, app_id=-1),
                        dict(policy, context='Other check')):
            row['gate_policy'] = invalid
            with self.assertRaisesRegex(ValueError, 'required strict SFL gate'):
                self.check()
        row['gate_policy'] = policy
        self.check()

    def test_native_or_stale_artifact_cannot_claim_sfl_runtime(self):
        row = self.complete_rollout()
        identity = copy.deepcopy(row['review_artifact_identity'])
        for invalid in (dict(identity, runtime='native_codex'), dict(identity, app_id=4448946), dict(identity, bot_user_id=1),
                        dict(identity, reviewed_head_sha='c'*40), dict(identity, reviewed_base_sha='b'*40)):
            row['review_artifact_identity'] = invalid
            with self.assertRaisesRegex(ValueError, 'SFL registered Codex immutable'):
                self.check()
        row['review_artifact_identity'] = identity
        self.check()

    def test_source_app_selection_must_match_sealed_owner_baseline(self):
        row = self.matrix['repositories'][0]
        row['source_app_access_in_baseline'] = not row['source_app_access_in_baseline']
        with self.assertRaisesRegex(ValueError, 'App access differs'):
            self.check()

    def test_observed_custom_tier_requires_components(self):
        row = self.complete_rollout()
        row['installed_tier'] = 'custom'
        for invalid in (None, []):
            row['installed_components'] = invalid
            with self.assertRaisesRegex(ValueError, 'Installed custom tier'):
                self.check()
        row['installed_components'] = ['sfl-pr-review-auto']
        row['selected_tier']='custom'
        row['selected_components']=['sfl-pr-review-auto']
        row['manifest_identity']['tier']='custom'
        self.check()

    def test_retention_cannot_expand_or_shrink_owner_approved_scope(self):
        original = copy.deepcopy(self.scope)
        self.scope['retained_repositories'].pop()
        with self.assertRaisesRegex(ValueError, 'fixed owner-approved'):
            self.check()
        self.scope = copy.deepcopy(original)
        candidate = copy.deepcopy(self.scope['retained_repositories'][0])
        repo = self.inventory['repositories'][0]
        candidate.update(repository_id=repo['id'], source=repo['full_name'], retained_repository=repo['full_name'])
        self.scope['retained_repositories'].append(candidate)
        with self.assertRaisesRegex(ValueError, 'fixed owner-approved'):
            self.check()
        self.scope = copy.deepcopy(original)
        self.scope['retained_repositories'][0] = candidate
        with self.assertRaisesRegex(ValueError, 'retained source ID'):
            self.check()

    def test_retention_cannot_substitute_an_arbitrary_owner_receipt(self):
        self.scope['retained_repositories'][0]['decision_evidence_url'] = 'https://example.com/approval'
        with self.assertRaisesRegex(ValueError, 'recorded owner decision'):
            self.check()

    def test_provider_candidates_cannot_be_removed_or_invented(self):
        for source, provider in (('HemSoft/codexbar', 'Fly.io'), ('HemSoft/dashboard', 'Vercel'),
                                 ('fhemmer/hs-cli-confluence-search', 'Blacksmith')):
            row = next(row for row in self.rows if row['source'] == source)
            original = row['provider_candidates_from_app_access']
            row['provider_candidates_from_app_access'] = '; '.join(name.strip() for name in original.split(';') if name.strip() != provider)
            with self.subTest(source=source), self.assertRaisesRegex(ValueError, 'Provider candidates'):
                self.check()
            row['provider_candidates_from_app_access'] = original + '; Invented provider'
            with self.subTest(source=source), self.assertRaisesRegex(ValueError, 'Provider candidates'):
                self.check()
            row['provider_candidates_from_app_access'] = original
        self.check()

    def test_disposable_inventory_cannot_be_removed_or_duplicated(self):
        extras = copy.deepcopy(self.matrix['disposable_validation_repositories'])
        for invalid in (None, [], [extras[0], extras[0]], [extras[0], dict(extras[0], repository_id=42)]):
            self.matrix['disposable_validation_repositories'] = invalid
            with self.assertRaises(ValueError):
                self.check()
        self.matrix['disposable_validation_repositories'] = extras
        self.check()


    def test_unrelated_pending_ledger_blocks_any_transfer(self):
        row=self.complete_rollout()
        other=next(item for item in self.rows if int(item['repository_id'])!=row['repository_id'])
        other['status']='owner_verification_pending'
        for health in ('verified','pending_rollout','pending_transfer'):
            row['health']=health
            with self.subTest(health=health), self.assertRaisesRegex(ValueError,'All transfer-target ledger gates'):
                self.check()

    def test_app_transfer_requires_all_transfer_target_gates(self):
        self.matrix['owned_app_transfer'].update(status='verified',owner='hemsoft-dev',
                                                evidence_url='https://example.com/app-transfer')
        with self.assertRaisesRegex(ValueError,'before App transfer'): self.check()
        self.complete_transfer_gates()
        self.check()

    def test_known_runner_cannot_disappear_or_become_absence(self):
        runner=next(row for row in self.rows if row.get('resource_kind')=='repository_runner')
        self.rows.remove(runner)
        with self.assertRaisesRegex(ValueError,'every observed repository runner'): self.check()
        self.rows.append(runner);runner['provider']='none'
        with self.assertRaisesRegex(ValueError,'runner cannot'): self.check()

    def test_custom_cannot_be_selected_for_a_fresh_or_noncustom_install(self):
        row=self.complete_rollout()
        row.update(selected_tier='custom',selected_components=['sfl-pr-review-auto'])
        row['manifest_identity']['tier']='custom'
        for tier in ('not_installed','reviewer','full'):
            row['installed_tier']=tier
            with self.subTest(tier=tier),self.assertRaisesRegex(ValueError,'existing custom'): self.check()

    def test_pilot_required_policy_and_authorized_review_context(self):
        self.complete_pilots()
        pilot=self.matrix['disposable_validation_repositories'][0]
        original=copy.deepcopy(pilot['validation_evidence'])
        for field,value in [('gate_policy',None),('requester_permission','read'),('review_requester',''),
                            ('review_pr_url','https://github.com/hemsoft-dev/other/pull/1'),
                            ('requester_permission_evidence_url',None)]:
            pilot['validation_evidence']=copy.deepcopy(original)
            pilot['validation_evidence'][field]=value
            with self.subTest(field=field),self.assertRaises(ValueError): self.check()
        pilot['validation_evidence']=copy.deepcopy(original)
        pilot['validation_evidence']['review_artifact_identity']['requester']='other'
        with self.assertRaisesRegex(ValueError,'artifact must bind'):self.check()

    def test_wider_pilot_and_auditor_are_required_before_rollout(self):
        self.complete_rollout()
        for pilot in self.matrix['disposable_validation_repositories']:
            pilot['validation_evidence']['wider_workflow_run_urls']=[]
        with self.assertRaisesRegex(ValueError,'wider-workflow and auditor pilot'): self.check()
        pilot=self.matrix['disposable_validation_repositories'][0]
        pilot['validation_evidence']['wider_workflow_run_urls']=['https://example.com/run']
        del pilot['validation_evidence']['auditor_run_url']
        with self.assertRaises(ValueError): self.check()

    def test_active_manifest_and_release_receipts_bind_identity(self):
        row=self.complete_rollout()
        original=copy.deepcopy(row)
        for field,value in [('manifest_identity',None),('release_url','https://github.com/other/source/releases/tag/v2.1.0'),
                            ('release_download_verification_url',None),('deployment_sha','d'*40)]:
            row.clear();row.update(copy.deepcopy(original));row[field]=value
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()


    def test_app_transfer_requires_retained_dependency_proof(self):
        self.complete_transfer_gates();self.complete_app_transfer()
        retained=next(row for row in self.matrix['repositories'] if row['health']=='retained_source')
        retained['retained_app_dependency']['status']='pending'
        with self.assertRaisesRegex(ValueError,'App transfer requires verified retained'): self.check()

    def test_completed_destination_app_access_requires_transfer(self):
        self.complete_rollout()
        self.matrix['owned_app_transfer']['status']='pending'
        with self.assertRaisesRegex(ValueError,'requires verified App transfer'):self.check()

    def test_installed_configuration_is_preserved(self):
        row=self.complete_rollout()
        row.update(installed_tier='full',installed_addons=['pr-review'])
        with self.assertRaisesRegex(ValueError,'preserve its installed tier and addons'):self.check()
        row['selected_tier']='full';row['manifest_identity']['tier']='full'
        with self.assertRaisesRegex(ValueError,'preserve its installed tier and addons'):self.check()
        row['selected_addons']=['pr-review'];row['wider_workflow_run_urls']=['https://example.com/run']
        self.check()

    def test_completed_review_requires_authorized_destination_pr(self):
        row=self.complete_rollout();original=copy.deepcopy(row)
        for field,value in [('requester_permission','read'),('requester_permission_evidence_url',None),
                            ('review_pr_url','https://github.com/hemsoft-dev/other/pull/1')]:
            row.clear();row.update(copy.deepcopy(original));row[field]=value
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()
        row.clear();row.update(original);row['review_artifact_identity']['requester']='other'
        with self.assertRaises(ValueError):self.check()

    def test_destination_codex_installation_cannot_lose_identity_or_proof(self):
        original=copy.deepcopy(self.matrix['destination_codex_installation'])
        for field,value in [('id',1),('app_id',4448946),('slug','other'),('repository_selection','selected'),
                            ('smoke_review_url','https://example.com/review'),('smoke_review_head','a'*40)]:
            self.matrix['destination_codex_installation']=copy.deepcopy(original)
            self.matrix['destination_codex_installation'][field]=value
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'Destination Codex'):self.check()
        del self.matrix['destination_codex_installation']
        with self.assertRaisesRegex(ValueError,'Destination Codex'):self.check()


if __name__ == '__main__':
    unittest.main()
