#!/usr/bin/env bash
set -Eeuo pipefail

[[ ${EUID:-$(id -u)} -eq 0 ]] || { echo "run as root" >&2; exit 1; }

RECIPIENT_FILE="${AGENTSPOOL_BACKUP_RECIPIENT_FILE:-}"
DESTINATION="${AGENTSPOOL_BACKUP_DESTINATION:-}"
DATA_ROOT="${AGENTSPOOL_BACKUP_DATA_ROOT:-/var/lib/agentspool}"
SERVICE="${AGENTSPOOL_BACKUP_SERVICE:-agentspool-server.service}"

while (($#)); do
  case "$1" in
    --recipient-file) RECIPIENT_FILE="$2"; shift 2 ;;
    --destination) DESTINATION="$2"; shift 2 ;;
    --data-root) DATA_ROOT="$2"; shift 2 ;;
    --service) SERVICE="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[[ -f "$RECIPIENT_FILE" ]] || { echo "age recipient file not found" >&2; exit 1; }
[[ "$DESTINATION" == *:* ]] || { echo "destination must be SSH_TARGET:/absolute/path" >&2; exit 1; }
REMOTE_TARGET="${DESTINATION%%:*}"
REMOTE_DIR="${DESTINATION#*:}"
[[ "$REMOTE_TARGET" =~ ^([A-Za-z0-9._-]+@)?[A-Za-z0-9._-]+$ ]] || { echo "invalid SSH target" >&2; exit 1; }
[[ "$REMOTE_DIR" =~ ^/[A-Za-z0-9._/-]+$ && "$REMOTE_DIR" != "/" ]] || { echo "invalid remote backup directory" >&2; exit 1; }
[[ -d "$DATA_ROOT/server" ]] || { echo "server data directory not found" >&2; exit 1; }
command -v age >/dev/null || { echo "age is required" >&2; exit 1; }
command -v ssh >/dev/null || { echo "ssh is required" >&2; exit 1; }
command -v scp >/dev/null || { echo "scp is required" >&2; exit 1; }

WORK_DIR="$(mktemp -d /tmp/agentspool-backup.XXXXXX)"
SERVER_WAS_ACTIVE=false
SERVER_STOPPED=false
cleanup() {
  if [[ "$SERVER_STOPPED" == true && "$SERVER_WAS_ACTIVE" == true ]]; then
    systemctl start "$SERVICE" || true
  fi
  rm -rf -- "$WORK_DIR"
}
trap cleanup EXIT

if systemctl is-active --quiet "$SERVICE"; then
  SERVER_WAS_ACTIVE=true
  systemctl stop "$SERVICE"
  SERVER_STOPPED=true
fi

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
HOST_LABEL="$(hostname | tr -cd 'A-Za-z0-9._-' | cut -c1-80)"
ARCHIVE_NAME="agentspool-${HOST_LABEL}-${STAMP}.tar.gz"
PLAIN_ARCHIVE="$WORK_DIR/$ARCHIVE_NAME"
ENCRYPTED_ARCHIVE="$PLAIN_ARCHIVE.age"

tar --xattrs --acls --numeric-owner -C "$DATA_ROOT" -czf "$PLAIN_ARCHIVE" server
if [[ "$SERVER_WAS_ACTIVE" == true ]]; then
  systemctl start "$SERVICE"
  SERVER_STOPPED=false
fi

age --encrypt --recipients-file "$RECIPIENT_FILE" --output "$ENCRYPTED_ARCHIVE" "$PLAIN_ARCHIVE"
rm -f -- "$PLAIN_ARCHIVE"
chmod 0600 "$ENCRYPTED_ARCHIVE"
(
  cd "$WORK_DIR"
  sha256sum "$(basename "$ENCRYPTED_ARCHIVE")" >"$(basename "$ENCRYPTED_ARCHIVE").sha256"
)

ssh -o BatchMode=yes "$REMOTE_TARGET" "install -d -m 0700 '$REMOTE_DIR'"
scp -q "$ENCRYPTED_ARCHIVE" "$ENCRYPTED_ARCHIVE.sha256" "$REMOTE_TARGET:$REMOTE_DIR/"
ssh -o BatchMode=yes "$REMOTE_TARGET" "cd '$REMOTE_DIR' && sha256sum -c '$(basename "$ENCRYPTED_ARCHIVE").sha256'"

echo "encrypted off-host backup verified: $DESTINATION/$(basename "$ENCRYPTED_ARCHIVE")"
