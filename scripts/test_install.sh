#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL="$ROOT/scripts/install.sh"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

asset() {
    PAIRROOM_TEST_OS="$1" PAIRROOM_TEST_ARCH="$2" PAIRROOM_VERSION="$3" \
        bash "$INSTALL" --print-asset
}

got="$(asset Linux x86_64 v1.2.0)"
[[ "$got" == pairroom-cli-v1.2.0-linux-amd64 ]] || fail "linux amd64 -> $got"

got="$(asset Darwin arm64 1.2.0)"
[[ "$got" == pairroom-cli-v1.2.0-darwin-arm64 ]] || fail "darwin arm64 -> $got"

got="$(asset Darwin x86_64 v1.2.0)"
[[ "$got" == pairroom-cli-v1.2.0-darwin-amd64 ]] || fail "darwin amd64 -> $got"

got="$(asset MINGW64_NT-10.0 x86_64 v1.2.0)"
[[ "$got" == pairroom-cli-v1.2.0-windows-amd64.exe ]] || fail "windows amd64 -> $got"

printf 'install.sh asset-name tests passed\n'
