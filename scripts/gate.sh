#!/usr/bin/env bash
# gate.sh runs the merge gates for the given package patterns, e.g.
#   scripts/gate.sh ./spec/...
set -euo pipefail
export GOTOOLCHAIN=local
if [ "$#" -eq 0 ]; then set -- ./...; fi
go build "$@"
go vet "$@"
go test -race -count=1 "$@"
unformatted=$(gofmt -l $(go list -f '{{.Dir}}' "$@") || true)
if [ -n "$unformatted" ]; then echo "gofmt needed:"; echo "$unformatted"; exit 1; fi
"$(dirname "$0")/check-paths.sh" $(go list -f '{{.Dir}}' "$@")
echo "gate: OK $*"
