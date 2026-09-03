#!/usr/bin/env bash
# Install the PairRoom CLI from GitHub Releases.
# Usage:
#   curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh | sh
#   PAIRROOM_VERSION=v1.2.0 PREFIX="$HOME/.local" sh install.sh
set -euo pipefail

REPO="${GITHUB_REPOSITORY:-sean2077/pairroom}"
PREFIX="${PREFIX:-}"
REQUESTED_VERSION="${PAIRROOM_VERSION:-}"

die() {
    printf 'pairroom-install: %s\n' "$*" >&2
    exit 1
}

cli_os() {
    local os
    os="${PAIRROOM_TEST_OS:-$(uname -s)}"
    os="$(printf '%s' "$os" | tr '[:upper:]' '[:lower:]')"
    case "$os" in
        linux) printf 'linux\n' ;;
        darwin) printf 'darwin\n' ;;
        mingw*|msys*|cygwin*) printf 'windows\n' ;;
        *) die "unsupported OS: $os (need linux, darwin, or Windows)" ;;
    esac
}

cli_arch() {
    local arch
    arch="${PAIRROOM_TEST_ARCH:-$(uname -m)}"
    case "$arch" in
        x86_64 | amd64) printf 'amd64\n' ;;
        arm64 | aarch64) printf 'arm64\n' ;;
        *) die "unsupported architecture: $arch" ;;
    esac
}

asset_name() {
    local os="$1" arch="$2" tag="$3"
    local name="pairroom-cli-${tag}-${os}-${arch}"
    if [[ "$os" == windows ]]; then
        name="${name}.exe"
    fi
    printf '%s\n' "$name"
}

resolve_tag() {
    local tag="$REQUESTED_VERSION"
    if [[ -n "$tag" ]]; then
        case "$tag" in
            v*) printf '%s\n' "$tag" ;;
            *) printf 'v%s\n' "$tag" ;;
        esac
        return 0
    fi
    tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" |
        sed -n 's/.*"tag_name":[[:space:]]*"\(v[^"]*\)".*/\1/p' | head -n 1)"
    [[ -n "$tag" ]] || die "could not resolve the latest PairRoom release tag"
    printf '%s\n' "$tag"
}

install_dir() {
    if [[ -n "$PREFIX" ]]; then
        printf '%s/bin\n' "${PREFIX%/}"
        return 0
    fi
    if [[ -w /usr/local/bin ]] || [[ "$(id -u)" -eq 0 ]]; then
        printf '/usr/local/bin\n'
        return 0
    fi
    printf '%s/.local/bin\n' "${HOME:?HOME is required}"
}

OS="$(cli_os)"
ARCH="$(cli_arch)"
if [[ "$OS" == linux && "$ARCH" != amd64 ]]; then
    die "Linux CLI releases are amd64 only (this host is ${ARCH})"
fi
if [[ "$OS" == windows && "$ARCH" != amd64 ]]; then
    die "Windows CLI releases are amd64 only (this host is ${ARCH})"
fi

TAG="$(resolve_tag)"
ASSET="$(asset_name "$OS" "$ARCH" "$TAG")"
if [[ "${1:-}" == "--print-asset" ]]; then
    printf '%s\n' "$ASSET"
    exit 0
fi

URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"
DEST_DIR="$(install_dir)"
mkdir -p "$DEST_DIR"
DEST="${DEST_DIR}/pairroom"
if [[ "$OS" == windows ]]; then
    DEST="${DEST}.exe"
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -fsSL "$URL" -o "$tmp"
chmod +x "$tmp"
mv "$tmp" "$DEST"
trap - EXIT

printf 'Installed PairRoom CLI %s to %s\n' "$TAG" "$DEST"
case ":$PATH:" in
    *":${DEST_DIR}:"*) ;;
    *)
        printf 'Note: %s is not on PATH.\n' "$DEST_DIR"
        ;;
esac
