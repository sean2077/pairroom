#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
source "$ROOT/scripts/lib/python.sh"
PYTHON=$(pairroom_resolve_python)
VERSION=${VERSION:-$(tr -d '\r\n' < VERSION)}
DIST=${DIST:-dist}

required=(
  "pairroom-cli-v${VERSION}-linux-amd64"
  "pairroom-cli-v${VERSION}-windows-amd64.exe"
  "pairroom-cli-v${VERSION}-darwin-arm64"
  "pairroom-cli-v${VERSION}-darwin-amd64"
  "pairroom-v${VERSION}-source.tar.gz"
  "pairroom-v${VERSION}-source.zip"
  "pairroom-v${VERSION}-SBOM.spdx.json"
  "pairroom-v${VERSION}-provenance.json"
  "install.sh"
  "RELEASE_NOTES.md"
  "THIRD_PARTY_NOTICES.md"
  "version-check.json"
  "SHA256SUMS"
)
for name in "${required[@]}"; do
  test -f "$DIST/$name" || { echo "missing release artifact: $name" >&2; exit 1; }
done

(cd "$DIST" && sha256sum -c SHA256SUMS)
file "$DIST/pairroom-cli-v${VERSION}-linux-amd64" | grep -q 'ELF 64-bit'
file "$DIST/pairroom-cli-v${VERSION}-windows-amd64.exe" | grep -q 'PE32+'
file "$DIST/pairroom-cli-v${VERSION}-darwin-arm64" | grep -q 'Mach-O 64-bit arm64'
file "$DIST/pairroom-cli-v${VERSION}-darwin-amd64" | grep -q 'Mach-O 64-bit x86_64'
tar -tzf "$DIST/pairroom-v${VERSION}-source.tar.gz" >/dev/null
unzip -tq "$DIST/pairroom-v${VERSION}-source.zip" >/dev/null

"$PYTHON" - "$DIST" "$VERSION" <<'PY'
import hashlib,json,os,sys
root,version=sys.argv[1:]
version_info=json.load(open(os.path.join(root,'version-check.json')))
assert version_info['version']==version, version_info
assert version_info['commit'] not in ('','dev'), version_info
assert version_info['build_date'] not in ('','unknown'), version_info
assert version_info['last_tag'] not in ('','unknown'), version_info
assert version_info['commits_since_tag'] not in ('','unknown'), version_info
sbom=json.load(open(os.path.join(root,f'pairroom-v{version}-SBOM.spdx.json')))
assert sbom['spdxVersion']=='SPDX-2.3'
assert sbom['packages'][0]['versionInfo']==version
provenance=json.load(open(os.path.join(root,f'pairroom-v{version}-provenance.json')))
assert provenance['version']==version
assert provenance['source_commit']==version_info['commit']
checks={}
for line in open(os.path.join(root,'SHA256SUMS')):
    digest,name=line.rstrip('\n').split('  ',1); checks[name]=digest
files=sorted(name for name in os.listdir(root) if os.path.isfile(os.path.join(root,name)) and name!='SHA256SUMS')
assert sorted(checks)==files, (sorted(checks),files)
for name,digest in checks.items():
    actual=hashlib.sha256(open(os.path.join(root,name),'rb').read()).hexdigest()
    assert actual==digest,(name,actual,digest)
PY

echo "release artifact verification passed"
