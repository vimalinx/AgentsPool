#!/usr/bin/env python3
"""Read only non-secret server transport settings; never source server.env."""
from pathlib import Path
import ssl
import sys
import urllib.request

settings = {}
for line in Path('/etc/agentspool/server.env').read_text().splitlines():
    key, sep, value = line.partition('=')
    if sep:
        settings[key] = value.strip().strip('"\'')
listen = settings.get('AGENTSPOOL_LISTEN', '127.0.0.1:8090')
if listen.startswith(':'):
    listen = '127.0.0.1' + listen
listen = listen.replace('0.0.0.0:', '127.0.0.1:').replace('[::]:', '[::1]:')
tls = bool(settings.get('AGENTSPOOL_TLS_CERT'))
# Local startup probe only. External clients must verify configured TLS normally.
context = ssl._create_unverified_context() if tls else None
try:
    with urllib.request.urlopen(('https://' if tls else 'http://') + listen + '/readyz', timeout=1, context=context) as response:
        sys.exit(0 if response.status == 200 else 1)
except Exception:
    sys.exit(1)
