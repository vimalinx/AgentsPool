#!/usr/bin/env bash
set -Eeuo pipefail

ARCHIVE=""
IDENTITY_FILE="${AGENTSPOOL_BACKUP_IDENTITY_FILE:-}"
BINARY="${AGENTSPOOL_BINARY:-agentspool}"
PORT="${AGENTSPOOL_RESTORE_DRILL_PORT:-18090}"

while (($#)); do
  case "$1" in
    --archive) ARCHIVE="$2"; shift 2 ;;
    --identity-file) IDENTITY_FILE="$2"; shift 2 ;;
    --binary) BINARY="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[[ -f "$ARCHIVE" ]] || { echo "encrypted backup archive not found" >&2; exit 1; }
[[ -f "$IDENTITY_FILE" ]] || { echo "age identity file not found" >&2; exit 1; }
IDENTITY_MODE="$(stat -c '%a' "$IDENTITY_FILE")"
(( (8#$IDENTITY_MODE & 077) == 0 )) || { echo "age identity file must not be accessible by group or others" >&2; exit 1; }
[[ "$PORT" =~ ^[0-9]+$ && "$PORT" -ge 1024 && "$PORT" -le 65535 ]] || { echo "invalid drill port" >&2; exit 1; }
command -v age >/dev/null || { echo "age is required" >&2; exit 1; }
command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 1; }
command -v "$BINARY" >/dev/null 2>&1 || [[ -x "$BINARY" ]] || { echo "AgentsPool binary not found" >&2; exit 1; }

WORK_DIR="$(mktemp -d /tmp/agentspool-restore-drill.XXXXXX)"
SERVER_PID=""
cleanup() {
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf -- "$WORK_DIR"
}
trap cleanup EXIT

age --decrypt --identity "$IDENTITY_FILE" --output "$WORK_DIR/backup.tar.gz" "$ARCHIVE"
python3 - "$WORK_DIR/backup.tar.gz" <<'PY'
import pathlib
import sys
import tarfile

archive = pathlib.Path(sys.argv[1])
with tarfile.open(archive, "r:gz") as source:
    members = source.getmembers()
    if not members:
        raise SystemExit("backup archive is empty")
    for member in members:
        path = pathlib.PurePosixPath(member.name)
        if path.is_absolute() or not path.parts or path.parts[0] != "server" or ".." in path.parts:
            raise SystemExit(f"archive contains an unexpected path: {member.name}")
        if not (member.isdir() or member.isfile()):
            raise SystemExit(f"archive contains an unsafe entry type: {member.name}")
PY
tar --no-same-owner -C "$WORK_DIR" -xzf "$WORK_DIR/backup.tar.gz"
[[ -f "$WORK_DIR/server/state.json" ]] || { echo "restored state.json is missing" >&2; exit 1; }

"$BINARY" token >"$WORK_DIR/admin.token"
"$BINARY" token >"$WORK_DIR/allocator.token"
"$BINARY" token >"$WORK_DIR/join.token"
chmod 0600 "$WORK_DIR"/*.token

"$BINARY" server \
  --listen "127.0.0.1:$PORT" \
  --data-dir "$WORK_DIR/server" \
  --admin-token-file "$WORK_DIR/admin.token" \
  --allocator-token-file "$WORK_DIR/allocator.token" \
  --join-token-file "$WORK_DIR/join.token" \
  >"$WORK_DIR/server.log" 2>&1 &
SERVER_PID=$!

READY=false
for _ in $(seq 1 100); do
  if curl --silent --fail "http://127.0.0.1:$PORT/readyz" >/dev/null; then
    READY=true
    break
  fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    break
  fi
  sleep 0.1
done
if [[ "$READY" != true ]]; then
  echo "restored control plane did not become ready" >&2
  sed -n '1,80p' "$WORK_DIR/server.log" >&2
  exit 1
fi

python3 - "$WORK_DIR/server" <<'PY'
import hashlib,json,pathlib,sys
root=pathlib.Path(sys.argv[1])
state=json.loads((root/'state.json').read_text())
artifacts=state.get('artifacts') or {}
checked=0
for artifact in artifacts.values():
    path=(root/'artifacts'/artifact['storage_path']).resolve()
    path.relative_to((root/'artifacts').resolve())
    if not path.is_file():
        raise SystemExit(f"artifact content missing: {artifact['id']}")
    hasher=hashlib.sha256()
    with path.open('rb') as source:
        for chunk in iter(lambda: source.read(1024*1024), b''):
            hasher.update(chunk)
    digest=hasher.hexdigest()
    if digest != artifact['sha256']:
        raise SystemExit(f"artifact digest mismatch: {artifact['id']}")
    checked += 1
print(json.dumps({"restore_drill":"passed","schema_version":state.get('schema_version'),"nodes":len(state.get('nodes') or {}),"sandboxes":len(state.get('sandboxes') or {}),"artifacts_verified":checked},separators=(',',':')))
PY
