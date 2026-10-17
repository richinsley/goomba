#!/usr/bin/env bash
# check-paths.sh fails if any given file or directory (default: the whole
# repository) contains a machine-specific path: a home directory, a user name
# or a local tool root. Conda's build placeholder (/home/conda/...) is package
# data and is allowed. Use the GOOMBA_* variables described in AGENTS.md.
set -euo pipefail
if [ "$#" -eq 0 ]; then set -- "$(git rev-parse --show-toplevel)"; fi
pattern='/home/(?!conda/)[A-Za-z0-9_.-]+|/Users/[A-Za-z0-9_.-]+|[A-Za-z]:\\\\Users\\\\|\.jb-mesh|/tmp/claude-'
if grep -rInP --exclude-dir=.git --exclude=check-paths.sh "$pattern" "$@"; then
  echo "check-paths: machine-specific path found; use GOOMBA_REFS, GOOMBA_REF_ROOT or GOOMBA_MICROMAMBA" >&2
  exit 1
fi
echo "check-paths: OK"
