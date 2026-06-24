"""Build the per-(task, arm) workdir and the `claude -p` invocation.

Both arms run in the default Claude config dir (auth is keychain-bound), in a fresh copy of
the fixture repo, with ambient settings and MCP stripped via spawnllm's flag-only isolation
(--setting-sources "" + --strict-mcp-config) and the same disallowed tools. The ONLY differences are the ccx arm's facade MCP, its `ccx` on PATH,
the ccx ladder appended to the system prompt, and — when the guard pack loads — the
capt-hook PreToolUse guards. Guard availability is probed once; if the pack fails to
load it is reported, not silently assumed active.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

from spawnllm import ClaudeConfig, RunSpec

from .config import Config
from .fixtures import FIXTURE_NAME
from .types import Task

CAPT_HOOK = "capt-hook>=3.14.0"

LADDER = (Path(__file__).resolve().parent / "ladder.txt").read_text()
# Both arms get matched, length-comparable "navigate efficiently" guidance so the paired
# delta isolates ccx's tools/guards rather than the generic frugality advice in the ladder.
BASELINE_CONTROL = (Path(__file__).resolve().parent / "baseline_control.txt").read_text()

GUARD_PROBE: dict[str, bool] = {}


def guard_command(cfg: Config) -> str:
    return f"uvx --from '{CAPT_HOOK}' capt-hook --hooks {cfg.plugin_hooks} run PreToolUse"


def guards_available(cfg: Config) -> bool:
    """Probe once that the ccx guard pack is live: a large unbounded Read must be denied.

    Drives the exact PreToolUse path the ccx arm uses (capt-hook + the canonical pack) against a
    synthetic >20 KB file. A `deny` whose reason names `ccx` proves the cc-context navigation
    guards loaded and fire; if the pack fails to import, the Read is allowed and the probe is False.
    """
    key = str(cfg.plugin_hooks)
    if key in GUARD_PROBE:
        return GUARD_PROBE[key]
    if not (cfg.plugin_hooks / "read_guards.py").exists():
        GUARD_PROBE[key] = False
        return False
    probe_file = Path(tempfile.gettempdir()) / "ccx_guard_probe_large.py"
    probe_file.write_text("# probe\n" + "x = 1\n" * 5000)
    payload = json.dumps(
        {"hook_event_name": "PreToolUse", "tool_name": "Read", "tool_input": {"file_path": str(probe_file)}}
    )
    try:
        proc = subprocess.run(
            ["uvx", "--from", CAPT_HOOK, "capt-hook", "--hooks", str(cfg.plugin_hooks), "run", "PreToolUse"],
            input=payload,
            cwd=tempfile.gettempdir(),
            capture_output=True,
            text=True,
            timeout=180,
        )
    except (subprocess.TimeoutExpired, FileNotFoundError):
        GUARD_PROBE[key] = False
        return False
    available = '"permissionDecision": "deny"' in proc.stdout and "ccx" in proc.stdout
    GUARD_PROBE[key] = available
    return available


def apply_edits(workdir: Path, task: Task) -> None:
    for edit in task.setup.get("edits", []):
        path = workdir / edit["file"]
        text = path.read_text()
        if edit["find"] not in text:
            raise ValueError(f"task {task.id}: setup find {edit['find']!r} absent from {edit['file']}")
        path.write_text(text.replace(edit["find"], edit["replace"], 1))


def prepare_workdir(cfg: Config, task: Task, arm: str, run_id: str) -> Path:
    """Create a fresh fixture checkout for one run and apply the task's setup edits."""
    src = cfg.fixtures_root / (FIXTURE_NAME if task.repo == "fixture" else task.repo)
    if not src.exists():
        raise FileNotFoundError(f"repo not staged: {src} (run `build-corpus` first)")
    workdir = cfg.work_root / run_id
    if workdir.exists():
        shutil.rmtree(workdir)
    shutil.copytree(src, workdir)
    # Defense in depth: the answer key must never reach a run, even from a stale fixture.
    (workdir / "manifest.json").unlink(missing_ok=True)
    apply_edits(workdir, task)
    return workdir


def mcp_config(cfg: Config, arm: str) -> str:
    servers = {"cc-context": {"command": str(cfg.ccx_bin), "args": ["mcp"]}} if arm == "ccx" else {}
    return json.dumps({"mcpServers": servers})


def run_settings(cfg: Config, with_guards: bool) -> str:
    """Command-line --settings JSON for one spawned run.

    The spawned session's cwd is a `.work/` checkout INSIDE this repo, so Claude Code loads the
    repo's project `.claude/settings.json` (and the user settings), whose env sets
    ENABLE_TOOL_SEARCH=true — which re-defers the cc-context MCP tools behind a ToolSearch
    discovery turn, an asymmetric tax on the ccx arm that RunSpec.env alone cannot beat. A
    command-line --settings env block outranks project and user settings, so force tool-search
    off here for both arms (a no-op for the MCP-less baseline). The ccx arm additionally carries
    the capt-hook guard PreToolUse hook.
    """
    settings: dict = {"env": {"ENABLE_TOOL_SEARCH": "false"}}
    if with_guards:
        settings["hooks"] = {
            "PreToolUse": [{"hooks": [{"type": "command", "command": guard_command(cfg)}]}]
        }
    return json.dumps(settings)


def build_run_spec(cfg: Config, task: Task, arm: str, model: str, workdir: Path) -> RunSpec:
    """Build the spawnllm RunSpec for one headless run.

    spawnllm delivers the prompt via stdin and owns transient-overload retry. The ONLY
    differences between arms are the ccx arm's facade MCP, its `ccx` prepended to PATH, the
    ccx ladder appended to the system prompt, and — when the pack loads — the guard settings.
    """
    ccx = arm == "ccx"
    # ENABLE_TOOL_SEARCH=false keeps the spawned session's tools eager: cc-context's MCP
    # tools list up front instead of hiding behind a ToolSearch discovery turn. Set for both
    # arms so the paired delta isolates ccx, not tool deferral; a no-op for the MCP-less baseline.
    env: dict[str, str] = {"ENABLE_TOOL_SEARCH": "false"}
    if ccx:
        env["PATH"] = f"{cfg.ccx_bin.parent}{os.pathsep}{os.environ.get('PATH', '')}"
    settings = run_settings(cfg, with_guards=ccx and guards_available(cfg))
    return RunSpec(
        prompt=task.prompt,
        model=model,
        schema=task.schema,
        cwd=str(workdir),
        env=env,
        timeout=cfg.timeout_s,
        provider_configs={
            "claude": ClaudeConfig(
                permission_mode=cfg.permission_mode,
                mcp_config=mcp_config(cfg, arm),
                strict_mcp=cfg.strip_mcp,
                append_system_prompt=LADDER if ccx else BASELINE_CONTROL,
                settings=settings,
                disallowed_tools=tuple(cfg.disallowed_tools),
            )
        },
    )
