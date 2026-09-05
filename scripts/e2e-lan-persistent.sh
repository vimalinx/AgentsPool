#!/usr/bin/env bash
set -Eeuo pipefail

CONTROL_HOST="${AGENTSPOOL_LAN_CONTROL_HOST:-agentspool-control}"
SERVER_URL="${AGENTSPOOL_LAN_SERVER_URL:-http://127.0.0.1:8090}"
CONTROL_NODE_NAME="${AGENTSPOOL_LAN_CONTROL_NODE_NAME:-control-x86_64}"
EDGE_NODE_NAME="${AGENTSPOOL_LAN_EDGE_NODE_NAME:-edge-arm64}"
WORKSTATION_NODE_NAME="${AGENTSPOOL_LAN_WORKSTATION_NODE_NAME:-workstation-x86_64}"
SSH_OPTIONS=(-o BatchMode=yes -o StrictHostKeyChecking=yes)
WORK_DIR="$(mktemp -d /tmp/agentspool-persistent-e2e.XXXXXX)"

cleanup() {
  [[ "$WORK_DIR" == /tmp/agentspool-persistent-e2e.* ]] || return 1
  find "$WORK_DIR" -maxdepth 1 -type f -delete
  rmdir "$WORK_DIR"
}
trap cleanup EXIT

remote_admin() {
  local command="$1"
  ssh "${SSH_OPTIONS[@]}" "$CONTROL_HOST" \
    "sudo bash -c 'set -a; . /etc/agentspool/server.env; set +a; exec /usr/local/bin/agentspool $command'"
}

remote_admin "status --server $SERVER_URL" >"$WORK_DIR/status.json"
remote_admin "nodes list --server $SERVER_URL" >"$WORK_DIR/nodes.json"

python3 - "$WORK_DIR/status.json" "$WORK_DIR/nodes.json" "$CONTROL_NODE_NAME" "$EDGE_NODE_NAME" "$WORKSTATION_NODE_NAME" <<'PY'
import json
import sys
from pathlib import Path

status = json.loads(Path(sys.argv[1]).read_text())
nodes = json.loads(Path(sys.argv[2]).read_text()).get("nodes", [])
expected_names = set(sys.argv[3:])
if status.get("node_counts", {}).get("online") != len(expected_names):
    raise SystemExit(f"expected exactly {len(expected_names)} online nodes: {status.get('node_counts')}")
if len(nodes) != len(expected_names) or any(node.get("status") != "online" for node in nodes):
    raise SystemExit(f"expected exactly {len(expected_names)} online node records")
if {node.get("name") for node in nodes} != expected_names:
    raise SystemExit("unexpected node names")
for node in nodes:
    capabilities = node.get("capabilities", {})
    if capabilities.get("agent_version") != "0.4.0-alpha.3":
        raise SystemExit(f"unexpected worker version on {node.get('name')}")
    if capabilities.get("gpu") is not None:
        raise SystemExit(f"GPU telemetry unexpectedly enabled on {node.get('name')}")
    if any(value.startswith("gpu.") for value in capabilities.get("capabilities", [])):
        raise SystemExit(f"GPU capability unexpectedly published by {node.get('name')}")
    if capabilities.get("capabilities", []).count("executor.shell") != 1:
        raise SystemExit(f"shell executor missing on {node.get('name')}")
    if node.get("resources", {}).get("max_concurrency") != 1:
        raise SystemExit(f"unexpected concurrency on {node.get('name')}")
PY

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
task_script=$'set -eu\nmkdir -p outputs\nsleep 8\nprintf \'host=%s\\narch=%s\\n\' "$(hostname)" "$(uname -m)" > outputs/result.txt'

make_task() {
  local suffix="$1"
  jq -cn \
    --arg name "persistent-lan-${suffix}-${stamp}" \
    --arg key "persistent-lan-${suffix}-${stamp}" \
    --arg script "$task_script" \
    '{
      name: $name,
      idempotency_key: $key,
      queue: "default",
      max_attempts: 1,
      retry_on_failure: false,
      side_effect_mode: "idempotent",
      requirements: {capabilities: ["executor.shell"]},
      executor: {kind: "shell", script: $script},
      outputs: [{pattern: "outputs/result.txt", required: true, media_type: "text/plain"}]
    }'
}

submit_task() {
  make_task "$1" | remote_admin "tasks submit --server $SERVER_URL --file -"
}

submit_task a >"$WORK_DIR/submit-a.json" &
submit_a_pid=$!
submit_task b >"$WORK_DIR/submit-b.json" &
submit_b_pid=$!
submit_task c >"$WORK_DIR/submit-c.json" &
submit_c_pid=$!
wait "$submit_a_pid"
wait "$submit_b_pid"
wait "$submit_c_pid"

task_a="$(tr -d '\r\n' <"$WORK_DIR/submit-a.json")"
task_b="$(tr -d '\r\n' <"$WORK_DIR/submit-b.json")"
task_c="$(tr -d '\r\n' <"$WORK_DIR/submit-c.json")"
[[ "$task_a" =~ ^task_[a-zA-Z0-9]+$ ]]
[[ "$task_b" =~ ^task_[a-zA-Z0-9]+$ ]]
[[ "$task_c" =~ ^task_[a-zA-Z0-9]+$ ]]

remote_admin "tasks wait --server $SERVER_URL $task_a" >"$WORK_DIR/wait-a.json" &
wait_a_pid=$!
remote_admin "tasks wait --server $SERVER_URL $task_b" >"$WORK_DIR/wait-b.json" &
wait_b_pid=$!
remote_admin "tasks wait --server $SERVER_URL $task_c" >"$WORK_DIR/wait-c.json" &
wait_c_pid=$!
wait "$wait_a_pid"
wait "$wait_b_pid"
wait "$wait_c_pid"

remote_admin "tasks get --server $SERVER_URL $task_a" >"$WORK_DIR/task-a.json"
remote_admin "tasks get --server $SERVER_URL $task_b" >"$WORK_DIR/task-b.json"
remote_admin "tasks get --server $SERVER_URL $task_c" >"$WORK_DIR/task-c.json"

python3 - "$WORK_DIR/nodes.json" "$WORK_DIR/task-a.json" "$WORK_DIR/task-b.json" "$WORK_DIR/task-c.json" <<'PY'
import json
import sys
from pathlib import Path

nodes = json.loads(Path(sys.argv[1]).read_text()).get("nodes", [])
names = {node["id"]: node["name"] for node in nodes}
rows = []
for path in sys.argv[2:]:
    value = json.loads(Path(path).read_text())
    task = value.get("task", value)
    executions = value.get("executions") or task.get("executions") or []
    if task.get("status") != "succeeded":
        raise SystemExit(f"task did not succeed: {task.get('id')} {task.get('status')}")
    if not executions:
        raise SystemExit(f"task has no execution: {task.get('id')}")
    execution = executions[-1]
    if execution.get("status") != "succeeded":
        raise SystemExit(f"execution did not succeed: {execution.get('id')}")
    artifact_ids = execution.get("output_artifact_ids") or task.get("result", {}).get("output_artifact_ids")
    if not artifact_ids:
        raise SystemExit(f"task has no uploaded artifact: {task.get('id')}")
    rows.append((task["id"], execution["id"], execution["node_id"], task["status"]))

node_ids = {row[2] for row in rows}
if len(node_ids) != len(sys.argv) - 2:
    raise SystemExit(f"expected {len(sys.argv) - 2} distinct execution nodes, got {sorted(node_ids)}")

print("PERSISTENT_LAN_E2E=pass")
for task_id, execution_id, node_id, status in rows:
    print(
        f"task={task_id} execution={execution_id} "
        f"node={node_id} name={names.get(node_id, 'unknown')} status={status}"
    )
print(f"distinct_nodes={len(node_ids)}")
print("gpu_detection=disabled")
print("executor=shell-only")
PY
