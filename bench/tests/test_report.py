"""Unit tests for the 3-arm token-savings report renderer (no API calls).

Synthetic runs.jsonl-shaped records plus hand-built single-turn transcripts drive both
headline sections (peak context, total tokens), the median-across-repeats selection, the
PASS/FAIL verdict paths, the isolation-panel breach, envelope-vs-transcript consistency, and
the corpus-SHA drift warning.

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from ccxbench import report
from ccxbench.runner import corpus_sha


class FakeCounter:
    """Deterministic stand-in for tokens.TokenCounter (no network)."""

    def count(self, text: str) -> int:
        return len(text) // 4


DEFAULT_INIT = {
    "baseline": {"mcp_servers": [], "plugins": [], "n_tools": 10, "n_skills": 0},
    "ccx-cli": {"mcp_servers": [], "plugins": [], "n_tools": 10, "n_skills": 0},
    "ccx-mcp": {"mcp_servers": ["cc-context"], "plugins": [], "n_tools": 12, "n_skills": 0},
}


def _transcript(high_water: int, output: int = 0) -> list[dict]:
    """One assistant event whose usage gives the turn this prompt high-water."""
    return [
        {
            "type": "assistant",
            "message": {
                "content": [{"type": "text", "text": "ok"}],
                "usage": {
                    "input_tokens": high_water,
                    "cache_creation_input_tokens": 0,
                    "cache_read_input_tokens": 0,
                    "output_tokens": output,
                },
            },
        }
    ]


def _record(
    *,
    task: str,
    arm: str,
    model: str,
    repeat: int,
    env_t: int,
    correct: bool,
    ok: bool,
    category: str,
    init: dict | None,
) -> dict:
    return {
        "task_id": task,
        "category": category,
        "arm": arm,
        "model": model,
        "repeat": repeat,
        "is_error": False,
        "correct": correct,
        "usage": {"input": env_t, "output": 0, "cache_read": 0, "cache_create_5m": 0, "cache_create_1h": 0},
        "guards_active": arm != "baseline",
        "integrity": {"ok": ok, "note": "" if ok else "mislabeled"},
        "init": init or DEFAULT_INIT[arm],
    }


def _add(
    records: list[dict],
    raw_dir: Path,
    *,
    task: str,
    arm: str,
    hs: list[int],
    ts: list[int],
    model: str = "sonnet",
    corrects: list[bool] | None = None,
    ok: bool = True,
    category: str = "navigation",
    init: dict | None = None,
) -> None:
    corrects = corrects if corrects is not None else [True] * len(hs)
    for repeat, (h, t, c) in enumerate(zip(hs, ts, corrects, strict=True)):
        records.append(
            _record(
                task=task,
                arm=arm,
                model=model,
                repeat=repeat,
                env_t=t,
                correct=c,
                ok=ok,
                category=category,
                init=init,
            )
        )
        (raw_dir / f"{task}__{arm}__{model}__r{repeat}.json").write_text(json.dumps(_transcript(h)))


def _render(records: list[dict], raw_dir: Path, *, meta: dict | None = None) -> str:
    return report.render(records, "sess", raw_dir=raw_dir, prompts={}, counter=FakeCounter(), meta=meta)


class TestHeadlinesAndVerdict(unittest.TestCase):
    def test_both_headlines_and_pass_verdict(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            for task in ("t1", "t2", "t3"):
                _add(recs, raw, task=task, arm="baseline", hs=[1000, 1000, 1000], ts=[2000, 2000, 2000])
                _add(recs, raw, task=task, arm="ccx-mcp", hs=[600, 600, 600], ts=[1200, 1200, 1200])
                _add(recs, raw, task=task, arm="ccx-cli", hs=[700, 700, 700], ts=[1400, 1400, 1400])
            meta = {"corpus_sha": corpus_sha(), "env_fingerprint": ["ANTHROPIC_API_KEY"]}
            md = _render(recs, raw, meta=meta)

        # Both headline sections render, per ccx arm.
        self.assertIn("Peak context (H = max single-turn", md)
        self.assertIn("Total tokens processed (T = Σ envelope usage)", md)
        # ccx-mcp: H and T both 1 - 600/1000 = 1 - 1200/2000 = 40%.
        self.assertIn("Mean savings: **+40.0%**", md)
        # ccx-cli: 1 - 700/1000 = 1 - 1400/2000 = 30%.
        self.assertIn("Mean savings: **+30.0%**", md)
        # Both arms PASS (accuracy equal, both CIs exclude zero in ccx's favor).
        self.assertIn("**PASS** — ccx-mcp vs baseline", md)
        self.assertIn("**PASS** — ccx-cli vs baseline", md)
        # Isolation panel proves the arms differ only in the ccx surface.
        self.assertIn("OK: exactly cc-context", md)
        self.assertIn("OK: zero MCP, tool surface == baseline", md)
        # Corpus SHA matches the (recomputed) build; env fingerprint rendered once.
        self.assertIn("Corpus SHA matches build", md)
        self.assertIn("Env fingerprint (shared across arms)", md)

    def test_fail_on_per_task_regression(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            for task in ("t1", "t2"):
                _add(recs, raw, task=task, arm="baseline", hs=[1000, 1000, 1000], ts=[2000, 2000, 2000])
                _add(recs, raw, task=task, arm="ccx-cli", hs=[700, 700, 700], ts=[1400, 1400, 1400])
            # ccx-mcp regresses on t2 (baseline all-correct, ccx-mcp wrong on one repeat).
            _add(recs, raw, task="t1", arm="ccx-mcp", hs=[600, 600, 600], ts=[1200, 1200, 1200])
            _add(recs, raw, task="t2", arm="ccx-mcp", hs=[600, 600, 600], ts=[1200, 1200, 1200], corrects=[True, False, True])
            md = _render(recs, raw)

        self.assertIn("**FAIL** — ccx-mcp vs baseline", md)
        self.assertIn("⚠️ regressions: `t2`", md)
        self.assertIn("per-task regressions: t2", md)
        # ccx-cli, with no regression and 30% savings, still PASSes.
        self.assertIn("**PASS** — ccx-cli vs baseline", md)

    def test_fail_when_ci_includes_zero(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            for task in ("t1", "t2", "t3"):
                _add(recs, raw, task=task, arm="baseline", hs=[1000, 1000, 1000], ts=[2000, 2000, 2000])
                _add(recs, raw, task=task, arm="ccx-mcp", hs=[600, 600, 600], ts=[1200, 1200, 1200])
                # ccx-cli exactly matches baseline: zero savings, CI does not exclude zero.
                _add(recs, raw, task=task, arm="ccx-cli", hs=[1000, 1000, 1000], ts=[2000, 2000, 2000])
            md = _render(recs, raw)

        self.assertIn("**PASS** — ccx-mcp vs baseline", md)
        self.assertIn("**FAIL** — ccx-cli vs baseline", md)
        self.assertIn("peak-context CI includes 0", md)
        self.assertIn("total-tokens CI includes 0", md)


class TestMedianAcrossRepeats(unittest.TestCase):
    def test_median_not_first_repeat(self) -> None:
        # Baseline r0 is a tiny outlier (100/200); the median (1000/2000) must win, not r0.
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            _add(recs, raw, task="t1", arm="baseline", hs=[100, 1000, 1900], ts=[200, 2000, 3800])
            _add(recs, raw, task="t1", arm="ccx-mcp", hs=[600, 600, 600], ts=[1200, 1200, 1200])
            _add(recs, raw, task="t1", arm="ccx-cli", hs=[600, 600, 600], ts=[1200, 1200, 1200])
            md = _render(recs, raw)

        # median-based: 1 - 600/1000 = +40.0%. first-repeat would be 1 - 600/100 = -500.0%.
        self.assertIn("Mean savings: **+40.0%**", md)
        self.assertNotIn("-500.0%", md)


class TestIsolationBreach(unittest.TestCase):
    def test_ccx_cli_with_mcp_server_is_breach(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            _add(recs, raw, task="t1", arm="baseline", hs=[1000], ts=[2000])
            _add(recs, raw, task="t1", arm="ccx-mcp", hs=[600], ts=[1200])
            # ccx-cli leaked the cc-context MCP into its init — the isolation is broken.
            breach_init = {"mcp_servers": ["cc-context"], "plugins": [], "n_tools": 12, "n_skills": 0}
            _add(recs, raw, task="t1", arm="ccx-cli", hs=[700], ts=[1400], init=breach_init)
            md = _render(recs, raw)

        self.assertIn("⚠️ BREACH: MCP servers present (cc-context)", md)


class TestConsistency(unittest.TestCase):
    def test_envelope_vs_transcript_within_2pct(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            # Two runs consistent (env T == transcript T), one off by 100%.
            _add(recs, raw, task="t1", arm="baseline", hs=[1000], ts=[1000])
            _add(recs, raw, task="t1", arm="ccx-mcp", hs=[1000], ts=[1000])
            _add(recs, raw, task="t1", arm="ccx-cli", hs=[2000], ts=[1000])
            md = _render(recs, raw)

        self.assertIn("Runs within 2%: **2 / 3**", md)
        self.assertIn("`t1` [ccx-cli]", md)


class TestCorpusDrift(unittest.TestCase):
    def test_drift_warns_on_mismatch(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            _add(recs, raw, task="t1", arm="baseline", hs=[1000], ts=[2000])
            _add(recs, raw, task="t1", arm="ccx-mcp", hs=[600], ts=[1200])
            _add(recs, raw, task="t1", arm="ccx-cli", hs=[700], ts=[1400])
            md = _render(recs, raw, meta={"corpus_sha": "deadbeef" * 8, "env_fingerprint": []})

        self.assertIn("CORPUS DRIFT", md)


class TestControlPanel(unittest.TestCase):
    def test_non_regression_excluded_from_headline_but_shown(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            _add(recs, raw, task="t1", arm="baseline", hs=[1000], ts=[2000])
            _add(recs, raw, task="t1", arm="ccx-mcp", hs=[600], ts=[1200])
            _add(recs, raw, task="t1", arm="ccx-cli", hs=[700], ts=[1400])
            # A non_regression control task: excluded from the paired headline, shown in the panel.
            _add(recs, raw, task="nr1", arm="baseline", hs=[500], ts=[500], category="non_regression")
            _add(recs, raw, task="nr1", arm="ccx-mcp", hs=[500], ts=[500], category="non_regression")
            _add(recs, raw, task="nr1", arm="ccx-cli", hs=[500], ts=[500], category="non_regression")
            md = _render(recs, raw)

        self.assertIn("Control panel — non_regression", md)
        # The control task never enters the paired headline (only t1 does).
        self.assertNotIn("`nr1`", md.split("Control panel")[0])


class TestFailFastInputs(unittest.TestCase):
    def test_missing_raw_dir_raises(self) -> None:
        recs = [
            _record(
                task="t1",
                arm="baseline",
                model="sonnet",
                repeat=0,
                env_t=1000,
                correct=True,
                ok=True,
                category="navigation",
                init=None,
            )
        ]
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(FileNotFoundError):
                report.render(recs, "sess", raw_dir=Path(tmp) / "missing", prompts={}, counter=FakeCounter(), meta={})

    def test_corrupt_transcript_raises(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            _add(recs, raw, task="t1", arm="baseline", hs=[1000], ts=[2000])
            _add(recs, raw, task="t1", arm="ccx-mcp", hs=[600], ts=[1200])
            _add(recs, raw, task="t1", arm="ccx-cli", hs=[700], ts=[1400])
            (raw / "t1__ccx-cli__sonnet__r0.json").write_text("not json")
            with self.assertRaises(ValueError):
                _render(recs, raw)


if __name__ == "__main__":
    unittest.main()
