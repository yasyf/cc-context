"""Offline tests for the Layer-1 micro-benchmark scoring and aggregation.

The fake counter (`len(text) // 4`) stands in for the count-tokens API so no network
call happens; ccx and the raw tools are never invoked — pre-captured output strings are
fed straight into the scoring function.
"""

from __future__ import annotations

import argparse
import unittest

from ccxbench import microbench
from ccxbench.microbench import Pair, Result, Row, cmd_microbench, format_table, score_pairs


def fake_count(text: str) -> int:
    return len(text) // 4


class TestScorePairs(unittest.TestCase):
    def test_ccx_smaller_yields_savings_and_ok(self) -> None:
        pair = Pair(
            intent="understand file",
            target="repo/big.go",
            raw_text="x" * 400,
            ccx_text="y" * 100,
        )
        result = score_pairs([pair], fake_count)
        (row,) = result.rows
        self.assertEqual(row.raw_tokens, 100)
        self.assertEqual(row.ccx_tokens, 25)
        self.assertTrue(row.ok)
        self.assertAlmostEqual(row.savings_pct, 75.0)
        self.assertTrue(result.all_ok)
        self.assertEqual(result.violations, ())

    def test_ccx_larger_yields_violation(self) -> None:
        pair = Pair(
            intent="find pattern",
            target="repo:needle",
            raw_text="x" * 40,
            ccx_text="y" * 400,
        )
        result = score_pairs([pair], fake_count)
        (row,) = result.rows
        self.assertEqual(row.raw_tokens, 10)
        self.assertEqual(row.ccx_tokens, 100)
        self.assertFalse(row.ok)
        self.assertLess(row.savings_pct, 0)
        self.assertFalse(result.all_ok)
        self.assertEqual(result.violations, (row,))

    def test_equal_tokens_is_ok(self) -> None:
        pair = Pair(intent="enumerate files", target="repo:*.go", raw_text="abcd", ccx_text="wxyz")
        result = score_pairs([pair], fake_count)
        (row,) = result.rows
        self.assertEqual(row.raw_tokens, row.ccx_tokens)
        self.assertTrue(row.ok)
        self.assertEqual(row.savings_pct, 0.0)

    def test_aggregate_reports_violation_among_wins(self) -> None:
        win = Pair("understand file", "repo/a.go", "x" * 800, "y" * 80)
        loss = Pair("find pattern", "repo:n", "x" * 40, "y" * 400)
        result = score_pairs([win, loss], fake_count)
        self.assertEqual(len(result.rows), 2)
        self.assertEqual(len(result.violations), 1)
        self.assertEqual(result.violations[0].intent, "find pattern")
        # 200 raw vs 120 ccx total -> still net savings, but not all_ok.
        self.assertEqual(result.total_raw, 210)
        self.assertEqual(result.total_ccx, 120)
        self.assertFalse(result.all_ok)
        self.assertGreater(result.overall_savings_pct, 0)


class TestResultProperties(unittest.TestCase):
    def test_zero_raw_savings_is_zero(self) -> None:
        row = Row(intent="i", target="t", raw_tokens=0, ccx_tokens=0)
        self.assertEqual(row.savings_pct, 0.0)
        self.assertTrue(row.ok)
        empty = Result(rows=())
        self.assertEqual(empty.overall_savings_pct, 0.0)
        self.assertTrue(empty.all_ok)


class TestFormatTable(unittest.TestCase):
    def test_table_marks_violation_and_summary(self) -> None:
        win = Pair("understand file", "repo/a.go", "x" * 800, "y" * 80)
        loss = Pair("find pattern", "repo:needle", "x" * 40, "y" * 400)
        table = format_table(score_pairs([win, loss], fake_count))
        self.assertIn("understand file", table)
        self.assertIn("find pattern", table)
        self.assertIn("FAIL", table)
        self.assertIn("1 violation(s)", table)


class FakeCounter:
    def count(self, text: str) -> int:
        return fake_count(text)


class TestCmdMicrobench(unittest.TestCase):
    def _run(self, pairs: list[Pair]) -> int:
        orig_build = microbench.build_pairs
        orig_counter = microbench.default_counter
        microbench.build_pairs = lambda cfg, repos: pairs
        microbench.default_counter = lambda: FakeCounter()
        try:
            args = argparse.Namespace(repo=None)
            return cmd_microbench(cfg=None, args=args)
        finally:
            microbench.build_pairs = orig_build
            microbench.default_counter = orig_counter

    def test_all_ok_returns_zero(self) -> None:
        pairs = [
            Pair("understand file", "repo/a.go", "x" * 800, "y" * 80),
            Pair("read region", "repo/a.go 1-40", "x" * 400, "y" * 100),
        ]
        self.assertEqual(self._run(pairs), 0)

    def test_violation_returns_nonzero(self) -> None:
        pairs = [
            Pair("understand file", "repo/a.go", "x" * 800, "y" * 80),
            Pair("find pattern", "repo:needle", "x" * 40, "y" * 400),
        ]
        self.assertEqual(self._run(pairs), 1)


if __name__ == "__main__":
    unittest.main()
