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
    printf '%s\n' "$out" >>"$FAKE_CURL_OUTPUT_LOG"
    cat "$FAKE_CURL_DIR/$key" >"$out"
else
    cat "$FAKE_CURL_DIR/$key"
fi
EOF
chmod +x "$work/bin/curl"

# Faults are injected at ordinary process boundaries, not production-only
# hooks. Record temp paths so cleanup is checked even before any HTTP call.
real_mktemp="$(command -v mktemp)"
real_mv="$(command -v mv)"
cat >"$work/bin/mktemp" <<'EOF'
#!/bin/sh
count="$(cat "$FAKE_MKTEMP_COUNT")"
count=$((count + 1))
printf '%s\n' "$count" >"$FAKE_MKTEMP_COUNT"
if [ "$count" = "${FAKE_MKTEMP_FAIL_AT:-}" ]; then
    printf 'injected mktemp failure\n' >&2
    exit 1
fi
path="$("$REAL_MKTEMP" "$@")" || exit 1
printf '%s\n' "$path" >>"$FAKE_MKTEMP_LOG"
printf '%s\n' "$path"
EOF
cat >"$work/bin/mv" <<'EOF'
#!/bin/sh
if [ "${FAKE_MV_FAIL:-}" = 1 ]; then
    printf 'injected replacement failure\n' >&2
    exit 1
fi
exec "$REAL_MV" "$@"
EOF
chmod +x "$work/bin/mktemp" "$work/bin/mv"

assert_temps_removed() {
    local path
    while IFS= read -r path; do
        [[ ! -e "$path" ]] || fail "temporary file leaked: $path"
    done <"$work/mktemp.log"
}

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
    : >"$work/curl-output.log"
    : >"$work/mktemp.log"
    printf '0\n' >"$work/mktemp-count"
    # shellcheck disable=SC2086
    env PATH="$work/bin:$PATH" FAKE_CURL_DIR="$work/fixtures" FAKE_CURL_LOG="$work/curl.log" \
        FAKE_CURL_OUTPUT_LOG="$work/curl-output.log" \
        REAL_MKTEMP="$real_mktemp" REAL_MV="$real_mv" FAKE_MKTEMP_LOG="$work/mktemp.log" \
        FAKE_MKTEMP_COUNT="$work/mktemp-count" \
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
    # Compare digests rather than depend on cmp, which minimal MSYS2 installs
    # lack (a PATH fallback to another MSYS runtime cannot see this /tmp).
    [[ "$(sha256 "$prefix/bin/pairroom")" == "$(sha256 "$work/good-binary")" ]] ||
        fail "[$shell] installed binary differs"
    [[ -x "$prefix/bin/pairroom" ]] || fail "[$shell] installed binary is not executable"
    grep -q 'Installed PairRoom CLI v9.8.7' "$work/out" || fail "[$shell] missing success line: $(cat "$work/out")"
    if grep -q 'someone/else' "$work/curl.log"; then
        fail "[$shell] GITHUB_REPOSITORY leaked into download URLs"
    fi

    # Staging must be on the destination filesystem: mv from /tmp may turn
    # into a copy/unlink and truncate a previously installed CLI on failure.
    staged="$(head -n 1 "$work/curl-output.log")"
    [[ "$(dirname "$staged")" == "$prefix/bin" ]] ||
        fail "[$shell] binary was not staged beside its destination: $staged"
    [[ ! -e "$staged" ]] || fail "[$shell] staging file was not removed"
    assert_temps_removed

    # PAIRROOM_REPOSITORY selects a fork, and a checksum mismatch installs nothing.
    prefix="$work/fork-${shell// /-}"
    if run_install "$shell" "$prefix" PAIRROOM_REPOSITORY=example/fork PAIRROOM_VERSION=1.0.0 >"$work/out" 2>&1; then
        fail "[$shell] checksum mismatch must fail"
    fi
    grep -q 'checksum mismatch' "$work/out" || fail "[$shell] unexpected mismatch output: $(cat "$work/out")"
    [[ ! -e "$prefix/bin/pairroom" ]] || fail "[$shell] mismatched binary was installed"
    grep -q '^https://github.com/example/fork/' "$work/curl.log" || fail "[$shell] PAIRROOM_REPOSITORY ignored"

    # Failed upgrades keep the previously installed CLI, emit no success,
    # and remove every allocated temporary file, including partial setup.
    prefix="$work/existing CLI-${shell// /-}"
    mkdir -p "$prefix/bin"
    printf '#!/bin/sh\necho previous pairroom\n' >"$prefix/bin/pairroom"
    chmod +x "$prefix/bin/pairroom"
    old_digest="$(sha256 "$prefix/bin/pairroom")"
    for fault in checksum allocate replace; do
        case "$fault" in
            checksum) faults=(PAIRROOM_REPOSITORY=example/fork PAIRROOM_VERSION=1.0.0); reason='checksum mismatch' ;;
            allocate) faults=(FAKE_MKTEMP_FAIL_AT=2); reason='injected mktemp failure' ;;
            replace) faults=(FAKE_MV_FAIL=1); reason='injected replacement failure' ;;
        esac
        if run_install "$shell" "$prefix" "${faults[@]}" >"$work/out" 2>&1; then
            fail "[$shell] $fault failure must not succeed"
        fi
        grep -q "$reason" "$work/out" || fail "[$shell] unexpected $fault failure: $(cat "$work/out")"
        [[ "$(sha256 "$prefix/bin/pairroom")" == "$old_digest" ]] ||
            fail "[$shell] $fault failure damaged the existing CLI"
        [[ -x "$prefix/bin/pairroom" ]] || fail "[$shell] $fault failure lost the executable CLI"
        if grep -q 'Installed PairRoom CLI' "$work/out"; then
            fail "[$shell] $fault failure reported success"
        fi
        assert_temps_removed
    done
    run_install "$shell" "$prefix" >"$work/out" 2>&1 || fail "[$shell] upgrade failed: $(cat "$work/out")"
    [[ "$(sha256 "$prefix/bin/pairroom")" == "$(sha256 "$work/good-binary")" ]] ||
        fail "[$shell] upgrade did not replace the existing CLI"
    assert_temps_removed

    # A directory at the executable path must not turn mv into a successful
    # move *inside* that directory, followed by a false installation receipt.
    prefix="$work/directory-${shell// /-}"
    mkdir -p "$prefix/bin/pairroom"
    if run_install "$shell" "$prefix" >"$work/out" 2>&1; then
        fail "[$shell] a directory at the CLI path must not report successful installation"
    fi
    [[ -z "$(find "$prefix/bin/pairroom" -type f -print)" ]] ||
        fail "[$shell] a directory at the CLI path was modified"
    assert_temps_removed

    # An unreachable release API fails with the resolution message.
    prefix="$work/offline-${shell// /-}"
    if run_install "$shell" "$prefix" PAIRROOM_REPOSITORY=missing/repo >"$work/out" 2>&1; then
        fail "[$shell] unresolved latest tag must fail"
    fi
    grep -q 'could not resolve the latest PairRoom release tag' "$work/out" ||
        fail "[$shell] unexpected resolution output: $(cat "$work/out")"

    printf 'install.sh tests passed under %s\n' "$shell"
done
