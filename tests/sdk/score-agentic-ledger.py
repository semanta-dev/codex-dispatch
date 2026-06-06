#!/usr/bin/env python3
"""Score the ultra-hard agentic-ledger design doc with a synonym-aware rubric."""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path


RUBRIC = [
    ("goals/non-goals", ["goal"], ["non-goal", "does not", "deliberately avoids", "out of scope"]),
    ("personas/workflows", ["persona", "workflow"]),
    ("domain model", ["task", "session", "attempt", "review", "artifact"]),
    ("state machines", ["state", "transition"]),
    ("storage tradeoffs", ["event-sourced", "relational", "hybrid", "recommend"]),
    ("api surface", ["api", "create", "claim", "append", "artifact", "review", "query", "subscribe"]),
    ("concurrency", ["idempotency", "optimistic", "lease", "conflict"]),
    ("review gates", ["review", "quality", "test", "acceptance", "security"]),
    ("scheduling", ["scheduling", "fairness", "backpressure", "cost"]),
    ("failure recovery", ["failure", "recovery", "crash", "timeout"]),
    ("observability", ["metrics", "logs", "traces", "slo", "alert"]),
    ("security/privacy", ["security", "privacy", "secret", "access", "retention"]),
    ("migration", ["migration"]),
    ("rollout", ["rollout"]),
    ("open questions", ["open question"]),
]


def present(text: str, alternatives: list[str]) -> bool:
    return any(needle in text for needle in alternatives)


def score(path: Path) -> dict[str, object]:
    exists = path.exists()
    text = path.read_text(errors="ignore") if exists else ""
    low = text.lower()
    missing: list[str] = []
    for name, *groups in RUBRIC:
        if not all(present(low, group) for group in groups):
            missing.append(name)
    return {
        "exists": exists,
        "words": len(re.findall(r"\S+", text)),
        "headings": len(re.findall(r"(?m)^#{1,6}\s+", text)),
        "tables": text.count("|"),
        "coverage": len(RUBRIC) - len(missing),
        "coverage_total": len(RUBRIC),
        "missing": missing,
        "chars": len(text),
    }


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: score-agentic-ledger.py DOC", file=sys.stderr)
        return 2
    print(json.dumps(score(Path(sys.argv[1])), indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
