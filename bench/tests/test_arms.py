"""Unit tests for per-arm workdir + RunSpec construction (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import dataclasses
import json
import os
import subprocess
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from unittest.mock import patch

from ccxbench import arms, taskgen
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


class TestExecPipelineDiagnostic(unittest.TestCase):
    """The T7 exec-vs-pipeline steering: length-matched per-arm suffixes, threaded onto each arm's
    ladder only for a task that carries them."""

    def test_exec_and_pipeline_suffixes_length_matched(self) -> None:
        counts = {arm: local_count(txt, "m") for arm, txt in taskgen.EXEC_PIPELINE_ADDENDA.items()}
        lo, hi = min(counts.values()), max(counts.values())
        self.assertLessEqual(hi / lo, 1.15, f"exec/pipeline suffixes diverge >15%: {counts}")

    def test_arm_addendum_appended_only_when_present(self) -> None:
        cfg = load()
        plain = Task("t", "navigation", "tornado", "p", {}, Grader("file_line"), {})
        diag = Task("flood-t7", "large_context_diag", "tornado", "p", {}, Grader("set_match"), {},
                    arm_addenda=taskgen.EXEC_PIPELINE_ADDENDA)
        with TemporaryDirectory() as tmp:
            shim = arms.ensure_baseline_shim(Path(tmp))
            with patch.object(arms, "guards_available", return_value=False):
                for arm in ARMS:
                    base = arms.build_run_spec(cfg, plain, arm, "sonnet", Path("/tmp/wd"), shim_dir=shim)
                    self.assertEqual(base.provider_configs["claude"].append_system_prompt, arms.ADDENDA[arm])
                    steered = arms.build_run_spec(cfg, diag, arm, "sonnet", Path("/tmp/wd"), shim_dir=shim)
                    prompt = steered.provider_configs["claude"].append_system_prompt
                    self.assertEqual(prompt, arms.ADDENDA[arm] + taskgen.EXEC_PIPELINE_ADDENDA[arm])
                    self.assertIn("pipeline" if arm == "baseline" else "exec", prompt)


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
        with TemporaryDirectory() as tmp:
            shim = arms.ensure_baseline_shim(Path(tmp))
            with patch.object(arms, "guards_available", return_value=False):
                for arm in ARMS:
                    spec = arms.build_run_spec(cfg, make_task("t", "tornado"), arm, "sonnet", workdir, shim_dir=shim)
                    cc = spec.provider_configs["claude"]
                    self.assertEqual(cc.append_system_prompt, arms.ADDENDA[arm])
                    self.assertEqual(cc.max_turns, cfg.max_turns)
                    # Every arm pins a minimal PATH (spec.env wins spawnllm's merge); none carries the
                    # shadowing dirs the pilots leaked (`/usr/local/bin`, any `.venv` segment).
                    dirs = spec.env["PATH"].split(os.pathsep)
                    self.assertNotIn("/usr/local/bin", dirs)
                    self.assertFalse(any(seg == ".venv" for d in dirs for seg in d.split(os.sep)), spec.env["PATH"])
                    if arm == "baseline":
                        # Baseline leads with the `ccx`-not-found shim; the real ccx dir is absent.
                        self.assertEqual(dirs[0], str(shim))
                        self.assertNotIn(str(cfg.ccx_bin.parent), dirs)
                    else:
                        self.assertEqual(dirs[0], str(cfg.ccx_bin.parent))
                        self.assertNotIn(str(shim), dirs)


class TestBaselineShim(unittest.TestCase):
    """Fix #1: the baseline arm gets a `ccx` stub that fails like a host without ccx installed."""

    def test_stub_exits_127_command_not_found(self) -> None:
        with TemporaryDirectory() as tmp:
            shim = arms.ensure_baseline_shim(Path(tmp))
            proc = subprocess.run([str(shim / "ccx"), "code", "outline", "x"], capture_output=True, text=True)
        self.assertEqual(proc.returncode, 127)
        self.assertIn("command not found", proc.stderr)

    def test_baseline_leads_with_shim_ccx_arms_omit_it(self) -> None:
        cfg = load()
        with TemporaryDirectory() as tmp:
            shim = arms.ensure_baseline_shim(Path(tmp))
            base = arms.run_path(cfg, ccx=False, shim_dir=shim).split(os.pathsep)
            ccxp = arms.run_path(cfg, ccx=True, shim_dir=shim).split(os.pathsep)
        self.assertEqual(base[0], str(shim))
        self.assertNotIn(str(shim), ccxp)


class TestValidateCcxBin(unittest.TestCase):
    """Fix #2: a missing or non-executable ccx binary fails loud at setup, never silently."""

    def test_missing_ccx_bin_raises(self) -> None:
        cfg = dataclasses.replace(load(), ccx_bin=Path("/nonexistent/dir/ccx"))
        with self.assertRaises(LookupError):
            arms.validate_ccx_bin(cfg)

    def test_non_executable_ccx_bin_raises(self) -> None:
        with TemporaryDirectory() as tmp:
            fake = Path(tmp) / "ccx"
            fake.write_text("#!/bin/sh\n")  # written but not chmod +x
            cfg = dataclasses.replace(load(), ccx_bin=fake)
            with self.assertRaises(LookupError):
                arms.validate_ccx_bin(cfg)


class TestPathExcluded(unittest.TestCase):
    """Fix #6: exclusions match after symlink + trailing-slash normalization, on path segments."""

    def test_usr_local_bin_excluded_with_or_without_trailing_slash(self) -> None:
        self.assertTrue(arms._path_excluded("/usr/local/bin"))
        self.assertTrue(arms._path_excluded("/usr/local/bin/"))

    def test_venv_and_venvs_segments_excluded(self) -> None:
        with TemporaryDirectory() as tmp:
            venv = Path(tmp) / ".venv" / "bin"
            venvs = Path(tmp) / ".venvs" / "tool" / "bin"
            venv.mkdir(parents=True)
            venvs.mkdir(parents=True)
            self.assertTrue(arms._path_excluded(str(venv)))
            self.assertTrue(arms._path_excluded(str(venvs)))

    def test_symlink_to_excluded_dir_excluded(self) -> None:
        with TemporaryDirectory() as tmp:
            target = Path(tmp) / ".venv" / "bin"
            target.mkdir(parents=True)
            link = Path(tmp) / "link"
            link.symlink_to(target)
            self.assertTrue(arms._path_excluded(str(link)))

    def test_plain_dir_not_excluded(self) -> None:
        with TemporaryDirectory() as tmp:
            d = Path(tmp) / "local" / "bin"
            d.mkdir(parents=True)
            self.assertFalse(arms._path_excluded(str(d)))


class TestGuardProbePath(unittest.TestCase):
    """Fix #3: the guard-liveness probe runs under the same composed child PATH the arms use."""

    DENY = json.dumps(
        {"hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "ccx"}}
    )

    def setUp(self) -> None:
        arms.GUARD_PROBE.clear()

    def test_probe_env_path_is_composed_ccx_path(self) -> None:
        cfg = load()
        captured: dict = {}

        def fake_run(*_args, **kwargs):
            captured["env"] = kwargs.get("env")
            return SimpleNamespace(stdout=self.DENY)

        with patch.object(arms.subprocess, "run", side_effect=fake_run):
            arms.guards_available(cfg)
        self.assertIn("env", captured)
        self.assertEqual(captured["env"]["PATH"], arms.run_path(cfg, ccx=True))


class TestRunPath(unittest.TestCase):
    """Fix #3: `run_path` composes a minimal child PATH, skipping the pilot's leakage vectors."""

    def _mkbin(self, d: Path, name: str) -> None:
        d.mkdir(parents=True, exist_ok=True)
        exe = d / name
        exe.write_text("#!/bin/sh\n")
        exe.chmod(0o755)

    def test_composition_skips_shadows_and_dedupes(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            home = root / "home"
            venv_bin = root / "proj" / ".venv" / "bin"
            superset_bin = home / ".superset" / "bin"
            claude_dir = root / "local" / "bin"
            uvx_dir = root / "tools"
            # Every decoy also carries a `claude`, so an unskipped one would win the scan.
            self._mkbin(venv_bin, "claude")
            self._mkbin(superset_bin, "claude")
            self._mkbin(claude_dir, "claude")
            self._mkbin(uvx_dir, "uvx")
            cfg = dataclasses.replace(load(), ccx_bin=root / "ccxbin" / "ccx")
            # `/usr/local/bin` is excluded by exact-string match, so its literal presence suffices.
            path = os.pathsep.join([str(venv_bin), str(superset_bin), "/usr/local/bin", str(claude_dir), str(uvx_dir)])
            with patch.dict(os.environ, {"PATH": path, "HOME": str(home)}, clear=True):
                base = arms.run_path(cfg, ccx=False)
                ccxp = arms.run_path(cfg, ccx=True)
        base_dirs = base.split(os.pathsep)
        ccx_dirs = ccxp.split(os.pathsep)
        for excluded in (str(venv_bin), str(superset_bin), "/usr/local/bin"):
            self.assertNotIn(excluded, base_dirs)
            self.assertNotIn(excluded, ccx_dirs)
        self.assertIn(str(claude_dir), base_dirs)
        self.assertIn(str(uvx_dir), base_dirs)
        # ccx binary dir leads for ccx arms only; baseline never carries it.
        self.assertEqual(ccx_dirs[0], str(cfg.ccx_bin.parent))
        self.assertNotIn(str(cfg.ccx_bin.parent), base_dirs)
        # Ordered, first-wins dedupe: resolved tools then the fixed system tail, each once.
        self.assertEqual(base_dirs, [str(claude_dir), str(uvx_dir), *arms.SYSTEM_PATH_DIRS])
        self.assertEqual(ccx_dirs, [str(cfg.ccx_bin.parent), str(claude_dir), str(uvx_dir), *arms.SYSTEM_PATH_DIRS])

    def test_raises_when_claude_unfindable(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            venv_bin = root / ".venv" / "bin"
            uvx_dir = root / "tools"
            self._mkbin(venv_bin, "claude")  # claude exists only in an excluded dir
            self._mkbin(uvx_dir, "uvx")
            cfg = dataclasses.replace(load(), ccx_bin=root / "ccxbin" / "ccx")
            path = os.pathsep.join([str(venv_bin), str(uvx_dir)])
            with patch.dict(os.environ, {"PATH": path, "HOME": str(root)}, clear=True):
                with self.assertRaises(LookupError):
                    arms.run_path(cfg, ccx=True)


class TestGuardsAvailable(unittest.TestCase):
    """Fix #1: the probe reads the hook's JSON response — live when it denies OR allows a bounded
    rewrite (`updatedInput` adds a `limit`), naming ccx. A plain allow, a missing pack, or a
    timeout is not live."""

    DENY = json.dumps(
        {
            "hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "permissionDecision": "deny",
                "permissionDecisionReason": "Blocked: use `ccx code outline` instead",
            }
        }
    )
    ALLOW_BOUNDED = json.dumps(
        {
            "hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "permissionDecision": "allow",
                "updatedInput": {"file_path": "/tmp/x.py", "limit": 100},
                "additionalContext": "Bounded an unbounded Read (~30 KB): Map the rest: `ccx code outline /tmp/x.py`",
            }
        }
    )
    ALLOW_PLAIN = json.dumps(
        {
            "hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "permissionDecision": "allow",
                "additionalContext": "an allow that mentions ccx but does not rewrite the Read",
            }
        }
    )

    def setUp(self) -> None:
        arms.GUARD_PROBE.clear()

    def _probe_stdout(self, stdout: str) -> bool:
        with patch.object(arms.subprocess, "run", return_value=SimpleNamespace(stdout=stdout)) as mrun:
            live = arms.guards_available(load())
        self.assertTrue(mrun.called)
        return live

    def test_old_style_deny_is_live(self) -> None:
        self.assertTrue(self._probe_stdout(self.DENY))

    def test_new_style_allow_rewrite_is_live(self) -> None:
        self.assertTrue(self._probe_stdout(self.ALLOW_BOUNDED))

    def test_plain_allow_without_updated_input_is_not_live(self) -> None:
        self.assertFalse(self._probe_stdout(self.ALLOW_PLAIN))

    def test_missing_read_guards_short_circuits(self) -> None:
        cfg = dataclasses.replace(load(), plugin_hooks=Path("/nonexistent/hooks"))
        with patch.object(arms.subprocess, "run") as mrun:
            self.assertFalse(arms.guards_available(cfg))
        self.assertFalse(mrun.called)

    def test_timeout_is_not_live(self) -> None:
        with patch.object(arms.subprocess, "run", side_effect=subprocess.TimeoutExpired("capt-hook", 180)):
            self.assertFalse(arms.guards_available(load()))

    def test_missing_uvx_is_not_live(self) -> None:
        with patch.object(arms.subprocess, "run", side_effect=FileNotFoundError()):
            self.assertFalse(arms.guards_available(load()))


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
