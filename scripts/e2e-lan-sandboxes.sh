#!/usr/bin/env bash
set -Eeuo pipefail

CONTROL_HOST="${AGENTSPOOL_LAN_CONTROL_HOST:-agentspool-control}"
CONTROL_NODE_NAME="${AGENTSPOOL_LAN_CONTROL_NODE_NAME:-control-x86_64}"
EDGE_NODE_NAME="${AGENTSPOOL_LAN_EDGE_NODE_NAME:-edge-arm64}"
WORKSTATION_NODE_NAME="${AGENTSPOOL_LAN_WORKSTATION_NODE_NAME:-workstation-x86_64}"
NETWORK_HEALTH_URL="${AGENTSPOOL_LAN_NETWORK_HEALTH_URL:-http://192.0.2.10:8090/healthz}"
SSH_OPTIONS=(-o BatchMode=yes -o StrictHostKeyChecking=yes)

[[ "$CONTROL_HOST" =~ ^[a-zA-Z0-9._-]+$ ]]
[[ "$CONTROL_NODE_NAME" =~ ^[a-zA-Z0-9._-]+$ ]]
[[ "$EDGE_NODE_NAME" =~ ^[a-zA-Z0-9._-]+$ ]]
[[ "$WORKSTATION_NODE_NAME" =~ ^[a-zA-Z0-9._-]+$ ]]
[[ "$NETWORK_HEALTH_URL" =~ ^https?://[a-zA-Z0-9.:-]+/healthz$ ]]

ssh "${SSH_OPTIONS[@]}" "$CONTROL_HOST" sudo bash -s -- \
  "$CONTROL_NODE_NAME" "$EDGE_NODE_NAME" "$WORKSTATION_NODE_NAME" "$NETWORK_HEALTH_URL" <<'REMOTE'
set -Eeuo pipefail
umask 077
node_names=("$1" "$2" "$3")
network_health_url="$4"
set -a
. /etc/agentspool/server.env
set +a
server=https://127.0.0.1:8090
work_dir="$(mktemp -d /tmp/agentspool-sandbox-e2e.XXXXXX)"
active_lease=""
active_lease_token=""
pending_lease=""
pending_lease_token=""
active_sandbox=""
active_sandbox_token=""

admin_tool() {
  /usr/local/bin/agentspool tool "$@" --server "$server"
}

lease_tool() {
  local token="$1"
  shift
  env -u AGENTSPOOL_ADMIN_TOKEN -u AGENTSPOOL_ADMIN_TOKEN_FILE AGENTSPOOL_RESOURCE_LEASE_TOKEN="$token" \
    /usr/local/bin/agentspool tool "$@" --server "$server"
}

sandbox_tool() {
  local token="$1"
  shift
  env -u AGENTSPOOL_ADMIN_TOKEN -u AGENTSPOOL_ADMIN_TOKEN_FILE -u AGENTSPOOL_RESOURCE_LEASE_TOKEN AGENTSPOOL_SANDBOX_TOKEN="$token" \
    /usr/local/bin/agentspool tool "$@" --server "$server"
}

sandbox_lease_tool() {
  local sandbox_token="$1"
  local lease_token="$2"
  shift 2
  env -u AGENTSPOOL_ADMIN_TOKEN -u AGENTSPOOL_ADMIN_TOKEN_FILE AGENTSPOOL_SANDBOX_TOKEN="$sandbox_token" AGENTSPOOL_RESOURCE_LEASE_TOKEN="$lease_token" \
    /usr/local/bin/agentspool tool "$@" --server "$server"
}

acquire_resource_lease() {
  local node_name="$1"
  local owner="$2"
  local cpu_millis="$3"
  local memory_mb="$4"
  local disk_mb="$5"
  local output="$6"
  local request
  while true; do
    admin_tool devices.list >"$work_dir/acquire-devices.json"
    acquire_revision="$(jq -er '.revision' "$work_dir/acquire-devices.json")"
    acquire_node_id="$(jq -er --arg name "$node_name" '.devices[] | select(.name == $name and .status == "online") | .id' "$work_dir/acquire-devices.json")"
    request="$(jq -cn \
      --arg node_id "$acquire_node_id" \
      --arg owner "$owner" \
      --argjson revision "$acquire_revision" \
      --argjson cpu_millis "$cpu_millis" \
      --argjson memory_mb "$memory_mb" \
      --argjson disk_mb "$disk_mb" \
      '{node_id:$node_id,owner:$owner,expected_revision:$revision,resources:{cpu_millis:$cpu_millis,memory_mb:$memory_mb,disk_mb:$disk_mb}}')"
    if admin_tool leases.acquire --input "$request" >"$output" 2>"$work_dir/acquire-error.txt"; then
      return 0
    fi
    if ! grep -q 'stale_registry_revision' "$work_dir/acquire-error.txt"; then
      cat "$work_dir/acquire-error.txt" >&2
      return 1
    fi
  done
}

resize_sandbox() {
  local sandbox_id="$1"
  local sandbox_token="$2"
  local cpu_millis="$3"
  local memory_mb="$4"
  local disk_mb="$5"
  local output="$6"
  local request
  while true; do
    admin_tool devices.list >"$work_dir/resize-devices.json"
    resize_revision="$(jq -er '.revision' "$work_dir/resize-devices.json")"
    request="$(jq -cn \
      --arg sandbox_id "$sandbox_id" \
      --argjson revision "$resize_revision" \
      --argjson cpu_millis "$cpu_millis" \
      --argjson memory_mb "$memory_mb" \
      --argjson disk_mb "$disk_mb" \
      '{sandbox_id:$sandbox_id,expected_revision:$revision,resources:{cpu_millis:$cpu_millis,memory_mb:$memory_mb,disk_mb:$disk_mb}}')"
    if sandbox_tool "$sandbox_token" sandboxes.resize --input "$request" >"$output" 2>"$work_dir/resize-error.txt"; then
      return 0
    fi
    if ! grep -Eq 'stale_registry_revision|expected revision .* current revision' "$work_dir/resize-error.txt"; then
      cat "$work_dir/resize-error.txt" >&2
      return 1
    fi
  done
}

cleanup_active() {
  if [[ -n "$active_sandbox" && -n "$active_sandbox_token" ]]; then
    destroy_json="$(jq -cn --arg sandbox_id "$active_sandbox" '{sandbox_id:$sandbox_id}')"
    if sandbox_tool "$active_sandbox_token" sandboxes.destroy --input "$destroy_json" >"$work_dir/cleanup-destroy.json" 2>/dev/null; then
      cleanup_op="$(jq -r '.operation.id // empty' "$work_dir/cleanup-destroy.json")"
      if [[ -n "$cleanup_op" ]]; then
        sandbox_tool "$active_sandbox_token" operations.wait --input "$(jq -cn --arg operation_id "$cleanup_op" '{operation_id:$operation_id}')" >/dev/null 2>&1 || true
      fi
    fi
  fi
  if [[ -n "$active_lease" && -n "$active_lease_token" ]]; then
    lease_tool "$active_lease_token" leases.release --input "$(jq -cn --arg lease_id "$active_lease" '{lease_id:$lease_id}')" >/dev/null 2>&1 || true
  fi
  if [[ -n "$pending_lease" && -n "$pending_lease_token" ]]; then
    lease_tool "$pending_lease_token" leases.release --input "$(jq -cn --arg lease_id "$pending_lease" '{lease_id:$lease_id}')" >/dev/null 2>&1 || true
  fi
  active_lease=""
  active_lease_token=""
  pending_lease=""
  pending_lease_token=""
  active_sandbox=""
  active_sandbox_token=""
}

cleanup() {
  cleanup_active
  case "$work_dir" in
    /tmp/agentspool-sandbox-e2e.*)
      find "$work_dir" -mindepth 1 -maxdepth 1 -type f -delete
      rmdir "$work_dir"
      ;;
  esac
}
trap cleanup EXIT

for node_name in "${node_names[@]}"; do
  echo "sandbox_node=$node_name step=inspect"
  acquire_resource_lease "$node_name" "lan-e2e-$node_name" 250 96 64 "$work_dir/acquire.json"
  revision="$acquire_revision"
  node_id="$acquire_node_id"
  echo "sandbox_node=$node_name step=lease_acquired"
  active_lease="$(jq -er '.lease.id' "$work_dir/acquire.json")"
  active_lease_token="$(jq -er '.lease_token' "$work_dir/acquire.json")"
  acquired_revision="$(jq -er '.revision' "$work_dir/acquire.json")"

  admin_tool devices.watch --input "$(jq -cn --argjson revision "$revision" '{since_revision:$revision,wait_seconds:0}')" >"$work_dir/watch.json"
  jq -e --argjson acquired "$acquired_revision" '.changed == true and .revision >= $acquired and ([.events[].type] | index("resource_lease.acquired") != null)' "$work_dir/watch.json" >/dev/null

  if [[ "$node_name" == "${node_names[0]}" ]]; then
    network_permission=false
    network_check='! grep -Eq "^[^[:space:]]+[[:space:]]+00000000[[:space:]]" /proc/net/route'
  else
    network_permission=true
    network_check="curl -fsS $network_health_url | grep -q status"
  fi
  ensure_json="$(jq -cn --arg name "persistent-$node_name" --arg resource_lease_id "$active_lease" --argjson network "$network_permission" '{name:$name,resource_lease_id:$resource_lease_id,permissions:{exec:true,data_read:true,data_write:true,network:$network}}')"
  lease_tool "$active_lease_token" sandboxes.ensure --input "$ensure_json" >"$work_dir/ensure.json"
  active_sandbox="$(jq -er '.sandbox.id' "$work_dir/ensure.json")"
  active_sandbox_token="$(jq -er '.sandbox_token' "$work_dir/ensure.json")"
  ensure_op="$(jq -er '.operation.id' "$work_dir/ensure.json")"
  sandbox_tool "$active_sandbox_token" operations.wait --input "$(jq -cn --arg operation_id "$ensure_op" '{operation_id:$operation_id}')" >"$work_dir/ensure-wait.json"
  echo "sandbox_node=$node_name step=sandbox_ready"

  stamp="$(date -u +%Y%m%dT%H%M%SZ)-$node_name"
  printf 'source=%s\n' "$stamp" >"$work_dir/input.txt"
  push_json="$(jq -cn --arg sandbox_id "$active_sandbox" --arg local_file "$work_dir/input.txt" '{sandbox_id:$sandbox_id,local_file:$local_file,path:"/workspace/input.txt"}')"
  sandbox_tool "$active_sandbox_token" files.push --input "$push_json" >"$work_dir/push.json"
  echo "sandbox_node=$node_name step=file_pushed"

  exec_json="$(jq -cn --arg sandbox_id "$active_sandbox" --arg network_check "$network_check" '{sandbox_id:$sandbox_id,command:["/bin/sh","-c",("set -eu; cat input.txt > output.txt; printf \"arch=%s\n\" \"$(uname -m)\" >> output.txt; printf persistent > marker.txt; test ! -e /etc/shadow; test ! -e /dev/nvidia0; " + $network_check)],working_dir:"/workspace"}')"
  sandbox_tool "$active_sandbox_token" sandboxes.exec --input "$exec_json" >"$work_dir/exec.json"
  exec_op="$(jq -er '.operation.id' "$work_dir/exec.json")"
  if ! sandbox_tool "$active_sandbox_token" operations.wait --input "$(jq -cn --arg operation_id "$exec_op" '{operation_id:$operation_id}')" >"$work_dir/exec-wait.json"; then
    jq -c '.operation | {status,exit_code,stderr,error}' "$work_dir/exec-wait.json"
    exit 1
  fi
  echo "sandbox_node=$node_name step=first_exec"
  jq -e '.operation.status == "succeeded" and .operation.exit_code == 0' "$work_dir/exec-wait.json" >/dev/null

  resize_sandbox "$active_sandbox" "$active_sandbox_token" 300 128 96 "$work_dir/resize.json"
  resize_op="$(jq -er '.operation.id' "$work_dir/resize.json")"
  sandbox_tool "$active_sandbox_token" operations.wait --input "$(jq -cn --arg operation_id "$resize_op" '{operation_id:$operation_id}')" >"$work_dir/resize-wait.json"
  echo "sandbox_node=$node_name step=resized"

  acquire_resource_lease "$node_name" "lan-e2e-rebind-$node_name" 280 112 96 "$work_dir/replacement.json"
  rebind_revision="$acquire_revision"
  pending_lease="$(jq -er '.lease.id' "$work_dir/replacement.json")"
  pending_lease_token="$(jq -er '.lease_token' "$work_dir/replacement.json")"
  rebind_json="$(jq -cn --arg sandbox_id "$active_sandbox" --arg resource_lease_id "$pending_lease" '{sandbox_id:$sandbox_id,resource_lease_id:$resource_lease_id}')"
  sandbox_lease_tool "$active_sandbox_token" "$pending_lease_token" sandboxes.rebind --input "$rebind_json" >"$work_dir/rebind.json"
  rebind_op="$(jq -er '.operation.id' "$work_dir/rebind.json")"
  sandbox_tool "$active_sandbox_token" operations.wait --input "$(jq -cn --arg operation_id "$rebind_op" '{operation_id:$operation_id}')" >"$work_dir/rebind-wait.json"
  lease_tool "$active_lease_token" leases.release --input "$(jq -cn --arg lease_id "$active_lease" '{lease_id:$lease_id}')" >"$work_dir/old-release.json"
  active_lease="$pending_lease"
  active_lease_token="$pending_lease_token"
  pending_lease=""
  pending_lease_token=""
  echo "sandbox_node=$node_name step=rebound"

  second_exec_json="$(jq -cn --arg sandbox_id "$active_sandbox" '{sandbox_id:$sandbox_id,command:["/bin/sh","-c","test \"$(cat marker.txt)\" = persistent"],working_dir:"/workspace"}')"
  sandbox_tool "$active_sandbox_token" sandboxes.exec --input "$second_exec_json" >"$work_dir/exec-second.json"
  second_op="$(jq -er '.operation.id' "$work_dir/exec-second.json")"
  if ! sandbox_tool "$active_sandbox_token" operations.wait --input "$(jq -cn --arg operation_id "$second_op" '{operation_id:$operation_id}')" >"$work_dir/second-wait.json"; then
    jq -c '.operation | {status,exit_code,stdout,stderr,error}' "$work_dir/second-wait.json"
    exit 1
  fi
  echo "sandbox_node=$node_name step=second_exec"

  sandbox_tool "$active_sandbox_token" sandboxes.stop --input "$(jq -cn --arg sandbox_id "$active_sandbox" '{sandbox_id:$sandbox_id}')" >"$work_dir/stop.json"
  stop_op="$(jq -er '.operation.id' "$work_dir/stop.json")"
  sandbox_tool "$active_sandbox_token" operations.wait --input "$(jq -cn --arg operation_id "$stop_op" '{operation_id:$operation_id}')" >"$work_dir/stop-wait.json"
  sandbox_tool "$active_sandbox_token" sandboxes.start --input "$(jq -cn --arg sandbox_id "$active_sandbox" '{sandbox_id:$sandbox_id}')" >"$work_dir/start.json"
  start_op="$(jq -er '.operation.id' "$work_dir/start.json")"
  sandbox_tool "$active_sandbox_token" operations.wait --input "$(jq -cn --arg operation_id "$start_op" '{operation_id:$operation_id}')" >"$work_dir/start-wait.json"
  resumed_exec_json="$(jq -cn --arg sandbox_id "$active_sandbox" '{sandbox_id:$sandbox_id,command:["/bin/sh","-c","test \"$(cat marker.txt)\" = persistent"],working_dir:"/workspace"}')"
  sandbox_tool "$active_sandbox_token" sandboxes.exec --input "$resumed_exec_json" >"$work_dir/exec-resumed.json"
  resumed_op="$(jq -er '.operation.id' "$work_dir/exec-resumed.json")"
  sandbox_tool "$active_sandbox_token" operations.wait --input "$(jq -cn --arg operation_id "$resumed_op" '{operation_id:$operation_id}')" >"$work_dir/resumed-wait.json"
  echo "sandbox_node=$node_name step=stop-start-persisted"

  pull_json="$(jq -cn --arg sandbox_id "$active_sandbox" --arg output "$work_dir/output.txt" '{sandbox_id:$sandbox_id,path:"/workspace/output.txt",output:$output}')"
  sandbox_tool "$active_sandbox_token" files.pull --input "$pull_json" >"$work_dir/pull.json"
  echo "sandbox_node=$node_name step=file_pulled"
  grep -qx "source=$stamp" "$work_dir/output.txt"
  grep -Eq '^arch=(x86_64|aarch64)$' "$work_dir/output.txt"

  destroy_json="$(jq -cn --arg sandbox_id "$active_sandbox" '{sandbox_id:$sandbox_id}')"
  sandbox_tool "$active_sandbox_token" sandboxes.destroy --input "$destroy_json" >"$work_dir/destroy.json"
  destroy_op="$(jq -er '.operation.id' "$work_dir/destroy.json")"
  sandbox_tool "$active_sandbox_token" operations.wait --input "$(jq -cn --arg operation_id "$destroy_op" '{operation_id:$operation_id}')" >/dev/null
  active_sandbox=""
  active_sandbox_token=""
  lease_tool "$active_lease_token" leases.release --input "$(jq -cn --arg lease_id "$active_lease" '{lease_id:$lease_id}')" >/dev/null
  active_lease=""
  active_lease_token=""
  echo "sandbox_node=$node_name status=pass"
done

echo "LAN_SANDBOX_E2E=pass"
echo "gpu_access=false"
echo "placement=agent-selected"
echo "tokens=scoped"
echo "data=artifact-push-pull"
echo "network=isolated-and-explicit-allow-verified"
REMOTE
