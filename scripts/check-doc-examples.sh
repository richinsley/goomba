#!/usr/bin/env bash
# check-doc-examples.sh fails if a Go code block in README.md or docs/ is not
# copied verbatim from a program in examples/, so that every Go example the
# documentation shows is part of a program the gate builds and vets (running
# the examples is a release step: see docs/library.md). Blocks in
# docs/architecture.md show API signatures, not examples, and are skipped.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
python3 - README.md docs/*.md <<'PY'
import pathlib, re, sys
examples = "\n".join(p.read_text() for p in sorted(pathlib.Path("examples").rglob("*.go")))
bad = 0
for name in sys.argv[1:]:
    if name == "docs/architecture.md":
        continue
    text = pathlib.Path(name).read_text()
    # Any fence of a Go block, indented (in a list) or not.
    for m in re.finditer(r"^([ \t]*)```(?:go|golang)[ \t]*\n(.*?)^[ \t]*```", text, re.S | re.M):
        indent = m.group(1)
        body = "".join(l[len(indent):] if l.startswith(indent) else l
                       for l in m.group(2).splitlines(keepends=True))
        if body not in examples:
            line = text.count("\n", 0, m.start()) + 1
            print(f"{name}:{line}: Go block is not in examples/")
            bad += 1
if bad:
    sys.exit(1)
print("check-doc-examples: OK")
PY
