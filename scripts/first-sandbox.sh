#!/usr/bin/env bash
set -Eeuo pipefail
# Uses the installed client profile or AGENTSPOOL_SERVER + ALLOCATOR_TOKEN_FILE.
NODE_ID="${1:?usage: first-sandbox.sh NODE_ID /absolute/output-file}"
OUTPUT="${2:?provide an absolute output file}"
[[ "$OUTPUT" == /* && ! -e "$OUTPUT" ]] || { echo "output must be absolute and must not exist" >&2; exit 2; }
command -v agentspool >/dev/null
command -v jq >/dev/null
umask 077
WORK="$(mktemp -d /tmp/agentspool-first-sandbox.XXXXXXXX)"
LEASE=""; SANDBOX=""
lease_tool() { AGENTSPOOL_RESOURCE_LEASE_TOKEN_FILE="$WORK/lease.token" agentspool tool "$@"; }
sandbox_tool() { AGENTSPOOL_SANDBOX_TOKEN_FILE="$WORK/sandbox.token" agentspool tool "$@"; }
cleanup() {
  local status=$? cleaned=true
  trap - EXIT
  if [[ -n "$SANDBOX" ]]; then
    if sandbox_tool sandboxes.destroy --input "$(jq -cn --arg id "$SANDBOX" '{sandbox_id:$id}')" >"$WORK/destroy.json"; then
      local operation
      operation="$(jq -er '.operation.id' "$WORK/destroy.json")"
      sandbox_tool operations.wait --input "$(jq -cn --arg id "$operation" '{operation_id:$id}')" >/dev/null || cleaned=false
    else cleaned=false; fi
  fi
  if [[ "$cleaned" == true && -n "$LEASE" ]]; then
    lease_tool leases.release --input "$(jq -cn --arg id "$LEASE" '{lease_id:$id}')" >/dev/null || cleaned=false
  fi
  if [[ "$cleaned" == true ]]; then rm -rf -- "$WORK";
  else echo "Cleanup needs retry; protected scoped credentials and IDs remain in $WORK" >&2; status=1; fi
  exit "$status"
}
trap cleanup EXIT
ACQUIRED=false
for ((attempt=0;attempt<3;attempt++)); do
  agentspool tool devices.list >"$WORK/devices.json"
  jq -e --arg id "$NODE_ID" '.devices[] | select(.id==$id) | .capabilities.capabilities | index("sandbox.systemd-bwrap") != null' "$WORK/devices.json" >/dev/null
  REVISION="$(jq -er '.revision' "$WORK/devices.json")"
  if agentspool tool leases.acquire --input "$(jq -cn --arg node "$NODE_ID" --argjson revision "$REVISION" '{node_id:$node,expected_revision:$revision,resources:{cpu_millis:250,memory_mb:128,disk_mb:64},ttl_seconds:0}')" >"$WORK/lease.json" 2>"$WORK/acquire.err"; then
    ACQUIRED=true; break
  fi
  # Only a confirmed stale-revision rejection is safe to retry. Unknown
  # transport outcomes must not allocate a second lease automatically.
  if ! grep -Fq 'HTTP 409 (stale_registry_revision)' "$WORK/acquire.err"; then
    cat "$WORK/acquire.err" >&2; exit 1
  fi
done
[[ "$ACQUIRED" == true ]] || { cat "$WORK/acquire.err" >&2; exit 1; }
LEASE="$(jq -er '.lease.id' "$WORK/lease.json")"
jq -er '.lease_token' "$WORK/lease.json" >"$WORK/lease.token"
lease_tool sandboxes.ensure --input "$(jq -cn --arg id "$LEASE" '{name:"first-sandbox",resource_lease_id:$id,permissions:{exec:true,data_read:true,data_write:true,network:false}}')" >"$WORK/sandbox.json"
SANDBOX="$(jq -er '.sandbox.id' "$WORK/sandbox.json")"
jq -er '.sandbox_token' "$WORK/sandbox.json" >"$WORK/sandbox.token"
OP="$(jq -er '.operation.id' "$WORK/sandbox.json")"
sandbox_tool operations.wait --input "$(jq -cn --arg id "$OP" '{operation_id:$id}')" >/dev/null
printf 'hello from AgentsPool\n' >"$WORK/input.txt"
sandbox_tool files.push --input "$(jq -cn --arg id "$SANDBOX" --arg file "$WORK/input.txt" '{sandbox_id:$id,local_file:$file,path:"/workspace/input.txt"}')" >/dev/null
sandbox_tool sandboxes.exec --input "$(jq -cn --arg id "$SANDBOX" '{sandbox_id:$id,command:["/bin/sh","-c","tr a-z A-Z < input.txt > result.txt; echo completed"],working_dir:"/workspace"}')" >"$WORK/exec.json"
OP="$(jq -er '.operation.id' "$WORK/exec.json")"
sandbox_tool operations.wait --input "$(jq -cn --arg id "$OP" '{operation_id:$id}')" >/dev/null
sandbox_tool operations.logs --input "$(jq -cn --arg id "$OP" '{operation_id:$id,after:0}')" | jq -r '.data'
sandbox_tool files.pull --input "$(jq -cn --arg id "$SANDBOX" --arg output "$OUTPUT" '{sandbox_id:$id,path:"/workspace/result.txt",output:$output}')" >/dev/null
grep -qx 'HELLO FROM AGENTSPOOL' "$OUTPUT"
echo "Verified result: $OUTPUT"
