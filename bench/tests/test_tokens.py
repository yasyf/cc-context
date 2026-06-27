"""Unit tests for the token-counting cache (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import os
import tempfile
import unittest
from pathlib import Path

from ccxbench.tokens import TokenCounter, api_count, local_count


class TestTokenCounter(unittest.TestCase):
    def test_miss_then_hit_calls_boundary_once(self) -> None:
        calls: list[tuple[str, str]] = []

        def fake(text: str, model: str) -> int:
            calls.append((text, model))
            return len(text)

        with tempfile.TemporaryDirectory() as tmp:
            counter = TokenCounter(model="m", count_fn=fake, cache_dir=Path(tmp))
            self.assertEqual(counter.count("hello"), 5)
            self.assertEqual(counter.count("hello"), 5)  # memoized
            self.assertEqual(len(calls), 1)

    def test_persisted_cache_survives_new_instance(self) -> None:
        calls: list[str] = []

        def fake(text: str, model: str) -> int:
            calls.append(text)
            return 42

        with tempfile.TemporaryDirectory() as tmp:
            cache = Path(tmp)
            first = TokenCounter(model="m", count_fn=fake, cache_dir=cache)
            self.assertEqual(first.count("x"), 42)

            second = TokenCounter(model="m", count_fn=fake, cache_dir=cache)
            self.assertEqual(second.count("x"), 42)  # read from disk, no second call
            self.assertEqual(len(calls), 1)

    def test_model_is_part_of_cache_key(self) -> None:
        calls: list[tuple[str, str]] = []

        def fake(text: str, model: str) -> int:
            calls.append((text, model))
            return len(model)

        with tempfile.TemporaryDirectory() as tmp:
            cache = Path(tmp)
            TokenCounter(model="a", count_fn=fake, cache_dir=cache).count("same")
            TokenCounter(model="bb", count_fn=fake, cache_dir=cache).count("same")
            self.assertEqual(len(calls), 2)  # different models -> different keys


class TestLocalCount(unittest.TestCase):
    def test_offline_and_deterministic(self) -> None:
        text = "def hello(name):\n    return f'hi {name}'\n"
        n = local_count(text, "any-model")
        self.assertGreater(n, 0)
        self.assertEqual(n, local_count(text, "any-model"))  # stable
        self.assertGreater(local_count(text * 4, "any-model"), n)  # monotonic in length

    def test_default_counter_needs_no_api_key(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            counter = TokenCounter(cache_dir=Path(tmp))  # default count_fn = local_count
            self.assertGreater(counter.count("hello world"), 0)

    @unittest.skipUnless(os.environ.get("ANTHROPIC_API_KEY"), "no ANTHROPIC_API_KEY: skip API fidelity check")
    def test_proxy_is_within_range_of_api(self) -> None:
        text = "def add(a, b):\n    return a + b\n" * 20
        proxy = local_count(text, TokenCounter().model)
        exact = api_count(text, TokenCounter().model)
        self.assertLess(abs(proxy - exact) / exact, 0.30)  # proxy within 30% of the API


if __name__ == "__main__":
    unittest.main()
