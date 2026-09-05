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

credential_digest() {
  ssh "${SSH_OPTIONS[@]}" "$1" \
    'sudo sha256sum /var/lib/agentspool/worker/credentials.json | cut -d" " -f1'
}

control_before="$(credential_digest "$CONTROL_HOST")"
edge_before="$(credential_digest "$EDGE_HOST")"
workstation_before="$(credential_digest "$WORKSTATION_HOST")"

ssh "${SSH_OPTIONS[@]}" "$CONTROL_HOST" \
  'sudo systemctl restart agentspool-server.service; sudo systemctl restart agentspool-worker.service'
ssh "${SSH_OPTIONS[@]}" "$EDGE_HOST" \
  'sudo systemctl restart agentspool-worker.service'
ssh "${SSH_OPTIONS[@]}" "$WORKSTATION_HOST" \
  'sudo systemctl restart agentspool-worker.service'

online=0
for _ in $(seq 1 30); do
  online="$(
    ssh "${SSH_OPTIONS[@]}" "$CONTROL_HOST" \
      "sudo bash -c 'set -a; . /etc/agentspool/server.env; set +a; /usr/local/bin/agentspool status --server $SERVER_URL'" \
      | jq -r '.node_counts.online // 0'
  )"
  [[ "$online" == 3 ]] && break
  sleep 1
done
[[ "$online" == 3 ]]

control_after="$(credential_digest "$CONTROL_HOST")"
edge_after="$(credential_digest "$EDGE_HOST")"
workstation_after="$(credential_digest "$WORKSTATION_HOST")"
[[ "$control_before" == "$control_after" ]]
[[ "$edge_before" == "$edge_after" ]]
[[ "$workstation_before" == "$workstation_after" ]]

ssh "${SSH_OPTIONS[@]}" "$CONTROL_HOST" \
  'test -L /etc/systemd/system/multi-user.target.wants/agentspool-server.service; test -L /etc/systemd/system/multi-user.target.wants/agentspool-worker.service; pgrep -f "^/usr/local/bin/agentspool server$" >/dev/null; pgrep -f "^/usr/local/bin/agentspool worker$" >/dev/null; curl -fsS http://127.0.0.1:8090/healthz >/dev/null'
ssh "${SSH_OPTIONS[@]}" "$EDGE_HOST" \
  'test -L /etc/systemd/system/multi-user.target.wants/agentspool-worker.service; pgrep -f "^/usr/local/bin/agentspool worker$" >/dev/null'
ssh "${SSH_OPTIONS[@]}" "$WORKSTATION_HOST" \
  'test -L /etc/systemd/system/multi-user.target.wants/agentspool-worker.service; pgrep -f "^/usr/local/bin/agentspool worker$" >/dev/null'

echo "PERSISTENT_LAN_RESTART=pass"
echo "online_nodes=$online"
echo "control_identity_preserved=yes"
echo "edge_identity_preserved=yes"
echo "workstation_identity_preserved=yes"

AGENTSPOOL_LAN_CONTROL_HOST="$CONTROL_HOST" \
AGENTSPOOL_LAN_CONTROL_NODE_NAME="$CONTROL_NODE_NAME" \
AGENTSPOOL_LAN_EDGE_NODE_NAME="$EDGE_NODE_NAME" \
AGENTSPOOL_LAN_WORKSTATION_NODE_NAME="$WORKSTATION_NODE_NAME" \
AGENTSPOOL_LAN_SERVER_URL="$SERVER_URL" \
  "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lan-persistent.sh"
