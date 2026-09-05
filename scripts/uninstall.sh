#!/usr/bin/env bash
set -Eeuo pipefail
[[ ${EUID:-$(id -u)} -eq 0 ]] || { echo "run as root" >&2; exit 1; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT/scripts/install-common.sh"
SANDBOX_DATA="$(read_setting /etc/agentspool/sandbox.env AGENTSPOOL_SANDBOX_DATA_DIR)"
SANDBOX_DATA="${SANDBOX_DATA:-/var/lib/agentspool/sandboxes}"
[[ "$SANDBOX_DATA" == /* && "$SANDBOX_DATA" != / ]] || { echo "unsafe sandbox data directory" >&2; exit 1; }
PURGE=0
[[ "${1:-}" == "--purge" ]] && PURGE=1
mapfile -t sandbox_units < <(systemctl list-units --all --type=service --plain --no-legend 'agentspool-sbx-*.service' 2>/dev/null | awk '$1 ~ /^agentspool-sbx-[A-Za-z0-9_-]+\.service$/ {print $1}')
for unit in "${sandbox_units[@]}"; do
  systemctl stop "$unit"
done
for unit in agentspool-backup.timer agentspool-tls-check.timer agentspool-backup.service agentspool-tls-check.service agentspool-worker agentspool-server agentspool-sandboxd agentspool-agentd; do
  systemctl disable --now "$unit" 2>/dev/null || true
done
if command -v findmnt >/dev/null && command -v umount >/dev/null; then
  mapfile -t sandbox_mounts < <(findmnt -rn -o TARGET | awk -v root="$SANDBOX_DATA" 'index($0,root "/sbx_")==1 && substr($0,length(root)+1) ~ /^\/sbx_[A-Za-z0-9_-]+\/rootfs$/')
  for ((index=${#sandbox_mounts[@]}-1; index>=0; index--)); do
    umount -- "${sandbox_mounts[$index]}"
  done
fi
rm -f /etc/systemd/system/agentspool-worker.service /etc/systemd/system/agentspool-server.service /etc/systemd/system/agentspool-sandboxd.service /etc/systemd/system/agentspool-agentd.service
rm -rf /etc/systemd/system/agentspool-worker.service.d /etc/systemd/system/agentspool-server.service.d /etc/systemd/system/agentspool-sandboxd.service.d
rm -f /usr/local/bin/agentspool /usr/local/bin/agentspool-agentd /usr/local/bin/agentspool-agentctl /usr/local/bin/agentspool-bridge
rm -f /etc/systemd/system/agentspool-backup.{service,timer} /etc/systemd/system/agentspool-tls-check.{service,timer}
rm -rf /usr/lib/agentspool
systemctl daemon-reload
if [[ $PURGE -eq 1 ]]; then
  rm -rf /etc/agentspool /var/lib/agentspool /usr/share/doc/agentspool
  userdel agentspool 2>/dev/null || true
  groupdel agentspool 2>/dev/null || true
  userdel agentspool-server 2>/dev/null || true
  groupdel agentspool-server 2>/dev/null || true
  echo "AgentsPool and all local data purged."
else
  echo "Program removed; /etc/agentspool and /var/lib/agentspool were preserved. Use --purge to delete data."
fi
