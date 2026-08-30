#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
source "$ROOT/scripts/lib/python.sh"
VERSION=${VERSION:-$(tr -d '\r\n' < VERSION)}
COMMIT=${COMMIT:-$(git rev-parse HEAD)}
BUILD_DATE=${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
LAST_TAG=${LAST_TAG:-$(git describe --tags --abbrev=0 2>/dev/null || printf unknown)}
COMMITS_SINCE_TAG=${COMMITS_SINCE_TAG:-$(git rev-list "${LAST_TAG}..HEAD" --count 2>/dev/null || printf unknown)}
DIST=${DIST:-dist}
PYTHON=$(pairroom_resolve_python)
VERSION_PKG=github.com/sean2077/pairroom/internal/version
LDFLAGS="-s -w -X ${VERSION_PKG}.Commit=${COMMIT} -X ${VERSION_PKG}.BuildDate=${BUILD_DATE} -X ${VERSION_PKG}.LastTag=${LAST_TAG} -X ${VERSION_PKG}.CommitsSinceTag=${COMMITS_SINCE_TAG}"

DIST_NATIVE=$("$PYTHON" - "$ROOT" "$DIST" <<'PY'
import pathlib,sys
root=pathlib.Path(sys.argv[1]).resolve()
candidate=pathlib.Path(sys.argv[2])
dist=(candidate if candidate.is_absolute() else root/candidate).resolve()
try:
    relative=dist.relative_to(root)
except ValueError:
    raise SystemExit(f"DIST must resolve below {root / 'dist'}")
if not relative.parts or relative.parts[0] != 'dist':
    raise SystemExit(f"DIST must resolve to {root / 'dist'} or one of its descendants")
print(dist)
PY
)
if command -v cygpath >/dev/null 2>&1; then
  DIST=$(cygpath -u "$DIST_NATIVE")
else
  DIST=$DIST_NATIVE
fi

if [[ $(tr -d '\r\n' < VERSION) != "$VERSION" ]]; then
  echo "VERSION file does not match requested version $VERSION" >&2
  exit 1
fi
if [[ -n $(git status --porcelain --untracked-files=normal) ]]; then
  echo "release requires a clean Git worktree" >&2
  git status --short >&2
  exit 1
fi
git cat-file -e "${COMMIT}^{commit}"
git fsck --full >/dev/null

NOTES_TMP=$(mktemp)
cleanup() { rm -f "$NOTES_TMP"; }
trap cleanup EXIT
"$PYTHON" scripts/extract-changelog.py \
  --changelog CHANGELOG.md \
  --tag "v${VERSION}" \
  --output "$NOTES_TMP"

if [[ ${SKIP_CHECKS:-0} != 1 ]]; then
  make check
  bash scripts/mock-e2e.sh
fi
rm -rf "$DIST"
mkdir -p "$DIST"

build() {
  local goos=$1 goarch=$2 output=$3
  echo "building ${goos}/${goarch} -> ${output}"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -buildvcs=false -trimpath -ldflags="$LDFLAGS" -o "$DIST/$output" ./cmd/pairroom
}

build linux amd64 "pairroom-v${VERSION}-linux-amd64"
build windows amd64 "pairroom-v${VERSION}-windows-amd64.exe"
build darwin arm64 "pairroom-v${VERSION}-darwin-arm64"
build darwin amd64 "pairroom-v${VERSION}-darwin-amd64"

git archive --format=tar.gz --prefix="pairroom-v${VERSION}/" -o "$DIST/pairroom-v${VERSION}-source.tar.gz" "$COMMIT"
git archive --format=zip --prefix="pairroom-v${VERSION}/" -o "$DIST/pairroom-v${VERSION}-source.zip" "$COMMIT"
cp "$NOTES_TMP" "$DIST/RELEASE_NOTES.md"

go run ./scripts/releasemeta.go --dist "$DIST" --version "$VERSION" --commit "$COMMIT" --build-date "$BUILD_DATE"

HOST_GOOS=$(go env GOOS)
HOST_GOARCH=$(go env GOARCH)
case "${HOST_GOOS}/${HOST_GOARCH}" in
  linux/amd64) HOST_BINARY="$DIST/pairroom-v${VERSION}-linux-amd64" ;;
  windows/amd64) HOST_BINARY="$DIST/pairroom-v${VERSION}-windows-amd64.exe" ;;
  darwin/arm64) HOST_BINARY="$DIST/pairroom-v${VERSION}-darwin-arm64" ;;
  darwin/amd64) HOST_BINARY="$DIST/pairroom-v${VERSION}-darwin-amd64" ;;
  *)
    echo "release verification does not build a host binary for ${HOST_GOOS}/${HOST_GOARCH}" >&2
    exit 1
    ;;
esac
"$HOST_BINARY" version --json > "$DIST/version-check.json"
# version-check is validation evidence rather than a release payload; regenerate
# checksums once more so it is covered too.
go run ./scripts/releasemeta.go --dist "$DIST" --version "$VERSION" --commit "$COMMIT" --build-date "$BUILD_DATE"
VERSION="$VERSION" DIST="$DIST" bash scripts/verify-artifacts.sh

echo "release artifacts written to $DIST"
