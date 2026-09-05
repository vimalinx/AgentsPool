#!/usr/bin/env bash
set -Eeuo pipefail

CONTROL_HOST="${AGENTSPOOL_LAN_CONTROL_HOST:-agentspool-control}"
EDGE_HOST="${AGENTSPOOL_LAN_EDGE_HOST:-agentspool-edge}"
WORKSTATION_HOST="${AGENTSPOOL_LAN_WORKSTATION_HOST:-agentspool-workstation}"
SERVER_URL="${AGENTSPOOL_LAN_SERVER_URL:-http://127.0.0.1:8090}"
CONTROL_NODE_NAME="${AGENTSPOOL_LAN_CONTROL_NODE_NAME:-control-x86_64}"
EDGE_NODE_NAME="${AGENTSPOOL_LAN_EDGE_NODE_NAME:-edge-arm64}"
WORKSTATION_NODE_NAME="${AGENTSPOOL_LAN_WORKSTATION_NODE_NAME:-workstation-x86_64}"
SSH_OPTIONS=(-o BatchMode=yes -o StrictHostKeyChecking=yes)

for value in "$CONTROL_HOST" "$EDGE_HOST" "$WORKSTATION_HOST" "$CONTROL_NODE_NAME" "$EDGE_NODE_NAME" "$WORKSTATION_NODE_NAME"; do
  [[ "$value" =~ ^[a-zA-Z0-9._-]+$ ]]
done
[[ "$SERVER_URL" =~ ^https?://[a-zA-Z0-9.:-]+$ ]]

inspect_worker() {
  local host="$1" node_name="$2" expect_server="$3"
  ssh "${SSH_OPTIONS[@]}" "$host" bash -s -- "$node_name" "$expect_server" <<'REMOTE'
set -eu
node_name="$1"
expect_server="$2"
test "$(/usr/local/bin/agentspool version)" = 0.4.0-alpha.3
test -L /etc/systemd/system/multi-user.target.wants/agentspool-worker.service
test -L /etc/systemd/system/multi-user.target.wants/agentspool-sandboxd.service
pgrep -f "^/usr/local/bin/agentspool worker$" >/dev/null
pgrep -f "^/usr/local/bin/agentspool sandboxd$" >/dev/null
sudo -u agentspool curl -fsS --unix-socket /run/agentspool/sandboxd.sock http://sandboxd/v1/ping | grep -q "gpu_access.*false"
sudo grep -Fxq "AGENTSPOOL_WORKER_NAME=$node_name" /etc/agentspool/worker.env
sudo grep -Fxq "AGENTSPOOL_WORKER_DETECT_GPU=false" /etc/agentspool/worker.env
sudo grep -Fxq "AGENTSPOOL_WORKER_ALLOWED_EXECUTORS=shell" /etc/agentspool/worker.env
sudo grep -Fxq "AGENTSPOOL_WORKER_MAX_CONCURRENCY=1" /etc/agentspool/worker.env
! sudo grep -Eq "^AGENTSPOOL_JOIN_TOKEN(_FILE)?=" /etc/agentspool/worker.env
test ! -e /etc/agentspool/worker-join.token
if [ "$expect_server" = yes ]; then
  test -L /etc/systemd/system/multi-user.target.wants/agentspool-server.service
  pgrep -f "^/usr/local/bin/agentspool server$" >/dev/null
  curl -fsS http://127.0.0.1:8090/healthz >/dev/null
fi
REMOTE
}

inspect_worker "$CONTROL_HOST" "$CONTROL_NODE_NAME" yes
inspect_worker "$EDGE_HOST" "$EDGE_NODE_NAME" no
inspect_worker "$WORKSTATION_HOST" "$WORKSTATION_NODE_NAME" no

ssh "${SSH_OPTIONS[@]}" "$CONTROL_HOST" \
  "sudo bash -c 'set -a; . /etc/agentspool/server.env; set +a; exec /usr/local/bin/agentspool nodes list --server $SERVER_URL'" \
  | python3 -c '
import json
import sys

expected = set(sys.argv[1:])
value = json.load(sys.stdin)
nodes = value.get("nodes", [])
if value.get("count") != len(expected) or len(nodes) != len(expected):
    raise SystemExit(f"expected exactly {len(expected)} nodes")
if any(node.get("status") != "online" for node in nodes):
    raise SystemExit("expected all nodes online")
if {node.get("name") for node in nodes} != expected:
    raise SystemExit("unexpected node names")
for node in nodes:
    capabilities = node.get("capabilities", {})
    if capabilities.get("gpu") is not None:
        raise SystemExit("GPU telemetry unexpectedly enabled")
    if any(item.startswith("gpu.") for item in capabilities.get("capabilities", [])):
        raise SystemExit("GPU capability unexpectedly published")
    if "sandbox.systemd-bwrap" not in capabilities.get("capabilities", []):
        raise SystemExit("sandbox helper capability missing")
' "$CONTROL_NODE_NAME" "$EDGE_NODE_NAME" "$WORKSTATION_NODE_NAME"

echo "PERSISTENT_LAN_INSPECT=pass"
echo "control_host=$CONTROL_HOST"
echo "nodes=$CONTROL_NODE_NAME,$EDGE_NODE_NAME,$WORKSTATION_NODE_NAME"
echo "gpu_detection=disabled"
echo "executor=shell-only"
echo "join_token_locator_and_file=absent"
