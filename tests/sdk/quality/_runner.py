"""Tiny test runner shared by all quality test files.

Each task's test file builds a list of (name, callable) cases and calls
run_cases(). Each callable should either return normally (pass) or raise
AssertionError (fail). The runner prints exactly one terminal line in the
format `SCORE: P/N` plus a list of failed case names, so the bash verifier
in compare-headless.sh can parse a single grep.
"""
from __future__ import annotations

import sys
import traceback
from typing import Callable, List, Tuple

Case = Tuple[str, Callable[[], None]]


def run_cases(cases: List[Case]) -> None:
    failed: List[Tuple[str, str]] = []
    for name, fn in cases:
        try:
            fn()
        except Exception as e:  # noqa: BLE001 — we want everything caught
            msg = type(e).__name__ + ": " + str(e)
            failed.append((name, msg))
    passed = len(cases) - len(failed)

    if failed:
        print("FAILED CASES:")
        for name, msg in failed:
            print(f"  - {name}: {msg[:120]}")
        if "COMPARE_VERBOSE" in __import__("os").environ:
            traceback.print_exc()
    # Last line is what the bash verifier + reviewer subagent parse.
    print(f"SCORE: {passed}/{len(cases)}")
    # Exit code: 0 on perfect, 1 on any failure. This way `--test-cmd` to the
    # plugin's reviewer signals "needs-changes" naturally on any missed case.
    sys.exit(0 if not failed else 1)
