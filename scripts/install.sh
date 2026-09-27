#!/bin/sh
# Install the PairRoom CLI from GitHub Releases.
# POSIX sh: this script must run under dash, busybox sh, and bash alike.
# Usage:
#   curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh | sh
#   PAIRROOM_VERSION=v1.2.0 PREFIX="$HOME/.local" sh install.sh
# PAIRROOM_REPOSITORY overrides the owner/name of the release repository (for
# forks); the generic GITHUB_REPOSITORY is deliberately ignored because every
# GitHub Actions job sets it to the calling repository.
set -eu

REPO="${PAIRROOM_REPOSITORY:-sean2077/pairroom}"
PREFIX="${PREFIX:-}"
REQUESTED_VERSION="${PAIRROOM_VERSION:-}"

die() {
    printf 'pairroom-install: %s\n' "$*" >&2
    exit 1
}

cli_os() {
    _os="${PAIRROOM_TEST_OS:-$(uname -s)}"
    _os="$(printf '%s' "$_os" | tr '[:upper:]' '[:lower:]')"
    case "$_os" in
        linux) printf 'linux\n' ;;
        darwin) printf 'darwin\n' ;;
        mingw*|msys*|cygwin*) printf 'windows\n' ;;
        *) die "unsupported OS: $_os (need linux, darwin, or Windows)" ;;
    esac
}

cli_arch() {
    _arch="${PAIRROOM_TEST_ARCH:-$(uname -m)}"
    case "$_arch" in
        x86_64 | amd64) printf 'amd64\n' ;;
        arm64 | aarch64) printf 'arm64\n' ;;
        *) die "unsupported architecture: $_arch" ;;
    esac
}

asset_name() {
    _name="pairroom-cli-${3}-${1}-${2}"
    if [ "$1" = windows ]; then
        _name="${_name}.exe"
    fi
    printf '%s\n' "$_name"
}

resolve_tag() {
    if [ -n "$REQUESTED_VERSION" ]; then
        case "$REQUESTED_VERSION" in
            v*) printf '%s\n' "$REQUESTED_VERSION" ;;
            *) printf 'v%s\n' "$REQUESTED_VERSION" ;;
        esac
        return 0
    fi
    # No pipefail in POSIX sh: fetch first so a failed request is not masked
    # by the status of the parsing pipeline.
    _json="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest")" ||
        die "could not resolve the latest PairRoom release tag"
    _tag="$(printf '%s\n' "$_json" |
        sed -n 's/.*"tag_name":[[:space:]]*"\(v[^"]*\)".*/\1/p' | head -n 1)"
    [ -n "$_tag" ] || die "could not resolve the latest PairRoom release tag"
    printf '%s\n' "$_tag"
}

install_dir() {
    if [ -n "$PREFIX" ]; then
        printf '%s/bin\n' "${PREFIX%/}"
        return 0
    fi
    if [ -w /usr/local/bin ] || [ "$(id -u)" -eq 0 ]; then
        printf '/usr/local/bin\n'
        return 0
    fi
    printf '%s/.local/bin\n' "${HOME:?HOME is required}"
}

OS="$(cli_os)"
ARCH="$(cli_arch)"
if [ "$OS" = linux ] && [ "$ARCH" != amd64 ]; then
    die "Linux CLI releases are amd64 only (this host is ${ARCH})"
fi
if [ "$OS" = windows ] && [ "$ARCH" != amd64 ]; then
    die "Windows CLI releases are amd64 only (this host is ${ARCH})"
fi

TAG="$(resolve_tag)"
ASSET="$(asset_name "$OS" "$ARCH" "$TAG")"
if [ "${1:-}" = "--print-asset" ]; then
    printf '%s\n' "$ASSET"
    exit 0
fi

URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"
DEST_DIR="$(install_dir)"
mkdir -p "$DEST_DIR"
DEST="${DEST_DIR}/pairroom"
if [ "$OS" = windows ]; then
    DEST="${DEST}.exe"
fi

tmp=""
tmp_sums=""
trap 'rm -f "$tmp" "$tmp_sums"' EXIT
# POSIX shells do not run the EXIT trap on a fatal signal; exit explicitly so
# an interrupted download still removes its temporary files.
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
# Stage the binary beside its destination. A cross-filesystem mv from /tmp
# can become a non-atomic copy that damages an installed CLI on failure.
# Install cleanup before either allocation, including a failed second mktemp.
tmp="$(mktemp "${DEST_DIR}/.pairroom-install.XXXXXX")" ||
    die "could not create a staging file in ${DEST_DIR}"
tmp_sums="$(mktemp)" || die "could not create a temporary checksum file"
curl -fsSL "$URL" -o "$tmp" || die "could not download ${ASSET} for ${TAG}"

# Verify the published release checksum before installing: a truncated or
# tampered download must never become the pairroom binary on PATH.
SUMS_URL="https://github.com/${REPO}/releases/download/${TAG}/SHA256SUMS"
curl -fsSL "$SUMS_URL" -o "$tmp_sums" ||
    die "could not download SHA256SUMS for ${TAG}; refusing to install an unverified binary"
expected="$(awk -v name="$ASSET" '$2 == name { print $1 }' "$tmp_sums" | head -n 1)"
[ -n "$expected" ] || die "SHA256SUMS for ${TAG} has no entry for ${ASSET}"
if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$tmp" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$tmp" | awk '{ print $1 }')"
else
    die "neither sha256sum nor shasum is available; cannot verify the downloaded binary"
fi
[ -n "$actual" ] && [ "$actual" = "$expected" ] ||
    die "checksum mismatch for ${ASSET}: expected ${expected}, got ${actual}"

chmod +x "$tmp"
# Without this check mv would install *inside* a directory (or its symlink)
# named pairroom and then falsely report a working executable at DEST.
[ ! -d "$DEST" ] || die "destination is a directory: ${DEST}"
mv "$tmp" "$DEST"
rm -f "$tmp_sums"
trap - EXIT HUP INT TERM

printf 'Installed PairRoom CLI %s to %s\n' "$TAG" "$DEST"
case ":$PATH:" in
    *":${DEST_DIR}:"*) ;;
    *)
        printf 'Note: %s is not on PATH.\n' "$DEST_DIR"
        ;;
esac
