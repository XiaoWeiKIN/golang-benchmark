"""Reproduce summary.json from the complete, unedited Go benchmark output.

Usage: python3 summarize.py raw.txt > summary.json
Timing values are observations, not significance or acceptance decisions.
"""
import json
import re
import statistics
import sys
from pathlib import Path

raw = Path(sys.argv[1]).read_text()
if not re.search(r"^PASS$", raw, re.MULTILINE):
    raise SystemExit("benchmark run did not pass")
rows = {}
for line in raw.splitlines():
    match = re.match(
        r"(Benchmark\S+)\s+\d+\s+([\d.]+) ns/op\s+(\d+) B/op\s+(\d+) allocs/op",
        line,
    )
    if match:
        name, ns, size, allocs = match.groups()
        rows.setdefault(name, []).append((float(ns), int(size), int(allocs)))
if len(rows) != 168 or any(len(samples) != 8 for samples in rows.values()):
    raise SystemExit("expected 168 benchmark variants with 8 samples each")
summary = {
    name: {
        "samples": len(samples),
        "median_ns": statistics.median(v[0] for v in samples),
        "min_ns": min(v[0] for v in samples),
        "max_ns": max(v[0] for v in samples),
        "bytes": sorted({v[1] for v in samples}),
        "allocs": sorted({v[2] for v in samples}),
    }
    for name, samples in rows.items()
}
print(json.dumps(summary, indent=2))
