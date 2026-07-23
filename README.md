# ![cc-context](docs/assets/readme-banner.webp)

**Give your agent bounded context.** Guard hooks catch `cat`, raw `grep`, and full-file reads, rewriting the mappable ones into budgeted ccx calls — line-numbered, token-capped, never silently truncated — and windowing or blocking the rest.

[![CI](https://img.shields.io/github/actions/workflow/status/yasyf/cc-context/ci.yml?branch=main&label=ci)](https://github.com/yasyf/cc-context/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yasyf/cc-context?sort=semver)](https://github.com/yasyf/cc-context/releases)
[![License PolyForm-Noncommercial-1.0.0](https://img.shields.io/badge/License-PolyForm--Noncommercial--1.0.0-blue.svg)](LICENSE)

## Get started

```
/plugin marketplace add yasyf/captain-hook
/plugin marketplace add yasyf/cc-context
/plugin install cc-context@cc-context
```

The captain-hook marketplace comes first: the plugin declares [captain-hook](https://github.com/yasyf/captain-hook) as a dependency, and it auto-installs only when its marketplace is already known. Upgrading an existing install? `claude plugin update` silently skips newly added dependencies; add the captain-hook marketplace, then re-run `/plugin install cc-context@cc-context`.

<img src="docs/assets/demo.png" alt="Terminal running 'ccx code outline internal/astgrep/run.go --budget 60' — a syntax-highlighted file outline cut at the budget, ending in '+5 lines, ~111 tokens omitted'" width="700">

The plugin arrives wired together. The `ccx` binary self-provisions, the MCP server auto-registers its `mcp__cc-context__ccx_*` tools plus `BashFormat`, the guard hooks turn on, the `ccx` skill teaches the reach-for-`ccx`-first workflow, and five read-only reader agents (`web-fetch`, `web-researcher`, `ci-triage`, `dep-reader`, `enumerator`) stand by to read pages, logs, and dependency source in their own context and hand back only cited conclusions. It needs [`uv`](https://docs.astral.sh/uv/) on `PATH`, since the hooks run through `uvx capt-hook`.

Driving with an agent? Paste this:

```
/plugin marketplace add yasyf/captain-hook
/plugin marketplace add yasyf/cc-context
/plugin install cc-context@cc-context
```

<details>
<summary>Using ccx outside Claude Code? Install the standalone CLI</summary>

The Homebrew formula pulls in ast-grep and uv:

```bash
brew install yasyf/tap/ccx
```

Release-tarball and `go install` installs need `ast-grep` ≥ 0.44 on PATH for the structural ops; install it with `brew install ast-grep` or `uv tool install ast-grep-cli`.

Without the plugin, register the MCP server by hand:

```bash
claude mcp add --scope user --transport stdio cc-context -- ccx mcp
```

</details>

---

## Use cases

### Orient in an unfamiliar repo in one command

Need the lay of the land before a deep dive? One command draws the map:

```bash
ccx repo overview
```

```console
# cc-context — go module github.com/yasyf/cc-context (go 1.26)
languages: go (217), py (67), md (31), rs (13), sh (5)
dirs: internal (31 pkgs: anchor, astgrep, backend, cache, …) · bench (4 pkgs: analysis, ccxbench, tasks, tests) · format-core (3 pkgs: core, corpus, wasm) · plugin (4 pkgs: agents, hooks, scripts, skills) · docs (2 pkgs: assets, scripts) · cmd/ccx · scripts
entry: cmd/ccx/main.go
manifests: go.mod (15 direct deps)
tests: 123 test files (go, py)
git: @ cf13457c "fix(lint): gosec/revive/staticcheck cleanups in read and overview" · 46 dirty · 262 commits
hot (90d): bench/tasks (233), plugin/hooks (167), internal/cli (154), bench/ccxbench (123), internal/web (105)
```

Structure, entry points, manifests, recent churn, and VCS state in a few hundred tokens. Skip it when you already know what you're looking for — a targeted search or symbol lookup beats a tour.

### Locate one symbol without paging through the file

Where does this function live, what's its contract, and how big is its blast radius? One call answers all three:

```bash
ccx code symbol NewRootCmd
```

```console
# symbol NewRootCmd — function — internal/cli/root.go:18-46#dezd
func NewRootCmd() *cobra.Command

NewRootCmd builds the root command and registers its subcommands.

refs 36 · tests 22 · siblings 11 — --callers/--tests/--siblings/--body/--full
```

Location, signature, and doc land in ~60 tokens; the counts trailer says whether expanding is worth a second call, and `--callers`, `--body`, or `--full` pull exactly the layer you need.

### Keep JSON output out of context

`gh --json` and `kubectl -o json` dump verbose, nested JSON that can swamp a context window. Run the command through `ccx format` and its JSON or NDJSON stdout comes back re-encoded in the leanest accurate shape:

```bash
ccx format -- gh release list --limit 5 --json tagName,publishedAt,isLatest
```

```console
|isLatest|publishedAt|tagName|
|---|---|---|
|true|2026-07-04T02:11:12Z|v0.4.0|
|false|2026-07-04T00:02:50Z|v0.3.0|
|false|2026-06-26T01:05:28Z|v0.2.1|
|false|2026-06-23T07:52:46Z|v0.2.0|
|false|2026-06-21T09:21:47Z|v0.1.1|
```

That's 40% fewer bytes than the raw JSON; grow the same table to 400 rows and it comes back as CSV at 60% off. A classifier reads the payload shape and picks among markdown table, CSV, TOON, TRON, JSONL, compact JSON, and, for prose-dominant payloads, the prose itself. Auto output never exceeds compact JSON by bytes; `--format=X` forces one encoder even when it's larger. Non-JSON output passes through verbatim, stderr streams live, and the exit code is propagated. The MCP `BashFormat` tool is the same wrapper in tool form.

### Fan out a dozen calls, get six lines back

Every command above bounds or compresses the output of a single call. `ccx exec` goes one tier further. It runs a short Python script in a sandbox whose async host functions are every ccx query op, a gated `sh(cmd)`, and the tools of every stateless MCP server registered with Claude Code. The script fans out calls, filters in the sandbox, and returns one value; only that value enters context, run through the same shape classifier as `ccx format` and capped at `--budget`:

```bash
ccx exec '
import asyncio
import re
async def main():
    raw = await outline("internal/cli")
    cmds = sorted(set(re.findall(r"func (new\w+Cmd)", raw)))
    return {"subcommands": len(cmds), "constructors": cmds}
asyncio.run(main())
'
```

```console
{"constructors":["newCodeCmd","newDepsCmd","newDiffCmd","newExecCmd","newFindCmd","newFormatCmd","newGrepCmd","newHistoryCmd","newLocateCmd","newMCPCmd","newOutlineCmd","newOverviewCmd","newReadCmd","newRelatedCmd","newReplaceCmd","newRepoCmd","newSearchCmd","newShipCmd","newShowCmd","newSymbolCmd","newVcsCmd"],"subcommands":21}
```

The ~11,000-character outline of 35 files stayed in the sandbox; only the answer came back. In the spike's four replayed agent episodes, that pattern cut the characters entering context by 12-99×, measured as a raw character delta, separate from the [benchmark suite](bench/README.md) — where agents never reached for `exec` unprompted. It pays when your prompts or project guides steer composition through it. The full measured story — where ccx costs tokens, where it saves them, and where it wins on accuracy instead — lives in [bench/FINDINGS.md](bench/FINDINGS.md). Scripts use a restricted Python subset; `ccx exec --list-tools` prints the host-function catalog and the full rules. The MCP facade exposes the same surface as `ccx_exec`.

## The guard pack enforces it

A budgeted command only helps if the agent reaches for it. The bundled [capt-hook](https://github.com/yasyf/captain-hook) guard pack makes that the path of least resistance. Its `PreToolUse`/`PostToolUse` hooks rewrite simple token-heavy commands in place — `cat` runs as `ccx code read`, a `sed` line range as `--section`, a literal `grep` (or a dialect-safe regex one, carried through as `--regex`) as `ccx code grep`, bare `git diff`/`git show`/`git log -p` as their `ccx vcs` equivalents, an unpiped `curl` page dump as `ccx web read` — each with a note saying what ran instead. A `grep` naming explicit files is rewritten when ccx can map it, and otherwise passes through only while those files stay under the large-read size threshold; over that threshold, or on a tree-wide shape with no faithful mapping (an exotic regex, an exit-code `grep -q`, an untranslatable flag), it blocks with a pointer at the `ccx` equivalent, and an unbounded full-file `Read` gets a hundred-line window plus a steer to `ccx code outline`. Whole-page `WebFetch`es keep the hard block — a hook can't swap one tool for another — and the block message names the drop-in instead: spawn the bundled `cc-context:web-fetch` agent with the same URL and prompt, and only the cited answer enters context. A deliberate re-run of the same URL still passes, so pages `ccx web` can't serve stay reachable. It also watches for JSON. A command flagged for JSON output (`--json`, `-o json`) gets rewritten to run through `ccx format`, and the pack learns which commands emit JSON so it can nudge you to wrap them next time.

When Claude Code defers MCP tools behind tool search (`ENABLE_TOOL_SEARCH`), the everyday tools — `ccx_code_read`, `ccx_code_grep`, `ccx_code_outline`, `ccx_code_search` — are marked `alwaysLoad`, so they stay in the prompt from the first turn and a guard redirect to one costs no tool-search round-trip. The rest of the surface stays deferred, loaded on demand.

## Commands

Each command is a token-bounded stand-in for a primitive an agent would otherwise reach for. Structural output is capped at `--budget` tokens, cut on a line boundary, with an explicit marker saying how much was dropped. The daily drivers:

| Command | What it does |
| --- | --- |
| `ccx repo overview` | Repository structure and entry points, for untargeted starts |
| `ccx code search <query> [path]` | Search routed by query kind; natural language runs semantic, an ast-grep pattern (`$A`, `$$$`) runs structural |
| `ccx code symbol <name>` (alias `grok`) | Location, signature, doc + caller/callee counts; flags expand body, callers, tests |
| `ccx code outline <file-or-dir>` | Token-budgeted structural outline of a file or directory |
| `ccx code read <file> --section A-B` | Read a line range or a `## Heading` instead of the whole file |
| `ccx vcs diff [uncommitted\|staged\|<ref>]` | VCS-aware structural diff; defaults to uncommitted |
| `ccx web outline <url>` | Heading tree of a web page with stable `§` section refs |
| `ccx web read <url> --section <ref>` | Read one section of a page, with prev/next nav, instead of the whole thing |
| `ccx web search <url> "<question>"` | Ask a page a question; top-k relevant chunks with `§` cites |
| `ccx format [-- <cmd>]` | Re-encode a command's JSON/NDJSON output lean, or filter a pipe |
| `ccx exec [script]` | Compose ccx ops, `sh()`, and reflected MCP tools in a sandbox; only the return value enters context |

`ccx --help` catalogs the rest, including structural find-replace with a preview-first `--apply`, dependency maps, per-commit symbol history, and `ccx vcs ship -m "<msg>" [paths...]` to commit, push, and watch CI in one call — it runs the repo's prek pre-commit hooks first via `uvx prek` (auto-fixes fold into the commit, a persistent failure aborts before anything lands, `--no-verify` skips), trailing paths scope the commit to just those files, `ccx vcs hunks` refs with `--skip-hunk`/`--only-hunk` scope it to individual hunks of a file, the push auto-advances only the trunk bookmark, and commits made from a Claude session carry a `Claude-Session-Id` trailer; `ccx <command> --help` has the flags. Two external engines sit behind the one surface — ast-grep for structural search, rewrites, and outlines, and ripgrep for every text grep (system `grep` fills in when `rg` is missing) — and everything else runs natively inside `ccx`: read, symbol, deps, diff, history, overview, and web are computed fresh from the working tree on every call, and semantic search embeds and ranks in-process (model weights download once into a local cache on first use). `ccx` routes each command for you.

## Configuration

`ccx` reads no config file; behavior is tuned through environment variables:

| Variable | Effect |
| --- | --- |
| `LOG_LEVEL` / `LOG_FORMAT` | `debug`, `info` (default), `warn`, or `error`, to stderr; set `LOG_FORMAT=json` for structured logs |
| `CCX_EXEC_MCP` | `off` disables MCP auto-reflection in `ccx exec`; `refresh` forces a fresh `claude mcp list` probe, bypassing the 15-minute per-project inventory cache |
| `CCX_EXEC_MCP_DENY` | comma-separated MCP server names to exclude from reflection; reflected servers run as fresh instances, so list any that need live session state |
| `CCX_EXEC_MCP_ALLOW` | comma-separated MCP server names to reflect even when classified stateful |
| `CCX_EXEC_MCP_TIMEOUT` | Go duration bounding the `claude mcp list` probe (default `30s`); a probe that fails past the cache TTL falls back to the last good inventory |

Licensed under [PolyForm Noncommercial 1.0.0](LICENSE).
