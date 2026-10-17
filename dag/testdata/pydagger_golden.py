"""Emit pydagger topology goldens for dag/testdata/pydagger_golden.json.

Run: python3 -I -B pydagger_golden.py [PYDAGGER_DIR] > pydagger_golden.json

PYDAGGER_DIR is a pydagger clone; it defaults to $GOOMBA_REFS/pydagger.

Descendents are listed sorted by (ordinal, node index): pydagger sorts them
by ordinal only, stably, from a set of id() values, so its order within one
ordinal follows memory addresses and differs from run to run. Ascendents are
listed exactly as pydagger returns them, in node insertion order.
"""
import json
import os
import sys

if len(sys.argv) > 1:
    PYDAGGER = sys.argv[1]
elif os.environ.get("GOOMBA_REFS"):
    PYDAGGER = os.path.join(os.environ["GOOMBA_REFS"], "pydagger")
else:
    sys.exit("usage: pydagger_golden.py PYDAGGER_DIR (or set GOOMBA_REFS)")
if not os.path.isfile(os.path.join(PYDAGGER, "pydagger", "graph.py")):
    sys.exit(f"pydagger_golden.py: {PYDAGGER!r} is not a pydagger clone")
sys.path.insert(0, PYDAGGER)
from pydagger import DaggerGraph, DaggerNode  # noqa: E402
from tests.graph_generators import (  # noqa: E402
    build_linear_chain, build_wide_fan_out, build_diamond, build_random_dag,
    build_parallel_chains, build_hourglass,
)
import random  # noqa: E402

SCENARIOS = [
    # tests/test_optimization.py
    ("linear_5", lambda: build_linear_chain(5)),
    ("linear_10", lambda: build_linear_chain(10)),
    ("linear_50", lambda: build_linear_chain(50)),
    ("linear_100", lambda: build_linear_chain(100)),
    ("linear_200", lambda: build_linear_chain(200)),
    ("fan_out_3x3", lambda: build_wide_fan_out(3, 3)),
    ("fan_out_4x3", lambda: build_wide_fan_out(4, 3)),
    ("fan_out_3x4", lambda: build_wide_fan_out(3, 4)),
    ("diamond_3x2", lambda: build_diamond(3, 2)),
    ("diamond_4x3", lambda: build_diamond(4, 3)),
    ("diamond_5x4", lambda: build_diamond(5, 4)),
    ("diamond_5x5", lambda: build_diamond(5, 5)),
    ("diamond_6x3", lambda: build_diamond(6, 3)),
    ("random_20_p30_s1", lambda: build_random_dag(20, 0.3, seed=1)),
    ("random_30_p20_s42", lambda: build_random_dag(30, 0.2, seed=42)),
    ("random_50_p15_s42", lambda: build_random_dag(50, 0.15, seed=42)),
    ("random_50_p10_s42", lambda: build_random_dag(50, 0.10, seed=42)),
    ("random_100_p05_s42", lambda: build_random_dag(100, 0.05, seed=42)),
    ("random_30_p20_s7", lambda: build_random_dag(30, 0.2, seed=7)),
    ("random_30_p20_s13", lambda: build_random_dag(30, 0.2, seed=13)),
    ("random_30_p20_s99", lambda: build_random_dag(30, 0.2, seed=99)),
    ("random_30_p20_s256", lambda: build_random_dag(30, 0.2, seed=256)),
    ("random_30_p20_s1024", lambda: build_random_dag(30, 0.2, seed=1024)),
    ("parallel_3x5", lambda: build_parallel_chains(3, 5)),
    ("parallel_5x20", lambda: build_parallel_chains(5, 20)),
    ("parallel_10x10", lambda: build_parallel_chains(10, 10)),
    ("hourglass_5_5_1", lambda: build_hourglass(5, 5, 1)),
    ("hourglass_10_10_2", lambda: build_hourglass(10, 10, 2)),
    ("hourglass_15_15_2", lambda: build_hourglass(15, 15, 2)),
    ("diamond_4x3_dual", lambda: build_diamond(4, 3, topology_count=2)),
    ("diamond_5x4_dual", lambda: build_diamond(5, 4, topology_count=2)),
    ("linear_50_dual", lambda: build_linear_chain(50, topology_count=2)),
    ("random_30_p20_s42_dual", lambda: build_random_dag(30, 0.2, seed=42, topology_count=2)),
    # tests/test_benchmark.py (consistency and invariants classes)
    ("wide_fan_out_5x3", lambda: build_wide_fan_out(5, 3)),
    ("random_50_p20_s42", lambda: build_random_dag(50, 0.2, seed=42)),
    ("random_100_p15_s123", lambda: build_random_dag(100, 0.15, seed=123)),
    ("random_80_p20_s99", lambda: build_random_dag(80, 0.2, seed=99)),
    ("parallel_5x10", lambda: build_parallel_chains(5, 10)),
    ("hourglass_8_8_2", lambda: build_hourglass(8, 8, 2)),
    ("diamond_3x3_dual", lambda: build_diamond(3, 3, topology_count=2)),
    ("linear_20", lambda: build_linear_chain(20)),
]


def summarize(graph, nodes, edges):
    nodes = graph.nodes
    idx = {id(n): i for i, n in enumerate(nodes)}
    out = {"nodes": len(nodes), "edges": [], "dims": []}
    seen = set()
    for a, b, t in edges:
        if t == 0:
            out["edges"].append([idx[id(a)], idx[id(b)]])
    for t in range(graph._topology_count):
        ords = [n.ordinal(t) for n in nodes]
        key = lambda i: (ords[i], i)
        d = {
            "max_ordinal": graph.max_ordinal(t),
            "subgraph_count": graph.sub_graph_count(t),
            "ordinal": ords,
            "subgraph": [n.subgraph_affiliation(t) for n in nodes],
            "descendents": [sorted((idx[id(x)] for x in n.descendents(t)), key=key) for n in nodes],
            "ascendents": [[idx[id(x)] for x in n.ascendents(t)] for n in nodes],
            "top": [idx[id(n)] for n in graph.top_level_nodes(t)],
            "bottom": [idx[id(n)] for n in graph.bottom_level_nodes(t)],
        }
        out["dims"].append(d)
    return out


res = {}
for name, b in SCENARIOS:
    g, nodes, edges = b()
    res[name] = summarize(g, nodes, edges)

# Empty and trivial graphs (test_optimization.py edge cases).
g = DaggerGraph(); res["empty"] = summarize(g, [], [])
g = DaggerGraph(); g.add_node(DaggerNode()); res["single_node"] = summarize(g, g.nodes, [])
g = DaggerGraph(); g.add_node(DaggerNode()); g.add_node(DaggerNode()); res["two_disconnected"] = summarize(g, g.nodes, [])

r = random.Random(42)
res["_python_random_42"] = [r.random() for _ in range(3)]
json.dump(res, sys.stdout, separators=(",", ":"), sort_keys=True)
