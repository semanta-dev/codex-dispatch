"""Graded checks for the small task (reverse_string)."""
import os, sys

sys.path.insert(0, os.path.dirname(__file__))
from _runner import run_cases

sys.path.insert(0, os.path.join(os.getcwd(), "src"))
import strings  # type: ignore  # noqa: E402


def basic():
    assert strings.reverse_string("abc") == "cba"


def empty():
    assert strings.reverse_string("") == ""


def single_char():
    assert strings.reverse_string("a") == "a"


def palindrome():
    assert strings.reverse_string("madam") == "madam"


def with_spaces():
    assert strings.reverse_string("hello world") == "dlrow olleh"


def with_newline():
    assert strings.reverse_string("a\nb") == "b\na"


def with_tab():
    assert strings.reverse_string("a\tb") == "b\ta"


def with_punctuation():
    assert strings.reverse_string("a,b!") == "!b,a"


def unicode_basic():
    # Latin-1 supplemental — should round-trip cleanly under any sane impl.
    assert strings.reverse_string("café") == "éfac"


def long_string():
    s = "abcdefghij" * 100
    assert strings.reverse_string(s) == s[::-1]


def does_not_mutate_argument():
    s = "abc"
    _ = strings.reverse_string(s)
    assert s == "abc"


def numeric_chars():
    assert strings.reverse_string("12345") == "54321"


run_cases([
    ("basic", basic),
    ("empty", empty),
    ("single_char", single_char),
    ("palindrome", palindrome),
    ("with_spaces", with_spaces),
    ("with_newline", with_newline),
    ("with_tab", with_tab),
    ("with_punctuation", with_punctuation),
    ("unicode_basic", unicode_basic),
    ("long_string", long_string),
    ("does_not_mutate_argument", does_not_mutate_argument),
    ("numeric_chars", numeric_chars),
])
