#!/usr/bin/env bash
# Exercise scripts/install.sh under every available POSIX shell (bash, dash,
# busybox sh, and /bin/sh), because the documented one-liner pipes it to `sh`.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL="$ROOT/scripts/install.sh"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

shells=("bash")
command -v dash >/dev/null 2>&1 && shells+=("dash")
command -v busybox >/dev/null 2>&1 && shells+=("busybox sh")
command -v sh >/dev/null 2>&1 && shells+=("sh")
if [[ -n "${PAIRROOM_REQUIRE_DASH:-}" ]] && ! command -v dash >/dev/null 2>&1; then
    fail "PAIRROOM_REQUIRE_DASH is set but dash is not installed"
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# A fake curl on PATH serves fixture files keyed by URL and records each URL,
# so the full download/verify/install path runs without network access.
mkdir -p "$work/bin" "$work/fixtures"
cat >"$work/bin/curl" <<'EOF'
#!/bin/sh
out=""
url=""
while [ $# -gt 0 ]; do
    case "$1" in
        -o) out="$2"; shift 2 ;;
        -*) shift ;;
        *) url="$1"; shift ;;
    esac
done
printf '%s\n' "$url" >>"$FAKE_CURL_LOG"
key="$(printf '%s' "$url" | sed 's#[^A-Za-z0-9._-]#_#g')"
[ -f "$FAKE_CURL_DIR/$key" ] || exit 22
if [ -n "$out" ]; then
    cat "$FAKE_CURL_DIR/$key" >"$out"
else
    cat "$FAKE_CURL_DIR/$key"
fi
EOF
chmod +x "$work/bin/curl"

fixture() {
    local key
    key="$(printf '%s' "$1" | sed 's#[^A-Za-z0-9._-]#_#g')"
    cat >"$work/fixtures/$key"
}

sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{ print $1 }'
    else
        shasum -a 256 "$1" | awk '{ print $1 }'
    fi
}

asset() {
    local shell="$1"
    shift
    # shellcheck disable=SC2086 # "busybox sh" is intentionally split.
    PAIRROOM_TEST_OS="$1" PAIRROOM_TEST_ARCH="$2" PAIRROOM_VERSION="$3" \
        $shell "$INSTALL" --print-asset
}

run_install() {
    local shell="$1" prefix="$2"
    shift 2
    : >"$work/curl.log"
    # shellcheck disable=SC2086
    env PATH="$work/bin:$PATH" FAKE_CURL_DIR="$work/fixtures" FAKE_CURL_LOG="$work/curl.log" \
        PAIRROOM_TEST_OS=Linux PAIRROOM_TEST_ARCH=x86_64 PREFIX="$prefix" "$@" \
        $shell "$INSTALL"
}

# Latest release v9.8.7 of the default repository, with a matching checksum.
# A shebang lets MSYS/Cygwin report the installed file as executable.
printf '#!/bin/sh\necho fake pairroom\n' >"$work/good-binary"
fixture "https://api.github.com/repos/sean2077/pairroom/releases/latest" <<'EOF'
{
  "url": "https://api.github.com/repos/sean2077/pairroom/releases/1",
  "tag_name": "v9.8.7",
  "name": "v9.8.7"
}
EOF
fixture "https://github.com/sean2077/pairroom/releases/download/v9.8.7/pairroom-cli-v9.8.7-linux-amd64" <"$work/good-binary"
printf '%s  pairroom-cli-v9.8.7-linux-amd64\n' "$(sha256 "$work/good-binary")" |
    fixture "https://github.com/sean2077/pairroom/releases/download/v9.8.7/SHA256SUMS"

# A fork release whose checksum does not match the downloaded bytes.
fixture "https://github.com/example/fork/releases/download/v1.0.0/pairroom-cli-v1.0.0-linux-amd64" <"$work/good-binary"
printf '%064d  pairroom-cli-v1.0.0-linux-amd64\n' 0 |
    fixture "https://github.com/example/fork/releases/download/v1.0.0/SHA256SUMS"

for shell in "${shells[@]}"; do
    got="$(asset "$shell" Linux x86_64 v1.2.0)"
    [[ "$got" == pairroom-cli-v1.2.0-linux-amd64 ]] || fail "[$shell] linux amd64 -> $got"

    got="$(asset "$shell" Darwin arm64 1.2.0)"
    [[ "$got" == pairroom-cli-v1.2.0-darwin-arm64 ]] || fail "[$shell] darwin arm64 -> $got"

    got="$(asset "$shell" Darwin x86_64 v1.2.0)"
    [[ "$got" == pairroom-cli-v1.2.0-darwin-amd64 ]] || fail "[$shell] darwin amd64 -> $got"

    got="$(asset "$shell" MINGW64_NT-10.0 x86_64 v1.2.0)"
    [[ "$got" == pairroom-cli-v1.2.0-windows-amd64.exe ]] || fail "[$shell] windows amd64 -> $got"

    if asset "$shell" Linux aarch64 v1.2.0 >/dev/null 2>&1; then
        fail "[$shell] linux arm64 must be rejected"
    fi

    # Latest-tag resolution, checksum verification, and installation. The
    # GitHub Actions GITHUB_REPOSITORY of a caller must not redirect the source.
    prefix="$work/prefix-${shell// /-}"
    run_install "$shell" "$prefix" GITHUB_REPOSITORY=someone/else >"$work/out" 2>&1 ||
        fail "[$shell] install failed: $(cat "$work/out")"
    cmp -s "$prefix/bin/pairroom" "$work/good-binary" || fail "[$shell] installed binary differs"
    [[ -x "$prefix/bin/pairroom" ]] || fail "[$shell] installed binary is not executable"
    grep -q 'Installed PairRoom CLI v9.8.7' "$work/out" || fail "[$shell] missing success line: $(cat "$work/out")"
    if grep -q 'someone/else' "$work/curl.log"; then
        fail "[$shell] GITHUB_REPOSITORY leaked into download URLs"
    fi

    # PAIRROOM_REPOSITORY selects a fork, and a checksum mismatch installs nothing.
    prefix="$work/fork-${shell// /-}"
    if run_install "$shell" "$prefix" PAIRROOM_REPOSITORY=example/fork PAIRROOM_VERSION=1.0.0 >"$work/out" 2>&1; then
        fail "[$shell] checksum mismatch must fail"
    fi
    grep -q 'checksum mismatch' "$work/out" || fail "[$shell] unexpected mismatch output: $(cat "$work/out")"
    [[ ! -e "$prefix/bin/pairroom" ]] || fail "[$shell] mismatched binary was installed"
    grep -q '^https://github.com/example/fork/' "$work/curl.log" || fail "[$shell] PAIRROOM_REPOSITORY ignored"

    # An unreachable release API fails with the resolution message.
    prefix="$work/offline-${shell// /-}"
    if run_install "$shell" "$prefix" PAIRROOM_REPOSITORY=missing/repo >"$work/out" 2>&1; then
        fail "[$shell] unresolved latest tag must fail"
    fi
    grep -q 'could not resolve the latest PairRoom release tag' "$work/out" ||
        fail "[$shell] unexpected resolution output: $(cat "$work/out")"

    printf 'install.sh tests passed under %s\n' "$shell"
done
