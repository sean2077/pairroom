#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
VERSION=${VERSION:-$(tr -d '\r\n' < VERSION)}
COMMIT=${COMMIT:-$(git rev-parse HEAD)}
BUILD_DATE=${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
DIST=${DIST:-dist}
VERSION_PKG=github.com/sean2077/pairroom/internal/version
LDFLAGS="-s -w -X ${VERSION_PKG}.Commit=${COMMIT} -X ${VERSION_PKG}.BuildDate=${BUILD_DATE}"

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
cp "docs/RELEASE_NOTES_v${VERSION}.md" "$DIST/RELEASE_NOTES.md"

go run ./scripts/releasemeta.go --dist "$DIST" --version "$VERSION" --commit "$COMMIT" --build-date "$BUILD_DATE"

"$DIST/pairroom-v${VERSION}-linux-amd64" version --json > "$DIST/version-check.json"
# version-check is validation evidence rather than a release payload; regenerate
# checksums once more so it is covered too.
go run ./scripts/releasemeta.go --dist "$DIST" --version "$VERSION" --commit "$COMMIT" --build-date "$BUILD_DATE"
VERSION="$VERSION" DIST="$DIST" bash scripts/verify-artifacts.sh

echo "release artifacts written to $DIST"
