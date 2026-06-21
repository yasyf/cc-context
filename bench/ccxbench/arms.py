"""Build the per-(task, arm) workdir and the `claude -p` invocation.

Both arms run in the default Claude config dir (auth is keychain-bound), in a fresh
copy of the fixture repo, with ambient MCP stripped (--strict-mcp-config) and the same
disallowed tools. The ONLY differences are the ccx arm's facade MCP, its `ccx` on PATH,
the ccx ladder appended to the system prompt, and — when the guard pack loads — the
capt-hook PreToolUse guards. Guard availability is probed once; if the pack fails to
load it is reported, not silently assumed active.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
from pathlib import Path

from .config import Config
from .fixtures import FIXTURE_NAME
from .types import Task

LADDER = (Path(__file__).resolve().parent / "ladder.txt").read_text()
# Both arms get matched, length-comparable "navigate efficiently" guidance so the paired
# delta isolates ccx's tools/guards rather than the generic frugality advice in the ladder.
BASELINE_CONTROL = (Path(__file__).resolve().parent / "baseline_control.txt").read_text()

GUARD_PROBE: dict[str, bool] = {}


def guard_command(cfg: Config) -> str:
    return f"uvx capt-hook --hooks {cfg.plugin_hooks} run PreToolUse"


def guards_available(cfg: Config) -> bool:
    """Probe once whether the ccx guard pack loads and blocks a large Read."""
    key = str(cfg.plugin_hooks)
    if key in GUARD_PROBE:
        return GUARD_PROBE[key]
    big = cfg.fixtures_root / FIXTURE_NAME / "internal" / "gen" / "generated.go"
    if not big.exists():
        GUARD_PROBE[key] = False
        return False
    payload = json.dumps(
        {
            "hook_event_name": "PreToolUse",
            "tool_name": "Read",
            "tool_input": {"file_path": str(big)},
            "cwd": str(cfg.fixtures_root / FIXTURE_NAME),
        }
    )
    try:
        proc = subprocess.run(
            ["uvx", "capt-hook", "--hooks", str(cfg.plugin_hooks), "run", "PreToolUse"],
            input=payload,
            capture_output=True,
            text=True,
            timeout=120,
        )
    except (subprocess.TimeoutExpired, FileNotFoundError):
        GUARD_PROBE[key] = False
        return False
    out = (proc.stdout + proc.stderr).lower()
    available = ("deny" in out or "block" in out) and "ccx" in out
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
    src = cfg.fixtures_root / FIXTURE_NAME
    if not src.exists():
        raise FileNotFoundError(f"fixture not built: {src} (run `build-corpus` first)")
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


def guard_settings(cfg: Config) -> str:
    return json.dumps(
        {"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": guard_command(cfg)}]}]}}
    )


def build_command(cfg: Config, task: Task, arm: str, model: str, workdir: Path) -> tuple[list[str], dict[str, str], Path]:
    """Return (argv, env, cwd) for the headless run. No shell; the prompt is a literal arg."""
    argv = [
        "claude",
        "-p",
        task.prompt,
        "--output-format",
        "json",
        "--model",
        model,
        "--json-schema",
        json.dumps(task.schema),
        "--permission-mode",
        cfg.permission_mode,
        "--mcp-config",
        mcp_config(cfg, arm),
    ]
    if cfg.strip_mcp:
        argv.append("--strict-mcp-config")
    if cfg.disallowed_tools:
        argv += ["--disallowedTools", *cfg.disallowed_tools]

    env = dict(os.environ)
    if arm == "ccx":
        argv += ["--append-system-prompt", LADDER]
        if guards_available(cfg):
            argv += ["--settings", guard_settings(cfg)]
        env["PATH"] = f"{cfg.ccx_bin.parent}{os.pathsep}{env.get('PATH', '')}"
    else:
        argv += ["--append-system-prompt", BASELINE_CONTROL]
    return argv, env, workdir
