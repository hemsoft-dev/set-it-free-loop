"""Reject stale retained execution outputs after evidence-validator changes."""
import ast
import hashlib
import json
import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parents[2]
INPUTS = ('deployment/scripts/validate-executed-transfers.py',
          'deployment/tests/test_executed_transfers.py',
          'deployment/scripts/check-executed-validation-record.py')
record = json.loads((ROOT / 'docs/organization-migration/execution/executed-transfer-validation.json').read_text())
expected = {name: hashlib.sha256((ROOT / name).read_bytes()).hexdigest() for name in INPUTS}
if record.get('inputs_sha256') != expected:
    raise ValueError('Retained validation output predates the current validator, corruption suite or record checker')
module = ast.parse((ROOT / INPUTS[1]).read_text())
count = sum(isinstance(node, ast.FunctionDef) and node.name.startswith('test_') for node in ast.walk(module))
tests = record['tests']
if record['exit_code'] != 0 or tests['exit_code'] != 0 or tests['command'] != [
        'python3', '-B', '-m', 'unittest', 'discover', '-s', 'deployment/tests', '-p', 'test_executed_transfers.py', '-v']:
    raise ValueError('Retained corruption suite execution is not bound to its successful current command')
if not re.search(r'(?m)^Ran ' + str(count) + r' tests in [0-9.]+s$', tests['stderr']) or not tests['stderr'].rstrip().endswith('OK'):
    raise ValueError('Retained corruption suite output does not match its current test count and result')
if record['command'] != ['python3', 'deployment/scripts/validate-executed-transfers.py', 'docs/organization-migration']:
    raise ValueError('Retained validator execution is not bound to the actual command')
print('Retained executed-transfer validation record matches the current validator and ' + str(count) + ' corruption tests')
