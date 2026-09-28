"""Summarize the predeclared 136 x 8 benchmark matrix without dropping samples.

Usage: python3 summarize.py raw.txt output-directory
Full sample ranges give a conservative sufficient separation check; this script
never converts a noisy median ranking into an accepted speedup claim.
"""
import json
import re
import statistics
import sys
from pathlib import Path

raw = Path(sys.argv[1]).read_text()
output = Path(sys.argv[2])
if not re.search(r"^PASS$", raw, re.MULTILINE):
    raise SystemExit("benchmark run did not pass")
rows = {}
for line in raw.splitlines():
    match = re.match(
        r"(Benchmark\S+)\s+\d+\s+([\d.]+) ns/op\s+(\d+) B/op\s+(\d+) allocs/op", line
    )
    if match:
        name, ns, size, allocs = match.groups()
        rows.setdefault(name, []).append((float(ns), int(size), int(allocs)))
if len(rows) != 136 or any(len(samples) != 8 for samples in rows.values()):
    raise SystemExit("expected 136 variants with 8 samples each")
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
comparisons = []
for name, candidate in summary.items():
    base, variant = name.rsplit("/", 1)
    if name.startswith("BenchmarkExtract/"):
        control = {"SliceScan": "SliceRepeated", "IndexEach": "SliceRepeated", "MapScan": "MapRepeated"}.get(variant)
    else:
        control = "Repeated" if variant in ("Indexed", "Extracted") else None
    if control is None:
        continue
    baseline = summary[base + "/" + control]
    duplicate = summary[base + "/" + control + "Noise"]
    noise = abs(duplicate["median_ns"] / baseline["median_ns"] - 1) * 100
    delta = (candidate["median_ns"] / baseline["median_ns"] - 1) * 100
    separated = candidate["max_ns"] < baseline["min_ns"] or candidate["min_ns"] > baseline["max_ns"]
    conclusion = "inconclusive"
    if separated and abs(delta) > noise:
        conclusion = "faster" if delta < 0 else "slower"
    comparisons.append({"candidate": name, "baseline": base + "/" + control,
                        "median_change_percent": delta, "noise_percent": noise,
                        "sample_ranges_separated": separated, "timing_conclusion": conclusion})
output.mkdir(parents=True, exist_ok=True)
(output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
(output / "comparisons.json").write_text(json.dumps(comparisons, indent=2) + "\n")
