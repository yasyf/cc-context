"""Guard the cc-guides rendered-artifact regime: block a direct edit to a generated
artifact, nudge an edit to the render sources to re-render.

Every managed doc (``AGENTS.md``, ``CLAUDE.md``, some ``install-binary.sh``) is rendered
from ``.claude/fragments/<target-path>/`` — local ``*.fragment.md``/``*.fragment.sh`` parts
plus a ``layout.toml`` — by the ``cc-guides`` binary, and stamped with a first-/second-line
banner. Editing the rendered artifact directly is always lost on the next ``cc-guides
render``; the fix is to edit the fragments and re-render, committing fragments and artifact
together. So:

* an ``Edit``/``Write``/``MultiEdit``/``NotebookEdit`` whose target IS a rendered artifact is
  **blocked**, steering to the fragment dir + ``cc-guides render``; and
* an edit to a render SOURCE — any file under ``.claude/fragments/``, or (in the cc-skills
  content repo) under ``guides/`` — draws a one-shot **nudge** to re-render and commit both.

The artifact block is precise by construction: it fires only when a sibling
``.claude/fragments/<repo-relative-target>/layout.toml`` exists AND the target's first two
lines carry the banner — so an unmanaged file with no fragment tree, or a file that merely
mentions the word GENERATED, is never touched. A fragment source (under
``.claude/fragments/``) is a SOURCE, not an artifact, and is excluded from the block so the
nudge lane owns it.
"""

from __future__ import annotations

import re

from captain_hook import (
    Allow,
    BaseHookEvent,
    CustomCondition,
    Event,
    FileFixture,
    Input,
    Tool,
    Warn,
    hook,
    nudge,
)


class RenderedArtifact(CustomCondition):
    """Matches an edit whose target IS a cc-guides rendered artifact — a generated file
    the next ``cc-guides render`` overwrites.

    Both cheap checks must hold. A sibling ``.claude/fragments/<repo-relative-target>/layout.toml``
    exists (one stat — so an unmanaged file with no fragment tree never matches), and the
    target's first two lines carry the cc-guides banner: a version, a ``src=`` fragment dir,
    and the ``| GENERATED`` marker (so a file that merely mentions GENERATED never matches).
    A file under ``.claude/fragments/`` is a fragment SOURCE, not an artifact, and is excluded
    here so the nudge lane owns it.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        f = evt.file
        if f is None or not f.is_file() or (root := evt.ctx.repo_root) is None:
            return False
        try:
            rel = f.path.resolve().relative_to(root.resolve())
        except ValueError:
            return False
        if rel.parts[:2] == (".claude", "fragments"):
            return False
        if not (root / ".claude" / "fragments" / rel / "layout.toml").is_file():
            return False
        try:
            with f.path.open(encoding="utf-8", errors="replace") as fh:
                head = fh.readline() + fh.readline()
        except OSError:
            return False
        return bool(re.search(r"cc-guides \S+ src=\S+( fragments=\S+)? \| GENERATED", head))


hook(
    Event.PreToolUse,
    "BLOCKED: this file is a cc-guides rendered artifact — a direct edit is discarded by the "
    "next `cc-guides render`. Edit the source instead: change the parts under "
    "`.claude/fragments/` that mirror this file's path (the `*.fragment.md`/`*.fragment.sh` "
    "files and `layout.toml`), run `cc-guides render`, then commit the fragments and the "
    "regenerated artifact together in one commit.",
    only_if=[Tool("Edit", "Write", "MultiEdit", "NotebookEdit"), RenderedArtifact()],
    block=True,
    tests={
        # The Block case needs a banner + a sibling layout.toml under a repo root — a real
        # tmp tree — so it lives in test_render_guards.py. Inline covers the ALLOW guards the
        # predicate must honor: an unmanaged path (no fragment tree), and a file that merely
        # contains the word GENERATED without the banner.
        Input(tool="Edit", file="/nope/internal/cli/root.go", content="x"): Allow(),
        Input(tool="Write", file=FileFixture(content="see the GENERATED docs\n", name="notes.md"), content="y"): Allow(),
    },
)


class RenderSource(CustomCondition):
    """Matches an edit to a cc-guides render SOURCE — a fragment tree, or (in the cc-skills
    content repo) the shared ``guides/`` home — where the fix is to re-render and commit both.

    Fires on any path under ``.claude/fragments/`` in any repo (a pure path property, no repo
    root needed), and on ``guides/`` only in cc-skills, whose ``guides/`` holds the fragments
    the rest of the fleet pulls.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        f = evt.file
        if f is None:
            return False
        if f.under(".claude/fragments"):
            return True
        root = evt.ctx.repo_root
        if root is None or root.name != "cc-skills":
            return False
        try:
            return f.path.resolve().relative_to(root.resolve()).parts[:1] == ("guides",)
        except ValueError:
            return False


nudge(
    "This is a cc-guides render SOURCE — the rendered artifact won't change until you re-render. "
    "Run `cc-guides render`, then commit the fragments and the regenerated artifact together in "
    "one commit (a fragment-only or artifact-only commit drifts, and the Guides CI re-renders on push).",
    only_if=[Tool("Edit", "Write", "MultiEdit", "NotebookEdit"), RenderSource()],
    events=Event.PreToolUse,
    max_fires=1,
    tests={
        Input(tool="Edit", file="/x/cc-squash/.claude/fragments/AGENTS.md/part-1.fragment.md", content="hi"): Warn(),
        Input(tool="Edit", file="/x/cc-squash/internal/cli/root.go", content="hi"): Allow(),
    },
)
