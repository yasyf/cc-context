"""Unit tests for shared typed records (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import unittest

from ccxbench.types import Grader, Task


def make_task(gold: dict) -> Task:
    return Task(
        id="t",
        category="navigation",
        repo="tornado",
        prompt="p",
        schema={},
        grader=Grader("file_line"),
        gold=gold,
    )


class TestTaskTraversalFiles(unittest.TestCase):
    def test_reads_from_gold(self) -> None:
        t = make_task({"answer": 1, "traversal_files": ["tornado/web.py", "tornado/httputil.py"]})
        self.assertEqual(t.traversal_files, ("tornado/web.py", "tornado/httputil.py"))

    def test_defaults_empty_when_absent(self) -> None:
        # non_regression tasks have no traversal floor; absence is fine.
        self.assertEqual(make_task({"answer": 1}).traversal_files, ())

    def test_roundtrip_preserves_gold_traversal(self) -> None:
        gold = {"answer": 1, "traversal_files": ["a.py"]}
        t = Task.from_dict(make_task(gold).to_dict())
        self.assertEqual(t.traversal_files, ("a.py",))


if __name__ == "__main__":
    unittest.main()
