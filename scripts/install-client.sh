#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE="${1:-$ROOT/agentspool}"
[[ -x "$SOURCE" || $# -gt 0 ]] || SOURCE="$ROOT/dist/agentspool"
SERVER="${2:-}"
ALLOCATOR_TOKEN_FILE="${3:-}"

[[ -x "$SOURCE" ]] || { echo "binary not found or not executable: $SOURCE" >&2; exit 1; }
[[ -f "$ROOT/.agents/skills/agentspool-compute/SKILL.md" ]] || { echo "agentspool-compute Skill is missing" >&2; exit 1; }

if [[ -n "$SERVER" || -n "$ALLOCATOR_TOKEN_FILE" ]]; then
  [[ -n "$SERVER" && -f "$ALLOCATOR_TOKEN_FILE" ]] || {
    echo "provide both SERVER and an existing ALLOCATOR_TOKEN_FILE, or neither" >&2; exit 1;
  }
fi

BIN_DIR="${XDG_BIN_HOME:-${HOME:?}/.local/bin}"
SKILL_DIR="${AGENTSPOOL_SHARED_SKILLS_DIR:-${HOME:?}/.agents/skills}/agentspool-compute"
install -d -m 0755 "$BIN_DIR" "$SKILL_DIR"
install -m 0755 "$SOURCE" "$BIN_DIR/agentspool"
install -m 0644 "$ROOT/.agents/skills/agentspool-compute/SKILL.md" "$SKILL_DIR/SKILL.md"

if [[ -n "$SERVER" || -n "$ALLOCATOR_TOKEN_FILE" ]]; then
  [[ -n "$SERVER" && -n "$ALLOCATOR_TOKEN_FILE" ]] || {
    echo "provide both SERVER and ALLOCATOR_TOKEN_FILE, or neither" >&2
    exit 1
  }
  "$BIN_DIR/agentspool" configure --server "$SERVER" --allocator-token-file "$ALLOCATOR_TOKEN_FILE"
fi

echo "Installed runtime-neutral CLI: $BIN_DIR/agentspool"
echo "Installed shared Skill: $SKILL_DIR/SKILL.md"
