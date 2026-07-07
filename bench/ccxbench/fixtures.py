"""Generate a deterministic multi-language fixture repo and a ground-truth manifest.

The repo content is fixed literal text, so line numbers and call edges are provable:
the manifest's line for each symbol is computed from the written content, and every
declared local call edge is validated by checking the callee's name appears in the
caller's file. Gold answers for navigation/callees/callers/intent tasks are derived
from this manifest, so they cannot drift from the repo.
"""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

from .types import Symbol

FIXTURE_NAME = "ccxfixture"


def _status_filler(section: str, n: int) -> str:
    """Deterministic const-block filler for status.go: every line trimmed-unique.

    Consts do not appear in `ccx code outline` output, so the stale_anchor capture
    stays compact while the file itself exceeds the guard pack's LARGE_READ_BYTES
    (20 KB) — an unbounded Read in the ccx arm must trip the guard, which is what
    makes the stale-anchor tasks exercise anchor resolution instead of a re-read.
    """
    lines = [f"// {section.capitalize()} thresholds tune the health probes of the {section} window."]
    lines.append("const (")
    for i in range(n):
        lines.append(f"\tprobe{section.capitalize()}{i:03d} = {i * 7 + len(section)} // {section} probe slot {i:03d}")
    lines.append(")")
    return "\n".join(lines)


def _status_go() -> str:
    """Compose internal/status/status.go: the handwritten funcs (whose exact lines
    taskgen.STALE_SPECS uses as edit anchors) interleaved with filler that pushes
    the file past 20 KB and the target funcs deep enough that a windowed Read
    cannot trivially reveal them."""
    header = """// Package status reports service health as a small state machine.
package status

// State enumerates a service's health level.
type State int
"""
    ready = """// Ready reports that the service is fully operational.
func Ready() State {
\tconst healthy = 2
\treturn State(healthy)
}
"""
    degraded = """// Degraded reports that the service is running with reduced capacity.
func Degraded() State {
\tconst impaired = 1
\treturn State(impaired)
}
"""
    report = """// Report renders a state as a short human-readable label.
func Report(s State) string {
\tswitch s {
\tcase Ready():
\t\treturn "ready"
\tcase Degraded():
\t\treturn "degraded"
\tdefault:
\t\treturn "offline"
\t}
}
"""
    reset = """// reset clears a probe back to the zero state.
func reset() State {
\tvar cleared State
\treturn cleared
}
"""
    return "\n".join(
        [
            header,
            _status_filler("startup", 140),
            ready,
            _status_filler("steady", 120),
            degraded,
            _status_filler("drain", 120),
            report,
            _status_filler("shutdown", 80),
            reset,
        ]
    )


FILES: dict[str, str] = {
    "go.mod": """module example.com/ccxfixture

go 1.23
""",
    "cmd/app/main.go": """// Command app prints a greeting and a computed sum.
package main

import (
\t"fmt"

\t"example.com/ccxfixture/internal/calc"
\t"example.com/ccxfixture/internal/greet"
)

func main() {
\tfmt.Println(greet.Greet("Ada"))
\tfmt.Println(calc.Compute([]int{1, 2, 3}))
}
""",
    "internal/greet/greet.go": """// Package greet builds friendly greetings.
package greet

import "strings"

// Greet returns a greeting for the given name, defaulting to "world".
func Greet(name string) string {
\tname = strings.TrimSpace(name)
\tif name == "" {
\t\tname = "world"
\t}
\treturn salutation() + ", " + name + "!"
}

// salutation is the fixed greeting prefix.
func salutation() string {
\treturn "Hello"
}
""",
    "internal/calc/calc.go": """// Package calc does small integer arithmetic.
package calc

// Add returns the sum of two integers.
func Add(a, b int) int {
\treturn a + b
}

// Sub returns the difference of two integers.
func Sub(a, b int) int {
\treturn a - b
}

// Double returns twice n.
func Double(n int) int {
\treturn Add(n, n)
}

// Triple returns three times n.
func Triple(n int) int {
\treturn Add(Double(n), n)
}

// Compute sums a slice of integers.
func Compute(xs []int) int {
\ttotal := 0
\tfor _, x := range xs {
\t\ttotal = Add(total, x)
\t}
\treturn total
}

// Max returns the largest integer in xs, or 0 when xs is empty.
func Max(xs []int) int {
\tbest := 0
\tfor _, x := range xs {
\t\tif x > best {
\t\t\tbest = x
\t\t}
\t}
\treturn best
}
""",
    "internal/status/status.go": _status_go(),
    "pysrc/util.py": '''"""Text helpers."""


def normalize(s):
    """Collapse whitespace and lowercase a string."""
    return " ".join(s.split()).lower()


def slugify(s):
    """Turn a string into a hyphenated slug."""
    return normalize(s).replace(" ", "-")


def titlecase(s):
    """Title-case a normalized string."""
    return normalize(s).title()


def is_blank(s):
    """Report whether a string is empty once normalized."""
    return normalize(s) == ""
''',
    "web/app.ts": """export function shout(msg: string): string {
  return msg.toUpperCase() + "!";
}

export function whisper(msg: string): string {
  return msg.toLowerCase();
}

export function announce(msg: string): string {
  return shout(msg) + " (announcement)";
}
""",
    "docs/guide.md": """# Fixture Guide

## Usage

Run the app to print a greeting and a computed sum.

## Internals

Greetings are built in the greet package; arithmetic lives in calc. Text helpers
that clean up and slugify strings live in the Python util module.
""",
}


SYMBOLS: tuple[Symbol, ...] = (
    Symbol("Greet", "internal/greet/greet.go", "func Greet(", "func", ("salutation",), ("main",)),
    Symbol("salutation", "internal/greet/greet.go", "func salutation(", "func", (), ("Greet",)),
    Symbol("Add", "internal/calc/calc.go", "func Add(", "func", (), ("Double", "Triple", "Compute")),
    Symbol("Sub", "internal/calc/calc.go", "func Sub(", "func", (), ()),
    Symbol("Double", "internal/calc/calc.go", "func Double(", "func", ("Add",), ("Triple",)),
    Symbol("Triple", "internal/calc/calc.go", "func Triple(", "func", ("Add", "Double"), ()),
    Symbol("Compute", "internal/calc/calc.go", "func Compute(", "func", ("Add",), ("main",)),
    Symbol("Max", "internal/calc/calc.go", "func Max(", "func", (), ()),
    Symbol("main", "cmd/app/main.go", "func main(", "func", ("Greet", "Compute"), ()),
    Symbol("normalize", "pysrc/util.py", "def normalize(", "func", (), ("slugify", "titlecase", "is_blank")),
    Symbol("slugify", "pysrc/util.py", "def slugify(", "func", ("normalize",), ()),
    Symbol("titlecase", "pysrc/util.py", "def titlecase(", "func", ("normalize",), ()),
    Symbol("is_blank", "pysrc/util.py", "def is_blank(", "func", ("normalize",), ()),
    Symbol("shout", "web/app.ts", "export function shout(", "func", (), ("announce",)),
    Symbol("whisper", "web/app.ts", "export function whisper(", "func", (), ()),
    Symbol("announce", "web/app.ts", "export function announce(", "func", ("shout",), ()),
    # Symbols in the large generated file (>20KB): big-file nav/symbol tasks exercise
    # ccx code outline/symbol versus a full native Read, and trip the large-Read guard.
    Symbol("Gen0", "internal/gen/generated.go", "func Gen0(", "func", (), ("GeneratedTotal",)),
    Symbol("Gen10", "internal/gen/generated.go", "func Gen10(", "func", (), ()),
    Symbol("Gen42", "internal/gen/generated.go", "func Gen42(", "func", (), ()),
    Symbol("GeneratedTotal", "internal/gen/generated.go", "func GeneratedTotal(", "func", ("Gen0", "Gen1", "Gen2"), ()),
)

# File-level intent phrases for intent_search tasks: a behavioral description that
# deliberately avoids the target file's own identifiers and docstring wording, so the
# task tests semantic search rather than a lexical grep for the phrase.
INTENTS: tuple[tuple[str, str], ...] = (
    ("produces the welcome line shown to a user, addressing them by name", "internal/greet/greet.go"),
    ("adds together every number in a list and returns the running total", "internal/calc/calc.go"),
    ("converts arbitrary text into a url-safe dashed identifier", "pysrc/util.py"),
    ("loudly capitalizes a message and tacks on emphasis", "web/app.ts"),
)


def generated_go(count: int = 220) -> str:
    """Deterministic >20KB Go file of Gen0..Gen{count-1} plus GeneratedTotal."""
    parts = [
        "// Package gen holds generated arithmetic helpers.",
        "package gen",
        "",
    ]
    for i in range(count):
        parts += [
            f"// Gen{i} returns n adjusted by {i}.",
            f"func Gen{i}(n int) int {{",
            "\tx := n",
            f"\tx += {i}",
            "\tif x < 0 {",
            "\t\tx = -x",
            "\t}",
            "\treturn x",
            "}",
            "",
        ]
    parts += [
        "// GeneratedTotal sums the first three generators applied to n.",
        "func GeneratedTotal(n int) int {",
        "\treturn Gen0(n) + Gen1(n) + Gen2(n)",
        "}",
        "",
    ]
    return "\n".join(parts)


def repo_files() -> dict[str, str]:
    """All fixture files, including the large generated Go file."""
    return {**FILES, "internal/gen/generated.go": generated_go()}


def line_of(content: str, needle: str) -> int:
    for i, line in enumerate(content.splitlines(), start=1):
        if needle in line:
            return i
    raise ValueError(f"decl {needle!r} not found")


def git_env() -> dict:
    """A fixed identity + date so the fixture commit is byte-deterministic."""
    return {
        "PATH": os.environ.get("PATH", ""),
        "GIT_AUTHOR_NAME": "ccxbench",
        "GIT_AUTHOR_EMAIL": "bench@cc-context.local",
        "GIT_AUTHOR_DATE": "2026-01-01T00:00:00Z",
        "GIT_COMMITTER_NAME": "ccxbench",
        "GIT_COMMITTER_EMAIL": "bench@cc-context.local",
        "GIT_COMMITTER_DATE": "2026-01-01T00:00:00Z",
    }


def validate() -> None:
    """Fail loudly if the manifest disagrees with the fixture content."""
    files = repo_files()
    for sym in SYMBOLS:
        content = files[sym.file]
        line_of(content, sym.decl)  # raises if decl missing
        for callee in sym.callees:
            if callee not in content:
                raise ValueError(f"{sym.name} declares callee {callee} but it is absent from {sym.file}")


def build(root: Path) -> dict:
    """Write the fixture under `root`, git-init + commit, and return the manifest dict."""
    validate()
    files = repo_files()
    root.mkdir(parents=True, exist_ok=True)
    for rel, content in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)

    env = git_env()
    git = ["git", "-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"]
    subprocess.run([*git, "init", "-q"], cwd=root, check=True, env=env)
    subprocess.run([*git, "add", "-A"], cwd=root, check=True, env=env)
    subprocess.run([*git, "commit", "-q", "-m", "fixture baseline"], cwd=root, check=True, env=env)

    manifest = {
        "name": FIXTURE_NAME,
        "symbols": [
            {
                "name": s.name,
                "file": s.file,
                "line": line_of(files[s.file], s.decl),
                "kind": s.kind,
                "callees": list(s.callees),
                "callers": list(s.callers),
            }
            for s in SYMBOLS
        ],
        "intents": [{"phrase": p, "file": f} for p, f in INTENTS],
        "files": list(files),
    }
    # The manifest is the answer key. It must NEVER live inside the checked-out repo,
    # or the model under test could read it instead of navigating. Write it as a sibling
    # of the repo dir, outside any per-run workdir.
    (root.parent / f"{root.name}.manifest.json").write_text(json.dumps(manifest, indent=2))
    return manifest
