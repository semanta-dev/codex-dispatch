"""Graded checks for the medium task (Stack)."""
import os, sys

sys.path.insert(0, os.path.dirname(__file__))
from _runner import run_cases

sys.path.insert(0, os.path.join(os.getcwd(), "src"))
from stack import Stack  # type: ignore  # noqa: E402


def fresh_is_empty():
    s = Stack()
    assert s.is_empty()
    assert s.size() == 0


def push_increases_size():
    s = Stack()
    s.push(1)
    assert s.size() == 1
    assert not s.is_empty()


def push_many_then_size():
    s = Stack()
    for i in range(100):
        s.push(i)
    assert s.size() == 100


def peek_returns_top():
    s = Stack()
    s.push("a")
    s.push("b")
    assert s.peek() == "b"


def peek_does_not_remove():
    s = Stack()
    s.push("a")
    _ = s.peek()
    _ = s.peek()
    assert s.size() == 1


def pop_returns_top():
    s = Stack()
    s.push(1)
    s.push(2)
    assert s.pop() == 2


def pop_reduces_size():
    s = Stack()
    s.push(1)
    s.push(2)
    s.pop()
    assert s.size() == 1


def lifo_order():
    s = Stack()
    for i in range(5):
        s.push(i)
    out = [s.pop() for _ in range(5)]
    assert out == [4, 3, 2, 1, 0]


def pop_on_empty_raises_index_error():
    s = Stack()
    try:
        s.pop()
        raise AssertionError("expected IndexError")
    except IndexError:
        pass


def push_none_is_allowed():
    s = Stack()
    s.push(None)
    assert s.size() == 1
    assert s.peek() is None
    assert s.pop() is None


def push_then_pop_full_cycle_keeps_size_zero():
    s = Stack()
    for _ in range(1000):
        s.push(1)
        s.pop()
    assert s.size() == 0
    assert s.is_empty()


def mixed_operations():
    s = Stack()
    s.push(1)
    s.push(2)
    s.pop()       # remove 2
    s.push(3)
    s.push(4)
    s.pop()       # remove 4
    assert s.peek() == 3
    assert s.size() == 2


run_cases([
    ("fresh_is_empty", fresh_is_empty),
    ("push_increases_size", push_increases_size),
    ("push_many_then_size", push_many_then_size),
    ("peek_returns_top", peek_returns_top),
    ("peek_does_not_remove", peek_does_not_remove),
    ("pop_returns_top", pop_returns_top),
    ("pop_reduces_size", pop_reduces_size),
    ("lifo_order", lifo_order),
    ("pop_on_empty_raises_index_error", pop_on_empty_raises_index_error),
    ("push_none_is_allowed", push_none_is_allowed),
    ("push_then_pop_full_cycle_keeps_size_zero", push_then_pop_full_cycle_keeps_size_zero),
    ("mixed_operations", mixed_operations),
])
