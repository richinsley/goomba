#!/usr/bin/env bash
# check-paths.sh fails if any given file or directory (default: the whole
# repository) contains a machine-specific path: a home directory, a user name
# or a local tool root. Conda's build placeholder (/home/conda/...) is package
# data and is allowed. Use the GOOMBA_* variables described in AGENTS.md.
#
# Only files that git tracks or would track are scanned: never .git (a file in
# a worktree, holding a machine path) or ignored scratch output such as tmp/.
set -uo pipefail
if [ "$#" -eq 0 ]; then set -- "$(git rev-parse --show-toplevel)"; fi
pattern='/home/(?!conda/)[A-Za-z0-9_.-]+|/Users/[A-Za-z0-9_.-]+|[A-Za-z]:(\\{1,2}|/)Users[\\/]|\.jb-mesh|/tmp/claude-'
git grep -InP --untracked -e "$pattern" -- "$@" ':(exclude,glob)**/check-paths.sh'
case $? in
  0) echo "check-paths: machine-specific path found; use GOOMBA_REFS, GOOMBA_REF_ROOT or GOOMBA_MICROMAMBA" >&2
     exit 1 ;;
  1) echo "check-paths: OK" ;;
  *) echo "check-paths: git grep failed" >&2
     exit 1 ;;
esac
