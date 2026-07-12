"""Unit tests for the traversal-bytes size floor and goldgen build-time helpers (no API calls).

Run: cd bench && python -m unittest tests.test_floor
"""

from __future__ import annotations

import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

from ccxbench import goldgen, taskgen
from ccxbench.config import load
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

    def test_diagnostic_family_excluded_from_floor(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            diag = Task("d", "large_context_diag", "r", "p", {}, Grader("set_match"), {"traversal_files": []})
            self.assertEqual(floor_rows(100_000, [diag], lambda _t: root), [])

    def test_floor_exempt_passes_under_floor(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "small.py").write_text("y" * 100)
            t = Task("x", "large_context", "r", "p", {}, Grader("set_match"),
                     {"traversal_files": ["small.py"]}, floor_exempt=True)
            (row,) = floor_rows(100_000, [t], lambda _t: root)
            self.assertTrue(row.ok)
            self.assertTrue(row.exempt)
            self.assertEqual(row.nbytes, 100)


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


class TestFloodPredicates(unittest.TestCase):
    """Stage-1 predicate mechanics: py_method over a file list/glob with excludes, and the transitive
    py_subclass_closure. Synthetic cases pin the logic; TestFloodCountsOnCheckout pins the counts."""

    def test_py_method_single_file_backcompat(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "m.py").write_text("class A:\n    def go(self): pass\n\nclass _P:\n    def go(self): pass\n")
            pred = {"kind": "py_method", "file": "m.py", "target": "go"}
            self.assertEqual(recompute_lc_predicate(root, pred, "r"), {"A"})

    def test_py_method_multi_file_union(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "a.py").write_text("class A:\n    def f(self): pass\n")
            (root / "b.py").write_text("class B:\n    def f(self): pass\n\nclass C:\n    def g(self): pass\n")
            pred = {"kind": "py_method", "files": ["a.py", "b.py"], "target": "f"}
            self.assertEqual(recompute_lc_predicate(root, pred, "r"), {"A", "B"})

    def test_py_method_glob_minus_exclude(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "pkg").mkdir()
            (root / "pkg" / "x.py").write_text("class X:\n    def close(self): pass\n")
            (root / "pkg" / "sub").mkdir()
            (root / "pkg" / "sub" / "y.py").write_text("class Y:\n    def close(self): pass\n")
            (root / "pkg" / "test").mkdir()
            (root / "pkg" / "test" / "t.py").write_text("class T:\n    def close(self): pass\n")
            pred = {"kind": "py_method", "files": ["pkg/**/*.py"], "exclude": ["pkg/test/*"], "target": "close"}
            # `pkg/**/*.py` spans x.py and sub/y.py; `pkg/test/*` (fnmatch, `*` spans `/`) drops the test tree.
            self.assertEqual(recompute_lc_predicate(root, pred, "r"), {"X", "Y"})

    def test_py_subclass_closure_transitive_dotted_and_public(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "a.py").write_text(
                "class Base: pass\n"
                "class Mid(Base): pass\n"          # direct subclass
                "class Leaf(mod.Mid): pass\n"      # transitive via a dotted base
                "class _Priv(Base): pass\n"        # non-public: excluded
                "class Other(Unrelated): pass\n"   # unrelated: excluded
            )
            pred = {"kind": "py_subclass_closure", "base": "Base", "files": ["a.py"]}
            # The closure excludes Base itself (a class is not its own subclass) and _Priv (non-public).
            self.assertEqual(recompute_lc_predicate(root, pred, "r"), {"Mid", "Leaf"})

    def test_py_import_closure_transitive_first_party_minus_seed_and_root(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "pkg").mkdir()
            (root / "pkg" / "__init__.py").write_text("")
            (root / "pkg" / "seed.py").write_text(
                "import os\n"                       # stdlib: not counted
                "import pkg.direct\n"               # direct submodule
                "from pkg import viafrom\n"         # `from pkg import X` -> pkg.viafrom
                "def f():\n"
                "    from pkg.infunc import g\n"     # import inside a function: counted
            )
            (root / "pkg" / "direct.py").write_text(
                "from pkg.deep import h\n"          # transitive
                "from pkg.ext import mask\n"        # C-extension module: has no .py but is a member
            )
            (root / "pkg" / "viafrom.py").write_text("x = 1\n")
            (root / "pkg" / "infunc.py").write_text("x = 1\n")
            (root / "pkg" / "deep.py").write_text("x = 1\n")
            (root / "pkg" / "unused.py").write_text("x = 1\n")                    # never imported
            (root / "pkg" / "ext.pyi").write_text("def mask() -> int: ...\n")     # stub only; no .py source
            pred = {"kind": "py_import_closure", "seed": "pkg.seed", "files": ["pkg/**/*.py"]}
            # Reaches direct, viafrom, infunc, transitively deep, and the source-less extension pkg.ext.
            # Excludes the seed, the bare `pkg` root, stdlib (`os`), and the never-imported `pkg.unused`.
            self.assertEqual(
                recompute_lc_predicate(root, pred, "r"),
                {"pkg.direct", "pkg.viafrom", "pkg.infunc", "pkg.deep", "pkg.ext"},
            )


class TestFloodCountsOnCheckout(unittest.TestCase):
    """Each Stage-1 flood predicate recomputes to its pinned member count on the real checkout —
    a regression guard against predicate drift (and the record that T6 is 8, not the doc's 6)."""

    def test_member_counts_match_pins(self) -> None:
        cfg = load()
        expect = {
            "flood-t1-click-convert": 11,
            "flood-t2-click-to-info-dict": 13,
            "flood-t3-tornado-close": 20,
            "flood-t4-tornado-initialize": 19,
            "flood-t5-tornado-configurable": 17,
            "flood-t6-mux-matcher": 8,
            # T5-family iteration. The import closures count every referenced first-party tornado.*
            # module (root excluded), including the C-extension tornado.speedups (no .py) → 21/15/19.
            "flood-t5b-click-paramtype": 15,
            "flood-t5c-tornado-web-imports": 21,
            "flood-t5d-tornado-httpserver-imports": 15,
            "flood-t5e-tornado-websocket-imports": 19,
        }
        tasks = {t.id: t for t in taskgen.large_context_tasks()}
        for tid, n in expect.items():
            t = tasks[tid]
            if not (cfg.fixtures_root / t.repo).is_dir():
                self.skipTest(f"{t.repo} checkout absent; run build-corpus first")
            members = recompute_lc_predicate(cfg.fixtures_root / t.repo, t.gold["lc_predicate"], t.repo)
            self.assertEqual(len(members), n, f"{tid}: {sorted(members)}")
            self.assertEqual(len(members), len(set(members)), f"{tid}: duplicate members")


if __name__ == "__main__":
    unittest.main()
