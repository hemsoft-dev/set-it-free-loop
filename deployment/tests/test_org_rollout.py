"""Regression tests for offline ledger and rollout completion gates."""

import copy
import base64
import datetime
import csv
import importlib.util
import json
import hashlib
import gzip
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
        resource_path=DIRECTORY/'current-survival-resources.json';resource_original=resource_path.read_bytes()
        self.addCleanup(resource_path.write_bytes,resource_original)
        resource=json.loads(resource_original)
        def synthetic_resource_times(value):
            if isinstance(value,dict):
                for key,item in value.items():
                    if key in {'observed_at','captured_at','derived_at'}:value[key]='2026-10-07T01:47:00Z'
                    else:synthetic_resource_times(item)
            elif isinstance(value,list):
                for item in value:synthetic_resource_times(item)
        synthetic_resource_times(resource)
        resource_bytes=(json.dumps(resource,indent=2)+'\n').encode();resource_path.write_bytes(resource_bytes)
        resource_pin=patch.object(validator,'SURVIVAL_RESOURCES_SHA256',hashlib.sha256(resource_bytes).hexdigest())
        resource_pin.start();self.addCleanup(resource_pin.stop)
        self.matrix = json.loads((DIRECTORY / 'rollout-matrix.json').read_text())
        self.scope = json.loads((DIRECTORY / 'scope-decisions.json').read_text())
        self.release_results={}
        self.real_release_verification=validator.run_release_verification
        self.release_verifier=patch.object(validator,'run_release_verification',
            side_effect=lambda argv:copy.deepcopy(self.release_results[tuple(argv)]))
        self.release_verifier.start();self.addCleanup(self.release_verifier.stop)
        with (DIRECTORY / 'integration-ledger.csv').open(newline='') as stream:
            self.rows = list(csv.DictReader(stream))
        self.bind_global_owner_scopes()
        decision={'repository_ids':sorted(validator.APPROVED_RETAINED_IDS),'disposition':validator.RETAINED_APP_DISPOSITION}
        at='2026-10-07T01:47:30Z';comment_id=int(validator.RETAINED_APP_RECEIPT.rsplit('-',1)[1])
        owner=self.capture({'method':'GET','http_status':200,'observed_at':at,
            'request_url':'https://api.github.com/repos/HemSoft/set-it-free-loop/issues/comments/'+str(comment_id),
            'comment':{'id':comment_id,'html_url':validator.RETAINED_APP_RECEIPT,'created_at':'2026-10-07T00:00:00Z',
                'updated_at':at,'user':{'id':8227352,'login':'HemSoft','type':'User'},
                'body':'Synthetic retained dependency waiver. <!-- sfl-migration-approval:'+json.dumps(decision)+' -->'}})
        for row in self.matrix['repositories']:
            if row['repository_id'] in validator.APPROVED_RETAINED_IDS:
                row['retained_app_dependency'].update(owner_comment_evidence_url=owner,decision_recorded_at=at,verified_at=at)

    def bind_global_owner_scopes(self):
        for filename in ('provider-absence-owner-evidence.json', 'external-resource-owner-scope-evidence.json',
                         'legacy-unused-credential-owner-evidence.json'):
            path=DIRECTORY/filename;original=path.read_bytes();record=json.loads(original)
            self.addCleanup(path.write_bytes,original)
            if filename.startswith('provider-absence'):
                decision={'scope':record['scope'],'providers':sorted(record['providers']),'disposition':record['disposition']}
            elif filename.startswith('external-resource'):
                decision={field:record[field] for field in ('scope','active_external_resources','blacksmith_usage',
                                                           'modern_web_stack_poc_usage','database_treatment')}
            else:decision={'repository_count':42,'disposition':'Unused by external clients','repositories':record['repositories']}
            at='2026-10-07T01:48:30Z';record['decision_recorded_at']=at
            comment_id=int(record['evidence_url'].rsplit('-',1)[1])
            record['owner_comment_evidence_url']=self.capture({'http_status':200,
                'request_url':'https://api.github.com/repos/HemSoft/set-it-free-loop/issues/comments/'+str(comment_id),
                'observed_at':at,'comment':{'id':comment_id,'html_url':record['evidence_url'],
                    'created_at':record['confirmed_at'],'updated_at':at,
                    'user':{'id':8227352,'login':'HemSoft','type':'User'},
                    'body':'Synthetic owner fact. <!-- sfl-migration-approval:'+json.dumps(decision)+' -->'}})
            path.write_text(json.dumps(record))

    def check(self):
        return validator.validate(self.inventory, self.rows, self.matrix, DIRECTORY, self.scope)

    def bind_final_pages(self, value):
        if isinstance(value.get('accounts'),list):
            for account in value['accounts'] + ([value['destination_account']] if 'destination_account' in value else []):
                owner=account['owner'];url=('https://api.github.com/user/repos?affiliation=owner&per_page=100' if owner=='HemSoft' else
                    'https://api.github.com/orgs/'+owner+'/repos?type=all&per_page=100')
                account['pages']=[{'method':'GET','request_url':url,'http_status':200,'response_headers':{},
                    'observed_at':value['observed_at'],'data':copy.deepcopy(account['repositories'])}]
        return value

    def capture(self, value, directory=DIRECTORY, prefix='fixture-capture-'):
        self.bind_final_pages(value)
        if 'comment' in value and 'request_url' in value:value.setdefault('method','GET')
        if 'artifact' in value and 'request_url' in value:
            value.setdefault('artifact_response',{'method':'GET','http_status':200,'request_url':value['request_url'],
                'observed_at':value['observed_at'],'data':copy.deepcopy(value['artifact'])})
        if 'pull_request' in value and 'request_url' in value:
            value.setdefault('method','GET');value.setdefault('http_status',200);value.setdefault('data',copy.deepcopy(value['pull_request']))
        if isinstance(value.get('run'),dict) and value['run'].get('html_url'):
            run=value['run'];run.setdefault('id',int(run['html_url'].rsplit('/',1)[1]))
            name=run.get('repository',{}).get('full_name')
            value.setdefault('run_response',{'method':'GET','http_status':200,'observed_at':value.get('observed_at',value.get('captured_at')),
                'request_url':'https://api.github.com/repos/'+name+'/actions/runs/'+str(run['id']),'data':copy.deepcopy(run)})
            if isinstance(value.get('check_run'),dict):
                check=value['check_run'];check.setdefault('id',int(check['html_url'].rsplit('/',1)[1]))
                value.setdefault('check_response',{'method':'GET','http_status':200,'observed_at':value['observed_at'],
                    'request_url':'https://api.github.com/repos/'+name+'/check-runs/'+str(check['id']),'data':copy.deepcopy(check)})
        if all(key in value for key in ('comparison','deployed_revision','reviewed_base_sha','repository')):
            value.setdefault('compare_response',{'method':'GET','http_status':200,'observed_at':value['observed_at'],
                'request_url':'https://api.github.com/repos/'+value['repository']+'/compare/'+
                    value['deployed_revision']+'...'+value['reviewed_base_sha'],'data':copy.deepcopy(value['comparison'])})
        if value.get('phase') in {'post_transfer','pre_cutover','pre_transfer'} and 'rulesets' in value and 'classic' in value and any(
                r['id']==value.get('repository_id') for r in self.inventory['repositories']):
            repo=next(r for r in self.inventory['repositories'] if r['id']==value['repository_id'])
            base='https://api.github.com/repos/'+value['repository'];at=value['observed_at']
            def response(suffix,data,status=200):return {'method':'GET','http_status':status,'observed_at':at,
                'request_url':base+suffix,'response_headers':{},'data':copy.deepcopy(data)}
            metadata={k:repo[k] for k in ('id','private','visibility','archived','default_branch')};metadata['full_name']=value['repository']
            value.setdefault('repository_response',response('',metadata))
            value.setdefault('revision_response',response('/git/ref/heads/'+urllib.parse.quote(repo['default_branch'],safe=''),
                {'ref':'refs/heads/'+repo['default_branch'],'object':{'sha':value['revision_sha']}}))
            if value['revision_sha'] is None:
                value['revision_response']=response('/git/ref/heads/'+urllib.parse.quote(repo['default_branch'],safe=''),
                    {'message':'Git Repository is empty.'},409)
            value.setdefault('ruleset_pages',[response('/rulesets?includes_parents=true&per_page=100',[{'id':r['id']} for r in value['rulesets']])])
            value.setdefault('ruleset_responses',{str(r['id']):response('/rulesets/'+str(r['id']),r) for r in value['rulesets']})
            names=sorted(set(value['classic'])|({value['branch']} if 'effective_rules' in value else set()))
            value.setdefault('protected_branch_pages',[response('/branches?protected=true&per_page=100',[{'name':name} for name in names])])
            value.setdefault('classic_responses',{name:response('/branches/'+urllib.parse.quote(name,safe='')+'/protection',
                value['classic'].get(name,{'message':'Branch not protected'}),200 if name in value['classic'] else 404) for name in names})
        if 'effective_rules' in value and 'classic_protection' in value and 'branch' in value:
            base='https://api.github.com/repos/'+value['repository'];branch=urllib.parse.quote(value['branch'],safe='')
            for field,suffix in (('effective_rules','/rules/branches/'+branch),('classic_protection','/branches/'+branch+'/protection')):
                response=value[field];absent=response.get('state')=='absent'
                response.setdefault('method','GET');response.setdefault('request_url',base+suffix)
                response.setdefault('http_status',404 if absent else 200);response.setdefault('observed_at',value['observed_at'])
                if absent:response.setdefault('data',{'message':'Branch not protected'})
        if 'manifest' in value and all(k in value for k in ('repository_id','repository','revision_sha')):
            value.setdefault('manifest_path','.sfl/sfl.json')
            value.setdefault('contents_response',self.file_response(value['repository'],value['revision_sha'],
                value['manifest_path'],json.dumps(value['manifest']).encode()))
        if 'codeowners' in value:
            base='https://api.github.com/repos/'+value['repository']
            for field,suffix in (('actions_policy','/actions/permissions'),('workflow_permissions','/actions/permissions/workflow')):
                value.setdefault(field+'_response',{'method':'GET','http_status':200,'request_url':base+suffix,
                    'observed_at':value['observed_at'],'data':copy.deepcopy(value[field])})
            value.setdefault('labels_pages',[{'method':'GET','http_status':200,'request_url':base+'/labels?per_page=100',
                'observed_at':value['observed_at'],'response_headers':{},'data':copy.deepcopy(value['labels'])}])
            value.setdefault('codeowners_contents',{'repository_id':value['repository_id'],'repository':value['repository'],
                'revision_sha':value['revision_sha'],'observed_at':value['observed_at'],
                'contents_response':self.file_response(value['repository'],value['revision_sha'],'.github/CODEOWNERS',value['codeowners'].encode())})
        if 'jobs' in value and 'request_url' in value:
            value.setdefault('pages',[{'method':'GET','http_status':200,'request_url':value['request_url'],
                'observed_at':value['observed_at'],'response_headers':{},
                    'data':{'total_count':value['total_count'],'jobs':copy.deepcopy(value['jobs'])}}])
        if 'captured_at' in value and 'html_url' in value and isinstance(value.get('repository'), dict):
            raw_run={key:item for key,item in value.items() if key not in {'captured_at','run_response'}}
            value.setdefault('run_response',{'method':'GET','http_status':200,'observed_at':value['captured_at'],
                'request_url':'https://api.github.com/repos/'+value['repository']['full_name']+
                    '/actions/runs/'+str(value['id']),'data':copy.deepcopy(raw_run)})
        def branch_pages(node):
            if isinstance(node,dict):
                if isinstance(node.get('branches_response'),dict):
                    branches=node['branches_response']
                    node.setdefault('tag_refs_response',{'method':'GET','http_status':200,
                        'request_url':branches['request_url'].split('/branches?')[0]+'/git/matching-refs/tags/',
                        'observed_at':node.get('observed_at',value.get('observed_at')),'response_headers':{},'data':[]})
                    node.setdefault('tag_object_responses',{})
                    if 'pages' not in branches:
                        branches['pages']=[{'method':'GET','http_status':branches.get('http_status'),
                            'request_url':branches.get('request_url'),'data':copy.deepcopy(branches.get('data')),
                            'response_headers':{},'observed_at':node.get('observed_at',value.get('observed_at'))}]
                for child in list(node.values()):branch_pages(child)
            elif isinstance(node,list):
                for child in node:branch_pages(child)
        branch_pages(value)
        if value.get('phase')=='post_transfer' and value.get('account')=='fhemmerrelias' and 'result' in value:
            value.setdefault('request_url','https://api.github.com/repos/'+value['repository']+'/collaborators/fhemmerrelias/permission')
            value['result'].setdefault('user',{'login':'fhemmerrelias'})
        if value.get('phase')=='post_transfer' and value.get('organization')=='hemsoft-dev' and 'plan' in value:
            value.setdefault('request_url','https://api.github.com/orgs/hemsoft-dev');value.setdefault('http_status',200)
            value.setdefault('data',{'id':338855369,'login':'hemsoft-dev','plan':copy.deepcopy(value['plan'])})
        with tempfile.NamedTemporaryFile(dir=directory,suffix='.json',prefix=prefix,delete=False) as stream:
            path=pathlib.Path(stream.name)
        path.write_text(json.dumps(value));self.addCleanup(path.unlink,missing_ok=True)
        return path.name

    def transfer_capture(self, row, timestamp='2026-10-07T01:50:30Z'):
        source=json.loads((DIRECTORY/self.matrix['pre_cutover_source_evidence_url']).read_text())
        current=next(r for a in source['accounts'] for r in a['repositories'] if r['id']==row['repository_id'])
        original=current['source_head']
        policy=json.loads((DIRECTORY/current['protection_evidence_url']).read_text())
        policy.update(phase='pre_transfer')
        policy_at=(validator.observed_time(timestamp,'fixture')-datetime.timedelta(seconds=5)).isoformat()
        self.retime_policy(policy,policy_at)
        policy_evidence=self.capture(policy)
        heads=json.loads(json.dumps(original).replace(row['source'],row['destination']))
        heads.update(phase='post_transfer',repository_id=row['repository_id'],repository=row['destination'],observed_at='2026-10-07T01:50:45Z')
        for page in heads['branches_response']['pages']:page['observed_at']=heads['observed_at']
        heads['tag_refs_response']['observed_at']=heads['observed_at']
        for primary in heads['tag_object_responses'].values():primary['observed_at']=heads['observed_at']
        heads['repository_response']={'method':'GET','http_status':200,'observed_at':heads['observed_at'],
            'request_url':'https://api.github.com/repos/'+row['destination'],'data':{'id':row['repository_id'],'full_name':row['destination']}}
        head_evidence=self.capture(heads)
        milliseconds=int(validator.observed_time(timestamp,'fixture').timestamp()*1000)
        event={'action':'repo.transfer','_document_id':'synthetic-transfer-'+str(row['repository_id']),
            'repo_id':row['repository_id'],'repo':row['destination'],'repo_was':row['source'],
            'org':'hemsoft-dev','org_id':338855369,'actor':'HemSoft','@timestamp':milliseconds,'created_at':milliseconds}
        return self.capture({'phase':'post_transfer','source':row['source'],
            'repository_id':row['repository_id'],'repository':row['destination'],
            'audit_log_url':'https://github.com/organizations/hemsoft-dev/settings/audit-log',
            'observed_at':'2026-10-07T01:50:45Z','event':event,
            'source_protection_evidence_url':policy_evidence,
            'destination_heads_evidence_url':head_evidence,
            'audit_export':self.audit_export_fixture([event],'2026-10-07T01:50:45Z')})

    def retime_policy(self, policy, timestamp):
        if isinstance(policy,dict):
            if 'observed_at' in policy:policy['observed_at']=timestamp
            for value in policy.values():self.retime_policy(value,timestamp)
        elif isinstance(policy,list):
            for value in policy:self.retime_policy(value,timestamp)

    def audit_export_fixture(self, events, timestamp):
        archive=gzip.compress(json.dumps(events).encode(),mtime=0)
        request={'method':'POST','http_status':201,'request_url':'https://github.com/orgs/hemsoft-dev/audit-log/export.json',
            'parameters':{'q':'action:repo.transfer','format':'json'},'observed_at':timestamp,
            'response':{'status_url_sha256':'a'*64,'verify_url_sha256':'b'*64,'export_url_sha256':'c'*64}}
        def response(suffix,digest,parameters):return {'method':'GET','http_status':200,'request_origin':'https://github.com',
            'request_path':'/orgs/hemsoft-dev/audit-log/'+suffix,'query_parameter_names':parameters,
            'request_url_sha256':digest,'observed_at':timestamp}
        return {'organization':'hemsoft-dev','organization_id':338855369,'request':request,
            'status_response':dict(response('export_status','a'*64,['export_id']),body=''),
            'verification_response':dict(response('export','b'*64,['export_id','verify_truncate']),data={'truncated':False}),
            'download_response':dict(response('export','c'*64,['export_id']),content_type='application/gzip',
                body_base64=base64.b64encode(archive).decode(),body_sha256=hashlib.sha256(archive).hexdigest())}

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
            '2026-10-07T03:39:00Z':'2026-10-07T01:55:00Z',
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
        files={path:{'repository_id':row['repository_id'],'repository':row['destination'],
                    'path':path,'revision_sha':proof['revision_sha'],'state':'absent','http_status':404,
                    'contents_response':{'http_status':404,'request_url':'https://api.github.com/repos/'+row['destination']+
                        '/contents/'+path+'?ref='+proof['revision_sha'],'data':{'message':'Not Found'}}}
               for path in ('.sfl/sfl.json','sfl.json')}
        if proof['state']=='present':
            files['.sfl/sfl.json'].update(state='observed',http_status=200,manifest={
                'tier':proof['tier'],'addons':proof['addons'],'components':proof['components']})
            files['.sfl/sfl.json']['contents_response']=self.file_response(row['destination'],proof['revision_sha'],
                '.sfl/sfl.json',json.dumps(files['.sfl/sfl.json']['manifest']).encode())
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

    def file_response(self, repository, revision, path, content):
        blob=hashlib.sha1(b'blob '+str(len(content)).encode()+b'\0'+content).hexdigest()
        return {'http_status':200,'request_url':'https://api.github.com/repos/'+repository+'/contents/'+path+'?ref='+revision,
            'data':{'path':path,'type':'file','encoding':'base64','content':base64.b64encode(content).decode(),
                'size':len(content),'sha':blob,'git_url':'https://api.github.com/repos/'+repository+'/git/blobs/'+blob}}

    def file_contents_capture(self, repository_id, repository, revision, path, content, directory=DIRECTORY):
        return self.capture({'repository_id':repository_id,'repository':repository,'revision_sha':revision,
            'observed_at':'2026-10-07T02:00:00Z','contents_response':self.file_response(repository,revision,path,content)},directory)

    def additional_owner_comment(self, decision, directory):
        comment_id=int(decision['evidence_url'].rsplit('-',1)[1]);owner=decision['evidence_url'].split('/')[3]
        approval={key:decision[key] for key in ('repository_id','repository','visibility','disposition','reason') if key in decision}
        decision['owner_comment_evidence_url']=self.capture({'http_status':200,
            'request_url':'https://api.github.com/repos/'+owner+'/set-it-free-loop/issues/comments/'+str(comment_id),
            'observed_at':decision['approved_at'],'comment':{'id':comment_id,'html_url':decision['evidence_url'],
                'created_at':decision['approved_at'],'updated_at':decision['approved_at'],
                'user':{'id':8227352,'login':'HemSoft','type':'User'},
                'body':'Approved. <!-- sfl-migration-approval:'+json.dumps(approval)+' -->'}},directory)

    def bind_smoke(self, row, directory=DIRECTORY):
        row.update(smoke_outcome='success',smoke_phase='post_transfer')
        repository=row['source'] if int(row['repository_id']) in validator.APPROVED_RETAINED_IDS else row['destination']
        smoke={'repository_id':int(row['repository_id']),'repository':repository,'provider':row['provider'],
            'resource_kind':row.get('resource_kind'),'resource_id':row.get('resource_id'),'outcome':'success',
            'phase':'post_transfer','destructive_changes':False,'continuity_verified':True,
            'observed_at':'2026-10-07T02:00:00Z','provider_responses':{}}
        kind,resource=row.get('resource_kind'),row.get('resource_id')
        def response(name,url,data,complete=False):
            smoke['provider_responses'][name]={'method':'GET','request_url':url,'http_status':200,
                'observed_at':smoke['observed_at'],'data':data}
            if complete:smoke['provider_responses'][name]['all_pages']=True
        if kind=='vercel_project':
            project=copy.deepcopy(next(r for r in json.loads((directory/'vercel-provider-evidence.json').read_text())['projects'] if r['id']==resource))
            if resource=='prj_hPjAbxtMlCi3A5waKxQpjATto0ae':project['link']=None
            else:project['link'].update(org=repository.split('/')[0],repo=repository.split('/')[1])
            response('project','https://api.vercel.com/v9/projects/'+resource+'?teamId='+project['accountId'],project)
        elif kind=='github_pages':
            baseline=validator.provider_preservation_baseline(row,self.inventory,directory)
            owner,name=repository.split('/');site='https://'+(baseline['cname']+'/' if baseline['cname'] else owner.lower()+'.github.io/'+name+'/')
            response('pages','https://api.github.com/repos/'+repository+'/pages',dict(baseline,html_url=site))
            response('site',site,{'final_url':site})
        elif kind=='cloudflare_zone':
            baseline=validator.provider_preservation_baseline(row,self.inventory,directory)
            base='https://api.cloudflare.com/client/v4/zones/'+resource
            response('zone',base,{'success':True,'result':{'id':resource,'account':{'id':baseline['account_id']},'plan':{'name':baseline['plan']}}})
            response('dns',base+'/dns_records?per_page=100',{'success':True,'result':baseline['website_records']},True)
            response('routes',base+'/workers/routes',{'success':True,'result':baseline['workers_routes']})
        elif kind=='cloudflare_worker':
            baseline=validator.provider_preservation_baseline(row,self.inventory,directory)
            base='https://api.cloudflare.com/client/v4/accounts/'+baseline['account_id']+'/workers/scripts'
            response('scripts',base,{'success':True,'result':[{'id':resource}]},True)
            response('subdomain',base+'/'+resource+'/subdomain',{'success':True,'result':{'enabled':True}})
            response('account_subdomain','https://api.cloudflare.com/client/v4/accounts/'+baseline['account_id']+'/workers/subdomain',{'success':True,'result':{'subdomain':'nlg'}})
            zone=json.loads((directory/'now-leadership-live-hosting-evidence.json').read_text())['cloudflare_owner_verification']['zone_id']
            response('routes','https://api.cloudflare.com/client/v4/zones/'+zone+'/workers/routes',{'success':True,'result':baseline['workers_routes']})
            smoke['observed_resource']=baseline
        elif kind=='repository_runner':
            if int(row['repository_id'])==validator.SURVIVAL_REPOSITORY_ID:
                repo=next(repo for repo in self.inventory['repositories'] if repo['id']==validator.SURVIVAL_REPOSITORY_ID)
                runner=copy.deepcopy(validator.reviewed_survival_resources(repo,directory)['runner'])
                runner.update(status='online',busy=False)
            else:
                runner={'id':int(resource),'name':'mini-github-runner-01','status':'online','busy':False,
                        'labels':[{'name':name} for name in ('self-hosted','Linux','X64','mini','yahtzee')]}
            response('runner','https://api.github.com/repos/'+repository+'/actions/runners/'+str(resource),runner)
        elif kind=='supabase_project':
            baseline=validator.provider_preservation_baseline(row,self.inventory,directory)
            row['smoke_outcome']=smoke['outcome']='baseline_preserved'
            smoke.update(baseline=baseline,observed_resource=copy.deepcopy(baseline),resource_unchanged=True,runtime_actions=[],
                owner_receipt_url='https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-6030145370')
            response('project','https://api.supabase.com/v1/projects/'+resource,{'id':'synthetic-project-id','ref':resource,'name':baseline['name'],
                'region':baseline['region'],'organization_id':'synthetic-org-id','organization_slug':baseline['organization'],'status':'INACTIVE'})
        row['smoke_evidence_url']=self.capture(smoke,directory)

    def bind_source_scan(self, accounts):
        original=json.loads((DIRECTORY/'source-tree-recheck-evidence.json').read_text())
        unresolved={r['repository_id']:r for r in original['records']}
        scans=[]
        for account in accounts:
            for repo in account['repositories']:
                baseline=unresolved.get(repo['id'],{})
                head=None if baseline.get('state')=='uninitialized' else baseline.get('commit_sha','e'*40)
                tree=None if head is None else baseline.get('tree_sha','f'*40)
                branches=copy.deepcopy(baseline.get('branches',[{'name':repo['default_branch'],'commit':{'sha':head}}]))
                base='https://api.github.com/repos/'+repo['full_name']
                data={'state':'observed' if head else 'uninitialized','head_sha':head,'tree_sha':tree,
                    'observed_at':'2026-10-07T01:48:00Z','branches_response':{'request_url':base+'/branches?per_page=100',
                        'http_status':200,'all_pages':True,'data':branches},
                    'ref_response':{'request_url':base+'/git/ref/heads/'+repo['default_branch'],'http_status':200 if head else 409,
                        'data':{'ref':'refs/heads/'+repo['default_branch'],'object':{'type':'commit','sha':head}}},
                    'commit_response':{'request_url':base+'/git/commits/'+str(head),'http_status':200,
                        'data':{'sha':head,'tree':{'sha':tree}}}}
                scans.append(dict(copy.deepcopy(data),repository_id=repo['id'],source=repo['full_name'],files=[],
                    referenced_secret_names=[],branch_scans=[],tree_response={'request_url':base+'/git/trees/'+str(tree)+'?recursive=1',
                    'http_status':200,'data':{'sha':tree,'truncated':False,'tree':[]}}))
                for other_head in sorted({branch['commit']['sha'] for branch in branches}-{head}):
                    scans[-1]['branch_scans'].append({'head_sha':other_head,'tree_sha':tree,
                        'commit_response':{'request_url':base+'/git/commits/'+other_head,'http_status':200,
                            'data':{'sha':other_head,'tree':{'sha':tree}}},
                        'tree_response':{'request_url':base+'/git/trees/'+str(tree)+'?recursive=1','http_status':200,
                            'data':{'sha':tree,'truncated':False,'tree':[]}},'files':[]})
                repo['source_head']=dict(data,observed_at='2026-10-07T01:49:30Z')
                repo['protection_evidence_url']=self.capture({'phase':'pre_cutover','repository_id':repo['id'],
                    'repository':repo['full_name'],'revision_sha':head,'observed_at':'2026-10-07T01:49:30Z',
                    **copy.deepcopy(repo['protections'])})
                for previous in json.loads((DIRECTORY/'workflow-reference-evidence.json').read_text())['records']:
                    if previous['repository_id']!=repo['id'] or previous.get('state')!='observed' or 'manifest' not in previous:continue
                    name=previous['path'];body=json.dumps(previous['manifest']).encode()
                    file=json.loads((DIRECTORY/self.file_contents_capture(repo['id'],repo['full_name'],head,name,body)).read_text())
                    file.update(path=name,observed_at=data['observed_at'],manifest=previous['manifest'])
                    scans[-1]['files'].append(file)
                    scans[-1]['tree_response']['data']['tree'].append({'path':name,'type':'blob','sha':file['contents_response']['data']['sha']})
                if repo['id'] == validator.SURVIVAL_REPOSITORY_ID:
                    sealed=next(r for r in self.inventory['repositories'] if r['id']==repo['id'])
                    name='.github/workflows/ci.yml';body=validator.reviewed_survival_resources(sealed,DIRECTORY)['workflow_bytes']
                    file=json.loads((DIRECTORY/self.file_contents_capture(repo['id'],repo['full_name'],head,name,body)).read_text())
                    file.update(path=name,observed_at=data['observed_at'])
                    scans[-1]['files'].append(file)
                    scans[-1]['tree_response']['data']['tree'].append({'path':name,'type':'blob','sha':file['contents_response']['data']['sha']})
        return self.capture({'phase':'pre_cutover','observed_at':'2026-10-07T01:48:00Z','repositories':scans})

    def workflow_contents(self, repository, revision):
        content=(ROOT/'deployment/infrastructure/sfl-pr-review-auto.yml').read_bytes()
        blob=hashlib.sha1(b'blob '+str(len(content)).encode()+b'\0'+content).hexdigest()
        return {'content':content.decode(),'contents_response':{'http_status':200,
            'request_url':'https://api.github.com/repos/'+repository+'/contents/.github/workflows/sfl-pr-review-auto.yml?ref='+revision,
            'data':{'path':'.github/workflows/sfl-pr-review-auto.yml','type':'file','encoding':'base64',
                'content':base64.b64encode(content).decode(),'size':len(content),'sha':blob,
                'git_url':'https://api.github.com/repos/'+repository+'/git/blobs/'+blob}}}

    def bind_attestation(self, download, directory=DIRECTORY):
        folder=tempfile.TemporaryDirectory();self.addCleanup(folder.cleanup)
        asset=pathlib.Path(folder.name)/download['asset_name'];asset.write_bytes(b'synthetic regression asset '+download['asset_name'].encode())
        download['expected_sha256']=download['actual_sha256']=hashlib.sha256(asset.read_bytes()).hexdigest()
        source=download['source_repository'];version=download['release_version'];purl='pkg:github/'+source+'@v'+version
        statement={'_type':'https://in-toto.io/Statement/v1','predicateType':'https://in-toto.io/attestation/release/v0.2',
            'subject':[{'uri':purl,'digest':{'sha1':download['source_sha']}},
                {'name':download['asset_name'],'digest':{'sha256':download['expected_sha256']}}],
            'predicate':{'repository':source,'repositoryId':'1169772257','tag':'v'+version,'purl':purl,'databaseId':'1'}}
        result={'attestation':{'bundle':{'dsseEnvelope':{'payloadType':'application/vnd.in-toto+json',
            'payload':base64.b64encode(json.dumps(statement).encode()).decode(),'signatures':[{'sig':'synthetic-regression-signature'}]}}},
            'verificationResult':{'signature':{'certificate':{'subjectAlternativeName':'https://dotcom.releases.github.com'}},
                'verifiedTimestamps':[{'timestamp':download['observed_at']}],'statement':statement}}
        asset_path=str(asset)
        self.release_results[tuple(['gh','release','verify-asset','v'+version,asset_path,'--repo',source,'--format','json'])]=copy.deepcopy(result)
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
        capture['run_response']['data']=copy.deepcopy(run);capture['check_response']['data']=copy.deepcopy(check)
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
        path=pathlib.Path(self.capture(capture,directory,prefix='fixture-gate-'))
        return {'state':'required','context':'SFL Reviewer Gate Runner','app_id':15368,'strict':True,
                'repository_id':repository_id,'repository':repository,'branch':self.branch(repository_id),'evidence_url':path.name}

    def complete_provider(self):
        row = next(r for r in self.rows if r['resource_kind']=='vercel_project' and r['resource_id']=='prj_ee45BuJUXLIUX7vbxZM2Nos2CYHv')
        row.update(status='verified', provider='Vercel', resource_owner='HemSoft',
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
                'number':int(operation['evidence_url'].rsplit('/',1)[1]),
                'merge_commit_sha':operation['revision_after'],'created_at':timestamp,'merged_at':timestamp,
                'base':{'repo':{'id':operation['repository_id'],'full_name':operation['repository']}}}}
        elif operation['outcome']=='no_changes':
            result={'change_count':0,'revision_before':operation['revision_before'],'revision_after':operation['revision_after']}
        elif operation['outcome']=='healthy':
            result={'health':'healthy','revision_sha':operation['revision_after'],'missing_files':[],'drifted_files':[]}
        elif operation['outcome']=='gate_removed':
            result={'gate_required':False,'unrelated_change_count':0,'preserved_unrelated_rules':True}
        capture={**operation,'status':'completed','exit_code':0,
            'started_at':timestamp,'observed_at':timestamp,'result':result}
        if operation['outcome']=='pull_request_merged':
            capture['pull_request_response']={'method':'GET','http_status':200,'observed_at':timestamp,
                'request_url':'https://api.github.com/repos/'+operation['repository']+'/pulls/'+
                    str(result['pull_request']['number']),'data':copy.deepcopy(result['pull_request'])}
        else:
            stdout=('SFL '+operation['command']+' is already up to date; no pull request needed\n' if
                operation['outcome']=='no_changes' else 'SFL Status: '+operation['repository']+'\n' if
                operation['outcome']=='healthy' else 'Removed designated SFL reviewer gate\n')
            argv=['gh','sfl',operation['command'],'--repo',operation['repository']]
            command='gh sfl '+operation['command']
            if operation['outcome']=='no_changes':argv.append('--pr')
            elif operation['outcome']=='gate_removed':
                command='gh api';argv=['gh','api','--method','DELETE','repos/'+operation['repository']+'/rulesets/1']
            capture['execution']={'command':command,'argv':argv,
                'exit_code':0,'started_at':timestamp,'completed_at':timestamp,
                'stdout':stdout,'stdout_sha256':hashlib.sha256(stdout.encode()).hexdigest()}
        operation['capture_evidence_url']=self.capture(capture,directory)

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
            base='https://api.github.com/repos/'+repository
            if 'commit_status' in value:
                value['status_pages']=[{'method':'GET','http_status':200,'request_url':base+'/commits/'+row['review_head_sha']+'/statuses?per_page=100',
                    'response_headers':{},'observed_at':timestamp,'data':[copy.deepcopy(value['commit_status'])]}]
            else:
                data=value.get('comment',value.get('artifact'))
                value.update(method='GET',http_status=200,request_url=base+'/issues/comments/'+str(data['id']),data=copy.deepcopy(data))
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
            'method':'GET','request_url':'https://api.github.com/repos/'+repository+'/collaborators/'+row['review_requester']+'/permission',
            'data':{'permission':'admin','role_name':'admin'},
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
                    'target_type':'Organization','repository_selection':'all','suspended_at':None,
                    'permissions':copy.deepcopy(self.inventory['known_owned_app']['data']['permissions'])}},directory)}

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
        actual=dict(decision,evidence_url=receipt['url']);self.additional_owner_comment(actual,DIRECTORY)
        row['exception_evidence_url']=self.capture(dict(decision,owner_comment=receipt,
            owner_comment_evidence_url=actual['owner_comment_evidence_url'],observed_at='2026-10-07T02:00:00Z'))
        decision['evidence_url']=row['exception_evidence_url']

    def complete_transfer_gates(self):
        self.matrix['pre_transfer_credential_verification'] = {'repository_id':1169772257,
            'repository':'HemSoft/set-it-free-loop','workflow':'.github/workflows/verify-sfl-app-credential.yml',
            'conclusion':'success','reviewed_sha':'e'*40,'app_id':4448946,'client_id':'Iv23liwvwJJUh2bUIKLW',
            'owner':'HemSoft','installation_id':150383874,'repository_selection':'all','permission_ceiling_verified':True,
            'run_url':'https://github.com/HemSoft/set-it-free-loop/actions/runs/1'}
        proof=self.matrix['pre_transfer_credential_verification']
        proof['implementation_evidence']={path:self.capture({'repository_id':proof['repository_id'],
            'repository':proof['repository'],'revision_sha':proof['reviewed_sha'],'observed_at':'2026-10-07T01:48:25Z',
            'contents_response':self.file_response(proof['repository'],proof['reviewed_sha'],path,(ROOT/path).read_bytes())})
            for path in (proof['workflow'],'deployment/scripts/SflGitHubAppBootstrap.psm1')}
        proof['credential_metadata_evidence_url']=self.capture({**{field:proof[field] for field in
            ('repository_id','repository','reviewed_sha','run_url','app_id','client_id','owner',
             'installation_id','repository_selection','permission_ceiling_verified')},
            'credential_verification':'success','installation_owner':'HemSoft','target_type':'User',
            'observed_at':'2026-10-07T01:48:10Z'})
        proof['workflow_run_evidence_url']=self.capture({'repository':{'id':proof['repository_id'],'full_name':proof['repository']},
            'html_url':proof['run_url'],'head_sha':proof['reviewed_sha'],'path':proof['workflow'],
            'head_branch':'main','event':'workflow_dispatch','id':1,'run_started_at':'2026-10-07T01:48:05Z','status':'completed','conclusion':'success',
            'created_at':'2026-10-07T01:48:05Z','updated_at':'2026-10-07T01:48:20Z','captured_at':'2026-10-07T01:48:25Z'})
        metadata=json.loads((DIRECTORY/proof['credential_metadata_evidence_url']).read_text())
        stream=io.BytesIO()
        with zipfile.ZipFile(stream,'w') as archive:archive.writestr('sfl-app-credential-metadata.json',json.dumps(metadata))
        url='https://api.github.com/repos/HemSoft/set-it-free-loop/actions/artifacts/1'
        proof['credential_artifact_evidence_url']=self.capture({'request_url':url,'download_url':url+'/zip',
            'observed_at':'2026-10-07T01:48:25Z','archive_base64':base64.b64encode(stream.getvalue()).decode(),
            'artifact':{'id':1,'name':'sfl-app-credential-metadata','expired':False,'archive_download_url':url+'/zip',
                'workflow_run':{'id':1,'head_sha':proof['reviewed_sha']},
                'created_at':'2026-10-07T01:48:12Z','updated_at':'2026-10-07T01:48:12Z',
                'digest':'sha256:'+hashlib.sha256(stream.getvalue()).hexdigest()}})
        accounts = [{'owner':owner,'state':'observed','all_pages':True,'repositories':[
            {**{k:repo[k] for k in ('id','full_name','private','visibility','archived','default_branch')},
             'protections':validator.protection_contract(repo)} for repo in self.inventory['repositories']
            if repo['full_name'].startswith(owner+'/')]} for owner in ('HemSoft','fhemmer')]
        for account in accounts:
            for current in account['repositories']:
                baseline=next(repo for repo in self.inventory['repositories'] if repo['id']==current['id'])
                current['secret_pages']=[{'method':'GET','http_status':200,
                    'request_url':'https://api.github.com/repos/'+current['full_name']+'/actions/secrets?per_page=100',
                    'response_headers':{},'observed_at':'2026-10-07T01:49:30Z','data':{
                        'total_count':len(baseline['settings']['secret_names']['data']),
                        'secrets':[{'name':name} for name in baseline['settings']['secret_names']['data']]}}]
                environments=validator.reviewed_environment_secrets(baseline,DIRECTORY)
                current['environment_pages']=[{'method':'GET','http_status':200,
                    'request_url':'https://api.github.com/repos/'+current['full_name']+'/environments?per_page=100',
                    'response_headers':{},'observed_at':'2026-10-07T01:49:30Z',
                    'data':{'total_count':len(environments),'environments':[{'name':name} for name in environments]}}]
                current['environment_secret_pages']={name:[{'method':'GET','http_status':200,
                    'request_url':'https://api.github.com/repos/'+current['full_name']+'/environments/'+
                        urllib.parse.quote(name,safe='')+'/secrets?per_page=100',
                    'response_headers':{},'observed_at':'2026-10-07T01:49:30Z',
                    'data':{'total_count':len(names),'secrets':[{'name':key} for key in sorted(names)]}}]
                    for name,names in environments.items()}
                runners=[runner for item in json.loads((DIRECTORY/'runtime-metadata.json').read_text())['repositories']
                    if item['source']==current['full_name'] for runner in (item['repository_runners'].get('data') or [])]
                if current['id']==validator.SURVIVAL_REPOSITORY_ID:
                    survival=validator.reviewed_survival_resources(baseline,DIRECTORY)
                    runners=[copy.deepcopy(survival['runner'])]
                    variable=copy.deepcopy(survival['capture']['variable_response'])
                    variable['observed_at']='2026-10-07T01:49:30Z'
                    current['variable_responses']={'UE_RUNNER_ENABLED':variable}
                current['runner_pages']=[{'method':'GET','http_status':200,
                    'request_url':'https://api.github.com/repos/'+current['full_name']+'/actions/runners?per_page=100',
                    'response_headers':{},'observed_at':'2026-10-07T01:49:30Z','data':{'total_count':len(runners),'runners':runners}}]
        self.matrix['pre_cutover_source_evidence_url']=self.capture({'phase':'pre_cutover',
            'observed_at':'2026-10-07T01:50:00Z','accounts':accounts,
            'reference_scan_evidence_url':self.bind_source_scan(accounts),
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
                unit='actions.runner.HemSoft-yahtzee.mini-github-runner-01.service'
                runner['service_evidence_url']=self.capture(dict(common,unit=unit,active_state='active',
                    argv=['systemctl','show',unit,'--property=Id,ActiveState','--no-pager'],
                    systemctl_show={'Id':unit,'ActiveState':'active'}))
                runner['smoke_job_id']=1
                runner['jobs_evidence_url']=self.capture({'request_url':'https://api.github.com/repos/'+repo['destination']+
                    '/actions/runs/1/attempts/1/jobs?per_page=100','all_pages':True,'total_count':1,
                    'observed_at':common['observed_at'],'jobs':[{'id':1,'run_id':1,'run_attempt':1,
                        'head_sha':runner['run_head_sha'],'runner_id':21,'runner_name':'mini-github-runner-01',
                        'labels':['self-hosted','Linux','X64','mini','yahtzee'],'status':'completed','conclusion':'success',
                        'started_at':common['observed_at'],'completed_at':common['observed_at']}]})
                runner['run_evidence_url']=self.capture(dict(common,read_only=True,run={'id':1,'run_attempt':1,'repository':{'id':repo['id'],
                    'full_name':repo['destination']},'html_url':runner['run_url'],'head_sha':runner['run_head_sha'],
                    'path':'.github/workflows/self-hosted-smoke.yml','event':'workflow_dispatch',
                    'status':'completed','conclusion':'success','created_at':'2026-10-07T02:00:00Z','updated_at':'2026-10-07T02:00:00Z'}))
                runner['workflow_evidence_url']=self.capture({'repository_id':repo['id'],'repository':repo['destination'],
                    'revision_sha':runner['run_head_sha'],'observed_at':common['observed_at'],
                    'contents_response':self.file_response(repo['destination'],runner['run_head_sha'],
                        '.github/workflows/self-hosted-smoke.yml',(DIRECTORY/'yahtzee-smoke-workflow.yml').read_bytes())})
            if repo['id']==validator.SURVIVAL_REPOSITORY_ID:
                self.bind_windows_runner(row,repo)
            if row['health'] == 'source_verified':
                continue
            row['destination_protections'] = dict(validator.protection_contract(repo),
                repository_id=repo['id'],repository=repo['destination'],revision_sha='e'*40,
                observed_at='2026-10-07T02:00:00Z')
            row['destination_protections']['evidence_url']=self.capture(dict(row['destination_protections'],phase='post_transfer'))
            if row['archived'] and row['repository_id'] not in validator.APPROVED_RETAINED_IDS:self.archived_status(row)
        for repo in self.inventory['repositories']:
            self.verify_source_ledger(repo['id'])
        self.bind_ledger_readiness()

    def bind_windows_runner(self,row,repo):
        resources=validator.reviewed_survival_resources(repo,DIRECTORY)
        actual=copy.deepcopy(resources['runner']);actual.update(status='online',busy=False)
        at='2026-10-07T02:00:00Z'
        common={'phase':'post_transfer','observed_at':at,'repository_id':repo['id'],
                'repository':repo['destination'],'runner_id':10}
        proof={'repository_id':repo['id'],'repository':repo['destination'],'runner_id':10,
               'online':True,'idle':True,'startup_model':'windows_logon_task','startup_active':True,
               'run_conclusion':'success','run_head_sha':'e'*40,'smoke_job_id':10,
               'run_url':'https://github.com/'+repo['destination']+'/actions/runs/10'}
        proof['registration_evidence_url']=self.capture(dict(common,runner=actual,
            runner_response={'method':'GET','http_status':200,'observed_at':at,
                'request_url':'https://api.github.com/repos/'+repo['destination']+'/actions/runners/10',
                'data':copy.deepcopy(actual)}))
        host=copy.deepcopy(resources['capture']['host_capture']);host['observed_at']=at
        host['data']['configurations'][0]['gitHubUrl']='https://github.com/'+repo['destination']
        task=copy.deepcopy(resources['capture']['startup_capture']);task['observed_at']=at
        proof['startup_evidence_url']=self.capture(dict(common,host_capture=host,startup_capture=task))
        proof['jobs_evidence_url']=self.capture({'request_url':'https://api.github.com/repos/'+repo['destination']+
            '/actions/runs/10/attempts/1/jobs?per_page=100','all_pages':True,'total_count':1,'observed_at':at,
            'jobs':[{'id':10,'name':'Build and simulation tests','run_id':10,'run_attempt':1,
                'head_sha':proof['run_head_sha'],'runner_id':10,'runner_name':actual['name'],
                'labels':[label['name'] for label in actual['labels']],'status':'completed','conclusion':'success',
                'started_at':at,'completed_at':at}]})
        proof['run_evidence_url']=self.capture(dict(common,verification_mode='existing_build_and_simulation_tests',
            run={'id':10,'run_attempt':1,'repository':{'id':repo['id'],'full_name':repo['destination']},
                'html_url':proof['run_url'],'head_sha':proof['run_head_sha'],'head_branch':repo['default_branch'],
                'path':'.github/workflows/ci.yml@'+repo['default_branch'],'event':'workflow_dispatch',
                'status':'completed','conclusion':'success','created_at':at,'updated_at':at}))
        proof['workflow_evidence_url']=self.capture({'repository_id':repo['id'],'repository':repo['destination'],
            'revision_sha':proof['run_head_sha'],'observed_at':at,'contents_response':self.file_response(
                repo['destination'],proof['run_head_sha'],'.github/workflows/ci.yml',resources['workflow_bytes'])})
        row['post_transfer_runner']=proof

    def bind_ledger_readiness(self):
        rows=copy.deepcopy(self.rows)
        for row in rows:
            if row['status']!='verified':continue
            row['verified_at']='2026-10-07T01:49:00Z'
            if row['provider']!='none':
                smoke=json.loads((DIRECTORY/row['smoke_evidence_url']).read_text())
                smoke.update(phase='pre_transfer',repository=row['source'],observed_at='2026-10-07T01:49:00Z')
                for response in smoke.get('provider_responses',{}).values():
                    response['observed_at']=smoke['observed_at']
                    response['request_url']=response['request_url'].replace(row['destination'],row['source'])
                if row['resource_kind']=='vercel_project' and smoke['provider_responses']['project']['data'].get('link'):
                    smoke['provider_responses']['project']['data']['link'].update(org=row['source'].split('/')[0],repo=row['source'].split('/')[1])
                if row['resource_kind']=='github_pages':
                    old_site=smoke['provider_responses']['pages']['data']['html_url']
                    new_site=old_site.replace('hemsoft-dev.github.io',row['source'].split('/')[0].lower()+'.github.io')
                    smoke['provider_responses']['pages']['data']['html_url']=new_site
                    smoke['provider_responses']['site'].update(request_url=new_site,data={'final_url':new_site})
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
        capture['run_response']={'method':'GET','http_status':200,
            'request_url':'https://api.github.com/repos/'+pilot['repository']+'/actions/runs/'+str(run_id),
            'observed_at':capture['observed_at'],'data':copy.deepcopy(capture['run'])}
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
            'phase':'post_transfer','observed_at':'2026-10-07T01:51:00Z',
            'request_url':'https://api.github.com/apps/sfl-app','http_status':200,'app':{'id':4448946,
            'client_id':'Iv23liwvwJJUh2bUIKLW','owner':{'id':338855369,'login':'hemsoft-dev','type':'Organization'},
            'permissions':self.inventory['known_owned_app']['data']['permissions']}}))
        path=DIRECTORY/'owned-app-organization-installation-evidence.json'
        if not hasattr(self,'original_owned_installation_capture'):
            self.original_owned_installation_capture=path.read_text()
            self.addCleanup(path.write_text,self.original_owned_installation_capture)
        path.write_text(json.dumps({'verification_status':'verified','account':{'login':'hemsoft-dev'},
            'phase':'post_transfer','observed_at':'2026-10-07T01:52:00Z',
            'installation':{'id':123,'app_id':4448946,'repository_selection':'all',
                'permissions':copy.deepcopy(self.inventory['known_owned_app']['data']['permissions'])}}))

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
        repo=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id'])
        row['default_branch_evidence_url']=self.capture({'phase':'post_transfer','method':'GET',
            'repository_id':repo['id'],'repository':row['destination'],'branch':repo['default_branch'],
            'http_status':200,'request_url':'https://api.github.com/repos/'+row['destination']+'/git/ref/heads/'+repo['default_branch'],
            'observed_at':'2026-10-07T03:40:00Z','data':{'ref':'refs/heads/'+repo['default_branch'],
                'object':{'type':'commit','sha':'b'*40}}})
        self.matrix['summary']['verified_rollouts'] += 1
        self.pilots_before_rollout()
        return row

    def complete_pilots(self):
        self.complete_transfer_gates()
        self.complete_app_transfer()
        self.complete_source_in_place()
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
                    'release_version':receipts['release_version'],
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
            before=json.loads((DIRECTORY/receipts['gate_policy']['evidence_url']).read_text())
            self.retime_policy(before,'2026-10-07T03:39:00Z')
            receipts['pre_cleanup_gate_policy_evidence_url']=self.capture(before)
            receipts['final_gate_policy_evidence_url']=self.capture({'repository_id':pilot['repository_id'],
                'repository':pilot['repository'],'branch':self.branch(pilot['repository_id']),
                'observed_at':'2026-10-07T03:41:00Z','classic_protection':{'state':'absent','http_status':404},
                'effective_rules':{'state':'observed','data':[]}})


    def complete_source(self):
        self.complete_pilots()
        row = next(row for row in self.matrix['repositories'] if row['source'] == 'HemSoft/set-it-free-loop')
        self.pilots_before_rollout()
        return row

    def complete_source_in_place(self):
        row = next(row for row in self.matrix['repositories'] if row['source'] == 'HemSoft/set-it-free-loop')
        if row['health'] == 'source_verified':
            return row
        self.complete_transfer_gates()
        self.complete_app_transfer()
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
        self.bind_review_operations(row,revision=row['in_place_evidence']['source_sha'],timestamp='2026-10-07T01:52:00Z')
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
        # Source verification finishes first. Re-time only the synthetic source
        # captures; the freshly generated registration already hashes this time.
        def retime(value):
            if isinstance(value,dict):return {key:retime(item) for key,item in value.items()}
            if isinstance(value,list):return [retime(item) for item in value]
            if isinstance(value,str) and value.startswith('2026-10-07T'):
                try:
                    at=datetime.datetime.fromisoformat(value.replace('Z','+00:00'))
                    for old,new in [('03:40:00','01:52:03'),('03:30:00','01:52:02'),('02:00:01','01:52:01'),('02:00:00','01:52:00')]:
                        if at >= datetime.datetime.fromisoformat('2026-10-07T'+old+'+00:00'):
                            return '2026-10-07T'+new+'Z'
                except ValueError:pass
            return value
        changed=retime(row);row.clear();row.update(changed)
        references=set()
        def collect(value):
            if isinstance(value,dict):
                for key,item in value.items():
                    if key.endswith(('evidence_url','verification_url')) and isinstance(item,str) and not item.startswith('https://'):
                        references.add(item)
                    elif isinstance(item,(dict,list)):collect(item)
            elif isinstance(value,list):
                for item in value:collect(item)
        collect(row);pending=list(references);done=set()
        while pending:
            reference=pending.pop()
            if reference in done:continue
            done.add(reference);path=DIRECTORY/reference
            data=retime(json.loads(path.read_text()))
            path.write_text(json.dumps(data));collect(data);pending.extend(references-done)
        return row

    def test_protected_source_can_complete_without_consumer_manifest(self):
        row = self.complete_source()
        self.assertIsNone(row['installed_tier'])
        self.assertIsNone(row['manifest_version'])
        self.assertEqual(self.check()['verified_rollouts'], 1)

    def test_source_in_place_verification_can_precede_pending_pilots(self):
        self.complete_source_in_place()
        self.assertTrue(all(p['validation_status']=='pending' for p in self.matrix['disposable_validation_repositories']))
        self.assertEqual(self.check()['verified_rollouts'],1)

    def test_source_verification_finishes_before_either_pilot_starts(self):
        self.complete_source();self.check()
        for pilot in self.matrix['disposable_validation_repositories']:
            operation=pilot['validation_evidence']['operation_receipts']['init_pr_url']
            path=DIRECTORY/operation['capture_evidence_url'];original=json.loads(path.read_text())
            capture=copy.deepcopy(original);capture['started_at']='2026-10-07T01:52:03Z'
            path.write_text(json.dumps(capture))
            with self.subTest(visibility=pilot['visibility']),self.assertRaisesRegex(ValueError,'Protected source must finish'):
                self.check()
            path.write_text(json.dumps(original))

    def test_pilots_cannot_retroactively_qualify_late_or_missing_source_verification(self):
        source=self.complete_source();self.check()
        path=DIRECTORY/source['in_place_evidence']['default_branch_evidence_url'];original=json.loads(path.read_text())
        for timestamp in ['2026-10-07T01:53:00Z','2026-10-07T03:42:00Z']:
            capture=copy.deepcopy(original);capture['observed_at']=timestamp;path.write_text(json.dumps(capture))
            with self.subTest(timestamp=timestamp),self.assertRaisesRegex(ValueError,'Protected source must finish'):
                self.check()
        path.write_text(json.dumps(original))
        source['health']='pending_transfer';self.matrix['summary']['verified_rollouts']-=1
        with self.assertRaisesRegex(ValueError,'Protected source must finish'):self.check()

    def test_pilot_init_start_is_required_and_bounded_by_its_actual_operation(self):
        self.complete_source();self.check()
        operation=self.matrix['disposable_validation_repositories'][0]['validation_evidence']['operation_receipts']['init_pr_url']
        path=DIRECTORY/operation['capture_evidence_url'];original=json.loads(path.read_text())
        for timestamp in [None,'2026-10-07T01:49:00Z','2026-10-07T04:00:00Z','2026-10-07T01:53:00']:
            capture=copy.deepcopy(original)
            if timestamp is None:del capture['started_at']
            else:capture['started_at']=timestamp
            path.write_text(json.dumps(capture))
            with self.subTest(timestamp=timestamp),self.assertRaisesRegex(ValueError,'Pilot init execution start'):
                self.check()
        path.write_text(json.dumps(original));self.check()

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
        row=self.complete_provider();index=self.rows.index(row)
        complete = copy.deepcopy(row)
        fields = ('resource_owner', 'resource_url', 'billing_dependency', 'credential_source',
                  'credential_validity', 'affected_reference', 'transfer_action', 'smoke_test',
                  'recovery_action', 'verified_by', 'verified_at', 'evidence_url')
        for field in fields:
            with self.subTest(field=field):
                self.rows[index] = copy.deepcopy(complete)
                self.rows[index][field] = ''
                with self.assertRaises(ValueError):
                    self.check()
        self.rows[index] = complete
        self.check()

    def test_verified_absence_requires_reason(self):
        row=self.rows[0];row.update(status='verified',verified_by='HemSoft',evidence_url=validator.EXTERNAL_SCOPE_RECEIPT,verified_at='2026-10-07T02:00:00Z')
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
        self.complete_transfer_gates()
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
        with self.assertRaisesRegex(ValueError,'(?:pilot requires completed owned|requires verified) App transfer'): self.check()
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
        capture=self.bind_final_pages({'observed_at':'2026-10-07T03:00:00Z','accounts':list(accounts.values())})
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
        row=self.rows[0];row.update(status='verified',verified_by='HemSoft')
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
            (directory/'capture.json').write_text(json.dumps(self.bind_final_pages(capture)))
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
            path=directory/reference;capture=json.loads(path.read_text());capture['observed_at']='2026-10-07T03:30:00Z'
            if field=='gate_policy':
                for response in ('effective_rules','classic_protection'):capture[response]['observed_at']=capture['observed_at']
            path.write_text(json.dumps(capture))
        gate_path=directory/proof['review_operation_receipts']['gate_run_url']['capture_evidence_url']
        gate_capture=json.loads(gate_path.read_text());gate_capture['observed_at']='2026-10-07T03:30:00Z'
        gate_capture['run'].update(created_at='2026-10-07T03:30:00Z',updated_at='2026-10-07T03:30:00Z')
        gate_capture['check_run'].update(started_at='2026-10-07T03:30:00Z',completed_at='2026-10-07T03:30:00Z');gate_path.write_text(json.dumps(gate_capture))
        proof['manifest_evidence_url']='post-status-manifest.json'
        (directory/proof['manifest_evidence_url']).write_text(json.dumps({'repository_id':42,'repository':name,'revision_sha':'f'*40,
            'manifest':proof['manifest_identity'],'manifest_path':'.sfl/sfl.json','observed_at':'2026-10-07T03:30:00Z',
            'contents_response':self.file_response(name,'f'*40,'.sfl/sfl.json',json.dumps(proof['manifest_identity']).encode())}))
        capture={'request_url':'https://api.github.com/repos/'+name,'http_status':200,
            'observed_at':'2026-10-07T03:05:00Z',
            'metadata':{'id':42,'full_name':name,'private':True,'visibility':'private','archived':False,'created_at':'2026-10-07T03:00:00Z','default_branch':'main'}}
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
        policy=json.loads((DIRECTORY/'codex-organization-installation-evidence.json').read_text())
        policy['observed_at']='2026-10-07T03:35:00Z'
        for page in policy['installation_pages']:page['observed_at']=policy['observed_at']
        proof['codex_installation_policy_evidence_url']=self.capture(policy,directory)
        proof['default_branch_evidence_url']=self.capture({'phase':'post_transfer','method':'GET','repository_id':42,
            'repository':name,'branch':'main','http_status':200,'request_url':'https://api.github.com/repos/'+name+'/git/ref/heads/main',
            'observed_at':'2026-10-07T03:40:00Z','data':{'ref':'refs/heads/main','object':{'type':'commit','sha':'f'*40}}},directory)
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
        changed['classic_protection']={'state':'observed','method':'GET','http_status':200,
            'request_url':'https://api.github.com/repos/'+row['destination']+'/branches/main/protection',
            'observed_at':changed['observed_at'],'data':{'required_status_checks':{'strict':True,
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
            changed=copy.deepcopy(original);changed[field]=value
            if field=='result':changed['data']=copy.deepcopy(value)
            row['requester_permission_evidence_url']=self.capture(changed)
            with self.subTest(field=field),self.assertRaisesRegex(ValueError,'permission capture'):self.check()
        changed=copy.deepcopy(original);changed['result']={'permission':'write','role_name':'maintain'}
        changed['data']=copy.deepcopy(changed['result'])
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
            path.write_text(json.dumps(self.bind_final_pages({'observed_at':proof['observed_at'],'accounts':accounts})))
            validator.validate_final_inventory(proof,directory,{}, {},onboarding)
            for timestamp in ('2026-10-07T01:00:00Z','2026-10-07T02:30:00Z','2026-10-07T03:15:00Z'):
                proof['observed_at']=timestamp;path.write_text(json.dumps(self.bind_final_pages({'observed_at':timestamp,'accounts':accounts})))
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

    def test_windows_continuity_requires_original_registration_and_startup(self):
        self.complete_transfer_gates()
        row=next(r for r in self.matrix['repositories'] if r['repository_id']==validator.SURVIVAL_REPOSITORY_ID)
        proof=row['post_transfer_runner'];earliest=validator.observed_time('2026-10-07T01:59:00Z','fixture')
        validator.validate_runner_captures(proof,DIRECTORY,earliest)
        path=DIRECTORY/proof['registration_evidence_url'];original=json.loads(path.read_text())
        for mutation in ('missing-get','failed-get','wrong-runner','wrong-labels','stale-get'):
            value=copy.deepcopy(original)
            if mutation=='missing-get':value.pop('runner_response')
            elif mutation=='failed-get':value['runner_response']['http_status']=403
            elif mutation=='wrong-runner':value['runner']['id']=11
            elif mutation=='wrong-labels':value['runner']['labels'].append({'name':'unreviewed'})
            else:value['runner_response']['observed_at']='2026-10-07T01:00:00Z'
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.validate_runner_captures(proof,DIRECTORY,earliest)
        path.write_text(json.dumps(original))
        path=DIRECTORY/proof['startup_evidence_url'];original=json.loads(path.read_text())
        for mutation in ('directory','disabled','principal','action','trigger','stale-host','listener','service'):
            value=copy.deepcopy(original);host=value['host_capture'];task=value['startup_capture']['data']
            if mutation=='directory':host['data']['configurations'][0]['directory']=r'C:\unreviewed'
            elif mutation=='disabled':task['enabled']=False
            elif mutation=='principal':task['principal']['runLevel']='Highest'
            elif mutation=='action':task['actions'][0]['Arguments']='--replace'
            elif mutation=='trigger':task['triggers'][0]['Enabled']=False
            elif mutation=='stale-host':host['observed_at']='2026-10-07T01:00:00Z'
            elif mutation=='listener':host['data']['processes'][0]['ExecutablePath']=r'C:\other\Runner.Listener.exe'
            else:host['data']['services']=[{'name':'new-service'}]
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.validate_runner_captures(proof,DIRECTORY,earliest)
        path.write_text(json.dumps(original))
        validator.validate_runner_captures(proof,DIRECTORY,earliest)

    def test_windows_continuity_executes_reviewed_existing_ci_on_current_attempt(self):
        self.complete_transfer_gates()
        row=next(r for r in self.matrix['repositories'] if r['repository_id']==validator.SURVIVAL_REPOSITORY_ID)
        proof=row['post_transfer_runner'];earliest=validator.observed_time('2026-10-07T01:59:00Z','fixture')
        path=DIRECTORY/proof['run_evidence_url'];original=json.loads(path.read_text())
        for valid in ('.github/workflows/ci.yml','.github/workflows/ci.yml@main'):
            value=copy.deepcopy(original);value['run']['path']=valid;value['run_response']['data']=copy.deepcopy(value['run'])
            path.write_text(json.dumps(value));validator.validate_runner_captures(proof,DIRECTORY,earliest)
        for mutation in ('wrong-ref','wrong-workflow','wrong-branch','wrong-event','wrong-mode','stale-run'):
            value=copy.deepcopy(original)
            if mutation=='wrong-ref':value['run']['path']='.github/workflows/ci.yml@feature'
            elif mutation=='wrong-workflow':value['run']['path']='.github/workflows/self-hosted-smoke.yml'
            elif mutation=='wrong-branch':value['run']['head_branch']='feature'
            elif mutation=='wrong-event':value['run']['event']='push'
            elif mutation=='wrong-mode':value['verification_mode']='read_only'
            else:value['run']['created_at']='2026-10-07T01:00:00Z'
            value['run_response']['data']=copy.deepcopy(value['run']);path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.validate_runner_captures(proof,DIRECTORY,earliest)
        path.write_text(json.dumps(original))
        path=DIRECTORY/proof['jobs_evidence_url'];original=json.loads(path.read_text())
        for field,value in (('name','unreviewed job'),('runner_id',11),('run_attempt',2),('conclusion','skipped')):
            changed=copy.deepcopy(original);changed['jobs'][0][field]=value
            changed['pages'][0]['data']['jobs']=copy.deepcopy(changed['jobs']);path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaises(ValueError):
                validator.validate_runner_captures(proof,DIRECTORY,earliest)
        path.write_text(json.dumps(original))
        validator.validate_runner_captures(proof,DIRECTORY,earliest)

    def test_current_windows_resources_are_pinned_without_replacing_inventory(self):
        repo=next(r for r in self.inventory['repositories'] if r['id']==validator.SURVIVAL_REPOSITORY_ID)
        current=validator.protection_contract(repo);original=validator.original_protection_contract(repo,DIRECTORY)
        self.assertEqual(original['rulesets'],[])
        self.assertEqual(len(current['rulesets']),1)
        path=DIRECTORY/'current-survival-resources.json';data=path.read_bytes()
        path.write_bytes(data+b' ')
        with self.assertRaisesRegex(ValueError,'reviewed preparation capture'):validator.protection_contract(repo)
        path.write_bytes(data)
        self.assertEqual(validator.protection_contract(repo),current)

    def test_windows_pre_cutover_preserves_enabled_variable_and_runner_labels(self):
        self.complete_transfer_gates();credential=self.matrix['pre_transfer_credential_verification']
        ref=self.matrix['pre_cutover_source_evidence_url'];path=DIRECTORY/ref;original=json.loads(path.read_text())
        validator.validate_source_refresh(ref,DIRECTORY,self.inventory,credential)
        for mutation in ('missing-variable','disabled-variable','stale-variable','changed-labels','offline','removed-runner'):
            value=copy.deepcopy(original);row=next(r for a in value['accounts'] for r in a['repositories']
                if r['id']==validator.SURVIVAL_REPOSITORY_ID)
            if mutation=='missing-variable':row.pop('variable_responses')
            elif mutation=='disabled-variable':row['variable_responses']['UE_RUNNER_ENABLED']['data']['value']='false'
            elif mutation=='stale-variable':row['variable_responses']['UE_RUNNER_ENABLED']['observed_at']='2026-10-07T01:00:00Z'
            elif mutation=='changed-labels':row['runner_pages'][0]['data']['runners'][0]['labels'].pop()
            elif mutation=='offline':row['runner_pages'][0]['data']['runners'][0]['status']='offline'
            else:row['runner_pages'][0]['data'].update(runners=[],total_count=0)
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.validate_source_refresh(ref,DIRECTORY,self.inventory,credential)
        path.write_text(json.dumps(original))

    def test_windows_workflow_drift_is_rejected_before_cutover(self):
        self.complete_transfer_gates();credential=self.matrix['pre_transfer_credential_verification']
        ref=self.matrix['pre_cutover_source_evidence_url'];source=json.loads((DIRECTORY/ref).read_text())
        validator.validate_source_refresh(ref,DIRECTORY,self.inventory,credential)
        path=DIRECTORY/source['reference_scan_evidence_url'];scan=json.loads(path.read_text())
        row=next(r for r in scan['repositories'] if r['repository_id']==validator.SURVIVAL_REPOSITORY_ID)
        repo=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id'])
        name='.github/workflows/ci.yml';body=validator.reviewed_survival_resources(repo,DIRECTORY)['workflow_bytes']
        changed=body+b'\n# Unreviewed workflow revision\n'
        file=json.loads((DIRECTORY/self.file_contents_capture(repo['id'],repo['full_name'],row['head_sha'],name,changed)).read_text())
        file.update(path=name,observed_at=row['observed_at'])
        row['files']=[file if f['path']==name else f for f in row['files']]
        next(entry for entry in row['tree_response']['data']['tree'] if entry['path']==name)['sha']=file['contents_response']['data']['sha']
        path.write_text(json.dumps(scan))
        with self.assertRaisesRegex(ValueError,'Windows workflow must match'):
            validator.validate_source_refresh(ref,DIRECTORY,self.inventory,credential)


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
            capture=copy.deepcopy(original);capture['observed_at']=timestamp
            for response in capture['provider_responses'].values():response['observed_at']=timestamp
            path.write_text(json.dumps(capture))
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
        with self.assertRaises(ValueError):self.check()
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
                new['archived']=archived;path.write_text(json.dumps(self.bind_final_pages({'observed_at':proof['observed_at'],'accounts':accounts})))
                with self.subTest(archived=archived),self.assertRaisesRegex(ValueError,'remain unarchived'):
                    validator.validate_final_inventory(proof,directory,{}, {},onboarding)
            new['archived']=False;path.write_text(json.dumps(self.bind_final_pages({'observed_at':proof['observed_at'],'accounts':accounts})))
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
        self.assertEqual(times[row['repository_id']],validator.observed_time('2026-10-07T03:40:00Z','fixture'))
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
        for field in ('effective_rules','classic_protection'):
            final_policy[field]['observed_at']=output['observed_at']
        final_policy_path.write_text(json.dumps(final_policy))
        before_path=DIRECTORY/pilot['validation_evidence']['pre_cleanup_gate_policy_evidence_url']
        before=json.loads(before_path.read_text());self.retime_policy(before,output['observed_at']);before_path.write_text(json.dumps(before))
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
        for kind in ('vercel_project','github_pages','cloudflare_zone','cloudflare_worker'):
            row=next(r for r in self.rows if r.get('resource_kind')==kind);original=copy.deepcopy(row)
            row['smoke_outcome']='baseline_preserved';smoke=json.loads((DIRECTORY/row['smoke_evidence_url']).read_text())
            smoke.update(outcome='baseline_preserved',baseline=validator.provider_preservation_baseline(row,self.inventory,DIRECTORY))
            smoke['observed_resource']=copy.deepcopy(smoke['baseline']);row['smoke_evidence_url']=self.capture(smoke)
            with self.subTest(kind=kind),self.assertRaisesRegex(ValueError,'Preservation-only outcomes'):self.check()
            row.clear();row.update(original)
        row=next(r for r in self.rows if r.get('resource_kind')=='supabase_project');self.check()
        smoke=json.loads((DIRECTORY/row['smoke_evidence_url']).read_text())
        smoke.update(baseline={'invented':'unchanged'},observed_resource={'invented':'unchanged'});row['smoke_evidence_url']=self.capture(smoke)
        with self.assertRaisesRegex(ValueError,'sealed provider resource'):self.check()


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
        smoke['observed_at']=cutoff
        for response in smoke['provider_responses'].values():response['observed_at']=cutoff
        smoke_path.write_text(json.dumps(smoke))
        runner=row['post_transfer_runner'];originals={field:json.loads((DIRECTORY/runner[field]).read_text()) for field in
            ('registration_evidence_url','isolation_evidence_url','service_evidence_url','run_evidence_url')}
        for field,capture in originals.items():
            capture['observed_at']=cutoff
            if field=='run_evidence_url':
                capture['run'].update(created_at=cutoff,updated_at=cutoff)
                capture['run_response'].update(observed_at=cutoff,data=copy.deepcopy(capture['run']))
            (DIRECTORY/runner[field]).write_text(json.dumps(capture))
        workflow_path=DIRECTORY/runner['workflow_evidence_url'];workflow=json.loads(workflow_path.read_text())
        workflow['observed_at']=cutoff;workflow_path.write_text(json.dumps(workflow))
        jobs_path=DIRECTORY/runner['jobs_evidence_url'];jobs=json.loads(jobs_path.read_text())
        jobs['observed_at']=cutoff
        for job in jobs['jobs']:job.update(started_at=cutoff,completed_at=cutoff)
        jobs['pages'][0]['observed_at']=cutoff
        jobs['pages'][0]['data']['jobs']=copy.deepcopy(jobs['jobs'])
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
        request['data']=copy.deepcopy(request['comment'])
        request_path.write_text(json.dumps(request))
        artifact_receipt=row['review_operation_receipts']['review_artifact_url'];artifact_path=DIRECTORY/artifact_receipt['capture_evidence_url']
        artifact=json.loads(artifact_path.read_text());row['review_artifact_url']=row['review_pr_url']+'#pullrequestreview-2'
        artifact_receipt['evidence_url']=row['review_artifact_url'];artifact['evidence_url']=row['review_artifact_url']
        artifact['kind']='pull_request_review';artifact['artifact'].update(html_url=row['review_artifact_url'],commit_id=row['review_head_sha'],submitted_at='2026-10-07T02:00:00Z')
        artifact['request_url']='https://api.github.com/repos/'+row['destination']+'/pulls/'+row['review_pr_url'].rsplit('/',1)[1]+'/reviews/2'
        artifact['data']=copy.deepcopy(artifact['artifact'])
        artifact_path.write_text(json.dumps(artifact))
        gate['check_run']['external_id']=external.replace(':context:fixture:',':context:fixture%3Awith%25encoding:').replace(':artifact:c2',':artifact:r2')
        gate_path.write_text(json.dumps(gate));self.bind_gate_execution(row);self.check()

    def test_pilots_use_actual_metadata_default_branch_and_preserve_it(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0];receipts=pilot['validation_evidence']
        metadata_path=DIRECTORY/receipts['metadata_evidence_url'];metadata=json.loads(metadata_path.read_text());self.check()
        metadata['metadata']['default_branch']='develop';metadata_path.write_text(json.dumps(metadata))
        with self.assertRaisesRegex(ValueError,'target the gated branch'):self.check()
        pr_path=DIRECTORY/receipts['review_pr_metadata_evidence_url'];pr=json.loads(pr_path.read_text())
        pr['pull_request']['base']['ref']='develop';pr['data']=copy.deepcopy(pr['pull_request']);pr_path.write_text(json.dumps(pr))
        receipts['gate_policy']['branch']='develop';policy_path=DIRECTORY/receipts['gate_policy']['evidence_url']
        policy=json.loads(policy_path.read_text());policy['branch']='develop'
        policy['effective_rules']['request_url']=policy['effective_rules']['request_url'].replace('/main','/develop')
        policy['classic_protection']['request_url']=policy['classic_protection']['request_url'].replace('/main','/develop')
        policy_path.write_text(json.dumps(policy))
        final_policy_path=DIRECTORY/receipts['final_gate_policy_evidence_url'];final_policy=json.loads(final_policy_path.read_text())
        final_policy['branch']='develop'
        for field in ('effective_rules','classic_protection'):
            final_policy[field]['request_url']=final_policy[field]['request_url'].replace('/main','/develop')
        before_path=DIRECTORY/receipts['pre_cleanup_gate_policy_evidence_url'];before=json.loads(before_path.read_text())
        before['branch']='develop'
        for field in ('effective_rules','classic_protection'):
            before[field]['request_url']=before[field]['request_url'].replace('/main','/develop')
        before_path.write_text(json.dumps(before))
        final_policy_path.write_text(json.dumps(final_policy));self.check()
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
        capture['artifact']['digest']='sha256:'+hashlib.sha256(stream.getvalue()).hexdigest()
        capture['artifact_response']['data']=copy.deepcopy(capture['artifact']);artifact_path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'independently downloaded output'):self.check()
        artifact_path.write_text(json.dumps(original));self.check()
        run_path=DIRECTORY/proof['workflow_run_evidence_url'];run=json.loads(run_path.read_text())
        run['run_started_at']='2026-10-07T00:00:01Z'
        run['run_response']['data']['run_started_at']=run['run_started_at'];run_path.write_text(json.dumps(run))
        with self.assertRaisesRegex(ValueError,'completed run'):self.check()

    def test_pilot_cleanup_follows_validation_and_final_gate_is_absent(self):
        self.complete_pilots();receipts=self.matrix['disposable_validation_repositories'][0]['validation_evidence'];self.check()
        operation=receipts['operation_receipts']['gate_uninstall_evidence_url'];path=DIRECTORY/operation['capture_evidence_url']
        original=json.loads(path.read_text());capture=copy.deepcopy(original)
        capture['started_at']='2026-10-07T02:00:00Z'
        capture['execution'].update(started_at=capture['started_at'],completed_at=capture['started_at'])
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
        capture['provider_responses']={'project':{'method':'GET','http_status':200,
            'request_url':'https://api.supabase.com/v1/projects/'+resource['resource_id'],
            'observed_at':capture['observed_at'],'data':{'ref':resource['resource_id'],'name':baseline['name'],
                'region':baseline['region'],'status':'INACTIVE','organization_slug':'tigijbtmewljfqdmdskh'}}}
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
        capture['run_response']['data']=copy.deepcopy(capture['run'])
        capture['run_response']['observed_at']=capture['observed_at']
        path.write_text(json.dumps(capture))
        self.bind_terminal_capture(receipts['operation_receipts']['gate_uninstall_evidence_url'],timestamp='2026-10-07T02:02:00Z')
        before_path=DIRECTORY/receipts['pre_cleanup_gate_policy_evidence_url'];before=json.loads(before_path.read_text())
        self.retime_policy(before,'2026-10-07T02:01:30Z');before_path.write_text(json.dumps(before))
        path=DIRECTORY/receipts['final_gate_policy_evidence_url'];capture=json.loads(path.read_text())
        capture['observed_at']='2026-10-07T02:03:00Z'
        for field in ('effective_rules','classic_protection'):
            capture[field]['observed_at']=capture['observed_at']
        path.write_text(json.dumps(capture))
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
            capture=copy.deepcopy(original);capture['jobs'][0][field]=value
            capture['pages'][0]['data']['jobs'][0][field]=value;path.write_text(json.dumps(capture))
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
            capture['event'].update({'@timestamp':milliseconds,'created_at':milliseconds})
            capture['audit_export']=self.audit_export_fixture([capture['event']],capture['observed_at']);path.write_text(json.dumps(capture))
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
        capture=json.loads(run_path.read_text());capture['run']['head_branch']='retained-branch'
        capture['run_response']['data']=copy.deepcopy(capture['run']);run_path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'actual default branch'):self.check()
        capture['run']['head_branch']='main';capture['run_response']['data']=copy.deepcopy(capture['run']);run_path.write_text(json.dumps(capture));self.check()
        del proof['default_branch_evidence_url']
        with self.assertRaisesRegex(ValueError,'Missing evidence'):self.check()


    def test_pre_cutover_source_refresh_preserves_exact_visibility(self):
        self.complete_transfer_gates();self.check();path=DIRECTORY/self.matrix['pre_cutover_source_evidence_url']
        original=json.loads(path.read_text());capture=copy.deepcopy(original)
        private=next(r for a in capture['accounts'] for r in a['repositories'] if r['private'])
        private['visibility']='internal';path.write_text(json.dumps(self.bind_final_pages(capture)))
        with self.assertRaisesRegex(ValueError,'metadata changed'):self.check()
        del private['visibility'];path.write_text(json.dumps(self.bind_final_pages(capture)))
        with self.assertRaisesRegex(ValueError,'metadata changed'):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_access_and_license_capture_follow_actual_repository_transfer(self):
        row=self.complete_rollout();self.assertEqual(row['repository_id'],1143951439);self.check()
        transfer_path=DIRECTORY/row['transfer_evidence_url'];capture=json.loads(transfer_path.read_text())
        milliseconds=int(validator.observed_time('2026-10-07T01:56:00Z','test').timestamp()*1000)
        capture['event'].update({'@timestamp':milliseconds,'created_at':milliseconds})
        capture['observed_at']='2026-10-07T01:56:30Z'
        capture['audit_export']=self.audit_export_fixture([capture['event']],capture['observed_at'])
        policy_path=DIRECTORY/capture['source_protection_evidence_url'];policy=json.loads(policy_path.read_text())
        self.retime_policy(policy,'2026-10-07T01:55:55Z');policy_path.write_text(json.dumps(policy))
        head_path=DIRECTORY/capture['destination_heads_evidence_url'];heads=json.loads(head_path.read_text())
        heads['observed_at']=capture['observed_at'];heads['repository_response']['observed_at']=capture['observed_at']
        for page in heads['branches_response']['pages']:page['observed_at']=capture['observed_at']
        heads['tag_refs_response']['observed_at']=capture['observed_at']
        for response in heads['tag_object_responses'].values():response['observed_at']=capture['observed_at']
        head_path.write_text(json.dumps(heads))
        transfer_path.write_text(json.dumps(capture))
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
            path.write_text(json.dumps(self.bind_final_pages(capture)))
            with self.subTest(name=name),self.assertRaisesRegex(ValueError,'newly occupied mapped transfer name'):self.check()
        capture=copy.deepcopy(original);capture['destination_account']['all_pages']=False;path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'complete destination'):self.check()
        capture=copy.deepcopy(original);del capture['destination_account'];path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'complete destination'):self.check()
        capture=copy.deepcopy(original);capture['destination_account']['repositories'].append({'id':999,'full_name':'hemsoft-dev/unrelated-new-repo'})
        path.write_text(json.dumps(self.bind_final_pages(capture)));self.check()


    def test_final_onboarding_orders_noop_operations_even_at_one_revision(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946)
            fields=('init_pr_url','repeat_onboarding_url','sync_pr_url','repeat_sync_url')
            for field in fields:
                path=directory/proof['onboarding_operation_receipts'][field]['capture_evidence_url']
                original=path.read_text();capture=json.loads(original);capture['observed_at']='2026-10-07T03:31:00Z'
                if capture['outcome']=='pull_request_merged':
                    capture['result']['pull_request']['merged_at']=capture['observed_at']
                    capture['pull_request_response'].update(observed_at=capture['observed_at'],data=copy.deepcopy(capture['result']['pull_request']))
                else:
                    capture['started_at']=capture['observed_at']
                    capture['execution'].update(started_at=capture['observed_at'],completed_at=capture['observed_at'])
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
                validator.observed_time('2026-10-07T03:40:00Z','fixture'))

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
                lambda c:c['comment'].update(created_at='2026-10-07T02:01:00Z'),
                lambda c:c['comment'].update(updated_at='2026-10-07T04:00:00Z'),lambda c:c.update(http_status=404),
                lambda c:c['comment'].update(body='Unrelated owner comment'),
                lambda c:c['comment'].update(body=c['comment']['body'].replace('"repository_id": 42','"repository_id": 43')))
            for mutate in mutations:
                changed=copy.deepcopy(original);mutate(changed);path.write_text(json.dumps(changed))
                with self.assertRaises(ValueError):validator.validate_final_inventory(proof,directory,{}, {})
            changed=copy.deepcopy(original);changed['comment']['html_url']=changed['comment']['html_url'].replace('/HemSoft/','/hemsoft-dev/')
            path.write_text(json.dumps(changed));validator.validate_final_inventory(proof,directory,{}, {})


    def test_scope_exception_binds_actual_owner_comment_get(self):
        row=self.complete_rollout();row.update(health='scope_exception',exception_evidence_url='https://example.com/exception')
        self.matrix['summary']['verified_rollouts']-=1;self.complete_scope_decision(row);self.check()
        decision=json.loads((DIRECTORY/row['exception_evidence_url']).read_text())
        path=DIRECTORY/decision['owner_comment_evidence_url'];original=json.loads(path.read_text())
        for mutate in (lambda c:c['comment']['user'].update(login='other'),lambda c:c['comment']['user'].update(id=42),
                       lambda c:c.update(http_status=404),lambda c:c['comment'].update(body='Unrelated owner comment'),
                       lambda c:c['comment'].update(body=c['comment']['body'].replace('exclude_runtime_rollout','include_final_inventory')),
                       lambda c:c['comment'].update(updated_at='2026-10-07T03:00:00Z')):
            changed=copy.deepcopy(original);mutate(changed);path.write_text(json.dumps(changed))
            with self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_post_transfer_app_owner_needs_actual_registration_get(self):
        self.complete_transfer_gates();self.complete_app_transfer();self.check()
        path=DIRECTORY/self.matrix['owned_app_transfer']['evidence_url'];original=json.loads(path.read_text())
        for mutate in (lambda c:c.pop('request_url'),lambda c:c.update(http_status=404),
                       lambda c:c.update(request_url='https://api.github.com/apps/another-app')):
            changed=copy.deepcopy(original);mutate(changed);path.write_text(json.dumps(changed))
            with self.assertRaisesRegex(ValueError,'post-transfer registration metadata'):self.check()
        original['app']['owner'].update(avatar_url='https://avatars.githubusercontent.com/u/338855369?v=4',node_id='O_fixture')
        path.write_text(json.dumps(original));self.check()

    def test_live_scenario_creation_must_follow_actual_pilot_cutoff(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0]
        result,capture=self.bind_live_scenario(pilot);path=DIRECTORY/result['capture_evidence_url'];self.check()
        capture['run']['created_at']='2026-10-07T01:50:00Z'
        capture['run_response']['data']=copy.deepcopy(capture['run']);path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'Live scenario run and attempt must start after'):self.check()
        capture['run']['created_at']='2026-10-07T02:00:00Z'
        capture['run_response']['data']=copy.deepcopy(capture['run']);path.write_text(json.dumps(capture));self.check()

    def test_runner_service_must_bind_preserved_unit_and_systemctl_output(self):
        self.complete_pilots();row=next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/yahtzee')
        row.update(health='scope_exception',transfer_evidence_url=self.transfer_capture(row),status_evidence_url='https://example.com/status')
        self.complete_scope_decision(row);self.check()
        path=DIRECTORY/row['post_transfer_runner']['service_evidence_url'];original=json.loads(path.read_text())
        for mutate in (lambda c:c.update(unit='unrelated.service'),lambda c:c['systemctl_show'].update(Id='unrelated.service'),
                       lambda c:c['systemctl_show'].update(ActiveState='inactive'),
                       lambda c:c.update(argv=['systemctl','show','unrelated.service','--property=Id,ActiveState','--no-pager'])):
            changed=copy.deepcopy(original);mutate(changed);path.write_text(json.dumps(changed))
            with self.assertRaisesRegex(ValueError,'guest service is active'):self.check()
        path.write_text(json.dumps(original));self.check()


    def test_final_status_preserves_repeated_sync_revision(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946,terminal_times={})
            operation=proof['onboarding_operation_receipts']['status_url']
            operation['revision_before']='d'*40;path=directory/operation['capture_evidence_url']
            capture=json.loads(path.read_text());capture['revision_before']=operation['revision_before'];path.write_text(json.dumps(capture))
            with self.assertRaisesRegex(ValueError,'Status needs captured healthy'):
                validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946,terminal_times={})

    def test_new_repository_creation_requires_successful_exact_metadata_get(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946,terminal_times={})
            path=directory/proof['metadata_evidence_url'];original=json.loads(path.read_text())
            for mutate in (lambda c:c.pop('request_url'),lambda c:c.update(http_status=404),
                           lambda c:c.update(request_url='https://api.github.com/repos/hemsoft-dev/old-repository'),
                           lambda c:c.update(observed_at='2026-10-07T02:00:00Z')):
                changed=copy.deepcopy(original);mutate(changed);path.write_text(json.dumps(changed))
                with self.assertRaises(ValueError):
                    validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946,terminal_times={})
            path.write_text(json.dumps(original));validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946,terminal_times={})

    def test_sfl_installation_grants_match_approved_permissions(self):
        row=self.complete_rollout();self.check()
        references=[row['destination_sfl_app_access']['evidence_url'],'owned-app-organization-installation-evidence.json']
        for reference in references:
            path=DIRECTORY/reference;original=json.loads(path.read_text())
            for mutate in (lambda c:c['installation'].pop('permissions'),
                           lambda c:c['installation']['permissions'].update(checks='read'),
                           lambda c:c['installation']['permissions'].update(contents='write')):
                changed=copy.deepcopy(original);mutate(changed);path.write_text(json.dumps(changed))
                with self.assertRaisesRegex(ValueError,'SFL coverage'):self.check()
            path.write_text(json.dumps(original));self.check()


    def test_every_source_head_and_branch_matches_the_fresh_reference_scan(self):
        self.complete_transfer_gates();self.check()
        path=DIRECTORY/self.matrix['pre_cutover_source_evidence_url'];original=json.loads(path.read_text())
        for change in ('head','tree','branch','missing'):
            capture=copy.deepcopy(original)
            repo=next(r for a in capture['accounts'] for r in a['repositories'] if r['full_name']=='HemSoft/dashboard')
            current=repo['source_head']
            if change=='head':
                current['head_sha']='d'*40;current['ref_response']['data']['object']['sha']='d'*40
                current['commit_response']['request_url']=current['commit_response']['request_url'].replace('e'*40,'d'*40)
                current['commit_response']['data']['sha']='d'*40
                current['branches_response']['data'][0]['commit']['sha']='d'*40
            elif change=='tree':
                current['tree_sha']='c'*40;current['commit_response']['data']['tree']['sha']='c'*40
            elif change=='branch':current['branches_response']['data'].append({'name':'new-integration','commit':{'sha':'d'*40}})
            else:repo.pop('source_head')
            path.write_text(json.dumps(capture))
            with self.subTest(change=change),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_fresh_scan_reads_all_workflows_and_reconciles_legacy_credentials(self):
        self.complete_transfer_gates();source=json.loads((DIRECTORY/self.matrix['pre_cutover_source_evidence_url']).read_text())
        path=DIRECTORY/source['reference_scan_evidence_url'];original=json.loads(path.read_text())
        row=next(r for r in original['repositories'] if r['source']=='HemSoft/dashboard')
        name='.github/workflows/new-client.yml';content=b'name: new client\nrun: echo ${{ secrets.SFL_APP_PRIVATE_KEY }}\n'
        file=json.loads((DIRECTORY/self.file_contents_capture(row['repository_id'],row['source'],row['head_sha'],name,content)).read_text())
        file['path']=name;file['observed_at']=row['observed_at']
        row['tree_response']['data']['tree']=[{'path':name,'type':'blob','sha':file['contents_response']['data']['sha']}]
        path.write_text(json.dumps(original))
        with self.assertRaisesRegex(ValueError,'read every workflow'):self.check()
        row['files']=[file];row['referenced_secret_names']=['SFL_APP_PRIVATE_KEY'];path.write_text(json.dumps(original))
        with self.assertRaisesRegex(ValueError,'newly referenced legacy credential'):self.check()
        row['referenced_secret_names']=[];path.write_text(json.dumps(original))
        with self.assertRaisesRegex(ValueError,'conclusions must derive'):self.check()

    def test_pre_cutover_credential_run_is_fresh_and_executes_current_source_main(self):
        self.complete_transfer_gates();self.check()
        path=DIRECTORY/self.matrix['pre_transfer_credential_verification']['workflow_run_evidence_url']
        original=json.loads(path.read_text());capture=copy.deepcopy(original)
        capture.update(created_at='2026-10-07T00:00:00Z',run_started_at='2026-10-07T00:00:00Z')
        capture['run_response']['data'].update(created_at=capture['created_at'],run_started_at=capture['run_started_at'])
        path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'new successful run after the fresh scan'):self.check()
        path.write_text(json.dumps(original))
        source_path=DIRECTORY/self.matrix['pre_cutover_source_evidence_url'];source=json.loads(source_path.read_text())
        source['observed_at']='2026-10-07T02:05:00Z';source_path.write_text(json.dumps(source))
        with self.assertRaisesRegex(ValueError,'within 15 minutes'):self.check()
        source['observed_at']='2026-10-07T01:50:00Z'
        scan_path=DIRECTORY/source['reference_scan_evidence_url'];scan=json.loads(scan_path.read_text())
        current=next(r for a in source['accounts'] for r in a['repositories'] if r['id']==1169772257)['source_head']
        scanned=next(r for r in scan['repositories'] if r['repository_id']==1169772257)
        for head in (current,scanned):
            head['head_sha']='d'*40;head['ref_response']['data']['object']['sha']='d'*40
            head['commit_response']['data']['sha']='d'*40
            head['commit_response']['request_url']=head['commit_response']['request_url'].replace('e'*40,'d'*40)
            head['branches_response']['data'][0]['commit']['sha']='d'*40
            head['branches_response']['pages'][0]['data'][0]['commit']['sha']='d'*40
            for file in head.get('files',[]):
                file['revision_sha']='d'*40
                file['contents_response']['request_url']=file['contents_response']['request_url'].replace('e'*40,'d'*40)
        scan_path.write_text(json.dumps(scan));source_path.write_text(json.dumps(source))
        current_row=next(r for a in source['accounts'] for r in a['repositories'] if r['id']==1169772257)
        policy_path=DIRECTORY/current_row['protection_evidence_url'];policy=json.loads(policy_path.read_text())
        policy['revision_sha']='d'*40;policy['revision_response']['data']['object']['sha']='d'*40
        policy_path.write_text(json.dumps(policy))
        with self.assertRaisesRegex(ValueError,'execute the current source main'):self.check()

    def test_provider_success_checks_actual_vercel_binding_production_and_aliases(self):
        row=self.complete_provider();self.check();path=DIRECTORY/row['smoke_evidence_url'];original=json.loads(path.read_text())
        proxy=copy.deepcopy(original)
        proxy['provider_responses']['project']['request_url']=proxy['provider_responses']['project']['request_url'].replace('https://api.vercel.com/','https://vercel.com/api/')
        path.write_text(json.dumps(proxy));self.check();path.write_text(json.dumps(original))
        mutations=(lambda c:c.pop('provider_responses'),lambda c:c['provider_responses']['project']['data'].update(link=None),
            lambda c:c['provider_responses']['project']['data']['link'].update(org='HemSoft'),
            lambda c:c['provider_responses']['project']['data']['targets']['production'].update(readyState='ERROR'),
            lambda c:c['provider_responses']['project']['data']['targets']['production'].update(alias=[]),
            lambda c:c['provider_responses']['project'].update(request_url='https://api.vercel.com/v9/projects/other'),
            lambda c:c['provider_responses']['project'].update(http_status=404))
        for mutate in mutations:
            capture=copy.deepcopy(original);mutate(capture);path.write_text(json.dumps(capture))
            with self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()
        self.assertEqual(original['provider_responses']['project']['data']['targets']['preview']['readyState'],'ERROR')

    def test_provider_success_primary_observations_must_follow_the_actual_transfer(self):
        self.complete_pilots();row=next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/yahtzee')
        row.update(health='scope_exception',transfer_evidence_url=self.transfer_capture(row),status_evidence_url='https://example.com/status')
        self.complete_scope_decision(row);self.check()
        resource=next(r for r in self.rows if r['source']=='HemSoft/yahtzee' and r.get('resource_kind')=='repository_runner')
        capture=json.loads((DIRECTORY/resource['smoke_evidence_url']).read_text())
        capture['provider_responses']['runner']['observed_at']='2026-10-07T01:00:00Z'
        resource['smoke_evidence_url']=self.capture(capture)
        with self.assertRaisesRegex(ValueError,'smoke observations must follow'):self.check()

    def test_known_provider_success_rejects_broken_pages_cloudflare_and_runner_state(self):
        cases=[('github_pages','pages',lambda d:d.update(https_enforced=False)),
               ('cloudflare_zone','dns',lambda d:d['result'][0].update(proxied=False)),
               ('cloudflare_worker','subdomain',lambda d:d['result'].update(enabled=False)),
               ('cloudflare_worker','account_subdomain',lambda d:d['result'].update(subdomain='other')),
               ('cloudflare_worker','routes',lambda d:d.update(result=[{'script':'other','pattern':'example.com/*'}])),
               ('repository_runner','runner',lambda d:d.update(status='offline'))]
        for kind,response,mutate in cases:
            row=next(r for r in self.rows if r['resource_kind']==kind)
            self.verify_source_ledger(int(row['repository_id']));self.check()
            path=DIRECTORY/row['smoke_evidence_url'];original=json.loads(path.read_text())
            proxy=copy.deepcopy(original)
            for captured in proxy['provider_responses'].values():
                captured['request_url']=captured['request_url'].replace('https://api.cloudflare.com/client/v4/','https://dash.cloudflare.com/api/v4/')
            if kind=='cloudflare_zone':proxy['provider_responses']['zone']['data']['result']['plan']['name']='Free Website'
            path.write_text(json.dumps(proxy));self.check();path.write_text(json.dumps(original))
            capture=copy.deepcopy(original);mutate(capture['provider_responses'][response]['data']);path.write_text(json.dumps(capture))
            with self.subTest(kind=kind,response=response),self.assertRaises(ValueError):self.check()
            path.write_text(json.dumps(original))

    def test_fresh_scanned_manifests_supply_the_current_installation_baseline(self):
        self.complete_transfer_gates()
        source=json.loads((DIRECTORY/self.matrix['pre_cutover_source_evidence_url']).read_text())
        revisions,scanned_at,manifests=validator.validate_reference_scan(source['reference_scan_evidence_url'],DIRECTORY,self.inventory)
        self.assertEqual(len(revisions),67)
        self.assertEqual(manifests[1169772257]['tier'],'full')
        scan_path=DIRECTORY/source['reference_scan_evidence_url'];scan=json.loads(scan_path.read_text())
        row=next(r for r in scan['repositories'] if r['repository_id']==1169772257)
        file=row['files'][0];file['manifest']=dict(file['manifest'],tier='reviewer')
        scan_path.write_text(json.dumps(scan))
        with self.assertRaisesRegex(ValueError,'derive from immutable bytes'):
            validator.validate_reference_scan(source['reference_scan_evidence_url'],DIRECTORY,self.inventory)

    def test_active_provider_cannot_claim_preservation_without_continuity(self):
        row=self.complete_provider();path=DIRECTORY/row['smoke_evidence_url'];original=json.loads(path.read_text())
        for outcome in ('baseline_preserved','preserved_unused'):
            capture=copy.deepcopy(original)
            row['smoke_outcome']=outcome
            capture.update(outcome=outcome,baseline=validator.provider_preservation_baseline(row,self.inventory,DIRECTORY),
                resource_unchanged=True,runtime_actions=[])
            capture['observed_resource']=copy.deepcopy(capture['baseline']);capture.pop('provider_responses')
            path.write_text(json.dumps(capture))
            with self.subTest(outcome=outcome),self.assertRaisesRegex(ValueError,'explicitly approved unused resource'):self.check()

    def test_approved_recovery_needs_bound_owner_comment_and_provider_gets(self):
        row=self.complete_provider();path=DIRECTORY/row['smoke_evidence_url'];capture=json.loads(path.read_text())
        row['smoke_outcome']='approved_recovery'
        capture.update(outcome='approved_recovery',recovery_success=True,approved_by='HemSoft',
            recovery_operation={'command':'provider-metadata-recovery','argv':['provider-metadata-recovery',row['resource_id']]})
        path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'Missing evidence'):self.check()
        decision={'repository_id':int(row['repository_id']),'repository':row['destination'],'provider':row['provider'],
            'resource_id':row['resource_id'],'disposition':'approved_recovery','reason':'Synthetic recovery approval',
            'recovery_action':row['recovery_action'],'operation':capture['recovery_operation']}
        receipt='https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-123'
        at='2026-10-07T02:00:00Z'
        comment={'id':123,'html_url':receipt,'created_at':at,'updated_at':at,
            'user':{'id':8227352,'login':'HemSoft','type':'User'},
            'body':'Approved. <!-- sfl-migration-approval:'+json.dumps(decision)+' -->'}
        comment_file=self.capture({'request_url':'https://api.github.com/repos/HemSoft/set-it-free-loop/issues/comments/123',
            'http_status':200,'observed_at':at,'comment':comment})
        capture.update(owner_receipt_url=receipt,owner_approval_evidence_url=self.capture(dict(decision,
            approved_at=at,owner_comment_evidence_url=comment_file)))
        capture['recovery_execution_evidence_url']=self.capture({**decision,'status':'completed','conclusion':'success',
            'exit_code':0,'started_at':at,'completed_at':at,'observed_at':at})
        path.write_text(json.dumps(capture));self.check()
        for mutate in (lambda c:c.pop('provider_responses'),
                       lambda c:c['provider_responses']['project']['data'].update(link=None)):
            changed=copy.deepcopy(capture);mutate(changed);path.write_text(json.dumps(changed))
            with self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(capture));comment['user']['login']='other'
        (DIRECTORY/comment_file).write_text(json.dumps({'request_url':'https://api.github.com/repos/HemSoft/set-it-free-loop/issues/comments/123',
            'http_status':200,'observed_at':at,'comment':comment}))
        with self.assertRaisesRegex(ValueError,'actual HemSoft comment'):self.check()

    def test_paused_supabase_preservation_requires_actual_project_metadata(self):
        row=next(r for r in self.rows if r['resource_kind']=='supabase_project')
        self.verify_source_ledger(int(row['repository_id']));self.check()
        path=DIRECTORY/row['smoke_evidence_url'];original=json.loads(path.read_text())
        for mutation in (lambda c:c.pop('provider_responses'),
                         lambda c:c['provider_responses']['project'].update(http_status=404),
                         lambda c:c['provider_responses']['project']['data'].update(status='ACTIVE_HEALTHY'),
                         lambda c:c['provider_responses']['project']['data'].update(organization_slug='other'),
                         lambda c:c['provider_responses']['project']['data'].update(ref='other'),
                         lambda c:c.update(runtime_actions=['resume']),
                         lambda c:c.update(owner_receipt_url='https://example.com/approval')):
            capture=copy.deepcopy(original);mutation(capture);path.write_text(json.dumps(capture))
            with self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_access_and_license_require_exact_raw_api_responses(self):
        row=self.complete_rollout();access=row['post_transfer_access'];self.check()
        permission_path=DIRECTORY/access['permission_evidence_url'];license_path=DIRECTORY/access['license_evidence_url']
        captures={permission_path:json.loads(permission_path.read_text()),license_path:json.loads(license_path.read_text())}
        cases=[(permission_path,lambda c:c.pop('request_url')),
            (permission_path,lambda c:c.update(request_url='https://api.github.com/repos/other/repo/collaborators/fhemmerrelias/permission')),
            (permission_path,lambda c:c['result']['user'].update(login='other')),
            (permission_path,lambda c:c.update(http_status=404)),
            (license_path,lambda c:c.pop('request_url')),(license_path,lambda c:c.pop('http_status')),
            (license_path,lambda c:c.pop('data')),(license_path,lambda c:c['data'].update(id=1)),
            (license_path,lambda c:c['data']['plan'].update(filled_seats=2))]
        for path,mutation in cases:
            capture=copy.deepcopy(captures[path]);mutation(capture);path.write_text(json.dumps(capture))
            with self.assertRaises(ValueError):self.check()
            path.write_text(json.dumps(captures[path]))
        self.check()

    def test_supabase_dashboard_metadata_binds_numeric_organization_to_slug(self):
        row=next(r for r in self.rows if r['resource_kind']=='supabase_project')
        self.verify_source_ledger(int(row['repository_id']));path=DIRECTORY/row['smoke_evidence_url']
        capture=json.loads(path.read_text());project=capture['provider_responses']['project']
        project['request_url']=project['request_url'].replace('/v1/','/platform/')
        project['data'].pop('organization_slug');project['data'].update(id=100,organization_id=200)
        capture['provider_responses']['organization']={'method':'GET','http_status':200,
            'request_url':'https://api.supabase.com/platform/organizations/tigijbtmewljfqdmdskh',
            'observed_at':project['observed_at'],'data':{'id':200,'slug':'tigijbtmewljfqdmdskh'}}
        path.write_text(json.dumps(capture));self.check()
        for value in (201,None):
            capture['provider_responses']['organization']['data']['id']=value;path.write_text(json.dumps(capture))
            with self.assertRaisesRegex(ValueError,'actual owner project metadata'):self.check()

    def test_final_inventory_derives_every_repository_from_raw_pages(self):
        at=validator.observed_time('2026-10-07T03:00:00Z','fixture')
        url='https://api.github.com/orgs/hemsoft-dev/repos?type=all&per_page=100'
        repo={'id':42,'full_name':'hemsoft-dev/addition','private':True,'visibility':'private',
            'archived':False,'default_branch':'main'}
        first={'method':'GET','request_url':url,'http_status':200,'observed_at':at.isoformat(),'data':[],
            'response_headers':{'Link':'<'+url+'&page=2>; rel="next"'}}
        second=dict(first,request_url=url+'&page=2',response_headers={},data=[repo])
        account={'owner':'hemsoft-dev','repositories':[repo],'pages':[first,second]}
        validator.validate_repository_enumeration(account,at)
        mutations=(lambda c:c.pop('pages'),lambda c:c.update(pages=c['pages'][:1]),
            lambda c:c.update(repositories=[]),lambda c:c['pages'][0].update(http_status=403),
            lambda c:c['pages'][0].update(request_url=url.replace('hemsoft-dev','other')),
            lambda c:c['pages'][1].update(request_url=url+'&page=3'),
            lambda c:c['pages'][0].update(response_headers={'Link':'<https://api.github.com/orgs/hemsoft-dev/repos?type=public&per_page=100&page=2>; rel="next"'}),
            lambda c:c['pages'][1].update(observed_at='2026-10-08T00:00:00Z'))
        for mutation in mutations:
            changed=copy.deepcopy(account);mutation(changed)
            with self.assertRaises(ValueError):validator.validate_repository_enumeration(changed,at)
        personal={'owner':'HemSoft','repositories':[],'pages':[dict(first,data=[],response_headers={},
            request_url='https://api.github.com/users/HemSoft/repos?per_page=100')]}
        with self.assertRaises(ValueError):validator.validate_repository_enumeration(personal,at)

    def test_release_verification_rejects_fabricated_success_and_changed_file(self):
        row=self.complete_rollout();path=DIRECTORY/row['release_download_verification_url'];download=json.loads(path.read_text())
        attestation=json.loads((DIRECTORY/download['attestation_evidence_url']).read_text());self.check()
        argv=tuple(attestation['argv']);original=copy.deepcopy(self.release_results[argv])
        self.release_verifier.stop()
        try:
            with patch.object(validator.subprocess,'run',return_value=subprocess.CompletedProcess(argv,1,'','signature invalid')) as process:
                with self.assertRaisesRegex(ValueError,'Independent release signature verification failed'):
                    validator.validate_release_download(row,DIRECTORY,row['repository_id'],row['destination'],
                        row['deployment_source'],row['deployment_sha'],row['manifest_version'])
                self.assertEqual(process.call_args.args[0],list(argv))
        finally:self.release_verifier.start()
        asset=pathlib.Path(attestation['asset_path']);asset.write_bytes(asset.read_bytes()+b'changed')
        with self.assertRaisesRegex(ValueError,'Actual downloaded file bytes'):self.check()
        self.release_results[argv]=original

    def test_release_verification_rejects_an_independently_different_statement(self):
        row=self.complete_rollout();download=json.loads((DIRECTORY/row['release_download_verification_url']).read_text())
        attestation=json.loads((DIRECTORY/download['attestation_evidence_url']).read_text());argv=tuple(attestation['argv'])
        self.release_results[argv]['verificationResult']['statement']['predicate']['databaseId']='2'
        with self.assertRaisesRegex(ValueError,'Independent cryptographic verification'):self.check()

    def test_missing_local_release_asset_is_downloaded_from_the_exact_canonical_release(self):
        name='gh-sfl_2.1.0-rc.15_linux_amd64';calls=[]
        def download(argv,**kwargs):
            calls.append(argv)
            self.assertEqual(argv[:8],['gh','release','download','v2.1.0-rc.15','--repo','hemsoft-dev/set-it-free-loop','--pattern',name])
            self.assertEqual(argv[8],'--dir');self.assertEqual(kwargs['timeout'],60)
            (pathlib.Path(argv[9])/name).write_bytes(b'synthetic download')
            return subprocess.CompletedProcess(argv,0,'','')
        with patch.object(validator.subprocess,'run',side_effect=download):
            with validator.canonical_release_asset('/missing/'+name,'hemsoft-dev/set-it-free-loop','2.1.0-rc.15',name) as asset:
                self.assertEqual(asset.name,name);self.assertEqual(asset.read_bytes(),b'synthetic download')
            self.assertFalse(asset.exists())
        self.assertEqual(len(calls),1)


    def test_source_refresh_lists_derive_from_successful_paginated_account_gets(self):
        self.complete_transfer_gates();self.check();path=DIRECTORY/self.matrix['pre_cutover_source_evidence_url']
        original=json.loads(path.read_text())
        for account_index in ('destination',0,1):
            for mutation in ('missing-pages','failed-get','omitted-next','mismatched-list','future-get','stale-get','wrong-endpoint'):
                capture=copy.deepcopy(original)
                account=capture['destination_account'] if account_index=='destination' else capture['accounts'][account_index]
                page=account['pages'][0]
                if mutation=='missing-pages':account.pop('pages')
                elif mutation=='failed-get':page['http_status']=403
                elif mutation=='omitted-next':page['response_headers']['Link']='<'+page['request_url']+'&page=2>; rel="next"'
                elif mutation=='mismatched-list':page['data'].append({'id':999,'full_name':account['owner']+'/omitted'})
                elif mutation=='future-get':page['observed_at']='2026-10-08T01:50:00Z'
                elif mutation=='stale-get':page['observed_at']='2026-10-07T01:47:00Z'
                else:page['request_url']='https://api.github.com/orgs/other/repos?type=all&per_page=100'
                path.write_text(json.dumps(capture))
                with self.subTest(account=account_index,mutation=mutation),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_consumer_terminal_head_rejects_missing_changed_or_early_default_ref(self):
        row=self.complete_rollout();self.check();reference=row['default_branch_evidence_url'];path=DIRECTORY/reference
        original=json.loads(path.read_text())
        row.pop('default_branch_evidence_url')
        with self.assertRaises(ValueError):self.check()
        row['default_branch_evidence_url']=reference
        for mutation in ('changed-head','early-get','wrong-ref','failed-get','wrong-repository','wrong-method'):
            capture=copy.deepcopy(original)
            if mutation=='changed-head':capture['data']['object']['sha']='e'*40
            elif mutation=='early-get':capture['observed_at']='2026-10-07T02:00:00Z'
            elif mutation=='wrong-ref':capture['data']['ref']='refs/heads/other'
            elif mutation=='failed-get':capture['http_status']=404
            elif mutation=='wrong-repository':capture['repository_id']=42
            else:capture['method']='POST'
            path.write_text(json.dumps(capture))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_unlinked_supabase_rejects_copied_baseline_without_current_provider_state(self):
        resource=self.matrix['account_resource_preservation'][0]
        baseline=next(p for p in json.loads((DIRECTORY/'supabase-provider-evidence.json').read_text())['projects'] if p['reference']==resource['resource_id'])
        capture={'resource_id':resource['resource_id'],'resource_owner':resource['resource_owner'],
            'state':'paused','project':baseline,'resource_changes_made':False,'operation':'read_only_preservation',
            'phase':'post_transfer','observed_at':'2026-10-07T05:00:00Z','database_actions':[],'credential_reads':False,
            'provider_responses':{'project':{'method':'GET','http_status':200,                'request_url':'https://api.supabase.com/v1/projects/'+resource['resource_id'],                'observed_at':'2026-10-07T04:30:00Z','data':{'ref':resource['resource_id'],'name':baseline['name'],                    'region':baseline['region'],'status':'INACTIVE','organization_slug':'tigijbtmewljfqdmdskh'}}}}
        resource.update(status='verified',post_transfer_evidence_url=self.capture(capture));path=DIRECTORY/resource['post_transfer_evidence_url']
        cutoff=validator.observed_time('2026-10-07T04:00:00Z','test')
        validator.validate_unlinked_supabase(resource,baseline,DIRECTORY,cutoff)
        for field,value in [('provider_responses',{}),('http_status',404),('status','ACTIVE'),('name','changed'),
                            ('ref','cevpnetigzotgstxxjpm'),('organization_slug','other'),('region','other'),
                            ('observed_at','2026-10-07T03:59:00Z'),('request_url','https://api.supabase.com/v1/projects/other')]:
            changed=copy.deepcopy(capture)
            if field=='provider_responses':changed[field]=value
            elif field in ('http_status','observed_at','request_url'):changed['provider_responses']['project'][field]=value
            else:changed['provider_responses']['project']['data'][field]=value
            path.write_text(json.dumps(changed))
            with self.subTest(field=field),self.assertRaises(ValueError):
                validator.validate_unlinked_supabase(resource,baseline,DIRECTORY,cutoff)


    def test_source_branches_require_complete_raw_pagination_and_fresh_pages(self):
        self.complete_transfer_gates()
        captured=json.loads((DIRECTORY/self.matrix['pre_cutover_source_evidence_url']).read_text())
        repo=self.inventory['repositories'][0]
        row=copy.deepcopy(next(r for a in captured['accounts'] for r in a['repositories'] if r['id']==repo['id'])['source_head'])
        url=row['branches_response']['request_url'];branches=row['branches_response'];original=copy.deepcopy(row)
        for mutation in ('no-pages','wrong-url','failed-get','omitted-next','wrong-list','stale-get','wrong-next-filter'):
            changed=copy.deepcopy(original);pages=changed['branches_response']['pages']
            if mutation=='no-pages':changed['branches_response'].pop('pages')
            elif mutation=='wrong-url':pages[0]['request_url']=url.replace(repo['full_name'],'other/repo')
            elif mutation=='failed-get':pages[0]['http_status']=403
            elif mutation=='omitted-next':pages[0]['response_headers']['Link']='<'+url+'&page=2>; rel="next"'
            elif mutation=='wrong-list':pages[0]['data'].append({'name':'hidden','commit':{'sha':'f'*40}})
            elif mutation=='stale-get':pages[0]['observed_at']='2026-10-07T01:47:00Z'
            else:pages[0]['response_headers']['Link']='<'+url.replace('per_page=100','per_page=30')+'&page=2>; rel="next"'
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.source_revision(changed,repo,validator.observed_time('2026-10-07T01:48:00Z','fixture'))
        branches['data'] += [{'name':'additional-'+str(i),'commit':{'sha':'f'*40}} for i in range(100)]
        first=copy.deepcopy(branches['pages'][0]);first['data']=copy.deepcopy(branches['data'][:100])
        first['response_headers']['Link']='<'+url+'&page=2>; rel="next"'
        second=copy.deepcopy(first);second.update(request_url=url+'&page=2',response_headers={},data=copy.deepcopy(branches['data'][100:]))
        branches['pages']=[first,second]
        self.assertEqual(len(validator.source_revision(row,repo)[2]),101)

    def test_final_onboarding_requires_current_default_head_after_all_evidence(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946,terminal_times={})
            reference=proof.pop('default_branch_evidence_url')
            with self.assertRaises(ValueError):validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946,terminal_times={})
            proof['default_branch_evidence_url']=reference;path=directory/reference;original=json.loads(path.read_text())
            for mutation in ('advanced','early','wrong-default-ref'):
                capture=copy.deepcopy(original)
                if mutation=='advanced':capture['data']['object']['sha']='b'*40
                elif mutation=='early':capture['observed_at']='2026-10-07T03:34:00Z'
                else:capture['data']['ref']='refs/heads/other'
                path.write_text(json.dumps(capture))
                with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                    validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946,terminal_times={})

    def test_codex_future_coverage_needs_raw_installation_pages_after_creation(self):
        original=json.loads((DIRECTORY/'codex-organization-installation-evidence.json').read_text())
        validator.validate_codex_installation_policy(original)
        for mutation in ('no-pages','failed-get','wrong-url','selected','suspended','omitted-next','wrong-total','wrong-account'):
            capture=copy.deepcopy(original);page=capture['installation_pages'][0]
            actual=next(i for i in page['data']['installations'] if i['app_id']==1144995)
            if mutation=='no-pages':capture.pop('installation_pages')
            elif mutation=='failed-get':page['http_status']=403
            elif mutation=='wrong-url':page['request_url']=page['request_url'].replace('hemsoft-dev','other')
            elif mutation=='selected':actual['repository_selection']='selected'
            elif mutation=='suspended':actual['suspended_at']='2026-10-07T01:00:00Z'
            elif mutation=='omitted-next':page['response_headers']['Link']='<'+page['request_url']+'&page=2>; rel="next"'
            elif mutation=='wrong-total':page['data']['total_count']+=1
            else:actual['account']['id']=42
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):validator.validate_codex_installation_policy(capture)
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            path=directory/proof['codex_installation_policy_evidence_url'];capture=json.loads(path.read_text())
            capture['installation_pages'][0]['observed_at']='2026-10-07T02:59:00Z';path.write_text(json.dumps(capture))
            with self.assertRaisesRegex(ValueError,'freshness boundary'):
                validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946,terminal_times={})

    def test_recovery_approval_cannot_authorize_a_different_execution(self):
        row=self.complete_provider();path=DIRECTORY/row['smoke_evidence_url'];smoke=json.loads(path.read_text())
        row['smoke_outcome']='approved_recovery';at='2026-10-07T02:00:00Z'
        operation={'command':'reconnect-approved-project','argv':['reconnect-approved-project',row['resource_id']]}
        decision={'repository_id':int(row['repository_id']),'repository':row['destination'],'provider':row['provider'],
            'resource_id':row['resource_id'],'disposition':'approved_recovery','reason':'Synthetic explicit remediation',
            'recovery_action':row['recovery_action'],'operation':operation}
        receipt='https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-123'
        comment_file=self.capture({'request_url':'https://api.github.com/repos/HemSoft/set-it-free-loop/issues/comments/123',
            'http_status':200,'observed_at':at,'comment':{'id':123,'html_url':receipt,'created_at':at,'updated_at':at,
                'user':{'id':8227352,'login':'HemSoft','type':'User'},'body':'Approved. <!-- sfl-migration-approval:'+json.dumps(decision)+' -->'}})
        execution={**decision,'status':'completed','conclusion':'success','exit_code':0,'started_at':at,'completed_at':at,'observed_at':at}
        reference=self.capture(execution)
        smoke.update(outcome='approved_recovery',recovery_success=True,approved_by='HemSoft',owner_receipt_url=receipt,
            owner_approval_evidence_url=self.capture(dict(decision,approved_at=at,owner_comment_evidence_url=comment_file)),
            recovery_operation=operation,recovery_execution_evidence_url=reference)
        path.write_text(json.dumps(smoke));self.check()
        for field,value in [('operation',{'command':'change-dns','argv':['change-dns','unapproved']}),
                            ('recovery_action','different action'),('exit_code',1),('resource_id','other'),
                            ('started_at','2026-10-07T01:59:00Z'),('completed_at','2026-10-07T02:01:00Z')]:
            capture=copy.deepcopy(execution);capture[field]=value;(DIRECTORY/reference).write_text(json.dumps(capture))
            with self.subTest(field=field),self.assertRaises(ValueError):self.check()


    def test_onboarding_manifest_derives_from_immutable_repository_contents(self):
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);proof=self.onboarding_fixture(directory)
            path=directory/proof['manifest_evidence_url'];original=json.loads(path.read_text())
            validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946,terminal_times={})
            for mutation in ('missing','absent','wrong-ref','wrong-tier','wrong-path','wrong-blob'):
                capture=copy.deepcopy(original)
                if mutation=='missing':capture.pop('contents_response')
                elif mutation=='absent':capture['contents_response']['http_status']=404
                elif mutation=='wrong-ref':capture['contents_response']['request_url']=capture['contents_response']['request_url'].replace('f'*40,'a'*40)
                elif mutation=='wrong-tier':
                    manifest=dict(proof['manifest_identity'],tier='full')
                    capture['contents_response']=self.file_response(proof['repository'],'f'*40,'.sfl/sfl.json',json.dumps(manifest).encode())
                elif mutation=='wrong-path':capture['manifest_path']='other.json'
                else:capture['contents_response']['data']['sha']='a'*40
                path.write_text(json.dumps(capture))
                with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                    validator.validate_final_onboarding(proof,directory,{},'hemsoft-dev',4448946,terminal_times={})
            path.write_text(json.dumps(original))

    def test_source_governance_derives_from_primary_repository_responses(self):
        row=self.complete_source();proof=row['in_place_evidence']
        repo=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id'])
        path=DIRECTORY/proof['governance_evidence_url'];original=json.loads(path.read_text())
        cutoff=validator.observed_time('2026-10-07T01:51:00Z','fixture')
        validator.validate_source_governance(proof,DIRECTORY,repo,cutoff)
        for mutation in ('missing-policy','wrong-policy','failed-policy','wrong-repo','early-policy','missing-labels','wrong-codeowners','wrong-revision'):
            capture=copy.deepcopy(original)
            if mutation=='missing-policy':capture.pop('actions_policy_response')
            elif mutation=='wrong-policy':capture['workflow_permissions_response']['data']={'default_workflow_permissions':'write'}
            elif mutation=='failed-policy':capture['actions_policy_response']['http_status']=403
            elif mutation=='wrong-repo':capture['actions_policy_response']['request_url']=capture['actions_policy_response']['request_url'].replace(repo['destination'],'hemsoft-dev/other')
            elif mutation=='early-policy':capture['actions_policy_response']['observed_at']='2026-10-07T01:50:00Z'
            elif mutation=='missing-labels':capture.pop('labels_pages')
            elif mutation=='wrong-codeowners':capture['codeowners_contents']['contents_response']=self.file_response(repo['destination'],proof['source_sha'],'.github/CODEOWNERS',b'* @other\n')
            else:capture['codeowners_contents']['revision_sha']='f'*40
            path.write_text(json.dumps(capture))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.validate_source_governance(proof,DIRECTORY,repo,cutoff)
        path.write_text(json.dumps(original))

    def test_transfer_event_derives_from_complete_successful_audit_export(self):
        self.complete_transfer_gates()
        row=next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/dashboard')
        repo=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id'])
        path=DIRECTORY/self.transfer_capture(row);original=json.loads(path.read_text())
        cutoff=validator.observed_time('2026-10-07T01:50:00Z','fixture')
        source=json.loads((DIRECTORY/self.matrix['pre_cutover_source_evidence_url']).read_text())
        heads=next(r['source_head'] for a in source['accounts'] for r in a['repositories'] if r['id']==repo['id'])
        revision=validator.source_revision(heads,repo)
        validator.validate_repository_transfer(path.name,DIRECTORY,repo,cutoff,revision)
        for mutation in ('missing','truncated','failed-download','wrong-route','different-export','bad-bytes','missing-event','invented-time','duplicate-event'):
            capture=copy.deepcopy(original);export=capture['audit_export']
            if mutation=='missing':capture.pop('audit_export')
            elif mutation=='truncated':export['verification_response']['data']['truncated']=True
            elif mutation=='failed-download':export['download_response']['http_status']=403
            elif mutation=='wrong-route':export['request']['request_url']=export['request']['request_url'].replace('hemsoft-dev','other')
            elif mutation=='different-export':export['download_response']['request_url_sha256']='d'*64
            elif mutation=='bad-bytes':export['download_response']['body_sha256']='0'*64
            elif mutation=='missing-event':capture['audit_export']=self.audit_export_fixture([],capture['observed_at'])
            elif mutation=='invented-time':capture['event']['@timestamp']+=1000;capture['event']['created_at']+=1000
            else:capture['audit_export']=self.audit_export_fixture([capture['event'],capture['event']],capture['observed_at'])
            path.write_text(json.dumps(capture))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.validate_repository_transfer(path.name,DIRECTORY,repo,cutoff,revision)
        path.write_text(json.dumps(original))

    def test_runner_job_derives_from_complete_successful_api_pages(self):
        self.complete_transfer_gates();row=next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/yahtzee')
        proof=row['post_transfer_runner'];path=DIRECTORY/proof['jobs_evidence_url'];original=json.loads(path.read_text())
        cutoff=validator.observed_time('2026-10-07T01:51:00Z','fixture')
        validator.validate_runner_captures(proof,DIRECTORY,cutoff)
        for mutation in ('missing','failed-get','wrong-attempt','omitted-next','wrong-total','wrong-runner','early-get'):
            capture=copy.deepcopy(original)
            if mutation=='missing':capture.pop('pages')
            elif mutation=='failed-get':capture['pages'][0]['http_status']=403
            elif mutation=='wrong-attempt':capture['pages'][0]['request_url']=capture['request_url'].replace('/attempts/1/','/attempts/2/')
            elif mutation=='omitted-next':capture['pages'][0]['response_headers']['Link']='<'+capture['request_url']+'&page=2>; rel="next"'
            elif mutation=='wrong-total':capture['pages'][0]['data']['total_count']=2
            elif mutation=='wrong-runner':capture['pages'][0]['data']['jobs'][0]['runner_id']=22
            else:capture['pages'][0]['observed_at']='2026-10-07T01:59:00Z'
            path.write_text(json.dumps(capture))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):validator.validate_runner_captures(proof,DIRECTORY,cutoff)
        capture=copy.deepcopy(original);job=capture['jobs'][0]
        capture['jobs']=[dict(job,id=n+2) for n in range(100)]+[job];capture['total_count']=101
        first=copy.deepcopy(capture['pages'][0]);first['data']={'total_count':101,'jobs':copy.deepcopy(capture['jobs'][:100])}
        first['response_headers']['Link']='<'+capture['request_url']+'&page=2>; rel="next"'
        second=copy.deepcopy(first);second.update(request_url=capture['request_url']+'&page=2',response_headers={})
        second['data']['jobs']=copy.deepcopy(capture['jobs'][100:]);capture['pages']=[first,second];path.write_text(json.dumps(capture))
        validator.validate_runner_captures(proof,DIRECTORY,cutoff)
        path.write_text(json.dumps(original))

    def test_owner_effective_approval_time_is_the_comment_edit(self):
        at='2026-10-07T02:00:00Z';later='2026-10-07T02:01:00Z'
        decision={'repository_id':42,'repository':'hemsoft-dev/example','disposition':'approved_recovery'}
        url='https://github.com/HemSoft/set-it-free-loop/issues/138#issuecomment-123'
        capture={'http_status':200,'request_url':'https://api.github.com/repos/HemSoft/set-it-free-loop/issues/comments/123',
            'observed_at':later,'comment':{'id':123,'html_url':url,'created_at':at,'updated_at':later,
                'user':{'id':8227352,'login':'HemSoft','type':'User'},'body':'Approved. <!-- sfl-migration-approval:'+json.dumps(decision)+' -->'}}
        reference=self.capture(capture);latest=validator.observed_time(later,'fixture')
        with self.assertRaises(ValueError):validator.validate_owner_approval_comment(reference,DIRECTORY,url,
            validator.observed_time(at,'fixture'),decision,latest)
        validator.validate_owner_approval_comment(reference,DIRECTORY,url,latest,decision,latest)

    def test_inherited_and_dynamic_secret_scope_cannot_use_unused_waiver(self):
        self.complete_transfer_gates();source=json.loads((DIRECTORY/self.matrix['pre_cutover_source_evidence_url']).read_text())
        scan_path=DIRECTORY/source['reference_scan_evidence_url'];original=json.loads(scan_path.read_text())
        unused=json.loads((DIRECTORY/'legacy-unused-credential-owner-evidence.json').read_text())['repositories'][0]
        repo=next(r for r in self.inventory['repositories'] if r['id']==unused['repository_id'])
        for workflow in ('jobs:\n  call:\n    uses: other/repo/.github/workflows/build.yml@main\n    secrets: inherit\n',
                         "jobs:\n  call:\n    uses: other/repo/.github/workflows/build.yml@main\n    secrets: 'inherit'\n",
                         "steps:\n  - run: echo '${{ secrets[inputs.key] }}'\n",
                         "steps:\n  - run: echo '${{ Secrets[format('KEY_{0}', inputs.key)] }}'\n",
                         "steps:\n  - run: echo '${{ toJSON(secrets) }}'\n"):
            scan=copy.deepcopy(original);row=next(r for r in scan['repositories'] if r['repository_id']==repo['id'])
            name='.github/workflows/inherited.yml';body=workflow.encode()
            file={'repository_id':repo['id'],'repository':repo['full_name'],'revision_sha':row['head_sha'],
                'path':name,'observed_at':row['observed_at'],'contents_response':self.file_response(repo['full_name'],row['head_sha'],name,body)}
            row['files'].append(file);row['tree_response']['data']['tree'].append({'path':name,'type':'blob','sha':file['contents_response']['data']['sha']})
            scan_path.write_text(json.dumps(scan))
            with self.subTest(workflow=workflow),self.assertRaisesRegex(ValueError,'Inherited or dynamic secret scope'):
                validator.validate_reference_scan(scan_path.name,DIRECTORY,self.inventory)
        scan_path.write_text(json.dumps(original))

    def test_non_default_workflow_trees_are_complete_and_reconcile_secrets(self):
        self.complete_transfer_gates();source=json.loads((DIRECTORY/self.matrix['pre_cutover_source_evidence_url']).read_text())
        scan_path=DIRECTORY/source['reference_scan_evidence_url'];scan=json.loads(scan_path.read_text())
        unused=json.loads((DIRECTORY/'legacy-unused-credential-owner-evidence.json').read_text())['repositories'][0]
        repo=next(r for r in self.inventory['repositories'] if r['id']==unused['repository_id'])
        row=next(r for r in scan['repositories'] if r['repository_id']==repo['id']);head='d'*40;tree='c'*40
        row['branches_response']['data'].append({'name':'only-other-branch','commit':{'sha':head}})
        row['branches_response']['pages'][0]['data']=copy.deepcopy(row['branches_response']['data'])
        base='https://api.github.com/repos/'+repo['full_name'];path='.github/workflows/other.yml'
        body=b'jobs:\n  test:\n    steps:\n      - run: echo ${{ secrets.SFL_APP_PRIVATE_KEY }}\n'
        file={'repository_id':repo['id'],'repository':repo['full_name'],'revision_sha':head,'path':path,
            'observed_at':row['observed_at'],'contents_response':self.file_response(repo['full_name'],head,path,body)}
        branch={'head_sha':head,'tree_sha':tree,'commit_response':{'http_status':200,'request_url':base+'/git/commits/'+head,
            'data':{'sha':head,'tree':{'sha':tree}}},'tree_response':{'http_status':200,
            'request_url':base+'/git/trees/'+tree+'?recursive=1','data':{'sha':tree,'truncated':False,
                'tree':[{'path':path,'type':'blob','sha':file['contents_response']['data']['sha']}]}},'files':[file]}
        row['branch_scans'].append(branch);row['referenced_secret_names']=['SFL_APP_PRIVATE_KEY']
        scan_path.write_text(json.dumps(scan))
        with self.assertRaisesRegex(ValueError,'newly referenced legacy credential'):
            validator.validate_reference_scan(scan_path.name,DIRECTORY,self.inventory)
        body=b'jobs:\n  test:\n    steps:\n      - run: echo safe\n'
        file['contents_response']=self.file_response(repo['full_name'],head,path,body)
        branch['tree_response']['data']['tree'][0]['sha']=file['contents_response']['data']['sha'];row['referenced_secret_names']=[]
        scan_path.write_text(json.dumps(scan));validator.validate_reference_scan(scan_path.name,DIRECTORY,self.inventory)
        original=copy.deepcopy(scan)
        for mutation in ('missing-branch','missing-workflow','wrong-commit','wrong-tree','wrong-file-head','truncated-tree'):
            changed=copy.deepcopy(original);target=next(r for r in changed['repositories'] if r['repository_id']==repo['id'])
            other=target['branch_scans'][-1]
            if mutation=='missing-branch':target['branch_scans'].pop()
            elif mutation=='missing-workflow':other['files']=[]
            elif mutation=='wrong-commit':other['commit_response']['data']['sha']='b'*40
            elif mutation=='wrong-tree':other['tree_response']['data']['sha']='b'*40
            elif mutation=='wrong-file-head':other['files'][0]['revision_sha']=row['head_sha']
            else:other['tree_response']['data']['truncated']=True
            scan_path.write_text(json.dumps(changed))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.validate_reference_scan(scan_path.name,DIRECTORY,self.inventory)

    def test_owned_app_check_executes_the_canonical_workflow_and_module(self):
        self.complete_transfer_gates();proof=self.matrix['pre_transfer_credential_verification']
        validator.validate_app_credential(proof,DIRECTORY)
        original=copy.deepcopy(proof['implementation_evidence']);proof.pop('implementation_evidence')
        with self.assertRaisesRegex(ValueError,'both reviewed canonical'):validator.validate_app_credential(proof,DIRECTORY)
        proof['implementation_evidence']=original
        for path in original:
            capture_path=DIRECTORY/original[path];capture=json.loads(capture_path.read_text());old=capture_path.read_text()
            capture['contents_response']=self.file_response(proof['repository'],proof['reviewed_sha'],path,b'Fabricated success output')
            capture_path.write_text(json.dumps(capture))
            with self.subTest(path=path),self.assertRaisesRegex(ValueError,'reviewed canonical source bytes'):
                validator.validate_app_credential(proof,DIRECTORY)
            capture_path.write_text(old)
        validator.validate_app_credential(proof,DIRECTORY)

    def test_pre_sync_manifest_paths_use_actual_immutable_contents_responses(self):
        row=self.complete_rollout();proof=row['pre_sync_installation'];path=DIRECTORY/proof['evidence_url']
        original=json.loads(path.read_text());validator.validate_pre_sync_installation(proof,DIRECTORY,row)
        for location in ('.sfl/sfl.json','sfl.json'):
            for mutation in ('missing','wrong-path','wrong-revision','wrong-status','invented-404'):
                changed=copy.deepcopy(original);response=changed['manifest_files'][location]['contents_response']
                if mutation=='missing':changed['manifest_files'][location].pop('contents_response')
                elif mutation=='wrong-path':response['request_url']=response['request_url'].replace('/contents/'+location,'/contents/other.json')
                elif mutation=='wrong-revision':response['request_url']=response['request_url'].replace(proof['revision_sha'],'f'*40)
                elif mutation=='wrong-status':response['http_status']=200
                else:response['data']={}
                path.write_text(json.dumps(changed))
                with self.subTest(location=location,mutation=mutation),self.assertRaises(ValueError):
                    validator.validate_pre_sync_installation(proof,DIRECTORY,row)
        path.write_text(json.dumps(original));validator.validate_pre_sync_installation(proof,DIRECTORY,row)

    def test_global_owner_facts_need_raw_identity_and_exact_structured_scope(self):
        self.check()

        for filename in ('provider-absence-owner-evidence.json','external-resource-owner-scope-evidence.json',
                         'legacy-unused-credential-owner-evidence.json'):
            record=json.loads((DIRECTORY/filename).read_text());path=DIRECTORY/record['owner_comment_evidence_url']
            original=json.loads(path.read_text())
            for mutation in ('wrong-author','wrong-id','wrong-request','failed-get','wrong-scope','missing-decision','backdated-edit'):
                capture=copy.deepcopy(original)
                if mutation=='wrong-author':capture['comment']['user']['login']='someone-else'
                elif mutation=='wrong-id':capture['comment']['user']['id']=42
                elif mutation=='wrong-request':capture['request_url']=capture['request_url'].replace('issues/comments/','pulls/comments/')
                elif mutation=='failed-get':capture['http_status']=403
                elif mutation=='wrong-scope':capture['comment']['body']='<!-- sfl-migration-approval:{"scope":"one repository"} -->'
                elif mutation=='missing-decision':capture['comment']['body']='No confirmation'
                else:capture['comment']['updated_at']='2026-10-07T02:00:00Z'
                path.write_text(json.dumps(capture))
                with self.subTest(filename=filename,mutation=mutation),self.assertRaises(ValueError):self.check()
            path.write_text(json.dumps(original))
        self.check()

    def test_committed_empty_tree_404_is_distinct_from_an_unavailable_tree(self):
        repo=next(r for r in self.inventory['repositories'] if r['full_name']=='fhemmer/hs-cli-confluence-search')
        head='e'*40;tree=hashlib.sha1(b'tree 0\0').hexdigest();base='https://api.github.com/repos/'+repo['full_name']
        branch={'head_sha':head,'tree_sha':tree,'files':[],'tree_response':{'request_url':base+'/git/trees/'+tree+'?recursive=1',
            'http_status':404,'data':{'message':'Not Found','status':'404'}}}
        at=validator.observed_time('2026-10-07T02:00:00Z','fixture')
        self.assertEqual(validator.validate_branch_reference_files(branch,repo,at),(set(),False,[]))
        for mutation in ('different-tree','unauthorized','wrong-endpoint','wrong-response','invented-file'):
            changed=copy.deepcopy(branch)
            if mutation=='different-tree':changed['tree_sha']='f'*40;changed['tree_response']['request_url']=base+'/git/trees/'+'f'*40+'?recursive=1'
            elif mutation=='unauthorized':changed['tree_response']['http_status']=403
            elif mutation=='wrong-endpoint':changed['tree_response']['request_url']=base+'/git/trees/'+head+'?recursive=1'
            elif mutation=='wrong-response':changed['tree_response']['data']={'message':'Permission denied'}
            else:changed['files']=[{'path':'.github/workflows/invented.yml'}]
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.validate_branch_reference_files(changed,repo,at)


    def test_registered_artifacts_require_resource_specific_successful_gets(self):
        row=self.complete_rollout();validator.registered_review_external_id(row,DIRECTORY,row['repository_id'],row['destination'])
        for field in ('review_registration_url','review_registry_status_url','review_artifact_url'):
            path=DIRECTORY/row['review_operation_receipts'][field]['capture_evidence_url'];original=json.loads(path.read_text())
            for mutation in ('missing','wrong-endpoint','failed-get','different-data'):
                value=copy.deepcopy(original)
                if field=='review_registry_status_url':
                    if mutation=='missing':value.pop('status_pages')
                    elif mutation=='wrong-endpoint':value['status_pages'][0]['request_url']=value['status_pages'][0]['request_url'].replace(row['review_head_sha'],'f'*40)
                    elif mutation=='failed-get':value['status_pages'][0]['http_status']=403
                    else:value['status_pages'][0]['data'][0]['id']=99
                elif mutation=='missing':value.pop('request_url')
                elif mutation=='wrong-endpoint':value['request_url']=value['request_url'].replace('/issues/comments/','/pulls/comments/')
                elif mutation=='failed-get':value['http_status']=403
                else:value['data']['body']='Fabricated summary'
                path.write_text(json.dumps(value))
                with self.subTest(field=field,mutation=mutation),self.assertRaises(ValueError):
                    validator.registered_review_external_id(row,DIRECTORY,row['repository_id'],row['destination'])
            path.write_text(json.dumps(original))

    def test_effective_policy_sources_need_exact_gets_and_terminal_freshness(self):
        row=self.complete_rollout();proof=row['gate_policy'];path=DIRECTORY/proof['evidence_url'];original=json.loads(path.read_text())
        at=validator.observed_time(original['observed_at'],'fixture')
        for field in ('effective_rules','classic_protection'):
            for mutation in ('missing-method','wrong-branch','failed-get','stale-response'):
                value=copy.deepcopy(original)
                if mutation=='missing-method':value[field].pop('method')
                elif mutation=='wrong-branch':value[field]['request_url']=value[field]['request_url'].replace('/main','/other')
                elif mutation=='failed-get':value[field]['http_status']=403
                else:value[field]['observed_at']='2026-10-07T01:00:00Z'
                path.write_text(json.dumps(value))
                with self.subTest(field=field,mutation=mutation),self.assertRaises(ValueError):
                    validator.validate_gate_policy(proof,DIRECTORY,row['repository_id'],row['destination'],'main',at)
        path.write_text(json.dumps(original));validator.validate_gate_policy(proof,DIRECTORY,row['repository_id'],row['destination'],'main',at)

    def test_requester_permission_is_derived_from_its_exact_collaborator_get(self):
        row=self.complete_rollout();path=DIRECTORY/row['requester_permission_evidence_url'];original=json.loads(path.read_text())
        for mutation in ('missing-method','wrong-actor','wrong-repository','different-data'):
            value=copy.deepcopy(original)
            if mutation=='missing-method':value.pop('method')
            elif mutation=='wrong-actor':value['request_url']=value['request_url'].replace('/'+row['review_requester']+'/permission','/other/permission')
            elif mutation=='wrong-repository':value['request_url']=value['request_url'].replace(row['destination'],'hemsoft-dev/other')
            else:value['data']={'permission':'read'}
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.validate_registered_review(row,DIRECTORY,row['destination'],target_branch='main',deployment_revision='b'*40)
        path.write_text(json.dumps(original))

    def broken_observer_scenario(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0]
        proof=pilot['validation_evidence'];result=proof['scenario_receipts']['findings'];path=DIRECTORY/result['capture_evidence_url']
        capture=json.loads(path.read_text());workflow_path=DIRECTORY/capture['workflow_evidence_url'];workflow=json.loads(workflow_path.read_text())
        marker='// BEGIN TESTABLE REQUESTER AUTHORIZATION';assert marker in workflow['content']
        workflow['content']=workflow['content'].replace(marker,marker+"\nthrow new Error('synthetic broken observer');",1)
        workflow['contents_response']=self.file_response(pilot['repository'],'b'*40,workflow['path'],workflow['content'].encode())
        workflow_path.write_text(json.dumps(workflow));output_path=DIRECTORY/capture['output_evidence_url'];output=json.loads(output_path.read_text())
        output['workflow_sha256']=hashlib.sha256(workflow['content'].encode()).hexdigest();output_path.write_text(json.dumps(output))
        capture['output_sha256']=hashlib.sha256(output_path.read_bytes()).hexdigest();path.write_text(json.dumps(capture))
        return pilot,proof,result

    def test_fixture_success_is_reproduced_against_the_actual_observer(self):
        pilot,proof,result=self.broken_observer_scenario()
        with self.assertRaisesRegex(ValueError,'Independent observer fixture execution'):
            validator.validate_pilot_scenario(result,'findings',DIRECTORY,pilot['repository_id'],pilot['repository'],
                proof['deployment_sha'],proof['release_version'],'b'*40)

    def test_fixture_cannot_execute_added_host_access_code(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0]
        proof=pilot['validation_evidence'];result=proof['scenario_receipts']['findings']
        path=DIRECTORY/result['capture_evidence_url'];capture=json.loads(path.read_text())
        workflow_path=DIRECTORY/capture['workflow_evidence_url'];workflow=json.loads(workflow_path.read_text())
        with tempfile.TemporaryDirectory(prefix='sfl-host-access-test-') as folder:
            sentinel=pathlib.Path(folder)/'injected.txt';marker='// BEGIN TESTABLE REQUESTER AUTHORIZATION'
            code="\nprocess.getBuiltinModule('node:fs').writeFileSync("+json.dumps(str(sentinel))+", 'synthetic marker');"
            workflow['content']=workflow['content'].replace(marker,marker+code,1)
            workflow['contents_response']=self.file_response(pilot['repository'],'b'*40,workflow['path'],workflow['content'].encode())
            workflow_path.write_text(json.dumps(workflow));output_path=DIRECTORY/capture['output_evidence_url'];output=json.loads(output_path.read_text())
            output['workflow_sha256']=hashlib.sha256(workflow['content'].encode()).hexdigest();output_path.write_text(json.dumps(output))
            capture['output_sha256']=hashlib.sha256(output_path.read_bytes()).hexdigest();path.write_text(json.dumps(capture))
            with self.assertRaisesRegex(ValueError,'reviewed canonical code blocks'):
                validator.validate_pilot_scenario(result,'findings',DIRECTORY,pilot['repository_id'],pilot['repository'],
                    proof['deployment_sha'],proof['release_version'],'b'*40)
            self.assertFalse(sentinel.exists())

    def test_fixture_child_receives_no_parent_credentials_or_node_options(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0]
        proof=pilot['validation_evidence'];result=proof['scenario_receipts']['findings'];real_run=subprocess.run
        validator.replay_observer_fixture.cache_clear();self.addCleanup(validator.replay_observer_fixture.cache_clear)
        with tempfile.TemporaryDirectory(prefix='sfl-child-environment-test-') as folder:
            checker=pathlib.Path(folder)/'check-environment.mjs'
            checker.write_text("for (const name of ['GH_TOKEN','GITHUB_TOKEN','SFL_APP_PRIVATE_KEY','OPENAI_API_KEY','NODE_OPTIONS']) { if (process.env[name]) throw new Error('Parent environment was inherited'); }\n")
            def checked_run(argv,**kwargs):
                return real_run([argv[0],'--import',checker.as_uri(),*argv[1:]],**kwargs)
            inherited={key:'synthetic marker' for key in ('GH_TOKEN','GITHUB_TOKEN','SFL_APP_PRIVATE_KEY','OPENAI_API_KEY')}
            inherited['NODE_OPTIONS']='--trace-deprecation'
            with patch.dict(os.environ,inherited),patch.object(validator.subprocess,'run',side_effect=checked_run) as execution:
                validator.validate_pilot_scenario(result,'findings',DIRECTORY,pilot['repository_id'],pilot['repository'],
                    proof['deployment_sha'],proof['release_version'],'b'*40)
                execution.assert_called_once()

    def test_pre_cutover_secret_names_are_complete_current_and_reconciled(self):
        self.complete_transfer_gates();credential=self.matrix['pre_transfer_credential_verification'];ref=self.matrix['pre_cutover_source_evidence_url']
        path=DIRECTORY/ref;original=json.loads(path.read_text());validator.validate_source_refresh(ref,DIRECTORY,self.inventory,credential)
        for mutation in ('missing','failed-get','stale','new-secret','omitted-next'):
            value=copy.deepcopy(original);row=value['accounts'][0]['repositories'][0];page=row['secret_pages'][0]
            if mutation=='missing':row.pop('secret_pages')
            elif mutation=='failed-get':page['http_status']=403
            elif mutation=='stale':page['observed_at']='2026-10-07T01:00:00Z'
            elif mutation=='new-secret':page['data']['secrets'].append({'name':'NEW_LEGACY_KEY'});page['data']['total_count']+=1
            else:page['response_headers']['Link']='<'+page['request_url']+'&page=2>; rel="next"'
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.validate_source_refresh(ref,DIRECTORY,self.inventory,credential)
        path.write_text(json.dumps(original))

    def test_pre_cutover_runner_enumeration_is_fresh_complete_and_reconciled(self):
        self.complete_transfer_gates();credential=self.matrix['pre_transfer_credential_verification'];ref=self.matrix['pre_cutover_source_evidence_url']
        path=DIRECTORY/ref;original=json.loads(path.read_text());validator.validate_source_refresh(ref,DIRECTORY,self.inventory,credential)
        for mutation in ('missing','failed-get','stale','new-runner','omitted-next'):
            value=copy.deepcopy(original);row=value['accounts'][0]['repositories'][0];page=row['runner_pages'][0]
            if mutation=='missing':row.pop('runner_pages')
            elif mutation=='failed-get':page['http_status']=403
            elif mutation=='stale':page['observed_at']='2026-10-07T01:00:00Z'
            elif mutation=='new-runner':page['data']['runners'].append({'id':999,'name':'unreviewed-runner'});page['data']['total_count']+=1
            else:page['response_headers']['Link']='<'+page['request_url']+'&page=2>; rel="next"'
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.validate_source_refresh(ref,DIRECTORY,self.inventory,credential)
        path.write_text(json.dumps(original))

    def test_terminal_gate_run_and_check_need_exact_successful_gets(self):
        row=self.complete_rollout();path=DIRECTORY/row['review_operation_receipts']['gate_run_url']['capture_evidence_url']
        original=json.loads(path.read_text())
        for field in ('run_response','check_response'):
            for mutation in ('missing','failed-get','wrong-resource','different-data'):
                value=copy.deepcopy(original)
                if mutation=='missing':value.pop(field)
                elif mutation=='failed-get':value[field]['http_status']=403
                elif mutation=='wrong-resource':value[field]['request_url']=value[field]['request_url'].replace('/1','/2')
                else:value[field]['data']['conclusion']='failure'
                path.write_text(json.dumps(value))
                with self.subTest(field=field,mutation=mutation),self.assertRaises(ValueError):
                    validator.validate_registered_review(row,DIRECTORY,row['destination'],target_branch='main',deployment_revision='b'*40)
        path.write_text(json.dumps(original))

    def test_reviewed_pr_context_derives_from_its_actual_get(self):
        row=self.complete_rollout();path=DIRECTORY/row['review_pr_metadata_evidence_url'];original=json.loads(path.read_text())
        for mutation in ('missing-method','failed-get','different-base','different-head'):
            value=copy.deepcopy(original)
            if mutation=='missing-method':value.pop('method')
            elif mutation=='failed-get':value['http_status']=403
            elif mutation=='different-base':value['data']['base']['ref']='unreviewed-branch'
            else:value['data']['head']['sha']='f'*40
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.validate_registered_review(row,DIRECTORY,row['destination'],target_branch='main',deployment_revision='b'*40)
        path.write_text(json.dumps(original))

    def test_retained_dependency_waiver_loads_the_actual_owner_comment(self):
        self.complete_transfer_gates();self.check()
        row=next(r for r in self.matrix['repositories'] if r['repository_id'] in validator.APPROVED_RETAINED_IDS)
        path=DIRECTORY/row['retained_app_dependency']['owner_comment_evidence_url'];original=json.loads(path.read_text())
        for mutation in ('missing-method','wrong-owner','wrong-comment','different-decision','failed-get'):
            value=copy.deepcopy(original)
            if mutation=='missing-method':value.pop('method')
            elif mutation=='wrong-owner':value['comment']['user']['id']=42
            elif mutation=='wrong-comment':value['request_url']=value['request_url'].replace('6028536416','1')
            elif mutation=='different-decision':value['comment']['body']=value['comment']['body'].replace('No SFL App dependency','Active SFL App dependency')
            else:value['http_status']=403
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original))

    def test_protection_preservation_derives_complete_primary_collections(self):
        self.complete_transfer_gates();row=next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/dashboard')
        repo=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id']);proof=copy.deepcopy(row['destination_protections'])
        rule={'id':990099,'name':'preserved-extra','target':'branch','enforcement':'active','conditions':{},'rules':[],'bypass_actors':[]}
        proof['rulesets'].append(rule);proof['classic']['release/smoke']={'required_status_checks':{'strict':True,'contexts':['legacy'],'checks':[{'context':'legacy','app_id':15368}]}}
        proof['evidence_url']=self.capture(dict(proof,phase='post_transfer'));path=DIRECTORY/proof['evidence_url'];original=json.loads(path.read_text())
        cutoff=validator.observed_time('2026-10-07T01:50:30Z','fixture')
        validator.validate_protection_preservation(proof,DIRECTORY,repo,cutoff)
        for mutation in ('missing-rules','failed-detail','unlisted-detail','omitted-branch-page','different-classic','wrong-repository','stale-response'):
            value=copy.deepcopy(original)
            if mutation=='missing-rules':value.pop('ruleset_pages')
            elif mutation=='failed-detail':value['ruleset_responses']['990099']['http_status']=403
            elif mutation=='unlisted-detail':value['ruleset_responses']['990099']['data']['id']=990098
            elif mutation=='omitted-branch-page':value['protected_branch_pages'][0]['response_headers']['Link']='<'+value['protected_branch_pages'][0]['request_url']+'&page=2>; rel="next"'
            elif mutation=='different-classic':value['classic_responses']['release/smoke']['data']={}
            elif mutation=='wrong-repository':value['repository_response']['data']['id']=42
            else:value['revision_response']['observed_at']='2026-10-07T01:49:00Z'
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):validator.validate_protection_preservation(proof,DIRECTORY,repo,cutoff)
        path.write_text(json.dumps(original))

    def test_wider_execution_needs_its_actual_successful_run_get(self):
        row=self.complete_source();proof=row['in_place_evidence'];operation=proof['workflow_operation_receipts'][0]
        path=DIRECTORY/operation['capture_evidence_url'];original=json.loads(path.read_text())
        for mutation in ('missing','failed-get','wrong-run','different-data'):
            value=copy.deepcopy(original)
            if mutation=='missing':value.pop('run_response')
            elif mutation=='failed-get':value['run_response']['http_status']=403
            elif mutation=='wrong-run':value['run_response']['request_url']=value['run_response']['request_url'].replace('/1','/2')
            else:value['run_response']['data']['conclusion']='failure'
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                validator.bound_workflow_operation(operation,DIRECTORY,row['repository_id'],row['destination'],
                    proof['source_sha'],proof['release_version'],operation['evidence_url'],{operation['workflow']},proof['source_sha'])
        path.write_text(json.dumps(original))

    def test_transfer_rechecks_every_destination_branch_against_the_scan(self):
        self.complete_transfer_gates();row=next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/dashboard')
        repo=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id']);path=DIRECTORY/self.transfer_capture(row)
        receipt=json.loads(path.read_text());head_path=DIRECTORY/receipt['destination_heads_evidence_url'];original=json.loads(head_path.read_text())
        source=json.loads((DIRECTORY/self.matrix['pre_cutover_source_evidence_url']).read_text())
        scanned=next(r['source_head'] for a in source['accounts'] for r in a['repositories'] if r['id']==repo['id'])
        revision=validator.source_revision(scanned,repo);cutoff=validator.observed_time('2026-10-07T01:50:00Z','fixture')
        validator.validate_repository_transfer(path.name,DIRECTORY,repo,cutoff,revision)
        for mutation in ('new-branch','changed-head','stale-heads','wrong-repository','omitted-page'):
            value=copy.deepcopy(original);branches=value['branches_response'];page=branches['pages'][0]
            if mutation=='new-branch':branches['data'].append({'name':'unreviewed','commit':{'sha':'d'*40}});page['data']=copy.deepcopy(branches['data'])
            elif mutation=='changed-head':branches['data'][0]['commit']['sha']='d'*40;page['data']=copy.deepcopy(branches['data'])
            elif mutation=='stale-heads':value['observed_at']='2026-10-07T01:49:00Z'
            elif mutation=='wrong-repository':value['repository_response']['data']['id']=42
            else:page['response_headers']['Link']='<'+page['request_url']+'&page=2>; rel="next"'
            head_path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):validator.validate_repository_transfer(path.name,DIRECTORY,repo,cutoff,revision)
        head_path.write_text(json.dumps(original))


    def test_final_pilot_policy_needs_exact_fresh_primary_responses(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0]
        proof=pilot['validation_evidence'];operations=proof['operation_receipts']
        times={key:validator.validate_terminal_operation(operations[key],DIRECTORY) for key in (
            'init_pr_url','repeat_onboarding_evidence_url','sync_pr_url','repeat_sync_evidence_url',
            'status_evidence_url','gate_uninstall_evidence_url')}
        path=DIRECTORY/proof['final_gate_policy_evidence_url'];original=json.loads(path.read_text())
        def check():validator.validate_pilot_cleanup(proof,DIRECTORY,pilot['repository_id'],pilot['repository'],'main',operations,times)
        check()
        for field in ('effective_rules','classic_protection'):
            for mutation in ('missing-method','wrong-branch','failed-get','stale-response','future-response'):
                value=copy.deepcopy(original)
                if mutation=='missing-method':value[field].pop('method')
                elif mutation=='wrong-branch':value[field]['request_url']=value[field]['request_url'].replace('/main','/other')
                elif mutation=='failed-get':value[field]['http_status']=403
                else:value[field]['observed_at']='2026-10-07T01:00:00Z' if mutation=='stale-response' else '2026-10-07T04:00:00Z'
                path.write_text(json.dumps(value))
                with self.subTest(field=field,mutation=mutation),self.assertRaises(ValueError):check()
        path.write_text(json.dumps(original));check()

    def test_live_scenario_run_requires_exact_fresh_primary_get(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0]
        result,capture=self.bind_live_scenario(pilot);proof=pilot['validation_evidence']
        path=DIRECTORY/result['capture_evidence_url'];original=json.loads(path.read_text())
        def check():validator.validate_pilot_scenario(result,'findings',DIRECTORY,pilot['repository_id'],pilot['repository'],proof['deployment_sha'],proof['release_version'],'b'*40)
        check()
        for mutation in ('missing','wrong-endpoint','failed-get','different-run','stale-response','future-response'):
            value=copy.deepcopy(original)
            if mutation=='missing':value.pop('run_response')
            elif mutation=='wrong-endpoint':value['run_response']['request_url']=value['run_response']['request_url'].replace('/99','/98')
            elif mutation=='failed-get':value['run_response']['http_status']=403
            elif mutation=='different-run':value['run_response']['data']['id']=98
            else:value['run_response']['observed_at']='2026-10-07T01:00:00Z' if mutation=='stale-response' else '2026-10-07T04:00:00Z'
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):check()
        path.write_text(json.dumps(original));check()

    def test_actions_artifact_metadata_requires_exact_fresh_primary_get(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0]
        result,capture=self.bind_live_scenario(pilot);proof=pilot['validation_evidence']
        path=DIRECTORY/capture['artifact_evidence_url'];original=json.loads(path.read_text())
        def check():validator.validate_pilot_scenario(result,'findings',DIRECTORY,pilot['repository_id'],pilot['repository'],proof['deployment_sha'],proof['release_version'],'b'*40)
        check()
        for mutation in ('missing','wrong-endpoint','failed-get','different-data','stale-response','future-response'):
            value=copy.deepcopy(original)
            if mutation=='missing':value.pop('artifact_response')
            elif mutation=='wrong-endpoint':value['artifact_response']['request_url']=value['artifact_response']['request_url'].replace('/100','/101')
            elif mutation=='failed-get':value['artifact_response']['http_status']=403
            elif mutation=='different-data':value['artifact_response']['data']['digest']='sha256:'+'f'*64
            else:value['artifact_response']['observed_at']='2026-10-07T01:00:00Z' if mutation=='stale-response' else '2026-10-07T04:00:00Z'
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):check()
        path.write_text(json.dumps(original));check()


    def test_fresh_source_protections_require_complete_current_primary_gets(self):
        self.complete_transfer_gates();credential=self.matrix['pre_transfer_credential_verification']
        ref=self.matrix['pre_cutover_source_evidence_url'];path=DIRECTORY/ref;original=json.loads(path.read_text())
        def check():validator.validate_source_refresh(ref,DIRECTORY,self.inventory,credential)
        check();row=original['accounts'][0]['repositories'][0]
        policy_path=DIRECTORY/row['protection_evidence_url'];baseline=json.loads(policy_path.read_text())
        for mutation in ('missing-evidence','failed-page','wrong-endpoint','stale-response','new-rule'):
            source=copy.deepcopy(original);policy=copy.deepcopy(baseline)
            if mutation=='missing-evidence':source['accounts'][0]['repositories'][0].pop('protection_evidence_url')
            elif mutation=='failed-page':policy['ruleset_pages'][0]['http_status']=403
            elif mutation=='wrong-endpoint':policy['ruleset_pages'][0]['request_url']=policy['ruleset_pages'][0]['request_url'].replace('/rulesets','/invented')
            elif mutation=='stale-response':policy['ruleset_pages'][0]['observed_at']='2026-10-07T01:00:00Z'
            else:
                rule={'id':999,'name':'new source rule','target':'branch','enforcement':'active','conditions':{},'rules':[{'type':'deletion'}],'bypass_actors':[]}
                policy['ruleset_pages'][0]['data'].append({'id':999})
                policy['ruleset_responses']['999']={'method':'GET','http_status':200,'observed_at':policy['observed_at'],
                    'request_url':'https://api.github.com/repos/'+row['full_name']+'/rulesets/999','data':rule}
            path.write_text(json.dumps(source));policy_path.write_text(json.dumps(policy))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):check()
        path.write_text(json.dumps(original));policy_path.write_text(json.dumps(baseline));check()


    def test_tag_only_commits_are_part_of_the_complete_credential_scan(self):
        self.complete_transfer_gates();ref=self.matrix['pre_cutover_source_evidence_url']
        source=json.loads((DIRECTORY/ref).read_text());scan_ref=source['reference_scan_evidence_url']
        path=DIRECTORY/scan_ref;original=json.loads(path.read_text());value=copy.deepcopy(original)
        row=next(r for r in value['repositories'] if r['source']=='HemSoft/dashboard');repo=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id'])
        row['tag_refs_response']['data']=[{'ref':'refs/tags/tag-only','object':{'type':'commit','sha':'d'*40}}]
        path.write_text(json.dumps(value))
        with self.assertRaisesRegex(ValueError,'branch and tagged commit'):validator.validate_reference_scan(scan_ref,DIRECTORY,self.inventory)
        row['branch_scans'].append({'head_sha':'d'*40,'tree_sha':'f'*40,
            'commit_response':{'http_status':200,'request_url':'https://api.github.com/repos/'+repo['full_name']+'/git/commits/'+'d'*40,'data':{'sha':'d'*40,'tree':{'sha':'f'*40}}},
            'tree_response':{'http_status':200,'request_url':'https://api.github.com/repos/'+repo['full_name']+'/git/trees/'+'f'*40+'?recursive=1','data':{'sha':'f'*40,'truncated':False,'tree':[]}},'files':[]})
        path.write_text(json.dumps(value));validator.validate_reference_scan(scan_ref,DIRECTORY,self.inventory)
        path.write_text(json.dumps(original))

    def test_tag_ref_capture_and_annotation_resolution_are_authenticated(self):
        self.complete_transfer_gates();source=json.loads((DIRECTORY/self.matrix['pre_cutover_source_evidence_url']).read_text())
        row=copy.deepcopy(source['accounts'][0]['repositories'][0]['source_head'])
        repo=next(r for r in self.inventory['repositories'] if r['full_name']==source['accounts'][0]['repositories'][0]['full_name'])
        base='https://api.github.com/repos/'+repo['full_name'];at=row['observed_at']
        row['tag_refs_response']['data']=[{'ref':'refs/tags/release','object':{'type':'tag','sha':'a'*40}}]
        row['tag_object_responses']={'a'*40:{'method':'GET','http_status':200,'observed_at':at,'request_url':base+'/git/tags/'+'a'*40,
            'data':{'sha':'a'*40,'object':{'type':'commit','sha':row['head_sha']}}}}
        tags=validator.source_revision(row,repo)[3];self.assertEqual(tags['refs/tags/release']['commit_sha'],row['head_sha'])
        for mutation in ('missing-refs','failed-get','wrong-endpoint','stale-ref','missing-annotation','different-object','cyclic-annotation'):
            value=copy.deepcopy(row)
            if mutation=='missing-refs':value.pop('tag_refs_response')
            elif mutation=='failed-get':value['tag_refs_response']['http_status']=403
            elif mutation=='wrong-endpoint':value['tag_refs_response']['request_url']=base+'/git/matching-refs/heads/'
            elif mutation=='stale-ref':value['tag_refs_response']['observed_at']='2026-10-07T01:00:00Z'
            elif mutation=='missing-annotation':value['tag_object_responses'].clear()
            elif mutation=='different-object':value['tag_object_responses']['a'*40]['data']['sha']='b'*40
            else:value['tag_object_responses']['a'*40]['data']['object']={'type':'tag','sha':'a'*40}
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):validator.source_revision(value,repo,validator.observed_time('2026-10-07T01:48:00Z','fixture'))

    def test_new_or_retargeted_tags_require_rescan_at_refresh_and_transfer(self):
        self.complete_transfer_gates();credential=self.matrix['pre_transfer_credential_verification'];ref=self.matrix['pre_cutover_source_evidence_url']
        path=DIRECTORY/ref;original=json.loads(path.read_text());row=next(r for a in original['accounts'] for r in a['repositories'] if r['full_name']=='HemSoft/dashboard')
        value=copy.deepcopy(original);changed=next(r for a in value['accounts'] for r in a['repositories'] if r['id']==row['id'])
        changed['source_head']['tag_refs_response']['data']=[{'ref':'refs/tags/new','object':{'type':'commit','sha':row['source_head']['head_sha']}}]
        path.write_text(json.dumps(value))
        with self.assertRaisesRegex(ValueError,'changed after reference scan'):validator.validate_source_refresh(ref,DIRECTORY,self.inventory,credential)
        path.write_text(json.dumps(original))
        rollout=next(r for r in self.matrix['repositories'] if r['repository_id']==row['id']);repo=next(r for r in self.inventory['repositories'] if r['id']==row['id'])
        transfer_ref=self.transfer_capture(rollout);transfer=json.loads((DIRECTORY/transfer_ref).read_text());heads_path=DIRECTORY/transfer['destination_heads_evidence_url'];heads=json.loads(heads_path.read_text())
        heads['tag_refs_response']['data']=[{'ref':'refs/tags/new','object':{'type':'commit','sha':row['source_head']['head_sha']}}];heads_path.write_text(json.dumps(heads))
        with self.assertRaisesRegex(ValueError,'changed after the credential/reference scan'):validator.validate_repository_transfer(transfer_ref,DIRECTORY,repo,validator.observed_time('2026-10-07T01:50:00Z','fixture'),validator.source_revision(row['source_head'],repo))


    def test_repeat_commands_need_concrete_successful_noop_executions(self):
        self.complete_pilots()
        operations=[p['validation_evidence']['operation_receipts'][key]
            for p in self.matrix['disposable_validation_repositories'] for key in ('repeat_onboarding_evidence_url','repeat_sync_evidence_url')]
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);onboarding=self.onboarding_fixture(directory)
            cases=[(operation,DIRECTORY) for operation in operations]+[
                (onboarding['onboarding_operation_receipts'][key],directory)
                for key in ('repeat_onboarding_url','repeat_sync_url')]
            for operation,where in cases:
                path=where/operation['capture_evidence_url'];original=json.loads(path.read_text())
                validator.validate_terminal_operation(operation,where)
                for mutation in ('missing','wrong-command','wrong-target','failed','early-completion','changed-output','bad-digest'):
                    value=copy.deepcopy(original)
                    if mutation=='missing':value.pop('execution')
                    elif mutation=='wrong-command':value['execution']['argv'][2]='status'
                    elif mutation=='wrong-target':value['execution']['argv'][4]='hemsoft-dev/other'
                    elif mutation=='failed':value['execution']['exit_code']=1
                    elif mutation=='early-completion':value['execution']['completed_at']='2026-10-07T00:00:00Z'
                    elif mutation=='changed-output':
                        value['execution']['stdout']='Created a pull request\n'
                        value['execution']['stdout_sha256']=hashlib.sha256(value['execution']['stdout'].encode()).hexdigest()
                    else:value['execution']['stdout_sha256']='f'*64
                    path.write_text(json.dumps(value))
                    with self.subTest(repository=operation['repository'],command=operation['command'],mutation=mutation),self.assertRaises(ValueError):
                        validator.validate_terminal_operation(operation,where)
                path.write_text(json.dumps(original))
                changed=copy.deepcopy(operation);changed['revision_after']='a'*40
                value=copy.deepcopy(original);value['revision_after']=changed['revision_after'];value['result']['revision_after']=changed['revision_after']
                path.write_text(json.dumps(value))
                with self.assertRaisesRegex(ValueError,'unchanged revision'):validator.validate_terminal_operation(changed,where)
                path.write_text(json.dumps(original));validator.validate_terminal_operation(operation,where)

    def test_fresh_source_environment_names_and_secrets_require_complete_gets(self):
        self.complete_transfer_gates();credential=self.matrix['pre_transfer_credential_verification']
        ref=self.matrix['pre_cutover_source_evidence_url'];path=DIRECTORY/ref;original=json.loads(path.read_text())
        def check():validator.validate_source_refresh(ref,DIRECTORY,self.inventory,credential)
        check()
        for mutation in ('missing-enumeration','new-environment','missing-secrets','new-secret','failed','wrong-endpoint','stale','omitted-page'):
            value=copy.deepcopy(original);row=next(r for a in value['accounts'] for r in a['repositories'] if r['full_name']=='HemSoft/dashboard')
            if mutation=='missing-enumeration':row.pop('environment_pages')
            elif mutation=='new-environment':
                row['environment_pages'][0]['data']['environments'].append({'name':'new-production'})
                row['environment_pages'][0]['data']['total_count']+=1
            elif mutation=='missing-secrets':row['environment_secret_pages'].pop('Production')
            else:
                page=row['environment_secret_pages']['Production'][0]
                if mutation=='new-secret':page['data']={'total_count':1,'secrets':[{'name':'SFL_APP_PRIVATE_KEY'}]}
                elif mutation=='failed':page['http_status']=403
                elif mutation=='wrong-endpoint':page['request_url']=page['request_url'].replace('/Production/','/Preview/')
                elif mutation=='stale':page['observed_at']='2026-10-07T00:00:00Z'
                else:page['response_headers']['Link']='<'+page['request_url']+'&page=2>; rel="next"'
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):check()
        path.write_text(json.dumps(original));check()

    def test_each_transfer_rechecks_immediate_source_protection_before_acceptance(self):
        self.complete_transfer_gates();row=next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/dashboard')
        repo=next(r for r in self.inventory['repositories'] if r['id']==row['repository_id'])
        source=json.loads((DIRECTORY/self.matrix['pre_cutover_source_evidence_url']).read_text())
        head=next(r['source_head'] for a in source['accounts'] for r in a['repositories'] if r['id']==row['repository_id'])
        revision=validator.source_revision(head,repo);cutoff=validator.observed_time('2026-10-07T01:50:10Z','fixture')
        ref=self.transfer_capture(row);path=DIRECTORY/ref;original=json.loads(path.read_text())
        policy_path=DIRECTORY/original['source_protection_evidence_url'];baseline=json.loads(policy_path.read_text())
        def check():validator.validate_repository_transfer(ref,DIRECTORY,repo,cutoff,revision)
        check()
        for mutation in ('missing','old-summary','old-primary','future','failed','late-new-rule'):
            capture=copy.deepcopy(original);policy=copy.deepcopy(baseline)
            if mutation=='missing':capture.pop('source_protection_evidence_url')
            elif mutation=='old-summary':self.retime_policy(policy,'2026-10-07T01:49:00Z')
            elif mutation=='old-primary':policy['ruleset_pages'][0]['observed_at']='2026-10-07T01:49:00Z'
            elif mutation=='future':self.retime_policy(policy,'2026-10-07T01:50:31Z')
            elif mutation=='failed':policy['ruleset_pages'][0]['http_status']=403
            else:
                rule={'id':999,'name':'late strengthened source protection','target':'branch','enforcement':'active',
                    'conditions':{},'rules':[{'type':'deletion'}],'bypass_actors':[]}
                policy['rulesets'].append(rule);policy['ruleset_pages'][0]['data'].append({'id':999})
                policy['ruleset_responses']['999']={'method':'GET','http_status':200,'observed_at':policy['observed_at'],
                    'request_url':'https://api.github.com/repos/'+row['source']+'/rulesets/999','data':rule}
            path.write_text(json.dumps(capture));policy_path.write_text(json.dumps(policy))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):check()
        path.write_text(json.dumps(original));policy_path.write_text(json.dumps(baseline));check()

    def test_edited_scope_approval_uses_primary_comment_update_time(self):
        row=self.matrix['repositories'][0];row['health']='scope_exception';self.complete_transfer_gates()
        row.update(transfer_evidence_url=self.transfer_capture(row),status_evidence_url='https://example.com/settings')
        self.complete_scope_decision(row);self.complete_post_transfer_access(row);self.check()
        path=DIRECTORY/row['exception_evidence_url'];value=json.loads(path.read_text())
        primary_path=DIRECTORY/value['owner_comment_evidence_url'];primary=json.loads(primary_path.read_text())
        value['owner_comment'].update(created_at='2026-10-06T19:00:00-04:00',updated_at=row['scope_exception_decision']['approved_at'])
        primary['comment'].update(created_at=value['owner_comment']['created_at'],updated_at=value['owner_comment']['updated_at'])
        path.write_text(json.dumps(value));primary_path.write_text(json.dumps(primary));self.check()
        for mutation in ('wrong-summary-time','wrong-primary-time','wrong-owner'):
            capture=copy.deepcopy(value);comment=copy.deepcopy(primary)
            if mutation=='wrong-summary-time':capture['owner_comment']['updated_at']='2026-10-06T19:00:00-04:00'
            elif mutation=='wrong-primary-time':comment['comment']['updated_at']='2026-10-06T19:00:00-04:00'
            else:comment['comment']['user']['login']='other'
            path.write_text(json.dumps(capture));primary_path.write_text(json.dumps(comment))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(value));primary_path.write_text(json.dumps(primary));self.check()

    def test_registered_review_ancestry_needs_exact_successful_primary_compare_get(self):
        row=self.complete_rollout()
        cases=[(row,DIRECTORY,row['repository_id'],row['destination'],'b'*40)]+[
            (p['validation_evidence'],DIRECTORY,p['repository_id'],p['repository'],'b'*40)
            for p in self.matrix['disposable_validation_repositories']]
        with tempfile.TemporaryDirectory() as folder:
            directory=pathlib.Path(folder);onboarding=self.onboarding_fixture(directory)
            cases.append((onboarding,directory,onboarding['repository_id'],onboarding['repository'],'f'*40))
            for proof,where,rid,name,deployed in cases:
                path=where/proof['review_deployment_evidence_url'];original=json.loads(path.read_text())
                def check():validator.validate_registered_review(proof,where,name,rid,'main',deployed)
                check()
                for mutation in ('missing','failed','wrong-endpoint','different-data','future'):
                    value=copy.deepcopy(original)
                    if mutation=='missing':value.pop('compare_response')
                    elif mutation=='failed':value['compare_response']['http_status']=403
                    elif mutation=='wrong-endpoint':value['compare_response']['request_url']=value['compare_response']['request_url'].replace('/compare/','/invented/')
                    elif mutation=='different-data':value['compare_response']['data']['merge_base_commit']['sha']='a'*40
                    else:value['compare_response']['observed_at']='2026-10-07T05:00:00Z'
                    path.write_text(json.dumps(value))
                    with self.subTest(repository=name,mutation=mutation),self.assertRaises(ValueError):check()
                path.write_text(json.dumps(original));check()


    def test_owned_credential_run_derives_from_exact_primary_actions_get(self):
        self.complete_transfer_gates();proof=self.matrix['pre_transfer_credential_verification']
        path=DIRECTORY/proof['workflow_run_evidence_url'];original=json.loads(path.read_text())
        validator.validate_app_credential(proof,DIRECTORY)
        for mutation in ('missing','failed','wrong-route','different-run','different-workflow','stale','future','wrong-event'):
            value=copy.deepcopy(original)
            if mutation=='missing':value.pop('run_response')
            elif mutation=='failed':value['run_response']['http_status']=403
            elif mutation=='wrong-route':value['run_response']['request_url']=value['run_response']['request_url'].replace('/1','/2')
            elif mutation=='different-run':value['run_response']['data']['id']=2
            elif mutation=='different-workflow':value['run_response']['data']['path']='.github/workflows/unrelated.yml'
            elif mutation=='wrong-event':value['event']='push';value['run_response']['data']['event']='push'
            else:value['run_response']['observed_at']='2026-10-07T01:48:00Z' if mutation=='stale' else '2026-10-07T02:00:00Z'
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):validator.validate_app_credential(proof,DIRECTORY)
        path.write_text(json.dumps(original));validator.validate_app_credential(proof,DIRECTORY)

    def test_terminal_operations_use_execution_or_merge_completion(self):
        self.complete_pilots();receipts=self.matrix['disposable_validation_repositories'][0]['validation_evidence']
        cutoff=validator.observed_time('2026-10-07T01:55:00Z','fixture')
        for key in ('init_pr_url','sync_pr_url','status_evidence_url','gate_uninstall_evidence_url'):
            operation=receipts['operation_receipts'][key];path=DIRECTORY/operation['capture_evidence_url']
            original=json.loads(path.read_text());late=copy.deepcopy(original);late['observed_at']='2026-10-07T04:00:00Z'
            path.write_text(json.dumps(late))
            completed=validator.validate_terminal_operation(operation,DIRECTORY,cutoff)
            expected=original['result']['pull_request']['merged_at'] if operation['outcome']=='pull_request_merged' else original['execution']['completed_at']
            self.assertEqual(completed,validator.observed_time(expected,'actual completion'))
            for mutation in ('early','missing','failed','future'):
                value=copy.deepcopy(late)
                if operation['outcome']=='pull_request_merged':
                    if mutation=='early':value['result']['pull_request'].update(created_at='2026-10-07T01:00:00Z',merged_at='2026-10-07T01:00:00Z')
                    elif mutation=='missing':value['result']['pull_request'].pop('merged_at')
                    elif mutation=='failed':value['result']['pull_request']['merged']=False
                    else:value['result']['pull_request']['merged_at']='2026-10-07T05:00:00Z'
                    value['pull_request_response']['data']=copy.deepcopy(value['result']['pull_request'])
                elif mutation=='missing':value.pop('execution')
                else:
                    at='2026-10-07T01:00:00Z' if mutation=='early' else '2026-10-07T05:00:00Z' if mutation=='future' else value['started_at']
                    value['started_at']=at;value['execution'].update(started_at=at,completed_at=at)
                    if mutation=='failed':value['execution']['exit_code']=1
                path.write_text(json.dumps(value))
                with self.subTest(operation=key,mutation=mutation),self.assertRaises(ValueError):
                    validator.validate_terminal_operation(operation,DIRECTORY,cutoff)
            path.write_text(json.dumps(original))
        gate=receipts['operation_receipts']['gate_uninstall_evidence_url'];path=DIRECTORY/gate['capture_evidence_url']
        capture=json.loads(path.read_text());capture['started_at']='2026-10-07T02:00:00Z'
        capture['execution'].update(started_at=capture['started_at'],completed_at=capture['started_at'])
        path.write_text(json.dumps(capture))
        with self.assertRaisesRegex(ValueError,'latest completed validation'):self.check()

    def test_terminal_merge_needs_its_exact_primary_pull_request_get(self):
        self.complete_pilots();receipts=self.matrix['disposable_validation_repositories'][0]['validation_evidence']
        for key in ('init_pr_url','sync_pr_url'):
            operation=receipts['operation_receipts'][key];path=DIRECTORY/operation['capture_evidence_url'];original=json.loads(path.read_text())
            validator.validate_terminal_operation(operation,DIRECTORY)
            for mutation in ('missing','failed','wrong-route','different-pr','stale','future'):
                value=copy.deepcopy(original)
                if mutation=='missing':value.pop('pull_request_response')
                elif mutation=='failed':value['pull_request_response']['http_status']=403
                elif mutation=='wrong-route':value['pull_request_response']['request_url']+='0'
                elif mutation=='different-pr':value['pull_request_response']['data']['number']=99
                else:value['pull_request_response']['observed_at']='2026-10-07T01:00:00Z' if mutation=='stale' else '2026-10-07T03:00:00Z'
                path.write_text(json.dumps(value))
                with self.subTest(operation=key,mutation=mutation),self.assertRaises(ValueError):validator.validate_terminal_operation(operation,DIRECTORY)
            path.write_text(json.dumps(original))

    def test_pilot_cleanup_preserves_unrelated_effective_and_classic_policy(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0];receipts=pilot['validation_evidence']
        before_path=DIRECTORY/receipts['pre_cleanup_gate_policy_evidence_url'];final_path=DIRECTORY/receipts['final_gate_policy_evidence_url']
        before=json.loads(before_path.read_text());final=json.loads(final_path.read_text())
        before['effective_rules']['data'][0]['parameters']['required_status_checks'].append({'context':'lint','integration_id':15368})
        before['effective_rules']['data'].append({'type':'deletion'})
        final['effective_rules']['data']=copy.deepcopy(before['effective_rules']['data'])
        final['effective_rules']['data'][0]['parameters']['required_status_checks']=[{'context':'lint','integration_id':15368}]
        before['classic_protection'].update(state='observed',http_status=200,data={
            'required_status_checks':{'strict':True,'contexts':['SFL Reviewer Gate Runner','unit'],
                'checks':[{'context':'SFL Reviewer Gate Runner','app_id':15368},{'context':'unit','app_id':15368}]},
            'required_pull_request_reviews':{'required_approving_review_count':2},'enforce_admins':{'enabled':True}})
        final['classic_protection'].update(state='observed',http_status=200,data=copy.deepcopy(before['classic_protection']['data']))
        final['classic_protection']['data']['required_status_checks'].update(contexts=['unit'],checks=[{'context':'unit','app_id':15368}])
        before_path.write_text(json.dumps(before));final_path.write_text(json.dumps(final));self.check()
        for mutation in ('all-effective','other-check','other-rule','classic-removed','reviews-weakened','strict-weakened'):
            value=copy.deepcopy(final)
            if mutation=='all-effective':value['effective_rules']['data']=[]
            elif mutation=='other-check':value['effective_rules']['data']=value['effective_rules']['data'][1:]
            elif mutation=='other-rule':value['effective_rules']['data']=value['effective_rules']['data'][:1]
            elif mutation=='classic-removed':value['classic_protection'].update(state='absent',http_status=404,data={'message':'Branch not protected'})
            elif mutation=='reviews-weakened':value['classic_protection']['data']['required_pull_request_reviews']['required_approving_review_count']=1
            else:value['classic_protection']['data']['required_status_checks']['strict']=False
            final_path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaisesRegex(ValueError,'preserve all unrelated'):self.check()
        final_path.write_text(json.dumps(final));self.check()

    def test_pre_cleanup_policy_is_current_and_bound_before_execution(self):
        self.complete_pilots();pilot=self.matrix['disposable_validation_repositories'][0];receipts=pilot['validation_evidence']
        path=DIRECTORY/receipts['pre_cleanup_gate_policy_evidence_url'];original=json.loads(path.read_text());self.check()
        for mutation in ('wrong-repository','wrong-branch','after-removal','before-validation','failed-get'):
            value=copy.deepcopy(original)
            if mutation=='wrong-repository':value['repository']='hemsoft-dev/unrelated'
            elif mutation=='wrong-branch':value['branch']='other'
            elif mutation=='failed-get':value['effective_rules']['http_status']=403
            else:self.retime_policy(value,'2026-10-07T03:41:00Z' if mutation=='after-removal' else '2026-10-07T01:00:00Z')
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):self.check()
        path.write_text(json.dumps(original));self.check()

    def test_runner_smoke_binds_primary_run_and_reviewed_read_only_workflow(self):
        self.complete_transfer_gates();row=next(r for r in self.matrix['repositories'] if r['source']=='HemSoft/yahtzee')
        proof=row['post_transfer_runner'];path=DIRECTORY/proof['run_evidence_url'];original=json.loads(path.read_text())
        cutoff=validator.observed_time('2026-10-07T01:51:00Z','fixture')
        validator.validate_runner_captures(proof,DIRECTORY,cutoff)
        for mutation in ('missing','failed','wrong-route','different-run','stale','future','wrong-workflow','wrong-event'):
            value=copy.deepcopy(original)
            if mutation=='missing':value.pop('run_response')
            elif mutation=='failed':value['run_response']['http_status']=403
            elif mutation=='wrong-route':value['run_response']['request_url']=value['run_response']['request_url'].replace('/1','/2')
            elif mutation=='different-run':value['run_response']['data']['id']=2
            elif mutation in {'wrong-workflow','wrong-event'}:
                value['run']['path' if mutation=='wrong-workflow' else 'event']='.github/workflows/destructive.yml' if mutation=='wrong-workflow' else 'push'
                value['run_response']['data']=copy.deepcopy(value['run'])
            else:value['run_response']['observed_at']='2026-10-07T01:00:00Z' if mutation=='stale' else '2026-10-07T03:00:00Z'
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):validator.validate_runner_captures(proof,DIRECTORY,cutoff)
        path.write_text(json.dumps(original))
        workflow_path=DIRECTORY/proof['workflow_evidence_url'];workflow=json.loads(workflow_path.read_text())
        workflow['contents_response']=self.file_response(proof['repository'],proof['run_head_sha'],
            '.github/workflows/self-hosted-smoke.yml',b'name: destructive workflow\npermissions: write-all\n')
        workflow_path.write_text(json.dumps(workflow))
        with self.assertRaisesRegex(ValueError,'reviewed read-only workflow bytes'):validator.validate_runner_captures(proof,DIRECTORY,cutoff)


    def test_source_free_plan_refusal_requires_fresh_owner_browser_proof(self):
        repo=next(r for r in self.inventory['repositories'] if r['id']==validator.FHEMMER_REPOSITORY_ID)
        at='2026-10-08T02:00:00Z';earliest=validator.observed_time('2026-10-08T01:55:00Z','fixture')
        browser={'phase':'pre_cutover'}
        for page,suffix,phrase in [('classic','branches','Classic branch protections have not been configured'),
                                  ('rulesets','rules',"You haven't created any rulesets")]:
            url='https://github.com/'+repo['full_name']+'/settings/'+suffix
            value={'url':url,'login':'HemSoft','observed_at':at,
                   'visible_text':'Settings: '+repo['full_name']+'\n'+phrase}
            if page=='classic':value.update(repository=repo['full_name'],repository_id=str(repo['id']))
            browser[page]={'toolIcon':{'pageUrl':url},'value':value}
        browser_path=DIRECTORY/self.capture(browser)
        reference=self.capture({'phase':'pre_cutover','repository_id':repo['id'],'repository':repo['full_name'],
            'revision_sha':'e'*40,'observed_at':at,'rulesets':[],'classic':{},'owner_browser_evidence_url':browser_path.name})
        policy=json.loads((DIRECTORY/reference).read_text())
        policy['ruleset_pages'][0].update(http_status=403,data={'message':
            'Upgrade to GitHub Pro or make this repository public to enable this feature.'})
        def check(value=policy):
            return validator.validate_protection_responses(value,repo,repo['full_name'],'e'*40,
                validator.observed_time(at,'fixture'),earliest,directory=DIRECTORY)
        self.assertEqual(check(),{'rulesets':[],'classic':{}})
        self.assertEqual(policy['ruleset_pages'][0]['http_status'],403)
        immediate=copy.deepcopy(policy);immediate['phase']='pre_transfer'
        browser_path.write_text(json.dumps(dict(browser,phase='pre_transfer')))
        self.assertEqual(check(immediate),{'rulesets':[],'classic':{}})
        with self.assertRaisesRegex(ValueError,'current cutover phase'):check()
        browser_path.write_text(json.dumps(browser))
        for mutation in ('wrong-owner','wrong-id','wrong-url','stale','missing-absence','wrong-phase'):
            value=copy.deepcopy(browser)
            if mutation=='wrong-owner':value['rulesets']['value']['login']='another-owner'
            elif mutation=='wrong-id':value['classic']['value']['repository_id']='42'
            elif mutation=='wrong-url':value['rulesets']['value']['url']=value['classic']['value']['url']
            elif mutation=='stale':value['classic']['value']['observed_at']='2026-10-08T01:00:00Z'
            elif mutation=='missing-absence':value['rulesets']['value']['visible_text']='Settings: '+repo['full_name']
            else:value['phase']='preparation'
            browser_path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):check()
        browser_path.write_text(json.dumps(browser))
        for mutation in ('quota-refusal','missing-browser','new-protected-branch','post-transfer'):
            value=copy.deepcopy(policy)
            if mutation=='quota-refusal':value['ruleset_pages'][0]['data']['message']='API rate limit exceeded'
            elif mutation=='missing-browser':value.pop('owner_browser_evidence_url')
            elif mutation=='new-protected-branch':value['protected_branch_pages'][0]['data']=[{'name':'main'}]
            else:value['phase']='post_transfer'
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):check(value)

    def test_tag_only_manifest_disagreement_preserves_current_branch_consistency(self):
        repo=next(r for r in self.inventory['repositories'] if r['full_name']=='HemSoft/hs-buddy')
        base='https://api.github.com/repos/'+repo['full_name'];at='2026-10-08T02:00:00Z'
        def branch(head,bodies):
            files=[];tree=[]
            for path,body in bodies.items():
                file={'repository_id':repo['id'],'repository':repo['full_name'],'revision_sha':head,
                    'path':path,'observed_at':at,'contents_response':self.file_response(repo['full_name'],head,path,body),
                    'manifest':json.loads(body)}
                files.append(file);tree.append({'path':path,'type':'blob','sha':file['contents_response']['data']['sha']})
            return {'head_sha':head,'tree_sha':'f'*40,'files':files,
                'commit_response':{'http_status':200,'request_url':base+'/git/commits/'+head,
                    'data':{'sha':head,'tree':{'sha':'f'*40}}},
                'tree_response':{'http_status':200,'request_url':base+'/git/trees/'+'f'*40+'?recursive=1',
                    'data':{'sha':'f'*40,'truncated':False,'tree':tree}}}
        row=dict(branch('e'*40,{}),repository_id=repo['id'],source=repo['full_name'],observed_at=at,
            referenced_secret_names=[],state='observed',
            ref_response={'http_status':200,'request_url':base+'/git/ref/heads/main',
                'data':{'ref':'refs/heads/main','object':{'type':'commit','sha':'e'*40}}},
            branches_response={'http_status':200,'request_url':base+'/branches?per_page=100','all_pages':True,
                'data':[{'name':'main','commit':{'sha':'e'*40}}]},
            tag_refs_response={'method':'GET','http_status':200,'request_url':base+'/git/matching-refs/tags/',
                'observed_at':at,'response_headers':{},
                'data':[{'ref':'refs/tags/historical','object':{'type':'commit','sha':'d'*40}}]},tag_object_responses={},
            branch_scans=[branch('d'*40,{'sfl.json':b'{"version":"legacy"}',
                                      '.sfl/sfl.json':b'{"version":"canonical"}'})])
        path=DIRECTORY/self.capture({'phase':'pre_cutover','observed_at':at,'repositories':[row]})
        value=json.loads(path.read_text())
        revisions,_,manifests=validator.validate_reference_scan(path.name,DIRECTORY,{'repositories':[repo]})
        self.assertEqual(revisions[repo['id']][0],'e'*40);self.assertEqual(manifests,{})
        target=value['repositories'][0]
        target['branches_response']['data'].append({'name':'restored-history','commit':{'sha':'d'*40}})
        target['branches_response']['pages'][0]['data']=copy.deepcopy(target['branches_response']['data'])
        path.write_text(json.dumps(value))
        with self.assertRaisesRegex(ValueError,'canonical and legacy installation manifests disagree'):
            validator.validate_reference_scan(path.name,DIRECTORY,{'repositories':[repo]})

    def historical_credential_fixture(self):
        repo=next(r for r in self.inventory['repositories'] if r['id']==1262580000)
        catalog=json.loads((DIRECTORY/'historical-workflow-reconciliation.json').read_text())
        entry=next(e for e in catalog['blobs'] if e['path']=='sfl.json')
        body=(ROOT/'deployment/tests/fixtures/retired-gh-x-sfl.json').read_bytes()
        at='2026-10-08T02:00:00Z';base='https://api.github.com/repos/'+repo['full_name']
        file={'repository_id':repo['id'],'repository':repo['full_name'],'revision_sha':entry['source_revision'],
            'path':'sfl.json','observed_at':at,'contents_response':self.file_response(repo['full_name'],entry['source_revision'],'sfl.json',body)}
        history={'head_sha':entry['source_revision'],'files':[file]}
        def response(suffix,data,status=200):return {'method':'GET','http_status':status,'observed_at':at,
            'request_url':base+suffix,'response_headers':{},'data':data}
        retired={path:response('/actions/workflows/'+path.rsplit('/',1)[1],
            {'id':wid,'path':path,'state':'deleted'} if wid else {'message':'Not Found'},200 if wid else 404)
            for path,wid in {'.github/workflows/sfl-pr-review-auto.yml':337272622,
                '.github/workflows/sfl-pr-review.lock.yml':325052102,'.github/workflows/sfl-pr-review-recovery.yml':None}.items()}
        capture={'phase':'pre_cutover','repository_id':repo['id'],'repository':repo['full_name'],'head_sha':'e'*40,
            'observed_at':at,'metadata_response':response('',{'id':repo['id'],'full_name':repo['full_name']}),
            'default_ref_response':response('/git/ref/heads/main',{'ref':'refs/heads/main','object':{'sha':'e'*40}}),
            'workflow_pages':[response('/actions/workflows?per_page=100',{'total_count':1,
                'workflows':[{'id':1,'path':'.github/workflows/ci.yml','state':'active'}]})],
            'historical_workflow_responses':retired}
        path=DIRECTORY/self.capture(capture)
        row={'head_sha':'e'*40,'files':[],'branch_scans':[history],'inactive_historical_workflows_evidence_url':path.name}
        revision=('e'*40,'f'*40,{'main':'e'*40},{'refs/tags/history':{'commit_sha':entry['source_revision']}})
        return repo,row,revision,history,path,capture

    def test_inactive_history_cannot_use_evidence_after_source_refresh(self):
        repo,row,revision,history,path,original=self.historical_credential_fixture()
        scanned=validator.observed_time('2026-10-08T01:55:00Z','fixture')
        refresh=validator.observed_time('2026-10-08T02:01:00Z','fixture')
        def check():
            validator.validate_inactive_historical_references(row,repo,revision,[history],
                ['OPENROUTER_API_KEY','SFL_APP_PRIVATE_KEY'],DIRECTORY,scanned,latest_at=refresh)
        check()
        future=copy.deepcopy(original);future['observed_at']='2026-10-08T02:02:00Z'
        path.write_text(json.dumps(future))
        with self.assertRaisesRegex(ValueError,'precede source refresh'):check()
        future=copy.deepcopy(original);future['metadata_response']['observed_at']='2026-10-08T02:02:00Z'
        path.write_text(json.dumps(future))
        with self.assertRaisesRegex(ValueError,'identity GETs'):check()

    def test_retired_history_requires_exact_bytes_and_current_primary_metadata(self):
        repo,row,revision,history,path,original=self.historical_credential_fixture()
        waived=['OPENROUTER_API_KEY','SFL_APP_PRIVATE_KEY'];scanned=validator.observed_time('2026-10-08T01:55:00Z','fixture')
        def check(r=row,v=revision,h=history):
            validator.validate_inactive_historical_references(r,repo,v,[h],waived,DIRECTORY,scanned)
        check()
        for mutation in ('wrong-head','stale','missing-get','failed-get','enabled','wrong-workflow-id','registered','omitted-page'):
            value=copy.deepcopy(original)
            if mutation=='wrong-head':value['head_sha']='b'*40
            elif mutation=='stale':value['metadata_response']['observed_at']='2026-10-08T01:00:00Z'
            elif mutation=='missing-get':value['historical_workflow_responses'].pop('.github/workflows/sfl-pr-review-recovery.yml')
            elif mutation=='failed-get':value['historical_workflow_responses']['.github/workflows/sfl-pr-review-auto.yml']['http_status']=403
            elif mutation=='enabled':value['historical_workflow_responses']['.github/workflows/sfl-pr-review-auto.yml']['data']['state']='active'
            elif mutation=='wrong-workflow-id':value['historical_workflow_responses']['.github/workflows/sfl-pr-review-auto.yml']['data']['id']=42
            elif mutation=='registered':value['workflow_pages'][0]['data']['workflows'][0]['path']='.github/workflows/sfl-pr-review-auto.yml'
            else:value['workflow_pages'][0]['response_headers']['Link']='<'+value['workflow_pages'][0]['request_url']+'&page=2>; rel="next"'
            path.write_text(json.dumps(value))
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):check()
        path.write_text(json.dumps(original))
        current=copy.deepcopy(revision);current[2]['restored']=history['head_sha']
        with self.assertRaisesRegex(ValueError,'current branch'):check(v=current)
        restored=copy.deepcopy(row);restored['files']=[{'path':'.github/workflows/sfl-pr-review-auto.yml'}]
        with self.assertRaisesRegex(ValueError,'restored historical'):check(r=restored)
        changed=copy.deepcopy(history);changed['files'][0]['contents_response']=self.file_response(repo['full_name'],history['head_sha'],
            'sfl.json',b'{"key":"${{ secrets.SFL_APP_PRIVATE_KEY }}"}')
        with self.assertRaisesRegex(ValueError,'reviewed exact bytes'):check(h=changed)
        catalog_path=DIRECTORY/'historical-workflow-reconciliation.json';catalog_bytes=catalog_path.read_bytes()
        self.addCleanup(catalog_path.write_bytes,catalog_bytes);catalog=json.loads(catalog_bytes)
        catalog['tag_only_revisions'].append('b'*40);catalog_path.write_text(json.dumps(catalog))
        with self.assertRaisesRegex(ValueError,'reviewed immutable catalog'):check()

    def existing_wider_fixture(self):
        row={'repository_id':1229335234,'source':'HemSoft/hs-buddy','destination':'hemsoft-dev/hs-buddy',
             'health':'verified','installed_tier':'full','selected_tier':'full','selected_addons':[],
             'deployment_sha':'a'*40,'manifest_version':'2.1.0-rc.14'}
        row['wider_workflow_run_urls']=['https://github.com/hemsoft-dev/hs-buddy/actions/runs/1',
                                         'https://github.com/hemsoft-dev/hs-buddy/actions/runs/2']
        row['wider_operation_receipts']=[]
        for url,workflow in zip(row['wider_workflow_run_urls'],
            ['.github/workflows/sfl-auditor.yml','.github/workflows/sfl-dispatcher.yml']):
            operation={'repository_id':row['repository_id'],'repository':row['destination'],
                       'deployment_sha':row['deployment_sha'],'release_version':row['manifest_version'],
                       'evidence_url':url,'conclusion':'success','run_head_sha':'b'*40,'workflow':workflow}
            self.bind_workflow_capture(operation)
            row['wider_operation_receipts'].append(operation)
        return row

    def test_existing_wider_pilot_preserves_baseline_full_consumer(self):
        row=self.existing_wider_fixture()
        validator.validate_existing_wider_pilot(row,DIRECTORY,'b'*40,
            validator.observed_time('2026-10-07T01:59:00Z','Cutover'))
        for field,value in [('repository_id',1),('destination','hemsoft-dev/another'),
                            ('installed_tier','not_installed'),('selected_tier','reviewer'),('health','pending_transfer')]:
            changed=copy.deepcopy(row);changed[field]=value
            with self.assertRaisesRegex(ValueError,'preserve the designated'):
                validator.validate_existing_wider_pilot(changed,DIRECTORY,'b'*40,None)

    def test_existing_wider_pilot_rejects_missing_distinct_auditor_and_forged_run_identity(self):
        row=self.existing_wider_fixture()
        changed=copy.deepcopy(row);changed['wider_workflow_run_urls']=changed['wider_workflow_run_urls'][:1]
        changed['wider_operation_receipts']=changed['wider_operation_receipts'][:1]
        with self.assertRaisesRegex(ValueError,'distinct successful'):
            validator.validate_existing_wider_pilot(changed,DIRECTORY,'b'*40,None)
        changed=copy.deepcopy(row);changed['wider_operation_receipts'][1]['repository_id']=1
        with self.assertRaisesRegex(ValueError,'bind'):
            validator.validate_existing_wider_pilot(changed,DIRECTORY,'b'*40,None)
        changed=copy.deepcopy(row);changed['wider_operation_receipts'][1]['run_head_sha']='c'*40
        with self.assertRaisesRegex(ValueError,'immutable run head'):
            validator.validate_existing_wider_pilot(changed,DIRECTORY,'b'*40,None)
        changed=copy.deepcopy(row);operation=changed['wider_operation_receipts'][1]
        operation['workflow']='.github/workflows/sfl-auditor.yml';self.bind_workflow_capture(operation)
        with self.assertRaisesRegex(ValueError,'one Auditor'):
            validator.validate_existing_wider_pilot(changed,DIRECTORY,'b'*40,None)

    def test_existing_wider_selection_rejects_arbitrary_repository(self):
        self.matrix['existing_wider_validation_repository_id']=1
        with self.assertRaisesRegex(ValueError,'designated baseline'):
            self.check()

    def test_existing_wider_pilot_must_finish_before_other_consumers(self):
        early=validator.observed_time('2026-10-07T02:00:00Z','Start')
        complete=validator.observed_time('2026-10-07T02:05:00Z','Complete')
        late=validator.observed_time('2026-10-07T02:06:00Z','Later consumer')
        validator.validate_wider_pilot_ordering(False,1229335234,complete,[(1229335234,early),(123,late)])
        for started in [early,complete]:
            with self.assertRaisesRegex(ValueError,'before other active'):
                validator.validate_wider_pilot_ordering(False,1229335234,complete,[(1229335234,early),(123,started)])
        with self.assertRaisesRegex(ValueError,'verified wider-workflow'):
            validator.validate_wider_pilot_ordering(False,1229335234,None,[(1229335234,early)])
        validator.validate_wider_pilot_ordering(True,None,None,[(123,early)])


if __name__ == '__main__':
    unittest.main()
