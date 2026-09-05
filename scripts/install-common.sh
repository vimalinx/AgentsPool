#!/usr/bin/env bash
# Shared by root installers; source only. Installation is one transaction because
# server, Worker and sandboxd use the same executable.
read_setting() {
  local file="$1" key="$2" value
  [[ -f "$file" ]] || return 0
  value="$(sed -n "s/^${key}=//p" "$file" 2>/dev/null | tail -n 1)"
  value="${value#\"}"; value="${value%\"}"; value="${value#\'}"; value="${value%\'}"
  printf '%s' "$value"
}
install_begin() {
  for dependency in systemctl curl python3 tar getent install flock timeout; do
    command -v "$dependency" >/dev/null || { echo "missing dependency: $dependency" >&2; exit 1; }
  done
  [[ -d /run/systemd/system ]] || { echo "systemd must be PID 1" >&2; exit 1; }
  exec 9>/run/agentspool-install.lock
  flock -n 9 || { echo "another install is in progress" >&2; exit 1; }
  "$SOURCE" version >/dev/null
  # Do not replace binaries under running sandbox processes or wait forever for them.
  if systemctl list-units --state=active,activating,deactivating --type=service --plain --no-legend 'agentspool-sbx-*.service' | grep -q 'agentspool-sbx-'; then
    echo "stop active sandboxes and drain Workers before upgrading" >&2; exit 1
  fi
  install -d -m 0700 /var/backups/agentspool-install
  INSTALL_BACKUP="$(mktemp -d /var/backups/agentspool-install/transaction-XXXXXXXX)"
  INSTALL_ACTIVE=()
  INSTALL_NEW=()
  INSTALL_ABSENT=()
  for candidate in /etc/agentspool /usr/local/bin/agentspool /usr/lib/agentspool /usr/share/doc/agentspool; do
    [[ -e "$candidate" ]] || INSTALL_ABSENT+=("$candidate")
  done
  for candidate in server worker sandboxd backup tls-check; do
    for suffix in service timer; do
      [[ -e "/etc/systemd/system/agentspool-$candidate.$suffix" ]] || INSTALL_ABSENT+=("/etc/systemd/system/agentspool-$candidate.$suffix")
    done
  done
  for unit in agentspool-server agentspool-sandboxd agentspool-worker; do
    if systemctl is-active --quiet "$unit"; then INSTALL_ACTIVE+=("$unit"); fi
  done
  # Preserve a forensic snapshot before mutation; credentials remain root-only.
  local paths=() candidate
  for candidate in etc/agentspool usr/local/bin/agentspool usr/lib/agentspool usr/share/doc/agentspool; do
    [[ ! -e "/$candidate" ]] || paths+=("$candidate")
  done
  while IFS= read -r candidate; do paths+=("${candidate#/}"); done < <(compgen -G '/etc/systemd/system/agentspool-*' || true)
  printf '%s\n' "${paths[@]}" >"$INSTALL_BACKUP/paths"
  tar -C / -cpf "$INSTALL_BACKUP/program-config.tar" --files-from "$INSTALL_BACKUP/paths"
  tar -tf "$INSTALL_BACKUP/program-config.tar" >"$INSTALL_BACKUP/inventory"
  INSTALL_PROCESS=$BASHPID
  trap 'install_on_error "$?"' ERR
  for unit in agentspool-worker agentspool-sandboxd agentspool-server; do
    if systemctl is-active --quiet "$unit"; then
      systemctl stop --no-block "$unit"
      local stopped=false
      for ((i=0;i<300;i++)); do
        if [[ "$(systemctl show "$unit" -p ActiveState --value)" =~ ^(inactive|failed)$ ]]; then stopped=true; break; fi
        sleep 0.1
      done
      [[ "$stopped" == true ]] || { echo "service did not stop within 30s: $unit" >&2; return 1; }
    fi
  done
  # State schemas can change at startup. Save stopped snapshots for rollback.
  local state_paths=() data
  for pair in 'server AGENTSPOOL_DATA_DIR' 'worker AGENTSPOOL_WORKER_DATA_DIR'; do
    read -r role key <<<"$pair"
    data="$(read_setting "/etc/agentspool/$role.env" "$key")"
    data="${data:-/var/lib/agentspool/$role}"
    [[ "$data" == /* && "$data" != / && ! -L "$data" ]] || { echo "unsafe data directory" >&2; return 1; }
    [[ ! -d "$data" ]] || state_paths+=("${data#/}")
  done
  tar -C / -cpf "$INSTALL_BACKUP/state.tar" "${state_paths[@]:---files-from=/dev/null}"
  echo "Installation backup: $INSTALL_BACKUP"
}
install_on_error() {
  # ERR is inherited by command substitutions; only the installer process owns
  # rollback. A probe failure in a subshell must propagate without mutating state.
  [[ "$BASHPID" == "$INSTALL_PROCESS" ]] || exit "$1"
  install_rollback "$1"
}
install_rollback() {
  local status="$1"
  trap - ERR
  set +e
  echo "Installation failed; restoring saved binary, configuration and state: $INSTALL_BACKUP" >&2
  for unit in agentspool-worker agentspool-sandboxd agentspool-server; do systemctl stop --no-block "$unit" 2>/dev/null; done
  # Rollback must never overwrite state that still has a live writer.
  for unit in agentspool-worker agentspool-sandboxd agentspool-server; do
    if [[ "$(systemctl show "$unit" -p LoadState --value)" != not-found ]]; then
      timeout 35s systemctl stop "$unit" 2>/dev/null || { echo "Rollback stopped: service is still running; backup retained." >&2; exit "$status"; }
    fi
  done
  rm -f /usr/local/bin/agentspool
  rm -rf /etc/agentspool /usr/lib/agentspool /usr/share/doc/agentspool
  rm -rf /etc/systemd/system/agentspool-{server,worker,sandboxd}.service.d
  for path in "${INSTALL_ABSENT[@]}"; do rm -rf -- "$path"; done
  for unit in "${INSTALL_NEW[@]}"; do systemctl disable "$unit" 2>/dev/null; done
  tar -C / -xpf "$INSTALL_BACKUP/program-config.tar"
  [[ ! -f "$INSTALL_BACKUP/state.tar" ]] || tar -C / -xpf "$INSTALL_BACKUP/state.tar"
  systemctl daemon-reload
  for unit in "${INSTALL_ACTIVE[@]}"; do systemctl start "$unit"; done
  exit "$status"
}
install_finish() {
  local requested="$1" unit pid expected actual
  systemctl daemon-reload
  # An upgrade preserves deliberately stopped services; a new installation starts
  # its requested services. Every previously active shared-binary service restarts.
  for unit in $requested; do
    if ! grep -qx "etc/systemd/system/$unit.service" "$INSTALL_BACKUP/inventory"; then
      INSTALL_NEW+=("$unit")
      systemctl enable "$unit"
    fi
  done
  for unit in agentspool-server agentspool-sandboxd agentspool-worker; do
    if [[ " ${INSTALL_ACTIVE[*]} ${INSTALL_NEW[*]} " == *" $unit "* ]]; then
      systemctl start "$unit"
      local ready=false
      for ((i=0;i<150;i++)); do
        pid="$(systemctl show "$unit" -p MainPID --value)"
        if [[ "$pid" =~ ^[1-9][0-9]*$ && -e "/proc/$pid/exe" ]]; then
          expected="$(stat -Lc '%d:%i' /usr/local/bin/agentspool)"
          actual="$(stat -Lc '%d:%i' "/proc/$pid/exe" 2>/dev/null || true)"
          if [[ "$expected" == "$actual" ]] && systemctl is-active --quiet "$unit"; then
            if [[ "$unit" == agentspool-sandboxd ]]; then
              local helper_socket
              helper_socket="$(read_setting /etc/agentspool/sandbox.env AGENTSPOOL_SANDBOX_SOCKET)"
              curl -fsS --unix-socket "${helper_socket:-/run/agentspool/sandboxd.sock}" http://localhost/v1/ping >/dev/null 2>&1 && ready=true
            elif [[ "$unit" == agentspool-server ]]; then
              python3 "$ROOT/scripts/install-ready.py" && ready=true
            else
              local worker_data
              worker_data="$(read_setting /etc/agentspool/worker.env AGENTSPOOL_WORKER_DATA_DIR)"
              [[ -s "${worker_data:-/var/lib/agentspool/worker}/credentials.json" ]] && ready=true
            fi
          fi
        fi
        [[ "$ready" != true ]] || break
        sleep 0.2
      done
      [[ "$ready" == true ]] || { echo "readiness/version verification failed: $unit" >&2; return 1; }
    fi
  done
  trap - ERR
  echo "Installed $(/usr/local/bin/agentspool version); running services use the installed executable."
}

prepare_service_data() {
  local role="$1" key="$2" account="$3" data
  data="$(read_setting "/etc/agentspool/$role.env" "$key")"
  data="${data:-/var/lib/agentspool/$role}"
  [[ "$data" == /* && "$data" != / && ! -L "$data" && "$data" != *$'\n'* ]] || { echo "unsafe service data path" >&2; return 1; }
  install -d -o "$account" -g "$account" -m 0700 "$data"
  chown -R "$account:$account" "$data"
  if [[ "$data" != "/var/lib/agentspool/$role" ]]; then
    install -d -m 0755 "/etc/systemd/system/agentspool-$role.service.d"
    python3 - "$role" "$data" <<'PY'
import json,sys
from pathlib import Path
role,data=sys.argv[1:]
quote=json.dumps(data.replace('%','%%'))
# BindPaths exposes only the configured state directory when ProtectHome hides
# its ancestors. The rest of the original service hardening remains active.
Path(f'/etc/systemd/system/agentspool-{role}.service.d/data-directory.conf').write_text(
    '[Service]\nReadWritePaths=\nReadWritePaths='+quote+'\nBindPaths='+quote+'\n')
PY
  else
    rm -f "/etc/systemd/system/agentspool-$role.service.d/data-directory.conf"
  fi
}
