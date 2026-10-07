"""Regression tests for offline ledger and rollout completion gates."""

import copy
import csv
import importlib.util
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch

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

    def capture(self, value, directory=DIRECTORY, prefix='fixture-capture-'):
        with tempfile.NamedTemporaryFile(dir=directory,suffix='.json',prefix=prefix,delete=False) as stream:
            path=pathlib.Path(stream.name)
        path.write_text(json.dumps(value));self.addCleanup(path.unlink,missing_ok=True)
        return path.name

    def branch(self, repository_id):
        return next((r['default_branch'] for r in self.inventory['repositories'] if r['id']==repository_id),'main')

    def bind_manifest(self, row, directory=DIRECTORY, repository_id=None, repository=None, revision='b'*40):
        repository_id=repository_id or row['repository_id'];repository=repository or row['destination']
        row['manifest_evidence_url']=self.capture({'repository_id':repository_id,'repository':repository,
            'revision_sha':revision,'manifest':row['manifest_identity'],'observed_at':'2026-10-07T02:00:00Z'},directory)

    def bind_smoke(self, row, directory=DIRECTORY):
        row.update(smoke_outcome='success',smoke_phase='post_transfer')
        row['smoke_evidence_url']=self.capture({'repository_id':int(row['repository_id']),
            'repository':row['source'] if int(row['repository_id']) in validator.APPROVED_RETAINED_IDS else row['destination'],
            'provider':row['provider'],
            'resource_kind':row.get('resource_kind'),'resource_id':row.get('resource_id'),'outcome':'success',
            'phase':'post_transfer','destructive_changes':False,'continuity_verified':True,
            'observed_at':'2026-10-07T02:00:00Z'},directory)

    def gate_policy(self, repository_id, repository, directory=DIRECTORY):
        capture = {'repository_id':repository_id,'repository':repository,'branch':self.branch(repository_id),
                   'observed_at':'2026-10-07T02:00:00Z','classic_protection':{'state':'absent','http_status':404},
                   'effective_rules':{'state':'observed','data':[{'type':'required_status_checks','parameters':{
                       'strict_required_status_checks_policy':True,
                       'required_status_checks':[{'context':'SFL Reviewer Gate Runner','integration_id':15368}]}}]}}
        with tempfile.NamedTemporaryFile(dir=directory,suffix='.json',prefix='fixture-gate-',delete=False) as stream:
            path=pathlib.Path(stream.name)
        path.write_text(json.dumps(capture));self.addCleanup(path.unlink,missing_ok=True)
        return {'state':'required','context':'SFL Reviewer Gate Runner','app_id':15368,'strict':True,
                'repository_id':repository_id,'repository':repository,'branch':self.branch(repository_id),'evidence_url':path.name}

    def complete_provider(self):
        row = self.rows[0]
        row.update(status='verified', provider='example', resource_owner='HemSoft',
                   resource_url='https://example.com/resource', billing_dependency='No purchase required',
                   credential_source='Provider-owned integration', credential_validity='verified',
                   affected_reference='Git repository link', transfer_action='Reconnect exact repository ID',
                   smoke_test='Expected response verified', recovery_action='Restore previous repository link',
                   verified_by='HemSoft', verified_at='2026-10-06T20:00:00-04:00',
                   evidence_url='https://github.com/HemSoft/set-it-free-loop/issues/138')
        self.bind_smoke(row)
        return row

    def verify_source_ledger(self, repo_id):
        for row in self.rows:
            if int(row['repository_id']) == repo_id:
                row.update(status='verified', provider={'vercel_project':'Vercel','repository_runner':'GitHub Actions','github_pages':'GitHub Pages','supabase_project':'Supabase','cloudflare_zone':'Cloudflare','cloudflare_worker':'Cloudflare'}.get(row.get('resource_kind'),'none'), absence_reason='Synthetic owner absence receipt',
                           verified_by='HemSoft', verified_at='2026-10-06T20:00:00-04:00',
                           evidence_url='https://github.com/HemSoft/set-it-free-loop/issues/138')
                if row['provider'] == 'none':
                    row.update(evidence_url=validator.EXTERNAL_SCOPE_RECEIPT, verified_at='2026-10-07T02:00:00Z')
                if row.get('resource_kind') in {'vercel_project','github_pages','supabase_project','cloudflare_zone','cloudflare_worker'}:
                    row.update(resource_owner='Synthetic verified project owner', credential_source='Provider-owned integration',
                               credential_validity='verified', affected_reference='Existing Git project link',
                               transfer_action='Reconnect after gated transfer', smoke_test='Synthetic deployment succeeds',
                               recovery_action='Retain previous deployment',billing_dependency='Existing plan')
                if row.get('resource_kind')=='supabase_project' and row.get('resource_id')=='cevpnetigzotgstxxjpm':
                    row['transfer_action']='Preserve paused database and configuration; no project/database transfer, resume, query or deletion'
                if row.get('resource_kind')=='vercel_project' and row.get('resource_id')=='prj_hPjAbxtMlCi3A5waKxQpjATto0ae':
                    row['transfer_action']='Git connection retired before transfer; transfer GitHub repository without reconnecting Vercel'
                if row.get('resource_kind') == 'repository_runner':
                    row.update(resource_owner='HemSoft', resource_url='https://example.com/runner',
                               billing_dependency='Existing VM',credential_source='Existing registration',
                               credential_validity='verified',affected_reference='Runner 21',
                               transfer_action='Verify registration continuity',smoke_test='Manual smoke succeeds',
                               recovery_action='Restore exact registration if needed')

                if row['provider']!='none':self.bind_smoke(row)

    def bind_consumer_runs(self, row):
        self.bind_manifest(row)
        row['wider_workflow_run_urls'] = ['https://github.com/'+row['destination']+'/actions/runs/'+str(i+1)
                                         for i,_ in enumerate(row['wider_workflow_run_urls'])]
        paths=validator.deployed_workflow_paths(row['selected_tier'],row['selected_addons'],row['selected_components']) - {'.github/workflows/sfl-pr-review-auto.yml'}
        row['wider_operation_receipts'] = [{'repository_id':row['repository_id'],'repository':row['destination'],
            'deployment_sha':row['deployment_sha'],'release_version':row['manifest_version'],'evidence_url':run,
            'conclusion':'success','run_head_sha':'b'*40,'workflow':next(iter(sorted(paths)),'.github/workflows/sfl-auditor.yml')}
            for run in row['wider_workflow_run_urls']]

    def bind_review_operations(self, row, repository_id=None, repository=None, directory=DIRECTORY):
        repository_id = repository_id or row['repository_id']
        repository = repository or row['destination']
        row['gate_policy']=self.gate_policy(repository_id,repository,directory)
        fields = ('gate_run_url','review_registration_url','review_registry_status_url','review_artifact_url')
        row['review_operation_receipts'] = {}
        for field in fields:
            row[field] = 'https://github.com/'+repository+'/issues/1#'+field
            row['review_operation_receipts'][field] = {'repository_id':repository_id,'repository':repository,
                'head_sha':row['review_head_sha'],'base_sha':row['review_base_sha'],'pr_url':row['review_pr_url'],
                'evidence_url':row[field]}

        row['requester_permission_evidence_url']=self.capture({'repository_id':repository_id,'repository':repository,
            'actor':row['review_requester'],'pr_url':row['review_pr_url'],'head_sha':row['review_head_sha'],
            'base_sha':row['review_base_sha'],'http_status':200,'result':{'permission':'admin','role_name':'admin'},
            'observed_at':'2026-10-07T02:00:00Z'},directory)
        row['gate_run_url']='https://github.com/'+repository+'/runs/1'
        gate=row['review_operation_receipts']['gate_run_url']
        gate.update(evidence_url=row['gate_run_url'],status='completed',conclusion='success',
            workflow='.github/workflows/sfl-pr-review-auto.yml',app_id=15368,context='SFL Reviewer Gate Runner')
        gate['capture_evidence_url']=self.capture(dict(gate,artifact_identity=row['review_artifact_identity'],
            observed_at='2026-10-07T02:00:00Z'),directory)

    def complete_app_coverage(self, row):
        row['destination_sfl_app_access'] = {'status':'verified','app_id':4448946,'owner':'hemsoft-dev',
            'repository_id':row['repository_id'],'repository':row['destination'],'installation_id':123,
            'evidence_url':'https://example.com/app-access'}

    def complete_post_transfer_access(self, row):
        if row['repository_id'] == 1143951439:
            row['post_transfer_access'].update(status='verified', effective_permission='none', filled_seats=1,
                paid_seats=1, verified_by='HemSoft', verified_at='2026-10-06T20:00:00-04:00',
                permission_evidence_url='https://example.com/access', license_evidence_url='https://example.com/license')

    def complete_scope_decision(self, row):
        if row['source_app_access_in_baseline']:
            self.complete_app_transfer();self.complete_app_coverage(row)
        row['scope_exception_decision'] = {'repository_id':row['repository_id'],'repository':row['destination'],
            'approved_by':'HemSoft','approved_at':'2026-10-06T20:00:00-04:00','reason':'Synthetic owner-approved fixture',
            'disposition':'exclude_runtime_rollout'}
        decision=row['scope_exception_decision']
        receipt={'author':'HemSoft','created_at':decision['approved_at'],
            'url':'https://github.com/HemSoft/set-it-free-loop/issues/139#issuecomment-123',
            'decision':{field:decision[field] for field in ('repository_id','repository','disposition','reason')}}
        row['exception_evidence_url']=self.capture(dict(decision,owner_comment=receipt))
        decision['evidence_url']=row['exception_evidence_url']

    def complete_transfer_gates(self):
        self.matrix['pre_transfer_credential_verification'] = {'repository_id':1169772257,
            'repository':'HemSoft/set-it-free-loop','workflow':'.github/workflows/verify-sfl-app-credential.yml',
            'conclusion':'success','reviewed_sha':'e'*40,'app_id':4448946,'client_id':'Iv23liwvwJJUh2bUIKLW',
            'owner':'HemSoft','installation_id':150383874,'repository_selection':'all','permission_ceiling_verified':True,
            'run_url':'https://github.com/HemSoft/set-it-free-loop/actions/runs/1'}
        for row in self.matrix['repositories']:
            repo=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id'])
            if repo['full_name']=='HemSoft/yahtzee':
                row['post_transfer_runner']={'repository_id':repo['id'],'repository':repo['destination'],'runner_id':21,
                    'online':True,'idle':True,'isolated':True,'service_active':True,'run_conclusion':'success',
                    'run_head_sha':'e'*40,'run_url':'https://github.com/'+repo['destination']+'/actions/runs/1',
                    'registration_evidence_url':'https://example.com/registration','isolation_evidence_url':'https://example.com/isolation',
                    'service_evidence_url':'https://example.com/service'}
            row['destination_protections'] = dict(validator.protection_contract(repo),
                repository_id=repo['id'],repository=repo['destination'],revision_sha='e'*40,
                observed_at='2026-10-07T02:00:00Z')
            row['destination_protections']['evidence_url']=self.capture(dict(row['destination_protections'],phase='post_transfer'))
        for repo in self.inventory['repositories']:
            self.verify_source_ledger(repo['id'])

    def complete_app_transfer(self):
        self.matrix['owned_app_transfer'].update(status='verified',owner='hemsoft-dev',evidence_url='https://example.com/app-transfer')
        path=DIRECTORY/'owned-app-organization-installation-evidence.json'
        if not hasattr(self,'original_owned_installation_capture'):
            self.original_owned_installation_capture=path.read_text()
            self.addCleanup(path.write_text,self.original_owned_installation_capture)
        path.write_text(json.dumps({'verification_status':'verified','account':{'login':'hemsoft-dev'},
            'installation':{'id':123,'app_id':4448946,'repository_selection':'all'}}))

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
        self.complete_app_coverage(row)
        row['pre_sync_installation'] = {'state':'absent','tier':'not_installed',
            'repository_id':row['repository_id'],'repository':row['destination'],'revision_sha':'d'*40,
            'manifest_paths':['.sfl/sfl.json','sfl.json'],'evidence_url':'https://example.com/pre-sync',
            'addons':[],'components':[]}
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
        self.bind_review_operations(row)
        self.verify_source_ledger(row['repository_id'])
        if row['repository_id'] == 1143951439:
            row['post_transfer_access'].update(status='verified', effective_permission='none', filled_seats=1,
                paid_seats=1, verified_by='HemSoft', verified_at='2026-10-06T20:00:00-04:00',
                permission_evidence_url='https://example.com/access', license_evidence_url='https://example.com/license')
        row.update(manifest_identity={'source':row['deployment_source'],'sourceSha':row['deployment_sha'],
                                      'version':row['manifest_version'],'tier':row['selected_tier'],'addons':[],'components':['sfl-pr-review-auto']},
                   manifest_evidence_url='https://example.com/manifest',
                   release_url='https://github.com/hemsoft-dev/set-it-free-loop/releases/tag/v'+row['manifest_version'],
                   release_download_verification_url='https://example.com/checksum')
        self.bind_manifest(row)
        self.matrix['summary']['verified_rollouts'] += 1
        return row

    def complete_pilots(self):
        self.complete_transfer_gates()
        self.complete_app_transfer()
        for pilot in self.matrix['disposable_validation_repositories']:
            pilot['validation_status'] = 'verified'
            pilot['validation_evidence'] = {field: 'https://github.com/' + pilot['repository'] + '/issues/1#' + field for field in
                ('init_pr_url', 'sync_pr_url', 'repeat_sync_evidence_url',
                 'repeat_onboarding_evidence_url', 'review_registration_url',
                 'review_registry_status_url', 'review_artifact_url', 'gate_run_url',
                 'status_evidence_url', 'gate_uninstall_evidence_url')}
            pilot['validation_evidence'].update(deployment_source='hemsoft-dev/set-it-free-loop',
                release_version='2.1.0-rc.14', deployment_sha='a'*40,
                manifest_identity={'source':'hemsoft-dev/set-it-free-loop','sourceSha':'a'*40,
                                   'version':'2.1.0-rc.14','tier':'reviewer','addons':[]},
                manifest_evidence_url='https://example.com/manifest',
                release_download_verification_url='https://example.com/checksum')
            receipts=pilot['validation_evidence']
            receipts['init_pr_url']='https://github.com/'+pilot['repository']+'/pull/1'
            receipts['sync_pr_url']='https://github.com/'+pilot['repository']+'/pull/2'
            receipts['operation_receipts']={field:{'repository_id':pilot['repository_id'],'repository':pilot['repository'],
                'deployment_sha':receipts['deployment_sha'],'release_version':receipts['release_version'],'evidence_url':receipts[field]}
                for field in ('init_pr_url','sync_pr_url','repeat_sync_evidence_url','repeat_onboarding_evidence_url',
                    'review_registration_url','review_registry_status_url','review_artifact_url','gate_run_url',
                    'status_evidence_url','gate_uninstall_evidence_url')}
            receipts.update(review_requester='HemSoft',review_pr_url='https://github.com/'+pilot['repository']+'/pull/1',
                review_head_sha='b'*40,review_base_sha='c'*40,requester_permission='admin',
                requester_permission_evidence_url='https://example.com/permission',
                gate_policy={'state':'required','context':'SFL Reviewer Gate Runner','app_id':15368,
                             'strict':True,'evidence_url':'https://example.com/rule'},wider_workflow_run_urls=[])
            if pilot['visibility']=='private':
                receipts['manifest_identity']['tier']='full'
                receipts['wider_workflow_run_urls']=['https://github.com/'+pilot['repository']+'/actions/runs/1']
                receipts['auditor_run_url']='https://github.com/'+pilot['repository']+'/actions/runs/2'
                receipts['wider_operation_receipts']=[{'repository_id':pilot['repository_id'],'repository':pilot['repository'],
                    'deployment_sha':receipts['deployment_sha'],'release_version':receipts['release_version'],
                    'evidence_url':receipts['wider_workflow_run_urls'][0],'conclusion':'success',
                    'workflow':'.github/workflows/sfl-dispatcher.yml','run_head_sha':'b'*40}]
                receipts['auditor_operation_receipt']=dict(receipts['wider_operation_receipts'][0],evidence_url=receipts['auditor_run_url'],workflow='.github/workflows/sfl-auditor.yml')
            receipts['destination_sfl_app_access'] = {'status':'verified','app_id':4448946,'owner':'hemsoft-dev',
                'repository_id':pilot['repository_id'],'repository':pilot['repository'],'installation_id':123,
                'evidence_url':'https://example.com/pilot-app-access'}
            receipts['scenario_receipts'] = {name:{'outcome':outcome,'mode':'workflow_fixture',
                'deployment_sha':receipts['deployment_sha'],'repository_id':pilot['repository_id'],
                'repository':pilot['repository'],'release_version':receipts['release_version'],
                'evidence_url':'https://github.com/'+pilot['repository']+'/issues/1#scenario-'+name}
                for name,outcome in validator.PILOT_SCENARIOS.items()}
            pilot['validation_evidence']['review_artifact_identity'] = {
                'runtime': 'sfl_registered_codex', 'app_id': 1144995, 'bot_user_id': 199175422,
                'reviewed_head_sha': 'b'*40, 'reviewed_base_sha': 'c'*40,
                'review_pr_url':receipts['review_pr_url'],'requester':receipts['review_requester']}
            self.bind_review_operations(receipts, pilot['repository_id'], pilot['repository'])
            for field in receipts['review_operation_receipts']:
                receipts['operation_receipts'][field]['evidence_url']=receipts[field]
            self.bind_manifest(receipts,repository_id=pilot['repository_id'],repository=pilot['repository'])
            for field,operation in receipts['operation_receipts'].items():
                if field not in {'init_pr_url','sync_pr_url','repeat_sync_evidence_url','repeat_onboarding_evidence_url','status_evidence_url','gate_uninstall_evidence_url'}:continue
                command='init' if field in {'init_pr_url','repeat_onboarding_evidence_url'} else 'sync' if field in {'sync_pr_url','repeat_sync_evidence_url'} else 'status' if field=='status_evidence_url' else 'uninstall-gate'
                outcome='pull_request_merged' if field in {'init_pr_url','sync_pr_url'} else 'healthy' if command=='status' else 'gate_removed' if command=='uninstall-gate' else 'no_changes'
                operation.update(command=command,outcome=outcome,revision_before='b'*40,revision_after='b'*40,
                    change_count=0,merged=outcome=='pull_request_merged',gate_only=True,unrelated_change_count=0)


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
        self.complete_app_coverage(row)
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
        self.bind_review_operations(row)
        proof=row['in_place_evidence'];proof['workflow_run_urls']=['https://github.com/'+row['destination']+'/actions/runs/1']
        proof['workflow_operation_receipts']=[{'repository_id':row['repository_id'],'repository':row['destination'],
            'deployment_sha':proof['source_sha'],'release_version':proof['release_version'],'evidence_url':proof['workflow_run_urls'][0],
            'conclusion':'success','workflow':'.github/workflows/validate-gh-sfl.yml','run_head_sha':proof['source_sha']}]
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
            self.bind_manifest(row)
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
        row['absence_reason'] = 'Owner confirms no other active external resources'
        row.update(evidence_url=validator.EXTERNAL_SCOPE_RECEIPT,verified_at='2026-10-07T02:00:00Z')
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
        self.bind_manifest(row)
        with self.assertRaisesRegex(ValueError, 'Wider tier'):
            self.check()
        row['wider_workflow_run_urls'] = ['https://example.com/run']
        self.bind_consumer_runs(row)
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
        self.complete_app_transfer();self.complete_app_coverage(row)
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
        self.complete_scope_decision(row)
        self.complete_post_transfer_access(row)
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
        row['pre_sync_installation'].update(tier=row['installed_tier'],state='absent' if row['installed_tier']=='not_installed' else 'present')
        row['installed_components'] = ['sfl-auditor','sfl-pr-review-auto']
        row['pre_sync_installation']['components']=['sfl-auditor','sfl-pr-review-auto']
        row['wider_workflow_run_urls'] = ['https://example.com/auditor-run']
        self.bind_consumer_runs(row)
        for components in (None, [], ['unknown-workflow']):
            row['selected_components'] = components
            with self.assertRaisesRegex(ValueError, 'Custom tier'):
                self.check()
        row['selected_components'] = ['sfl-auditor','sfl-pr-review-auto']
        row['manifest_identity']['components'] = ['sfl-auditor','sfl-pr-review-auto']
        self.bind_consumer_runs(row)
        self.check()

    def test_legacy_installed_review_tier_recorded_truthfully(self):
        row = self.complete_rollout()
        row['installed_tier'] = 'review'
        row['pre_sync_installation'].update(tier=row['installed_tier'],state='absent' if row['installed_tier']=='not_installed' else 'present')
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
        row['manifest_identity']['addons'] = ['pr-review']
        self.bind_consumer_runs(row)
        self.check()

    def test_review_only_custom_tier_needs_no_unrelated_workflow_run(self):
        row = self.complete_rollout()
        row.update(selected_tier='custom', installed_tier='custom', installed_components=['sfl-pr-review-auto'],selected_components=['sfl-pr-review-auto'])
        row['pre_sync_installation'].update(tier=row['installed_tier'],state='absent' if row['installed_tier']=='not_installed' else 'present')
        row['manifest_identity']['tier']='custom'
        row['pre_sync_installation']['components']=row['installed_components']
        row['wider_workflow_run_urls'] = []
        self.bind_manifest(row)
        self.check()
        row['selected_components'].append('sfl-auditor')
        row['installed_components'].append('sfl-auditor')
        row['manifest_identity']['components'].append('sfl-auditor')
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
            self.complete_post_transfer_access(row)
            self.complete_scope_decision(row)
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
        self.complete_post_transfer_access(row)
        self.complete_scope_decision(row)
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
        row['pre_sync_installation'].update(tier=row['installed_tier'],state='absent' if row['installed_tier']=='not_installed' else 'present')
        for invalid in (None, []):
            row['installed_components'] = invalid
            with self.assertRaisesRegex(ValueError, 'Installed custom tier'):
                self.check()
        row['installed_components'] = ['sfl-pr-review-auto']
        row['selected_tier']='custom'
        row['selected_components']=['sfl-pr-review-auto']
        row['manifest_identity']['tier']='custom'
        row['pre_sync_installation']['components']=row['installed_components']
        self.bind_manifest(row)
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
            row['pre_sync_installation'].update(tier=row['installed_tier'],state='absent' if row['installed_tier']=='not_installed' else 'present')
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
        row['pre_sync_installation'].update(addons=['pr-review'])
        row['pre_sync_installation'].update(tier=row['installed_tier'],state='absent' if row['installed_tier']=='not_installed' else 'present')
        with self.assertRaisesRegex(ValueError,'preserve its installed tier and addons'):self.check()
        row['selected_tier']='full';row['manifest_identity']['tier']='full'
        with self.assertRaisesRegex(ValueError,'preserve its installed tier and addons'):self.check()
        row['selected_addons']=['pr-review'];row['wider_workflow_run_urls']=['https://example.com/run']
        self.bind_consumer_runs(row)
        row['manifest_identity']['addons']=['pr-review']
        self.bind_manifest(row)
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


    def test_manifest_must_observe_selected_addons_and_custom_components(self):
        row = self.complete_rollout()
        row.update(installed_tier='minimal', selected_tier='minimal', installed_addons=['pr-review'],
                   selected_addons=['pr-review'], wider_workflow_run_urls=['https://example.com/run'])
        row['pre_sync_installation'].update(tier=row['installed_tier'],state='absent' if row['installed_tier']=='not_installed' else 'present')
        row['manifest_identity']['tier'] = 'minimal'
        row['pre_sync_installation']['addons']=row['installed_addons']
        for addons in (None, [], ['policy-manager']):
            row['manifest_identity']['addons'] = addons
            with self.subTest(addons=addons), self.assertRaisesRegex(ValueError, 'manifest must match the selected addons'):
                self.check()
        row['manifest_identity']['addons'] = ['pr-review']
        self.bind_consumer_runs(row)
        self.check()
        row.update(installed_tier='custom', selected_tier='custom', installed_components=['sfl-pr-review-auto'],
                   selected_components=['sfl-pr-review-auto'])
        row['pre_sync_installation'].update(tier=row['installed_tier'],state='absent' if row['installed_tier']=='not_installed' else 'present')
        row['manifest_identity']['tier'] = 'custom'
        row['pre_sync_installation']['components']=row['installed_components']
        for components in (None, [], ['sfl-auditor']):
            row['manifest_identity']['components'] = components
            with self.subTest(components=components), self.assertRaisesRegex(ValueError, 'custom manifest must match'):
                self.check()
        row['manifest_identity']['components'] = ['sfl-pr-review-auto']
        row['wider_workflow_run_urls']=[]
        self.bind_consumer_runs(row)
        self.check()

    def test_fresh_pilot_only_accepts_init_supported_tiers(self):
        self.complete_pilots()
        public = next(p for p in self.matrix['disposable_validation_repositories'] if p['visibility'] == 'public')
        for tier in ('custom', 'review'):
            public['validation_evidence']['manifest_identity']['tier'] = tier
            with self.subTest(tier=tier), self.assertRaisesRegex(ValueError, 'pilot manifest'):
                self.check()

    def test_designated_pilot_identity_and_visibility_cannot_be_replaced(self):
        original = copy.deepcopy(self.matrix['disposable_validation_repositories'])
        for field, value in [('repository_id', 42), ('repository', 'hemsoft-dev/replacement'), ('visibility', 'public')]:
            self.matrix['disposable_validation_repositories'] = copy.deepcopy(original)
            self.matrix['disposable_validation_repositories'][0][field] = value
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, 'designated identities'):
                self.check()
        self.matrix['disposable_validation_repositories'] = original[:1]
        with self.assertRaisesRegex(ValueError, 'Both designated'): self.check()

    def test_fhemmer_transfer_requires_effective_access_and_one_seat_receipt(self):
        self.complete_transfer_gates()
        row = next(r for r in self.matrix['repositories'] if r['repository_id'] == 1143951439)
        row.update(health='pending_rollout', transfer_evidence_url='https://example.com/transfer')
        with self.assertRaisesRegex(ValueError, 'post-transfer access and seat proof'): self.check()
        row['post_transfer_access'].update(status='verified', effective_permission='none', filled_seats=1,
            paid_seats=1, verified_by='HemSoft', verified_at='2026-10-06T20:00:00-04:00',
            permission_evidence_url='https://example.com/access', license_evidence_url='https://example.com/license')
        self.check()
        original = copy.deepcopy(row['post_transfer_access'])
        for field, value in [('effective_permission','admin'), ('account','HemSoft'), ('repository','fhemmer/hs-cli-confluence-search'),
                             ('filled_seats',2), ('paid_seats',2), ('paid_seats',True), ('permission_evidence_url',None),
                             ('license_evidence_url',None), ('verified_at','2026-10-06T20:00:00')]:
            row['post_transfer_access'] = copy.deepcopy(original)
            row['post_transfer_access'][field] = value
            with self.subTest(field=field,value=value), self.assertRaises(ValueError): self.check()


    def test_wider_pilot_must_deploy_an_auditor_tier(self):
        self.complete_pilots()
        pilot = next(p for p in self.matrix['disposable_validation_repositories'] if p['visibility']=='private')
        pilot['validation_evidence']['manifest_identity']['tier']='minimal'
        pilot['validation_evidence']['manifest_identity']['addons']=['pr-review']
        self.bind_manifest(pilot['validation_evidence'],repository_id=pilot['repository_id'],repository=pilot['repository'])
        with self.assertRaisesRegex(ValueError, 'wider deployed configuration'): self.check()

    def test_unused_credentials_remain_bound_to_exact_owner_scope(self):
        original=copy.deepcopy(self.rows)
        unused=next(r for r in self.rows if r['unused_repository_credential_names'])
        for field,value in [('unused_repository_credential_names',''),
                            ('unused_repository_credential_names','OPENAI_API_KEY'),
                            ('unused_repository_credential_evidence_url','https://example.com/unrelated')]:
            previous=unused[field];unused[field]=value
            with self.subTest(field=field,value=value), self.assertRaisesRegex(ValueError,'unused credential'): self.check()
            unused[field]=previous
        referenced=next(r for r in self.rows if r['source']=='HemSoft/hs-buddy')
        referenced['unused_repository_credential_names']='SFL_APP_PRIVATE_KEY'
        with self.assertRaisesRegex(ValueError,'unused credential names'): self.check()
        self.rows=original
        self.check()

    def test_verified_pilot_needs_transferred_app_and_bound_access_receipt(self):
        self.complete_pilots()
        self.matrix['owned_app_transfer']['status']='pending'
        with self.assertRaisesRegex(ValueError,'pilot requires completed owned App transfer'): self.check()
        self.complete_app_transfer()
        pilot=self.matrix['disposable_validation_repositories'][0]
        original=copy.deepcopy(pilot['validation_evidence']['destination_sfl_app_access'])
        for field,value in [('status','pending'),('owner','HemSoft'),('app_id',1144995),
                            ('repository_id',42),('repository','hemsoft-dev/other'),('installation_id',0),('evidence_url',None)]:
            pilot['validation_evidence']['destination_sfl_app_access']=copy.deepcopy(original)
            pilot['validation_evidence']['destination_sfl_app_access'][field]=value
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()

    def test_known_vercel_projects_cannot_disappear_or_become_absence(self):
        row=next(r for r in self.rows if r['resource_kind']=='vercel_project')
        row['provider']='none'
        with self.assertRaisesRegex(ValueError,'Vercel project cannot'):self.check()
        row['provider']='Vercel';row['resource_kind']=''
        with self.assertRaisesRegex(ValueError,'every captured Vercel'):self.check()
        row['resource_kind']='vercel_project';row['resource_id']='other'
        with self.assertRaisesRegex(ValueError,'Vercel project resource'):self.check()

    def test_verified_pilot_requires_all_negative_scenario_outcomes(self):
        self.complete_pilots()
        receipts=self.matrix['disposable_validation_repositories'][0]['validation_evidence']
        original=copy.deepcopy(receipts['scenario_receipts'])
        for scenario in validator.PILOT_SCENARIOS:
            receipts['scenario_receipts']=copy.deepcopy(original)
            del receipts['scenario_receipts'][scenario]
            with self.subTest(missing=scenario),self.assertRaisesRegex(ValueError,'every mandatory negative'):self.check()
        for field,value in [('outcome','gate_passed'),('mode','assumed'),('deployment_sha','b'*40),('evidence_url',None)]:
            receipts['scenario_receipts']=copy.deepcopy(original)
            receipts['scenario_receipts']['malformed_output'][field]=value
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()
        receipts['scenario_receipts']=original
        self.check()

    def test_scope_exception_requires_owner_repository_reason_and_timestamp(self):
        row=self.complete_rollout()
        row['health']='scope_exception';row['exception_evidence_url']='https://example.com/exception'
        with self.assertRaisesRegex(ValueError,'repository-bound explicit owner'):self.check()
        self.complete_scope_decision(row)
        self.matrix['summary']['verified_rollouts']-=1
        self.check()
        original=copy.deepcopy(row['scope_exception_decision'])
        for field,value in [('approved_by','other'),('repository_id',42),('repository','hemsoft-dev/other'),
                            ('reason',''),('disposition','skip'),('approved_at','2026-10-06T20:00:00'),
                            ('evidence_url','https://example.com/unrelated')]:
            row['scope_exception_decision']=copy.deepcopy(original);row['scope_exception_decision'][field]=value
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()

    def test_not_installed_requires_absence_and_preserves_known_manifests(self):
        row=self.complete_rollout(); self.check()
        original=copy.deepcopy(row['pre_sync_installation'])
        for field,value in [('repository_id',1),('repository','hemsoft-dev/other'),('revision_sha','main'),('state','present'),('manifest_paths',[])]:
            with self.subTest(field=field):
                row['pre_sync_installation']=copy.deepcopy(original);row['pre_sync_installation'][field]=value
                with self.assertRaises(ValueError):self.check()
        row['pre_sync_installation']=original
        for source in ('HemSoft/hs-buddy','HemSoft/buddy-ios'):
            inventory=next(r for r in self.inventory['repositories'] if r['full_name']==source)
            candidate=next(r for r in self.matrix['repositories'] if r['source']==source)
            replacement=copy.deepcopy(row)
            for field in ('repository_id','source','destination','visibility','archived','source_app_access_in_baseline','rollout_action'):
                replacement[field]=candidate[field]
            replacement.pop('post_transfer_access',None)
            replacement['destination_sfl_app_access'].update(repository_id=inventory['id'],repository=inventory['destination'])
            replacement['destination_protections']=copy.deepcopy(candidate['destination_protections'])
            replacement['pre_sync_installation'].update(repository_id=inventory['id'],repository=inventory['destination'])
            index=self.matrix['repositories'].index(candidate);self.matrix['repositories'][index]=replacement
            with self.assertRaisesRegex(ValueError,'cannot erase a captured'):self.check()
            self.matrix['repositories'][index]=candidate

    def test_present_installation_cannot_downgrade_a_captured_manifest(self):
        row = self.complete_rollout()
        target = next(r for r in self.matrix['repositories'] if r['source'] == 'HemSoft/hs-buddy')
        replacement = copy.deepcopy(row)
        for field in ('repository_id', 'source', 'destination', 'visibility', 'archived',
                      'source_app_access_in_baseline', 'rollout_action', 'destination_protections'):
            replacement[field] = target[field]
        replacement.pop('post_transfer_access', None)
        replacement['destination_sfl_app_access'].update(repository_id=target['repository_id'],repository=target['destination'])
        replacement.update(installed_tier='reviewer', selected_tier='reviewer', installed_components=[])
        replacement['review_pr_url']='https://github.com/'+target['destination']+'/pull/1'
        replacement['review_artifact_identity']['review_pr_url']=replacement['review_pr_url']
        replacement['pre_sync_installation'].update(repository_id=target['repository_id'],repository=target['destination'],
                                                    state='present',tier='reviewer')
        index = self.matrix['repositories'].index(target)
        self.matrix['repositories'][index] = replacement
        with self.assertRaisesRegex(ValueError, 'independently captured manifest'): self.check()

    def test_wider_and_auditor_operations_bind_the_pilot_release(self):
        self.complete_pilots()
        pilot = next(p for p in self.matrix['disposable_validation_repositories'] if p['visibility']=='private')
        receipts = pilot['validation_evidence']
        original = copy.deepcopy(receipts)
        for field, value in [('repository_id',42),('repository','hemsoft-dev/other'),
                             ('deployment_sha','d'*40),('release_version','2.1.0-rc.13'),('evidence_url',None)]:
            for key in ('wider_operation_receipts','auditor_operation_receipt'):
                pilot['validation_evidence']=copy.deepcopy(original)
                operation=pilot['validation_evidence'][key]
                if isinstance(operation,list): operation=operation[0]
                operation[field]=value
                with self.subTest(key=key,field=field),self.assertRaises(ValueError):self.check()
        pilot['validation_evidence']=copy.deepcopy(original)
        pilot['validation_evidence']['wider_workflow_run_urls']=['https://github.com/hemsoft-dev/other/actions/runs/1']
        with self.assertRaises(ValueError):self.check()

    def test_transfer_gates_require_owned_app_credential_workflow_proof(self):
        self.complete_transfer_gates()
        original=copy.deepcopy(self.matrix['pre_transfer_credential_verification'])
        for field,value in [('reviewed_sha','main'),('workflow','other.yml'),('conclusion','failure'),
                            ('app_id',1144995),('owner','other'),('client_id','wrong'),('installation_id',0),
                            ('permission_ceiling_verified',False),('run_url','https://github.com/other/repo/actions/runs/1')]:
            self.matrix['pre_transfer_credential_verification']=copy.deepcopy(original)
            self.matrix['pre_transfer_credential_verification'][field]=value
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()
        self.matrix['pre_transfer_credential_verification']=None
        with self.assertRaises(ValueError):self.check()

    def test_unverified_trees_need_independent_repository_bound_resolution(self):
        self.complete_transfer_gates()
        path=DIRECTORY/'source-tree-recheck-evidence.json'
        original=json.loads(path.read_text())
        original_read=pathlib.Path.read_text
        for field,value in [('repository_id',42),('source','HemSoft/other'),('state','assumed'),('tree_sha','a'*40)]:
            capture=copy.deepcopy(original);capture['records'][0][field]=value
            def read(file,*args,**kwargs):
                return json.dumps(capture) if file.name==path.name else original_read(file,*args,**kwargs)
            with self.subTest(field=field),patch.object(pathlib.Path,'read_text',read),self.assertRaises(ValueError):self.check()

    def test_completed_transfer_preserves_unrelated_effective_protections(self):
        self.complete_transfer_gates()
        repo=next(r for r in self.inventory['repositories'] if r['full_name']=='HemSoft/dashboard')
        proof=dict(validator.protection_contract(repo), repository_id=repo['id'],repository=repo['destination'],
                   revision_sha='d'*40,observed_at='2026-10-07T02:00:00Z')
        proof['evidence_url']=self.capture(dict(proof,phase='post_transfer'))
        validator.validate_protection_preservation(proof,DIRECTORY,repo)
        original=copy.deepcopy(proof)
        for field,value in [('repository_id',42),('repository','HemSoft/dashboard'),('rulesets',[]),('revision_sha','main')]:
            proof=copy.deepcopy(original);proof[field]=value
            with self.subTest(field=field),self.assertRaises(ValueError):
                validator.validate_protection_preservation(proof,DIRECTORY,repo)
        proof=copy.deepcopy(original);proof['rulesets'][0]['rules'][0]['parameters']['required_status_checks']=[]
        proof['evidence_url']=self.capture(dict(proof,phase='post_transfer'))
        with self.assertRaisesRegex(ValueError,'every unrelated baseline ruleset'):
            validator.validate_protection_preservation(proof,DIRECTORY,repo)

    def test_final_inventory_reconciles_locations_state_and_additions(self):
        expected={r['id']:r for r in self.inventory['repositories']}
        retained={r['repository_id'] for r in self.scope['retained_repositories']}
        accounts={owner:{'owner':owner,'state':'observed','all_pages':True,'repositories':[]} for owner in ('HemSoft','fhemmer','hemsoft-dev')}
        for repo_id,repo in expected.items():
            name=repo['full_name'] if repo_id in retained else repo['destination']
            accounts[name.split('/')[0]]['repositories'].append({'id':repo_id,'full_name':name,'private':repo['private'],'archived':repo['archived']})
        for repo_id,(name,visibility) in validator.APPROVED_PILOTS.items():
            accounts['hemsoft-dev']['repositories'].append({'id':repo_id,'full_name':name,'private':visibility=='private','archived':False})
        capture={'observed_at':'2026-10-07T03:00:00Z','accounts':list(accounts.values())}
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);path=directory/'final-inventory.json'
            proof={'observed_at':capture['observed_at'],'evidence_url':path.name,'additional_repositories':[]}
            path.write_text(json.dumps(capture));validator.validate_final_inventory(proof,directory,expected,retained)
            original=copy.deepcopy(capture)
            for field,value in [('full_name','HemSoft/.github'),('private',True),('id',1)]:
                capture=copy.deepcopy(original);capture['accounts'][2]['repositories'][1][field]=value
                path.write_text(json.dumps(capture))
                with self.subTest(field=field),self.assertRaises(ValueError):
                    validator.validate_final_inventory(proof,directory,expected,retained)
            capture=copy.deepcopy(original);capture['accounts'][2]['all_pages']=False;path.write_text(json.dumps(capture))
            with self.assertRaises(ValueError):validator.validate_final_inventory(proof,directory,expected,retained)

    def test_source_and_consumer_app_coverage_require_bound_installation(self):
        row=self.complete_rollout();self.check();original=copy.deepcopy(row['destination_sfl_app_access'])
        for value in ['verified',{'status':'verified'},{**original,'repository_id':1},{**original,'repository':'hemsoft-dev/other'},{**original,'installation_id':0}]:
            row['destination_sfl_app_access']=value
            with self.assertRaisesRegex(ValueError,'repository-bound'):self.check()
        row['destination_sfl_app_access']=original
        source=self.complete_source();source['destination_sfl_app_access']='verified'
        with self.assertRaisesRegex(ValueError,'repository-bound'):self.check()

    def test_pilot_operations_bind_repository_release_and_deployment(self):
        self.complete_pilots();self.check();receipts=self.matrix['disposable_validation_repositories'][0]['validation_evidence']
        original=copy.deepcopy(receipts['operation_receipts'])
        for field in original:
            receipts['operation_receipts']=copy.deepcopy(original)
            receipts['operation_receipts'][field]['deployment_sha']='d'*40
            with self.subTest(field=field):
                with self.assertRaisesRegex(ValueError,'operation receipt'):self.check()
        receipts['operation_receipts']=original
        receipts['init_pr_url']='https://github.com/hemsoft-dev/other/pull/1'
        receipts['operation_receipts']['init_pr_url']['evidence_url']=receipts['init_pr_url']
        with self.assertRaisesRegex(ValueError,'designated repository'):self.check()

    def test_known_pages_and_supabase_resources_cannot_disappear(self):
        original=copy.deepcopy(self.rows)
        for kind in ('github_pages','supabase_project'):
            self.rows=[r for r in original if r.get('resource_kind')!=kind]
            with self.subTest(kind=kind):
                with self.assertRaisesRegex(ValueError,'every captured Pages/Supabase'):self.check()
        self.rows=original;row=next(r for r in self.rows if r.get('resource_kind')=='supabase_project');row['provider']='none'
        with self.assertRaisesRegex(ValueError,'provider absence'):self.check()

    def test_provider_absence_requires_the_approved_owner_scope(self):
        row = self.complete_provider()
        row.update(provider='none', absence_reason='No other resources', evidence_url='https://example.com',
                   verified_at='2026-10-07T02:00:00Z')
        with self.assertRaisesRegex(ValueError, 'approved owner receipt'):
            self.check()
        row['evidence_url'] = validator.EXTERNAL_SCOPE_RECEIPT
        self.check()
        row['verified_at'] = '2026-10-06T20:00:00-04:00'
        with self.assertRaisesRegex(ValueError, 'approved owner receipt'):
            self.check()

    def test_cloudflare_zone_and_worker_cannot_be_omitted(self):
        original = copy.deepcopy(self.rows)
        for kind in ('cloudflare_zone', 'cloudflare_worker'):
            self.rows = [r for r in original if r.get('resource_kind') != kind]
            with self.subTest(kind=kind), self.assertRaisesRegex(ValueError, 'every captured Pages/Supabase/Cloudflare'):
                self.check()
        self.rows = original
        row = next(r for r in self.rows if r.get('resource_kind') == 'cloudflare_worker')
        row['provider'] = 'none'
        with self.assertRaisesRegex(ValueError, 'provider absence'):
            self.check()

    def test_consumer_wider_receipts_bind_repository_release_and_sha(self):
        row = self.complete_rollout()
        row.update(selected_tier='full', wider_workflow_run_urls=['https://example.com/unrelated'])
        row['manifest_identity']['tier'] = 'full'
        self.bind_manifest(row)
        with self.assertRaisesRegex(ValueError, 'bound operation receipts'):
            self.check()
        self.bind_consumer_runs(row)
        self.check()
        original = copy.deepcopy(row['wider_operation_receipts'])
        for field, value in [('repository_id', 1), ('repository', 'hemsoft-dev/other'),
                             ('deployment_sha', 'd'*40), ('release_version', '2.0.0')]:
            row['wider_operation_receipts'] = copy.deepcopy(original)
            row['wider_operation_receipts'][0][field] = value
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, 'Operation receipt'):
                self.check()
        row['wider_operation_receipts'] = original
        row['wider_workflow_run_urls'] = ['https://github.com/hemsoft-dev/other/actions/runs/1']
        row['wider_operation_receipts'][0]['evidence_url'] = row['wider_workflow_run_urls'][0]
        with self.assertRaisesRegex(ValueError, 'designated repository'):
            self.check()

    def test_final_addition_needs_an_independent_dated_owner_decision(self):
        accounts = [{'owner':owner, 'state':'observed', 'all_pages':True, 'repositories':[]}
                    for owner in ('HemSoft', 'fhemmer', 'hemsoft-dev')]
        for repo_id, (name, visibility) in validator.APPROVED_PILOTS.items():
            accounts[2]['repositories'].append({'id':repo_id, 'full_name':name, 'private':visibility=='private'})
        accounts[2]['repositories'].append({'id':42, 'full_name':'hemsoft-dev/addition', 'private':True})
        capture = {'observed_at':'2026-10-07T03:00:00Z', 'accounts':accounts}
        extra = {'repository_id':42, 'repository':'hemsoft-dev/addition', 'visibility':'private',
                 'approved_by':'HemSoft', 'evidence_url':'https://example.com/approval'}
        with tempfile.TemporaryDirectory() as folder:
            directory = pathlib.Path(folder)
            (directory/'capture.json').write_text(json.dumps(capture))
            proof = {'observed_at':capture['observed_at'], 'evidence_url':'capture.json',
                     'additional_repositories':[extra]}
            with self.assertRaisesRegex(ValueError, 'Missing evidence reference'):
                validator.validate_final_inventory(proof, directory, {}, {})
            extra.update(decision_artifact='decision.json', evidence_url=
                'https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-123')
            decision = dict(extra, approved_at='2026-10-07T02:00:00Z',
                            reason='Synthetic owner-approved fixture', disposition='include_final_inventory')
            (directory/'decision.json').write_text(json.dumps(decision))
            validator.validate_final_inventory(proof, directory, {}, {})
            for field, value in [('repository_id',1), ('visibility','public'), ('reason',''),
                                 ('approved_at','2026-10-08T02:00:00Z'), ('disposition','ignore')]:
                changed = dict(decision); changed[field] = value
                (directory/'decision.json').write_text(json.dumps(changed))
                with self.subTest(field=field), self.assertRaises(ValueError):
                    validator.validate_final_inventory(proof, directory, {}, {})

    def test_every_unlinked_supabase_project_requires_account_accounting(self):
        original = copy.deepcopy(self.matrix['account_resource_preservation'])
        for resources in ([], [dict(original[0], resource_id='wrong')],
                          [dict(original[0], resource_owner='wrong')],
                          [dict(original[0], association='guessed_dashboard_link')]):
            self.matrix['account_resource_preservation'] = resources
            with self.subTest(resources=resources), self.assertRaisesRegex(ValueError, 'Supabase'):
                self.check()
        self.matrix['account_resource_preservation'] = original
        self.check()

    def test_retained_app_dependencies_require_approved_safe_disposition(self):
        retained = next(r for r in self.matrix['repositories'] if r['health']=='retained_source')
        original = copy.deepcopy(retained['retained_app_dependency'])
        for field, value in [('disposition','site depends on the App and will break'),
                             ('evidence_url','https://example.com/unrelated'), ('verified_by','other')]:
            retained['retained_app_dependency'] = dict(original)
            retained['retained_app_dependency'][field] = value
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, 'approved no-dependency'):
                self.check()

    def test_source_workflows_require_repository_release_sha_receipts(self):
        row = self.complete_source()
        proof = row['in_place_evidence']
        operations = proof.pop('workflow_operation_receipts')
        with self.assertRaisesRegex(ValueError, 'bound workflow operation receipts'):
            self.check()
        proof['workflow_operation_receipts'] = operations
        self.check()
        for field, value in [('repository','hemsoft-dev/other'), ('deployment_sha','e'*40),
                             ('release_version','2.0.0')]:
            proof['workflow_operation_receipts'] = copy.deepcopy(operations)
            proof['workflow_operation_receipts'][0][field] = value
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, 'Operation receipt'):
                self.check()

    def test_review_gate_receipts_require_repository_head_base_binding(self):
        row = self.complete_rollout()
        original = copy.deepcopy(row['review_operation_receipts'])
        for key in original:
            for field, value in [('repository','hemsoft-dev/other'), ('head_sha','d'*40), ('base_sha','e'*40)]:
                row['review_operation_receipts'] = copy.deepcopy(original)
                row['review_operation_receipts'][key][field] = value
                with self.subTest(key=key,field=field), self.assertRaisesRegex(ValueError, 'Review operation receipt'):
                    self.check()
        row['review_operation_receipts'] = original
        row['gate_run_url']='https://example.com/unrelated'
        row['review_operation_receipts']['gate_run_url']['evidence_url']=row['gate_run_url']
        with self.assertRaisesRegex(ValueError, 'designated repository'):
            self.check()

    def test_unverified_fhemmer_rules_need_the_independent_owner_capture(self):
        repo = next(r for r in self.inventory['repositories'] if r['id']==validator.FHEMMER_REPOSITORY_ID)
        capture_path = DIRECTORY/'fhemmer-protection-evidence.json'
        original = json.loads(capture_path.read_text())
        original_read = pathlib.Path.read_text
        for field, value in [('repository_id',1), ('repository','other/repo'),
                             ('rulesets',{'observation':'Source has a required ruleset'})]:
            changed = dict(original); changed[field]=value
            def read(path,*args,**kwargs):
                return json.dumps(changed) if path.name==capture_path.name else original_read(path,*args,**kwargs)
            with self.subTest(field=field), patch.object(pathlib.Path,'read_text',read), self.assertRaisesRegex(ValueError,'independent owner capture'):
                validator.protection_contract(repo)

    def test_yahtzee_completion_requires_destination_runner_smoke(self):
        self.complete_transfer_gates(); self.complete_app_transfer()
        row = next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/yahtzee')
        row.update(health='scope_exception',exception_evidence_url='https://example.com/exception',
                   transfer_evidence_url='https://example.com/transfer',status_evidence_url='https://example.com/status')
        self.complete_scope_decision(row)
        original = row.pop('post_transfer_runner')
        with self.assertRaisesRegex(ValueError,'structured destination continuity'):
            self.check()
        row['post_transfer_runner'] = original
        self.check()
        for field, value in [('runner_id',22),('repository','HemSoft/yahtzee'),('online',False),
                             ('isolated',False),('service_active',False),('run_conclusion','failure')]:
            row['post_transfer_runner']=dict(original);row['post_transfer_runner'][field]=value
            with self.subTest(field=field), self.assertRaisesRegex(ValueError,'structured destination continuity'):
                self.check()
        row['post_transfer_runner']=dict(original,run_url='https://github.com/HemSoft/yahtzee/actions/runs/1')
        with self.assertRaisesRegex(ValueError,'destination repository'):
            self.check()

    def onboarding_fixture(self, directory):
        row=self.complete_rollout();name='hemsoft-dev/sfl-migration-new-repository'
        proof=copy.deepcopy(row)
        proof.update(status='verified',repository_id=42,repository=name,destination=name,visibility='private',
                     rollout_completed_at='2026-10-07T02:00:00Z',release_version=row['manifest_version'])
        proof['review_pr_url']='https://github.com/'+name+'/pull/2'
        proof['review_artifact_identity'].update(review_pr_url=proof['review_pr_url'])
        proof['destination_sfl_app_access'].update(repository_id=42,repository=name)
        proof['destination_codex_access']=dict(proof['destination_sfl_app_access'],app_id=1144995,installation_id=168678981)
        self.bind_review_operations(proof,directory=directory)
        proof['gate_policy']=self.gate_policy(42,name,directory)
        (directory/'owned-app-organization-installation-evidence.json').write_text((DIRECTORY/'owned-app-organization-installation-evidence.json').read_text())
        (directory/'codex-organization-installation-evidence.json').write_text((DIRECTORY/'codex-organization-installation-evidence.json').read_text())
        fields=('init_pr_url','repeat_onboarding_url','sync_pr_url','repeat_sync_url','status_url')
        proof['onboarding_operation_receipts']={}
        for field in fields:
            proof[field]='https://github.com/'+name+'/issues/1#'+field
            proof['onboarding_operation_receipts'][field]={'repository_id':42,'repository':name,
                'deployment_sha':proof['deployment_sha'],'release_version':proof['release_version'],'evidence_url':proof[field]}
        proof['init_pr_url']='https://github.com/'+name+'/pull/3'
        for field,operation in proof['onboarding_operation_receipts'].items():
            command='init' if field in {'init_pr_url','repeat_onboarding_url'} else 'status' if field=='status_url' else 'sync'
            operation.update(command=command,outcome='pull_request_merged' if field=='init_pr_url' else 'healthy' if field=='status_url' else 'no_changes',revision_before='f'*40,revision_after='f'*40,change_count=0,merged=field=='init_pr_url',evidence_url=proof[field])
        proof['manifest_evidence_url']='post-status-manifest.json'
        (directory/proof['manifest_evidence_url']).write_text(json.dumps({'repository_id':42,'repository':name,'revision_sha':'f'*40,'manifest':proof['manifest_identity'],'observed_at':'2026-10-07T03:30:00Z'}))
        capture={'metadata':{'id':42,'full_name':name,'private':True,'archived':False,'created_at':'2026-10-07T03:00:00Z','default_branch':'main'}}
        (directory/'metadata.json').write_text(json.dumps(capture));proof['metadata_evidence_url']='metadata.json'
        completion={'completed_at':proof['rollout_completed_at'],'organization':'hemsoft-dev',
                    'deployment_sha':proof['deployment_sha'],'release_version':proof['release_version'],'repositories':[]}
        (directory/'completion.json').write_text(json.dumps(completion));proof['rollout_completion_evidence_url']='completion.json'
        proof['release_download_verification_url']=self.capture({'release_url':proof['release_url'],
            'source_repository':proof['deployment_source'],'source_repository_id':1169772257,
            'source_sha':proof['deployment_sha'],'release_version':proof['release_version'],
            'target_repository_id':42,'target_repository':name,'asset_name':'gh-sfl_linux_amd64',
            'asset_url':'https://github.com/'+proof['deployment_source']+'/releases/download/v'+proof['release_version']+'/gh-sfl_linux_amd64',
            'expected_sha256':'a'*64,'actual_sha256':'a'*64,'checksum_verified':True,'attestation_verified':True,
            'observed_at':'2026-10-07T03:30:00Z'},directory)
        return proof

    def test_post_rollout_onboarding_requires_distinct_dynamic_app_coverage(self):
        with self.assertRaisesRegex(ValueError,'separate post-rollout'):
            validator.validate_final_onboarding(None,DIRECTORY,{},'hemsoft-dev',4448946)
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)
            for field,value in [('repository_id',1408025382),('destination_codex_access',{'status':'verified'}),
                                ('rollout_completed_at','2026-10-08T02:00:00Z'),('onboarding_operation_receipts',{})]:
                changed=copy.deepcopy(proof);changed[field]=value
                with self.subTest(field=field),self.assertRaises(ValueError):
                    validator.validate_final_onboarding(changed,directory,{},'hemsoft-dev',4448946)

    def test_terminal_inventory_cannot_omit_the_post_rollout_new_repository(self):
        self.complete_source()
        for row in self.matrix['repositories']:
            if row['health'] in {'retained_source','source_verified'}:
                continue
            row.update(health='archived_verified' if row['archived'] else 'scope_exception',
                       transfer_evidence_url='https://example.com/transfer',status_evidence_url='https://example.com/status')
            self.complete_post_transfer_access(row)
            self.complete_app_coverage(row)
            if not row['archived']:
                row['exception_evidence_url']='https://example.com/exception'
                self.complete_scope_decision(row)
        accounts={owner:{'owner':owner,'state':'observed','all_pages':True,'repositories':[]}
                  for owner in ('HemSoft','fhemmer','hemsoft-dev')}
        for repo in self.inventory['repositories']:
            name=repo['full_name'] if repo['id'] in validator.APPROVED_RETAINED_IDS else repo['destination']
            accounts[name.split('/')[0]]['repositories'].append({'id':repo['id'],'full_name':name,
                'private':repo['private'],'archived':repo['archived']})
        for repo_id,(name,visibility) in validator.APPROVED_PILOTS.items():
            accounts['hemsoft-dev']['repositories'].append({'id':repo_id,'full_name':name,'private':visibility=='private'})
        with tempfile.TemporaryDirectory(dir=DIRECTORY) as folder:
            directory=pathlib.Path(folder)
            capture={'observed_at':'2026-10-07T03:00:00Z','accounts':list(accounts.values())}
            path=directory/'final.json';path.write_text(json.dumps(capture))
            self.matrix['final_inventory']={'observed_at':capture['observed_at'],
                'evidence_url':str(path.relative_to(DIRECTORY)),'additional_repositories':[]}
            resource=self.matrix['account_resource_preservation'][0]
            path=directory/'supabase.json';path.write_text(json.dumps({'resource_id':resource['resource_id'],
                'resource_owner':resource['resource_owner'],'state':'paused','resource_changes_made':False,
                'operation':'read_only_preservation'}))
            resource.update(status='verified',post_transfer_evidence_url=str(path.relative_to(DIRECTORY)))
            with self.assertRaisesRegex(ValueError,'separate post-rollout new-repository'):
                self.check()


    def test_effective_gate_capture_rejects_optional_other_repository_and_unbound_app(self):
        row=self.complete_rollout();policy=row['gate_policy'];path=DIRECTORY/policy['evidence_url']
        baseline=json.loads(path.read_text());self.check()
        for field,value in [('repository_id',42),('repository','hemsoft-dev/other'),('branch','other')]:
            changed=copy.deepcopy(baseline);changed[field]=value;path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'capture must match'):self.check()
        for checks in ([],[{'context':'SFL Reviewer Gate Runner','integration_id':42}]):
            changed=copy.deepcopy(baseline)
            changed['effective_rules']['data'][0]['parameters']['required_status_checks']=checks
            path.write_text(json.dumps(changed))
            with self.subTest(checks=checks),self.assertRaisesRegex(ValueError,'actually require'):self.check()
        changed=copy.deepcopy(baseline);changed['effective_rules']['data']=[]
        changed['classic_protection']={'state':'observed','data':{'required_status_checks':{'strict':True,
            'checks':[{'context':'SFL Reviewer Gate Runner','app_id':15368}]}}}
        path.write_text(json.dumps(changed));self.check()

    def test_public_pilot_cannot_reuse_private_scenario_receipts(self):
        self.complete_pilots();private,public=self.matrix['disposable_validation_repositories']
        if private['visibility']!='private':private,public=public,private
        public['validation_evidence']['scenario_receipts']=copy.deepcopy(private['validation_evidence']['scenario_receipts'])
        with self.assertRaisesRegex(ValueError,'Pilot scenario'):self.check()

    def test_new_repository_cannot_precede_independent_rollout_completion(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            metadata=json.loads((directory/'metadata.json').read_text())
            metadata['metadata']['created_at']='2026-10-07T01:30:00Z'
            (directory/'metadata.json').write_text(json.dumps(metadata))
            proof['rollout_completed_at']='2026-10-07T01:00:00Z'
            with self.assertRaisesRegex(ValueError,'independently captured deployment'):
                validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)

    def test_newly_observed_manifest_cannot_drop_addons_or_custom_components(self):
        row=self.complete_rollout();row['installed_tier']='reviewer'
        row['pre_sync_installation'].update(state='present',tier='reviewer',addons=['pr-review'])
        with self.assertRaisesRegex(ValueError,'pre-sync receipt'):self.check()
        row['pre_sync_installation']['addons']=[]
        row['pre_sync_installation']['components']=['sfl-auditor']
        with self.assertRaisesRegex(ValueError,'pre-sync receipt'):self.check()

    def test_pilot_manifest_must_install_review_observer(self):
        self.complete_pilots();public=next(p for p in self.matrix['disposable_validation_repositories'] if p['visibility']=='public')
        manifest=public['validation_evidence']['manifest_identity']
        for tier in ('minimal','standard'):
            manifest.update(tier=tier,addons=[])
            with self.subTest(tier=tier),self.assertRaisesRegex(ValueError,'actually deploy the review observer'):self.check()
            manifest['addons']=['pr-review']
            self.bind_manifest(public['validation_evidence'],repository_id=public['repository_id'],repository=public['repository'])
            self.check()

    def test_failed_or_unrelated_workflow_cannot_complete_source_consumer_and_auditor(self):
        row=self.complete_rollout();row['selected_tier']='full';row['manifest_identity']['tier']='full'
        row['wider_workflow_run_urls']=['https://example.com/run'];self.bind_consumer_runs(row)
        operation=row['wider_operation_receipts'][0];baseline=copy.deepcopy(operation);self.check()
        for field,value in [('conclusion','failure'),('conclusion','cancelled'),('workflow','.github/workflows/unrelated.yml'),('run_head_sha',None)]:
            operation.clear();operation.update(baseline);operation[field]=value
            with self.subTest(field=field,value=value),self.assertRaisesRegex(ValueError,'successful expected'):self.check()
        operation.clear();operation.update(baseline)
        private=next(p for p in self.matrix['disposable_validation_repositories'] if p['visibility']=='private')
        private['validation_evidence']['auditor_operation_receipt']['workflow']='.github/workflows/sfl-dispatcher.yml'
        with self.assertRaisesRegex(ValueError,'successful expected'):self.check()

    def test_new_onboarding_codex_coverage_must_use_actual_org_installation(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            proof['destination_codex_access']['installation_id']=42
            with self.assertRaisesRegex(ValueError,'captured organization installation'):
                validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)

    def test_archived_baseline_app_coverage_cannot_remain_pending(self):
        self.complete_transfer_gates();self.complete_app_transfer()
        row=next(r for r in self.matrix['repositories'] if r['archived'] and r['source_app_access_in_baseline'])
        row.update(health='archived_verified',transfer_evidence_url='https://example.com/transfer',status_evidence_url='https://example.com/status')
        self.complete_app_coverage(row);self.check()
        row['destination_sfl_app_access']['status']='pending'
        with self.assertRaisesRegex(ValueError,'baseline-covered terminal'):self.check()


    def test_post_rollout_onboarding_requires_manifest_and_idempotent_operation_results(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)
            for field in ('manifest_identity','manifest_evidence_url','release_download_verification_url','release_url'):
                changed=copy.deepcopy(proof);changed.pop(field)
                with self.subTest(field=field),self.assertRaises(ValueError):
                    validator.validate_final_onboarding(changed,directory,{},'hemsoft-dev',4448946)
            for field in ('repeat_onboarding_url','repeat_sync_url','sync_pr_url'):
                changed=copy.deepcopy(proof);changed['onboarding_operation_receipts'][field]['change_count']=1
                with self.subTest(field=field),self.assertRaisesRegex(ValueError,'zero changes'):
                    validator.validate_final_onboarding(changed,directory,{},'hemsoft-dev',4448946)
            changed=copy.deepcopy(proof);changed['init_pr_url']='https://github.com/'+proof['repository']+'/issues/1'
            changed['onboarding_operation_receipts']['init_pr_url']['evidence_url']=changed['init_pr_url']
            with self.assertRaisesRegex(ValueError,'actual merged deployment PR'):
                validator.validate_final_onboarding(changed,directory,{},'hemsoft-dev',4448946)
            capture=json.loads((directory/'post-status-manifest.json').read_text());capture['manifest']['sourceSha']='c'*40
            (directory/'post-status-manifest.json').write_text(json.dumps(capture))
            with self.assertRaisesRegex(ValueError,'manifest capture must match'):
                validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)

    def test_sfl_coverage_matches_one_saved_destination_installation(self):
        row=self.complete_rollout();self.check();baseline=copy.deepcopy(row['destination_sfl_app_access'])
        row['destination_sfl_app_access']['installation_id']=42
        with self.assertRaisesRegex(ValueError,'captured all-repositories destination'):self.check()
        row['destination_sfl_app_access']=baseline
        path=DIRECTORY/'owned-app-organization-installation-evidence.json';capture=json.loads(path.read_text())
        for field,value in [('id',42),('repository_selection','selected')]:
            changed=copy.deepcopy(capture);changed['installation'][field]=value;path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'captured all-repositories destination'):self.check()
        path.write_text(json.dumps(capture));self.check()

    def test_source_all_repository_selection_must_be_saved_and_verified(self):
        self.complete_transfer_gates();proof=self.matrix['pre_transfer_credential_verification']
        for selection in (None,'selected'):
            proof['repository_selection']=selection
            with self.subTest(selection=selection),self.assertRaisesRegex(ValueError,'owned-App credential'):self.check()
        proof['repository_selection']='all';self.check()

    def test_known_provider_actions_cannot_contradict_owner_dispositions(self):
        self.complete_transfer_gates();self.check()
        for resource_id,action in [('cevpnetigzotgstxxjpm','Resume the database'),
                                  ('cevpnetigzotgstxxjpm','Delete the database'),
                                  ('prj_hPjAbxtMlCi3A5waKxQpjATto0ae','Reconnect Git after transfer')]:
            row=next(r for r in self.rows if r['resource_id']==resource_id)
            original=row['transfer_action'];row['transfer_action']=action
            with self.subTest(action=action),self.assertRaisesRegex(ValueError,'approved.*disposition'):self.check()
            row['transfer_action']=original

    def test_one_auditor_run_cannot_count_as_wider_workflow_proof(self):
        self.complete_pilots();self.check()
        pilot=next(p for p in self.matrix['disposable_validation_repositories'] if p['visibility']=='private')
        receipts=pilot['validation_evidence']
        receipts['wider_workflow_run_urls']=[receipts['auditor_run_url']]
        receipts['wider_operation_receipts']=[copy.deepcopy(receipts['auditor_operation_receipt'])]
        with self.assertRaisesRegex(ValueError,'distinct successful non-Auditor'):self.check()


    def test_workflow_run_heads_must_match_the_captured_deployment_revision(self):
        row=self.complete_source();operation=row['in_place_evidence']['workflow_operation_receipts'][0]
        operation['run_head_sha']='f'*40
        with self.assertRaisesRegex(ValueError,'successful expected'):self.check()
        operation['run_head_sha']=row['in_place_evidence']['source_sha'];self.check()
        private=next(p for p in self.matrix['disposable_validation_repositories'] if p['visibility']=='private')
        receipts=private['validation_evidence'];receipts['auditor_operation_receipt']['run_head_sha']='f'*40
        with self.assertRaisesRegex(ValueError,'successful expected'):self.check()
        receipts['auditor_operation_receipt']['run_head_sha']='b'*40
        consumer=self.complete_rollout();consumer['selected_tier']='full';consumer['manifest_identity']['tier']='full'
        consumer['wider_workflow_run_urls']=['https://example.com/run'];self.bind_consumer_runs(consumer)
        consumer['wider_operation_receipts'][0]['run_head_sha']='f'*40
        with self.assertRaisesRegex(ValueError,'successful expected'):self.check()

    def test_pilot_operations_need_terminal_results_and_an_ordered_safe_revision_chain(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0]
        receipts=pilot['validation_evidence'];original=copy.deepcopy(receipts['operation_receipts'])
        changes=[('init_pr_url','merged',False),('sync_pr_url','outcome','open'),
                 ('repeat_sync_evidence_url','change_count',1),('repeat_onboarding_evidence_url','outcome','failed'),
                 ('status_evidence_url','outcome','unhealthy'),('gate_uninstall_evidence_url','gate_only',False),
                 ('gate_uninstall_evidence_url','unrelated_change_count',1),('sync_pr_url','revision_before','d'*40)]
        for field,key,value in changes:
            receipts['operation_receipts']=copy.deepcopy(original);receipts['operation_receipts'][field][key]=value
            with self.subTest(field=field,key=key),self.assertRaises(ValueError):self.check()
        receipts['operation_receipts']=original;self.check()

    def test_registered_review_requires_an_actual_successful_gate_and_matching_capture(self):
        row=self.complete_rollout();original=copy.deepcopy(row);gate=row['review_operation_receipts']['gate_run_url']
        for field,value in [('status','queued'),('conclusion','cancelled'),('workflow','.github/workflows/unrelated.yml'),('app_id',1)]:
            row.clear();row.update(copy.deepcopy(original));row['review_operation_receipts']['gate_run_url'][field]=value
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'successful terminal'):self.check()
        row.clear();row.update(copy.deepcopy(original));row['gate_run_url']='https://github.com/'+row['destination']+'/issues/1'
        row['review_operation_receipts']['gate_run_url']['evidence_url']=row['gate_run_url']
        with self.assertRaisesRegex(ValueError,'actual repository Actions'):self.check()
        row.clear();row.update(original);gate=row['review_operation_receipts']['gate_run_url']
        capture=json.loads((DIRECTORY/gate['capture_evidence_url']).read_text());capture['artifact_identity']['reviewed_head_sha']='f'*40
        gate['capture_evidence_url']=self.capture(capture)
        with self.assertRaisesRegex(ValueError,'match the completed gate'):self.check()

    def test_new_onboarding_checksum_capture_must_verify_its_canonical_asset(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            original=json.loads((directory/proof['release_download_verification_url']).read_text())
            for field,value in [('source_sha','f'*40),('release_version','2.0.0'),('actual_sha256','b'*64),
                                ('attestation_verified',False),('asset_url','https://example.com/download'),('target_repository_id',43)]:
                capture=copy.deepcopy(original);capture[field]=value
                proof['release_download_verification_url']=self.capture(capture,directory)
                with self.subTest(field=field),self.assertRaisesRegex(ValueError,'canonical asset'):
                    validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)
            proof['release_download_verification_url']='https://example.com/checksum'
            with self.assertRaisesRegex(ValueError,'independent local capture'):
                validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)

    def test_scope_exception_requires_a_separate_captured_owner_decision(self):
        row=self.complete_rollout();row.update(health='scope_exception',exception_evidence_url='https://example.com/exception')
        self.matrix['summary']['verified_rollouts']-=1;self.complete_scope_decision(row);self.check()
        original=json.loads((DIRECTORY/row['exception_evidence_url']).read_text())
        for field,value in [('repository_id',42),('reason','Different reason')]:
            capture=copy.deepcopy(original);capture[field]=value
            row['exception_evidence_url']=self.capture(capture);row['scope_exception_decision']['evidence_url']=row['exception_evidence_url']
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'independent owner'):self.check()
        capture=copy.deepcopy(original);capture['owner_comment']['author']='other'
        row['exception_evidence_url']=self.capture(capture);row['scope_exception_decision']['evidence_url']=row['exception_evidence_url']
        with self.assertRaisesRegex(ValueError,'captured HemSoft issue decision'):self.check()

    def test_integration_smoke_requires_successful_non_destructive_captured_outcome(self):
        row=self.complete_provider();original=copy.deepcopy(row)
        for field,value in [('smoke_outcome','failed'),('smoke_phase','unknown'),('smoke_evidence_url','https://example.com/smoke')]:
            row.clear();row.update(copy.deepcopy(original));row[field]=value
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()
        row.clear();row.update(original);capture=json.loads((DIRECTORY/row['smoke_evidence_url']).read_text())
        for field,value in [('destructive_changes',True),('continuity_verified',False),('repository_id',42)]:
            changed=copy.deepcopy(capture);changed[field]=value;row['smoke_evidence_url']=self.capture(changed)
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()

    def test_destination_protections_require_matching_post_transfer_capture(self):
        row=self.complete_rollout();proof=row['destination_protections']
        original=json.loads((DIRECTORY/proof['evidence_url']).read_text())
        for field,value in [('phase','pre_transfer'),('revision_sha','f'*40),('repository_id',42),('observed_at','2026-10-07T02:00:00')]:
            changed=copy.deepcopy(original);changed[field]=value;proof['evidence_url']=self.capture(changed)
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()

    def test_requester_permission_requires_a_bound_independent_get_result(self):
        row=self.complete_rollout();original=json.loads((DIRECTORY/row['requester_permission_evidence_url']).read_text())
        for field,value in [('actor','other'),('repository_id',42),('pr_url','https://github.com/'+row['destination']+'/pull/2'),
                            ('head_sha','f'*40),('http_status',403),('result',{'permission':'read','role_name':'read'})]:
            changed=copy.deepcopy(original);changed[field]=value;row['requester_permission_evidence_url']=self.capture(changed)
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'permission capture'):self.check()
        changed=copy.deepcopy(original);changed['result']={'permission':'write','role_name':'maintain'}
        row['requester_permission']='maintain';row['requester_permission_evidence_url']=self.capture(changed);self.check()

    def test_strict_gate_policy_must_cover_the_actual_default_branch(self):
        for branch in ('master','develop'):
            repo=next(r for r in self.inventory['repositories'] if r['default_branch']==branch)
            policy=self.gate_policy(repo['id'],repo['destination'])
            validator.validate_gate_policy(policy,DIRECTORY,repo['id'],repo['destination'],branch)
            capture=json.loads((DIRECTORY/policy['evidence_url']).read_text());capture['branch']='main'
            policy.update(branch='main',evidence_url=self.capture(capture))
            with self.subTest(branch=branch),self.assertRaisesRegex(ValueError,'required strict'):
                validator.validate_gate_policy(policy,DIRECTORY,repo['id'],repo['destination'],branch)

    def test_every_verified_consumer_configuration_must_deploy_the_observer(self):
        row=self.complete_rollout()
        for tier in ('minimal','standard'):
            row.update(selected_tier=tier,selected_addons=[]);row['manifest_identity'].update(tier=tier,addons=[])
            row['wider_workflow_run_urls']=['https://example.com/run'];self.bind_consumer_runs(row)
            with self.subTest(tier=tier),self.assertRaisesRegex(ValueError,'actually deploy the review observer'):self.check()
            row['selected_addons']=['pr-review'];row['manifest_identity']['addons']=['pr-review'];self.bind_consumer_runs(row);self.check()


if __name__ == '__main__':
    unittest.main()
