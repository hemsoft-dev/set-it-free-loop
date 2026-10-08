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
    def test_original_dashboard_policy_capture_binding(self):
        for key, value in [('method', 'DELETE'), ('http_status', 403),
                           ('request_url', 'https://api.github.com/repos/HemSoft/unrelated/rulesets/11400445'),
                           ('observed_at', '2000-01-01T00:00:00Z')]:
            self.rejects('repository-transfers/repository-transfer-1120402599.json',
                         lambda d, k=key, v=value: d['before_policy']['ruleset_details'][0].update({k: v}),
                         'Original dashboard policy')

    def test_app_ownership_capture_chronology(self):
        for key, value in [('started_at', '2000-01-01T00:00:00Z'),
                           ('completed_at', '2000-01-01T00:00:00Z')]:
            self.rejects('pr156-app-owned-metadata-current.json',
                         lambda d, k=key, v=value: d.update({k: v}), 'ownership capture')

    def test_pilot_final_default_head_capture(self):
        import json
        for role in ('private', 'public'):
            def mutate(d):
                data = json.loads(d['stdout'])
                data['commit']['sha'] = '0' * 40
                d['stdout'] = json.dumps(data)
            self.rejects('rc21-' + role + '-post-live-after-cleanup-head.json', mutate, 'final default head')

    def test_owned_app_public_discovery_is_rejected(self):
        self.rejects('pr156-app-anonymous-discovery-primary.json',
                     lambda d: d.update(http_status=200), 'unauthenticated public App endpoint')
        self.rejects('pr156-app-anonymous-discovery-primary.json',
                     lambda d: d.update(authentication='HemSoft'), 'unauthenticated public App endpoint')

    def test_historical_gate_list_and_detail_primary_binding(self):
        for role in ('private', 'public'):
            self.rejects('rc21-' + role + '-live-before-rulesets.json',
                         lambda d: d.update(exit_code=1), 'Pilot API capture')
            self.rejects('rc21-' + role + '-post-live-precleanup-owned-gate.json',
                         lambda d: d.update(argv=['gh', 'api', 'unrelated']), 'Pilot API capture')

    def test_actual_receipts_verify_identity_without_acceptance_claim(self):
        result = VALIDATOR.validate(DIRECTORY)
        self.assertEqual(result['transferred'], 65)
        self.assertEqual(result['source_protection_evidence'], 'incomplete')
        self.assertFalse(result['migration_acceptance_complete'])
        self.assertEqual(result['owned_app_transfer'], 'verified')
        self.assertEqual(result['app_coverage_targets'], 68)
        self.assertEqual(result['historical_pilot_premerge_strict_policy'], 'guard_snapshot_only')

    def test_executed_app_registration_and_run_are_primary_bound(self):
        self.rejects('pr156-app-owned-metadata-current.json',
                     lambda d: d['data']['owner'].update(id=8227352, login='HemSoft', type='User'),
                     'organization registration')
        self.rejects('organization-app-coverage-run-primary.json',
                     lambda d: d['data'].update(conclusion='failure'), 'successful reviewed main workflow')
        self.rejects('organization-app-coverage-verified.json',
                     lambda d: d.update(installation_id=1), 'actual artifact')
        self.rejects('executed-phase-status.json',
                     lambda d: d['owned_app_transfer'].update(owner='HemSoft'), 'Executed phase status')

    def test_rehashed_app_coverage_cannot_change_actual_installation_permissions(self):
        import hashlib
        import json
        path = DIRECTORY / 'execution/organization-app-repository-coverage-primary.json'
        changed = json.loads(path.read_bytes())
        changed['repositories'][0]['installation']['permissions']['contents'] = 'write'
        content = json.dumps(changed).encode()
        real_bytes, real_read = pathlib.Path.read_bytes, VALIDATOR.read
        def read_bytes(p):
            return content if p == path else real_bytes(p)
        def read(directory, name):
            value = real_read(directory, name)
            if name == 'organization-app-coverage-verified.json':
                value['artifact_json_sha256'] = hashlib.sha256(content).hexdigest()
            return value
        with mock.patch.object(pathlib.Path, 'read_bytes', read_bytes), mock.patch.object(VALIDATOR, 'read', read):
            with self.assertRaisesRegex(ValueError, 'archive'):
                VALIDATOR.validate(DIRECTORY)

    def test_app_archive_digest_and_member_binding_rejected(self):
        self.rejects('pr156-app-coverage-archive-primary.json',
                     lambda d: d.update(archive_sha256='0' * 64), 'archive')
        self.rejects('pr156-app-coverage-archive-primary.json',
                     lambda d: d.update(member='unrelated.json'), 'archive')
        self.rejects('organization-app-coverage-artifact-primary.json',
                     lambda d: d['data']['artifacts'][1].update(digest='sha256:' + '0' * 64), 'archive')

    def test_cli_version_first_line_rejects_dev_with_matching_update_tag(self):
        import json
        original_read = VALIDATOR.read
        original_text = pathlib.Path.read_text
        output = 'gh-sfl dev (2026-10-08) HemSoft\nLatest release (v2.1.0-rc.21)\n'
        def read(directory, name):
            value = copy.deepcopy(original_read(directory, name))
            if name == 'rc21-independent-download-proof.json':
                value['version_output'] = output
            elif name == 'rc21-independent-default-download-verification.json':
                result = json.loads(value['stdout'])
                result['version'] = output.strip()
                value['stdout'] = json.dumps(result)
            return value
        def read_text(path, *args, **kwargs):
            return output if path.name == 'rc21-version.log' else original_text(path, *args, **kwargs)
        with mock.patch.object(VALIDATOR, 'read', side_effect=read), mock.patch.object(pathlib.Path, 'read_text', read_text):
            with self.assertRaisesRegex(ValueError, 'isolated CLI logs'):
                VALIDATOR.validate(DIRECTORY)

    def test_protection_gap_derives_from_owner_cutover_and_audit_captures(self):
        self.rejects('fhemmer-protection-verification-limitation.json',
                     lambda d: d.update(actual_transfer_started_at='2026-10-08T02:21:00Z'), 'actual owner browser')
        self.rejects('repository-transfers/repository-transfer-1143951439.json',
                     lambda d: d['before_policy']['ruleset_pages'][0].update(http_status=200), 'cutover API refusal')
        self.rejects('fhemmer-post-transfer-browser-audit.json',
                     lambda d: d.update(text=d['text'] + '\nHemSoft – repo.add_branch_protection_rule\n'),
                     'limited event capture')

    def test_audit_rejects_protection_events_from_other_actors(self):
        for actor in ('SomeBot', 'github-actions[bot]', 'another-owner'):
            self.rejects('fhemmer-post-transfer-browser-audit.json',
                         lambda d, a=actor: d.update(text=d['text'] + '\n' + a + ' – repo.add_branch_protection_rule\n'),
                         'limited event capture')

    def test_explicit_visibility_is_preserved_for_transfers_and_retained_sources(self):
        self.rejects('repository-transfers/repository-transfer-1143951439.json',
                     lambda d: d['after']['data'].update(visibility='internal'), 'Transfer changed visibility')
        self.rejects('retained-repository-1162179521.json',
                     lambda d: d['data'].update(visibility='internal'), 'Retained repository configuration')

    def test_verifier_path_capture_binds_bytes_and_retrospective_timing(self):
        for field, value in [('captured_path', '/tmp/unrelated.py'), ('captured_sha256', '0' * 64),
                             ('capture_timing', 'at_execution'), ('observed_at', '2026-10-07T00:00:00Z')]:
            self.rejects('pr156-rc21-executed-verifier-path-capture.json',
                         lambda d, k=field, v=value: d.update({k: v}), 'Retrospective verifier path capture')

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

    def test_pilot_merge_requires_actual_post_merge_identity(self):
        self.rejects('rc21-private-live-merged-pr-current.json',
                     lambda d: d['data'].update(merged=False), 'post-merge PR capture')
        self.rejects('rc21-public-live-merged-pr-current.json',
                     lambda d: d['data']['head']['repo'].update(id=1), 'post-merge PR capture')

    def test_terminal_repeat_cannot_target_another_repository(self):
        for op in ('init', 'sync', 'status'):
            self.rejects('rc21-private-post-live-repeat-' + op + '.json',
                         lambda d: d['execution']['argv'].__setitem__(4, 'hemsoft-dev/unrelated'),
                         'terminal repeat')

    def test_source_gate_scope_and_unrelated_policy_are_validated(self):
        import json
        def wrong_scope(d):
            headers, body = d['stdout'].split('\n\n', 1)
            gate = json.loads(body)
            gate['conditions']['repository_id']['repository_ids'] = [1]
            d['stdout'] = headers + '\n\n' + json.dumps(gate)
        self.rejects('rc21-source-only-org-review-gate-create.json', wrong_scope, 'Source organization gate')
        def changed_rule(d):
            rule = json.loads(d['stdout'])
            rule['bypass_actors'] = [{'actor_id': 1}]
            d['stdout'] = json.dumps(rule)
        self.rejects('rc21-source-org-gate-after-unrelated.json', changed_rule, 'unrelated organization rule')

    def test_refs_cannot_drop_both_default_branch_captures(self):
        def drop(d):
            for side in ('before_refs', 'after_refs'):
                d[side]['refs'].clear()
                d[side]['responses'][0]['data'].clear()
        self.rejects('repository-transfers/repository-transfer-1143951439.json', drop, 'declared default branch')

    def test_refs_require_original_terminal_pagination_metadata(self):
        self.rejects('repository-transfers/repository-transfer-1143951439.json',
                     lambda d: d['before_refs']['responses'][0].pop('response_headers'), 'pagination headers')
        self.rejects('repository-transfers/repository-transfer-1143951439.json',
                     lambda d: d['before_refs']['responses'][0]['response_headers'].update(
                         Link='<https://api.github.com/repos/fhemmer/hs-cli-confluence-search/git/matching-refs/?page=2>; rel="next"'),
                     'pagination headers')

    def test_release_summary_is_bound_to_signed_primary_and_api(self):
        for field, value in [('source_sha', '0' * 40), ('immutable', False), ('signed_release_verified', False),
                             ('default_status_exit_code', 1)]:
            self.rejects('rc21-independent-download-proof.json',
                         lambda d, key=field, new=value: d.update({key: new}), 'release summary')
        self.rejects('rc21-independent-download-proof.json',
                     lambda d: d['digests'].update({'gh-sfl_2.1.0-rc.21_linux_amd64': '0' * 64}), 'asset digests')
        self.rejects('pr156-rc21-release-metadata-primary.json',
                     lambda d: d['data'].update(immutable=False), 'Immutable release API')
        self.rejects('rc21-signed-release-verification-primary.json',
                     lambda d: d['verificationResult']['statement']['predicate'].update(repositoryId='1'),
                     'signed statement')

    def test_dashboard_repair_is_bound_to_original_policy(self):
        self.rejects('dashboard-policy-repair.json',
                     lambda d: d['payload'].update(bypass_actors=[]), 'administrator bypass')
        self.rejects('dashboard-policy-repair.json',
                     lambda d: d['response'].update(request_url='https://api.github.com/repos/hemsoft-dev/unrelated/rulesets/11400445'),
                     'PUT response')
        self.rejects('dashboard-policy-repair.json',
                     lambda d: d['response']['data']['rules'].clear(), 'administrator bypass')

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

    def test_retained_projection_requires_successful_primary_receipt(self):
        primary = 'pr156-sixth-retained-nlg-current-primary.json'
        for mutation in (lambda d: d.update(actor='someone-else'),
                         lambda d: d.update(exit_code=1),
                         lambda d: d.update(argv=['gh', 'api', 'rate_limit']),
                         lambda d: d.update(started_at='2000-01-01T00:00:00Z'),
                         lambda d: d.update(completed_at='2099-01-01T00:00:00Z'),
                         lambda d: d.update(stdout='{}')):
            self.rejects(primary, mutation, 'successful command receipt')
        original = VALIDATOR.read
        def missing(directory, name):
            if name == primary:
                raise FileNotFoundError(name)
            return original(directory, name)
        with mock.patch.object(VALIDATOR, 'read', side_effect=missing):
            with self.assertRaises(FileNotFoundError):
                VALIDATOR.validate(DIRECTORY)

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

    def test_final_manifest_git_blob_and_deployment_fields_rejected(self):
        import base64
        import hashlib
        import json
        original = VALIDATOR.read
        for field, new in [('source', 'attacker/repo'), ('components', []),
                           ('enginePolicy', {'defaultProfile': 'untrusted', 'workflows': []})]:
            for rehash in (False, True):
                def changed(directory, name):
                    value = copy.deepcopy(original(directory, name))
                    if name == 'rc21-private-final-installed-manifest-capture.json':
                        data = value['data']
                        manifest = json.loads(base64.b64decode(data['content']))
                        manifest[field] = new
                        content = json.dumps(manifest).encode()
                        data['content'] = base64.b64encode(content).decode()
                        if rehash:
                            data['sha'] = hashlib.sha1(b'blob ' + str(len(content)).encode() + b'\0' + content).hexdigest()
                    elif name == 'rc21-private-final-installed-equivalence.json':
                        value['manifest'][field] = new
                    return value
                with self.subTest(field=field, rehash=rehash), mock.patch.object(VALIDATOR, 'read', side_effect=changed):
                    with self.assertRaisesRegex(ValueError, 'final installed manifest'):
                        VALIDATOR.validate(DIRECTORY)
        self.rejects('rc21-public-final-installed-manifest-capture.json',
                     lambda d: d['data'].update(sha='0' * 40), 'final installed manifest')

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

    def test_gate_urls_and_pull_identity_require_the_actual_pilot(self):
        for mutation in (lambda d: d['gate'].update(url='https://api.github.com/repos/other/repo/check-runs/1'),
                         lambda d: d['gate'].update(html_url='https://github.com/other/repo/runs/1'),
                         lambda d: d['gate'].update(pull_requests=[]),
                         lambda d: d['gate']['pull_requests'][0].update(number=999),
                         lambda d: d['gate']['pull_requests'][0].update(url='https://api.github.com/repos/other/repo/pulls/12'),
                         lambda d: d['gate']['pull_requests'][0]['head']['repo'].update(id=1),
                         lambda d: d['gate']['pull_requests'][0]['base']['repo'].update(id=1),
                         lambda d: d['gate']['pull_requests'][0]['head'].update(sha='0' * 40)):
            self.rejects('rc21-private-live-guarded-merge.json', mutation,
                         'gate repository and pull request binding')

    def test_release_payload_cannot_be_forged_under_original_signature(self):
        import base64
        import json
        def forge(value):
            value['verificationResult']['statement']['subject'][1]['digest']['sha256'] = '0' * 64
            value['attestation']['bundle']['dsseEnvelope']['payload'] = base64.b64encode(
                json.dumps(value['verificationResult']['statement']).encode()).decode()
        self.rejects('rc21-signed-release-verification-primary.json', forge, 'DSSE signature')
        self.rejects('rc21-signed-release-verification-primary.json',
                     lambda d: d['attestation']['bundle']['dsseEnvelope']['signatures'][0].update(sig='AAAA'),
                     'DSSE signature')

    def test_live_run_and_jobs_cannot_be_observed_before_completion(self):
        for role in ('private', 'public'):
            for suffix in ('run', 'jobs'):
                self.rejects(f'rc21-{role}-live-runtime-{suffix}.json',
                             lambda d: d.update(started_at='2000-01-01T00:00:00Z',
                                                completed_at='2000-01-01T00:00:01Z'),
                             'capture must follow terminal completion')

    def test_dropped_dashboard_policy_capture_is_repository_bound(self):
        for field, value in (('method', 'DELETE'), ('http_status', 500),
                             ('request_url', 'https://api.github.com/repos/other/repo/rulesets/11400445'),
                             ('observed_at', '2000-01-01T00:00:00Z')):
            self.rejects('dashboard-policy-repair.json',
                         lambda d: d['before']['ruleset_details'][0].update({field: value}),
                         'post-transfer ruleset GET')

    def test_destination_refs_cannot_precede_transfer_acceptance(self):
        def backdate(value):
            submitted = VALIDATOR.timestamp(value['submitted_at'])
            accepted = VALIDATOR.timestamp(value['transfer_response']['observed_at'])
            midpoint = submitted + (accepted - submitted) / 2
            value['after_refs']['responses'][0]['observed_at'] = midpoint.isoformat()
        self.rejects('repository-transfers/repository-transfer-1143951439.json', backdate,
                     'outside its transfer phase')

    def test_request_and_native_comment_urls_bind_the_actual_pilot(self):
        for comment in ('request', 'native_clean'):
            for field in ('url', 'issue_url', 'html_url'):
                self.rejects('rc21-private-live-guarded-merge.json',
                             lambda d: d[comment].update({field: 'https://github.com/other/repo/pull/999'}),
                             'request binding URLs')

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
