#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
VERSION="$(awk -F'"' '/const Version/{print $2}' internal/config/config.go)"
WORK_DIR="$(mktemp -d /tmp/agentspool-release-verify.XXXXXX)"

cleanup() {
  case "$WORK_DIR" in
    /tmp/agentspool-release-verify.*) rm -rf -- "$WORK_DIR" ;;
    *) echo "refusing to clean unexpected release verification path: $WORK_DIR" >&2 ;;
  esac
}
trap cleanup EXIT

release_checksums() {
  local destination="$1"
  (
    cd "$ROOT/dist/release"
    sha256sum ./* | LC_ALL=C sort -k2
  ) >"$destination"
}

echo "building release candidate twice"
AGENTSPOOL_ALLOW_DIRTY_RELEASE=1 ./scripts/build-release.sh
release_checksums "$WORK_DIR/first.sha256"
AGENTSPOOL_ALLOW_DIRTY_RELEASE=1 ./scripts/build-release.sh
release_checksums "$WORK_DIR/second.sha256"
diff -u "$WORK_DIR/first.sha256" "$WORK_DIR/second.sha256"

echo "verifying binary archive default client installation"
mkdir -p "$WORK_DIR/binary"
tar -xzf "$ROOT/dist/release/AgentsPool-v${VERSION}-linux-amd64.tar.gz" -C "$WORK_DIR/binary"
BINARY_ROOT="$WORK_DIR/binary/AgentsPool-v${VERSION}-linux-amd64"
XDG_BIN_HOME="$WORK_DIR/client/bin" AGENTSPOOL_SHARED_SKILLS_DIR="$WORK_DIR/client/skills" \
  bash "$BINARY_ROOT/scripts/install-client.sh"
test "$("$WORK_DIR/client/bin/agentspool" version)" = "$VERSION"
test -f "$WORK_DIR/client/skills/agentspool-compute/SKILL.md"
if [[ "${AGENTSPOOL_VERIFY_DOCKER:-0}" == 1 ]]; then
  docker build -t agentspool:binary-release-verification "$BINARY_ROOT"
  test "$(docker run --rm agentspool:binary-release-verification version)" = "$VERSION"
fi

echo "verifying source archive without Git metadata"
mkdir -p "$WORK_DIR/source"
unzip -q "$ROOT/dist/release/AgentsPool-v${VERSION}-source.zip" -d "$WORK_DIR/source"
SOURCE_ROOT="$WORK_DIR/source/AgentsPool-v${VERSION}"
test -d "$SOURCE_ROOT"
test ! -e "$SOURCE_ROOT/.git"
make -C "$SOURCE_ROOT" check build release
(
  cd "$SOURCE_ROOT/dist/release"
  sha256sum -c SHA256SUMS.txt
)

echo "release reproducibility and standalone source verification passed"
