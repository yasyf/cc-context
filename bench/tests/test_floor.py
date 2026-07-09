"""Unit tests for the traversal-bytes size floor and goldgen build-time helpers (no API calls).

Run: cd bench && python -m unittest tests.test_floor
"""

from __future__ import annotations

import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

from ccxbench import goldgen
from ccxbench.goldgen import (
    FloorRow,
    floor_rows,
    go_funcs,
    make_patch,
    recompute_lc_predicate,
    resolve_decl_line,
    symbols_changed_by_patch,
    traversal_bytes,
)
from ccxbench.types import Grader, Task


def _task(tid: str, files: list[str], category: str = "navigation", repo: str = "r") -> Task:
    return Task(tid, category, repo, "p", {}, Grader("file_line"), {"traversal_files": files})


class TestTraversalBytes(unittest.TestCase):
    def test_sums_on_disk_file_sizes(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "a.py").write_text("x" * 1000)
            (root / "b.py").write_text("y" * 2500)
            self.assertEqual(traversal_bytes(root, _task("t", ["a.py", "b.py"])), 3500)

    def test_empty_traversal_is_zero(self) -> None:
        with TemporaryDirectory() as tmp:
            self.assertEqual(traversal_bytes(Path(tmp), _task("t", [])), 0)

    def test_missing_file_fails_loud(self) -> None:
        with TemporaryDirectory() as tmp:
            with self.assertRaises(FileNotFoundError):
                traversal_bytes(Path(tmp), _task("t", ["gone.py"]))


class TestFloorRows(unittest.TestCase):
    def test_over_floor_ok_under_floor_rejected(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "big.py").write_text("x" * 150_000)
            (root / "small.py").write_text("y" * 100)
            tasks = [_task("big", ["big.py"]), _task("small", ["small.py"])]
            rows = floor_rows(100_000, tasks, lambda _t: root)
            by_id = {r.task_id: r for r in rows}
            self.assertIsInstance(rows[0], FloorRow)
            self.assertEqual(by_id["big"].nbytes, 150_000)
            self.assertTrue(by_id["big"].ok)
            self.assertEqual(by_id["small"].nbytes, 100)
            self.assertFalse(by_id["small"].ok)

    def test_exactly_at_floor_clears_it(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "exact.py").write_text("x" * 100_000)
            (row,) = floor_rows(100_000, [_task("exact", ["exact.py"])], lambda _t: root)
            self.assertTrue(row.ok)

    def test_control_family_excluded_from_floor(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            control = Task("nr", "non_regression", "empty", "p", {}, Grader("keywords"), {})
            self.assertEqual(floor_rows(100_000, [control], lambda _t: root), [])


class TestGoldgenHelpers(unittest.TestCase):
    def test_module_exports_build_time_helpers(self) -> None:
        for name in ("go_funcs", "recompute_lc_predicate", "traversal_bytes", "floor_rows", "FloorRow"):
            self.assertTrue(hasattr(goldgen, name), f"goldgen missing {name}")

    def test_go_funcs_brace_scans_top_level_funcs(self) -> None:
        src = "func Add(a, b int) int {\n\treturn a + b\n}\n\nfunc (r *R) M() {\n\tAdd(1, 2)\n}\n"
        funcs = dict(go_funcs(src))
        self.assertIn("Add", funcs)
        self.assertIn("M", funcs)
        self.assertIn("Add(1, 2)", funcs["M"])

    def test_recompute_go_callers_from_checkout(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "x.go").write_text(
                "func addMatcher() {}\n\n"
                "func Uses() {\n\taddMatcher()\n}\n\n"
                "func Skips() {\n\treturn\n}\n"
            )
            pred = {"kind": "go_callers", "files": ["x.go"], "target": "addMatcher"}
            self.assertEqual(recompute_lc_predicate(root, pred, "r"), {"Uses"})

    def test_recompute_py_subclass_across_files(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "a.py").write_text("class Base:\n    pass\n\nclass Child(Base):\n    pass\n")
            (root / "b.py").write_text("import mod\n\nclass Far(mod.Base):\n    pass\n\nclass _Priv(Base):\n    pass\n")
            pred = {"kind": "py_subclass", "base": "Base", "files": ["a.py", "b.py"]}
            self.assertEqual(recompute_lc_predicate(root, pred, "r"), {"Child", "Far"})


class TestResolveDeclLine(unittest.TestCase):
    def test_unique_decl_resolves(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "m.py").write_text("import x\n\n\ndef target(a, b):\n    return a\n")
            self.assertEqual(resolve_decl_line(root, "m.py", "def target("), 4)

    def test_ambiguous_decl_fails_loud(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "m.py").write_text("def dup():\n    pass\n\ndef dup():\n    pass\n")
            with self.assertRaises(SystemExit):
                resolve_decl_line(root, "m.py", "def dup(")


class TestPatchGeneration(unittest.TestCase):
    def test_patch_and_attribution_roundtrip(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            src = (
                "class C:\n"
                "    def a(self):\n"
                "        return 1\n"
                "\n"
                "    def b(self):\n"
                "        return 2\n"
                "\n\n"
                "def top():\n"
                "    return 3\n"
            )
            (root / "m.py").write_text(src)
            edits = [
                {"file": "m.py", "find": "        return 1", "replace": "        return 11"},
                {"file": "m.py", "find": "    return 3", "replace": "    return 33"},
            ]
            patch, files = make_patch(root, edits)
            self.assertEqual(files, ["m.py"])
            self.assertIn("--- a/m.py", patch)
            self.assertIn("+++ b/m.py", patch)
            self.assertEqual(symbols_changed_by_patch(root, patch), {"a", "top"})

    def test_nonunique_find_fails_loud(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "m.py").write_text("def a():\n    x = 1\n    x = 1\n    return x\n")
            with self.assertRaises(SystemExit):
                make_patch(root, [{"file": "m.py", "find": "    x = 1", "replace": "    x = 2"}])


if __name__ == "__main__":
    unittest.main()
