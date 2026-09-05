#!/usr/bin/env bash
set -Eeuo pipefail

[[ ${EUID:-$(id -u)} -eq 0 ]] || { echo "run as root" >&2; exit 1; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE="${1:-$ROOT/agentspool}"
[[ -x "$SOURCE" || $# -gt 0 ]] || SOURCE="$ROOT/dist/agentspool"

source "$ROOT/scripts/install-common.sh"
install_begin
[[ -x "$SOURCE" ]] || { echo "binary not found or not executable: $SOURCE" >&2; exit 1; }

# 0.4 no longer owns Agent runtimes. Stop and remove any system-level takeover
# service left by 0.2/0.3 before installing the runtime-neutral control plane.
systemctl disable --now agentspool-agentd.service 2>/dev/null || true
rm -f /etc/systemd/system/agentspool-agentd.service
rm -f /usr/local/bin/agentspool-agentd /usr/local/bin/agentspool-agentctl /usr/local/bin/agentspool-bridge

getent group agentspool-server >/dev/null || groupadd --system agentspool-server
id agentspool-server >/dev/null 2>&1 || useradd --system --gid agentspool-server --home-dir /var/lib/agentspool/server --shell /usr/sbin/nologin agentspool-server
install -m 0755 "$SOURCE" /usr/local/bin/.agentspool.installing
mv -f /usr/local/bin/.agentspool.installing /usr/local/bin/agentspool
install -d -o root -g root -m 0755 /etc/agentspool
install -d -o agentspool-server -g agentspool-server -m 0700 /var/lib/agentspool/server
chown -R agentspool-server:agentspool-server /var/lib/agentspool/server
install -m 0644 "$ROOT/deploy/systemd/agentspool-server.service" /etc/systemd/system/agentspool-server.service
install -d -o root -g root -m 0755 /usr/lib/agentspool
install -m 0755 "$ROOT/scripts/restore-drill.sh" /usr/lib/agentspool/restore-drill.sh
install -m 0755 "$ROOT/scripts/backup-server.sh" /usr/lib/agentspool/backup-server.sh
install -m 0755 "$ROOT/scripts/check-tls-expiry.sh" /usr/lib/agentspool/check-tls-expiry.sh
install -m 0644 "$ROOT/deploy/systemd/agentspool-backup.service" /etc/systemd/system/agentspool-backup.service
install -m 0644 "$ROOT/deploy/systemd/agentspool-backup.timer" /etc/systemd/system/agentspool-backup.timer
install -m 0644 "$ROOT/deploy/systemd/agentspool-tls-check.service" /etc/systemd/system/agentspool-tls-check.service
install -m 0644 "$ROOT/deploy/systemd/agentspool-tls-check.timer" /etc/systemd/system/agentspool-tls-check.timer
install -d -m 0755 /usr/share/doc/agentspool
install -m 0644 "$ROOT/README.md" /usr/share/doc/agentspool/README.md

SERVER_ENV=/etc/agentspool/server.env
read_env_value() {
  local key="$1" value
  value="$(sed -n "s/^${key}=//p" "$SERVER_ENV" 2>/dev/null | tail -n 1)"
  value="${value#\"}"; value="${value%\"}"
  value="${value#\'}"; value="${value%\'}"
  printf '%s' "$value"
}
prepare_token_file() {
  local name="$1" destination="$2" raw locator value
  raw="$(read_env_value "AGENTSPOOL_${name}_TOKEN")"
  locator="$(read_env_value "AGENTSPOOL_${name}_TOKEN_FILE")"
  if [[ -n "$raw" && -n "$locator" ]]; then
    echo "server.env configures both AGENTSPOOL_${name}_TOKEN and AGENTSPOOL_${name}_TOKEN_FILE" >&2
    exit 1
  fi
  if [[ -n "$raw" ]]; then
    value="$raw"
  elif [[ -n "$locator" && "$locator" != "$destination" ]]; then
    [[ -f "$locator" ]] || { echo "token file not found: $locator" >&2; exit 1; }
    value="$(<"$locator")"
  elif [[ -f "$destination" ]]; then
    value="$(<"$destination")"
  else
    value="$(/usr/local/bin/agentspool token)"
  fi
  [[ ${#value} -ge 24 && "$value" != replace-with-* && "$value" != *changeme* ]] || { echo "AGENTSPOOL_${name}_TOKEN is too short" >&2; exit 1; }
  install -o root -g agentspool-server -m 0640 /dev/null "$destination"
  printf '%s\n' "$value" >"$destination"
  unset value raw locator
}

if [[ ! -e "$SERVER_ENV" ]]; then
  cat >"$SERVER_ENV" <<EOF_ENV
AGENTSPOOL_LISTEN=${AGENTSPOOL_LISTEN:-127.0.0.1:8090}
AGENTSPOOL_DATA_DIR=/var/lib/agentspool/server
AGENTSPOOL_LEASE_TTL=45s
AGENTSPOOL_NODE_TIMEOUT=35s
AGENTSPOOL_MAX_REQUEST_BYTES=0
AGENTSPOOL_MAX_ARTIFACT_BYTES=0
AGENTSPOOL_EVENT_RETENTION=0
AGENTSPOOL_LOG_FORMAT=json
AGENTSPOOL_LOG_LEVEL=info
EOF_ENV
fi

prepare_token_file ADMIN /etc/agentspool/admin.token
prepare_token_file ALLOCATOR /etc/agentspool/allocator.token
prepare_token_file JOIN /etc/agentspool/join.token

SERVER_ENV_TMP="$(mktemp /etc/agentspool/.server.env.XXXXXX)"
grep -Ev '^AGENTSPOOL_(ADMIN|ALLOCATOR|JOIN)_TOKEN(_FILE)?=' "$SERVER_ENV" >"$SERVER_ENV_TMP" || true
cat >>"$SERVER_ENV_TMP" <<'EOF_ENV'
AGENTSPOOL_ADMIN_TOKEN_FILE=/etc/agentspool/admin.token
AGENTSPOOL_ALLOCATOR_TOKEN_FILE=/etc/agentspool/allocator.token
AGENTSPOOL_JOIN_TOKEN_FILE=/etc/agentspool/join.token
EOF_ENV
install -o root -g root -m 0600 "$SERVER_ENV_TMP" "$SERVER_ENV"
rm -f "$SERVER_ENV_TMP"
echo "Protected credential files and locators are configured in /etc/agentspool."
echo "Tokens are intentionally not printed. Existing inline credentials were migrated without changing their values."

systemctl daemon-reload
prepare_service_data server AGENTSPOOL_DATA_DIR agentspool-server
install_finish "agentspool-server"
systemctl enable --now agentspool-tls-check.timer
if [[ -f /etc/agentspool/backup.env ]]; then
  systemctl enable --now agentspool-backup.timer
fi
echo "AgentsPool control plane installed with a loopback-only default."
echo "For LAN workers, set AGENTSPOOL_LISTEN to an explicit private address in /etc/agentspool/server.env."
