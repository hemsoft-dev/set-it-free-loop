#!/usr/bin/env python3
"""Read-only GitHub migration preflight. Requires Python 3 and authenticated gh."""

import argparse
import concurrent.futures
import datetime
import json
import pathlib
import re
import subprocess
import urllib.parse

SOURCE_OWNERS = ('HemSoft', 'fhemmer')
COLLISION_NAMES = {'fhemmer/hs-cli-confluence-search': 'hs-cli-confluence-search-fhemmer'}


def api(endpoint, collection=None):
    """Keep failed/hidden endpoints distinct from successful empty collections."""
    args = ['gh', 'api', '--method', 'GET', endpoint]
    if collection is not None:
        args += ['--paginate', '--slurp']
    result = subprocess.run(args, capture_output=True, text=True, timeout=90)
    if result.returncode:
        match = re.search(r'HTTP (\d{3})', result.stderr)
        return {'state': 'unverified', 'http_status': int(match[1]) if match else None}
    value = json.loads(result.stdout)
    if collection is not None:
        value = [item for page in value for item in (page if collection == '' else page[collection])]
    return {'state': 'observed', 'data': value}


def select(value, keys):
    return {key: value.get(key) for key in keys}


def project(result, transform):
    if result['state'] == 'observed':
        result['data'] = transform(result['data'])
    return result


def source_repositories():
    personal = api('user/repos?affiliation=owner&per_page=100', '')
    organization = api('orgs/fhemmer/repos?type=all&per_page=100', '')
    if any(result['state'] != 'observed' for result in (personal, organization)):
        raise RuntimeError('Cannot enumerate both source owners completely')
    repos = personal['data'] + organization['data']
    if any(repo['owner']['login'] not in SOURCE_OWNERS for repo in repos):
        raise RuntimeError('Unexpected owner in source enumeration')
    return sorted(repos, key=lambda repo: repo['full_name'].lower())


def capture_repo(repo, destination):
    name = repo['full_name']
    prefix = 'repos/' + name
    record = select(repo, ('id', 'node_id', 'full_name', 'visibility', 'private', 'archived',
                           'fork', 'default_branch', 'html_url', 'has_pages', 'has_wiki',
                           'has_issues', 'has_discussions', 'disabled'))
    record['destination'] = destination + '/' + COLLISION_NAMES.get(name, repo['name'])
    record['mapping_reason'] = ('Preserve both colliding repositories; retain the public name.'
                                if name in COLLISION_NAMES else 'Retain repository name.')
    record['captured_at'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    settings = {}
    settings['collaborators'] = project(api(prefix + '/collaborators?affiliation=all&per_page=100', ''),
        lambda items: [select(item, ('login', 'id', 'role_name', 'permissions')) for item in items])
    settings['teams'] = project(api(prefix + '/teams?per_page=100', ''),
        lambda items: [select(item, ('id', 'slug', 'permission')) for item in items])
    settings['rulesets'] = api(prefix + '/rulesets?includes_parents=true&per_page=100', '')
    if settings['rulesets']['state'] == 'observed':
        for item in settings['rulesets']['data']:
            item['details'] = api(prefix + '/rulesets/' + str(item['id']))
    settings['protected_branches'] = api(prefix + '/branches?protected=true&per_page=100', '')
    if settings['protected_branches']['state'] == 'observed':
        settings['protected_branches']['data'] = [
            {'name': branch['name'], 'protection': api(prefix + '/branches/' +
             urllib.parse.quote(branch['name'], safe='') + '/protection')}
            for branch in settings['protected_branches']['data']]
    settings['workflows'] = project(api(prefix + '/actions/workflows?per_page=100', 'workflows'),
        lambda items: [select(item, ('id', 'name', 'path', 'state', 'html_url')) for item in items])
    settings['actions_policy'] = api(prefix + '/actions/permissions')
    settings['workflow_permissions'] = api(prefix + '/actions/permissions/workflow')
    settings['secret_names'] = project(api(prefix + '/actions/secrets?per_page=100', 'secrets'),
        lambda items: sorted(item['name'] for item in items))
    # Never retain Actions variable values, even when the REST endpoint returns them.
    settings['variable_names'] = project(api(prefix + '/actions/variables?per_page=100', 'variables'),
        lambda items: sorted(item['name'] for item in items))
    settings['environments'] = project(api(prefix + '/environments?per_page=100', 'environments'),
        lambda items: [select(item, ('id', 'name', 'protection_rules', 'deployment_branch_policy'))
                       for item in items])
    settings['deploy_keys'] = project(api(prefix + '/keys?per_page=100', ''),
        lambda items: [select(item, ('id', 'title', 'read_only', 'verified')) for item in items])
    # Hook paths and query strings may contain credentials; retain only routing hosts.
    settings['webhooks'] = project(api(prefix + '/hooks?per_page=100', ''),
        lambda items: [{'id': item['id'], 'active': item['active'], 'events': item['events'],
                        'delivery_host': urllib.parse.urlsplit(item.get('config', {}).get('url', '')).hostname}
                       for item in items])
    settings['pages'] = project(api(prefix + '/pages'),
        lambda value: select(value, ('status', 'cname', 'build_type', 'source', 'public', 'https_enforced')))
    # This endpoint may require a GitHub App JWT. A 404 is not proof of no installation.
    settings['app_installation'] = project(api(prefix + '/installation'),
        lambda value: select(value, ('id', 'app_id', 'target_type', 'repository_selection', 'permissions')))
    record['settings'] = settings
    return record


def validate(snapshot):
    repositories = snapshot['repositories']
    ids = [repo['id'] for repo in repositories]
    targets = [repo['destination'].lower() for repo in repositories]
    if len(ids) != len(set(ids)) or len(targets) != len(set(targets)):
        raise ValueError('Duplicate repository ID or destination')
    if not repositories or set(snapshot['source_owners']) != set(SOURCE_OWNERS):
        raise ValueError('Missing migration population')
    if {repo['full_name'].split('/')[0] for repo in repositories} != set(SOURCE_OWNERS):
        raise ValueError('Repository population does not cover both source owners')
    for repo in repositories:
        if repo['full_name'].split('/')[0] not in SOURCE_OWNERS:
            raise ValueError('Out-of-scope source')
        if repo['destination'].split('/')[0] != snapshot['destination_login']:
            raise ValueError('Destination owner mismatch')
    return {'repositories': len(repositories),
            'private': sum(repo['private'] for repo in repositories),
            'archived': sum(repo['archived'] for repo in repositories),
            'unverified_endpoints': sum(result['state'] != 'observed'
                for repo in repositories for result in repo['settings'].values())}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--destination', default='hemsoft-dev')
    parser.add_argument('--output', type=pathlib.Path,
                        default=pathlib.Path('docs/organization-migration/inventory.json'))
    parser.add_argument('--check', type=pathlib.Path, help='Validate an existing snapshot without network calls')
    args = parser.parse_args()
    if args.check:
        print(json.dumps(validate(json.loads(args.check.read_text())), indent=2))
        return
    if not re.fullmatch(r'[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?', args.destination):
        parser.error('Invalid destination login')
    identity = api('user')
    if identity['state'] != 'observed' or identity['data'].get('login') != 'HemSoft':
        raise RuntimeError('Authenticate as HemSoft before discovery')
    repos = source_repositories()
    organization = project(api('orgs/' + args.destination),
        lambda value: select(value, ('id', 'login', 'type', 'plan', 'default_repository_permission',
                                    'members_can_create_repositories', 'two_factor_requirement_enabled')))
    snapshot = {
        'schema_version': 1,
        'captured_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
        'requester': 'HemSoft', 'source_owners': list(SOURCE_OWNERS),
        'source_personal_plan': identity['data'].get('plan', {}).get('name'),
        'destination_login': args.destination,
        'destination_organization': organization,
        'destination_owner_membership': project(api('orgs/' + args.destination + '/memberships/HemSoft'),
                                               lambda value: select(value, ('state', 'role'))),
        'destination_teams': project(api('orgs/' + args.destination + '/teams?per_page=100', ''),
                                    lambda items: [select(item, ('id', 'slug', 'permission')) for item in items]),
        'destination_actions_policy': api('orgs/' + args.destination + '/actions/permissions'),
        'source_organization': project(api('orgs/fhemmer'),
            lambda value: select(value, ('id', 'login', 'plan', 'default_repository_permission'))),
        'source_organization_membership': project(api('orgs/fhemmer/memberships/HemSoft'),
                                                 lambda value: select(value, ('state', 'role'))),
        'source_organization_apps': project(api('orgs/fhemmer/installations?per_page=100', 'installations'),
            lambda items: [select(item, ('id', 'app_id', 'app_slug', 'repository_selection', 'permissions'))
                           for item in items]),
        'known_owned_app': project(api('apps/sfl-app'),
            lambda value: {'id': value['id'], 'slug': value['slug'], 'owner_login': value['owner']['login'],
                           'owner_type': value['owner']['type'], 'permissions': value['permissions']}),
        'coverage_limits': [
            'Unverified endpoints, including 404 responses, do not prove absence.',
            'App installation inventory requires App JWT/organization access or owner UI verification.',
            'External deployment accounts, packages and runner registrations need separate owner verification.',
            'Environment credential names and values are not collected.',
            'Webhook delivery hosts are recorded; URLs, variable values, secret values and key material are excluded.',
        ],
        'repositories': [],
    }
    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
        pending = {pool.submit(capture_repo, repo, args.destination): repo for repo in repos}
        for future in concurrent.futures.as_completed(pending):
            snapshot['repositories'].append(future.result())
            print(f"Captured {len(snapshot['repositories'])}/{len(repos)} repositories", flush=True)
    snapshot['repositories'].sort(key=lambda repo: repo['full_name'].lower())
    # A changing owner population makes the snapshot unsuitable for cutover.
    fresh = source_repositories()
    def fingerprint(items):
        return {(repo['id'], repo['full_name'], repo['private'], repo['archived'], repo['default_branch'])
                for repo in items}
    if fingerprint(fresh) != fingerprint(repos):
        raise RuntimeError('Source population changed during capture; rerun discovery')
    snapshot['summary'] = validate(snapshot)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(snapshot, indent=2) + '\n')
    print(json.dumps(snapshot['summary']), flush=True)


if __name__ == '__main__':
    main()
