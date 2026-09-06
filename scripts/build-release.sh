#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
VERSION="$(awk -F'"' '/const Version/{print $2}' internal/config/config.go)"
OUT="$ROOT/dist/release"
if git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  if [[ -n "$(git -C "$ROOT" status --porcelain --untracked-files=normal)" && "${AGENTSPOOL_ALLOW_DIRTY_RELEASE:-0}" != "1" ]]; then
    echo "refusing to publish a release from a dirty worktree; commit the release or set AGENTSPOOL_ALLOW_DIRTY_RELEASE=1 for a local test build" >&2
    exit 1
  fi
  SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" log -1 --format=%ct)}"
else
  # ZIP timestamps cannot predate 1980. A source release has no Git metadata,
  # so use the ZIP epoch unless the caller provides a preserved build epoch.
  SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-315532800}"
fi
export SOURCE_DATE_EPOCH
rm -rf "$OUT"; mkdir -p "$OUT"
TARGETS=(
  "linux-amd64|linux|amd64|"
  "linux-arm64|linux|arm64|"
  "linux-armv7|linux|arm|GOARM=7"
  "linux-mipsle-softfloat|linux|mipsle|GOMIPS=softfloat"
)
COMMANDS=(
  "agentspool|./cmd/agentspool"
)
build_target() {
  local target="$1" name goos goarch extra package stage command binary package_path
  IFS='|' read -r name goos goarch extra <<<"$target"
  package="AgentsPool-v${VERSION}-${name}"
  stage="$OUT/$package"
  mkdir -p "$stage/examples/tasks" "$stage/examples/python" "$stage/deploy" "$stage/scripts"
  echo "building $name"
  for command in "${COMMANDS[@]}"; do
    IFS='|' read -r binary package_path <<<"$command"
    if [[ -n "$extra" ]]; then
      env CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" $extra go build -buildvcs=false -trimpath -ldflags='-s -w' -o "$stage/$binary" "$package_path"
    else
      env CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -buildvcs=false -trimpath -ldflags='-s -w' -o "$stage/$binary" "$package_path"
    fi
  done
  cp README.md README.en.md START_HERE.zh-CN.md LICENSE CHANGELOG.md CONTRIBUTING.md SECURITY.md compose.yaml .env.example "$stage/"
  cp deploy/Dockerfile.binary "$stage/Dockerfile"
  cp -R .agents "$stage/"
  cp examples/tasks/*.json "$stage/examples/tasks/"
  cp examples/python/*.py "$stage/examples/python/"
  cp -R docs api sdk "$stage/"
  cp -R deploy/systemd deploy/openwrt "$stage/deploy/"
  cp scripts/install-client.sh scripts/install-server.sh scripts/install-worker.sh scripts/uninstall.sh \
    scripts/install-common.sh scripts/install-ready.py scripts/init-compose.sh scripts/sandbox-volume.py scripts/first-sandbox.sh \
    scripts/inspect-lan-persistent.sh scripts/e2e-lan-persistent.sh scripts/e2e-lan-restart.sh \
    scripts/e2e-lan-sandboxes.sh scripts/backup-server.sh scripts/restore-drill.sh \
    scripts/check-tls-expiry.sh "$stage/scripts/"
  find "$stage" -type d -name __pycache__ -prune -exec rm -rf {} +
  find "$stage" -type f -name '*.py[co]' -delete
  (
    cd "$stage"
    sha256sum agentspool > SHA256SUMS.txt
  )
  tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner \
    -C "$OUT" -cf - "$package" | gzip -n -9 >"$OUT/$package.tar.gz"
  rm -rf "$stage"
}

# Build one architecture per background job. Every target contains the same
# single runtime-neutral agentspool binary.
pids=()
for target in "${TARGETS[@]}"; do
  build_target "$target" &
  pids+=("$!")
done
for pid in "${pids[@]}"; do
  wait "$pid"
done
python3 - "$ROOT" "$OUT" "$VERSION" "$SOURCE_DATE_EPOCH" <<'PY'
import datetime,stat,sys,zipfile
from pathlib import Path
root=Path(sys.argv[1]);out=Path(sys.argv[2]);version=sys.argv[3];epoch=int(sys.argv[4])
dest=out/f'AgentsPool-v{version}-source.zip'
root_files={
    '.dockerignore','.env.example','.gitleaksignore','.gitignore','CHANGELOG.md','CONTRIBUTING.md',
    'Dockerfile','LICENSE','Makefile','README.en.md','README.md','SECURITY.md',
    'START_HERE.zh-CN.md','compose.yaml','go.mod',
}
root_dirs={'.agents','.github','api','cmd','deploy','docs','examples','internal','scripts','sdk'}
forbidden={'.ai','.git','dist','build','packages','__pycache__','.pytest_cache','agentspool_sdk.egg-info'}
inventory=[]
for value in sorted(root_files):
    path=root/value
    if path.is_file(): inventory.append(path)
for value in sorted(root_dirs):
    base=root/value
    if not base.is_dir(): continue
    inventory.extend(sorted(path for path in base.rglob('*') if path.is_file()))
timestamp=datetime.datetime.fromtimestamp(max(epoch,315532800),datetime.timezone.utc).timetuple()[:6]
def info_for(path,arcname):
    info=zipfile.ZipInfo(str(arcname),timestamp)
    info.create_system=3
    info.compress_type=zipfile.ZIP_DEFLATED
    mode=path.stat().st_mode
    info.external_attr=(stat.S_IFREG | (0o755 if mode & stat.S_IXUSR else 0o644)) << 16
    return info
with zipfile.ZipFile(dest,'w',zipfile.ZIP_DEFLATED,compresslevel=9) as z:
    for path in sorted(inventory):
        rel=path.relative_to(root)
        if set(rel.parts) & forbidden or path.suffix in {'.pyc','.pyo'}: continue
        if path.is_symlink(): raise SystemExit(f'refusing symlink in source archive: {rel}')
        resolved=path.resolve()
        resolved.relative_to(root.resolve())
        arcname=Path(f'AgentsPool-v{version}')/rel
        z.writestr(info_for(path,arcname),path.read_bytes())
PY
python3 - "$ROOT" "$OUT" "$VERSION" "$SOURCE_DATE_EPOCH" <<'PY'
import datetime,json,subprocess,sys,urllib.parse,uuid
from pathlib import Path
root=Path(sys.argv[1]);out=Path(sys.argv[2]);version=sys.argv[3];epoch=int(sys.argv[4])
process=subprocess.run(
    ['go','list','-m','-json','all'],cwd=root,check=True,capture_output=True,text=True,
)
decoder=json.JSONDecoder();offset=0;modules=[]
while offset < len(process.stdout):
    while offset < len(process.stdout) and process.stdout[offset].isspace():offset+=1
    if offset >= len(process.stdout):break
    value,offset=decoder.raw_decode(process.stdout,offset);modules.append(value)
if not modules or not modules[0].get('Main'):
    raise SystemExit('go list did not return the main module first')
created=datetime.datetime.fromtimestamp(epoch,datetime.timezone.utc).replace(microsecond=0).isoformat().replace('+00:00','Z')
namespace_uuid=uuid.uuid5(uuid.NAMESPACE_URL,f'github.com/vimalinx/AgentsPool/{version}/{epoch}')
packages=[]
relationships=[]
main_id='SPDXRef-Package-AgentsPool'
for index,module in enumerate(modules):
    path=module['Path'];module_version=module.get('Version') or version
    spdx_id=main_id if module.get('Main') else f'SPDXRef-Package-GoModule-{index}'
    package={
        'name':'AgentsPool' if module.get('Main') else path,
        'SPDXID':spdx_id,
        'versionInfo':module_version,
        'downloadLocation':(
            f'https://github.com/vimalinx/AgentsPool/releases/download/v{version}/AgentsPool-v{version}-source.zip'
            if module.get('Main') else 'NOASSERTION'
        ),
        'filesAnalyzed':False,
        'licenseConcluded':'MIT' if module.get('Main') else 'NOASSERTION',
        'licenseDeclared':'MIT' if module.get('Main') else 'NOASSERTION',
        'copyrightText':'NOASSERTION',
        'externalRefs':[{
            'referenceCategory':'PACKAGE-MANAGER',
            'referenceType':'purl',
            'referenceLocator':f"pkg:golang/{urllib.parse.quote(path,safe='/')}@{urllib.parse.quote(module_version,safe='.-_+')}"
        }],
    }
    packages.append(package)
    if not module.get('Main'):
        relationships.append({'spdxElementId':main_id,'relationshipType':'DEPENDS_ON','relatedSpdxElement':spdx_id})
relationships.insert(0,{'spdxElementId':'SPDXRef-DOCUMENT','relationshipType':'DESCRIBES','relatedSpdxElement':main_id})
document={
    'spdxVersion':'SPDX-2.3',
    'dataLicense':'CC0-1.0',
    'SPDXID':'SPDXRef-DOCUMENT',
    'name':f'AgentsPool-v{version}',
    'documentNamespace':f'https://github.com/vimalinx/AgentsPool/spdx/{version}/{namespace_uuid}',
    'creationInfo':{'created':created,'creators':['Tool: AgentsPool release builder']},
    'packages':packages,
    'relationships':relationships,
}
(out/'SBOM.spdx.json').write_text(json.dumps(document,indent=2,sort_keys=True)+"\n")
PY
(
  cd "$OUT"
  sha256sum AgentsPool-v"$VERSION"-linux-*.tar.gz AgentsPool-v"$VERSION"-source.zip SBOM.spdx.json > SHA256SUMS.txt
)
python3 - "$OUT" "$VERSION" "$SOURCE_DATE_EPOCH" <<'PY'
import datetime,hashlib,json,stat,subprocess,sys,zipfile
from pathlib import Path
out=Path(sys.argv[1]);version=sys.argv[2];epoch=int(sys.argv[3])
files=sorted([*out.glob(f'AgentsPool-v{version}-linux-*.tar.gz'),out/f'AgentsPool-v{version}-source.zip',out/'SBOM.spdx.json'])
def digest(path):
    h=hashlib.sha256()
    with path.open('rb') as f:
        for chunk in iter(lambda:f.read(1024*1024),b''):h.update(chunk)
    return h.hexdigest()
manifest={
    'version':version,
    'source_date_epoch':epoch,
    'go_version':subprocess.run(['go','version'],check=True,capture_output=True,text=True).stdout.strip(),
    'files':[{'name':p.name,'bytes':p.stat().st_size,'sha256':digest(p)} for p in files],
}
(out/'RELEASE_MANIFEST.json').write_text(json.dumps(manifest,indent=2)+"\n")
complete=out/f'AgentsPool-v{version}-complete-release.zip'
timestamp=datetime.datetime.fromtimestamp(max(epoch,315532800),datetime.timezone.utc).timetuple()[:6]
def add(z,path,arcname):
    info=zipfile.ZipInfo(str(arcname),timestamp)
    info.create_system=3;info.compress_type=zipfile.ZIP_DEFLATED
    info.external_attr=(stat.S_IFREG|0o644)<<16
    z.writestr(info,path.read_bytes())
with zipfile.ZipFile(complete,'w',zipfile.ZIP_DEFLATED,compresslevel=9) as z:
    for p in files+[out/'SHA256SUMS.txt',out/'RELEASE_MANIFEST.json']:
        add(z,p,p.name)
    repo=out.parents[1]
    for rel in ['START_HERE.zh-CN.md','README.md','SECURITY.md','docs/README.md','docs/AGENT-TOOLS.zh-CN.md','docs/ARCHITECTURE.zh-CN.md','docs/QUICKSTART.zh-CN.md','docs/OPERATIONS.zh-CN.md','docs/DATA-LIFECYCLE.zh-CN.md','docs/MIGRATION-v0.4.zh-CN.md','docs/SECURITY.md','docs/IMPLEMENTATION_STATUS.zh-CN.md']:
        add(z,repo/rel,rel)
(out/f'AgentsPool-v{version}-complete-release.sha256').write_text(f'{digest(complete)}  {complete.name}\n')
PY
# Reverse-open every archive and ensure all expected binaries are present.
# Store the listing first: with `set -o pipefail`, `tar | grep -q` can return
# SIGPIPE (141) after grep finds its match even though the archive is valid.
for archive in "$OUT"/AgentsPool-v"$VERSION"-linux-*.tar.gz; do
  listing="$(mktemp)"
  trap 'rm -f "$listing"' EXIT
  tar -tzf "$archive" >"$listing"
  for binary in agentspool; do
    grep -q "/$binary$" "$listing"
  done
  rm -f "$listing"
  trap - EXIT
done
unzip -tq "$OUT/AgentsPool-v$VERSION-source.zip" >/dev/null
unzip -tq "$OUT/AgentsPool-v$VERSION-complete-release.zip" >/dev/null
python3 - "$OUT/AgentsPool-v$VERSION-source.zip" <<'PY'
import sys,zipfile
archive=sys.argv[1]
forbidden={'.ai','.git','dist','packages','__pycache__'}
with zipfile.ZipFile(archive) as source:
    bad=[]
    for name in source.namelist():
        parts=set(name.split('/'))
        if parts & forbidden or name.endswith(('.pyc','.pyo')):
            bad.append(name)
if bad:
    raise SystemExit('private or generated files entered source archive: '+', '.join(bad[:10]))
PY
( cd "$OUT" && sha256sum -c SHA256SUMS.txt >/dev/null )
echo "release artifacts written to $OUT"
