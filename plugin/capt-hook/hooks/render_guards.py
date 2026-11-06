"""Guard the cc-guides rendered-artifact regime: warn on edits to a generated artifact or its render sources."""

from __future__ import annotations

import re
from pathlib import Path

from captain_hook import (
    Allow,
    BaseHookEvent,
    CustomCondition,
    Event,
    FileFixture,
    HookResult,
    Input,
    PreToolUseEvent,
    Tool,
    Warn,
    nudge,
    on,
)

CI_RENDER = re.compile(r"yasyf/cc-guides/\.github/workflows/guides\.yml@|cc-guides pr-check")
CI_RENDERED_FIXTURE = Path(__file__).parent / "fixtures" / "ci-rendered"


class CiRendered(CustomCondition):
    def check(self, evt: BaseHookEvent) -> bool:
        if (f := evt.file) is None:
            return False
        repo = next(
            (p.parent.parent for p in f.path.resolve().parents if p.parts[-2:] == (".claude", "fragments")),
            evt.ctx.repo_root,
        )
        if repo is None or not (workflow := repo / ".github" / "workflows" / "guides.yml").is_file():
            return False
        return bool(CI_RENDER.search(workflow.read_text(encoding="utf-8", errors="replace")))


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


@on(
    Event.PreToolUse,
    only_if=[Tool("Edit", "Write", "MultiEdit", "NotebookEdit"), RenderedArtifact()],
    max_fires=1,
    tests={
        Input(tool="Edit", file="/nope/internal/cli/root.go", content="x"): Allow(),
        Input(tool="Write", file=FileFixture(content="see the GENERATED docs\n", name="notes.md"), content="y"): Allow(),
    },
)
def steer_rendered_artifact_edit(evt: PreToolUseEvent) -> HookResult:
    if CiRendered().check(evt):
        return evt.context(
            "This file is a cc-guides rendered artifact that `.github/workflows/guides.yml` renders in CI, and its "
            "pr-check refuses a hand edit. Edit the matching parts under `.claude/fragments/` and commit only those."
        )
    return evt.context(
        "This file is a cc-guides rendered artifact, so a direct edit is discarded on the next render. "
        "Edit the matching parts under `.claude/fragments/`, then run `cc-guides render` and commit both together."
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
    skip_if=[CiRendered()],
    events=Event.PreToolUse,
    max_fires=1,
    tests={
        Input(tool="Edit", file="/x/cc-squash/.claude/fragments/AGENTS.md/part-1.fragment.md", content="hi"): Warn(
            pattern=r"Run `cc-guides render`, then commit the fragments and the artifact together"
        ),
        Input(tool="Edit", file="/x/cc-squash/internal/cli/root.go", content="hi"): Allow(),
        Input(
            tool="Edit", file=str(CI_RENDERED_FIXTURE / ".claude/fragments/AGENTS.md/part-1.fragment.md"), content="hi"
        ): Allow(),
    },
)

nudge(
    "This is a cc-guides render source, and `.github/workflows/guides.yml` renders the artifacts and lock in CI. "
    "Commit only the source edits; never commit `cc-guides render` output.",
    only_if=[Tool("Edit", "Write", "MultiEdit", "NotebookEdit"), RenderSource(), CiRendered()],
    events=Event.PreToolUse,
    max_fires=1,
    tests={
        Input(
            tool="Edit", file=str(CI_RENDERED_FIXTURE / ".claude/fragments/AGENTS.md/part-1.fragment.md"), content="hi"
        ): Warn(pattern=r"Commit only the source edits"),
        Input(tool="Edit", file="/x/cc-squash/.claude/fragments/AGENTS.md/part-1.fragment.md", content="hi"): Allow(),
    },
)
