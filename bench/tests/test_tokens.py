"""Unit tests for the token-counting cache (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from ccxbench.tokens import TokenCounter


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


if __name__ == "__main__":
    unittest.main()
