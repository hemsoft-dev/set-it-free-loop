#!/usr/bin/env python3
"""Offline acceptance check for an exported pmd SFL review API response.

Usage: python3 reviewer/scripts/check_v0_review.py review.json expected-head
No credentials, network calls or repository code execution are needed.
Fenced source examples cannot supply review structure or the parent footer.
"""
import json
import re
import sys


BOT_ID = 345646943


def outside_fenced_code(body):
    """Keep only lines outside backtick/tilde code fences for validation."""
    visible = []
    fence = None
    for line in body.split('\n'):
        if fence is not None:
            closing = r' {0,3}' + re.escape(fence[0]) + '{' + str(len(fence)) + r',}[ \t]*'
            if re.fullmatch(closing, line):
                fence = None
            visible.append('')
            continue
        opening = re.fullmatch(r' {0,3}(`{3,}|~{3,})(.*)', line)
        # Backtick info strings cannot contain backticks. Closing fences must
        # use the opening character, at least its length, and no trailing text.
        if opening and (opening[1][0] != '`' or '`' not in opening[2]):
            fence = opening[1]
            visible.append('')
        else:
            visible.append(line)
    return '\n'.join(visible)


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
    body = outside_fenced_code(body)
    # Code spans deliberately preserve source literals. Escaped ampersands in
    # prose can also be intentional examples, so they are not evidence of a leak.
    prose_body = re.sub(r'(?<!`)(`+)(?!`)[^\n]*?(?<!`)\1(?!`)', '', body)
    if re.search(r'&(?:\\)?#(?:x27|39);', prose_body, re.I):
        errors.append('apostrophe entity leaked into Markdown')
    # Match structural ATX headings, including trailing whitespace and optional
    # closing hashes. Use the same matches for counts, ordering and boundaries.
    headings = [list(re.finditer(r'^ {0,3}###[ \t]+' + re.escape(title)
                                 + r'(?:[ \t]+#+)?[ \t]*$', body, re.M))
                for title in ('Key Findings', 'Coverage')]
    if any(len(matches) != 1 for matches in headings):
        errors.append('expected one findings and coverage section')
    findings = headings[0][0] if headings[0] else None
    section = headings[1][0] if headings[1] else None
    # Only a complete formatter separator line ends coverage. A plain-text
    # prefix such as ---not-a-rule must remain subject to coverage checks.
    coverage = re.split(r'^---[ \t]*$', body[section.end():], maxsplit=1, flags=re.M)[0] if section else ''
    if section and findings and findings.start() > section.start():
        errors.append('findings must precede coverage')
    labels = ['Files reviewed', 'Context read', 'Verified', 'Not verified', 'Tests run']
    actual = re.findall(r'^- \*\*([^*]+):\*\*(?: |$)', coverage, re.M)
    if actual != labels:
        errors.append('coverage must contain the five ordered bullets')
    boundary = r'\n(?=- \*\*(?:' + '|'.join(map(re.escape, labels)) + r'):\*\*)'
    first = re.search(r'^- \*\*Files reviewed:\*\*', coverage, re.M)
    if first and coverage[:first.start()].strip():
        errors.append('unexpected content before first coverage group')
    for label in labels[:-1]:
        match = re.search(r'^- \*\*' + re.escape(label) + r':\*\*(.*)$', coverage, re.M)
        if not match:
            continue
        inline = match[1]
        following = re.split(boundary, coverage[match.end():], maxsplit=1)[0]
        items = [line for line in following.splitlines() if line.strip()]
        if inline == ' None reported.' and label in ('Context read', 'Not verified'):
            if following.strip():
                errors.append(label + ' has content after None reported')
        elif inline or not items or any(not re.fullmatch(r'  - [ \t]*\S.*', line) for line in items):
            errors.append(label + ' must use nested coverage bullets')
    last = re.search(r'^- \*\*Tests run:\*\*.*$', coverage, re.M)
    if last and coverage[last.end():].strip():
        errors.append('unexpected content after tests disclaimer')
    disclaimer = '- **Tests run:** None. Reviewer execution is disabled.'
    if (not last or last[0] != disclaimer
            or len(re.findall(r'^' + re.escape(disclaimer) + r'$', body, re.M)) != 1):
        errors.append('expected one parent-owned tests disclaimer')
    if re.search(r'^(?:[-*>] )?(?:\*{1,2})?No tests were executed[.!]?(?:\*{1,2})?$', prose_body, re.M):
        errors.append('legacy tests disclaimer remains')
    markers = re.findall(r'^<!-- sfl request=\d+ sha=([0-9a-f]{40}) -->$', body, re.M)
    if markers != [head]:
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
