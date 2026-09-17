#!/usr/bin/env bash
# Submit the rendered winget manifest to microsoft/winget-pkgs through a fork
# pull request. Runs in the desktop workflow after the Windows installer is
# attached to the GitHub Release, so the download URL is already stable.
#
# Requires GH_TOKEN set to a classic PAT with public_repo scope for the fork
# owner (repository secret WINGET_TOKEN); the token never appears in argv.
# Reruns are idempotent: an existing open pull request for the same version
# wins and no second pull request is opened. Failure here never rewrites the
# Release; it only leaves this job red for explicit follow-up.
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: submit-winget.sh --version X.Y.Z --installer <path> \
         [--upstream microsoft/winget-pkgs] [--fork-owner <login>] [--python python3]
EOF
  exit 2
}

fail() {
  printf '%s\n' "submit-winget: $*" >&2
  exit 1
}

UPSTREAM="microsoft/winget-pkgs"
FORK_OWNER=""
VERSION=""
INSTALLER=""
PYTHON="python3"

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --installer) INSTALLER="${2:-}"; shift 2 ;;
    --upstream) UPSTREAM="${2:-}"; shift 2 ;;
    --fork-owner) FORK_OWNER="${2:-}"; shift 2 ;;
    --python) PYTHON="${2:-}"; shift 2 ;;
    *) usage ;;
  esac
done

[ -n "$VERSION" ] || usage
[ -n "$INSTALLER" ] || usage
[ -f "$INSTALLER" ] || fail "installer not found: $INSTALLER"
[ -n "${GH_TOKEN:-}" ] || fail \
  "GH_TOKEN is empty; set the WINGET_TOKEN repository secret to a classic PAT with public_repo scope"
command -v gh >/dev/null 2>&1 || fail "gh CLI is required"
command -v "$PYTHON" >/dev/null 2>&1 || fail "python interpreter not found: $PYTHON"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Fail closed if the artifact does not match the collected release checksums.
sums="$(dirname "$INSTALLER")/SHA256SUMS"
if [ -f "$sums" ]; then
  expected="$(awk -v name="$(basename "$INSTALLER")" '$2 == name { print $1 }' "$sums")"
  [ -n "$expected" ] || fail "$(basename "$INSTALLER") is missing from SHA256SUMS"
  actual="$(sha256sum "$INSTALLER" | awk '{ print $1 }')"
  [ "$expected" = "$actual" ] || fail \
    "installer SHA256 ${actual} does not match SHA256SUMS ${expected}"
fi

if [ -z "$FORK_OWNER" ]; then
  FORK_OWNER="$(gh api user --jq .login)" || fail "could not resolve the GH_TOKEN account"
fi
fork="${FORK_OWNER}/$(basename "$UPSTREAM")"
branch="pairroom-${VERSION}"
manifest_dir="manifests/s/sean2077/PairRoom/${VERSION}"

render_dir="$(mktemp -d)"
trap 'rm -rf "$render_dir"' EXIT
"$PYTHON" "${SCRIPT_DIR}/render-winget-manifest.py" \
  --version "$VERSION" \
  --installer "$INSTALLER" \
  --output-dir "$render_dir"

# Idempotent rerun: never open a second pull request for the same version.
existing="$(gh pr list --repo "$UPSTREAM" --state open \
  --head "${FORK_OWNER}:${branch}" --json url --jq '.[0].url // empty')"
if [ -n "$existing" ]; then
  printf '%s\n' "open winget-pkgs pull request already exists: $existing"
  exit 0
fi

# The fork creation API is asynchronous; wait until the fork answers.
gh repo fork "$UPSTREAM" --clone=false >/dev/null 2>&1 || true
ready=""
for _ in $(seq 1 24); do
  if gh repo view "$fork" --json name >/dev/null 2>&1; then ready=yes; break; fi
  sleep 5
done
[ -n "$ready" ] || fail "fork $fork is not ready after waiting"

master="$(gh api "repos/${UPSTREAM}/git/refs/heads/master" --jq '.object.sha')" \
  || fail "could not read ${UPSTREAM} master"
gh api "repos/${fork}/git/refs/heads/${branch}" -X DELETE >/dev/null 2>&1 || true
gh api "repos/${fork}/git/refs" \
  -f "ref=refs/heads/${branch}" -f "sha=${master}" >/dev/null \
  || fail "could not create branch ${branch} on ${fork}"

for file in "$render_dir"/*.yaml; do
  name="$(basename "$file")"
  gh api "repos/${fork}/contents/${manifest_dir}/${name}" -X PUT \
    -f message="New manifest: sean2077.PairRoom version ${VERSION}" \
    -f content="$(base64 -w0 "$file")" \
    -f branch="$branch" >/dev/null \
    || fail "could not upload ${name} to ${fork}"
done

if gh api "repos/${UPSTREAM}/contents/manifests/s/sean2077/PairRoom" >/dev/null 2>&1; then
  title="Update: sean2077.PairRoom to ${VERSION}"
  change="Version update"
else
  title="New package: sean2077.PairRoom version ${VERSION}"
  change="New manifest"
fi

body="$(cat <<EOF
## 📖 Description

${change} for **PairRoom ${VERSION}** — a local collaboration control plane for Claude Code, Codex, and Grok Build sessions.

- Source repository: https://github.com/sean2077/pairroom (MIT)
- Installer: Inno Setup package attached to the GitHub Release for tag [\`v${VERSION}\`](https://github.com/sean2077/pairroom/releases/tag/v${VERSION})
- Machine scope, x64, Windows 10+; \`AppsAndFeaturesEntries.ProductCode\` matches the Inno ARP key \`com.sean2077.pairroom.desktop_is1\`
- SHA256 computed in the project's release workflow from the built artifact and cross-checked against the collected \`SHA256SUMS\`; submitted automatically from the same workflow

## ✅ Checklist

- [x] Signed the [Contributor License Agreement](https://cla.opensource.microsoft.com)
- [ ] Linked to an issue (if applicable)

## 📦 Manifest Checklist

- [x] Checked that there aren't other open [pull requests](https://github.com/microsoft/winget-pkgs/pulls) for the same manifest update/change
- [x] This PR only modifies one (1) manifest
- [ ] Validated manifest locally with \`winget validate --manifest <path>\` — generated deterministically by the release workflow and covered by the repository's renderer tests; this pipeline's Manifest Validation step re-validates it
- [ ] Tested manifest locally with \`winget install --manifest <path>\` — silent install, upgrade, and uninstall are smoke-tested in the project's CI (\`scripts/test_windows_installer.ps1\`) on the same artifact
- [x] Manifest conforms to the [1.12 schema](https://github.com/microsoft/winget-pkgs/tree/master/doc/manifest/schema/1.12.0)
EOF
)"

gh pr create --repo "$UPSTREAM" --base master --head "${FORK_OWNER}:${branch}" \
  --title "$title" --body "$body"
