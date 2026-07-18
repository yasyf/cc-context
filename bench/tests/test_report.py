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
from ccxbench.types import Decomposition, TrajectoryMetrics


DEFAULT_INIT = {
    "baseline": {"mcp_servers": [], "plugins": [], "n_tools": 10, "n_skills": 0},
    "ccx-cli": {"mcp_servers": [], "plugins": [], "n_tools": 10, "n_skills": 0},
    "ccx-mcp": {"mcp_servers": ["cc-context"], "plugins": [], "n_tools": 12, "n_skills": 0},
}

# A real retry-inclusive opus modelUsage (trace-tornado-parse-body ccx-mcp r3): the top-level envelope
# reported only 86,512 (retry-exclusive); the billed per-model total is 128,550.
OPUS_MU = {"inputTokens": 6, "cacheReadInputTokens": 114602, "cacheCreationInputTokens": 13153, "outputTokens": 789, "costUSD": 0.208586, "webSearchRequests": 0, "contextWindow": 200000, "maxOutputTokens": 32000}
HAIKU_MU = {"inputTokens": 586, "cacheReadInputTokens": 0, "cacheCreationInputTokens": 0, "outputTokens": 16, "costUSD": 0.000666, "webSearchRequests": 0, "contextWindow": 200000, "maxOutputTokens": 32000}


class FakeCounter:
    """Deterministic stand-in for tokens.TokenCounter (no network)."""

    def count(self, text: str) -> int:
        return len(text) // 4


def _transcript(high_water: int, output: int = 0, tool_chars: int = 0) -> list[dict]:
    """One assistant event whose usage gives the turn this prompt high-water.

    `tool_chars` > 0 appends a tool_result user event so `cumulative_tool_output` is non-zero
    (FakeCounter counts len//4), driving the tool-result co-metric.
    """
    events: list[dict] = [
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
    if tool_chars:
        events.append(
            {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "t", "content": "z" * tool_chars}]}}
        )
    return events


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
    model_ids: list[str] | None = None,
) -> dict:
    return {
        "task_id": task,
        "category": category,
        "arm": arm,
        "model": model,
        "model_ids": model_ids or [],
        "repeat": repeat,
        "is_error": False,
        "correct": correct,
        "usage": {"input": env_t, "output": 0, "cache_read": 0, "cache_create": 0},
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
    model_ids: list[str] | None = None,
    tool_cs: list[int] | None = None,
) -> None:
    corrects = corrects if corrects is not None else [True] * len(hs)
    tool_cs = tool_cs if tool_cs is not None else [0] * len(hs)
    for repeat, (h, t, c, tc) in enumerate(zip(hs, ts, corrects, tool_cs, strict=True)):
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
                model_ids=model_ids,
            )
        )
        (raw_dir / f"{task}__{arm}__{model}__r{repeat}.json").write_text(json.dumps(_transcript(h, tool_chars=tc)))


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
    def test_stream_completeness_flags_billed_vs_transcript_gap(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            # Two runs complete (billed T == transcript T), one whose transcript is 100% off (a call
            # the billing counts is absent from the saved stream).
            _add(recs, raw, task="t1", arm="baseline", hs=[1000], ts=[1000])
            _add(recs, raw, task="t1", arm="ccx-mcp", hs=[1000], ts=[1000])
            _add(recs, raw, task="t1", arm="ccx-cli", hs=[2000], ts=[1000])
            md = _render(recs, raw)

        self.assertIn("#### Stream completeness", md)
        self.assertIn("billed T within 2% of transcript T): **2 / 3**", md)
        self.assertIn("`t1` [ccx-cli]", md)


class TestConsistencyChecks(unittest.TestCase):
    """The two split cross-checks: billing reconstruction (healthy) and stream completeness (capture)."""

    def _write(self, raw: Path, rid: str, msg_ids: list[str], num_turns: int, model_usage: dict) -> float:
        events: list[dict] = [
            {
                "type": "assistant",
                "message": {
                    "id": mid,
                    "content": [{"type": "text", "text": "x"}],
                    "usage": {"input_tokens": 2, "cache_read_input_tokens": 1000 * (i + 1), "cache_creation_input_tokens": 0, "output_tokens": 1},
                },
            }
            for i, mid in enumerate(msg_ids)
        ]
        cost = sum(d["costUSD"] for d in model_usage.values())
        events.append({"type": "result", "subtype": "success", "num_turns": num_turns, "total_cost_usd": cost, "modelUsage": model_usage})
        (raw / f"{rid}.json").write_text(json.dumps(events))
        return cost

    def _rec(self, usage: dict, cost: float) -> dict:
        return {"task_id": "t1", "arm": "ccx-mcp", "model": "opus", "repeat": 0, "usage": usage, "total_cost_usd": cost}

    # modelUsage bills 3 opus calls (input 6, cache_read 6000); the billed record usage matches it.
    MU = {"claude-opus-4-8": {"inputTokens": 6, "cacheReadInputTokens": 6000, "cacheCreationInputTokens": 0, "outputTokens": 3, "costUSD": 0.05}}
    BILLED = {"input": 6, "output": 3, "cache_read": 6000, "cache_create": 0}

    def test_billing_clean_but_stream_missing_a_call(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            cost = self._write(raw, "t1__ccx-mcp__opus__r0", ["a", "b"], num_turns=3, model_usage=self.MU)  # 3 billed, 2 saved
            md = "\n".join(report.consistency_section([self._rec(self.BILLED, cost)], raw_dir=raw, prompts={}, counter=FakeCounter()))
        self.assertIn("reconstructs the recorded `total_cost_usd`: **1 / 1**", md)
        self.assertIn("billed T within 2% of transcript T): **0 / 1**", md)
        self.assertIn("3,003 tokens short", md)

    def test_complete_run_passes_both_checks(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            cost = self._write(raw, "t1__ccx-mcp__opus__r0", ["a", "b", "c"], num_turns=3, model_usage=self.MU)  # 3 billed, 3 saved
            md = "\n".join(report.consistency_section([self._rec(self.BILLED, cost)], raw_dir=raw, prompts={}, counter=FakeCounter()))
        self.assertIn("reconstructs the recorded `total_cost_usd`: **1 / 1**", md)
        self.assertIn("billed T within 2% of transcript T): **1 / 1**", md)

    def test_billing_mismatch_flagged(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            self._write(raw, "t1__ccx-mcp__opus__r0", ["a", "b", "c"], num_turns=3, model_usage=self.MU)
            # Record's total_cost_usd disagrees with the per-model cost sum → billing reconstruction fails.
            md = "\n".join(report.consistency_section([self._rec(self.BILLED, 0.99)], raw_dir=raw, prompts={}, counter=FakeCounter()))
        self.assertIn("reconstructs the recorded `total_cost_usd`: **0 / 1**", md)


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

    def test_diagnostic_excluded_from_headline_but_shown(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            _add(recs, raw, task="t1", arm="baseline", hs=[1000], ts=[2000])
            _add(recs, raw, task="t1", arm="ccx-mcp", hs=[600], ts=[1200])
            _add(recs, raw, task="t1", arm="ccx-cli", hs=[700], ts=[1400])
            # A large_context_diag task: excluded from the paired headline, shown in its own panel.
            _add(recs, raw, task="d1", arm="baseline", hs=[500], ts=[500], category="large_context_diag")
            _add(recs, raw, task="d1", arm="ccx-mcp", hs=[500], ts=[500], category="large_context_diag")
            _add(recs, raw, task="d1", arm="ccx-cli", hs=[500], ts=[500], category="large_context_diag")
            md = _render(recs, raw)

        self.assertIn("Diagnostic panel — large_context_diag", md)
        # The diagnostic task never enters the paired headline (only t1 does).
        self.assertNotIn("`d1`", md.split("Diagnostic panel")[0])


class TestArmSubsetRendering(unittest.TestCase):
    def test_missing_ccx_arm_not_rendered_as_pair(self) -> None:
        # A `--arms baseline,ccx-cli` session: no ccx-mcp runs at all. The absent arm must not
        # produce a vacuous "ccx-mcp vs baseline" section with a spurious 0/0-accuracy FAIL.
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            _add(recs, raw, task="t1", arm="baseline", hs=[1000], ts=[2000])
            _add(recs, raw, task="t1", arm="ccx-cli", hs=[700], ts=[1400])
            md = _render(recs, raw)
        self.assertIn("### ccx-cli vs baseline", md)
        self.assertNotIn("### ccx-mcp vs baseline", md)


class TestPerTaskRegression(unittest.TestCase):
    """Fix #6: a per-task regression is `ccx correct-rate < baseline correct-rate`, not the old
    all-or-nothing `all(baseline) and not all(ccx)`."""

    def test_rate_drop_is_regression(self) -> None:
        cells = {
            "baseline": {"t1": report.Cell(corrects=[True, True, False])},  # 2/3
            "ccx-mcp": {"t1": report.Cell(corrects=[False, False, False])},  # 0/3
        }
        reg, imp = report._regressions(cells, ["t1"], "ccx-mcp")
        self.assertEqual(reg, ["t1"])
        self.assertEqual(imp, [])

    def test_rate_gain_is_improvement(self) -> None:
        cells = {
            "baseline": {"t1": report.Cell(corrects=[False, False, False])},  # 0/3
            "ccx-mcp": {"t1": report.Cell(corrects=[True, False, False])},  # 1/3
        }
        reg, imp = report._regressions(cells, ["t1"], "ccx-mcp")
        self.assertEqual((reg, imp), ([], ["t1"]))

    def test_equal_rate_is_neither(self) -> None:
        cells = {
            "baseline": {"t1": report.Cell(corrects=[True, False])},  # 1/2
            "ccx-mcp": {"t1": report.Cell(corrects=[False, True])},  # 1/2
        }
        self.assertEqual(report._regressions(cells, ["t1"], "ccx-mcp"), ([], []))


class TestIncompleteCampaign(unittest.TestCase):
    """Fix #1: a halt marker OR observed runs below meta.expected_runs marks the campaign
    incomplete — a banner is rendered and every verdict is forced to FAIL."""

    def _passing_records(self, recs: list[dict], raw: Path) -> None:
        for task in ("t1", "t2", "t3"):
            _add(recs, raw, task=task, arm="baseline", hs=[1000, 1000, 1000], ts=[2000, 2000, 2000])
            _add(recs, raw, task=task, arm="ccx-mcp", hs=[600, 600, 600], ts=[1200, 1200, 1200])
            _add(recs, raw, task=task, arm="ccx-cli", hs=[700, 700, 700], ts=[1400, 1400, 1400])

    def test_missing_runs_forces_fail(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            self._passing_records(recs, raw)  # 27 records that would otherwise PASS
            meta = {"corpus_sha": corpus_sha(), "expected_runs": len(recs) + 3, "env_fingerprint": []}
            md = _render(recs, raw, meta=meta)
        self.assertIn("INCOMPLETE CAMPAIGN", md)
        self.assertIn(f"3 of {len(recs) + 3} planned runs missing", md)
        self.assertNotIn("**PASS**", md)
        self.assertIn("incomplete campaign", md)

    def test_halt_marker_forces_fail(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            self._passing_records(recs, raw)
            meta = {"corpus_sha": corpus_sha(), "expected_runs": len(recs), "env_fingerprint": []}
            md = report.render(recs, "sess", raw_dir=raw, prompts={}, counter=FakeCounter(), meta=meta, halted=True)
        self.assertIn("INCOMPLETE CAMPAIGN", md)
        self.assertIn("HALTED", md)
        self.assertNotIn("**PASS**", md)

    def test_complete_campaign_not_flagged(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            self._passing_records(recs, raw)
            meta = {"corpus_sha": corpus_sha(), "expected_runs": len(recs), "env_fingerprint": []}
            md = _render(recs, raw, meta=meta)
        self.assertNotIn("INCOMPLETE CAMPAIGN", md)
        self.assertIn("**PASS**", md)


class TestIntegrityExclusionGating(unittest.TestCase):
    """Fix #2: integrity-excluded runs stay out of aggregates, but any exclusion touching a
    headline model x arm forces that verdict to FAIL; the control panel only reports its own."""

    def _passing_headlines(self, recs: list[dict], raw: Path) -> None:
        for task in ("t1", "t2", "t3"):
            _add(recs, raw, task=task, arm="baseline", hs=[1000, 1000, 1000], ts=[2000, 2000, 2000])
            _add(recs, raw, task=task, arm="ccx-mcp", hs=[600, 600, 600], ts=[1200, 1200, 1200])
            _add(recs, raw, task=task, arm="ccx-cli", hs=[700, 700, 700], ts=[1400, 1400, 1400])

    def test_headline_exclusion_forces_fail(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            self._passing_headlines(recs, raw)
            # A mislabeled ccx-mcp run (its own task, kept out of aggregates) must still FAIL ccx-mcp.
            _add(recs, raw, task="t4", arm="ccx-mcp", hs=[600], ts=[1200], ok=False)
            md = _render(recs, raw)
        self.assertIn("**FAIL** — ccx-mcp vs baseline", md)
        self.assertIn("integrity exclusions present", md)
        self.assertIn("t4 [ccx-mcp]", md)
        # ccx-cli, untouched by the exclusion, still PASSes.
        self.assertIn("**PASS** — ccx-cli vs baseline", md)

    def test_control_exclusion_reported_not_gated(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            self._passing_headlines(recs, raw)
            _add(recs, raw, task="nr1", arm="baseline", hs=[500], ts=[500], category="non_regression")
            _add(recs, raw, task="nr1", arm="ccx-mcp", hs=[500], ts=[500], category="non_regression")
            _add(recs, raw, task="nr1", arm="ccx-cli", hs=[500], ts=[500], category="non_regression", ok=False)
            md = _render(recs, raw)
        self.assertIn("integrity exclusions (reported, not verdict-forcing)", md)
        self.assertIn("nr1 [ccx-cli]", md)
        # The control exclusion does not force the ccx-cli headline verdict to FAIL.
        self.assertIn("**PASS** — ccx-cli vs baseline", md)


class TestResolvedModelId(unittest.TestCase):
    """Fix #10/#4: each requested-model group maps to exactly one resolved id — but only after
    filtering to ids carrying the alias token, since the envelope also lists Claude Code's internal
    helper models (the haiku title/summary helper). Two matching ids under one alias is a loud
    failure; a helper id alongside one matching id is not, and is rendered as `helper models:`."""

    def test_header_shows_resolved_id(self) -> None:
        recs: list[dict] = []
        mid = ["claude-sonnet-4-5-20250929"]
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            _add(recs, raw, task="t1", arm="baseline", hs=[1000], ts=[2000], model_ids=mid)
            _add(recs, raw, task="t1", arm="ccx-mcp", hs=[600], ts=[1200], model_ids=mid)
            _add(recs, raw, task="t1", arm="ccx-cli", hs=[700], ts=[1400], model_ids=mid)
            md = _render(recs, raw)
        self.assertIn("## Model: sonnet (resolved: `claude-sonnet-4-5-20250929`)", md)

    def test_multiple_resolved_ids_raises(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            _add(recs, raw, task="t1", arm="baseline", hs=[1000], ts=[2000], model_ids=["claude-sonnet-A"])
            _add(recs, raw, task="t1", arm="ccx-mcp", hs=[600], ts=[1200], model_ids=["claude-sonnet-B"])
            _add(recs, raw, task="t1", arm="ccx-cli", hs=[700], ts=[1400], model_ids=["claude-sonnet-A"])
            with self.assertRaises(ValueError):
                _render(recs, raw)

    def test_haiku_helper_alongside_one_sonnet_id_does_not_raise(self) -> None:
        recs: list[dict] = []
        mid = ["claude-sonnet-5", "claude-haiku-4-5-20251001"]  # the haiku entry is the internal helper.
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            _add(recs, raw, task="t1", arm="baseline", hs=[1000], ts=[2000], model_ids=mid)
            _add(recs, raw, task="t1", arm="ccx-mcp", hs=[600], ts=[1200], model_ids=mid)
            _add(recs, raw, task="t1", arm="ccx-cli", hs=[700], ts=[1400], model_ids=mid)
            md = _render(recs, raw)
        self.assertIn("## Model: sonnet (resolved: `claude-sonnet-5`)", md)
        self.assertIn("helper models: `claude-haiku-4-5-20251001`", md)


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


class TestThirdMetric(unittest.TestCase):
    """The tool-result tokens co-metric: renders paired with a CI, is non-gating, and per-metric
    skips pairs whose baseline emitted no tool output (no ZeroDivisionError)."""

    def test_three_headline_blocks_with_tool_ci(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            for task in ("t1", "t2", "t3"):
                _add(recs, raw, task=task, arm="baseline", hs=[1000] * 5, ts=[2000] * 5, tool_cs=[4000] * 5)  # tool 1000
                _add(recs, raw, task=task, arm="ccx-mcp", hs=[600] * 5, ts=[1200] * 5, tool_cs=[2000] * 5)  # tool 500
                _add(recs, raw, task=task, arm="ccx-cli", hs=[700] * 5, ts=[1400] * 5, tool_cs=[2000] * 5)
            md = _render(recs, raw)

        # All three headline blocks render.
        self.assertIn("Peak context (H = max single-turn", md)
        self.assertIn("Total tokens processed (T = Σ envelope usage)", md)
        self.assertIn("Tool-result tokens", md)
        # The tool block carries the same paired CI machinery; its savings is 1 - 500/1000 = +50%.
        tool_block = md[md.index("Tool-result tokens") :][:400]
        self.assertIn("95% CI", tool_block)
        self.assertIn("Mean savings: **+50.0%**", tool_block)

    def test_tool_ci_includes_zero_but_verdict_pass(self) -> None:
        # H and T clearly favor ccx (CIs exclude 0); tool output is identical between arms, so the
        # tool savings is 0 on every task (CI includes 0). Non-gating → the verdict still PASSes.
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            for task in ("t1", "t2", "t3"):
                _add(recs, raw, task=task, arm="baseline", hs=[1000] * 5, ts=[2000] * 5, tool_cs=[4000] * 5)
                _add(recs, raw, task=task, arm="ccx-mcp", hs=[600] * 5, ts=[1200] * 5, tool_cs=[4000] * 5)
                _add(recs, raw, task=task, arm="ccx-cli", hs=[700] * 5, ts=[1400] * 5, tool_cs=[4000] * 5)
            md = _render(recs, raw)

        self.assertIn("**PASS** — ccx-mcp vs baseline", md)
        self.assertIn("**PASS** — ccx-cli vs baseline", md)
        # The tool co-metric shows zero savings (CI does not exclude 0) yet never forces a FAIL.
        tool_block = md[md.index("Tool-result tokens") :][:400]
        self.assertIn("Mean savings: **+0.0%**", tool_block)
        self.assertNotIn("tool-result CI includes 0", md)

    def test_zero_baseline_tool_output_skipped_no_crash(self) -> None:
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            for task in ("t1", "t2"):
                _add(recs, raw, task=task, arm="baseline", hs=[1000] * 5, ts=[2000] * 5, tool_cs=[4000] * 5)
                _add(recs, raw, task=task, arm="ccx-mcp", hs=[600] * 5, ts=[1200] * 5, tool_cs=[2000] * 5)
                _add(recs, raw, task=task, arm="ccx-cli", hs=[700] * 5, ts=[1400] * 5, tool_cs=[2000] * 5)
            # t3's baseline emitted no tool output — the tool metric must skip it, not divide by zero.
            _add(recs, raw, task="t3", arm="baseline", hs=[1000] * 5, ts=[2000] * 5, tool_cs=[0] * 5)
            _add(recs, raw, task="t3", arm="ccx-mcp", hs=[600] * 5, ts=[1200] * 5, tool_cs=[2000] * 5)
            _add(recs, raw, task="t3", arm="ccx-cli", hs=[700] * 5, ts=[1400] * 5, tool_cs=[2000] * 5)
            md = _render(recs, raw)  # must not raise ZeroDivisionError

        self.assertIn("Skipped 1 task(s) with zero baseline tool-result tokens", md)
        # H and T still pair over all three tasks; only the tool metric drops t3.
        self.assertIn("Paired on **3 both-correct task(s)**", md)

    def test_all_pairs_skipped_renders_reason_verdict_unaffected(self) -> None:
        # Every task's baseline emitted no tool output → the tool metric skips all pairs (n == 0).
        recs: list[dict] = []
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            for task in ("t1", "t2", "t3"):
                _add(recs, raw, task=task, arm="baseline", hs=[1000] * 5, ts=[2000] * 5, tool_cs=[0] * 5)
                _add(recs, raw, task=task, arm="ccx-mcp", hs=[600] * 5, ts=[1200] * 5, tool_cs=[2000] * 5)
                _add(recs, raw, task=task, arm="ccx-cli", hs=[700] * 5, ts=[1400] * 5, tool_cs=[2000] * 5)
            md = _render(recs, raw)

        self.assertIn("all 3 pair(s) skipped: zero baseline tool-result tokens", md)
        # H and T still gate and PASS; the all-skipped tool metric leaves the verdict untouched.
        self.assertIn("**PASS** — ccx-mcp vs baseline", md)


def _tm(high_water: int) -> TrajectoryMetrics:
    return TrajectoryMetrics(
        high_water=high_water,
        decomposition=Decomposition(high_water, 0, 0, 0, 0),
        cumulative_tool_output=0,
        turn_count=1,
        tool_call_count=0,
        peak_turn=0,
        tool_calls=(),
        total_prompt=high_water,
        total_output=0,
    )


class TestRepresentative(unittest.TestCase):
    """`ArmAgg.representative` picks the transcript whose high-water is closest to the median."""

    def test_even_size_picks_nearer_of_the_two_middles(self) -> None:
        agg = report.ArmAgg(metrics=[_tm(100), _tm(200), _tm(300), _tm(400)], envelope_t=[0, 0, 0, 0])
        # median_h = (200 + 300) / 2 = 250; both 200 and 300 are 50 away — the tie breaks to 200.
        self.assertEqual(agg.representative.high_water, 200)

    def test_odd_size_picks_the_exact_median(self) -> None:
        agg = report.ArmAgg(metrics=[_tm(100), _tm(300), _tm(200)], envelope_t=[0, 0, 0])
        self.assertEqual(agg.representative.high_water, 200)


def _result_event(model_usage: dict) -> dict:
    return {
        "type": "result",
        "subtype": "success",
        "is_error": False,
        "num_turns": 1,
        "total_cost_usd": sum(d["costUSD"] for d in model_usage.values()),
        "session_id": "test",
        "usage": {"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
        "modelUsage": model_usage,
        "permission_denials": [],
    }


def _raw(events: list[dict]) -> bytes:
    return json.dumps(events).encode()


class TestBilledUsage(unittest.TestCase):
    """T sources billed per-model usage (retry-inclusive), not the retry-exclusive top-level envelope."""

    def test_picks_main_model_and_sums_retry_inclusive(self) -> None:
        raw = _raw([_result_event({"claude-opus-4-8": OPUS_MU, "claude-haiku-4-5-20251001": HAIKU_MU})])
        u = report._billed_usage_from_raw(raw, "opus")
        self.assertEqual(u, {"input": 6, "output": 789, "cache_read": 114602, "cache_create": 13153})
        # Excludes the haiku helper; the total is the billed 128,550, not the 86,512 top-level envelope.
        self.assertEqual(u["input"] + u["cache_read"] + u["cache_create"] + u["output"], 128550)

    def test_missing_main_model_raises(self) -> None:
        with self.assertRaises(ValueError):
            report._billed_usage_from_raw(_raw([_result_event({"claude-haiku-4-5-20251001": HAIKU_MU})]), "opus")

    def test_ambiguous_main_model_raises(self) -> None:
        raw = _raw([_result_event({"claude-opus-4-8": OPUS_MU, "claude-opus-4-8-preview": OPUS_MU})])
        with self.assertRaises(ValueError):
            report._billed_usage_from_raw(raw, "opus")

    def test_no_result_event_raises(self) -> None:
        asst = {"type": "assistant", "session_id": "test", "message": {"id": "m", "content": [], "usage": {}}}
        with self.assertRaises(ValueError):
            report._billed_usage_from_raw(_raw([asst]), "opus")

    def test_refresh_rewrites_stale_usage_from_raw(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp)
            (raw / "t1__baseline__opus__r0.json").write_text(
                json.dumps(
                    [
                        {"type": "assistant", "session_id": "test", "message": {"id": "m", "content": [{"type": "text", "text": "x"}], "usage": {"input_tokens": 2, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 0, "output_tokens": 1}}},
                        _result_event({"claude-opus-4-8": OPUS_MU}),
                    ]
                )
            )
            # A record carrying the stale retry-EXCLUSIVE top-level usage.
            records = [{"task_id": "t1", "arm": "baseline", "model": "opus", "repeat": 0, "usage": {"input": 4, "output": 740, "cache_read": 78294, "cache_create": 7474}}]
            report.refresh_billed_usage(records, raw)
        self.assertEqual(records[0]["usage"], {"input": 6, "output": 789, "cache_read": 114602, "cache_create": 13153})
        self.assertEqual(report._envelope_tokens(records[0]), 128550)

    def test_refresh_skips_errored_record(self) -> None:
        records = [{"task_id": "t", "arm": "baseline", "model": "opus", "repeat": 0, "is_error": True}]
        report.refresh_billed_usage(records, Path("/nonexistent"))  # no raw read, no raise
        self.assertNotIn("usage", records[0])


if __name__ == "__main__":
    unittest.main()
