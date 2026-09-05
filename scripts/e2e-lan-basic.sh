#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $# -lt 3 ]]; then
  echo "usage: $0 SERVER_LAN_IP_OR_tunnel SSH_TARGET SSH_TARGET [SSH_TARGET ...]" >&2
  exit 2
fi

SERVER_HOST="$1"
shift
TARGETS=("$@")
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/agentspool-lan-alpha2-local-XXXXXX)"
REMOTE_DIRS=()
REMOTE_NAMES=()
TUNNEL_PIDS=()
SERVER_PID=""

for command_name in go ssh scp curl python3; do
  command -v "$command_name" >/dev/null || { echo "missing command: $command_name" >&2; exit 1; }
done

remote_cleanup() {
  local target="$1" remote_dir="$2"
  ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=yes "$target" sh -s -- "$remote_dir" <<'SH' || true
set -eu
remote_dir="$1"
case "$remote_dir" in
  /tmp/agentspool-lan-alpha2-*) ;;
  *) echo "refusing unsafe cleanup path: $remote_dir" >&2; exit 1 ;;
esac
if [ -f "$remote_dir/worker.pid" ]; then
  worker_pid="$(cat "$remote_dir/worker.pid" 2>/dev/null || true)"
  case "$worker_pid" in
    ''|*[!0-9]*) worker_pid='' ;;
  esac
  if [ -n "$worker_pid" ] && [ -r "/proc/$worker_pid/cmdline" ]; then
    command_line="$(tr '\000' ' ' < "/proc/$worker_pid/cmdline")"
    case "$command_line" in
      *"$remote_dir/agentspool worker"*) kill -TERM "$worker_pid" 2>/dev/null || true ;;
    esac
  fi
fi
sleep 0.2
rm -rf -- "$remote_dir"
SH
}

cleanup() {
  local status=$?
  set +e
  if [[ $status -ne 0 ]]; then
    echo "--- LAN E2E server log ---" >&2
    sed -n '1,240p' "$TMP/server.log" >&2 2>/dev/null
    for index in "${!TARGETS[@]}"; do
      if [[ -n "${REMOTE_DIRS[$index]:-}" ]]; then
        echo "--- LAN E2E worker log: ${TARGETS[$index]} ---" >&2
        ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=yes "${TARGETS[$index]}" \
          "sed -n '1,240p' '${REMOTE_DIRS[$index]}/worker.log'" >&2 2>/dev/null
      fi
    done
  fi
  for index in "${!TARGETS[@]}"; do
    if [[ -n "${REMOTE_DIRS[$index]:-}" ]]; then
      remote_cleanup "${TARGETS[$index]}" "${REMOTE_DIRS[$index]}"
    fi
  done
  for tunnel_pid in "${TUNNEL_PIDS[@]:-}"; do
    if [[ -n "$tunnel_pid" ]]; then
      kill -TERM "$tunnel_pid" 2>/dev/null || true
      wait "$tunnel_pid" 2>/dev/null || true
    fi
  done
  if [[ -n "$SERVER_PID" ]]; then
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf -- "$TMP"
  exit "$status"
}
trap cleanup EXIT INT TERM

build_binary() {
  local go_arch="$1" output="$TMP/agentspool-linux-$1"
  if [[ ! -x "$output" ]]; then
    case "$go_arch" in
      arm) (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -o "$output" ./cmd/agentspool) ;;
      *) (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$go_arch" go build -trimpath -o "$output" ./cmd/agentspool) ;;
    esac
  fi
  printf '%s\n' "$output"
}

LOCAL_ARCH="$(go env GOARCH)"
LOCAL_BIN="$(build_binary "$LOCAL_ARCH")"
VERSION="$($LOCAL_BIN version)"
ADMIN_TOKEN="$($LOCAL_BIN token)"
JOIN_TOKEN="$($LOCAL_BIN token)"
PORT="$(python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(('0.0.0.0', 0))
    print(sock.getsockname()[1])
PY
)"
LOCAL_SERVER_URL="http://127.0.0.1:$PORT"
if [[ "$SERVER_HOST" == "tunnel" ]]; then
  LISTEN_ADDRESS="127.0.0.1:$PORT"
else
  LISTEN_ADDRESS="0.0.0.0:$PORT"
fi

AGENTSPOOL_ADMIN_TOKEN="$ADMIN_TOKEN" AGENTSPOOL_JOIN_TOKEN="$JOIN_TOKEN" \
  "$LOCAL_BIN" server --listen "$LISTEN_ADDRESS" --data-dir "$TMP/server" \
  --lease-ttl 15s --node-timeout 10s >"$TMP/server.log" 2>&1 &
SERVER_PID=$!

for _ in {1..100}; do
  if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null

for index in "${!TARGETS[@]}"; do
  target="${TARGETS[$index]}"
  remote_facts="$(ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=yes "$target" \
    'printf "%s\t%s\n" "$(uname -m)" "$(hostname)"')"
  remote_machine_arch="${remote_facts%%$'\t'*}"
  remote_hostname="${remote_facts#*$'\t'}"
  case "$remote_machine_arch" in
    x86_64|amd64) go_arch=amd64 ;;
    aarch64|arm64) go_arch=arm64 ;;
    armv7l|armv7) go_arch=arm ;;
    *) echo "unsupported remote architecture $remote_machine_arch on $target" >&2; exit 1 ;;
  esac
  remote_name="lan-${index}-${remote_hostname//[^a-zA-Z0-9_.-]/-}"
  remote_dir="/tmp/agentspool-lan-alpha2-$$-${index}"
  REMOTE_NAMES[$index]="$remote_name"
  REMOTE_DIRS[$index]="$remote_dir"
  binary="$(build_binary "$go_arch")"

  if [[ "$SERVER_HOST" == "tunnel" ]]; then
    ssh -N -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=yes \
      -o ExitOnForwardFailure=yes -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
      -R "127.0.0.1:$PORT:127.0.0.1:$PORT" "$target" >"$TMP/tunnel-$index.log" 2>&1 &
    tunnel_pid=$!
    TUNNEL_PIDS[$index]="$tunnel_pid"
    sleep 0.3
    if ! kill -0 "$tunnel_pid" 2>/dev/null; then
      sed -n '1,120p' "$TMP/tunnel-$index.log" >&2
      echo "could not establish SSH reverse tunnel for $target" >&2
      exit 1
    fi
    worker_server_url="http://127.0.0.1:$PORT"
  else
    worker_server_url="http://$SERVER_HOST:$PORT"
  fi

  ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=yes "$target" \
    "umask 077; mkdir -p '$remote_dir'; chmod 700 '$remote_dir'"
  scp -q -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=yes \
    "$binary" "$target:$remote_dir/agentspool"
  {
    printf 'AGENTSPOOL_SERVER=%s\n' "$worker_server_url"
    printf 'AGENTSPOOL_JOIN_TOKEN=%s\n' "$JOIN_TOKEN"
    printf 'AGENTSPOOL_WORKER_NAME=%s\n' "$remote_name"
    printf 'AGENTSPOOL_WORKER_DATA_DIR=%s/data\n' "$remote_dir"
    printf 'AGENTSPOOL_WORKER_LABELS=lan_smoke=true,lan_index=%s\n' "$index"
    printf 'AGENTSPOOL_WORKER_QUEUES=default\n'
    printf 'AGENTSPOOL_WORKER_DETECT_GPU=false\n'
    printf 'AGENTSPOOL_WORKER_ALLOWED_EXECUTORS=shell\n'
    printf 'AGENTSPOOL_WORKER_MAX_CONCURRENCY=1\n'
    printf 'AGENTSPOOL_WORKER_POLL_INTERVAL=200ms\n'
    printf 'AGENTSPOOL_WORKER_HEARTBEAT_INTERVAL=1s\n'
    printf 'AGENTSPOOL_WORKER_REQUEST_TIMEOUT=5s\n'
    printf 'AGENTSPOOL_WORKER_TRANSFER_TIMEOUT=30s\n'
  } | ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=yes "$target" \
    "umask 077; tee '$remote_dir/worker.env' >/dev/null; chmod 600 '$remote_dir/worker.env'"
  ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=yes "$target" sh -s -- "$remote_dir" <<'SH'
set -eu
remote_dir="$1"
chmod 700 "$remote_dir/agentspool"
set -a
. "$remote_dir/worker.env"
set +a
nohup "$remote_dir/agentspool" worker --detect-gpu=false --log-format json >"$remote_dir/worker.log" 2>&1 </dev/null &
printf '%s\n' "$!" >"$remote_dir/worker.pid"
SH
done

NODES_JSON="$TMP/nodes.json"
for _ in {1..150}; do
  AGENTSPOOL_ADMIN_TOKEN="$ADMIN_TOKEN" "$LOCAL_BIN" nodes list --server "$LOCAL_SERVER_URL" >"$NODES_JSON"
  if python3 - "$NODES_JSON" "${#TARGETS[@]}" "$VERSION" <<'PY'
import json,sys
data=json.load(open(sys.argv[1])); expected=int(sys.argv[2]); version=sys.argv[3]
nodes=[node for node in data.get('nodes',[]) if node.get('name','').startswith('lan-')]
ok=len(nodes)==expected and all(node.get('status')=='online' for node in nodes)
ok=ok and all(node.get('capabilities',{}).get('agent_version')==version for node in nodes)
raise SystemExit(0 if ok else 1)
PY
  then break; fi
  sleep 0.2
done

python3 - "$NODES_JSON" "${#TARGETS[@]}" "$VERSION" <<'PY'
import json,sys
data=json.load(open(sys.argv[1])); expected=int(sys.argv[2]); version=sys.argv[3]
nodes=[node for node in data.get('nodes',[]) if node.get('name','').startswith('lan-')]
assert len(nodes)==expected, nodes
for node in nodes:
    caps=node['capabilities']; resources=node.get('resources',{})
    assert node['status']=='online', node
    assert caps['agent_version']==version, caps
    assert not caps.get('gpus'), caps
    assert not resources.get('gpus'), resources
    assert not any(value.startswith('gpu.') for value in caps.get('capabilities',[])), caps
PY

TASK_IDS=()
for index in "${!TARGETS[@]}"; do
  task_file="$TMP/task-$index.json"
  python3 - "$task_file" "$index" <<'PY'
import json,sys
path,index=sys.argv[1:]
spec={
  "name":f"LAN minimum scheduler task {index}",
  "queue":"default",
  "requirements":{"capabilities":["executor.shell"],"os":"linux","min_cpu_threads":1,"min_memory_mb":64,"min_disk_mb":32},
  "executor":{"kind":"shell","script":"set -eu\nsleep 3\nmkdir -p outputs\nprintf 'host=%s arch=%s task=%s\\n' \"$(hostname)\" \"$(uname -m)\" '"+index+"' > outputs/result.txt"},
  "outputs":[{"pattern":"outputs/result.txt","required":True,"media_type":"text/plain"}]
}
json.dump(spec,open(path,'w'))
PY
  TASK_IDS[$index]="$(AGENTSPOOL_ADMIN_TOKEN="$ADMIN_TOKEN" "$LOCAL_BIN" tasks submit --server "$LOCAL_SERVER_URL" --file "$task_file")"
done

for task_id in "${TASK_IDS[@]}"; do
  AGENTSPOOL_ADMIN_TOKEN="$ADMIN_TOKEN" "$LOCAL_BIN" tasks wait "$task_id" --server "$LOCAL_SERVER_URL" >/dev/null
done

DETAIL_FILES=()
for index in "${!TASK_IDS[@]}"; do
  detail_file="$TMP/detail-$index.json"
  DETAIL_FILES[$index]="$detail_file"
  AGENTSPOOL_ADMIN_TOKEN="$ADMIN_TOKEN" "$LOCAL_BIN" tasks get "${TASK_IDS[$index]}" --server "$LOCAL_SERVER_URL" >"$detail_file"
done

python3 - "$NODES_JSON" "${DETAIL_FILES[@]}" <<'PY'
import json,sys
nodes=json.load(open(sys.argv[1]))['nodes']
details=[json.load(open(path)) for path in sys.argv[2:]]
assigned=[]; summary=[]
for detail in details:
    task=detail['task']; assert task['status']=='succeeded', task
    executions=detail['executions']; assert len(executions)==1, executions
    execution=executions[0]; assert execution['status']=='succeeded', execution
    assigned.append(execution['node_id'])
    assert len(task['result']['output_artifact_ids'])==1, task
    summary.append({'task_id':task['id'],'execution_id':execution['id'],'node_id':execution['node_id'],'status':task['status']})
assert len(set(assigned))==len(details), assigned
node_summary=[]
for node in nodes:
    if node['id'] in assigned:
        node_summary.append({'node_id':node['id'],'name':node['name'],'arch':node['capabilities']['arch'],'version':node['capabilities']['agent_version']})
print(json.dumps({'result':'PASS','nodes':sorted(node_summary,key=lambda item:item['name']),'tasks':summary,'distinct_nodes':len(set(assigned))},indent=2))
PY

for index in "${!DETAIL_FILES[@]}"; do
  artifact_id="$(python3 - "${DETAIL_FILES[$index]}" <<'PY'
import json,sys
print(json.load(open(sys.argv[1]))['task']['result']['output_artifact_ids'][0])
PY
)"
  AGENTSPOOL_ADMIN_TOKEN="$ADMIN_TOKEN" "$LOCAL_BIN" artifacts download "$artifact_id" \
    --server "$LOCAL_SERVER_URL" --output "$TMP/result-$index.txt" >/dev/null
  grep -Eq '^host=.+ arch=.+ task=[0-9]+$' "$TMP/result-$index.txt"
done
