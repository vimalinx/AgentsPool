#!/usr/bin/env python3
"""Repository consistency checks using only the Python standard library."""
from __future__ import annotations
import json,re,sys
from pathlib import Path
from urllib.parse import unquote
ROOT=Path(__file__).resolve().parents[1];ERRORS=[]
def error(message):ERRORS.append(message)
def read(path):return (ROOT/path).read_text(encoding='utf-8')
def normalize_version(value):
    value=value.strip()
    match=re.fullmatch(r'(\d+)\.(\d+)\.(\d+)a(\d+)',value)
    return f'{match.group(1)}.{match.group(2)}.{match.group(3)}-alpha.{match.group(4)}' if match else value

def match_version(label,file,pattern):
    found=re.search(pattern,read(file),re.MULTILINE)
    if not found:error(f'{label}: version pattern not found in {file}');return None
    return normalize_version(found.group(1))

def check_versions():
    values={
      'Go config':match_version('Go config','internal/config/config.go',r'const Version = "([^"]+)"'),
      'README':match_version('README','README.md',r'当前版本：\*\*([^*]+)\*\*'),
      'English README':match_version('English README','README.en.md',r'Version \*\*([^*]+)\*\*'),
      'Compose':match_version('Compose','compose.yaml',r'image: agentspool:([^\s]+)'),
      'Task OpenAPI':match_version('Task OpenAPI','api/openapi.yaml',r'^  version: ([^\s]+)'),
      'Python pyproject':match_version('Python pyproject','sdk/python/pyproject.toml',r'^version = "([^"]+)"'),
      'Python package':match_version('Python package','sdk/python/agentspool_sdk/__init__.py',r'__version__ = "([^"]+)"'),
      'Changelog':match_version('Changelog','CHANGELOG.md',r'^## ([^\s]+)'),
    }
    values={k:v for k,v in values.items() if v is not None}
    if values and len(set(values.values()))!=1:error('version mismatch: '+', '.join(f'{k}={v}' for k,v in values.items()))

def check_json_examples():
    for path in sorted((ROOT/'examples').rglob('*.json')):
        try:payload=json.loads(path.read_text(encoding='utf-8'))
        except Exception as exc:error(f'invalid JSON in {path.relative_to(ROOT)}: {exc}');continue
        if not isinstance(payload,dict):error(f'{path.relative_to(ROOT)} must contain a JSON object')

def check_markdown_links():
    pattern=re.compile(r'(?<!!)\[[^\]]*\]\(([^)]+)\)')
    for path in sorted(ROOT.rglob('*.md')):
        if any(part in {'dist','.git'} for part in path.relative_to(ROOT).parts):continue
        for raw in pattern.findall(path.read_text(encoding='utf-8')):
            target=raw.strip().split(maxsplit=1)[0].strip('<>')
            if not target or target.startswith(('http://','https://','mailto:','#')):continue
            target=unquote(target.split('#',1)[0]);candidate=(path.parent/target).resolve()
            try:candidate.relative_to(ROOT.resolve())
            except ValueError:error(f'{path.relative_to(ROOT)} links outside repository: {raw}');continue
            if not candidate.exists():error(f'broken link in {path.relative_to(ROOT)}: {raw}')

def parse_go_routes(file,prefix):
    text=read(file);routes=set(re.findall(r'(?:mux|s\.mux)\.Handle(?:Func)?\("(GET|POST|PUT|PATCH|DELETE) ([^"]+)"',text));routes.discard(('GET','/'));return routes

def parse_openapi(file):
    routes=set();current='';inside=False
    for line in read(file).splitlines():
        if line=='paths:':inside=True;continue
        if inside and line and not line.startswith(' '):break
        m=re.match(r'^  (/[^:]*):\s*$',line)
        if m:current=m.group(1);continue
        m=re.match(r'^    (get|post|put|patch|delete):\s*$',line)
        if current and m:routes.add((m.group(1).upper(),current))
    return routes

def compare_routes(label,go_file,openapi_file):
    actual=parse_go_routes(go_file,label);documented=parse_openapi(openapi_file)
    for route in sorted(actual-documented):error(f'{label} route missing from OpenAPI: {route[0]} {route[1]}')
    for route in sorted(documented-actual):error(f'{label} OpenAPI route has no handler: {route[0]} {route[1]}')
    return len(actual)

def check_installers():
    for rel in ('scripts/install-server.sh',):
        text=read(rel)
        if re.search(r'echo[^\n]*\$(?:ADMIN_TOKEN|JOIN_TOKEN)',text):
            error(f'{rel} must not print generated credentials')
    worker=read('scripts/install-worker.sh')
    if 'AGENTSPOOL_WORKER_DETECT_GPU=$DETECT_GPU' not in worker:
        error('scripts/install-worker.sh must write an explicit GPU detection setting')
    server=read('scripts/install-server.sh')
    for forbidden in ('AGENTSPOOL_ADMIN_TOKEN=$ADMIN_TOKEN','AGENTSPOOL_JOIN_TOKEN=$JOIN_TOKEN'):
        if forbidden in server:error('scripts/install-server.sh must use credential file locators, not EnvironmentFile token values')
    for required in ('AGENTSPOOL_ADMIN_TOKEN_FILE=/etc/agentspool/admin.token','AGENTSPOOL_ALLOCATOR_TOKEN_FILE=/etc/agentspool/allocator.token','AGENTSPOOL_JOIN_TOKEN_FILE=/etc/agentspool/join.token'):
        if required not in server:error(f'scripts/install-server.sh is missing token locator: {required}')
    for required in ('useradd --system --gid agentspool-server','root -g agentspool-server -m 0640'):
        if required not in server:error(f'scripts/install-server.sh is missing control-plane account isolation: {required}')
    server_unit=read('deploy/systemd/agentspool-server.service')
    for required in ('User=agentspool-server','Group=agentspool-server'):
        if required not in server_unit:error(f'control-plane unit is missing account isolation: {required}')

def check_tool_catalog():
    catalog=read('cmd/agentspool/tool_catalog.go')
    names=set(re.findall(r'descriptor\("([a-z]+\.[a-z]+)"',catalog))
    expected={
      'devices.list','devices.get','devices.watch','devices.annotate','devices.unannotate',
      'leases.acquire','leases.list','leases.get','leases.renew','leases.resize','leases.release',
      'sandboxes.ensure','sandboxes.list','sandboxes.get','sandboxes.exec','sandboxes.resize',
      'sandboxes.rebind','sandboxes.start','sandboxes.stop','sandboxes.destroy',
      'services.list','operations.get','operations.logs','operations.cancel','operations.wait','files.push','files.pull',
      'artifacts.list','artifacts.delete','maintenance.prune',
    }
    if names!=expected:error('tool catalog mismatch: missing='+str(sorted(expected-names))+', extra='+str(sorted(names-expected)))
    cli=read('cmd/agentspool/tools.go')
    for required in ('runToolList(args[1:])','runToolDescribe(args[1:])','AGENTSPOOL_ADMIN_TOKEN_FILE','AGENTSPOOL_ALLOCATOR_TOKEN_FILE','AGENTSPOOL_RESOURCE_LEASE_TOKEN_FILE','AGENTSPOOL_SANDBOX_TOKEN_FILE'):
        if required not in cli:error(f'generic Tool discovery or token locator missing: {required}')
    skill=read('.agents/skills/agentspool-compute/SKILL.md')
    for required in ('agentspool tool list --json','agentspool tool describe TOOL_NAME --json','AGENTSPOOL_*_TOKEN_FILE'):
        if required not in skill:error(f'Agent Skill is missing generic discovery guidance: {required}')
    profile=read('cmd/agentspool/client_profile.go')
    for required in ('AGENTSPOOL_CONFIG','allocator_token_file','admin_token_file'):
        if required not in profile:error(f'client profile is missing discovery contract: {required}')
    installer=read('scripts/install-client.sh')
    for required in ('configure --server','agentspool-compute','allocator-token-file'):
        if required not in installer:error(f'client/Skill installer is missing shared discovery setup: {required}')

def check_runtime_neutral_surface():
    forbidden_paths=(
      'api/agent-openapi.yaml',
      'cmd/agentspool-agentd',
      'cmd/agentspool-agentctl',
      'cmd/agentspool-bridge',
      'internal/agentbridge',
      'internal/agentclient',
      'internal/agentcore',
      'internal/agentserver',
      'internal/agentstore',
      'deploy/systemd/agentspool-agentd.service',
      'deploy/systemd-user/agentspool-bridge.service',
      'scripts/e2e-agent-plane.py',
      'scripts/fake-pi.py',
      'scripts/install-agentd.sh',
      'scripts/install-user-bridge.sh',
      'internal/server/web',
      'examples/python/dispatch_agent.py',
      'examples/tasks/secret-injection.json',
      'docs/TASK-PLANE-v0.1.0-IMPLEMENTATION_STATUS.zh-CN.md',
    )
    for rel in forbidden_paths:
        if (ROOT/rel).exists():error(f'runtime takeover surface must be removed: {rel}')
    for rel in ('Makefile','Dockerfile','compose.yaml','scripts/build-release.sh'):
        text=read(rel)
        for binary in ('agentspool-agentd','agentspool-agentctl','agentspool-bridge'):
            if binary in text:error(f'{rel} still builds or ships removed binary {binary}')
    installer=read('scripts/install-server.sh')
    if 'disable --now agentspool-agentd.service' not in installer:
        error('scripts/install-server.sh must disable the legacy takeover service during upgrade')

def check_open_source_hardening():
    env_example=read('.env.example')
    if 'AGENTSPOOL_AGENT_' in env_example:
        error('.env.example must not advertise the removed Agent Plane')
    for required in (
        'AGENTSPOOL_LISTEN=127.0.0.1:8090',
        'AGENTSPOOL_WORKER_ALLOWED_EXECUTORS=shell',
        'AGENTSPOOL_WORKER_ALLOW_NETWORK=false',
    ):
        if required not in env_example:error(f'.env.example is missing safe default: {required}')

    config=read('internal/config/config.go')
    for required in (
        'envOr("AGENTSPOOL_LISTEN", "127.0.0.1:8090")',
        'envOr("AGENTSPOOL_WORKER_ALLOWED_EXECUTORS", "shell")',
        'boolEnv("AGENTSPOOL_WORKER_ALLOW_NETWORK", false)',
    ):
        if required not in config:error(f'config safe default missing: {required}')

    dockerfile=read('Dockerfile')
    if 'golang:1.27-alpine3.24' not in dockerfile or 'FROM alpine:3.24' not in dockerfile:
        error('Dockerfile must use the supported Go 1.27 and Alpine 3.24 lines')
    if not dockerfile.startswith('# syntax=docker/dockerfile:1@sha256:'):
        error('Dockerfile frontend must be pinned by digest')
    for required in ('adduser -S -D -H -G agentspool-server agentspool-server','USER agentspool-server:agentspool-server'):
        if required not in dockerfile:error(f'Docker control plane is missing account isolation: {required}')

    action_ref=re.compile(r'^\s*- uses: actions/[a-z0-9_-]+@([0-9a-f]{40})(?:\s+#.*)?$',re.MULTILINE)
    for workflow in ('.github/workflows/ci.yml','.github/workflows/release.yml'):
        text=read(workflow)
        action_lines=[line for line in text.splitlines() if re.match(r'^\s*- uses: actions/',line)]
        pinned=action_ref.findall(text)
        if len(pinned)!=len(action_lines):error(f'{workflow} must pin every GitHub Action to a full commit SHA')

    release=read('scripts/build-release.sh')
    if "['git','-C',str(root),'ls-files'" in release or 'git ls-files -co' in release:
        error('source archive inventory must work without Git metadata')
    if 'SOURCE_DATE_EPOCH' not in release:
        error('release build must normalize archive timestamps')
    if "out/'SBOM.spdx.json'" not in release or 'SBOM.spdx.json > SHA256SUMS.txt' not in release:
        error('release build must publish and checksum an SPDX SBOM')

    release_workflow=read('.github/workflows/release.yml')
    for required in ('attestations: write','id-token: write','sbom-path: dist/release/SBOM.spdx.json'):
        if required not in release_workflow:error(f'release workflow is missing attestation setting: {required}')
    if release_workflow.count('uses: actions/attest@') < 2:
        error('release workflow must attest both provenance and the SPDX SBOM')
    for workflow in ('.github/workflows/ci.yml','.github/workflows/release.yml'):
        if 'golang.org/x/vuln/cmd/govulncheck@v1.7.0' not in read(workflow):
            error(f'{workflow} must run the pinned Go vulnerability scanner')

    ci=read('.github/workflows/ci.yml')
    for required in ('docker compose config --quiet','docker build -t agentspool:ci .','docker run --rm agentspool:ci version','make release-verify'):
        if required not in ci:error(f'CI is missing packaging verification: {required}')

    for required in ('SECURITY.md','CONTRIBUTING.md','docs/README.md','docs/DATA-LIFECYCLE.zh-CN.md','docs/archive/README.md','examples/README.md','.github/dependabot.yml','.gitleaksignore'):
        if not (ROOT/required).is_file():error(f'missing open-source readiness file: {required}')

def check_public_privacy():
    private_path_or_ip=re.compile(r'/home/(?!agent\b)[A-Za-z0-9._-]+|\b192\.168\.\d{1,3}\.\d{1,3}\b')
    private_labels=('ide'+'apad','ras'+'pi','vi'+'malinx')
    private_label=re.compile(r'\b(?:'+"|".join(map(re.escape,private_labels))+r')[A-Za-z0-9._-]*\b',re.IGNORECASE)
    generated_id=re.compile(r'\b(?:node|task|exec|art)_[0-9a-f]{16,}\b')
    for base in ('docs','scripts','deploy'):
        for path in sorted((ROOT/base).rglob('*')):
            if not path.is_file():continue
            if path.resolve()==Path(__file__).resolve():continue
            try:text=path.read_text(encoding='utf-8')
            except UnicodeDecodeError:continue
            if private_path_or_ip.search(text):error(f'{path.relative_to(ROOT)} contains a private home path or LAN address')
            if private_label.search(text):error(f'{path.relative_to(ROOT)} contains a private deployment label')
            if generated_id.search(text):error(f'{path.relative_to(ROOT)} contains a generated deployment object ID')

def main():
    check_versions();check_json_examples();check_markdown_links();task_routes=compare_routes('AgentsPool','internal/server/server.go','api/openapi.yaml');check_installers();check_tool_catalog();check_runtime_neutral_surface();check_open_source_hardening();check_public_privacy()
    if ERRORS:
        for message in ERRORS:print('ERROR:',message,file=sys.stderr)
        print(f'repository validation failed with {len(ERRORS)} error(s)',file=sys.stderr);return 1
    print(f'repository validation passed: versions, examples, Markdown links, {task_routes} API operations, 30 discoverable Agent tools, token locators, open-source hardening')
    return 0
if __name__=='__main__':raise SystemExit(main())
