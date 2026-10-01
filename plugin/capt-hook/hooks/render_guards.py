"""Guard the cc-guides rendered-artifact regime: block edits to a generated artifact, nudge edits to its render sources."""

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
    def check(self, evt: BaseHookEvent) -> bool:
        f = evt.file
        if f is None or not f.is_file() or (root := evt.ctx.repo_root) is None:
            return False
        path = f.path.resolve()
        if not path.is_relative_to(root.resolve()):
            return False
        rel = path.relative_to(root.resolve())
        if rel.parts[:2] == (".claude", "fragments"):
            return False
        if not (root / ".claude" / "fragments" / rel / "layout.toml").is_file():
            return False
        with f.path.open(encoding="utf-8", errors="replace") as fh:
            head = fh.readline() + fh.readline()
        return bool(re.search(r"cc-guides \S+ src=\S+( fragments=\S+)? \| GENERATED", head))


hook(
    Event.PreToolUse,
    "This file is a cc-guides rendered artifact, so a direct edit is discarded on the next render. "
    "Edit the matching parts under `.claude/fragments/`, then run `cc-guides render` and commit both together.",
    only_if=[Tool("Edit", "Write", "MultiEdit", "NotebookEdit"), RenderedArtifact()],
    block=True,
    tests={
        Input(tool="Edit", file="/nope/internal/cli/root.go", content="x"): Allow(),
        Input(tool="Write", file=FileFixture(content="see the GENERATED docs\n", name="notes.md"), content="y"): Allow(),
    },
)


class RenderSource(CustomCondition):
    def check(self, evt: BaseHookEvent) -> bool:
        f = evt.file
        if f is None:
            return False
        if f.under(".claude/fragments"):
            return True
        root = evt.ctx.repo_root
        if root is None or root.name != "cc-skills":
            return False
        path = f.path.resolve()
        return path.is_relative_to(root.resolve()) and path.relative_to(root.resolve()).parts[:1] == ("guides",)


nudge(
    "This is a cc-guides render source, so the rendered artifact stays stale until re-rendered. "
    "Run `cc-guides render`, then commit the fragments and the artifact together.",
    only_if=[Tool("Edit", "Write", "MultiEdit", "NotebookEdit"), RenderSource()],
    events=Event.PreToolUse,
    max_fires=1,
    tests={
        Input(tool="Edit", file="/x/cc-squash/.claude/fragments/AGENTS.md/part-1.fragment.md", content="hi"): Warn(),
        Input(tool="Edit", file="/x/cc-squash/internal/cli/root.go", content="hi"): Allow(),
    },
)
