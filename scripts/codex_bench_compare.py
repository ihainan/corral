#!/usr/bin/env python3
"""Compare Codex parsing with one baseline source file via a Go overlay; never checkout files."""
import argparse
import json
from pathlib import Path
import re
import statistics
import subprocess
import sys
import tempfile

from codex_checks import ROOT, source_state


def measurements(output):
    rows = {}
    for line in output.splitlines():
        match = re.match(r"(BenchmarkCodexFeed/\S+)\s+\d+\s+([\d.]+) ns/op\s+[\d.]+ MB/s\s+(\d+) B/op\s+(\d+) allocs/op", line)
        if match:
            name = re.sub(r"-\d+$", "", match[1])
            rows.setdefault(name, []).append([float(match[2]), int(match[3]), int(match[4])])
    return {name: {"samples": len(samples), "ns_per_op": statistics.median(row[0] for row in samples),
                   "bytes_per_op": statistics.median(row[1] for row in samples),
                   "allocs_per_op": statistics.median(row[2] for row in samples)} for name, samples in rows.items()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", default="HEAD")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    report = {"kind": "synthetic-parser-benchmark-comparison", "source": source_state(), "baseline_ref": args.baseline,
              "baseline_scope": "internal/convo/codex.go only; identical benchmark and other source files", "passed": False}
    command = ["go", "test", "./internal/convo", "-run", "^$", "-bench", "^BenchmarkCodexFeed$", "-benchmem", "-benchtime=500ms", "-count=5", "-cpu=1"]
    try:
        with tempfile.TemporaryDirectory(prefix="corral-codex-bench-") as temp:
            baseline = Path(temp) / "codex.go"
            baseline.write_bytes(subprocess.check_output(["git", "show", args.baseline + ":internal/convo/codex.go"], cwd=ROOT))
            overlay = Path(temp) / "overlay.json"
            overlay.write_text(json.dumps({"Replace": {str(ROOT / "internal/convo/codex.go"): str(baseline)}}))
            for label, extra in (("before", ["-overlay", str(overlay)]), ("after", [])):
                result = subprocess.run(command[:2] + extra + command[2:], cwd=ROOT, capture_output=True, text=True, timeout=180)
                (args.output / (label + ".txt")).write_text(result.stdout + result.stderr)
                values = measurements(result.stdout)
                if result.returncode or len(values) != 4 or any(value["samples"] != 5 for value in values.values()):
                    raise RuntimeError("incomplete benchmark: " + label)
                report[label] = values
        report["changes"] = {name: {"time_percent": round((value["ns_per_op"] / report["before"][name]["ns_per_op"] - 1) * 100, 2),
                                    "allocation_bytes_percent": round((value["bytes_per_op"] / report["before"][name]["bytes_per_op"] - 1) * 100, 2)}
                             for name, value in report["after"].items()}
        report["passed"] = True
    except (OSError, RuntimeError, subprocess.SubprocessError) as error:
        report["error"] = str(error)
    (args.output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
