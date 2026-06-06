"""Graded checks for the trivial task (hello.txt)."""
import os, sys

sys.path.insert(0, os.path.dirname(__file__))
from _runner import run_cases


def exact_content():
    with open("hello.txt") as f:
        assert f.read() == "Hello, World!"


def is_single_line():
    with open("hello.txt") as f:
        text = f.read()
    # The prompt says "single line, no extra whitespace" — strict.
    assert "\n" not in text or text == "Hello, World!\n", f"extra content: {text!r}"


def no_trailing_whitespace():
    with open("hello.txt") as f:
        text = f.read().rstrip("\n")
    assert text == text.rstrip(), f"trailing whitespace in line: {text!r}"


def correct_punctuation():
    with open("hello.txt") as f:
        text = f.read().strip()
    assert "," in text and "!" in text, f"missing punctuation: {text!r}"


def file_exists():
    assert os.path.exists("hello.txt")


run_cases([
    ("file_exists", file_exists),
    ("exact_content", exact_content),
    ("is_single_line", is_single_line),
    ("no_trailing_whitespace", no_trailing_whitespace),
    ("correct_punctuation", correct_punctuation),
])
