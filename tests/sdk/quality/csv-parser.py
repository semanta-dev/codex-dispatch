"""Graded checks for the csv-parser task — RFC-4180 edges."""
import os, sys

sys.path.insert(0, os.path.dirname(__file__))
from _runner import run_cases

sys.path.insert(0, os.path.join(os.getcwd(), "src"))
from csvparser import parse_csv  # type: ignore  # noqa: E402


def simple_single_row():
    assert parse_csv("a,b,c") == [["a", "b", "c"]]


def two_rows_lf():
    assert parse_csv("a,b\nc,d") == [["a", "b"], ["c", "d"]]


def two_rows_crlf():
    assert parse_csv("a,b\r\nc,d") == [["a", "b"], ["c", "d"]]


def trailing_newline_no_extra_row():
    assert parse_csv("a,b\n") == [["a", "b"]]


def empty_fields():
    assert parse_csv("a,,c") == [["a", "", "c"]]


def all_empty_fields():
    assert parse_csv(",,") == [["", "", ""]]


def quoted_with_comma():
    assert parse_csv('"a,b",c') == [["a,b", "c"]]


def quoted_with_embedded_newline():
    assert parse_csv('"a\nb",c') == [["a\nb", "c"]]


def escaped_quote():
    # Two double-quotes inside a quoted field represent one literal "
    assert parse_csv('"a""b"') == [['a"b']]


def quote_only_field():
    # `""""` = one quoted field containing one literal "
    assert parse_csv('""""') == [['"']]


def empty_input():
    assert parse_csv("") == [] or parse_csv("") == [[""]], "empty input must return [] or [['']]"


def unicode_fields():
    assert parse_csv("café,naïve") == [["café", "naïve"]]


def multiple_quoted_with_specials():
    got = parse_csv('"hello, world","line1\nline2","quote""inside"')
    assert got == [["hello, world", "line1\nline2", 'quote"inside']], f"got {got!r}"


def long_input_many_rows():
    text = "\n".join(["a,b,c"] * 50)
    got = parse_csv(text)
    assert len(got) == 50
    assert all(row == ["a", "b", "c"] for row in got)


run_cases([
    ("simple_single_row", simple_single_row),
    ("two_rows_lf", two_rows_lf),
    ("two_rows_crlf", two_rows_crlf),
    ("trailing_newline_no_extra_row", trailing_newline_no_extra_row),
    ("empty_fields", empty_fields),
    ("all_empty_fields", all_empty_fields),
    ("quoted_with_comma", quoted_with_comma),
    ("quoted_with_embedded_newline", quoted_with_embedded_newline),
    ("escaped_quote", escaped_quote),
    ("quote_only_field", quote_only_field),
    ("empty_input", empty_input),
    ("unicode_fields", unicode_fields),
    ("multiple_quoted_with_specials", multiple_quoted_with_specials),
    ("long_input_many_rows", long_input_many_rows),
])
