"""Unit tests for per-arm workdir + RunSpec construction (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import dataclasses
import json
import subprocess
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest.mock import patch

from ccxbench import arms
from ccxbench.config import load
from ccxbench.tokens import local_count
from ccxbench.types import ARMS, Grader, Task


def make_task(tid: str, repo: str, setup: dict | None = None) -> Task:
    return Task(tid, "navigation", repo, "p", {}, Grader("file_line"), {}, setup=setup or {})


def cfg_in(root: Path):
    cfg = load()
    return dataclasses.replace(cfg, work_root=root / "work", fixtures_root=root / "fixtures")


class TestLadderParity(unittest.TestCase):
    def test_addenda_cover_every_arm(self) -> None:
        self.assertEqual(set(arms.ADDENDA), set(ARMS))

    def test_addenda_token_length_within_15pct(self) -> None:
        counts = {arm: local_count(text, "m") for arm, text in arms.ADDENDA.items()}
        lo, hi = min(counts.values()), max(counts.values())
        self.assertLessEqual(hi / lo, 1.15, f"addenda token counts diverge >15%: {counts}")


class TestMcpConfig(unittest.TestCase):
    def test_cc_context_served_only_for_ccx_mcp(self) -> None:
        cfg = load()
        for arm in ARMS:
            servers = json.loads(arms.mcp_config(cfg, arm))["mcpServers"]
            if arm == "ccx-mcp":
                self.assertEqual(set(servers), {"cc-context"})
            else:
                self.assertEqual(servers, {})


class TestBuildRunSpec(unittest.TestCase):
    def test_addendum_max_turns_and_path_per_arm(self) -> None:
        cfg = load()
        workdir = Path("/tmp/wd")
        with patch.object(arms, "guards_available", return_value=False):
            for arm in ARMS:
                spec = arms.build_run_spec(cfg, make_task("t", "tornado"), arm, "sonnet", workdir)
                cc = spec.provider_configs["claude"]
                self.assertEqual(cc.append_system_prompt, arms.ADDENDA[arm])
                self.assertEqual(cc.max_turns, cfg.max_turns)
                if arm == "baseline":
                    self.assertNotIn("PATH", spec.env)
                else:
                    self.assertIn(str(cfg.ccx_bin.parent), spec.env["PATH"])


class TestPrepareWorkdir(unittest.TestCase):
    def test_empty_repo_yields_bare_workdir(self) -> None:
        with TemporaryDirectory() as tmp:
            cfg = cfg_in(Path(tmp))
            wd = arms.prepare_workdir(cfg, make_task("t", "empty"), "baseline", "t__baseline__sonnet__r0")
            self.assertTrue(wd.is_dir())
            self.assertEqual(list(wd.iterdir()), [])

    def test_setup_patch_applied_to_checkout(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            cfg = cfg_in(root)
            (cfg.fixtures_root / "myrepo").mkdir(parents=True)
            (cfg.fixtures_root / "myrepo" / "f.txt").write_text("line1\nline2\nline3\n")
            patches = root / "patches"
            patches.mkdir()
            (patches / "mytask.patch").write_text(
                "--- a/f.txt\n+++ b/f.txt\n@@ -1,3 +1,3 @@\n line1\n-line2\n+CHANGED\n line3\n"
            )
            with patch.object(arms, "PATCHES_DIR", patches):
                wd = arms.prepare_workdir(cfg, make_task("mytask", "myrepo"), "ccx-cli", "r0")
            self.assertEqual((wd / "f.txt").read_text(), "line1\nCHANGED\nline3\n")

    def test_setup_patch_reject_raises(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            cfg = cfg_in(root)
            (cfg.fixtures_root / "myrepo").mkdir(parents=True)
            (cfg.fixtures_root / "myrepo" / "f.txt").write_text("actual\ncontent\n")
            patches = root / "patches"
            patches.mkdir()
            (patches / "bad.patch").write_text(
                "--- a/f.txt\n+++ b/f.txt\n@@ -1,2 +1,2 @@\n nomatch\n-gone\n+new\n"
            )
            with patch.object(arms, "PATCHES_DIR", patches):
                with self.assertRaises(subprocess.CalledProcessError):
                    arms.prepare_workdir(cfg, make_task("bad", "myrepo"), "ccx-cli", "r0")

    def test_no_patch_leaves_checkout_untouched(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            cfg = cfg_in(root)
            (cfg.fixtures_root / "myrepo").mkdir(parents=True)
            (cfg.fixtures_root / "myrepo" / "f.txt").write_text("unchanged\n")
            with patch.object(arms, "PATCHES_DIR", root / "patches"):
                wd = arms.prepare_workdir(cfg, make_task("nopatch", "myrepo"), "baseline", "r0")
            self.assertEqual((wd / "f.txt").read_text(), "unchanged\n")


if __name__ == "__main__":
    unittest.main()
