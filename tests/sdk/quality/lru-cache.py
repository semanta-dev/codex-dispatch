"""Graded checks for the lru-cache task."""
import os, sys

sys.path.insert(0, os.path.dirname(__file__))
from _runner import run_cases

sys.path.insert(0, os.path.join(os.getcwd(), "src"))
from lru import LRUCache  # type: ignore  # noqa: E402


def basic_put_get():
    c = LRUCache(2)
    c.put(1, "a")
    assert c.get(1) == "a"


def get_missing_returns_neg1():
    c = LRUCache(2)
    assert c.get(99) == -1


def capacity_eviction():
    c = LRUCache(2)
    c.put(1, "a")
    c.put(2, "b")
    c.put(3, "c")  # evicts 1
    assert c.get(1) == -1
    assert c.get(2) == "b"
    assert c.get(3) == "c"


def get_is_recency_touch():
    c = LRUCache(2)
    c.put(1, "a")
    c.put(2, "b")
    assert c.get(1) == "a"   # 1 is now MRU
    c.put(3, "c")            # evicts 2 (LRU), not 1
    assert c.get(2) == -1
    assert c.get(1) == "a"


def put_existing_key_does_not_evict():
    c = LRUCache(2)
    c.put(1, "a")
    c.put(2, "b")
    c.put(1, "updated")      # update, no eviction
    assert c.get(2) == "b"
    assert c.get(1) == "updated"


def put_existing_is_recency_touch():
    c = LRUCache(2)
    c.put(1, "a")
    c.put(2, "b")
    c.put(1, "updated")      # 1 becomes MRU
    c.put(3, "c")            # should evict 2
    assert c.get(2) == -1
    assert c.get(1) == "updated"
    assert c.get(3) == "c"


def capacity_one():
    c = LRUCache(1)
    c.put(1, "a")
    c.put(2, "b")            # evicts 1
    assert c.get(1) == -1
    assert c.get(2) == "b"


def no_eviction_below_capacity():
    c = LRUCache(10)
    for i in range(5):
        c.put(i, str(i))
    for i in range(5):
        assert c.get(i) == str(i)


def value_can_be_none():
    c = LRUCache(2)
    c.put(1, None)
    assert c.get(1) is None
    assert c.get(99) == -1   # must still distinguish missing from None


def long_workload_eviction_order():
    c = LRUCache(3)
    # insert 1, 2, 3; touch 1; insert 4 — should evict 2 not 1
    c.put(1, "a")
    c.put(2, "b")
    c.put(3, "c")
    c.get(1)                 # 1 → MRU
    c.put(4, "d")            # evicts 2 (LRU)
    assert c.get(2) == -1
    assert c.get(1) == "a"
    assert c.get(3) == "c"
    assert c.get(4) == "d"


def repeated_put_same_key():
    c = LRUCache(2)
    for i in range(100):
        c.put(1, i)
    assert c.get(1) == 99


run_cases([
    ("basic_put_get", basic_put_get),
    ("get_missing_returns_neg1", get_missing_returns_neg1),
    ("capacity_eviction", capacity_eviction),
    ("get_is_recency_touch", get_is_recency_touch),
    ("put_existing_key_does_not_evict", put_existing_key_does_not_evict),
    ("put_existing_is_recency_touch", put_existing_is_recency_touch),
    ("capacity_one", capacity_one),
    ("no_eviction_below_capacity", no_eviction_below_capacity),
    ("value_can_be_none", value_can_be_none),
    ("long_workload_eviction_order", long_workload_eviction_order),
    ("repeated_put_same_key", repeated_put_same_key),
])
