#!/usr/bin/env bash
set -Eeuo pipefail

CERT_FILE="${AGENTSPOOL_TLS_CERT_FILE:-}"
WARN_DAYS="${AGENTSPOOL_TLS_WARN_DAYS:-30}"
SERVER_ENV="${AGENTSPOOL_SERVER_ENV:-/etc/agentspool/server.env}"

if [[ -z "$CERT_FILE" && -f "$SERVER_ENV" ]]; then
  CERT_FILE="$(sed -n 's/^AGENTSPOOL_TLS_CERT=//p' "$SERVER_ENV" | tail -n 1)"
  CERT_FILE="${CERT_FILE#\"}"; CERT_FILE="${CERT_FILE%\"}"
  CERT_FILE="${CERT_FILE#\'}"; CERT_FILE="${CERT_FILE%\'}"
fi
if [[ -z "$CERT_FILE" ]]; then
  echo "TLS certificate is not configured; expiry check skipped"
  exit 0
fi
[[ -f "$CERT_FILE" ]] || { echo "TLS certificate file is missing: $CERT_FILE" >&2; exit 1; }
[[ "$WARN_DAYS" =~ ^[0-9]+$ ]] || { echo "AGENTSPOOL_TLS_WARN_DAYS must be a non-negative integer" >&2; exit 1; }
command -v openssl >/dev/null || { echo "openssl is required" >&2; exit 1; }

SECONDS_AHEAD=$((WARN_DAYS * 86400))
if ! openssl x509 -in "$CERT_FILE" -noout -checkend "$SECONDS_AHEAD" >/dev/null; then
  openssl x509 -in "$CERT_FILE" -noout -subject -issuer -enddate -fingerprint -sha256
  echo "TLS certificate expires within $WARN_DAYS days" >&2
  exit 1
fi
openssl x509 -in "$CERT_FILE" -noout -subject -issuer -enddate -fingerprint -sha256
echo "TLS certificate expiry check passed"
