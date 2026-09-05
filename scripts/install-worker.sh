#!/usr/bin/env bash
set -Eeuo pipefail

[[ ${EUID:-$(id -u)} -eq 0 ]] || { echo "run as root" >&2; exit 1; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE="${1:-$ROOT/agentspool}"
[[ -x "$SOURCE" || $# -gt 0 ]] || SOURCE="$ROOT/dist/agentspool"

source "$ROOT/scripts/install-common.sh"
for dependency in bwrap systemd-run setpriv mkfs.ext4 e2fsck resize2fs; do
  command -v "$dependency" >/dev/null || { echo "missing dependency: $dependency" >&2; exit 1; }
done
if [[ ! -e /etc/agentspool/worker.env ]]; then
  [[ -n "${AGENTSPOOL_SERVER:-}" && ( -n "${AGENTSPOOL_JOIN_TOKEN:-}" || -f "${AGENTSPOOL_JOIN_TOKEN_FILE:-}" ) ]] || { echo "set AGENTSPOOL_SERVER and AGENTSPOOL_JOIN_TOKEN_FILE before installation" >&2; exit 1; }
fi
install_begin
[[ -x "$SOURCE" ]] || { echo "binary not found or not executable: $SOURCE" >&2; exit 1; }

getent group agentspool >/dev/null || groupadd --system agentspool
id agentspool >/dev/null 2>&1 || useradd --system --gid agentspool --home-dir /var/lib/agentspool --shell /usr/sbin/nologin agentspool
install -m 0755 "$SOURCE" /usr/local/bin/.agentspool.installing
mv -f /usr/local/bin/.agentspool.installing /usr/local/bin/agentspool
install -d -o root -g root -m 0755 /etc/agentspool
install -d -o agentspool -g agentspool -m 0700 /var/lib/agentspool/worker
command -v bwrap >/dev/null || { echo "bwrap is required for persistent sandbox support" >&2; exit 1; }
command -v systemd-run >/dev/null || { echo "systemd-run is required for persistent sandbox support" >&2; exit 1; }
command -v setpriv >/dev/null || { echo "setpriv is required for persistent sandbox support" >&2; exit 1; }
command -v mkfs.ext4 >/dev/null || { echo "e2fsprogs is required for persistent sandbox disk quotas" >&2; exit 1; }
command -v e2fsck >/dev/null || { echo "e2fsprogs is required for persistent sandbox disk quotas" >&2; exit 1; }
command -v resize2fs >/dev/null || { echo "e2fsprogs is required for persistent sandbox disk quotas" >&2; exit 1; }
install -d -o root -g agentspool -m 0710 /var/lib/agentspool/sandboxes
install -m 0644 "$ROOT/deploy/systemd/agentspool-worker.service" /etc/systemd/system/agentspool-worker.service
install -m 0644 "$ROOT/deploy/systemd/agentspool-sandboxd.service" /etc/systemd/system/agentspool-sandboxd.service
install -d -m 0755 /usr/share/doc/agentspool
install -m 0644 "$ROOT/README.md" /usr/share/doc/agentspool/README.md

if [[ ! -e /etc/agentspool/worker.env ]]; then
  SERVER_URL="${AGENTSPOOL_SERVER:-}"
  JOIN_TOKEN="${AGENTSPOOL_JOIN_TOKEN:-}"
  JOIN_TOKEN_SOURCE="${AGENTSPOOL_JOIN_TOKEN_FILE:-}"
  JOIN_TOKEN_DESTINATION=/etc/agentspool/worker-join.token
  HOST_NAME="${AGENTSPOOL_WORKER_NAME:-$(hostname)}"
  LABELS="${AGENTSPOOL_WORKER_LABELS:-room=unknown}"
  QUEUES="${AGENTSPOOL_WORKER_QUEUES:-default}"
  DETECT_GPU="${AGENTSPOOL_WORKER_DETECT_GPU:-false}"
  ALLOWED_EXECUTORS="${AGENTSPOOL_WORKER_ALLOWED_EXECUTORS:-shell}"
  MAX_CONCURRENCY="${AGENTSPOOL_WORKER_MAX_CONCURRENCY:-2}"
  if [[ -n "$JOIN_TOKEN" ]]; then
    install -o root -g agentspool -m 0640 /dev/null "$JOIN_TOKEN_DESTINATION"
    printf '%s\n' "$JOIN_TOKEN" >"$JOIN_TOKEN_DESTINATION"
  elif [[ -n "$JOIN_TOKEN_SOURCE" ]]; then
    if [[ "$JOIN_TOKEN_SOURCE" == "$JOIN_TOKEN_DESTINATION" ]]; then
      chown root:agentspool "$JOIN_TOKEN_DESTINATION"
      chmod 0640 "$JOIN_TOKEN_DESTINATION"
    else
      install -o root -g agentspool -m 0640 "$JOIN_TOKEN_SOURCE" "$JOIN_TOKEN_DESTINATION"
    fi
  else
    install -o root -g agentspool -m 0640 /dev/null "$JOIN_TOKEN_DESTINATION"
    printf '%s\n' 'replace-with-control-plane-join-token' >"$JOIN_TOKEN_DESTINATION"
  fi
  cat >/etc/agentspool/worker.env <<EOF_ENV
AGENTSPOOL_SERVER=${SERVER_URL:-http://control-plane.example.invalid:8090}
AGENTSPOOL_JOIN_TOKEN_FILE=$JOIN_TOKEN_DESTINATION
AGENTSPOOL_WORKER_NAME=$HOST_NAME
AGENTSPOOL_WORKER_DATA_DIR=/var/lib/agentspool/worker
AGENTSPOOL_WORKER_LABELS=$LABELS
AGENTSPOOL_WORKER_QUEUES=$QUEUES
AGENTSPOOL_WORKER_DETECT_GPU=$DETECT_GPU
AGENTSPOOL_WORKER_ALLOWED_EXECUTORS=$ALLOWED_EXECUTORS
AGENTSPOOL_WORKER_MAX_CONCURRENCY=$MAX_CONCURRENCY
AGENTSPOOL_WORKER_ALLOW_NETWORK=false
AGENTSPOOL_WORKER_ALLOW_HOST_NETWORK=false
AGENTSPOOL_WORKER_INHERIT_HOST_ENV=false
AGENTSPOOL_SANDBOX_SOCKET=/run/agentspool/sandboxd.sock
AGENTSPOOL_LOG_FORMAT=json
AGENTSPOOL_LOG_LEVEL=info
EOF_ENV
  chmod 0600 /etc/agentspool/worker.env
else
  echo "Keeping existing /etc/agentspool/worker.env"
fi

# Upgrade older installs that kept the reusable Join Token inline. A Worker
# with durable node credentials no longer needs bootstrap authority. Otherwise
# migrate the same value to a protected file locator without rotating it.
WORKER_ENV=/etc/agentspool/worker.env
read_worker_env_value() {
  local key="$1" value
  value="$(sed -n "s/^${key}=//p" "$WORKER_ENV" 2>/dev/null | tail -n 1)"
  value="${value#\"}"; value="${value%\"}"
  value="${value#\'}"; value="${value%\'}"
  printf '%s' "$value"
}
WORKER_DATA_DIR="$(read_worker_env_value AGENTSPOOL_WORKER_DATA_DIR)"
WORKER_DATA_DIR="${WORKER_DATA_DIR:-/var/lib/agentspool/worker}"
WORKER_ENV_TMP="$(mktemp /etc/agentspool/.worker.env.XXXXXX)"
grep -Ev '^AGENTSPOOL_JOIN_TOKEN(_FILE)?=' "$WORKER_ENV" >"$WORKER_ENV_TMP" || true
if [[ -f "$WORKER_DATA_DIR/credentials.json" ]]; then
  rm -f /etc/agentspool/worker-join.token
  echo "Removed the no-longer-needed Worker Join Token after confirming durable node credentials."
else
  JOIN_TOKEN="$(read_worker_env_value AGENTSPOOL_JOIN_TOKEN)"
  JOIN_TOKEN_SOURCE="$(read_worker_env_value AGENTSPOOL_JOIN_TOKEN_FILE)"
  if [[ -z "$JOIN_TOKEN" && -n "$JOIN_TOKEN_SOURCE" ]]; then
    [[ -f "$JOIN_TOKEN_SOURCE" ]] || { echo "join token file not found: $JOIN_TOKEN_SOURCE" >&2; exit 1; }
    JOIN_TOKEN="$(<"$JOIN_TOKEN_SOURCE")"
  fi
  if [[ -z "$JOIN_TOKEN" && -f /etc/agentspool/worker-join.token ]]; then
    JOIN_TOKEN="$(</etc/agentspool/worker-join.token)"
  fi
  [[ ${#JOIN_TOKEN} -ge 24 && "$JOIN_TOKEN" != replace-with-* && "$JOIN_TOKEN" != *changeme* ]] || { echo "Worker Join Token is missing or too short" >&2; exit 1; }
  install -o root -g agentspool -m 0640 /dev/null /etc/agentspool/worker-join.token
  printf '%s\n' "$JOIN_TOKEN" >/etc/agentspool/worker-join.token
  printf '%s\n' 'AGENTSPOOL_JOIN_TOKEN_FILE=/etc/agentspool/worker-join.token' >>"$WORKER_ENV_TMP"
  unset JOIN_TOKEN JOIN_TOKEN_SOURCE
fi
install -o root -g root -m 0600 "$WORKER_ENV_TMP" "$WORKER_ENV"
rm -f "$WORKER_ENV_TMP"

if [[ "${AGENTSPOOL_ENABLE_DOCKER:-0}" == "1" ]]; then
  getent group docker >/dev/null || { echo "docker group does not exist" >&2; exit 1; }
  usermod -aG docker agentspool
  install -d -m 0755 /etc/systemd/system/agentspool-worker.service.d
  install -m 0644 "$ROOT/deploy/systemd/worker-docker.conf.example" /etc/systemd/system/agentspool-worker.service.d/docker.conf
fi

install -d -m 0755 /usr/lib/agentspool
install -m 0755 "$ROOT/scripts/sandbox-volume.py" /usr/lib/agentspool/sandbox-volume.py
prepare_service_data worker AGENTSPOOL_WORKER_DATA_DIR agentspool
# Keep helper transfers aligned with a configured non-default Worker directory.
python3 - "$WORKER_DATA_DIR" "$(read_worker_env_value AGENTSPOOL_SANDBOX_SOCKET)" <<'PY_ENV'
import json,sys
from pathlib import Path
path=Path('/etc/agentspool/sandbox.env')
lines=path.read_text().splitlines() if path.exists() else []
keys=('AGENTSPOOL_SANDBOX_TRANSFER_ROOT','AGENTSPOOL_SANDBOX_SOCKET')
lines=[line for line in lines if line.partition('=')[0] not in keys]
lines += [keys[0]+'='+json.dumps(sys.argv[1]+'/transfers'), keys[1]+'='+json.dumps(sys.argv[2] or '/run/agentspool/sandboxd.sock')]
path.write_text('\n'.join(lines)+'\n');path.chmod(0o600)
PY_ENV
install_finish "agentspool-sandboxd agentspool-worker"
