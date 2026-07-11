"""Unit tests for config.toml parsing (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import unittest

from ccxbench.config import load


class TestConfig(unittest.TestCase):
    def test_run_fields(self) -> None:
        cfg = load()
        self.assertEqual(cfg.models, ("sonnet", "opus"))
        self.assertEqual(cfg.repeats, 5)
        self.assertEqual(cfg.max_turns, 60)
        self.assertEqual(cfg.safety_ceiling_usd, 800.0)

    def test_corpus_floor(self) -> None:
        self.assertEqual(load().min_traversal_bytes, 100_000)

    def test_tornado_repo_present(self) -> None:
        repos = {r.name: r for r in load().repos}
        self.assertIn("tornado", repos)
        self.assertEqual(repos["tornado"].ref, "v6.4.1")
        self.assertEqual(repos["tornado"].kind, "python")
        self.assertEqual(repos["tornado"].url, "https://github.com/tornadoweb/tornado")

    def test_cost_fields_removed(self) -> None:
        cfg = load()
        self.assertFalse(hasattr(cfg, "budget_usd"))
        self.assertFalse(hasattr(cfg, "cost_tolerance"))


if __name__ == "__main__":
    unittest.main()
