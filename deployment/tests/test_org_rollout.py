"""Regression tests for offline ledger and rollout completion gates."""

import copy
import base64
import datetime
import csv
import importlib.util
import json
import hashlib
import io
import zipfile
import urllib.parse
import pathlib
import tempfile
import subprocess
import os
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

    def transfer_capture(self, row, timestamp='2026-10-07T01:50:30Z'):
        milliseconds=int(validator.observed_time(timestamp,'fixture').timestamp()*1000)
        return self.capture({'phase':'post_transfer','source':row['source'],
            'repository_id':row['repository_id'],'repository':row['destination'],
            'audit_log_url':'https://github.com/organizations/hemsoft-dev/settings/audit-log',
            'observed_at':'2026-10-07T01:50:45Z','event':{'action':'repo.transfer',
                '_document_id':'synthetic-transfer-'+str(row['repository_id']),
                'repo_id':row['repository_id'],'repo':row['destination'],'repo_was':row['source'],
                'org':'hemsoft-dev','org_id':338855369,'actor':'HemSoft',
                '@timestamp':milliseconds,'created_at':milliseconds}})

    def pilots_before_rollout(self):
        # Keep complete_pilots' deliberate timing fixtures, then build a coherent
        # pilot-before-consumer positive fixture when an active rollout is added.
        references=set()
        def collect(value):
            if isinstance(value,dict):
                for key,item in value.items():
                    if key.endswith(('evidence_url','verification_url')) and isinstance(item,str) and not item.startswith('https://'):
                        references.add(item)
                    elif isinstance(item,(dict,list)):collect(item)
            elif isinstance(value,list):
                for item in value:collect(item)
        for pilot in self.matrix['disposable_validation_repositories']:
            receipts=pilot['validation_evidence']
            # Registration context hashes include the request time. Generate a
            # new coherent request rather than changing its timestamp in place.
            self.bind_review_operations(receipts,pilot['repository_id'],pilot['repository'],timestamp='2026-10-07T01:53:00Z')
            for field in receipts['review_operation_receipts']:
                receipts['operation_receipts'][field]['evidence_url']=receipts[field]
            collect(receipts)
        pending=list(references);done=set()
        substitutions={'2026-10-07T02:00:00Z':'2026-10-07T01:53:00Z',
            '2026-10-07T03:30:00Z':'2026-10-07T01:54:00Z',
            '2026-10-07T03:40:00Z':'2026-10-07T01:56:00Z',
            '2026-10-07T03:41:00Z':'2026-10-07T01:57:00Z'}
        while pending:
            reference=pending.pop()
            if reference in done:continue
            done.add(reference);path=DIRECTORY/reference
            data=path.read_text()
            for old,new in substitutions.items():data=data.replace(old,new)
            path.write_text(data);collect(json.loads(data));pending.extend(references-done)
        for reference in done:
            path=DIRECTORY/reference;capture=json.loads(path.read_text())
            if 'output_evidence_url' in capture and 'output_sha256' in capture:
                capture['output_sha256']=hashlib.sha256((DIRECTORY/capture['output_evidence_url']).read_bytes()).hexdigest()
                path.write_text(json.dumps(capture))

    def branch(self, repository_id):
        return next((r['default_branch'] for r in self.inventory['repositories'] if r['id']==repository_id),'main')

    def bind_pre_sync(self, row):
        proof=row['pre_sync_installation']
        files={path:{'path':path,'revision_sha':proof['revision_sha'],'state':'absent','http_status':404}
               for path in ('.sfl/sfl.json','sfl.json')}
        if proof['state']=='present':
            files['.sfl/sfl.json'].update(state='observed',http_status=200,manifest={
                'tier':proof['tier'],'addons':proof['addons'],'components':proof['components']})
        deployed=json.loads((DIRECTORY/row['manifest_evidence_url']).read_text())
        operation=self.capture({'repository_id':row['repository_id'],'repository':row['destination'],
            'input_revision_sha':proof['revision_sha'],'result_revision_sha':deployed['revision_sha'],
            'started_at':'2026-10-07T02:00:00Z','observed_at':'2026-10-07T02:00:00Z','comparison':{'status':'ahead',
                'base_commit':{'sha':proof['revision_sha']},'merge_base_commit':{'sha':proof['revision_sha']},
                'html_url':'https://github.com/'+row['destination']+'/compare/'+proof['revision_sha']+'...'+deployed['revision_sha']}})
        proof['evidence_url']=self.capture({**proof,'phase':'pre_sync',
            'observed_at':'2026-10-07T02:00:00Z','manifest_files':files,
            'default_branch_head':{'branch':row['gate_policy']['branch'],'sha':proof['revision_sha'],'http_status':200,
                'request_url':'https://api.github.com/repos/'+row['destination']+'/git/ref/heads/'+row['gate_policy']['branch'],
                'data':{'object':{'sha':proof['revision_sha']}}},
            'deployment_input_evidence_url':operation})

    def bind_manifest(self, row, directory=DIRECTORY, repository_id=None, repository=None, revision='b'*40):
        repository_id=repository_id or row['repository_id'];repository=repository or row['destination']
        row['manifest_evidence_url']=self.capture({'repository_id':repository_id,'repository':repository,
            'revision_sha':revision,'manifest':row['manifest_identity'],'observed_at':'2026-10-07T02:00:00Z'},directory)
        if row.get('health') == 'verified':
            checks=[]
            for path in sorted(validator.deployed_workflow_paths(row['selected_tier'],row['selected_addons'],row['selected_components'])):
                name=path.rsplit('/',1)[1]
                source_path=('.github/workflows/'+name if name.endswith('.lock.yml') else
                    ('deployment/infrastructure/' if name in {'sfl-pr-review-auto.yml','sfl-dispatcher.yml','sfl-auditor.yml'}
                     else 'deployment/workflows/')+name)
                source=(ROOT/source_path).read_bytes();installed=source.decode()
                if name=='sfl-pr-review-auto.yml':
                    ref=row['deployment_source']+'/'+source_path+'@'+row['deployment_sha']
                    branch=row['gate_policy']['branch'].replace("'","''")
                    installed=installed.replace('# Source: HemSoft/set-it-free-loop/deployment/infrastructure/sfl-pr-review-auto.yml@main','# Source: '+ref,1)
                    installed=installed.replace('    branches: [main]',"    branches: ['"+branch+"']",1)
                    installed=installed.replace('  SFL_REVIEW_BASE_BRANCH: main',"  SFL_REVIEW_BASE_BRANCH: '"+branch+"'",1)
                    installed='# Deployed from: '+ref+'\n# To upgrade: re-run deploy-workflow.ps1 at the desired SHA\n'+installed
                installed=installed.replace('__SFL_VERSION__',row['manifest_version']).encode()
                checks.append({'path':path,'present':True,'expected_sha256':hashlib.sha256(installed).hexdigest(),
                    'actual_sha256':hashlib.sha256(installed).hexdigest(),
                    'source_contents_evidence_url':self.file_contents_capture(1169772257,row['deployment_source'],row['deployment_sha'],source_path,source,directory),
                    'deployed_contents_evidence_url':self.file_contents_capture(repository_id,repository,revision,path,installed,directory)})
            row['status_evidence_url']=self.capture({'repository_id':repository_id,'repository':repository,
                'revision_sha':revision,'command':'status','status':'completed','exit_code':0,'health':'healthy',
                'manifest_identity':row['manifest_identity'],'observed_at':'2026-10-07T02:00:00Z',
                'file_checks':checks,
                'missing_files':[],'drifted_files':[]},directory)

    def file_contents_capture(self, repository_id, repository, revision, path, content, directory=DIRECTORY):
        blob=hashlib.sha1(b'blob '+str(len(content)).encode()+b'\0'+content).hexdigest()
        return self.capture({'repository_id':repository_id,'repository':repository,'revision_sha':revision,
            'observed_at':'2026-10-07T02:00:00Z','contents_response':{'http_status':200,
                'request_url':'https://api.github.com/repos/'+repository+'/contents/'+path+'?ref='+revision,
                'data':{'path':path,'type':'file','encoding':'base64','content':base64.b64encode(content).decode(),
                    'size':len(content),'sha':blob,'git_url':'https://api.github.com/repos/'+repository+'/git/blobs/'+blob}}},directory)

    def additional_owner_comment(self, decision, directory):
        comment_id=int(decision['evidence_url'].rsplit('-',1)[1]);owner=decision['evidence_url'].split('/')[3]
        approval={key:decision[key] for key in ('repository_id','repository','visibility','disposition','reason')}
        decision['owner_comment_evidence_url']=self.capture({'http_status':200,
            'request_url':'https://api.github.com/repos/'+owner+'/set-it-free-loop/issues/comments/'+str(comment_id),
            'observed_at':decision['approved_at'],'comment':{'id':comment_id,'html_url':decision['evidence_url'],
                'created_at':decision['approved_at'],'updated_at':decision['approved_at'],
                'user':{'id':8227352,'login':'HemSoft','type':'User'},
                'body':'Approved. <!-- sfl-migration-approval:'+json.dumps(approval)+' -->'}},directory)

    def bind_smoke(self, row, directory=DIRECTORY):
        row.update(smoke_outcome='success',smoke_phase='post_transfer')
        row['smoke_evidence_url']=self.capture({'repository_id':int(row['repository_id']),
            'repository':row['source'] if int(row['repository_id']) in validator.APPROVED_RETAINED_IDS else row['destination'],
            'provider':row['provider'],
            'resource_kind':row.get('resource_kind'),'resource_id':row.get('resource_id'),'outcome':'success',
            'phase':'post_transfer','destructive_changes':False,'continuity_verified':True,
            'observed_at':'2026-10-07T02:00:00Z'},directory)

    def workflow_contents(self, repository, revision):
        content=(ROOT/'deployment/infrastructure/sfl-pr-review-auto.yml').read_bytes()
        blob=hashlib.sha1(b'blob '+str(len(content)).encode()+b'\0'+content).hexdigest()
        return {'content':content.decode(),'contents_response':{'http_status':200,
            'request_url':'https://api.github.com/repos/'+repository+'/contents/.github/workflows/sfl-pr-review-auto.yml?ref='+revision,
            'data':{'path':'.github/workflows/sfl-pr-review-auto.yml','type':'file','encoding':'base64',
                'content':base64.b64encode(content).decode(),'size':len(content),'sha':blob,
                'git_url':'https://api.github.com/repos/'+repository+'/git/blobs/'+blob}}}

    def bind_attestation(self, download, directory=DIRECTORY):
        source=download['source_repository'];version=download['release_version'];purl='pkg:github/'+source+'@v'+version
        statement={'_type':'https://in-toto.io/Statement/v1','predicateType':'https://in-toto.io/attestation/release/v0.2',
            'subject':[{'uri':purl,'digest':{'sha1':download['source_sha']}},
                {'name':download['asset_name'],'digest':{'sha256':download['expected_sha256']}}],
            'predicate':{'repository':source,'repositoryId':'1169772257','tag':'v'+version,'purl':purl,'databaseId':'1'}}
        result={'attestation':{'bundle':{'dsseEnvelope':{'payloadType':'application/vnd.in-toto+json',
            'payload':base64.b64encode(json.dumps(statement).encode()).decode(),'signatures':[{'sig':'synthetic-regression-signature'}]}}},
            'verificationResult':{'signature':{'certificate':{'subjectAlternativeName':'https://dotcom.releases.github.com'}},
                'verifiedTimestamps':[{'timestamp':download['observed_at']}],'statement':statement}}
        asset_path='/tmp/synthetic-release/'+download['asset_name']
        download['attestation_evidence_url']=self.capture({'command':'gh release verify-asset',
            'argv':['gh','release','verify-asset','v'+version,asset_path,'--repo',source,'--format','json'],
            'asset_path':asset_path,'exit_code':0,'status':'completed','observed_at':download['observed_at'],'result':result},directory)

    def bind_gate_execution(self, row, directory=DIRECTORY, revision='b'*40):
        gate=row['review_operation_receipts']['gate_run_url'];path=directory/gate['capture_evidence_url'];capture=json.loads(path.read_text())
        run=capture['run'];run.update(id=1,event='issue_comment',run_attempt=1,check_suite_id=2)
        check=capture['check_run'];check.update(id=1,check_suite={'id':3})
        workflow_sha=run['head_sha']
        execution={'repository_id':gate['repository_id'],'repository':gate['repository'],'run_id':run['id'],
            'run_attempt':run['run_attempt'],'execution_sha':run['head_sha'],'workflow_sha':workflow_sha,
            'workflow_path':run['path'],'event':run['event'],'check_run_id':check['id'],'external_id':check['external_id']}
        check['output']={'summary':'Synthetic successful result\n\n<!-- sfl-gate-execution:'+base64.b64encode(json.dumps(execution).encode()).decode()+' -->'}
        capture['execution_evidence_url']=self.capture({'repository_id':gate['repository_id'],'repository':gate['repository'],
            'run_id':run['id'],'run_attempt':run['run_attempt'],'workflow_sha':workflow_sha,'deployment_revision':revision,
            'observed_at':capture['observed_at'],'executed_workflow':self.workflow_contents(gate['repository'],workflow_sha),
            'deployed_workflow':self.workflow_contents(gate['repository'],revision),
            'comparison':{'status':'identical' if workflow_sha==revision else 'ahead','base_commit':{'sha':revision},
                'merge_base_commit':{'sha':revision},'html_url':'https://github.com/'+gate['repository']+'/compare/'+revision+'...'+workflow_sha}},directory)
        path.write_text(json.dumps(capture))

    def bind_download(self, row, directory=DIRECTORY, repository_id=None, repository=None,
                      source=None, sha=None, version=None, field='release_download_verification_url'):
        repository_id=repository_id or row['repository_id'];repository=repository or row['destination']
        source=source or row['deployment_source'];sha=sha or row['deployment_sha']
        version=version or row.get('manifest_version') or row['release_version']
        download={'release_url':'https://github.com/'+source+'/releases/tag/v'+version,
            'source_repository':source,'source_repository_id':1169772257,'source_sha':sha,'release_version':version,
            'target_repository_id':repository_id,'target_repository':repository,'asset_name':'gh-sfl_'+version+'_linux_amd64','platform':'linux_amd64',
            'asset_url':'https://github.com/'+source+'/releases/download/v'+version+'/'+urllib.parse.quote('gh-sfl_'+version+'_linux_amd64'),
            'expected_sha256':'a'*64,'actual_sha256':'a'*64,'checksum_verified':True,'attestation_verified':True,
            'observed_at':'2026-10-07T03:30:00Z'}
        self.bind_attestation(download,directory)
        row[field]=self.capture(download,directory)

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

    def bind_terminal_capture(self, operation, directory=DIRECTORY, timestamp='2026-10-07T02:00:00Z'):
        result={}
        if operation['outcome']=='pull_request_merged':
            result={'pull_request':{'html_url':operation['evidence_url'],'merged':True,'state':'closed',
                'merge_commit_sha':operation['revision_after'],'merged_at':timestamp,
                'base':{'repo':{'id':operation['repository_id'],'full_name':operation['repository']}}}}
        elif operation['outcome']=='no_changes':
            result={'change_count':0,'revision_before':operation['revision_before'],'revision_after':operation['revision_after']}
        elif operation['outcome']=='healthy':
            result={'health':'healthy','revision_sha':operation['revision_after'],'missing_files':[],'drifted_files':[]}
        elif operation['outcome']=='gate_removed':
            result={'gate_required':False,'unrelated_change_count':0,'preserved_unrelated_rules':True}
        operation['capture_evidence_url']=self.capture({**operation,'status':'completed','exit_code':0,
            'observed_at':timestamp,'result':result},directory)

    def bind_workflow_capture(self, operation):
        operation['capture_evidence_url']=self.capture({'observed_at':'2026-10-07T02:00:00Z','run':{
            'repository':{'id':operation['repository_id'],'full_name':operation['repository']},
            'html_url':operation['evidence_url'],'head_sha':operation['run_head_sha'],'path':operation['workflow'],
            'head_branch':self.branch(operation['repository_id']),
            'status':'completed','conclusion':'success','created_at':'2026-10-07T02:00:00Z','updated_at':'2026-10-07T02:00:00Z'}})

    def bind_consumer_runs(self, row):
        self.bind_manifest(row)
        row['wider_workflow_run_urls'] = ['https://github.com/'+row['destination']+'/actions/runs/'+str(i+1)
                                         for i,_ in enumerate(row['wider_workflow_run_urls'])]
        paths=validator.deployed_workflow_paths(row['selected_tier'],row['selected_addons'],row['selected_components']) - {'.github/workflows/sfl-pr-review-auto.yml'}
        row['wider_operation_receipts'] = [{'repository_id':row['repository_id'],'repository':row['destination'],
            'deployment_sha':row['deployment_sha'],'release_version':row['manifest_version'],'evidence_url':run,
            'conclusion':'success','run_head_sha':'b'*40,'workflow':next(iter(sorted(paths)),'.github/workflows/sfl-auditor.yml')}
            for run in row['wider_workflow_run_urls']]
        for operation in row['wider_operation_receipts']:self.bind_workflow_capture(operation)

    def bind_review_operations(self, row, repository_id=None, repository=None, directory=DIRECTORY, revision="b"*40, timestamp="2026-10-07T02:00:00Z"):
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

        number=row['review_pr_url'].rsplit('/',1)[1]
        row['review_pr_metadata_evidence_url']=self.capture({'request_url':'https://api.github.com/repos/'+repository+'/pulls/'+number,
            'observed_at':timestamp,'pull_request':{'number':int(number),'html_url':row['review_pr_url'],
                'base':{'ref':self.branch(repository_id),'sha':row['review_base_sha'],
                    'repo':{'id':repository_id,'full_name':repository}},
                'head':{'sha':row['review_head_sha'],'repo':{'id':repository_id,'full_name':repository}}}},directory)
        context='fixture'
        raw={
            'review_registration_url':{'comment':{'id':1,'html_url':row['review_pr_url']+'#issuecomment-1',
                'user':{'login':row['review_requester']},'created_at':timestamp,'updated_at':timestamp,
                'body':'@codex review\n\n<!-- sfl-codex-review:head='+row['review_head_sha']+';base='+row['review_base_sha']+';context='+context+' -->'}},
            'review_registry_status_url':{'commit_status':{'id':1,'state':'success',
                'context':'SFL Codex Review Request Registry','target_url':row['review_pr_url']+'#issuecomment-1',
                'creator':{'login':row['review_requester']},
                'description':'SFL Codex request comment 1 for PR #'+number+' base '+row['review_base_sha']}},
            'review_artifact_url':{'kind':'issue_comment','artifact':{'id':2,'html_url':row['review_pr_url']+'#issuecomment-2',
                'user':{'id':199175422},'performed_via_github_app':{'id':1144995},'created_at':timestamp,
                'body':'**Reviewed commit:** `'+row['review_head_sha']+'`'}}}
        for field,value in raw.items():
            row[field]=value.get('comment',value.get('artifact',{})).get('html_url',
                'https://github.com/'+repository+'/commit/'+row['review_head_sha'])
            receipt=row['review_operation_receipts'][field];receipt['evidence_url']=row[field]
            receipt['capture_evidence_url']=self.capture({**receipt,**value,'observed_at':timestamp},directory)
        external_id='sfl-codex-review:pull:'+number+':base:'+row['review_base_sha']+':context:fixture:request:1:at:'+str(int(datetime.datetime.fromisoformat(timestamp.replace('Z','+00:00')).timestamp()*1000))+':artifact:c2'
        row['review_deployment_evidence_url']=self.capture({'repository_id':repository_id,'repository':repository,
            'deployed_revision':revision,'reviewed_base_sha':row['review_base_sha'],'pr_url':row['review_pr_url'],
            'head_sha':row['review_head_sha'],'observed_at':'2026-10-07T03:30:00Z',
            'comparison':{'status':'identical' if revision==row['review_base_sha'] else 'ahead',
                'base_commit':{'sha':revision},'merge_base_commit':{'sha':revision},
                'html_url':'https://github.com/'+repository+'/compare/'+revision+'...'+row['review_base_sha']}},directory)
        row['requester_permission_evidence_url']=self.capture({'repository_id':repository_id,'repository':repository,
            'actor':row['review_requester'],'pr_url':row['review_pr_url'],'head_sha':row['review_head_sha'],
            'base_sha':row['review_base_sha'],'http_status':200,'result':{'permission':'admin','role_name':'admin'},
            'observed_at':timestamp},directory)
        row['gate_run_url']='https://github.com/'+repository+'/runs/1'
        gate=row['review_operation_receipts']['gate_run_url']
        gate.update(evidence_url=row['gate_run_url'],status='completed',conclusion='success',
            workflow='.github/workflows/sfl-pr-review-auto.yml',app_id=15368,context='SFL Reviewer Gate Runner')
        gate['capture_evidence_url']=self.capture(dict(gate,artifact_identity=row['review_artifact_identity'],
            observed_at=timestamp,run={'repository':{'id':repository_id,'full_name':repository},
                'head_sha':row['review_head_sha'],'path':gate['workflow'],'status':'completed','conclusion':'success',
                'html_url':'https://github.com/'+repository+'/actions/runs/1',
                'created_at':timestamp,'updated_at':timestamp},
            check_run={'head_sha':row['review_head_sha'],'name':gate['context'],'app':{'id':15368},
                'status':'completed','conclusion':'success','html_url':row['gate_run_url'],
                'external_id':external_id,
                'started_at':timestamp,'completed_at':timestamp}),directory)
        self.bind_gate_execution(row,directory,revision)

    def complete_app_coverage(self, row, directory=DIRECTORY, timestamp='2026-10-07T02:00:00Z'):
        name=row.get('destination') or row['repository']
        row['destination_sfl_app_access'] = {'status':'verified','app_id':4448946,'owner':'hemsoft-dev',
            'repository_id':row['repository_id'],'repository':name,'installation_id':123,
            'evidence_url':self.capture({'repository_id':row['repository_id'],'repository':name,
                'request_url':'https://api.github.com/repos/'+name+'/installation','http_status':200,
                'observed_at':timestamp,'installation':{'id':123,'app_id':4448946,
                    'account':{'id':338855369,'login':'hemsoft-dev','type':'Organization'},
                    'target_type':'Organization','repository_selection':'all','suspended_at':None}},directory)}

    def terminal_protections(self, row, revision):
        repo=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id'])
        row['terminal_protections']=dict(validator.protection_contract(repo),repository_id=repo['id'],
            repository=repo['destination'],revision_sha=revision,observed_at='2026-10-07T02:00:01Z')
        policy=json.loads((DIRECTORY/row['gate_policy']['evidence_url']).read_text())
        row['terminal_protections']['evidence_url']=self.capture(dict(policy,**row['terminal_protections'],phase='post_transfer'))

    def archived_status(self, row, timestamp='2026-10-07T02:00:00Z'):
        repo=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id'])
        row['status_evidence_url']=self.capture({'phase':'post_transfer','repository_id':repo['id'],
            'repository':repo['destination'],'request_url':'https://api.github.com/repos/'+repo['destination'],
            'http_status':200,'observed_at':timestamp,'metadata':{'id':repo['id'],'full_name':repo['destination'],
                **{k:repo[k] for k in ('private','visibility','default_branch','archived')}}})

    def complete_post_transfer_access(self, row):
        if row['repository_id'] == 1143951439:
            row['post_transfer_access'].update(status='verified', effective_permission='none', filled_seats=1,
                paid_seats=1, verified_by='HemSoft',
                permission_evidence_url=self.capture({'phase':'post_transfer','repository_id':row['repository_id'],
                    'repository':row['destination'],'account':'fhemmerrelias','effective_permission':'none',
                    'http_status':200,'result':{'permission':'none'},'observed_at':'2026-10-07T01:55:00Z'}),
                license_evidence_url=self.capture({'phase':'post_transfer','organization_id':338855369,
                    'organization':'hemsoft-dev','plan':{'name':'team','filled_seats':1,'seats':1},
                    'observed_at':'2026-10-07T01:55:00Z'}),verified_at='2026-10-07T02:00:00Z')

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
        proof=self.matrix['pre_transfer_credential_verification']
        proof['credential_metadata_evidence_url']=self.capture({**{field:proof[field] for field in
            ('repository_id','repository','reviewed_sha','run_url','app_id','client_id','owner',
             'installation_id','repository_selection','permission_ceiling_verified')},
            'credential_verification':'success','installation_owner':'HemSoft','target_type':'User',
            'observed_at':'2026-10-07T00:00:00Z'})
        proof['workflow_run_evidence_url']=self.capture({'repository':{'id':proof['repository_id'],'full_name':proof['repository']},
            'html_url':proof['run_url'],'head_sha':proof['reviewed_sha'],'path':proof['workflow'],
            'head_branch':'main','id':1,'run_started_at':'2026-10-07T00:00:00Z','status':'completed','conclusion':'success',
            'created_at':'2026-10-07T00:00:00Z','updated_at':'2026-10-07T00:01:00Z','captured_at':'2026-10-07T00:02:00Z'})
        metadata=json.loads((DIRECTORY/proof['credential_metadata_evidence_url']).read_text())
        stream=io.BytesIO()
        with zipfile.ZipFile(stream,'w') as archive:archive.writestr('sfl-app-credential-metadata.json',json.dumps(metadata))
        url='https://api.github.com/repos/HemSoft/set-it-free-loop/actions/artifacts/1'
        proof['credential_artifact_evidence_url']=self.capture({'request_url':url,'download_url':url+'/zip',
            'observed_at':'2026-10-07T00:02:00Z','archive_base64':base64.b64encode(stream.getvalue()).decode(),
            'artifact':{'id':1,'name':'sfl-app-credential-metadata','expired':False,'archive_download_url':url+'/zip',
                'workflow_run':{'id':1,'head_sha':proof['reviewed_sha']},
                'created_at':'2026-10-07T00:00:30Z','updated_at':'2026-10-07T00:00:30Z',
                'digest':'sha256:'+hashlib.sha256(stream.getvalue()).hexdigest()}})
        accounts = [{'owner':owner,'state':'observed','all_pages':True,'repositories':[
            {**{k:repo[k] for k in ('id','full_name','private','visibility','archived','default_branch')},
             'protections':validator.protection_contract(repo)} for repo in self.inventory['repositories']
            if repo['full_name'].startswith(owner+'/')]} for owner in ('HemSoft','fhemmer')]
        self.matrix['pre_cutover_source_evidence_url']=self.capture({'phase':'pre_cutover',
            'observed_at':'2026-10-07T01:50:00Z','accounts':accounts,
            'destination_account':{'owner':'hemsoft-dev','state':'observed','all_pages':True,
                'request_url':'https://api.github.com/orgs/hemsoft-dev/repos?type=all&per_page=100',
                'repositories':[{'id':p['repository_id'],'full_name':p['repository']}
                    for p in self.matrix['disposable_validation_repositories']]},
            'owned_app':{'id':4448946,'client_id':'Iv23liwvwJJUh2bUIKLW','owner':{'login':'HemSoft','type':'User'},
                         'permissions':self.inventory['known_owned_app']['data']['permissions']},
            'source_installation':{'id':150383874,'app_id':4448946,'owner':'HemSoft','repository_selection':'all'},
            'source_organization_installations':self.inventory['source_organization_apps']})
        tree_refresh=json.loads((DIRECTORY/'source-tree-recheck-evidence.json').read_text())
        tree_refresh.update(phase='pre_cutover',observed_at='2026-10-07T01:50:10Z')
        for tree in tree_refresh['records']:
            repo=next(r for r in self.inventory['repositories'] if r['id']==tree['repository_id'])
            tree.update(observed_at='2026-10-07T01:50:05Z',
                metadata_request_url='https://api.github.com/repos/'+tree['source'],metadata_http_status=200,
                metadata={'id':repo['id'],'full_name':tree['source'],'size':0,'default_branch':repo['default_branch']},
                branches_response={'request_url':'https://api.github.com/repos/'+tree['source']+'/branches?per_page=100',
                    'http_status':200,'all_pages':True,'data':copy.deepcopy(tree['branches'])})
            if tree['state']=='empty_tree':
                tree['commit_response']={'request_url':'https://api.github.com/repos/'+tree['source']+'/git/commits/'+tree['commit_sha'],
                    'http_status':200,'data':{'sha':tree['commit_sha'],'tree':{'sha':tree['tree_sha']}}}
        self.matrix['pre_cutover_tree_evidence_url']=self.capture(tree_refresh)
        for row in self.matrix['repositories']:
            repo=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id'])
            if repo['full_name']=='HemSoft/yahtzee':
                row['post_transfer_runner']={'repository_id':repo['id'],'repository':repo['destination'],'runner_id':21,
                    'online':True,'idle':True,'isolated':True,'service_active':True,'run_conclusion':'success',
                    'run_head_sha':'e'*40,'run_url':'https://github.com/'+repo['destination']+'/actions/runs/1',
                    'registration_evidence_url':'https://example.com/registration','isolation_evidence_url':'https://example.com/isolation',
                    'service_evidence_url':'https://example.com/service'}
                runner=row['post_transfer_runner']
                common={'phase':'post_transfer','observed_at':'2026-10-07T02:00:00Z',
                        'repository_id':repo['id'],'repository':repo['destination'],'runner_id':21}
                runner['registration_evidence_url']=self.capture(dict(common,runner={'id':21,'name':'mini-github-runner-01','status':'online','busy':False,
                    'labels':[{'name':label} for label in ('self-hosted','Linux','X64','mini','yahtzee')]}))
                runner['isolation_evidence_url']=self.capture(dict(common,tailscale_present=False,
                    public_dns_https='passed',isolation_checks=[{'target':t,'blocked':True} for t in
                        ('100.101.122.39:22','100.117.202.124:22','100.69.182.27:22','192.168.1.1:80','10.0.0.1:443','172.16.0.1:443')]))
                runner['service_evidence_url']=self.capture(dict(common,unit='actions.runner.fixture.service',active_state='active'))
                runner['smoke_job_id']=1
                runner['jobs_evidence_url']=self.capture({'request_url':'https://api.github.com/repos/'+repo['destination']+
                    '/actions/runs/1/attempts/1/jobs?per_page=100','all_pages':True,'total_count':1,
                    'observed_at':common['observed_at'],'jobs':[{'id':1,'run_id':1,'run_attempt':1,
                        'head_sha':runner['run_head_sha'],'runner_id':21,'runner_name':'mini-github-runner-01',
                        'labels':['self-hosted','Linux','X64','mini','yahtzee'],'status':'completed','conclusion':'success',
                        'started_at':common['observed_at'],'completed_at':common['observed_at']}]})
                runner['run_evidence_url']=self.capture(dict(common,read_only=True,run={'id':1,'run_attempt':1,'repository':{'id':repo['id'],
                    'full_name':repo['destination']},'html_url':runner['run_url'],'head_sha':runner['run_head_sha'],
                    'status':'completed','conclusion':'success','created_at':'2026-10-07T02:00:00Z','updated_at':'2026-10-07T02:00:00Z'}))
            row['destination_protections'] = dict(validator.protection_contract(repo),
                repository_id=repo['id'],repository=repo['destination'],revision_sha='e'*40,
                observed_at='2026-10-07T02:00:00Z')
            row['destination_protections']['evidence_url']=self.capture(dict(row['destination_protections'],phase='post_transfer'))
            if row['archived'] and row['repository_id'] not in validator.APPROVED_RETAINED_IDS:self.archived_status(row)
        for repo in self.inventory['repositories']:
            self.verify_source_ledger(repo['id'])
        self.bind_ledger_readiness()

    def bind_ledger_readiness(self):
        rows=copy.deepcopy(self.rows)
        for row in rows:
            if row['status']!='verified':continue
            row['verified_at']='2026-10-07T01:49:00Z'
            if row['provider']!='none':
                smoke=json.loads((DIRECTORY/row['smoke_evidence_url']).read_text())
                smoke.update(phase='pre_transfer',repository=row['source'],observed_at='2026-10-07T01:49:00Z')
                row.update(smoke_phase='pre_transfer',smoke_evidence_url=self.capture(smoke))
        pinned={}
        for row in rows:
            if row['status']!='verified':continue
            for key,reference in row.items():
                if key.endswith('evidence_url') and reference and not reference.startswith('https://'):
                    pinned[reference]=hashlib.sha256((DIRECTORY/reference).read_bytes()).hexdigest()
        self.matrix['pre_cutover_ledger_evidence_url']=self.capture({'phase':'pre_cutover','organization_id':338855369,
            'verified_by':'HemSoft','observed_at':'2026-10-07T01:49:00Z','rows':rows,
            'ledger_sha256':validator.ledger_digest(rows),'evidence_sha256':pinned})

    def bind_live_scenario(self, pilot, scenario='findings', run_id=99, artifact_id=100):
        result=pilot['validation_evidence']['scenario_receipts'][scenario]
        path=DIRECTORY/result['capture_evidence_url'];capture=json.loads(path.read_text())
        result.update(mode='live',evidence_url='https://github.com/'+pilot['repository']+'/actions/runs/'+str(run_id))
        capture.update(mode='live',run={'repository':{'id':pilot['repository_id'],'full_name':pilot['repository']},
            'id':run_id,'run_attempt':1,'run_started_at':'2026-10-07T02:00:00Z','html_url':result['evidence_url'],'head_sha':'b'*40,
            'path':'.github/workflows/sfl-pr-review-auto.yml','status':'completed','conclusion':'success',
            'created_at':'2026-10-07T02:00:00Z','updated_at':'2026-10-07T02:00:00Z'})
        output_path=DIRECTORY/capture['output_evidence_url'];output=json.loads(output_path.read_text())
        output.update(mode='live',run_id=run_id,run_attempt=1);output_path.write_text(json.dumps(output))
        stream=io.BytesIO()
        with zipfile.ZipFile(stream,'w') as archive:archive.writestr('scenario.json',json.dumps(output))
        url='https://api.github.com/repos/'+pilot['repository']+'/actions/artifacts/'+str(artifact_id)
        capture['artifact_evidence_url']=self.capture({'request_url':url,'download_url':url+'/zip',
            'observed_at':'2026-10-07T02:00:00Z','archive_base64':base64.b64encode(stream.getvalue()).decode(),
            'artifact':{'id':artifact_id,'name':'sfl-observer-scenario-'+scenario,'expired':False,
                'archive_download_url':url+'/zip','workflow_run':{'id':run_id,'head_sha':'b'*40},
                'created_at':'2026-10-07T02:00:00Z','updated_at':'2026-10-07T02:00:00Z',
                'digest':'sha256:'+hashlib.sha256(stream.getvalue()).hexdigest()}})
        path.write_text(json.dumps(capture))
        return result,capture

    def complete_app_transfer(self):
        self.matrix['owned_app_transfer']['pre_transfer_owner_evidence_url']=self.capture({
            'phase':'pre_transfer','observed_at':'2026-10-07T01:50:15Z',
            'request_url':'https://api.github.com/apps/sfl-app','http_status':200,
            'app':{'id':4448946,'client_id':'Iv23liwvwJJUh2bUIKLW',
                'owner':{'id':8227352,'login':'HemSoft','type':'User'},
                'permissions':copy.deepcopy(self.inventory['known_owned_app']['data']['permissions'])}})
        self.matrix['owned_app_transfer'].update(status='verified',owner='hemsoft-dev',evidence_url=self.capture({
            'phase':'post_transfer','observed_at':'2026-10-07T01:51:00Z','app':{'id':4448946,
            'client_id':'Iv23liwvwJJUh2bUIKLW','owner':{'id':338855369,'login':'hemsoft-dev','type':'Organization'},
            'permissions':self.inventory['known_owned_app']['data']['permissions']}}))
        path=DIRECTORY/'owned-app-organization-installation-evidence.json'
        if not hasattr(self,'original_owned_installation_capture'):
            self.original_owned_installation_capture=path.read_text()
            self.addCleanup(path.write_text,self.original_owned_installation_capture)
        path.write_text(json.dumps({'verification_status':'verified','account':{'login':'hemsoft-dev'},
            'phase':'post_transfer','observed_at':'2026-10-07T01:52:00Z',
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
                   destination_sfl_app_access='verified', transfer_evidence_url=self.transfer_capture(row),
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
                paid_seats=1, verified_by='HemSoft',
                permission_evidence_url=self.capture({'phase':'post_transfer','repository_id':row['repository_id'],
                    'repository':row['destination'],'account':'fhemmerrelias','effective_permission':'none',
                    'http_status':200,'result':{'permission':'none'},'observed_at':'2026-10-07T01:55:00Z'}),
                license_evidence_url=self.capture({'phase':'post_transfer','organization_id':338855369,
                    'organization':'hemsoft-dev','plan':{'name':'team','filled_seats':1,'seats':1},
                    'observed_at':'2026-10-07T01:55:00Z'}),verified_at='2026-10-07T02:00:00Z')
        row.update(manifest_identity={'source':row['deployment_source'],'sourceSha':row['deployment_sha'],
                                      'version':row['manifest_version'],'tier':row['selected_tier'],'addons':[],'components':['sfl-pr-review-auto']},
                   manifest_evidence_url='https://example.com/manifest',
                   release_url='https://github.com/hemsoft-dev/set-it-free-loop/releases/tag/v'+row['manifest_version'],
                   release_download_verification_url='https://example.com/checksum')
        self.bind_manifest(row)
        self.bind_pre_sync(row)
        self.bind_download(row)
        self.terminal_protections(row,'b'*40)
        self.matrix['summary']['verified_rollouts'] += 1
        self.pilots_before_rollout()
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
            receipts['metadata_evidence_url']=self.capture({'observed_at':'2026-10-07T02:00:00Z',
                'metadata':{'id':pilot['repository_id'],'full_name':pilot['repository'],
                    'private':pilot['visibility']=='private','visibility':pilot['visibility'],'archived':False,'default_branch':'main'}})
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
                for operation in receipts['wider_operation_receipts']:self.bind_workflow_capture(operation)
                self.bind_workflow_capture(receipts['auditor_operation_receipt'])
            pilot_coverage=dict(receipts,repository_id=pilot['repository_id'],repository=pilot['repository'])
            self.complete_app_coverage(pilot_coverage)
            receipts['destination_sfl_app_access']=pilot_coverage['destination_sfl_app_access']
            receipts['scenario_receipts'] = {name:{'outcome':outcome,'mode':'workflow_fixture',
                'deployment_sha':receipts['deployment_sha'],'repository_id':pilot['repository_id'],
                'repository':pilot['repository'],'release_version':receipts['release_version'],
                'evidence_url':'https://github.com/'+pilot['repository']+'/issues/1#scenario-'+name}
                for name,outcome in validator.PILOT_SCENARIOS.items()}
            workflow_content=(ROOT/'deployment/infrastructure/sfl-pr-review-auto.yml').read_bytes()
            blob_sha=hashlib.sha1(b'blob '+str(len(workflow_content)).encode()+b'\0'+workflow_content).hexdigest()
            workflow_source=self.capture({'repository_id':pilot['repository_id'],'repository':pilot['repository'],
                'revision_sha':'b'*40,'path':'.github/workflows/sfl-pr-review-auto.yml',
                'content':workflow_content.decode(),'contents_response':{'http_status':200,
                    'request_url':'https://api.github.com/repos/'+pilot['repository']+'/contents/.github/workflows/sfl-pr-review-auto.yml?ref='+'b'*40,
                    'data':{'path':'.github/workflows/sfl-pr-review-auto.yml','type':'file','encoding':'base64',
                        'content':base64.b64encode(workflow_content).decode(),'size':len(workflow_content),'sha':blob_sha,
                        'git_url':'https://api.github.com/repos/'+pilot['repository']+'/git/blobs/'+blob_sha}}})
            for scenario,result in receipts['scenario_receipts'].items():
                output=self.capture({'repository_id':pilot['repository_id'],'repository':pilot['repository'],
                    'deployment_sha':receipts['deployment_sha'],'tested_revision_sha':'b'*40,'scenario':scenario,
                    'mode':result['mode'],'outcome':result['outcome'],'passed':True,'observed_at':'2026-10-07T02:00:00Z',
                    'runner_sha256':hashlib.sha256((ROOT/'deployment/tests/run-org-observer-fixtures.cjs').read_bytes()).hexdigest(),
                    'workflow_sha256':hashlib.sha256((ROOT/'deployment/infrastructure/sfl-pr-review-auto.yml').read_bytes()).hexdigest()})
                result['capture_evidence_url']=self.capture({**result,'scenario':scenario,'tested_revision_sha':'b'*40,
                    'status':'completed','conclusion':'success','command':'run-org-observer-fixtures','exit_code':0,
                    'workflow_evidence_url':workflow_source,'output_sha256':hashlib.sha256((DIRECTORY/output).read_bytes()).hexdigest(),
                    'argv':['node','deployment/tests/run-org-observer-fixtures.cjs','--workflow','docs/organization-migration/'+workflow_source,
                        '--repository-id',str(pilot['repository_id']),'--repository',pilot['repository'],
                        '--deployment-sha',receipts['deployment_sha'],'--release-version',receipts['release_version'],
                        '--revision','b'*40,'--scenario',scenario,'--output','docs/organization-migration/'+output],
                    'observed_at':'2026-10-07T02:00:00Z','output_evidence_url':output})
                result['evidence_url']=result['capture_evidence_url']
            pilot['validation_evidence']['review_artifact_identity'] = {
                'runtime': 'sfl_registered_codex', 'app_id': 1144995, 'bot_user_id': 199175422,
                'reviewed_head_sha': 'b'*40, 'reviewed_base_sha': 'c'*40,
                'review_pr_url':receipts['review_pr_url'],'requester':receipts['review_requester']}
            self.bind_review_operations(receipts, pilot['repository_id'], pilot['repository'])
            for field in receipts['review_operation_receipts']:
                receipts['operation_receipts'][field]['evidence_url']=receipts[field]
            self.bind_manifest(receipts,repository_id=pilot['repository_id'],repository=pilot['repository'])
            self.bind_download(receipts,repository_id=pilot['repository_id'],repository=pilot['repository'])
            for field,operation in receipts['operation_receipts'].items():
                if field not in {'init_pr_url','sync_pr_url','repeat_sync_evidence_url','repeat_onboarding_evidence_url','status_evidence_url','gate_uninstall_evidence_url'}:continue
                command='init' if field in {'init_pr_url','repeat_onboarding_evidence_url'} else 'sync' if field in {'sync_pr_url','repeat_sync_evidence_url'} else 'status' if field=='status_evidence_url' else 'uninstall-gate'
                outcome='pull_request_merged' if field in {'init_pr_url','sync_pr_url'} else 'healthy' if command=='status' else 'gate_removed' if command=='uninstall-gate' else 'no_changes'
                operation.update(command=command,outcome=outcome,revision_before='b'*40,revision_after='b'*40,
                    change_count=0,merged=outcome=='pull_request_merged',gate_only=True,unrelated_change_count=0)
                self.bind_terminal_capture(operation,timestamp='2026-10-07T03:40:00Z' if command=='uninstall-gate' else '2026-10-07T02:00:00Z')
            receipts['final_gate_policy_evidence_url']=self.capture({'repository_id':pilot['repository_id'],
                'repository':pilot['repository'],'branch':self.branch(pilot['repository_id']),
                'observed_at':'2026-10-07T03:41:00Z','classic_protection':{'state':'absent','http_status':404},
                'effective_rules':{'state':'observed','data':[]}})


    def complete_source(self):
        self.complete_pilots()
        self.complete_transfer_gates()
        self.complete_app_transfer()
        row = next(row for row in self.matrix['repositories'] if row['source'] == 'HemSoft/set-it-free-loop')
        row.update(health='source_verified', transfer_evidence_url=self.transfer_capture(row),
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
        root = ROOT / 'deployment/governance'
        repo = next(r for r in self.inventory['repositories'] if r['id']==row['repository_id'])
        row['in_place_evidence']['governance_evidence_url']=self.capture({'phase':'post_transfer',
            'repository_id':row['repository_id'],'repository':row['destination'],
            'revision_sha':row['in_place_evidence']['source_sha'],'observed_at':'2026-10-07T02:00:00Z',
            'labels':json.loads((root/'labels.json').read_text()),'codeowners':(root/'CODEOWNERS').read_text(),
            'actions_policy':repo['settings']['actions_policy']['data'],
            'workflow_permissions':repo['settings']['workflow_permissions']['data']})
        self.bind_review_operations(row,revision=row['in_place_evidence']['source_sha'])
        proof=row['in_place_evidence'];proof['workflow_run_urls']=['https://github.com/'+row['destination']+'/actions/runs/1']
        proof['workflow_operation_receipts']=[{'repository_id':row['repository_id'],'repository':row['destination'],
            'deployment_sha':proof['source_sha'],'release_version':proof['release_version'],'evidence_url':proof['workflow_run_urls'][0],
            'conclusion':'success','workflow':'.github/workflows/sfl-dispatcher.yml','run_head_sha':proof['source_sha']}]
        for operation in proof['workflow_operation_receipts']:self.bind_workflow_capture(operation)
        self.bind_download(proof,repository_id=row['repository_id'],repository=row['destination'],
            source='hemsoft-dev/set-it-free-loop',sha=proof['source_sha'],version=proof['release_version'],field='release_verification_url')
        self.verify_source_ledger(row['repository_id'])
        self.terminal_protections(row,proof['source_sha'])
        self.matrix['summary']['verified_rollouts'] += 1
        proof['default_branch_evidence_url']=self.capture({'phase':'post_transfer','repository_id':row['repository_id'],
            'repository':row['destination'],'branch':repo['default_branch'],'http_status':200,
            'request_url':'https://api.github.com/repos/'+row['destination']+'/git/ref/heads/'+repo['default_branch'],
            'observed_at':'2026-10-07T03:40:00Z','data':{'ref':'refs/heads/'+repo['default_branch'],
                'object':{'type':'commit','sha':proof['source_sha']}}})
        self.pilots_before_rollout()
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
            self.bind_download(row)
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
        row.update(health='archived_verified', transfer_evidence_url=self.transfer_capture(row),
                   status_evidence_url='https://example.com/settings')
        self.archived_status(row)
        self.complete_app_transfer();self.complete_app_coverage(row)
        self.check()

    def test_scope_exception_needs_owner_receipt(self):
        row = self.matrix['repositories'][0]
        row['health'] = 'scope_exception'
        self.complete_transfer_gates()
        row['transfer_evidence_url'] = self.transfer_capture(row)
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
        self.bind_pre_sync(row)
        row['installed_components'] = ['sfl-auditor','sfl-pr-review-auto']
        row['pre_sync_installation']['components']=['sfl-auditor','sfl-pr-review-auto']
        self.bind_pre_sync(row)
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
        self.bind_pre_sync(row)
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
        self.bind_pre_sync(row)
        row['manifest_identity']['tier']='custom'
        row['pre_sync_installation']['components']=row['installed_components']
        self.bind_pre_sync(row)
        row['wider_workflow_run_urls'] = []
        self.bind_manifest(row)
        self.check()
        row['selected_components'].append('sfl-auditor')
        row['installed_components'].append('sfl-auditor')
        row['manifest_identity']['components'].append('sfl-auditor')
        self.bind_pre_sync(row)
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
                       transfer_evidence_url=self.transfer_capture(row),
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
        row['transfer_evidence_url'] = self.transfer_capture(row)
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
        self.bind_pre_sync(row)
        for invalid in (None, []):
            row['installed_components'] = invalid
            with self.assertRaisesRegex(ValueError, 'Installed custom tier'):
                self.check()
        row['installed_components'] = ['sfl-pr-review-auto']
        row['selected_tier']='custom'
        row['selected_components']=['sfl-pr-review-auto']
        row['manifest_identity']['tier']='custom'
        row['pre_sync_installation']['components']=row['installed_components']
        self.bind_pre_sync(row)
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
        self.complete_app_transfer()
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
            self.bind_pre_sync(row)
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
        self.bind_pre_sync(row)
        row['pre_sync_installation'].update(tier=row['installed_tier'],state='absent' if row['installed_tier']=='not_installed' else 'present')
        self.bind_pre_sync(row)
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
        self.bind_pre_sync(row)
        row['manifest_identity']['tier'] = 'minimal'
        row['pre_sync_installation']['addons']=row['installed_addons']
        self.bind_pre_sync(row)
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
        self.bind_pre_sync(row)
        row['manifest_identity']['tier'] = 'custom'
        row['pre_sync_installation']['components']=row['installed_components']
        self.bind_pre_sync(row)
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
        row.update(health='pending_rollout', transfer_evidence_url=self.transfer_capture(row))
        with self.assertRaisesRegex(ValueError, 'post-transfer access and seat proof'): self.check()
        row['post_transfer_access'].update(status='verified', effective_permission='none', filled_seats=1,
            paid_seats=1, verified_by='HemSoft',
            permission_evidence_url=self.capture({'phase':'post_transfer','repository_id':row['repository_id'],
                    'repository':row['destination'],'account':'fhemmerrelias','effective_permission':'none',
                    'http_status':200,'result':{'permission':'none'},'observed_at':'2026-10-07T01:55:00Z'}),
                license_evidence_url=self.capture({'phase':'post_transfer','organization_id':338855369,
                    'organization':'hemsoft-dev','plan':{'name':'team','filled_seats':1,'seats':1},
                    'observed_at':'2026-10-07T01:55:00Z'}),verified_at='2026-10-07T02:00:00Z')
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
            self.complete_app_coverage(replacement)
            replacement['destination_protections']=copy.deepcopy(candidate['destination_protections'])
            replacement['pre_sync_installation'].update(repository_id=inventory['id'],repository=inventory['destination'])
            replacement['transfer_evidence_url']=self.transfer_capture(replacement)
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
        replacement['transfer_evidence_url']=self.transfer_capture(replacement)
        self.complete_app_coverage(replacement)
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
            accounts[name.split('/')[0]]['repositories'].append({'id':repo_id,'full_name':name,'private':repo['private'],'visibility':repo['visibility'],'archived':repo['archived'],'default_branch':repo['default_branch']})
        for repo_id,(name,visibility) in validator.APPROVED_PILOTS.items():
            accounts['hemsoft-dev']['repositories'].append({'id':repo_id,'full_name':name,'private':visibility=='private','visibility':visibility,'archived':False})
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
            accounts[2]['repositories'].append({'id':repo_id, 'full_name':name, 'private':visibility=='private','visibility':visibility})
        accounts[2]['repositories'].append({'id':42, 'full_name':'hemsoft-dev/addition', 'private':True,'visibility':'private'})
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
            self.additional_owner_comment(decision,directory)
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
                   transfer_evidence_url=self.transfer_capture(row),status_evidence_url='https://example.com/status')
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
        self.complete_app_coverage(proof,directory,'2026-10-07T03:30:00Z')
        proof['destination_codex_access']=dict(proof['destination_sfl_app_access'],app_id=1144995,installation_id=168678981)
        self.bind_review_operations(proof,directory=directory,revision='f'*40,timestamp='2026-10-07T03:30:00Z')
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
            self.bind_terminal_capture(operation,directory,'2026-10-07T03:30:00Z')
        for field in ('gate_policy','requester_permission_evidence_url'):
            reference=proof[field]['evidence_url'] if field=='gate_policy' else proof[field]
            path=directory/reference;capture=json.loads(path.read_text());capture['observed_at']='2026-10-07T03:30:00Z';path.write_text(json.dumps(capture))
        gate_path=directory/proof['review_operation_receipts']['gate_run_url']['capture_evidence_url']
        gate_capture=json.loads(gate_path.read_text());gate_capture['observed_at']='2026-10-07T03:30:00Z'
        gate_capture['run'].update(created_at='2026-10-07T03:30:00Z',updated_at='2026-10-07T03:30:00Z')
        gate_capture['check_run'].update(started_at='2026-10-07T03:30:00Z',completed_at='2026-10-07T03:30:00Z');gate_path.write_text(json.dumps(gate_capture))
        proof['manifest_evidence_url']='post-status-manifest.json'
        (directory/proof['manifest_evidence_url']).write_text(json.dumps({'repository_id':42,'repository':name,'revision_sha':'f'*40,'manifest':proof['manifest_identity'],'observed_at':'2026-10-07T03:30:00Z'}))
        capture={'metadata':{'id':42,'full_name':name,'private':True,'visibility':'private','archived':False,'created_at':'2026-10-07T03:00:00Z','default_branch':'main'}}
        (directory/'metadata.json').write_text(json.dumps(capture));proof['metadata_evidence_url']='metadata.json'
        completion={'completed_at':proof['rollout_completed_at'],'organization':'hemsoft-dev',
                    'deployment_sha':proof['deployment_sha'],'release_version':proof['release_version'],'repositories':[]}
        (directory/'completion.json').write_text(json.dumps(completion));proof['rollout_completion_evidence_url']='completion.json'
        proof['release_download_verification_url']=self.capture({'release_url':proof['release_url'],
            'source_repository':proof['deployment_source'],'source_repository_id':1169772257,
            'source_sha':proof['deployment_sha'],'release_version':proof['release_version'],
            'target_repository_id':42,'target_repository':name,'asset_name':'gh-sfl_'+proof['release_version']+'_linux_amd64','platform':'linux_amd64',
            'asset_url':'https://github.com/'+proof['deployment_source']+'/releases/download/v'+proof['release_version']+'/gh-sfl_'+proof['release_version']+'_linux_amd64',
            'expected_sha256':'a'*64,'actual_sha256':'a'*64,'checksum_verified':True,'attestation_verified':True,
            'observed_at':'2026-10-07T03:30:00Z'},directory)
        download_path=directory/proof['release_download_verification_url'];download=json.loads(download_path.read_text())
        self.bind_attestation(download,directory);download_path.write_text(json.dumps(download))
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
                       transfer_evidence_url=self.transfer_capture(row),status_evidence_url='https://example.com/status')
            if row['archived']:self.archived_status(row)
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
                'private':repo['private'],'visibility':repo['visibility'],'archived':repo['archived'],'default_branch':repo['default_branch']})
        for repo_id,(name,visibility) in validator.APPROVED_PILOTS.items():
            accounts['hemsoft-dev']['repositories'].append({'id':repo_id,'full_name':name,'private':visibility=='private','visibility':visibility})
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
        row.update(health='archived_verified',transfer_evidence_url=self.transfer_capture(row),status_evidence_url='https://example.com/status')
        self.archived_status(row)
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
            with self.assertRaisesRegex(ValueError,'Terminal operation capture|actual merged deployment PR'):
                validator.validate_final_onboarding(changed,directory,{},'hemsoft-dev',4448946)
            capture=json.loads((directory/'post-status-manifest.json').read_text());capture['manifest']['sourceSha']='c'*40
            (directory/'post-status-manifest.json').write_text(json.dumps(capture))
            with self.assertRaisesRegex(ValueError,'manifest capture must match'):
                validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)

    def test_sfl_coverage_matches_one_saved_destination_installation(self):
        row=self.complete_rollout();self.check();baseline=copy.deepcopy(row['destination_sfl_app_access'])
        row['destination_sfl_app_access']['installation_id']=42
        with self.assertRaisesRegex(ValueError,'repository-specific destination installation GET'):self.check()
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


    def test_app_credential_assertions_match_uploaded_metadata_and_successful_run(self):
        self.complete_transfer_gates();proof=self.matrix['pre_transfer_credential_verification']
        original=json.loads((DIRECTORY/proof['credential_metadata_evidence_url']).read_text())
        for field,value in [('installation_id',42),('repository_selection','selected'),('reviewed_sha','f'*40),
                            ('permission_ceiling_verified',False),('credential_verification','failed'),('target_type','Organization')]:
            capture=copy.deepcopy(original);capture[field]=value;proof['credential_metadata_evidence_url']=self.capture(capture)
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'uploaded workflow metadata'):self.check()
        proof['credential_metadata_evidence_url']=self.capture(original)
        run=json.loads((DIRECTORY/proof['workflow_run_evidence_url']).read_text())
        for field,value in [('path','.github/workflows/unrelated.yml'),('conclusion','failure'),('head_sha','f'*40),('head_branch','feature/unsafe')]:
            capture=copy.deepcopy(run);capture[field]=value;proof['workflow_run_evidence_url']=self.capture(capture)
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'successful reviewed main'):self.check()

    def test_consumer_pilot_and_source_downloads_require_structured_checksum_evidence(self):
        row=self.complete_rollout();original=row['release_download_verification_url']
        row['release_download_verification_url']='https://example.com/unrelated'
        with self.assertRaisesRegex(ValueError,'Release download needs an independent'):self.check()
        row['release_download_verification_url']=original
        pilot=self.matrix['disposable_validation_repositories'][0]['validation_evidence']
        original=pilot['release_download_verification_url'];capture=json.loads((DIRECTORY/original).read_text())
        capture['actual_sha256']='f'*64;pilot['release_download_verification_url']=self.capture(capture)
        with self.assertRaisesRegex(ValueError,'canonical asset'):self.check()
        pilot['release_download_verification_url']=original
        row=self.complete_source();row['in_place_evidence']['release_verification_url']='https://example.com/unrelated'
        with self.assertRaisesRegex(ValueError,'Release download needs an independent'):self.check()

    def test_classic_protection_semantics_survive_owner_and_repository_url_changes(self):
        repos=[r for r in self.inventory['repositories'] if validator.protection_contract(r)['classic']]
        self.assertEqual(len(repos),5)
        for repo in repos:
            uri='https://api.github.com/repos/'+repo['full_name']+'/branches/main/protection'
            normalized=validator.protection_semantics({'url':uri,'contexts':[uri]},repo)
            self.assertEqual(normalized['url'],'https://api.github.com/repos/'+repo['destination']+'/branches/main/protection')
            self.assertEqual(normalized['contexts'],[uri])
            proof=dict(validator.protection_contract(repo),repository_id=repo['id'],repository=repo['destination'],
                       revision_sha='e'*40,observed_at='2026-10-07T03:30:00Z')
            proof['classic']=validator.protection_semantics(proof['classic'],repo)
            proof['evidence_url']=self.capture(dict(proof,phase='post_transfer'))
            with self.subTest(repository=repo['full_name']):validator.validate_protection_preservation(proof,DIRECTORY,repo)
            branch=next(iter(proof['classic']));proof['classic'][branch]['allow_deletions']={'enabled':True}
            proof['evidence_url']=self.capture(dict(proof,phase='post_transfer'))
            with self.subTest(repository=repo['full_name']),self.assertRaisesRegex(ValueError,'every unrelated classic'):
                validator.validate_protection_preservation(proof,DIRECTORY,repo)

    def test_registered_reviews_must_have_a_base_containing_the_deployed_revision(self):
        row=self.complete_rollout();original=json.loads((DIRECTORY/row['review_deployment_evidence_url']).read_text())
        for field,value in [('deployed_revision','d'*40),('reviewed_base_sha','f'*40),('repository_id',42)]:
            capture=copy.deepcopy(original);capture[field]=value;row['review_deployment_evidence_url']=self.capture(capture)
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'deployment capture must bind'):self.check()
        capture=copy.deepcopy(original);capture['comparison']['merge_base_commit']['sha']='d'*40
        row['review_deployment_evidence_url']=self.capture(capture)
        with self.assertRaisesRegex(ValueError,'must contain the captured deployed'):self.check()
        row['review_deployment_evidence_url']=self.capture(original)
        pilot=self.matrix['disposable_validation_repositories'][0]['validation_evidence']
        capture=json.loads((DIRECTORY/pilot['review_deployment_evidence_url']).read_text());capture['comparison']['status']='behind'
        pilot['review_deployment_evidence_url']=self.capture(capture)
        with self.assertRaisesRegex(ValueError,'must contain the captured deployed'):self.check()
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            capture=json.loads((directory/proof['review_deployment_evidence_url']).read_text());capture['comparison']['status']='diverged'
            proof['review_deployment_evidence_url']=self.capture(capture,directory)
            with self.assertRaisesRegex(ValueError,'must contain the captured deployed'):
                validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)

    def test_source_workflow_glob_triggers_inventory_for_pr_and_main_push(self):
        import fnmatch
        import re
        workflow=(ROOT/'.github/workflows/validate-org-migration.yml').read_text()
        filters=re.findall(r'    paths:\n((?:      - .*\n)+)',workflow)
        self.assertEqual(len(filters),2)
        for filters_for_event in filters:
            paths=re.findall(r"      - '([^']+)'",filters_for_event)
            self.assertIn('deployment/tests/run-org-observer-fixtures.cjs',paths)
            for source_workflow in ('validate-gh-sfl.yml','sfl-pr-review-auto.yml','renamed-source-workflow.yml'):
                with self.subTest(workflow=source_workflow):
                    self.assertTrue(any(fnmatch.fnmatch('.github/workflows/'+source_workflow,path) for path in paths))


    def test_app_ownership_requires_registration_capture_not_installation(self):
        self.complete_transfer_gates();self.complete_app_transfer();self.check()
        proof=self.matrix['owned_app_transfer'];path=DIRECTORY/proof['evidence_url']
        original=json.loads(path.read_text())
        for field,value in [('owner',{'id':8227352,'login':'HemSoft','type':'User'}),
                            ('id',42),('client_id','wrong'),('permissions',{})]:
            changed=copy.deepcopy(original);changed['app'][field]=value;path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'registration metadata'):self.check()
        path.write_text(json.dumps(original));proof['evidence_url']='https://example.com/installation'
        with self.assertRaisesRegex(ValueError,'local capture'):self.check()

    def test_permission_and_seats_require_independent_destination_captures(self):
        row=self.complete_rollout();access=row['post_transfer_access'];self.check()
        for reference,field,value in [('permission_evidence_url','repository_id',42),
            ('permission_evidence_url','account','someone'),('permission_evidence_url','effective_permission','admin'),
            ('license_evidence_url','organization_id',42),('license_evidence_url','plan',{'name':'team','filled_seats':2,'seats':2}),
            ('permission_evidence_url','observed_at','2026-10-06T00:00:00Z')]:
            path=DIRECTORY/access[reference];original=json.loads(path.read_text());changed=copy.deepcopy(original)
            changed[field]=value;path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()
            path.write_text(json.dumps(original))

    def test_pre_cutover_refresh_rejects_omissions_drift_and_stale_apps(self):
        self.complete_transfer_gates();self.check();path=DIRECTORY/self.matrix['pre_cutover_source_evidence_url']
        original=json.loads(path.read_text())
        mutations=[lambda c:c['accounts'][0].update(all_pages=False),
            lambda c:c['accounts'][0]['repositories'].pop(),
            lambda c:c['accounts'][0]['repositories'][0].update(private=True),
            lambda c:c['accounts'][0]['repositories'][0].update(default_branch='other'),
            lambda c:c['accounts'][0]['repositories'][0]['protections'].update(classic={'main':{}}),
            lambda c:c.update(observed_at='2026-10-06T00:00:00Z'),
            lambda c:c['source_installation'].update(repository_selection='selected'),
            lambda c:c['owned_app'].update(permissions={})]
        # Flip rather than assume the first repository is public.
        mutations[2]=lambda c:c['accounts'][0]['repositories'][0].update(private=not c['accounts'][0]['repositories'][0]['private'])
        for index,mutate in enumerate(mutations):
            changed=copy.deepcopy(original);mutate(changed);path.write_text(json.dumps(changed))
            with self.subTest(index=index),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_source_governance_requires_bound_authoritative_configuration(self):
        row=self.complete_source();self.check();proof=row['in_place_evidence']
        path=DIRECTORY/proof['governance_evidence_url'];original=json.loads(path.read_text())
        for field,value in [('repository_id',42),('repository','hemsoft-dev/other'),('revision_sha','f'*40),
            ('phase','pre_transfer'),('labels',[]),('codeowners','* @someone'),('actions_policy',{}),
            ('workflow_permissions',{'default_workflow_permissions':'write'})]:
            changed=copy.deepcopy(original);changed[field]=value;path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_account_preservation_requires_post_rollout_observation(self):
        cutoff=validator.observed_time('2026-10-07T02:00:00Z','test')
        capture={'phase':'post_transfer','observed_at':'2026-10-07T03:00:00Z'}
        validator.validate_preservation_time(capture,cutoff)
        for changed in ({},dict(capture,phase='pre_transfer'),dict(capture,observed_at='2026-10-07T01:00:00Z'),
                        dict(capture,observed_at='2026-10-07T02:00:00Z')):
            with self.subTest(capture=changed),self.assertRaises(ValueError):
                validator.validate_preservation_time(changed,cutoff)

    def test_final_inventory_follows_new_onboarding_final_status(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);onboarding=self.onboarding_fixture(directory)
            accounts=[{'owner':o,'state':'observed','all_pages':True,'repositories':[]} for o in ('HemSoft','fhemmer','hemsoft-dev')]
            for repo_id,(name,visibility) in validator.APPROVED_PILOTS.items():
                accounts[2]['repositories'].append({'id':repo_id,'full_name':name,'private':visibility=='private','visibility':visibility,'archived':False})
            accounts[2]['repositories'].append({'id':42,'full_name':onboarding['repository'],'private':True,'visibility':'private','archived':False,'default_branch':'main'})
            proof={'observed_at':'2026-10-07T04:00:00Z','evidence_url':'final.json','additional_repositories':[]}
            path=directory/'final.json'
            path.write_text(json.dumps({'observed_at':proof['observed_at'],'accounts':accounts}))
            validator.validate_final_inventory(proof,directory,{}, {},onboarding)
            for timestamp in ('2026-10-07T01:00:00Z','2026-10-07T02:30:00Z','2026-10-07T03:15:00Z'):
                proof['observed_at']=timestamp;path.write_text(json.dumps({'observed_at':timestamp,'accounts':accounts}))
                with self.subTest(timestamp=timestamp),self.assertRaisesRegex(ValueError,'must follow rollout'):
                    validator.validate_final_inventory(proof,directory,{}, {},onboarding)


    def test_runner_continuity_loads_independent_operational_captures(self):
        self.complete_transfer_gates();self.complete_app_transfer()
        row=next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/yahtzee')
        row.update(health='scope_exception',transfer_evidence_url=self.transfer_capture(row),
                   status_evidence_url='https://example.com/status');self.complete_scope_decision(row);self.check()
        proof=row['post_transfer_runner']
        for reference,field,value in [('registration_evidence_url','runner',{'id':21,'status':'offline','busy':False}),
            ('isolation_evidence_url','tailscale_present',True),('isolation_evidence_url','isolation_checks',[]),
            ('service_evidence_url','active_state','inactive'),('run_evidence_url','read_only',False),
            ('run_evidence_url','repository_id',42)]:
            path=DIRECTORY/proof[reference];original=json.loads(path.read_text());changed=copy.deepcopy(original)
            changed[field]=value;path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()
            path.write_text(json.dumps(original))


    def test_pilot_scenarios_load_executed_fixture_output(self):
        self.complete_pilots();self.check()
        result=self.matrix['disposable_validation_repositories'][0]['validation_evidence']['scenario_receipts']['findings']
        path=DIRECTORY/result['capture_evidence_url'];original=json.loads(path.read_text())
        for field,value in [('status','pending'),('exit_code',1),('tested_revision_sha','f'*40),
                            ('scenario','new_head'),('command',''),('conclusion','failure')]:
            changed=copy.deepcopy(original);changed[field]=value;path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));output_path=DIRECTORY/original['output_evidence_url']
        output=json.loads(output_path.read_text());output_path.write_text(json.dumps(dict(output,passed=False)))
        with self.assertRaisesRegex(ValueError,'output must prove'):self.check()
        output_path.write_text(json.dumps(output));result['evidence_url']='https://github.com/'+result['repository']+'/issues/1#scenario'
        with self.assertRaisesRegex(ValueError,'successful executed command'):self.check()

    def test_live_pilot_scenario_requires_terminal_observer_run(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0]
        result,capture=self.bind_live_scenario(pilot);path=DIRECTORY/result['capture_evidence_url'];self.check()
        for field,value in [('status','in_progress'),('conclusion','cancelled'),('head_sha','f'*40),
                            ('path','.github/workflows/unrelated.yml')]:
            changed=copy.deepcopy(capture);changed['run'][field]=value;path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'terminal deployed-observer'):self.check()
        path.write_text(json.dumps(capture));result['evidence_url']='https://github.com/'+pilot['repository']+'/issues/1#scenario'
        with self.assertRaisesRegex(ValueError,'terminal deployed-observer'):self.check()

    def test_post_transfer_smokes_follow_source_app_and_destination_captures(self):
        self.complete_transfer_gates();self.complete_app_transfer()
        row=next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/yahtzee')
        row.update(health='scope_exception',transfer_evidence_url=self.transfer_capture(row),status_evidence_url='https://example.com/status')
        self.complete_scope_decision(row);self.check()
        resource=next(r for r in self.rows if r['source']=='HemSoft/yahtzee' and r.get('resource_kind')=='repository_runner');path=DIRECTORY/resource['smoke_evidence_url']
        original=json.loads(path.read_text())
        for timestamp in ('2026-10-07T00:05:00Z','2026-10-07T00:30:00Z','2026-10-07T01:30:00Z'):
            path.write_text(json.dumps(dict(original,observed_at=timestamp)))
            with self.subTest(timestamp=timestamp),self.assertRaisesRegex(ValueError,'smoke observations must follow'):self.check()
        path.write_text(json.dumps(original));self.check()


    def test_destination_protection_capture_follows_cutover(self):
        row=self.complete_rollout();self.check();proof=row['destination_protections']
        path=DIRECTORY/proof['evidence_url'];original=json.loads(path.read_text())
        for timestamp in ('2026-10-07T00:05:00Z','2026-10-07T00:30:00Z','2026-10-07T01:00:00Z'):
            proof['observed_at']=timestamp;path.write_text(json.dumps(dict(original,observed_at=timestamp)))
            with self.subTest(timestamp=timestamp),self.assertRaisesRegex(ValueError,'protections must be captured after'):self.check()
        proof['observed_at']=original['observed_at'];path.write_text(json.dumps(original));self.check()

    def test_pre_sync_capture_proves_manifest_presence_absence_and_configuration(self):
        row=self.complete_rollout();self.check();proof=row['pre_sync_installation']
        path=DIRECTORY/proof['evidence_url'];original=json.loads(path.read_text())
        for field,value in [('repository_id',42),('repository','hemsoft-dev/other'),('revision_sha','f'*40),
                            ('phase','post_sync'),('manifest_paths',[]),('state','present'),('tier','full'),
                            ('addons',['pr-review']),('components',['sfl-auditor']),
                            ('observed_at','2026-10-07T01:00:00Z')]:
            changed=copy.deepcopy(original);changed[field]=value;path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()
        changed=copy.deepcopy(original);changed['manifest_files']['.sfl/sfl.json'].update(
            state='observed',http_status=200,manifest={'tier':'reviewer','addons':[]})
        path.write_text(json.dumps(changed))
        with self.assertRaisesRegex(ValueError,'independent absence'):self.check()
        changed=copy.deepcopy(original);changed['manifest_files']['sfl.json']['http_status']=403
        path.write_text(json.dumps(changed))
        with self.assertRaisesRegex(ValueError,'captured 404'):self.check()
        path.write_text(json.dumps(original));row.update(installed_tier='reviewer',installed_addons=['pr-review'],selected_addons=['pr-review'])
        proof.update(state='present',tier='reviewer',addons=['pr-review']);row['manifest_identity']['addons']=['pr-review']
        self.bind_manifest(row);self.bind_consumer_runs(row);self.bind_pre_sync(row);self.check()
        path=DIRECTORY/proof['evidence_url'];changed=json.loads(path.read_text());changed['manifest_files']['.sfl/sfl.json']['manifest']['addons']=[]
        path.write_text(json.dumps(changed))
        with self.assertRaisesRegex(ValueError,'captured manifest contents'):self.check()

    def test_final_onboarding_inventory_preserves_active_archive_state(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);onboarding=self.onboarding_fixture(directory)
            accounts=[{'owner':o,'state':'observed','all_pages':True,'repositories':[]} for o in ('HemSoft','fhemmer','hemsoft-dev')]
            for repo_id,(name,visibility) in validator.APPROVED_PILOTS.items():
                accounts[2]['repositories'].append({'id':repo_id,'full_name':name,'private':visibility=='private','visibility':visibility,'archived':False})
            new={'id':42,'full_name':onboarding['repository'],'private':True,'visibility':'private','archived':False,'default_branch':'main'};accounts[2]['repositories'].append(new)
            proof={'observed_at':'2026-10-07T04:00:00Z','evidence_url':'final.json','additional_repositories':[]};path=directory/'final.json'
            for archived in (True,None):
                new['archived']=archived;path.write_text(json.dumps({'observed_at':proof['observed_at'],'accounts':accounts}))
                with self.subTest(archived=archived),self.assertRaisesRegex(ValueError,'remain unarchived'):
                    validator.validate_final_inventory(proof,directory,{}, {},onboarding)
            new['archived']=False;path.write_text(json.dumps({'observed_at':proof['observed_at'],'accounts':accounts}))
            validator.validate_final_inventory(proof,directory,{}, {},onboarding)

    def test_designated_new_onboarding_proves_default_reviewer_without_wider_credentials(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)
            for tier,addons in [('minimal',[]),('standard',[]),('full',[]),('reviewer',['pr-review'])]:
                changed=copy.deepcopy(proof);changed['manifest_identity'].update(tier=tier,addons=addons)
                manifest=json.loads((directory/proof['manifest_evidence_url']).read_text());manifest['manifest']=changed['manifest_identity']
                changed['manifest_evidence_url']=self.capture(manifest,directory)
                with self.subTest(tier=tier,addons=addons),self.assertRaisesRegex(ValueError,'default reviewer tier'):
                    validator.validate_final_onboarding(changed,directory,{},'hemsoft-dev',4448946)


    def test_shared_workflow_capture_proves_actual_terminal_run_and_creation_time(self):
        row=self.complete_source();self.check();operation=row['in_place_evidence']['workflow_operation_receipts'][0]
        path=DIRECTORY/operation['capture_evidence_url'];original=json.loads(path.read_text())
        for field,value in [('head_sha','f'*40),('path','.github/workflows/unrelated.yml'),
                            ('conclusion','failure'),('status','in_progress'),
                            ('created_at','2026-10-07T00:30:00Z'),('created_at','2026-10-07T03:00:00Z')]:
            changed=copy.deepcopy(original);changed['run'][field]=value;path.write_text(json.dumps(changed))
            with self.subTest(field=field,value=value),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()
        pilot=self.matrix['disposable_validation_repositories'][0]
        operation=pilot['validation_evidence']['auditor_operation_receipt']
        path=DIRECTORY/operation['capture_evidence_url'];capture=json.loads(path.read_text());capture['run']['repository']['id']=42
        path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'execution capture must prove'):self.check()

    def test_source_governance_observation_follows_app_cutover(self):
        row=self.complete_source();proof=row['in_place_evidence'];path=DIRECTORY/proof['governance_evidence_url']
        capture=json.loads(path.read_text());capture['observed_at']='2026-10-07T00:30:00Z';path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'governance must be captured after'):self.check()


    def test_destination_installation_capture_follows_app_transfer(self):
        self.complete_rollout();path=DIRECTORY/'owned-app-organization-installation-evidence.json'
        original=json.loads(path.read_text());self.check()
        for field,value in [('phase','pre_transfer'),('observed_at','2026-10-07T00:30:00Z'),('observed_at',None)]:
            capture=copy.deepcopy(original);capture[field]=value;path.write_text(json.dumps(capture))
            with self.subTest(field=field,value=value),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_pilot_terminal_execution_corroborates_each_result(self):
        self.complete_pilots();receipts=self.matrix['disposable_validation_repositories'][0]['validation_evidence']
        cases=[('init_pr_url',lambda c:c['result']['pull_request'].update(merged=False)),
               ('sync_pr_url',lambda c:c['result']['pull_request'].update(merge_commit_sha='f'*40)),
               ('repeat_onboarding_evidence_url',lambda c:c['result'].update(change_count=1)),
               ('repeat_sync_evidence_url',lambda c:c.update(exit_code=1)),
               ('status_evidence_url',lambda c:c['result'].update(drifted_files=['workflow.yml'])),
               ('gate_uninstall_evidence_url',lambda c:c['result'].update(gate_required=True))]
        self.check()
        for field,mutate in cases:
            operation=receipts['operation_receipts'][field];path=DIRECTORY/operation['capture_evidence_url']
            original=json.loads(path.read_text());capture=copy.deepcopy(original);mutate(capture);path.write_text(json.dumps(capture))
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()
            path.write_text(json.dumps(original))
        self.check()

    def test_effective_gate_policy_is_current_for_terminal_review(self):
        row=self.complete_rollout();path=DIRECTORY/row['gate_policy']['evidence_url']
        original=json.loads(path.read_text());self.check()
        for timestamp in ['2026-10-07T00:30:00Z','2026-10-07T01:59:59Z']:
            capture=copy.deepcopy(original);capture['observed_at']=timestamp;path.write_text(json.dumps(capture))
            with self.subTest(timestamp=timestamp),self.assertRaisesRegex(ValueError,'Effective gate policy must follow'):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_registered_review_raw_run_executes_after_cutover(self):
        row=self.complete_rollout();gate=row['review_operation_receipts']['gate_run_url']
        path=DIRECTORY/gate['capture_evidence_url'];original=json.loads(path.read_text());self.check()
        for field,value in [('created_at','2026-10-07T01:59:59Z'),('created_at',None),('head_sha',None),('path','.github/workflows/validate-gh-sfl.yml')]:
            capture=copy.deepcopy(original);capture['run'][field]=value;path.write_text(json.dumps(capture))
            with self.subTest(field=field,value=value),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_source_runtime_rejects_ancillary_successful_workflow(self):
        row=self.complete_source();self.check();operation=row['in_place_evidence']['workflow_operation_receipts'][0]
        operation['workflow']='.github/workflows/validate-org-migration.yml';self.bind_workflow_capture(operation)
        with self.assertRaisesRegex(ValueError,'expected deployed workflow'):self.check()

    def test_final_inventory_preserves_default_branch(self):
        expected={r['id']:r for r in self.inventory['repositories']};retained=set(validator.APPROVED_RETAINED_IDS)
        accounts={o:{'owner':o,'state':'observed','all_pages':True,'repositories':[]} for o in ('HemSoft','fhemmer','hemsoft-dev')}
        for rid,repo in expected.items():
            name=repo['full_name'] if rid in retained else repo['destination']
            accounts[name.split('/')[0]]['repositories'].append({'id':rid,'full_name':name,'private':repo['private'],'visibility':repo['visibility'],
                'archived':repo['archived'],'default_branch':repo['default_branch']})
        for rid,(name,visibility) in validator.APPROVED_PILOTS.items():
            accounts['hemsoft-dev']['repositories'].append({'id':rid,'full_name':name,'private':visibility=='private','visibility':visibility,'archived':False})
        capture={'observed_at':'2026-10-07T04:00:00Z','accounts':list(accounts.values())}
        proof={'observed_at':capture['observed_at'],'evidence_url':self.capture(capture),'additional_repositories':[]}
        validator.validate_final_inventory(proof,DIRECTORY,expected,retained)
        target=next(r for r in capture['accounts'][2]['repositories'] if r['id'] in expected)
        target['default_branch']='different-branch';proof['evidence_url']=self.capture(capture)
        with self.assertRaisesRegex(ValueError,'preserve state'):validator.validate_final_inventory(proof,DIRECTORY,expected,retained)


    def test_completion_cutoff_follows_latest_terminal_repository_evidence(self):
        row=self.complete_rollout();times=validator.repository_terminal_times([row],self.rows,DIRECTORY)
        self.assertEqual(times[row['repository_id']],validator.observed_time('2026-10-07T03:30:00Z','fixture'))
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            baseline=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id'])
            path=directory/proof['rollout_completion_evidence_url'];capture=json.loads(path.read_text())
            capture['repositories']=[{'repository_id':baseline['id'],'repository':baseline['destination'],
                'health':'verified','completed_at':'2026-10-07T02:00:00Z'}];path.write_text(json.dumps(capture))
            with self.assertRaisesRegex(ValueError,'latest independently validated'):
                validator.validate_final_onboarding(proof,directory,{baseline['id']:baseline},'hemsoft-dev',4448946,terminal_times=times)

    def test_final_onboarding_inventory_preserves_gated_branch(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);onboarding=self.onboarding_fixture(directory)
            repos=[{'id':rid,'full_name':name,'private':visibility=='private','visibility':visibility,'archived':False,'default_branch':'main'}
                   for rid,(name,visibility) in validator.APPROVED_PILOTS.items()]
            repos.append({'id':42,'full_name':onboarding['repository'],'private':True,'visibility':'private','archived':False,'default_branch':'main'})
            capture={'observed_at':'2026-10-07T04:00:00Z','accounts':[{'owner':o,'state':'observed','all_pages':True,
                'repositories':repos if o=='hemsoft-dev' else []} for o in ('HemSoft','fhemmer','hemsoft-dev')]}
            proof={'observed_at':capture['observed_at'],'evidence_url':self.capture(capture,directory),'additional_repositories':[]}
            validator.validate_final_inventory(proof,directory,{}, {},onboarding)
            repos[-1]['default_branch']='develop';proof['evidence_url']=self.capture(capture,directory)
            with self.assertRaisesRegex(ValueError,'actually gated default branch'):
                validator.validate_final_inventory(proof,directory,{}, {},onboarding)

    def test_issue_comment_run_sha_does_not_replace_actual_reviewed_check_head(self):
        row=self.complete_rollout();path=DIRECTORY/row['review_operation_receipts']['gate_run_url']['capture_evidence_url']
        capture=json.loads(path.read_text());capture['run']['head_sha']=row['review_base_sha'];capture['run']['event']='issue_comment'
        path.write_text(json.dumps(capture));self.bind_gate_execution(row);capture=json.loads(path.read_text());self.check()
        original=copy.deepcopy(capture)
        for field,value in [('head_sha','f'*40),('external_id','sfl-codex-review:pull:99:base:'+row['review_base_sha']+':context:fixture'),('app',{'id':1144995})]:
            capture=copy.deepcopy(original);capture['check_run'][field]=value;path.write_text(json.dumps(capture))
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'actual reviewed PR head'):self.check()

    def test_fixture_runner_command_and_generated_output_are_bound(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0]
        result=pilot['validation_evidence']['scenario_receipts']['findings'];path=DIRECTORY/result['capture_evidence_url']
        original=json.loads(path.read_text());self.check()
        for field,value in [('command','true'),('argv',['true']),('output_sha256','f'*64)]:
            capture=copy.deepcopy(original);capture[field]=value;path.write_text(json.dumps(capture))
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'approved runner'):self.check()
        path.write_text(json.dumps(original))
        execution=subprocess.run(original['argv'],cwd=ROOT,env={'PATH':os.environ['PATH']},text=True,capture_output=True)
        self.assertEqual(execution.returncode,0,execution.stderr)
        output=json.loads((DIRECTORY/original['output_evidence_url']).read_text())
        self.assertTrue(output['passed']);self.assertEqual(output['scenario'],'findings')
        self.assertEqual(output['workflow_sha256'],hashlib.sha256((ROOT/'deployment/infrastructure/sfl-pr-review-auto.yml').read_bytes()).hexdigest())
        original['observed_at']=output['observed_at']
        original['output_sha256']=hashlib.sha256((DIRECTORY/original['output_evidence_url']).read_bytes()).hexdigest()
        cleanup=pilot['validation_evidence']['operation_receipts']['gate_uninstall_evidence_url']
        self.bind_terminal_capture(cleanup,timestamp=output['observed_at'])
        final_policy_path=DIRECTORY/pilot['validation_evidence']['final_gate_policy_evidence_url']
        final_policy=json.loads(final_policy_path.read_text());final_policy['observed_at']=output['observed_at']
        final_policy_path.write_text(json.dumps(final_policy))
        path.write_text(json.dumps(original));self.check()

    def test_pre_sync_revision_must_be_actual_current_deployment_input(self):
        row=self.complete_rollout();proof=row['pre_sync_installation'];path=DIRECTORY/proof['evidence_url']
        original=json.loads(path.read_text());self.check()
        capture=copy.deepcopy(original);capture['default_branch_head']['data']['object']['sha']='f'*40;path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'current default-branch ref'):self.check()
        path.write_text(json.dumps(original));input_path=DIRECTORY/original['deployment_input_evidence_url']
        capture=json.loads(input_path.read_text());capture['input_revision_sha']='f'*40;input_path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'actual deployment input'):self.check()

    def test_preservation_baseline_is_bound_to_each_sealed_provider_resource(self):
        self.complete_transfer_gates()
        for kind in ('vercel_project','github_pages','supabase_project','cloudflare_zone','cloudflare_worker'):
            row=next(r for r in self.rows if r.get('resource_kind')==kind)
            original=copy.deepcopy(row);row['smoke_outcome']='baseline_preserved'
            smoke=json.loads((DIRECTORY/row['smoke_evidence_url']).read_text());smoke.update(outcome='baseline_preserved')
            baseline=validator.provider_preservation_baseline(row,self.inventory,DIRECTORY)
            smoke.update(baseline=baseline,observed_resource=baseline);row['smoke_evidence_url']=self.capture(smoke);self.check()
            smoke.update(baseline={'invented':'unchanged'},observed_resource={'invented':'unchanged'});row['smoke_evidence_url']=self.capture(smoke)
            with self.subTest(kind=kind),self.assertRaisesRegex(ValueError,'sealed provider resource'):self.check()
            row.clear();row.update(original)


    def test_final_inventory_follows_terminal_status_not_only_manifest(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);onboarding=self.onboarding_fixture(directory)
            repos=[{'id':rid,'full_name':name,'private':visibility=='private','visibility':visibility,'archived':False,'default_branch':'main'}
                   for rid,(name,visibility) in validator.APPROVED_PILOTS.items()]
            repos.append({'id':42,'full_name':onboarding['repository'],'private':True,'visibility':'private','archived':False,'default_branch':'main'})
            capture={'observed_at':'2026-10-07T04:00:00Z','accounts':[{'owner':o,'state':'observed','all_pages':True,
                'repositories':repos if o=='hemsoft-dev' else []} for o in ('HemSoft','fhemmer','hemsoft-dev')]}
            proof={'observed_at':capture['observed_at'],'evidence_url':self.capture(capture,directory),'additional_repositories':[]}
            validator.validate_final_inventory(proof,directory,{}, {},onboarding)
            operation=onboarding['onboarding_operation_receipts']['status_url']
            path=directory/operation['capture_evidence_url'];status=json.loads(path.read_text())
            status['observed_at']='2026-10-07T05:00:00Z';path.write_text(json.dumps(status))
            with self.assertRaisesRegex(ValueError,'final status'):
                validator.validate_final_inventory(proof,directory,{}, {},onboarding)

    def test_fixture_source_reconciles_immutable_github_contents(self):
        self.complete_pilots();result=self.matrix['disposable_validation_repositories'][0]['validation_evidence']['scenario_receipts']['findings']
        execution=json.loads((DIRECTORY/result['capture_evidence_url']).read_text())
        path=DIRECTORY/execution['workflow_evidence_url'];original=json.loads(path.read_text());self.check()
        mutations=[lambda c:c.pop('contents_response'),
            lambda c:c['contents_response'].update(request_url=c['contents_response']['request_url'].replace('ref='+'b'*40,'ref=main')),
            lambda c:c['contents_response']['data'].update(content=base64.b64encode(b'fabricated source').decode()),
            lambda c:c['contents_response']['data'].update(sha='f'*40),
            lambda c:c['contents_response']['data'].update(size=1)]
        for index,mutate in enumerate(mutations):
            capture=copy.deepcopy(original);mutate(capture);path.write_text(json.dumps(capture))
            with self.subTest(index=index),self.assertRaisesRegex(ValueError,'immutable|Git blob'):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_consumer_status_requires_actual_healthy_installed_files(self):
        row=self.complete_rollout();path=DIRECTORY/row['status_evidence_url'];original=json.loads(path.read_text());self.check()
        for field,value in [('revision_sha','f'*40),('exit_code',1),('health','drifted'),('manifest_identity',{}),
                            ('observed_at','2026-10-07T00:00:00Z'),('file_checks',[]),('missing_files',['sfl-pr-review-auto.yml'])]:
            capture=copy.deepcopy(original);capture[field]=value;path.write_text(json.dumps(capture))
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'Consumer status'):self.check()
        capture=copy.deepcopy(original);capture['file_checks'][0]['actual_sha256']='f'*64;path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'Consumer status'):self.check()
        path.write_text(json.dumps(original));self.check()
        row['status_evidence_url']='https://example.com/claimed-status'
        with self.assertRaisesRegex(ValueError,'Consumer status'):self.check()

    def test_runner_registration_and_execution_follow_destination_cutover(self):
        self.complete_pilots();row=next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/yahtzee')
        row.update(health='scope_exception',transfer_evidence_url=self.transfer_capture(row),status_evidence_url='https://example.com/status')
        self.complete_scope_decision(row)
        cutoff='2026-10-07T03:00:00Z';row['destination_protections']['observed_at']=cutoff
        protection_path=DIRECTORY/row['destination_protections']['evidence_url'];protection=json.loads(protection_path.read_text())
        protection['observed_at']=cutoff;protection_path.write_text(json.dumps(protection))
        resource=next(r for r in self.rows if r['source']=='HemSoft/yahtzee' and r.get('resource_kind')=='repository_runner')
        smoke_path=DIRECTORY/resource['smoke_evidence_url'];smoke=json.loads(smoke_path.read_text())
        smoke['observed_at']=cutoff;smoke_path.write_text(json.dumps(smoke))
        runner=row['post_transfer_runner'];originals={field:json.loads((DIRECTORY/runner[field]).read_text()) for field in
            ('registration_evidence_url','isolation_evidence_url','service_evidence_url','run_evidence_url')}
        for field,capture in originals.items():
            capture['observed_at']=cutoff
            if field=='run_evidence_url':capture['run'].update(created_at=cutoff,updated_at=cutoff)
            (DIRECTORY/runner[field]).write_text(json.dumps(capture))
        jobs_path=DIRECTORY/runner['jobs_evidence_url'];jobs=json.loads(jobs_path.read_text())
        jobs['observed_at']=cutoff
        for job in jobs['jobs']:job.update(started_at=cutoff,completed_at=cutoff)
        jobs_path.write_text(json.dumps(jobs))
        self.check()
        for field,original in originals.items():
            capture=copy.deepcopy(original);capture['observed_at']='2026-10-07T02:00:00Z'
            (DIRECTORY/runner[field]).write_text(json.dumps(capture))
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'cutover'):self.check()
            (DIRECTORY/runner[field]).write_text(json.dumps(original))
        capture=copy.deepcopy(originals['run_evidence_url']);capture['run']['created_at']='2026-10-07T02:00:00Z'
        (DIRECTORY/runner['run_evidence_url']).write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'destination run'):self.check()

    def test_gate_binds_complete_registered_request_and_native_artifact(self):
        row=self.complete_rollout();gate_path=DIRECTORY/row['review_operation_receipts']['gate_run_url']['capture_evidence_url']
        gate=json.loads(gate_path.read_text());self.check()
        external=gate['check_run']['external_id']
        for value in [external.replace(':request:1:',':request:9:'),external.replace(':artifact:c2',':artifact:c9'),
                      external.replace(':context:fixture:',':context:other:'),external.replace(':at:1791338400000',':at:1')]:
            capture=copy.deepcopy(gate);capture['check_run']['external_id']=value;gate_path.write_text(json.dumps(capture))
            with self.subTest(external_id=value),self.assertRaisesRegex(ValueError,'actual reviewed PR head'):self.check()
        gate_path.write_text(json.dumps(gate))
        mutations=[('review_registration_url',lambda c:c['comment'].update(updated_at='2026-10-07T03:00:00Z')),
            ('review_registry_status_url',lambda c:c['commit_status'].update(target_url=row['review_pr_url']+'#issuecomment-9')),
            ('review_artifact_url',lambda c:c['artifact']['performed_via_github_app'].update(id=1)),
            ('review_artifact_url',lambda c:c['artifact'].update(body='**Reviewed commit:** `'+('f'*40)+'`'))]
        for field,mutate in mutations:
            path=DIRECTORY/row['review_operation_receipts'][field]['capture_evidence_url'];original=json.loads(path.read_text())
            capture=copy.deepcopy(original);mutate(capture);path.write_text(json.dumps(capture))
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()
            path.write_text(json.dumps(original))
        self.check()
        request_path=DIRECTORY/row['review_operation_receipts']['review_registration_url']['capture_evidence_url']
        request=json.loads(request_path.read_text());request['comment']['body']=request['comment']['body'].replace('context=fixture','context=fixture:with%encoding')
        request_path.write_text(json.dumps(request))
        artifact_receipt=row['review_operation_receipts']['review_artifact_url'];artifact_path=DIRECTORY/artifact_receipt['capture_evidence_url']
        artifact=json.loads(artifact_path.read_text());row['review_artifact_url']=row['review_pr_url']+'#pullrequestreview-2'
        artifact_receipt['evidence_url']=row['review_artifact_url'];artifact['evidence_url']=row['review_artifact_url']
        artifact['kind']='pull_request_review';artifact['artifact'].update(html_url=row['review_artifact_url'],commit_id=row['review_head_sha'],submitted_at='2026-10-07T02:00:00Z')
        artifact_path.write_text(json.dumps(artifact))
        gate['check_run']['external_id']=external.replace(':context:fixture:',':context:fixture%3Awith%25encoding:').replace(':artifact:c2',':artifact:r2')
        gate_path.write_text(json.dumps(gate));self.bind_gate_execution(row);self.check()

    def test_pilots_use_actual_metadata_default_branch_and_preserve_it(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0];receipts=pilot['validation_evidence']
        metadata_path=DIRECTORY/receipts['metadata_evidence_url'];metadata=json.loads(metadata_path.read_text());self.check()
        metadata['metadata']['default_branch']='develop';metadata_path.write_text(json.dumps(metadata))
        with self.assertRaisesRegex(ValueError,'target the gated branch'):self.check()
        pr_path=DIRECTORY/receipts['review_pr_metadata_evidence_url'];pr=json.loads(pr_path.read_text())
        pr['pull_request']['base']['ref']='develop';pr_path.write_text(json.dumps(pr))
        receipts['gate_policy']['branch']='develop';policy_path=DIRECTORY/receipts['gate_policy']['evidence_url']
        policy=json.loads(policy_path.read_text());policy['branch']='develop';policy_path.write_text(json.dumps(policy))
        final_policy_path=DIRECTORY/receipts['final_gate_policy_evidence_url'];final_policy=json.loads(final_policy_path.read_text())
        final_policy['branch']='develop';final_policy_path.write_text(json.dumps(final_policy));self.check()
        repos=[{'id':rid,'full_name':name,'private':visibility=='private','visibility':visibility,'archived':False,'default_branch':'main'}
            for rid,(name,visibility) in validator.APPROVED_PILOTS.items()]
        capture={'observed_at':'2026-10-07T04:00:00Z','accounts':[{'owner':o,'state':'observed','all_pages':True,
            'repositories':repos if o=='hemsoft-dev' else []} for o in ('HemSoft','fhemmer','hemsoft-dev')]}
        proof={'observed_at':capture['observed_at'],'evidence_url':self.capture(capture),'additional_repositories':[]}
        branches={rid:'develop' if rid==pilot['repository_id'] else 'main' for rid in validator.APPROVED_PILOTS}
        with self.assertRaisesRegex(ValueError,'pilot.*branch'):
            validator.validate_final_inventory(proof,DIRECTORY,{}, {},pilot_branches=branches)
        next(r for r in repos if r['id']==pilot['repository_id'])['default_branch']='develop'
        proof['evidence_url']=self.capture(capture);validator.validate_final_inventory(proof,DIRECTORY,{}, {},pilot_branches=branches)


    def test_observer_gate_requires_exact_execution_and_pinned_workflow(self):
        row=self.complete_rollout();path=DIRECTORY/row['review_operation_receipts']['gate_run_url']['capture_evidence_url']
        original=json.loads(path.read_text());self.check()
        self.assertNotEqual(original['run']['check_suite_id'],original['check_run']['check_suite']['id'])
        for section,field,value in [('run','id',9),('run','event','push'),('run','run_attempt',2),('check_run','id',9),('check_run','output',{})]:
            capture=copy.deepcopy(original);capture[section][field]=value;path.write_text(json.dumps(capture))
            with self.subTest(section=section,field=field),self.assertRaisesRegex(ValueError,'observer|Observer'):self.check()
        path.write_text(json.dumps(original));workflow_path=DIRECTORY/original['execution_evidence_url'];workflow=json.loads(workflow_path.read_text())
        changed=copy.deepcopy(workflow);changed['comparison']['merge_base_commit']['sha']='f'*40;workflow_path.write_text(json.dumps(changed))
        with self.assertRaisesRegex(ValueError,'Actually executed observer'):self.check()
        changed=copy.deepcopy(workflow);content=(changed['executed_workflow']['content']+'\n# changed observer').encode()
        blob=hashlib.sha1(b'blob '+str(len(content)).encode()+b'\0'+content).hexdigest()
        changed['executed_workflow']['content']=content.decode()
        changed['executed_workflow']['contents_response']['data'].update(content=base64.b64encode(content).decode(),size=len(content),sha=blob,
            git_url='https://api.github.com/repos/'+row['destination']+'/git/blobs/'+blob)
        workflow_path.write_text(json.dumps(changed))
        with self.assertRaisesRegex(ValueError,'Actually executed observer'):self.check()
        workflow_path.write_text(json.dumps(workflow));self.check()

    def test_final_inventory_rejects_internal_visibility_for_private_repositories(self):
        expected={r['id']:r for r in self.inventory['repositories']};retained={r['repository_id'] for r in self.scope['retained_repositories']}
        accounts={owner:{'owner':owner,'state':'observed','all_pages':True,'repositories':[]} for owner in ('HemSoft','fhemmer','hemsoft-dev')}
        for rid,repo in expected.items():
            name=repo['full_name'] if rid in retained else repo['destination']
            accounts[name.split('/')[0]]['repositories'].append({'id':rid,'full_name':name,'private':repo['private'],
                'visibility':repo['visibility'],'archived':repo['archived'],'default_branch':repo['default_branch']})
        for rid,(name,visibility) in validator.APPROVED_PILOTS.items():
            accounts['hemsoft-dev']['repositories'].append({'id':rid,'full_name':name,'private':visibility=='private',
                'visibility':visibility,'archived':False,'default_branch':'main'})
        capture={'observed_at':'2026-10-07T04:00:00Z','accounts':list(accounts.values())}
        proof={'observed_at':capture['observed_at'],'evidence_url':self.capture(capture),'additional_repositories':[]}
        validator.validate_final_inventory(proof,DIRECTORY,expected,retained)
        repo=next(r for r in accounts['hemsoft-dev']['repositories'] if r['id'] in expected and r['private'])
        repo['visibility']='internal';proof['evidence_url']=self.capture(capture)
        with self.assertRaisesRegex(ValueError,'preserve state'):validator.validate_final_inventory(proof,DIRECTORY,expected,retained)
        repo['visibility']='private'
        pilot=next(r for r in accounts['hemsoft-dev']['repositories'] if r['id'] in validator.APPROVED_PILOTS and r['private'])
        pilot['visibility']='internal';proof['evidence_url']=self.capture(capture)
        with self.assertRaisesRegex(ValueError,'recorded identity and visibility'):validator.validate_final_inventory(proof,DIRECTORY,expected,retained)

    def test_release_digest_comes_from_independent_signed_asset_verification(self):
        row=self.complete_rollout();path=DIRECTORY/row['release_download_verification_url'];original=json.loads(path.read_text());self.check()
        substituted=copy.deepcopy(original);substituted.update(expected_sha256='f'*64,actual_sha256='f'*64);path.write_text(json.dumps(substituted))
        with self.assertRaisesRegex(ValueError,'independently verified signed'):self.check()
        path.write_text(json.dumps(original));attestation_path=DIRECTORY/original['attestation_evidence_url'];attestation=json.loads(attestation_path.read_text())
        for field,value in [('argv',['true']),('exit_code',1),('result',{}),('asset_path','/tmp/other')]:
            changed=copy.deepcopy(attestation);changed[field]=value;attestation_path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'attestation'):self.check()
        attestation_path.write_text(json.dumps(attestation))
        changed=copy.deepcopy(attestation);statement=changed['result']['verificationResult']['statement'];statement['subject'][0]['digest']['sha1']='f'*40
        changed['result']['attestation']['bundle']['dsseEnvelope']['payload']=base64.b64encode(json.dumps(statement).encode()).decode()
        attestation_path.write_text(json.dumps(changed))
        with self.assertRaisesRegex(ValueError,'source commit'):self.check()
        attestation_path.write_text(json.dumps(attestation));self.check()

    def test_real_observer_publication_records_its_execution_identity(self):
        source=(ROOT/'deployment/infrastructure/sfl-pr-review-auto.yml').read_text()
        start=source.index('            await github.rest.checks.update({\n              owner,\n              repo,\n              check_run_id: published.data.id,\n              status: "completed",\n              conclusion: "success",')
        end=source.index('            await github.rest.repos.createCommitStatus({',start)
        script="""const fs=require('node:fs');const fragment=fs.readFileSync(0,'utf8');
const AsyncFunction=Object.getPrototypeOf(async function(){}).constructor;
const fn=new AsyncFunction('github','context','owner','repo','published','externalId','title','result','detailsURL',fragment);
let observed;const github={rest:{checks:{update:async x=>{observed=x}}}};
(async()=>{await fn(github,{payload:{repository:{id:42}},runId:7,sha:'b'.repeat(40),eventName:'issue_comment'},'hemsoft-dev','example',{data:{id:57}},'exact-external-id','Success',{reason:'Exact clean result'},'https://example.com/review');process.stdout.write(JSON.stringify(observed));})().catch(()=>process.exit(1));"""
        run=subprocess.run(['node','-e',script],input=source[start:end],text=True,capture_output=True,
            env={'PATH':os.environ['PATH'],'GITHUB_RUN_ATTEMPT':'2','GITHUB_WORKFLOW_SHA':'c'*40})
        self.assertEqual(run.returncode,0,run.stderr);result=json.loads(run.stdout)
        self.assertEqual(result['check_run_id'],57);self.assertEqual(result['conclusion'],'success')
        encoded=result['output']['summary'].split('<!-- sfl-gate-execution:',1)[1].split(' -->',1)[0]
        self.assertEqual(json.loads(base64.b64decode(encoded)),{'repository_id':42,'repository':'hemsoft-dev/example',
            'run_id':7,'run_attempt':2,'execution_sha':'b'*40,'workflow_sha':'c'*40,
            'workflow_path':'.github/workflows/sfl-pr-review-auto.yml','event':'issue_comment','check_run_id':57,'external_id':'exact-external-id'})


    def test_live_scenario_output_is_bound_to_immutable_run_artifact(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0]
        result,capture=self.bind_live_scenario(pilot);self.check()
        path=DIRECTORY/result['capture_evidence_url'];artifact_path=DIRECTORY/capture['artifact_evidence_url']
        original=json.loads(artifact_path.read_text())
        for change in (lambda a:a['artifact']['workflow_run'].update(id=98),
                       lambda a:a['artifact'].update(name='sfl-observer-scenario-new_head'),
                       lambda a:a.update(archive_base64=base64.b64encode(b'unrelated archive').decode()),
                       lambda a:a['artifact'].update(digest='sha256:'+'f'*64)):
            artifact=copy.deepcopy(original);change(artifact);artifact_path.write_text(json.dumps(artifact))
            with self.assertRaises(ValueError):self.check()
        artifact_path.write_text(json.dumps(original))
        unrelated=copy.deepcopy(capture);del unrelated['artifact_evidence_url'];path.write_text(json.dumps(unrelated))
        with self.assertRaisesRegex(ValueError,'Missing evidence'):self.check()
        path.write_text(json.dumps(capture))
        other,other_capture=self.bind_live_scenario(pilot,'pending_request',run_id=99,artifact_id=101);self.check()
        other_path=DIRECTORY/other['capture_evidence_url'];other_capture['artifact_evidence_url']=capture['artifact_evidence_url']
        other_path.write_text(json.dumps(other_capture))
        with self.assertRaisesRegex(ValueError,'exact repository run'):self.check()

    def test_workflow_capture_cannot_backdate_terminal_completion(self):
        self.complete_pilots();receipts=self.matrix['disposable_validation_repositories'][0]['validation_evidence']
        operation=receipts['auditor_operation_receipt'];path=DIRECTORY/operation['capture_evidence_url']
        original=json.loads(path.read_text());self.check()
        for value in ('2026-10-07T02:01:00Z','2026-10-07T01:59:00Z',None):
            capture=copy.deepcopy(original);capture['run']['updated_at']=value;path.write_text(json.dumps(capture))
            with self.subTest(value=value),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()
        row={'repository_id':42,'workflow':{'capture_evidence_url':operation['capture_evidence_url']}}
        times=validator.repository_terminal_times([row],[],DIRECTORY)
        self.assertEqual(times[42],validator.observed_time(original['run']['updated_at'],'test'))

    def test_pre_cutover_ledger_is_separate_pinned_readiness_snapshot(self):
        self.complete_transfer_gates();self.complete_app_transfer();self.check()
        path=DIRECTORY/self.matrix['pre_cutover_ledger_evidence_url'];original=json.loads(path.read_text())
        for change in (lambda c:c.update(observed_at='2026-10-07T03:00:00Z'),
                       lambda c:c['rows'][0].update(verified_at='2026-10-07T03:00:00Z'),
                       lambda c:c['rows'][0].update(status='owner_verification_pending'),
                       lambda c:c.update(evidence_sha256={})):
            capture=copy.deepcopy(original);change(capture);capture['ledger_sha256']=validator.ledger_digest(capture['rows'])
            path.write_text(json.dumps(capture))
            with self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()
        provider=next(r for r in original['rows'] if r['provider']!='none' and r['status']=='verified')
        smoke_path=DIRECTORY/provider['smoke_evidence_url'];smoke=json.loads(smoke_path.read_text())
        smoke_path.write_text(json.dumps(dict(smoke,observed_at='2026-10-07T03:00:00Z')))
        with self.assertRaisesRegex(ValueError,'pinned pre-transfer evidence'):self.check()
        smoke_path.write_text(json.dumps(smoke));self.check()
        self.matrix['pre_cutover_ledger_evidence_url']=None
        with self.assertRaisesRegex(ValueError,'Missing evidence'):self.check()

    def test_release_attestation_requires_versioned_executable_asset(self):
        row=self.complete_rollout();path=DIRECTORY/row['release_download_verification_url']
        original=json.loads(path.read_text());self.check()
        for asset,platform in [('SHA256SUMS','linux_amd64'),('gh-sfl_linux_amd64','linux_amd64'),
                               ('gh-sfl_0.0.1_linux_amd64','linux_amd64'),
                               ('gh-sfl_'+row['manifest_version']+'_linux_amd64','windows_amd64')]:
            download=copy.deepcopy(original);download.update(asset_name=asset,platform=platform,
                asset_url='https://github.com/'+download['source_repository']+'/releases/download/v'+download['release_version']+'/'+asset)
            self.bind_attestation(download);path.write_text(json.dumps(download))
            with self.subTest(asset=asset),self.assertRaisesRegex(ValueError,'versioned installable CLI asset'):self.check()
        download=copy.deepcopy(original);download.update(platform='windows_amd64',
            asset_name='gh-sfl_'+row['manifest_version']+'_windows_amd64.exe')
        download['asset_url']='https://github.com/'+download['source_repository']+'/releases/download/v'+download['release_version']+'/'+download['asset_name']
        self.bind_attestation(download);path.write_text(json.dumps(download));self.check()


    def test_app_credential_metadata_matches_uploaded_run_artifact(self):
        self.complete_transfer_gates();proof=self.matrix['pre_transfer_credential_verification'];self.check()
        artifact_path=DIRECTORY/proof['credential_artifact_evidence_url'];original=json.loads(artifact_path.read_text())
        for change in (lambda c:c['artifact']['workflow_run'].update(id=2),
                       lambda c:c['artifact'].update(name='unrelated-artifact'),
                       lambda c:c['artifact'].update(digest='sha256:'+'f'*64)):
            capture=copy.deepcopy(original);change(capture);artifact_path.write_text(json.dumps(capture))
            with self.assertRaises(ValueError):self.check()
        capture=copy.deepcopy(original);metadata=json.loads((DIRECTORY/proof['credential_metadata_evidence_url']).read_text())
        metadata['repository_selection']='selected';stream=io.BytesIO()
        with zipfile.ZipFile(stream,'w') as archive:archive.writestr('sfl-app-credential-metadata.json',json.dumps(metadata))
        capture['archive_base64']=base64.b64encode(stream.getvalue()).decode()
        capture['artifact']['digest']='sha256:'+hashlib.sha256(stream.getvalue()).hexdigest();artifact_path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'independently downloaded output'):self.check()
        artifact_path.write_text(json.dumps(original));self.check()
        run_path=DIRECTORY/proof['workflow_run_evidence_url'];run=json.loads(run_path.read_text())
        run['run_started_at']='2026-10-07T00:00:01Z';run_path.write_text(json.dumps(run))
        with self.assertRaisesRegex(ValueError,'completed run'):self.check()

    def test_pilot_cleanup_follows_validation_and_final_gate_is_absent(self):
        self.complete_pilots();receipts=self.matrix['disposable_validation_repositories'][0]['validation_evidence'];self.check()
        operation=receipts['operation_receipts']['gate_uninstall_evidence_url'];path=DIRECTORY/operation['capture_evidence_url']
        original=json.loads(path.read_text());capture=copy.deepcopy(original);capture['observed_at']='2026-10-07T02:00:00Z'
        path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'latest completed validation'):self.check()
        path.write_text(json.dumps(original));self.check()
        final_path=DIRECTORY/receipts['final_gate_policy_evidence_url'];final=json.loads(final_path.read_text())
        capture=copy.deepcopy(final);capture['observed_at']='2026-10-07T03:39:00Z';final_path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'follow gate removal'):self.check()
        capture=copy.deepcopy(final);capture['effective_rules']['data']=[{'type':'required_status_checks','parameters':{
            'required_status_checks':[{'context':'SFL Reviewer Gate Runner','integration_id':15368}]}}]
        final_path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'gate is absent'):self.check()
        final_path.write_text(json.dumps(final));self.check()

    def test_unlinked_paused_supabase_keeps_sealed_configuration(self):
        resource=self.matrix['account_resource_preservation'][0]
        baseline=next(p for p in json.loads((DIRECTORY/'supabase-provider-evidence.json').read_text())['projects'] if p['reference']==resource['resource_id'])
        capture={'resource_id':resource['resource_id'],'resource_owner':resource['resource_owner'],
            'state':'paused','project':baseline,'resource_changes_made':False,'operation':'read_only_preservation',
            'phase':'post_transfer','observed_at':'2026-10-07T05:00:00Z','database_actions':[],'credential_reads':False}
        resource.update(status='verified',post_transfer_evidence_url=self.capture(capture));path=DIRECTORY/resource['post_transfer_evidence_url']
        cutoff=validator.observed_time('2026-10-07T04:00:00Z','test')
        validator.validate_unlinked_supabase(resource,baseline,DIRECTORY,cutoff)
        for field,value in [('name','renamed'),('region','different-region'),('resource_url','https://example.com/project')]:
            changed=copy.deepcopy(capture);changed['project'][field]=value;path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'exact account project configuration'):
                validator.validate_unlinked_supabase(resource,baseline,DIRECTORY,cutoff)


    def test_both_pilots_finish_before_active_deployment(self):
        self.complete_rollout();self.check()
        for pilot in self.matrix['disposable_validation_repositories']:
            receipts=pilot['validation_evidence']
            policy_path=DIRECTORY/receipts['final_gate_policy_evidence_url']
            original=json.loads(policy_path.read_text());late=copy.deepcopy(original)
            late['observed_at']='2026-10-07T02:01:00Z';policy_path.write_text(json.dumps(late))
            with self.subTest(visibility=pilot['visibility']),self.assertRaisesRegex(ValueError,'Both pilots must finish'):
                self.check()
            policy_path.write_text(json.dumps(original))
        # A successful wider-workflow test is also a prerequisite, not a later
        # action allowed to retroactively qualify the active deployment.
        private=next(p for p in self.matrix['disposable_validation_repositories'] if p['visibility']=='private')
        receipts=private['validation_evidence'];operation=receipts['auditor_operation_receipt']
        path=DIRECTORY/operation['capture_evidence_url'];capture=json.loads(path.read_text())
        capture['observed_at']='2026-10-07T02:01:00Z'
        capture['run'].update(created_at='2026-10-07T02:01:00Z',updated_at='2026-10-07T02:01:00Z')
        path.write_text(json.dumps(capture))
        self.bind_terminal_capture(receipts['operation_receipts']['gate_uninstall_evidence_url'],timestamp='2026-10-07T02:02:00Z')
        path=DIRECTORY/receipts['final_gate_policy_evidence_url'];capture=json.loads(path.read_text())
        capture['observed_at']='2026-10-07T02:03:00Z';path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'Both pilots must finish'):self.check()

    def test_runner_smoke_executes_on_preserved_runner(self):
        self.complete_transfer_gates();self.complete_app_transfer()
        row=next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/yahtzee')
        row.update(health='scope_exception',transfer_evidence_url=self.transfer_capture(row),status_evidence_url='https://example.com/status')
        self.complete_scope_decision(row);self.check()
        proof=row['post_transfer_runner'];path=DIRECTORY/proof['jobs_evidence_url'];original=json.loads(path.read_text())
        for field,value in [('runner_id',22),('runner_name','GitHub Actions 1'),('labels',['ubuntu-latest']),
                            ('run_id',2),('run_attempt',2),('head_sha','f'*40),('conclusion','skipped'),
                            ('completed_at','2026-10-07T02:01:00Z')]:
            capture=copy.deepcopy(original);capture['jobs'][0][field]=value;path.write_text(json.dumps(capture))
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'preserved self-hosted runner'):self.check()
        path.write_text(json.dumps(original));self.check()
        capture=copy.deepcopy(original);capture['request_url']=capture['request_url'].replace('/attempts/1/','/attempts/2/')
        path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'current run attempt'):self.check()
        path.write_text(json.dumps(original));del proof['jobs_evidence_url']
        with self.assertRaisesRegex(ValueError,'Missing evidence'):self.check()

    def test_repository_transfer_follows_pre_transfer_gates(self):
        self.complete_transfer_gates()
        row=next(r for r in self.matrix['repositories'] if not r['archived'] and r['repository_id']!=1143951439)
        row.update(health='pending_rollout',transfer_evidence_url=self.transfer_capture(row));self.check()
        path=DIRECTORY/row['transfer_evidence_url'];original=json.loads(path.read_text())
        for timestamp in ('2026-10-07T01:48:00Z','2026-10-07T01:49:30Z'):
            capture=copy.deepcopy(original);milliseconds=int(validator.observed_time(timestamp,'test').timestamp()*1000)
            capture['event'].update({'@timestamp':milliseconds,'created_at':milliseconds});path.write_text(json.dumps(capture))
            with self.subTest(timestamp=timestamp),self.assertRaisesRegex(ValueError,'immutable ledger readiness and source recheck'):
                self.check()
        for field,value in [('action','repo.transfer_start'),('repo_id',1),('repo','hemsoft-dev/other'),
                            ('repo_was','HemSoft/other'),('org_id',1),('actor','another-owner'),('_document_id','')]:
            capture=copy.deepcopy(original);capture['event'][field]=value;path.write_text(json.dumps(capture))
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'acceptance event'):self.check()
        path.write_text(json.dumps(original));self.check()
        row['transfer_evidence_url']='https://example.com/transfer'
        with self.assertRaisesRegex(ValueError,'independent local capture'):self.check()

    def test_retained_dependency_verification_precedes_app_transfer(self):
        self.complete_transfer_gates();self.complete_app_transfer();self.check()
        for row in self.matrix['repositories']:
            if row['health']!='retained_source':continue
            dependency=row['retained_app_dependency'];original=dependency['verified_at']
            for timestamp in ('2026-10-07T01:51:00Z','2026-10-07T02:00:00Z'):
                dependency['verified_at']=timestamp
                with self.subTest(repository=row['source'],timestamp=timestamp),self.assertRaisesRegex(ValueError,'must precede App transfer'):
                    self.check()
            dependency['verified_at']=original
        self.check()


    def test_registered_review_binds_actual_pr_target_in_every_completion_mode(self):
        for mode in ('consumer','pilot','source','onboarding'):
            test=RolloutTests();test.setUp()
            try:
                if mode=='consumer':row=test.complete_rollout();directory=DIRECTORY;check=test.check
                elif mode=='pilot':
                    test.complete_pilots();row=test.matrix['disposable_validation_repositories'][0]['validation_evidence']
                    directory=DIRECTORY;check=test.check
                elif mode=='source':row=test.complete_source();directory=DIRECTORY;check=test.check
                else:
                    temporary=tempfile.TemporaryDirectory();test.addCleanup(temporary.cleanup)
                    directory=pathlib.Path(temporary.name);row=test.onboarding_fixture(directory)
                    check=lambda:validator.validate_final_onboarding(row,directory,{},'hemsoft-dev',4448946)
                check();path=directory/row['review_pr_metadata_evidence_url'];original=json.loads(path.read_text())
                for change in (lambda p:p['pull_request']['base'].update(ref='develop'),
                               lambda p:p['pull_request']['base'].update(sha='f'*40),
                               lambda p:p['pull_request']['head'].update(sha='f'*40),
                               lambda p:p['pull_request']['base']['repo'].update(id=1),
                               lambda p:p.update(request_url=p['request_url']+'/unrelated')):
                    capture=copy.deepcopy(original);change(capture);path.write_text(json.dumps(capture))
                    with self.subTest(mode=mode),self.assertRaisesRegex(ValueError,'target the gated branch'):check()
                path.write_text(json.dumps(original));check()
                row['review_pr_metadata_evidence_url']='https://example.com/pr-metadata'
                with self.subTest(mode=mode),self.assertRaisesRegex(ValueError,'independent local capture'):check()
            finally:test.doCleanups()

    def test_source_verifies_current_default_ref_after_its_actual_runs(self):
        row=self.complete_source();self.check();proof=row['in_place_evidence']
        path=DIRECTORY/proof['default_branch_evidence_url'];original=json.loads(path.read_text())
        for change in (lambda c:c['data']['object'].update(sha='f'*40),
                       lambda c:c['data'].update(ref='refs/heads/retained-branch'),
                       lambda c:c.update(branch='retained-branch'),
                       lambda c:c.update(observed_at='2026-10-07T02:00:00Z'),
                       lambda c:c.update(http_status=403)):
            capture=copy.deepcopy(original);change(capture);path.write_text(json.dumps(capture))
            with self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()
        operation=proof['workflow_operation_receipts'][0];run_path=DIRECTORY/operation['capture_evidence_url']
        capture=json.loads(run_path.read_text());capture['run']['head_branch']='retained-branch';run_path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'actual default branch'):self.check()
        capture['run']['head_branch']='main';run_path.write_text(json.dumps(capture));self.check()
        del proof['default_branch_evidence_url']
        with self.assertRaisesRegex(ValueError,'Missing evidence'):self.check()


    def test_pre_cutover_source_refresh_preserves_exact_visibility(self):
        self.complete_transfer_gates();self.check();path=DIRECTORY/self.matrix['pre_cutover_source_evidence_url']
        original=json.loads(path.read_text());capture=copy.deepcopy(original)
        private=next(r for a in capture['accounts'] for r in a['repositories'] if r['private'])
        private['visibility']='internal';path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'metadata changed'):self.check()
        del private['visibility'];path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'metadata changed'):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_access_and_license_capture_follow_actual_repository_transfer(self):
        row=self.complete_rollout();self.assertEqual(row['repository_id'],1143951439);self.check()
        transfer_path=DIRECTORY/row['transfer_evidence_url'];capture=json.loads(transfer_path.read_text())
        milliseconds=int(validator.observed_time('2026-10-07T01:56:00Z','test').timestamp()*1000)
        capture['event'].update({'@timestamp':milliseconds,'created_at':milliseconds})
        capture['observed_at']='2026-10-07T01:56:30Z';transfer_path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'follow repository transfer'):self.check()
        access=row['post_transfer_access'];captures={}
        for field in ('permission_evidence_url','license_evidence_url'):
            path=DIRECTORY/access[field];current=json.loads(path.read_text());current['observed_at']='2026-10-07T01:57:00Z'
            path.write_text(json.dumps(current));captures[field]=current
        self.check()
        for field,original in captures.items():
            path=DIRECTORY/access[field]
            for timestamp in ('2026-10-07T01:55:00Z','2026-10-07T01:56:00Z'):
                current=copy.deepcopy(original);current['observed_at']=timestamp;path.write_text(json.dumps(current))
                with self.subTest(field=field,timestamp=timestamp),self.assertRaisesRegex(ValueError,'follow repository transfer'):
                    self.check()
            path.write_text(json.dumps(original))
        self.check()

    def test_pre_cutover_refresh_rechecks_destination_name_collisions(self):
        self.complete_transfer_gates();self.check();path=DIRECTORY/self.matrix['pre_cutover_source_evidence_url']
        original=json.loads(path.read_text());target=next(r for r in self.inventory['repositories'] if r['id'] not in validator.APPROVED_RETAINED_IDS)
        for name in (target['destination'],target['destination'].split('/')[0]+'/'+target['destination'].split('/')[1].upper()):
            capture=copy.deepcopy(original);capture['destination_account']['repositories'].append({'id':999,'full_name':name})
            path.write_text(json.dumps(capture))
            with self.subTest(name=name),self.assertRaisesRegex(ValueError,'newly occupied mapped transfer name'):self.check()
        capture=copy.deepcopy(original);capture['destination_account']['all_pages']=False;path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'complete destination'):self.check()
        capture=copy.deepcopy(original);del capture['destination_account'];path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'complete destination'):self.check()
        capture=copy.deepcopy(original);capture['destination_account']['repositories'].append({'id':999,'full_name':'hemsoft-dev/unrelated-new-repo'})
        path.write_text(json.dumps(capture));self.check()


    def test_final_onboarding_orders_noop_operations_even_at_one_revision(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)
            fields=('init_pr_url','repeat_onboarding_url','sync_pr_url','repeat_sync_url')
            for field in fields:
                path=directory/proof['onboarding_operation_receipts'][field]['capture_evidence_url']
                original=path.read_text();capture=json.loads(original);capture['observed_at']='2026-10-07T03:31:00Z'
                path.write_text(json.dumps(capture))
                with self.subTest(field=field),self.assertRaisesRegex(ValueError,'terminal operations must follow'):
                    validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)
                path.write_text(original)

    def test_archived_completion_loads_metadata_and_contributes_its_terminal_time(self):
        self.complete_transfer_gates();self.complete_app_transfer()
        row=next(r for r in self.matrix['repositories'] if r['archived'] and r['health']!='retained_source')
        row.update(health='archived_verified',transfer_evidence_url=self.transfer_capture(row));self.complete_app_coverage(row)
        self.check();path=DIRECTORY/row['status_evidence_url'];original=json.loads(path.read_text())
        mutations=(lambda c:c['metadata'].update(archived=False),lambda c:c['metadata'].update(full_name='HemSoft/other'),
            lambda c:c.update(http_status=404),lambda c:c.update(observed_at='2026-10-07T01:49:00Z'))
        for mutate in mutations:
            capture=copy.deepcopy(original);mutate(capture);path.write_text(json.dumps(capture))
            with self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(dict(original,observed_at='2026-10-07T04:00:00Z')));self.check()
        self.assertEqual(validator.repository_terminal_times([row],[],DIRECTORY)[row['repository_id']],
            validator.observed_time('2026-10-07T04:00:00Z','fixture'))
        row['status_evidence_url']='https://example.com/unrelated'
        with self.assertRaisesRegex(ValueError,'Archived destination status needs an independent local capture'):self.check()

    def test_new_repository_loads_actual_app_installation_after_creation(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)
            path=directory/proof['destination_sfl_app_access']['evidence_url'];original=json.loads(path.read_text())
            mutations=(lambda c:c.update(observed_at='2026-10-07T02:59:59Z'),lambda c:c.update(repository_id=43),
                lambda c:c.update(http_status=404),lambda c:c['installation'].update(repository_selection='selected'),
                lambda c:c['installation'].update(suspended_at='2026-10-07T03:00:00Z'),
                lambda c:c.update(request_url='https://api.github.com/repos/hemsoft-dev/other/installation'))
            for mutate in mutations:
                capture=copy.deepcopy(original);mutate(capture);path.write_text(json.dumps(capture))
                with self.assertRaises(ValueError):
                    validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)
            path.write_text(json.dumps(dict(original,observed_at='2026-10-07T03:35:00Z')))
            validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)
            self.assertEqual(validator.final_onboarding_terminal_time(proof,directory),
                validator.observed_time('2026-10-07T03:35:00Z','fixture'))

    def test_unavailable_trees_need_current_branches_between_refresh_and_transfer(self):
        self.complete_transfer_gates();self.check();path=DIRECTORY/self.matrix['pre_cutover_tree_evidence_url']
        original=json.loads(path.read_text())
        mutations=(lambda c:c.update(observed_at='2026-10-07T01:49:00Z'),
            lambda c:c['records'][0].update(observed_at='2026-10-07T01:49:00Z'),
            lambda c:c['records'][0]['branches_response']['data'][0]['commit'].update(sha='a'*40),
            lambda c:c['records'][1]['branches_response']['data'].append({'name':'main','commit':{'sha':'a'*40}}),
            lambda c:c['records'][0]['commit_response']['data']['tree'].update(sha='a'*40),
            lambda c:c['records'][0]['branches_response'].update(all_pages=False))
        for mutate in mutations:
            capture=copy.deepcopy(original);mutate(capture);path.write_text(json.dumps(capture))
            with self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()
        row=next(r for r in self.matrix['repositories'] if not r['archived'] and r['repository_id']!=1143951439)
        row.update(health='pending_rollout',transfer_evidence_url=self.transfer_capture(row,'2026-10-07T01:50:05Z'))
        with self.assertRaisesRegex(ValueError,'transfer must follow'):self.check()
        row['transfer_evidence_url']=self.transfer_capture(row);self.check()

    def test_terminal_gate_preserves_baseline_protections_after_installation(self):
        index=next(i for i,r in enumerate(self.matrix['repositories']) if r['source']=='HemSoft/dashboard')
        self.matrix['repositories'].insert(0,self.matrix['repositories'].pop(index))
        row=self.complete_rollout();self.check();proof=row['terminal_protections'];original=copy.deepcopy(proof)
        path=DIRECTORY/proof['evidence_url'];original_capture=json.loads(path.read_text())
        mutations=(lambda p:p.update(observed_at='2026-10-07T01:59:59Z'),lambda p:p.update(rulesets=[]),
            lambda p:p.update(revision_sha='d'*40))
        for mutate in mutations:
            changed=copy.deepcopy(original);mutate(changed);row['terminal_protections']=changed
            path.write_text(json.dumps(dict(original_capture,**{k:v for k,v in changed.items() if k!='evidence_url'})))
            with self.assertRaises(ValueError):self.check()
        row['terminal_protections']=original;path.write_text(json.dumps(original_capture));self.check()
        capture=copy.deepcopy(original_capture);capture['effective_rules']['data']=[];path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'actually require the strict'):self.check()


    def test_app_transfer_brackets_ownership_after_all_pre_transfer_gates(self):
        self.complete_transfer_gates();self.complete_app_transfer();self.check()
        proof=self.matrix['owned_app_transfer'];path=DIRECTORY/proof['pre_transfer_owner_evidence_url']
        original=json.loads(path.read_text())
        mutations=(lambda c:c.update(observed_at='2026-10-07T01:50:10Z'),lambda c:c.update(http_status=404),
            lambda c:c['app']['owner'].update(login='hemsoft-dev',type='Organization'),
            lambda c:c['app']['owner'].update(id=42),lambda c:c['app'].update(client_id='other'))
        for mutate in mutations:
            capture=copy.deepcopy(original);mutate(capture);path.write_text(json.dumps(capture))
            with self.assertRaisesRegex(ValueError,'still-personally-owned registration GET'):self.check()
        path.write_text(json.dumps(original));self.check()
        after_path=DIRECTORY/proof['evidence_url'];capture=json.loads(after_path.read_text())
        capture['observed_at']='2026-10-07T01:50:14Z';after_path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'post-transfer registration metadata'):self.check()


    def test_consumer_workflow_hashes_derive_from_canonical_and_deployed_contents(self):
        row=self.complete_rollout();row.update(selected_tier='full',wider_workflow_run_urls=['https://example.com/run'])
        row['manifest_identity']['tier']='full';self.bind_consumer_runs(row);self.check()
        path=DIRECTORY/row['status_evidence_url'];original=json.loads(path.read_text())
        capture=copy.deepcopy(original);check=next(c for c in capture['file_checks'] if c['path'].endswith('/sfl-auditor.yml'))
        substituted=b'name: Substituted workflow\non: workflow_dispatch\n'
        check['expected_sha256']=check['actual_sha256']=hashlib.sha256(substituted).hexdigest()
        check['deployed_contents_evidence_url']=self.file_contents_capture(row['repository_id'],row['destination'],'b'*40,check['path'],substituted)
        path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'hashes must derive from the pinned canonical'):self.check()
        path.write_text(json.dumps(original));self.check()
        reference=original['file_checks'][0]['source_contents_evidence_url'];source_path=DIRECTORY/reference
        source=json.loads(source_path.read_text());baseline=copy.deepcopy(source)
        for mutate in (lambda c:c['contents_response']['data'].update(sha='f'*40),
                       lambda c:c.update(revision_sha='d'*40),lambda c:c.update(repository_id=42)):
            changed=copy.deepcopy(baseline);mutate(changed);source_path.write_text(json.dumps(changed))
            with self.assertRaises(ValueError):self.check()

    def test_final_addition_loads_actual_owner_comment_author_and_decision(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder)
            repositories=[{'id':rid,'full_name':name,'private':visibility=='private','visibility':visibility}
                for rid,(name,visibility) in validator.APPROVED_PILOTS.items()]
            repositories.append({'id':42,'full_name':'hemsoft-dev/addition','private':True,'visibility':'private'})
            capture={'observed_at':'2026-10-07T03:00:00Z','accounts':[{'owner':owner,'state':'observed','all_pages':True,
                'repositories':repositories if owner=='hemsoft-dev' else []} for owner in ('HemSoft','fhemmer','hemsoft-dev')]}
            extra={'repository_id':42,'repository':'hemsoft-dev/addition','visibility':'private','approved_by':'HemSoft',
                'evidence_url':'https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-123'}
            decision=dict(extra,approved_at='2026-10-07T02:00:00Z',reason='Owner approved addition',disposition='include_final_inventory')
            self.additional_owner_comment(decision,directory);extra['decision_artifact']=self.capture(decision,directory)
            proof={'evidence_url':self.capture(capture,directory),'observed_at':capture['observed_at'],'additional_repositories':[extra]}
            validator.validate_final_inventory(proof,directory,{}, {})
            path=directory/decision['owner_comment_evidence_url'];original=json.loads(path.read_text())
            mutations=(lambda c:c['comment']['user'].update(login='other'),lambda c:c['comment']['user'].update(id=42),
                lambda c:c['comment'].update(created_at='2026-10-07T01:59:00Z'),
                lambda c:c['comment'].update(updated_at='2026-10-07T04:00:00Z'),lambda c:c.update(http_status=404),
                lambda c:c['comment'].update(body='Unrelated owner comment'),
                lambda c:c['comment'].update(body=c['comment']['body'].replace('"repository_id": 42','"repository_id": 43')))
            for mutate in mutations:
                changed=copy.deepcopy(original);mutate(changed);path.write_text(json.dumps(changed))
                with self.assertRaises(ValueError):validator.validate_final_inventory(proof,directory,{}, {})
            changed=copy.deepcopy(original);changed['comment']['html_url']=changed['comment']['html_url'].replace('/HemSoft/','/hemsoft-dev/')
            path.write_text(json.dumps(changed));validator.validate_final_inventory(proof,directory,{}, {})


if __name__ == '__main__':
    unittest.main()
