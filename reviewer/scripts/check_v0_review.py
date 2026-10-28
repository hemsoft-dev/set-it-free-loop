#!/usr/bin/env python3
"""Offline acceptance check for an exported pmd SFL review API response.

Usage: python3 reviewer/scripts/check_v0_review.py review.json expected-head
No credentials, network calls or repository code execution are needed.
"""
import json
import re
import sys


BOT_ID = 345646943


def check(review, head):
    errors = []
    if not re.fullmatch(r'[0-9a-f]{40}', head):
        return ['expected head must be a full lowercase SHA']
    if review.get('commit_id') != head:
        errors.append('review targets a different head')
    user = review.get('user') or {}
    if user.get('id') != BOT_ID or user.get('login') != 'hemsoft-sfl-reviewer[bot]':
        errors.append('review is not from the SFL bot')
    body = review.get('body') or ''
    if not isinstance(body, str):
        return errors + ['review body must be text']
    if re.search(r'&(?:amp;)?(?:\\)?#(?:x27|39);', body, re.I):
        errors.append('apostrophe entity leaked into Markdown')
    headings = ['### Key Findings', '### Coverage']
    if any(body.count(heading) != 1 for heading in headings):
        errors.append('expected one findings and coverage section')
    coverage = body.split('### Coverage', 1)[-1].split('\n---', 1)[0]
    labels = ['Files reviewed', 'Context read', 'Verified', 'Not verified', 'Tests run']
    actual = re.findall(r'^- \*\*([^*]+):\*\* ', coverage, re.M)
    if actual != labels:
        errors.append('coverage must contain the five ordered bullets')
    if body.count('- **Tests run:** None. Reviewer execution is disabled.') != 1:
        errors.append('expected one parent-owned tests disclaimer')
    if 'No tests were executed' in body:
        errors.append('legacy tests disclaimer remains')
    if not re.search(r'<!-- sfl request=\d+ sha=' + head + r' -->', body):
        errors.append('request marker does not name the expected head')
    return errors


def main():
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    try:
        with open(sys.argv[1], encoding='utf-8') as source:
            review = json.load(source)
        if not isinstance(review, dict):
            raise ValueError('review must be an object')
        errors = check(review, sys.argv[2])
    except (OSError, ValueError) as exc:
        print(str(exc), file=sys.stderr)
        return 2
    for error in errors:
        print(error, file=sys.stderr)
    if not errors:
        print('SFL v0 identity, head and review format passed')
    return int(bool(errors))


if __name__ == '__main__':
    sys.exit(main())
