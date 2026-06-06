#!/usr/bin/env python3
"""Run a benchmark command repeatedly and summarize reliability/timing.

Example:
  tests/sdk/repeat-command-benchmark.py --runs 10 --out /tmp/bench -- make test
"""

from __future__ import annotations

import argparse
import json
import pathlib
import statistics
import subprocess
import sys
import time


def percentile(values: list[float], pct: float) -> float | None:
    if not values:
        return None
    if len(values) == 1:
        return values[0]
    ordered = sorted(values)
    idx = round((pct / 100) * (len(ordered) - 1))
    return ordered[idx]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runs", type=int, default=10)
    parser.add_argument("--out", required=True, help="directory for run artifacts and summary.json")
    parser.add_argument("--label", default="benchmark")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()

    if args.runs < 1:
        parser.error("--runs must be >= 1")
    if not args.command or args.command[0] != "--" or len(args.command) == 1:
        parser.error("pass command after --")
    command = args.command[1:]
    out_dir = pathlib.Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)

    records = []
    for index in range(1, args.runs + 1):
        started = time.monotonic()
        run_dir = out_dir / f"run-{index:03d}"
        run_dir.mkdir(exist_ok=True)
        with (run_dir / "stdout.log").open("wb") as stdout, (run_dir / "stderr.log").open("wb") as stderr:
            proc = subprocess.run(command, stdout=stdout, stderr=stderr)
        wall_s = time.monotonic() - started
        records.append(
            {
                "index": index,
                "exit_code": proc.returncode,
                "wall_s": round(wall_s, 3),
                "stdout_path": str(run_dir / "stdout.log"),
                "stderr_path": str(run_dir / "stderr.log"),
            }
        )

    walls = [record["wall_s"] for record in records]
    passed = sum(1 for record in records if record["exit_code"] == 0)
    summary = {
        "label": args.label,
        "command": command,
        "runs": len(records),
        "passed": passed,
        "pass_rate": passed / len(records),
        "wall_s_min": min(walls),
        "wall_s_p50": percentile(walls, 50),
        "wall_s_p95": percentile(walls, 95),
        "wall_s_mean": round(statistics.mean(walls), 3),
        "records": records,
    }
    (out_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))
    return 0 if passed == len(records) else 1


if __name__ == "__main__":
    raise SystemExit(main())
