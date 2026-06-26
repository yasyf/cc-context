"""Unit tests for grade_set_match subset/superset semantics (no API calls).

Run: cd bench && python -m unittest tests.test_graders_subset -v
"""

import unittest
from pathlib import Path

from ccxbench.graders import GradeContext, grade_set_match


def ctx() -> GradeContext:
    return GradeContext(result_text="", workdir=Path("."))


class TestSetMatchSubset(unittest.TestCase):
    """subset mode: the answer must be a subset of gold (answer ⊆ gold)."""

    gold = {"items": ["alpha", "beta", "gamma"]}
    spec = {"field": "items", "mode": "subset", "lower": True}

    def test_proper_subset_passes(self):
        res = grade_set_match({"items": ["alpha", "beta"]}, self.gold, self.spec, ctx())
        self.assertTrue(res.correct, res.detail)

    def test_equal_set_passes(self):
        res = grade_set_match({"items": ["alpha", "beta", "gamma"]}, self.gold, self.spec, ctx())
        self.assertTrue(res.correct, res.detail)

    def test_empty_answer_passes(self):
        res = grade_set_match({"items": []}, self.gold, self.spec, ctx())
        self.assertTrue(res.correct, res.detail)

    def test_element_not_in_gold_fails(self):
        res = grade_set_match({"items": ["alpha", "delta"]}, self.gold, self.spec, ctx())
        self.assertFalse(res.correct, res.detail)

    def test_superset_of_gold_fails(self):
        res = grade_set_match(
            {"items": ["alpha", "beta", "gamma", "omega"]}, self.gold, self.spec, ctx()
        )
        self.assertFalse(res.correct, res.detail)


class TestSetMatchSuperset(unittest.TestCase):
    """Regression guard: superset mode is unchanged (gold ⊆ answer)."""

    gold = {"items": ["alpha", "beta"]}
    spec = {"field": "items", "mode": "superset", "lower": True}

    def test_answer_contains_all_gold_passes(self):
        res = grade_set_match(
            {"items": ["alpha", "beta", "gamma"]}, self.gold, self.spec, ctx()
        )
        self.assertTrue(res.correct, res.detail)

    def test_exact_gold_passes(self):
        res = grade_set_match({"items": ["alpha", "beta"]}, self.gold, self.spec, ctx())
        self.assertTrue(res.correct, res.detail)

    def test_missing_gold_element_fails(self):
        res = grade_set_match({"items": ["alpha"]}, self.gold, self.spec, ctx())
        self.assertFalse(res.correct, res.detail)


if __name__ == "__main__":
    unittest.main()
