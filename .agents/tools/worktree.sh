#!/usr/bin/env bash
# worktree.sh — one-command worktree-per-change lifecycle, the mechanical half
# of trunk_edit_guard.sh. Installed into a project by the agent-scaffold skill.
#
# Discipline (why this exists): never edit the primary worktree directly. Its
# checked-out branch is the active trunk, regardless of branch name. Every change
# starts in its own .worktrees/<name> branch cut from that trunk, is merged back
# into the recorded trunk, and is cleaned up — with a fast-forward-only push and
# no history rewrites.
#
# Subcommands:
#   new <name> [--type feat|fix|docs|chore] [--trunk <branch>]
#       Branch <type>/<name> + worktree .worktrees/<name> cut from the trunk tip.
#   done [--dir <wt>] [--trunk <branch>] [--message <msg>] [--no-push] [--keep-branch]
#       Merge the current (or --dir) feature branch back into its local trunk
#       (--no-ff), ff-only push trunk, then remove the worktree + branch and prune.
#       A clean detached release worktree is removed through the same guarded path.
#       A rejected push keeps retry state; --no-push explicitly cleans up locally.
#       A branch with zero new commits skips the merge.
#   release <ref>
#       Detached ref/tag-pinned worktree with a portable ref + commit basename.
#   list
#       git worktree list (verbatim).
#
# Optional heavy-dir sharing: set WORKTREE_SHARE to a space-separated list of
# repo-relative *gitignored/untracked* dirs (e.g. "node_modules") to hardlink-share
# into a new/release worktree — zero extra disk, kept out of `git status`.
#
# Removal retries are bounded by WORKTREE_REMOVE_RETRIES (default 8; 0 disables)
# and WORKTREE_REMOVE_RETRY_DELAY_SECONDS (default 1). Retries never use --force
# or recursive deletion; an unregistered residue is removed only when empty.
#
# Trunk resolution: --trunk, then $WORKTREE_TRUNK, then the primary worktree
# branch. `new` records the result so `done` returns to the same trunk.
# Push is fast-forward-only; it never force-pushes — that stays the user's call.
# ---8<--- help ends here
set -euo pipefail

usage() { sed -n '2,/^# ---8<---/p' "$0" | sed '/^# ---8<---/d; s/^# \?//'; exit "${1:-0}"; }
log()  { printf '\033[1;34m[worktree]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[worktree] ABORT:\033[0m %s\n' "$*" >&2; exit 2; }

[[ $# -ge 1 ]] || usage 2
CMD="$1"; shift
case "$CMD" in
    new|release|done|list) ;;
    -h|--help) [[ $# -eq 0 ]] || usage 2; usage 0 ;;
    *) die "unknown subcommand: $CMD (new/done/release/list)" ;;
esac
if [[ $# -eq 1 && ( "$1" == -h || "$1" == --help ) ]]; then
    usage 0
fi

# Repo anchors come from the installed helper, not the caller's current directory.
# COMMON_GIT = shared git dir; ROOT = primary worktree (share source).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)" \
    || die "could not resolve the installed helper directory"
HELPER_REPO="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel 2>/dev/null)" \
    || die "worktree helper is not installed inside a git repository"
COMMON_GIT="$(git -C "$HELPER_REPO" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)" \
    || die "could not resolve the repository common git directory"
COMMON_GIT_CANON="$(cd "$COMMON_GIT" 2>/dev/null && pwd -P)" \
    || die "could not canonicalize the repository common git directory"
ROOT="$(dirname "$COMMON_GIT")"
WT_BASE="$ROOT/.worktrees"

default_trunk() {
    local branch
    if [[ -n "${WORKTREE_TRUNK:-}" ]]; then
        printf '%s\n' "$WORKTREE_TRUNK"
        return 0
    fi
    branch="$(git -C "$ROOT" symbolic-ref --short -q HEAD 2>/dev/null)" \
        || die "primary worktree is detached; check out the intended trunk or pass --trunk <branch>"
    printf '%s\n' "$branch"
}

recorded_trunk_for() {
    git -C "$ROOT" config --get "branch.$1.agentScaffoldTrunk" 2>/dev/null || true
}

ensure_worktrees_ignored() {
    git -C "$ROOT" check-ignore -q .worktrees 2>/dev/null && return 0
    local excl="$COMMON_GIT/info/exclude"
    mkdir -p "$COMMON_GIT/info"
    grep -qE '^/?\.worktrees/?$' "$excl" 2>/dev/null && return 0
    printf '.worktrees/\n' >> "$excl"
    log "excluded '.worktrees/' via .git/info/exclude (add it to tracked .gitignore for a shared rule)"
}

repoint_submodules() {
    local wt="$1" sub
    [[ -f "$ROOT/.gitmodules" ]] || return 0
    while read -r _ sub; do
        [[ -e "$wt/$sub/.git" ]] || continue
        [[ -d "$COMMON_GIT/modules/$sub" ]] || continue
        echo "gitdir: $COMMON_GIT/modules/$sub" > "$wt/$sub/.git"
    done < <(git config -f "$ROOT/.gitmodules" --get-regexp '\.path$' 2>/dev/null || true)
}

share_dirs() {
    local wt="$1" d
    [[ -n "${WORKTREE_SHARE:-}" ]] || return 0
    for d in $WORKTREE_SHARE; do
        [[ -d "$ROOT/$d" ]] || { log "WARN: share dir '$d' not found, skipping"; continue; }
        [[ -e "$wt/$d" ]] && { log "WARN: '$d' already present in worktree, skipping share"; continue; }
        mkdir -p "$wt/$(dirname "$d")"
        if cp -al "$ROOT/$d" "$wt/$d" 2>/dev/null; then
            log "shared $d ← primary worktree (hardlinked, zero new disk)"
        else
            cp -a "$ROOT/$d" "$wt/$d"
            log "WARN: hardlink unsupported here - copied $d (uses disk; not a zero-cost share)"
        fi
    done
    repoint_submodules "$wt"
}

primary_dir_for_trunk() {
    git -C "$ROOT" worktree list --porcelain | awk -v want="refs/heads/$1" '
        /^worktree /{dir=substr($0,10)} /^branch /{if (substr($0,8)==want) {print dir; exit}}'
}

cmd_new() {
    local NAME="" TYPE="feat" TRUNK=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --type)  [[ $# -ge 2 ]] || die "--type requires feat|fix|docs|chore"; TYPE="$2"; shift ;;
            --trunk) [[ $# -ge 2 ]] || die "--trunk requires a branch"; TRUNK="$2"; shift ;;
            -*) die "unknown flag: $1" ;;
            *)  [[ -z "$NAME" ]] || die "only one <name> accepted"; NAME="$1" ;;
        esac
        shift
    done
    [[ "$NAME" =~ ^[a-z][a-z0-9-]{1,46}[a-z0-9]$ ]] \
        || die "name must be lowercase kebab-case, 3-48 chars (got: ${NAME:-<empty>})"
    case "$TYPE" in feat|fix|docs|chore) ;; *) die "--type feat|fix|docs|chore" ;; esac
    [[ -n "$TRUNK" ]] || TRUNK="$(default_trunk)"
    git -C "$ROOT" show-ref --verify -q "refs/heads/$TRUNK" || die "no local trunk branch '$TRUNK'"
    local BRANCH="$TYPE/$NAME" WTDIR="$WT_BASE/$NAME"
    [[ ! -e "$WTDIR" ]] || die "already exists: $WTDIR"
    ensure_worktrees_ignored
    git -C "$ROOT" worktree add "$WTDIR" -b "$BRANCH" "$TRUNK"
    git -C "$ROOT" config "branch.$BRANCH.agentScaffoldTrunk" "$TRUNK" \
        || die "created $WTDIR but could not record its trunk; rerun done with --trunk \"$TRUNK\""
    share_dirs "$WTDIR"
    log "ready: $WTDIR  (branch $BRANCH ← $TRUNK tip)"
    log "when done, leave it before cleanup:"
    log "  cd \"$ROOT\" && bash \"$ROOT/.agents/tools/worktree.sh\" done --dir \"$WTDIR\" --trunk \"$TRUNK\""
}

cmd_release() {
    [[ $# -eq 1 ]] || die "usage: release <ref>"
    local REF="$1" RESOLVED COMMIT COMMIT_SHORT SAFE_REF WTDIR
    RESOLVED="$(git -C "$ROOT" rev-parse -q --verify "$REF" 2>/dev/null)" \
        || die "no such ref: $REF"
    COMMIT="$(git -C "$ROOT" rev-parse -q --verify "$RESOLVED^{commit}" 2>/dev/null)" \
        || die "ref does not resolve to a commit: $REF"
    COMMIT_SHORT="$(git -C "$ROOT" rev-parse --short=12 "$COMMIT")" \
        || die "could not abbreviate commit for ref: $REF"
    SAFE_REF="$(printf '%s' "$REF" \
        | sed 's/[^A-Za-z0-9._-]/-/g; s/--*/-/g; s/^-//; s/-$//' \
        | cut -c1-40)"
    [[ -n "$SAFE_REF" ]] || SAFE_REF=ref
    WTDIR="$WT_BASE/release-$SAFE_REF-$COMMIT_SHORT"
    [[ ! -e "$WTDIR" ]] || die "already exists: $WTDIR"
    ensure_worktrees_ignored
    git -C "$ROOT" worktree add --detach "$WTDIR" "$COMMIT"
    share_dirs "$WTDIR"
    log "ready: $WTDIR  (detached @ $REF) — when packaging is done: cd \"$ROOT\" && bash \"$ROOT/.agents/tools/worktree.sh\" done --dir \"$WTDIR\""
}

worktree_is_registered() {
    local TARGET="$1" REGISTRY FIELD FOUND=1 temp_parent temp_prefix temp_suffix
    temp_parent="$(cd "${TMPDIR:-/tmp}" 2>/dev/null && pwd -P)" \
        || die "temporary-directory parent is unavailable: ${TMPDIR:-/tmp}"
    temp_prefix="${temp_parent%/}/agent-scaffold-worktrees."
    REGISTRY="$(mktemp "${temp_prefix}XXXXXX")" \
        || die "could not create temporary worktree registry file"
    temp_suffix="${REGISTRY#"$temp_prefix"}"
    [[ "$REGISTRY" == "$temp_prefix"* && -n "$temp_suffix" && -f "$REGISTRY" ]] \
        || die "mktemp returned an unsafe worktree registry path: ${REGISTRY:-<empty>}"
    if ! git -C "$ROOT" worktree list --porcelain -z >"$REGISTRY"; then
        rm -f "$REGISTRY"
        die "could not inspect the worktree registry"
    fi
    while IFS= read -r -d '' FIELD; do
        if [[ "$FIELD" == "worktree $TARGET" ]]; then
            FOUND=0
            break
        fi
    done <"$REGISTRY"
    rm -f "$REGISTRY"
    return "$FOUND"
}

validate_remove_retry_config() {
    local RETRIES_RAW="${WORKTREE_REMOVE_RETRIES:-8}"
    local DELAY_RAW="${WORKTREE_REMOVE_RETRY_DELAY_SECONDS:-1}"

    [[ "$RETRIES_RAW" =~ ^[0-9]{1,2}$ ]] \
        || die "WORKTREE_REMOVE_RETRIES must be an integer from 0 to 60 (got: $RETRIES_RAW)"
    [[ "$DELAY_RAW" =~ ^[0-9]{1,2}$ ]] \
        || die "WORKTREE_REMOVE_RETRY_DELAY_SECONDS must be an integer from 0 to 30 (got: $DELAY_RAW)"
    WORKTREE_REMOVE_RETRIES_VALUE=$((10#$RETRIES_RAW))
    WORKTREE_REMOVE_RETRY_DELAY_SECONDS_VALUE=$((10#$DELAY_RAW))
    (( WORKTREE_REMOVE_RETRIES_VALUE <= 60 )) \
        || die "WORKTREE_REMOVE_RETRIES must be an integer from 0 to 60 (got: $RETRIES_RAW)"
    (( WORKTREE_REMOVE_RETRY_DELAY_SECONDS_VALUE <= 30 )) \
        || die "WORKTREE_REMOVE_RETRY_DELAY_SECONDS must be an integer from 0 to 30 (got: $DELAY_RAW)"
}

remove_done_worktree() {
    local WT="$1"
    local RETRIES="$WORKTREE_REMOVE_RETRIES_VALUE"
    local DELAY="$WORKTREE_REMOVE_RETRY_DELAY_SECONDS_VALUE"
    local RETRY=0 STATUS ATTEMPTS

    # Keep Git's process cwd in the stable primary worktree. On Windows, using
    # the worktree that is being removed as `git -C` can lock or invalidate the
    # command context midway through cleanup.
    while true; do
        if git -C "$ROOT" worktree remove "$WT"; then
            (( RETRY == 0 )) || log "removed worktree after $RETRY cleanup retries: $WT"
            return 0
        fi

        if ! worktree_is_registered "$WT"; then
            break
        fi

        STATUS="$(git -C "$WT" status --porcelain 2>/dev/null)" \
            || die "worktree removal failed and '$WT' remains registered but its status cannot be inspected; refusing force removal. Worktree and branch kept"
        [[ -z "$STATUS" ]] \
            || die "worktree removal failed and '$WT' is no longer clean after the removal attempt; refusing force removal. Worktree and branch kept"

        if (( RETRY >= RETRIES )); then
            ATTEMPTS=$((RETRY + 1))
            die "worktree removal failed after $ATTEMPTS attempts and '$WT' remains registered; refusing force removal.
Close processes whose current directory or open files are under it, then rerun done from a stable worktree. Worktree and branch kept"
        fi

        RETRY=$((RETRY + 1))
        log "worktree removal is blocked; retrying $RETRY/$RETRIES in ${DELAY}s: $WT"
        (( DELAY == 0 )) || sleep "$DELAY"
    done

    # Git can unregister a worktree before Windows finishes deleting its
    # directory. Registration is the safety boundary: continue branch cleanup,
    # but never recursively or forcibly delete residue that may contain new data.
    log "worktree removal reported failure after '$WT' was already unregistered; continuing cleanup"
    [[ -d "$WT" ]] || return 0

    RETRY=0
    while true; do
        if rmdir "$WT" 2>/dev/null; then
            if (( RETRY == 0 )); then
                log "removed empty residual worktree directory: $WT"
            else
                log "removed empty residual worktree directory after $RETRY retries: $WT"
            fi
            return 0
        fi
        [[ -d "$WT" ]] || return 0

        if find "$WT" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null | grep -q .; then
            log "WARNING: unregistered residual directory is non-empty; left untouched (no recursive delete attempted): $WT"
            return 0
        fi

        if (( RETRY >= RETRIES )); then
            ATTEMPTS=$((RETRY + 1))
            log "WARNING: empty unregistered residual directory remains locked after $ATTEMPTS attempts: $WT"
            log "after the owning process exits, run from outside it: rmdir \"$WT\""
            return 0
        fi

        RETRY=$((RETRY + 1))
        log "empty residual directory is still in use; retrying rmdir $RETRY/$RETRIES in ${DELAY}s: $WT"
        (( DELAY == 0 )) || sleep "$DELAY"
    done
}

cmd_done() {
    local WT="$PWD" TRUNK="" MSG="" PUSH=1 KEEP=0
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --dir)         [[ $# -ge 2 ]] || die "--dir requires a worktree path"; WT="$2"; shift ;;
            --trunk)       [[ $# -ge 2 ]] || die "--trunk requires a branch"; TRUNK="$2"; shift ;;
            --message)     [[ $# -ge 2 ]] || die "--message requires text"; MSG="$2"; shift ;;
            --no-push)     PUSH=0 ;;
            --keep-branch) KEEP=1 ;;
            *) die "unknown flag: $1" ;;
        esac
        shift
    done
    validate_remove_retry_config
    WT="$(git -C "$WT" rev-parse --show-toplevel)" || die "--dir is not inside a git repository"
    local WT_COMMON WT_COMMON_CANON WT_GIT_DIR WT_GIT_DIR_CANON
    WT_COMMON="$(git -C "$WT" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)" \
        || die "could not resolve --dir's repository identity"
    WT_COMMON_CANON="$(cd "$WT_COMMON" 2>/dev/null && pwd -P)" \
        || die "could not canonicalize --dir's repository identity"
    [[ "$WT_COMMON_CANON" == "$COMMON_GIT_CANON" ]] \
        || die "--dir belongs to a different repository than this worktree helper: $WT"
    worktree_is_registered "$WT" \
        || die "--dir is not an exact registered worktree of this repository: $WT"
    WT_GIT_DIR="$(git -C "$WT" rev-parse --path-format=absolute --git-dir 2>/dev/null)" \
        || die "could not resolve --dir's worktree git directory"
    WT_GIT_DIR_CANON="$(cd "$WT_GIT_DIR" 2>/dev/null && pwd -P)" \
        || die "could not canonicalize --dir's worktree git directory"
    [[ "$WT_GIT_DIR_CANON" != "$COMMON_GIT_CANON" ]] \
        || die "--dir is the primary worktree; done only finishes linked change or release worktrees"
    local CALLER_TOP=""
    CALLER_TOP="$(git -C "$PWD" rev-parse --show-toplevel 2>/dev/null || true)"
    case "$(uname -s)" in
        MINGW*|MSYS*|CYGWIN*)
            [[ "$CALLER_TOP" != "$WT" ]] \
                || log "WARNING: caller is inside the target worktree; Windows may keep its final directory locked. Prefer the outside-worktree --dir command printed by 'new'."
            ;;
    esac
    [[ -z "$(git -C "$WT" status --porcelain)" ]] || die "worktree is dirty, commit/stash first:
$(git -C "$WT" status --short | head -10)"
    local BRANCH RECORDED_TRUNK="" branch_rc=0
    if BRANCH="$(git -C "$WT" symbolic-ref --short -q HEAD)"; then
        :
    else
        branch_rc=$?
        [[ $branch_rc -eq 1 ]] || die "could not resolve the worktree branch"
        cd "$ROOT"
        remove_done_worktree "$WT"
        git -C "$ROOT" worktree prune
        log "done. removed clean detached release worktree: $WT"
        return 0
    fi
    RECORDED_TRUNK="$(recorded_trunk_for "$BRANCH")"
    if [[ -z "$TRUNK" ]]; then
        if [[ -n "$RECORDED_TRUNK" ]]; then
            TRUNK="$RECORDED_TRUNK"
        else
            TRUNK="$(default_trunk)"
        fi
    fi
    [[ "$BRANCH" != "$TRUNK" ]] \
        || die "target worktree checks out trunk '$TRUNK' — done only finishes change worktrees"
    git -C "$ROOT" show-ref --verify -q "refs/heads/$TRUNK" || die "trunk '$TRUNK' does not exist, pass --trunk"
    local PD; PD="$(primary_dir_for_trunk "$TRUNK")"
    [[ -n "$PD" ]] || die "no worktree has '$TRUNK' checked out (git worktree list); check it out somewhere first"
    [[ -z "$(git -C "$PD" status --porcelain)" ]] || die "trunk worktree $PD is dirty, tidy it first"
    cd "$PD"   # $WT is about to disappear; step out of it
    if git merge-base --is-ancestor "$BRANCH" "$TRUNK"; then
        log "$BRANCH has zero new commits over $TRUNK — skipping merge, just cleaning up"
    else
        # git-native message so commitlint's defaultIgnores skips this merge commit
        # shellcheck disable=SC2016  # single quotes are literal: git's default "Merge branch 'x'" wording
        git merge --no-ff "$BRANCH" -m "${MSG:-Merge branch '$BRANCH'}"
        log "merged → $TRUNK @ $(git rev-parse --short HEAD)"
    fi
    local RETRY_KEEP=""
    [[ $KEEP -eq 1 ]] && RETRY_KEEP=" --keep-branch"
    if [[ $PUSH -eq 1 ]]; then
        git push origin "$TRUNK" || die "push rejected (remote moved?); worktree and branch kept.
Resolve origin/$TRUNK in the trunk worktree, then retry:
  git -C \"$PD\" fetch origin
  git -C \"$PD\" merge origin/$TRUNK
  bash \"$PD/.agents/tools/worktree.sh\" done --dir \"$WT\" --trunk \"$TRUNK\"$RETRY_KEEP
Never force-push from here — that is the user's call."
        log "pushed $TRUNK → origin"
    else
        log "not pushed (--no-push); remember to push $TRUNK later"
    fi
    remove_done_worktree "$WT"
    [[ $KEEP -eq 1 ]] || git branch -d "$BRANCH" 2>/dev/null \
        || log "WARN: branch $BRANCH not fully merged into $TRUNK - kept; delete: git branch -D $BRANCH"
    git -C "$ROOT" worktree prune
    log "done. (if your shell is still in the removed dir, run: cd $PD)"
}

case "$CMD" in
    new)       cmd_new "$@" ;;
    release)   cmd_release "$@" ;;
    done)      cmd_done "$@" ;;
    list)      [[ $# -eq 0 ]] || die "list accepts no arguments"; git worktree list ;;
esac
