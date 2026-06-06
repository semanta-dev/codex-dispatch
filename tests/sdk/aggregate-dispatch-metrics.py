#!/usr/bin/env python3
"""Aggregate persistent worktree-dispatch metrics.

Inputs may be worktree run directories or stdout files produced by
graphrag-worktree-dispatch.sh. The script reads dispatch-result.json and
dispatch-stdout.log from each selected run directory.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import statistics
import sys
from typing import Any


def run_dir_from_stdout(path: pathlib.Path) -> pathlib.Path | None:
    last = ""
    for line in path.read_text(errors="ignore").splitlines():
        line = line.strip()
        if line:
            last = line
    if not last:
        return None
    candidate = pathlib.Path(last)
    if candidate.is_dir():
        return candidate
    return None


def coerce_run_dir(path: pathlib.Path) -> pathlib.Path | None:
    if path.is_dir():
        return path
    if path.is_file():
        return run_dir_from_stdout(path)
    return None


def read_json(path: pathlib.Path) -> dict[str, Any]:
    if not path.exists():
        return {}
    try:
        return json.loads(path.read_text(errors="ignore"))
    except json.JSONDecodeError:
        return {}


def read_log_metrics(path: pathlib.Path) -> dict[str, int]:
    metrics = {
        "input_tokens": 0,
        "cached_tokens": 0,
        "output_tokens": 0,
        "reasoning_tokens": 0,
        "duration_ms": 0,
    }
    if not path.exists():
        return metrics

    last_tokens = {
        "input_tokens": 0,
        "cached_tokens": 0,
        "output_tokens": 0,
        "reasoning_tokens": 0,
    }
    for line in path.read_text(errors="ignore").splitlines():
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        method = msg.get("method")
        params = msg.get("params") or {}
        if method == "thread/tokenUsage/updated":
            usage = ((params.get("tokenUsage") or {}).get("total") or {})
            last_tokens["input_tokens"] = int(usage.get("inputTokens") or 0)
            last_tokens["cached_tokens"] = int(usage.get("cachedInputTokens") or 0)
            last_tokens["output_tokens"] = int(usage.get("outputTokens") or 0)
            last_tokens["reasoning_tokens"] = int(usage.get("reasoningOutputTokens") or 0)
        elif method == "turn/completed":
            turn = params.get("turn") or {}
            metrics["duration_ms"] += int(turn.get("durationMs") or 0)

    metrics.update(last_tokens)
    return metrics


def percentile(values: list[float], pct: float) -> float | None:
    if not values:
        return None
    if len(values) == 1:
        return values[0]
    idx = round((pct / 100) * (len(values) - 1))
    return sorted(values)[idx]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("paths", nargs="+", help="run directories or dispatcher stdout files")
    parser.add_argument("--output", help="write JSON summary to this path")
    args = parser.parse_args()

    runs = []
    seen: set[pathlib.Path] = set()
    for raw in args.paths:
        run_dir = coerce_run_dir(pathlib.Path(raw))
        if run_dir is None:
            print(f"warning: could not resolve run dir from {raw}", file=sys.stderr)
            continue
        run_dir = run_dir.resolve()
        if run_dir in seen:
            continue
        seen.add(run_dir)

        result = read_json(run_dir / "dispatch-result.json")
        log_metrics = read_log_metrics(run_dir / "dispatch-stdout.log")
        run = {
            "run_dir": str(run_dir),
            "exit_code": int(result.get("exit_code", 64)),
            "session_id": result.get("session_id", ""),
            "files_changed": result.get("files_changed", []),
            "lines_added": int(result.get("lines_added", 0) or 0),
            "lines_removed": int(result.get("lines_removed", 0) or 0),
            **log_metrics,
        }
        runs.append(run)

    totals = {
        "runs": len(runs),
        "passed": sum(1 for run in runs if run["exit_code"] == 0),
        "input_tokens": sum(run["input_tokens"] for run in runs),
        "cached_tokens": sum(run["cached_tokens"] for run in runs),
        "output_tokens": sum(run["output_tokens"] for run in runs),
        "reasoning_tokens": sum(run["reasoning_tokens"] for run in runs),
        "duration_ms": sum(run["duration_ms"] for run in runs),
        "lines_added": sum(run["lines_added"] for run in runs),
        "lines_removed": sum(run["lines_removed"] for run in runs),
    }
    durations = [run["duration_ms"] for run in runs if run["duration_ms"]]
    totals["pass_rate"] = (totals["passed"] / totals["runs"]) if totals["runs"] else 0
    totals["duration_ms_p50"] = percentile(durations, 50)
    totals["duration_ms_p95"] = percentile(durations, 95)
    totals["duration_ms_mean"] = statistics.mean(durations) if durations else None

    summary = {"totals": totals, "runs": runs}
    text = json.dumps(summary, indent=2) + "\n"
    if args.output:
        pathlib.Path(args.output).write_text(text)
    print(text, end="")
    return 0 if runs else 1


if __name__ == "__main__":
    raise SystemExit(main())
