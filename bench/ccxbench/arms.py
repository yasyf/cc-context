"""Build the per-(task, arm) workdir and the `claude -p` invocation.

Three arms run isolated — fresh config dir, ambient settings/MCP/plugins stripped — in a
fresh checkout with the same disallowed tools. `baseline` gets none of ccx. `ccx-mcp` serves
the cc-context facade MCP; `ccx-cli` serves zero MCP servers and reaches ccx only through the
Bash `ccx` on PATH — that zero-MCP isolation is what the cli-vs-mcp comparison leans on. Both
ccx arms get `ccx` on PATH, the capt-hook PreToolUse guards, and a length-matched ccx ladder
appended to the system prompt (baseline gets the matched native-tool control instead).
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

from spawnllm import ClaudeConfig, RunSpec

from .config import BENCH_DIR, Config
from .types import Task

CAPT_HOOK = "capt-hook>=3.14.0"
CCX_ARMS = ("ccx-mcp", "ccx-cli")
PATCHES_DIR = BENCH_DIR / "tasks" / "patches"

# The fixed tail every arm's child PATH ends with. `run_path` resolves `claude`/`uvx` off the
# host PATH ahead of these but skips the leakage vectors that corrupted the pilots (see below).
SYSTEM_PATH_DIRS = ("/opt/homebrew/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin")

# The baseline arm gets a `ccx` that fails like a host without ccx installed, so an attempted call
# surfaces in the transcript instead of silently resolving the brew ccx and contaminating the arm.
BASELINE_SHIM_STUB = '#!/bin/sh\necho "ccx: command not found" >&2\nexit 127\n'

# Length-matched (±15%) addenda so the paired delta isolates ccx, not the volume of advice:
# the MCP ladder names the mcp__cc-context__* tools, the CLI ladder the `ccx` commands.
ADDENDA_DIR = Path(__file__).resolve().parent
LADDER_MCP = (ADDENDA_DIR / "ladder_mcp.txt").read_text()
LADDER_CLI = (ADDENDA_DIR / "ladder_cli.txt").read_text()
BASELINE_CONTROL = (ADDENDA_DIR / "baseline_control.txt").read_text()
ADDENDA: dict[str, str] = {"baseline": BASELINE_CONTROL, "ccx-mcp": LADDER_MCP, "ccx-cli": LADDER_CLI}

GUARD_PROBE: dict[str, bool] = {}


def guard_command(cfg: Config) -> str:
    return f"uvx --from '{CAPT_HOOK}' capt-hook --hooks {cfg.plugin_hooks} run PreToolUse"


def guard_response_live(stdout: str) -> bool:
    """True if the hook's PreToolUse response denies or bounds the probe Read while naming ccx.

    The v0.8.0+ ccx pack answers an unbounded large Read two ways, both proof the guards loaded:
    an old-style `deny`, or an `allow` that rewrites the call via `updatedInput` to bound it
    (adding a `limit`). A pack that failed to import allows the Read unchanged — no `updatedInput`,
    no `ccx` mention — and is not live.
    """
    try:
        parsed = json.loads(stdout)
    except json.JSONDecodeError:
        return False
    hook_out = parsed.get("hookSpecificOutput") if isinstance(parsed, dict) else None
    if not isinstance(hook_out, dict):
        return False
    decision = hook_out.get("permissionDecision")
    updated = hook_out.get("updatedInput")
    bounded = decision == "allow" and isinstance(updated, dict) and "limit" in updated
    return (decision == "deny" or bounded) and "ccx" in stdout


def guards_available(cfg: Config) -> bool:
    """Probe once that the ccx guard pack is live against a synthetic >20 KB unbounded Read.

    Drives the exact PreToolUse path the ccx arms use (capt-hook + the canonical pack) and parses
    the hook's JSON response: guards are live when it denies the Read or allows a rewritten,
    bounded one, naming `ccx` either way (see `guard_response_live`). If the pack fails to import,
    the Read is allowed unchanged and the probe is False.
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
    # Probe against the SAME pinned PATH the ccx arms run under, so the hook resolves the same
    # `uvx`/`capt-hook` at probe time that it will at runtime — not whatever the host shell exposes.
    child_env = {**os.environ, "PATH": run_path(cfg, ccx=True)}
    try:
        proc = subprocess.run(
            ["uvx", "--from", CAPT_HOOK, "capt-hook", "--hooks", str(cfg.plugin_hooks), "run", "PreToolUse"],
            input=payload,
            cwd=tempfile.gettempdir(),
            capture_output=True,
            text=True,
            timeout=180,
            env=child_env,
        )
    except (subprocess.TimeoutExpired, FileNotFoundError):
        GUARD_PROBE[key] = False
        return False
    GUARD_PROBE[key] = guard_response_live(proc.stdout)
    return GUARD_PROBE[key]


def apply_edits(workdir: Path, task: Task) -> None:
    for edit in task.setup.get("edits", []):
        path = workdir / edit["file"]
        text = path.read_text()
        if edit["find"] not in text:
            raise ValueError(f"task {task.id}: setup find {edit['find']!r} absent from {edit['file']}")
        path.write_text(text.replace(edit["find"], edit["replace"], 1))


def apply_patch(workdir: Path, task: Task) -> None:
    """Apply the task's uncommitted-diff setup patch, if one exists, failing loud on reject."""
    patch = PATCHES_DIR / f"{task.id}.patch"
    if not patch.exists():
        return
    subprocess.run(["git", "apply", str(patch)], cwd=workdir, check=True)


def prepare_workdir(cfg: Config, task: Task, arm: str, run_id: str) -> Path:
    """Create a fresh checkout for one run and apply the task's setup edits/patch.

    `repo == "empty"` yields a bare workdir with no checkout (the non_regression control family).
    """
    workdir = cfg.work_root / run_id
    if workdir.exists():
        shutil.rmtree(workdir)
    if task.repo == "empty":
        workdir.mkdir(parents=True)
        return workdir
    src = cfg.fixtures_root / task.repo
    if not src.exists():
        raise FileNotFoundError(f"repo not staged: {src} (run `build-corpus` first)")
    shutil.copytree(src, workdir)
    # Defense in depth: the answer key must never reach a run, even from a stale checkout.
    (workdir / "manifest.json").unlink(missing_ok=True)
    apply_edits(workdir, task)
    apply_patch(workdir, task)
    return workdir


def mcp_config(cfg: Config, arm: str) -> str:
    """Serve the cc-context facade MCP for ccx-mcp only; every other arm gets zero MCP servers."""
    servers = {"cc-context": {"command": str(cfg.ccx_bin), "args": ["mcp"]}} if arm == "ccx-mcp" else {}
    return json.dumps({"mcpServers": servers})


def run_settings(cfg: Config, with_guards: bool) -> str:
    """--settings JSON for one run: pin ENABLE_TOOL_SEARCH off, and add the capt-hook guard hook when with_guards."""
    settings: dict = {"env": {"ENABLE_TOOL_SEARCH": "false"}}
    if with_guards:
        settings["hooks"] = {
            "PreToolUse": [{"hooks": [{"type": "command", "command": guard_command(cfg)}]}]
        }
    return json.dumps(settings)


def _path_excluded(d: str) -> bool:
    """True if a PATH dir is a known leakage vector, matched after symlink + trailing-slash normalize.

    Excluded: any `.venv`/`.venvs` path segment (harness/pyenv tool venvs shadowed the fixture's
    `click`), `/usr/local/bin` (legacy python2.7 tooling), and anything under `~/.superset` (the
    superset `claude` shim; over-exclusion there is fail-safe).
    """
    real = os.path.realpath(d)
    parts = real.split(os.sep)
    if ".venv" in parts or ".venvs" in parts:
        return True
    if real == "/usr/local/bin":
        return True
    superset = os.path.realpath(os.path.expanduser("~/.superset"))
    return real == superset or real.startswith(superset + os.sep)


def _tool_dir(name: str) -> str:
    """First host-PATH dir carrying an executable `name`, skipping the `_path_excluded` leakage vectors.

    Raises `LookupError` when `name` resolves only in excluded dirs — never silently falls back.
    Returns the PATH entry as written (not its realpath) so the composed child PATH is unsurprising.
    """
    for d in os.environ.get("PATH", "").split(os.pathsep):
        if not d or _path_excluded(d):
            continue
        candidate = Path(d) / name
        if candidate.is_file() and os.access(candidate, os.X_OK):
            return d
    raise LookupError(f"{name!r} not found in any non-excluded host PATH dir")


def baseline_shim_dir(session_dir: Path) -> Path:
    """The per-session dir holding the baseline arm's `ccx`-not-found shim."""
    return session_dir / "baseline-shim"


def ensure_baseline_shim(session_dir: Path) -> Path:
    """Create the baseline `ccx` shim (a stub that exits 127) and return its dir (idempotent)."""
    d = baseline_shim_dir(session_dir)
    d.mkdir(parents=True, exist_ok=True)
    stub = d / "ccx"
    stub.write_text(BASELINE_SHIM_STUB)
    stub.chmod(0o755)
    return d


def validate_ccx_bin(cfg: Config) -> str:
    """Assert the configured ccx binary exists and is executable; return its `--version` output.

    Fails loud at setup so a missing or mis-pinned `ccx_bin` never silently degrades a ccx arm to
    a no-ccx run — the whole comparison depends on the binary actually being there.
    """
    ccx = cfg.ccx_bin
    if not (ccx.is_file() and os.access(ccx, os.X_OK)):
        raise LookupError(f"configured ccx_bin is not an executable file: {ccx}")
    out = subprocess.run([str(ccx), "--version"], capture_output=True, text=True, timeout=30)
    return out.stdout.strip() or out.stderr.strip()


def run_path(cfg: Config, *, ccx: bool, shim_dir: Path | None = None) -> str:
    """Compose the minimal child PATH for one arm, ordered with first-wins dedupe.

    Order: a leading dir (the ccx binary's dir for ccx arms, else `shim_dir` for baseline), then
    the resolved `claude` and `uvx` dirs, then `SYSTEM_PATH_DIRS`. spawnllm merges
    `os.environ | backend env | spec.env` with spec.env winning, so this pins exactly which
    `claude`/`uvx`/`ccx` a bare-argv run resolves — baseline's leading shim making `ccx` fail loud.
    """
    if ccx:
        lead = [str(cfg.ccx_bin.parent)]
    elif shim_dir is not None:
        lead = [str(shim_dir)]
    else:
        lead = []
    dirs = lead + [_tool_dir("claude"), _tool_dir("uvx"), *SYSTEM_PATH_DIRS]
    seen: set[str] = set()
    ordered = [d for d in dirs if not (d in seen or seen.add(d))]
    return os.pathsep.join(ordered)


def build_run_spec(cfg: Config, task: Task, arm: str, model: str, workdir: Path, *, shim_dir: Path) -> RunSpec:
    """Build the spawnllm RunSpec for one headless run.

    `shim_dir` is the session's baseline `ccx`-not-found shim; it leads the baseline arm's PATH and
    is ignored for ccx arms (which lead with the real ccx binary's dir).
    """
    ccx = arm in CCX_ARMS
    # spawnllm inherits os.environ, so force ENABLE_TOOL_SEARCH off (it leaks true from the dev
    # shell) and pin a minimal PATH (spec.env wins the merge) so no host dir shadows the tools.
    env: dict[str, str] = {"ENABLE_TOOL_SEARCH": "false", "PATH": run_path(cfg, ccx=ccx, shim_dir=shim_dir)}
    settings = run_settings(cfg, with_guards=ccx and guards_available(cfg))
    # A task may carry a per-arm ladder addendum (the exec-vs-pipeline diagnostic); otherwise none.
    addendum = ADDENDA[arm] + task.arm_addenda.get(arm, "")
    return RunSpec(
        prompt=task.prompt,
        model=model,
        isolated=True,
        schema=task.schema,
        cwd=str(workdir),
        env=env,
        timeout=cfg.timeout_s,
        provider_configs={
            "claude": ClaudeConfig(
                permission_mode=cfg.permission_mode,
                mcp_config=mcp_config(cfg, arm),
                strict_mcp=cfg.strip_mcp,
                append_system_prompt=addendum,
                settings=settings,
                disallowed_tools=tuple(cfg.disallowed_tools),
                max_turns=cfg.max_turns,
            )
        },
    )
