"""Graded checks for the merge-intervals task."""
import os, sys, copy

sys.path.insert(0, os.path.dirname(__file__))
from _runner import run_cases

sys.path.insert(0, os.path.join(os.getcwd(), "src"))
from intervals import merge_intervals  # type: ignore  # noqa: E402


def empty_input():
    assert merge_intervals([]) == []


def single_interval():
    got = merge_intervals([("2026-01-01", "2026-01-05")])
    assert got == [("2026-01-01", "2026-01-05")], f"got {got!r}"


def two_disjoint():
    got = merge_intervals([
        ("2026-01-01", "2026-01-05"),
        ("2026-02-01", "2026-02-05"),
    ])
    assert len(got) == 2


def two_overlapping():
    got = merge_intervals([
        ("2026-01-01", "2026-01-10"),
        ("2026-01-05", "2026-01-15"),
    ])
    assert got == [("2026-01-01", "2026-01-15")], f"got {got!r}"


def two_adjacent_day_apart():
    # Adjacent: first ends 2026-01-09, second starts 2026-01-10 → merge.
    got = merge_intervals([
        ("2026-01-01", "2026-01-09"),
        ("2026-01-10", "2026-01-15"),
    ])
    assert got == [("2026-01-01", "2026-01-15")], f"adjacent must merge: got {got!r}"


def almost_adjacent_two_day_gap():
    # Gap of 2 days → should NOT merge.
    got = merge_intervals([
        ("2026-01-01", "2026-01-09"),
        ("2026-01-11", "2026-01-15"),
    ])
    assert len(got) == 2, f"2-day gap must not merge: got {got!r}"


def fully_contained():
    got = merge_intervals([
        ("2026-01-01", "2026-01-31"),
        ("2026-01-10", "2026-01-20"),
    ])
    assert got == [("2026-01-01", "2026-01-31")], f"got {got!r}"


def unsorted_input():
    got = merge_intervals([
        ("2026-02-01", "2026-02-05"),
        ("2026-01-01", "2026-01-10"),
    ])
    assert got[0][0] == "2026-01-01", f"must sort by start: got {got!r}"


def duplicate_intervals():
    got = merge_intervals([
        ("2026-01-01", "2026-01-10"),
        ("2026-01-01", "2026-01-10"),
    ])
    assert got == [("2026-01-01", "2026-01-10")], f"got {got!r}"


def month_boundary_adjacent():
    # Jan 31 ends, Feb 1 starts — adjacent across month boundary.
    got = merge_intervals([
        ("2026-01-15", "2026-01-31"),
        ("2026-02-01", "2026-02-10"),
    ])
    assert got == [("2026-01-15", "2026-02-10")], f"got {got!r}"


def year_boundary_adjacent():
    got = merge_intervals([
        ("2025-12-15", "2025-12-31"),
        ("2026-01-01", "2026-01-10"),
    ])
    assert got == [("2025-12-15", "2026-01-10")], f"got {got!r}"


def leap_year_boundary():
    # 2024 is a leap year — Feb 29 exists.
    got = merge_intervals([
        ("2024-02-25", "2024-02-29"),
        ("2024-03-01", "2024-03-05"),
    ])
    assert got == [("2024-02-25", "2024-03-05")], f"got {got!r}"


def does_not_mutate_argument():
    arg = [
        ("2026-01-01", "2026-01-10"),
        ("2026-01-05", "2026-01-15"),
    ]
    snapshot = copy.deepcopy(arg)
    _ = merge_intervals(arg)
    assert arg == snapshot, "must not mutate input"


def three_overlapping_chain():
    got = merge_intervals([
        ("2026-01-01", "2026-01-05"),
        ("2026-01-04", "2026-01-08"),
        ("2026-01-07", "2026-01-12"),
    ])
    assert got == [("2026-01-01", "2026-01-12")], f"got {got!r}"


def single_day_intervals_adjacent():
    # Two single-day intervals on consecutive days — endpoints inclusive, so
    # 2026-01-01 ends day before 2026-01-02 starts → merge.
    got = merge_intervals([
        ("2026-01-01", "2026-01-01"),
        ("2026-01-02", "2026-01-02"),
    ])
    assert got == [("2026-01-01", "2026-01-02")], f"got {got!r}"


run_cases([
    ("empty_input", empty_input),
    ("single_interval", single_interval),
    ("two_disjoint", two_disjoint),
    ("two_overlapping", two_overlapping),
    ("two_adjacent_day_apart", two_adjacent_day_apart),
    ("almost_adjacent_two_day_gap", almost_adjacent_two_day_gap),
    ("fully_contained", fully_contained),
    ("unsorted_input", unsorted_input),
    ("duplicate_intervals", duplicate_intervals),
    ("month_boundary_adjacent", month_boundary_adjacent),
    ("year_boundary_adjacent", year_boundary_adjacent),
    ("leap_year_boundary", leap_year_boundary),
    ("does_not_mutate_argument", does_not_mutate_argument),
    ("three_overlapping_chain", three_overlapping_chain),
    ("single_day_intervals_adjacent", single_day_intervals_adjacent),
])
