#!/usr/bin/env bash

# Private helper shared by PairRoom's Bash entry points. PYTHON, when set,
# names one executable path; otherwise prefer the native Python 3 command.
pairroom_resolve_python() {
  if [[ -n ${PYTHON:-} ]]; then
    if command -v "$PYTHON" >/dev/null 2>&1; then
      printf '%s\n' "$PYTHON"
      return 0
    fi
    printf 'configured PYTHON executable not found: %s\n' "$PYTHON" >&2
    return 1
  fi
  if command -v python3 >/dev/null 2>&1; then
    printf '%s\n' python3
    return 0
  fi
  if command -v python >/dev/null 2>&1; then
    printf '%s\n' python
    return 0
  fi
  printf 'PairRoom tooling requires Python 3\n' >&2
  return 1
}
