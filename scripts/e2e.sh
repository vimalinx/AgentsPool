#!/usr/bin/env bash
set -Eeuo pipefail

BIN="${1:-./dist/agentspool}"
BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"
[[ -x "$BIN" ]] || { echo "binary not executable: $BIN" >&2; exit 1; }

TMP="$(mktemp -d)"
if [[ -n "${AGENTSPOOL_E2E_PORT:-}" ]]; then
  PORT="$AGENTSPOOL_E2E_PORT"
else
  PORT="$(python3 - <<'PY'
import socket
with socket.socket() as s:
    s.bind(('127.0.0.1', 0))
    print(s.getsockname()[1])
PY
)"
fi
URL="http://127.0.0.1:$PORT"
ADMIN_TOKEN="$($BIN token)"
JOIN_TOKEN="$($BIN token)"
SERVER_PID=""
WORKER_PID=""

cleanup() {
  local status=$?
  [[ -n "$WORKER_PID" ]] && kill -TERM "$WORKER_PID" 2>/dev/null || true
  [[ -n "$SERVER_PID" ]] && kill -TERM "$SERVER_PID" 2>/dev/null || true
  [[ -n "$WORKER_PID" ]] && wait "$WORKER_PID" 2>/dev/null || true
  [[ -n "$SERVER_PID" ]] && wait "$SERVER_PID" 2>/dev/null || true
  if [[ $status -ne 0 ]]; then
    echo "--- server.log ---" >&2
    cat "$TMP/server.log" >&2 2>/dev/null || true
    echo "--- worker.log ---" >&2
    cat "$TMP/worker.log" >&2 2>/dev/null || true
  fi
  rm -rf "$TMP"
  exit "$status"
}
trap cleanup EXIT INT TERM

AGENTSPOOL_ADMIN_TOKEN="$ADMIN_TOKEN" \
AGENTSPOOL_JOIN_TOKEN="$JOIN_TOKEN" \
  "$BIN" server --listen "127.0.0.1:$PORT" --data-dir "$TMP/server" \
  --lease-ttl 15s --node-timeout 10s >"$TMP/server.log" 2>&1 &
SERVER_PID=$!

for _ in {1..100}; do
  if curl -fsS "$URL/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
curl -fsS "$URL/healthz" >/dev/null

AGENTSPOOL_SERVER="$URL" \
AGENTSPOOL_JOIN_TOKEN="$JOIN_TOKEN" \
  "$BIN" worker --data-dir "$TMP/worker" --name e2e-worker \
  --executors shell,process --max-concurrency 2 --poll-interval 100ms \
  --heartbeat-interval 1s --graceful-timeout 5s >"$TMP/worker.log" 2>&1 &
WORKER_PID=$!

printf 'agentspool-input\n' >"$TMP/input.txt"
"$BIN" artifacts upload --server "$URL" --token "$ADMIN_TOKEN" \
  --name input.txt --media-type text/plain "$TMP/input.txt" >"$TMP/input-artifact.json"
INPUT_ARTIFACT_ID="$(python3 - "$TMP/input-artifact.json" <<'PY'
import json,sys
obj=json.load(open(sys.argv[1]))
print(obj['artifact']['id'])
PY
)"
[[ "$INPUT_ARTIFACT_ID" == art_* ]] || { echo "invalid input artifact id: $INPUT_ARTIFACT_ID" >&2; exit 1; }

python3 - "$TMP/task.json" "$INPUT_ARTIFACT_ID" <<'PY'
import json,sys
path,artifact_id=sys.argv[1:3]
spec={
  "name": "AgentsPool release E2E",
  "max_attempts": 2,
  "retry_on_failure": True,
  "side_effect_mode": "idempotent",
  "executor": {
    "kind": "shell",
    "script": "set -eu\necho hello-from-agentspool\ncat inputs/message.txt\nmkdir -p outputs/nested\ntr '[:lower:]' '[:upper:]' < inputs/message.txt > outputs/nested/result.txt"
  },
  "inputs": [
    {"artifact_id": artifact_id, "path": "inputs/message.txt"}
  ],
  "outputs": [
    {"pattern": "outputs/**/*.txt", "required": True, "media_type": "text/plain"}
  ]
}
with open(path,'w') as f:
    json.dump(spec,f)
PY

for _ in {1..100}; do
  if "$BIN" nodes list --server "$URL" --token "$ADMIN_TOKEN" 2>/dev/null | grep -q 'e2e-worker'; then break; fi
  sleep 0.1
done

TASK_ID="$("$BIN" tasks submit --server "$URL" --token "$ADMIN_TOKEN" --file "$TMP/task.json")"
[[ "$TASK_ID" == task_* ]] || { echo "invalid task id: $TASK_ID" >&2; exit 1; }
"$BIN" tasks wait "$TASK_ID" --server "$URL" --token "$ADMIN_TOKEN" --follow
"$BIN" tasks get "$TASK_ID" --server "$URL" --token "$ADMIN_TOKEN" >"$TMP/detail.json"

ARTIFACT_ID="$(python3 - "$TMP/detail.json" <<'PY'
import json,sys
obj=json.load(open(sys.argv[1]))
task=obj['task']
assert task['status']=='succeeded', task
ids=task['result']['output_artifact_ids']
assert len(ids)==1, ids
print(ids[0])
PY
)"
"$BIN" artifacts download "$ARTIFACT_ID" --server "$URL" --token "$ADMIN_TOKEN" --output "$TMP/result.txt" >/dev/null
[[ "$(cat "$TMP/result.txt")" == "AGENTSPOOL-INPUT" ]] || { echo "artifact content mismatch" >&2; exit 1; }

grep -q 'hello-from-agentspool' "$TMP/server/logs/"*.jsonl
grep -q 'agentspool-input' "$TMP/server/logs/"*.jsonl
curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" "$URL/metrics" | grep -q '^agentspool_completions_total 1$'

"$BIN" tasks delete "$TASK_ID" --server "$URL" --token "$ADMIN_TOKEN" >"$TMP/task-delete.json"
python3 - "$TMP/task-delete.json" <<'PY'
import json,sys
result=json.load(open(sys.argv[1]))
assert result['deleted'] is True, result
assert result['executions_deleted']==1, result
assert result['logs_deleted']==1, result
PY
[[ -z "$(find "$TMP/server/logs" -type f -print -quit)" ]] || { echo "task deletion left execution logs" >&2; exit 1; }

"$BIN" artifacts delete "$INPUT_ARTIFACT_ID" --server "$URL" --token "$ADMIN_TOKEN" >"$TMP/input-delete.json"
"$BIN" artifacts delete "$ARTIFACT_ID" --server "$URL" --token "$ADMIN_TOKEN" >"$TMP/output-delete.json"
"$BIN" artifacts gc --server "$URL" --token "$ADMIN_TOKEN" >"$TMP/artifact-gc.json"
python3 - "$TMP/input-delete.json" "$TMP/output-delete.json" "$TMP/artifact-gc.json" <<'PY'
import json,sys
input_delete,output_delete,gc=(json.load(open(path)) for path in sys.argv[1:])
assert input_delete['deleted'] and input_delete['content_deleted'], input_delete
assert output_delete['deleted'] and output_delete['content_deleted'], output_delete
assert gc['dry_run'] is True and gc['orphan_files']==0, gc
PY

echo "AgentsPool E2E passed with explicit data cleanup: task=$TASK_ID input=$INPUT_ARTIFACT_ID output=$ARTIFACT_ID"
