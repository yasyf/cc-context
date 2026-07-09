"""Layer-1 deterministic tool-level micro-benchmark.

Proves the foundational claim that every benchmark headline rests on: for the same
intent, the ccx tool emits no more tokens into Claude's context than the raw tool it
replaces. No LLM, no agent — just the raw tool, its ccx equivalent, and the
count-tokens ground truth on each output.

Pairs are matched by intent: "understand a file" pits the file's full text (what a
raw `Read` injects) against `ccx code outline`; "find a pattern" pits `grep -rn` against
`ccx code grep`; and so on. Each pair asserts `ccx_tokens <= raw_tokens`; any violation
fails the bench, which is the CI regression guard for "strictly less at the tool
level."
"""

from __future__ import annotations

import argparse
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from .config import Config
from .tokens import default_counter

CountFn = Callable[[str], int]

TIMEOUT_S = 30


@dataclass(frozen=True)
class Pair:
    """One intent comparison: the raw tool's output text vs the ccx tool's output text."""

    intent: str
    target: str
    raw_text: str
    ccx_text: str


@dataclass(frozen=True)
class Row:
    """A scored `Pair`: token counts for each arm and whether ccx stayed at or under raw."""

    intent: str
    target: str
    raw_tokens: int
    ccx_tokens: int

    @property
    def savings_pct(self) -> float:
        if self.raw_tokens == 0:
            return 0.0
        return 100.0 * (1.0 - self.ccx_tokens / self.raw_tokens)

    @property
    def ok(self) -> bool:
        return self.ccx_tokens <= self.raw_tokens


@dataclass(frozen=True)
class Result:
    """The scored micro-benchmark: every row, plus the aggregate savings and violations."""

    rows: tuple[Row, ...]

    @property
    def violations(self) -> tuple[Row, ...]:
        return tuple(r for r in self.rows if not r.ok)

    @property
    def total_raw(self) -> int:
        return sum(r.raw_tokens for r in self.rows)

    @property
    def total_ccx(self) -> int:
        return sum(r.ccx_tokens for r in self.rows)

    @property
    def overall_savings_pct(self) -> float:
        if self.total_raw == 0:
            return 0.0
        return 100.0 * (1.0 - self.total_ccx / self.total_raw)

    @property
    def all_ok(self) -> bool:
        return not self.violations


def score_pairs(pairs: list[Pair], count: CountFn) -> Result:
    """Tokenize each pair's two outputs through `count` and score raw-vs-ccx per intent.

    `count` is the only external boundary; tests inject a deterministic fake so the
    scoring logic runs offline.
    """
    rows = tuple(
        Row(
            intent=p.intent,
            target=p.target,
            raw_tokens=count(p.raw_text),
            ccx_tokens=count(p.ccx_text),
        )
        for p in pairs
    )
    return Result(rows=rows)


def _ccx(cfg: Config, repo: Path, args: list[str]) -> str:
    proc = subprocess.run(
        [str(cfg.ccx_bin), *args],
        cwd=repo,
        capture_output=True,
        text=True,
        timeout=TIMEOUT_S,
    )
    if proc.returncode != 0:
        raise RuntimeError(f"ccx {' '.join(args)} (in {repo}) exited {proc.returncode}: {proc.stderr.strip()}")
    return proc.stdout


def _shell(cmd: list[str], cwd: Path, *, ok_codes: tuple[int, ...] = (0,)) -> str:
    """Run a raw shell command, failing loud on an unexpected exit code.

    `ok_codes` widens the accepted set for tools that signal a benign empty result with a
    non-zero code (grep returns 1 when nothing matched), so a real failure (a typo, a
    missing fixture, exit 2) raises instead of collapsing to an empty zero-token win.
    """
    proc = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=TIMEOUT_S)
    if proc.returncode not in ok_codes:
        raise RuntimeError(f"{' '.join(cmd)} (in {cwd}) exited {proc.returncode}: {proc.stderr.strip()}")
    return proc.stdout


def build_pairs(cfg: Config, repos: list[str] | None = None) -> list[Pair]:
    """Run the raw tool and its ccx equivalent over a small fixed corpus, one Pair per intent.

    The corpus is hard-coded and tiny on purpose: the real counter costs API calls, so a
    handful of representative files, patterns, and symbols per repo keeps the cached-count
    surface small while still exercising every intent.
    """
    root = cfg.fixtures_root
    corpus = _corpus(repos)
    pairs: list[Pair] = []

    for repo_name, spec in corpus.items():
        repo = root / repo_name

        for rel in spec["files"]:
            text = (repo / rel).read_text()
            pairs.append(
                Pair(
                    intent="understand file",
                    target=f"{repo_name}/{rel}",
                    raw_text=text,
                    ccx_text=_ccx(cfg, repo, ["code", "outline", rel]),
                )
            )

        for rel, section in spec["sections"]:
            text = (repo / rel).read_text()
            pairs.append(
                Pair(
                    intent="read region",
                    target=f"{repo_name}/{rel} {section}",
                    raw_text=text,
                    ccx_text=_ccx(cfg, repo, ["code", "read", rel, "--section", section]),
                )
            )

        for pattern in spec["patterns"]:
            pairs.append(
                Pair(
                    intent="find pattern",
                    target=f"{repo_name}:{pattern}",
                    raw_text=_shell(["grep", "-rn", pattern, "."], repo, ok_codes=(0, 1)),
                    ccx_text=_ccx(cfg, repo, ["code", "grep", pattern]),
                )
            )

        for glob in spec["globs"]:
            pairs.append(
                Pair(
                    intent="enumerate files",
                    target=f"{repo_name}:{glob}",
                    raw_text=_shell(["find", ".", "-type", "f", "-name", glob], repo),
                    ccx_text=_ccx(cfg, repo, ["repo", "find", glob]),
                )
            )

        for name in spec["symbols"]:
            pairs.append(
                Pair(
                    intent="understand symbol",
                    target=f"{repo_name}:{name}",
                    raw_text=_shell(["grep", "-rn", name, "."], repo, ok_codes=(0, 1)),
                    ccx_text=_ccx(cfg, repo, ["code", "symbol", name]),
                )
            )

    return pairs


def _corpus(repos: list[str] | None) -> dict[str, dict]:
    """The fixed micro-corpus: a few files, sections, patterns, globs, and symbols per repo."""
    full = {
        "gorilla-mux": {
            "files": ["mux.go", "route.go"],
            "sections": [("mux.go", "1-40"), ("regexp.go", "1-50")],
            "patterns": ["NewRouter", "ServeHTTP"],
            "globs": ["*.go"],
            "symbols": ["NewRouter", "Router"],
        },
        "click": {
            "files": ["src/click/core.py"],
            "sections": [("src/click/core.py", "1-60")],
            "patterns": ["def make_pass_decorator"],
            "globs": ["*.py"],
            "symbols": ["batch"],
        },
        "tornado": {
            "files": ["tornado/httputil.py"],
            "sections": [("tornado/web.py", "1-50")],
            "patterns": ["self.request"],
            "globs": ["*.py"],
            "symbols": ["RequestHandler"],
        },
    }
    if repos is None:
        return full
    return {name: spec for name, spec in full.items() if name in repos}


def format_table(result: Result) -> str:
    """Render the scored result as a fixed-width table plus an aggregate summary line."""
    header = f"{'intent':16} {'target':40} {'raw':>8} {'ccx':>8} {'save%':>7}  ok"
    sep = "-" * len(header)
    lines = [header, sep]
    for r in result.rows:
        target = r.target if len(r.target) <= 40 else r.target[:37] + "..."
        mark = "ok" if r.ok else "FAIL"
        lines.append(
            f"{r.intent:16} {target:40} {r.raw_tokens:>8} {r.ccx_tokens:>8} {r.savings_pct:>6.1f}%  {mark}"
        )
    lines.append(sep)
    lines.append(
        f"overall: {result.total_ccx} ccx vs {result.total_raw} raw tokens "
        f"-> {result.overall_savings_pct:.1f}% savings, {len(result.violations)} violation(s)"
    )
    return "\n".join(lines)


def cmd_microbench(cfg: Config, args: argparse.Namespace) -> int:
    """Build the micro-corpus, score it through the count-tokens counter, and print the table.

    Returns 0 when every intent pair satisfies `ccx_tokens <= raw_tokens`, else 1.
    """
    repos = args.repo.split(",") if getattr(args, "repo", None) else None
    pairs = build_pairs(cfg, repos)
    counter = default_counter()
    result = score_pairs(pairs, counter.count)
    print(format_table(result))
    if not result.all_ok:
        print(f"\nFAIL: {len(result.violations)} intent pair(s) where ccx emitted more than raw:")
        for r in result.violations:
            print(f"  - {r.intent} {r.target}: ccx {r.ccx_tokens} > raw {r.raw_tokens}")
        return 1
    return 0
