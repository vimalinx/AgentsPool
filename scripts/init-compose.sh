#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 - "$ROOT/.env" <<'PY'
import os,secrets,sys
path=sys.argv[1]
try:
 fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
except FileExistsError:
 raise SystemExit('.env already exists; kept unchanged')
with os.fdopen(fd,'w') as out:
 for name in ('ADMIN','ALLOCATOR','JOIN'):
  out.write(f'AGENTSPOOL_{name}_TOKEN={secrets.token_urlsafe(48)}\n')
 out.write('AGENTSPOOL_PUBLISH_ADDRESS=127.0.0.1\n')
print('Created protected .env with three distinct random credentials. Tokens were not printed.')
PY
