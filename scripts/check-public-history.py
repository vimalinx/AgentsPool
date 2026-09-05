#!/usr/bin/env python3
"""Check publishable Git history for deployment identifiers; report paths, not values."""
import re
import subprocess
import sys

patterns = [
    re.compile(rb'/home/(?!agent\b)[A-Za-z0-9._-]+'),
    re.compile(rb'\b192\.168\.\d{1,3}\.\d{1,3}\b'),
    re.compile(rb'\b(?:' + b'ide' + b'apad|ras' + b'pi|vi' + b'malinx' + rb')[A-Za-z0-9._-]*\b', re.I),
    re.compile(rb'\b(?:node|task|exec|art)_[0-9a-f]{16,}\b'),
]
root = subprocess.check_output(['git', 'rev-parse', '--show-toplevel'], text=True).strip()
listing = subprocess.check_output(['git', '-C', root, 'rev-list', '--objects', '--all'], text=True)
checked, failures = set(), []
for line in listing.splitlines():
    sha, _, path = line.partition(' ')
    if sha in checked or not (path.startswith(('docs/', 'deploy/', 'scripts/')) or path in ('README.md', 'README.en.md', 'START_HERE.zh-CN.md')):
        continue
    if path in ('scripts/check-public-history.py', 'scripts/validate-repo.py'):
        continue
    kind = subprocess.check_output(['git', '-C', root, 'cat-file', '-t', sha], text=True).strip()
    if kind != 'blob':
        continue
    checked.add(sha)
    data = subprocess.check_output(['git', '-C', root, 'cat-file', 'blob', sha])
    if b'\x00' not in data and any(pattern.search(data) for pattern in patterns):
        failures.append((sha[:12], path))
for sha, path in failures:
    print(f'Private deployment identifier in history: {sha} {path}', file=sys.stderr)
if failures:
    raise SystemExit('Refusing public history; export a reviewed clean snapshot or rewrite a backed-up clone.')
print(f'Public history privacy check passed: {len(checked)} unique documentation/deployment blobs')
