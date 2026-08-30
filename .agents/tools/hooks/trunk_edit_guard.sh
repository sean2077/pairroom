#!/usr/bin/env bash
# trunk_edit_guard.sh — shared PreToolUse guard for the worktree-per-change flow.
#
# Installed by the agent-scaffold skill. Enforces the hard invariant: never edit
# non-ignored project files in the primary worktree. Its checked-out branch is the
# active trunk regardless of branch name; linked change worktrees pass.
# Blocks the wrong move and points at `.agents/tools/worktree.sh new <name>`.
#
# Wired for BOTH runtimes at the same shared impl:
#   - Claude Code: .claude/settings.json PreToolUse → this script ($CLAUDE_PROJECT_DIR set)
#   - Codex:       .codex/hooks.json     PreToolUse → this script (proj resolved via git)
# Reads the tool-call JSON on stdin and exits:
#   0  allow
#   2  block — the message on stderr tells the agent what to run instead
# Invalid or unparseable hook input exits 2 so a transport failure cannot turn a
# protected edit into an allow decision. Other host/runtime failures stay visible.
#
# Only guards files in the project repo (same git-common-dir); nested/sibling
# repos pass through, and gitignored paths (build output, caches) are never blocked.
#
# Escape hatches — use ONLY when the user explicitly authorizes a trunk edit:
#   WORKTREE_ALLOW_TRUNK_EDIT=1              one-shot env bypass
#   touch <repo>/.claude/allow-trunk-edit   flag file, auto-expires in 2 h
set -uo pipefail

[[ "${WORKTREE_ALLOW_TRUNK_EDIT:-0}" == "1" ]] && exit 0

hook_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
common="$hook_dir/hook-common.sh"
[[ -f "$common" ]] || { echo "trunk_edit_guard: missing hook-common.sh, allowing" >&2; exit 0; }
# shellcheck source=hook-common.sh
# shellcheck disable=SC1091
source "$common"
proj="$(hook_project_root 2>/dev/null || true)"
[[ -n "$proj" ]] || { echo "trunk_edit_guard: cannot resolve project root, allowing" >&2; exit 0; }
wt_cmd="${WORKTREE_GUARD_CMD:-bash .agents/tools/worktree.sh}"

# Pull every file path the tool call would touch out of the hook JSON on stdin.
# Handles Edit/Write/NotebookEdit (file_path/notebook_path/path) and apply_patch
# style payloads (*** Add|Update|Delete File: …). The shared helper uses python
# only for JSON parsing, then Git Bash/cygpath normalizes native Windows paths.
extract_paths() {
    hook_extract_paths
}

check_path() {
    local file_path="$1"
    [[ -n "${file_path:-}" && "$file_path" == /* ]] || return 0

    # Walk up to the nearest existing dir (the file itself may be a new file).
    local dir
    dir="$(dirname -- "$file_path")"
    while [[ ! -d "$dir" && "$dir" != "/" ]]; do
        dir="$(dirname -- "$dir")"
    done

    git -C "$dir" rev-parse --is-inside-work-tree >/dev/null 2>&1 || return 0

    # Only guard the primary worktree of the project repo. In Git's model its
    # per-worktree git-dir equals the common git-dir; linked worktrees differ.
    local proj_common file_common file_git_dir
    proj_common="$(git -C "$proj" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)" || return 0
    file_common="$(git -C "$dir" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)" || return 0
    proj_common="$(cd "$proj_common" 2>/dev/null && pwd -P)" || return 0
    file_common="$(cd "$file_common" 2>/dev/null && pwd -P)" || return 0
    [[ "$file_common" == "$proj_common" ]] || return 0
    file_git_dir="$(git -C "$dir" rev-parse --path-format=absolute --git-dir 2>/dev/null)" || return 0
    file_git_dir="$(cd "$file_git_dir" 2>/dev/null && pwd -P)" || return 0
    [[ "$file_git_dir" == "$file_common" ]] || return 0

    # Never block ignored files (build output, caches, vendored payloads, …).
    git -C "$dir" check-ignore -q -- "$file_path" 2>/dev/null && return 0

    local toplevel branch flag stale now mtime
    toplevel="$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null)" || return 0
    branch="$(git -C "$toplevel" symbolic-ref --short -q HEAD 2>/dev/null || true)"
    [[ -n "$branch" ]] || branch="<detached>"
    flag="$toplevel/.claude/allow-trunk-edit"
    stale=""
    if [[ -f "$flag" ]]; then
        now="$(date +%s)"
        mtime="$(stat -c %Y "$flag" 2>/dev/null || stat -f %m "$flag" 2>/dev/null || echo 0)"
        if ((now - mtime <= 7200)); then
            return 0
        fi
        stale=" (a STALE $flag exists — touch it again to renew)"
    fi

    cat >&2 <<EOF
trunk_edit_guard: BLOCKED — $file_path
This is the primary worktree on active trunk branch '$branch'. Every change,
however small ("just docs" is NOT an exception), starts in .worktrees/:
    $wt_cmd new <name>      # then edit inside .worktrees/<name>/
Only if the user explicitly authorized a trunk edit in this conversation:
    touch $toplevel/.claude/allow-trunk-edit    # auto-expires in 2 h${stale}
EOF
    return 2
}

paths=""
if ! paths="$(extract_paths)"; then
    echo "trunk_edit_guard: cannot parse hook input safely; blocking the edit" >&2
    exit 2
fi

blocked=0
while IFS= read -r path; do
    [[ -n "$path" ]] || continue
    if ! check_path "$path"; then
        blocked=2
    fi
done <<<"$paths"

exit "$blocked"
