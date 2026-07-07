# ![cc-context](docs/assets/readme-banner.webp)

**Take `cat` away from your agent.** Guard hooks block cat, raw grep, and full-file reads and rewrite each into a token-budgeted ccx call for an outline, a symbol with its callers, or a diff.

[![CI](https://img.shields.io/github/actions/workflow/status/yasyf/cc-context/ci.yml?branch=main&label=ci)](https://github.com/yasyf/cc-context/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yasyf/cc-context?sort=semver)](https://github.com/yasyf/cc-context/releases)
[![License PolyForm-Noncommercial-1.0.0](https://img.shields.io/badge/License-PolyForm--Noncommercial--1.0.0-blue.svg)](LICENSE)

## Get started

```
/plugin marketplace add yasyf/cc-skills
/plugin install cc-context@skills
```

<img src="docs/assets/demo.png" alt="Terminal running 'ccx code outline internal/astgrep/run.go --budget 60' — a syntax-highlighted file outline cut at the budget, ending in '+5 lines, ~111 tokens omitted'" width="700">

The plugin arrives wired together. The `ccx` binary self-provisions, the MCP server auto-registers its `mcp__cc-context__ccx_*` tools plus `BashFormat`, the guard hooks turn on, and the `ccx` skill teaches the reach-for-`ccx`-first workflow. It needs [`uv`](https://docs.astral.sh/uv/) on `PATH`, since the hooks run through `uvx capt-hook` and semantic search shells out to [semble](https://github.com/MinishLab/semble) via `uvx`.

Driving with an agent? Paste this:

```
/plugin marketplace add yasyf/cc-skills
/plugin install cc-context@skills
```

<details>
<summary>Using ccx outside Claude Code? Install the standalone CLI</summary>

The Homebrew formula pulls in ast-grep and uv:

```bash
brew install yasyf/tap/ccx
```

Without the plugin, register the MCP server by hand:

```bash
claude mcp add --scope user --transport stdio cc-context -- ccx mcp
```

</details>

---

## Use cases

### Orient in an unfamiliar repo in one command

Cold-starting in a codebase you've never seen burns the first few thousand tokens on `ls` and `cat` wandering. One command replaces the tour:

```bash
ccx repo overview
```

```console
[tilth] Go project — 125 source files, 10 directories
  dirs: cli/ codeexec/ format/ render/ backend/ astgrep/ anchor/ vcs/ cache/ grok/
  deps: asm, cobra, encoding, go-sdk, jsonschema-go, mousetrap, oauth2, pflag
  hot (× = importers): plugin/hooks/common.py ×3
  git: branch main, clean
  tests: tests/, _test.go, test_*.py
  manifest: go.mod (github.com/yasyf/cc-context)
```

Structure, dependencies, the hottest files by importer count, and VCS state, in a few hundred tokens instead of a directory crawl.

### Pull one symbol with its callers, not the whole file

Understanding one function shouldn't cost a whole-file read plus a grep for its call sites. Ask for the symbol:

```bash
ccx code symbol NewRootCmd
```

```console
# grok: NewRootCmd [internal/cli/root.go:11]

## signature
func NewRootCmd() *cobra.Command

## doc
// NewRootCmd builds the root command and registers its subcommands.

## body
…

## callees (4 internal, 5 extern)
…

## callers (5 of 6)
  internal/cli/cli_test.go
    [30]   in TestRootHelpListsAllOps()
…
```

Definition, doc, body, callees, and callers arrive in one budgeted response, so the agent edits code it never paged through.

### Keep JSON output out of context

`gh --json` and `kubectl -o json` dump verbose, nested JSON that floods the context window. Run the command through `ccx format` and its JSON or NDJSON stdout comes back re-encoded in the leanest accurate shape:

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

The ~11,000-character outline of 35 files stayed in the sandbox; only the answer came back. In the spike's four replayed agent episodes, that pattern cut the characters entering context by 12-99×, measured as a raw character delta, separate from the [benchmark suite](bench/README.md). Scripts use a restricted Python subset; `ccx exec --list-tools` prints the host-function catalog and the full rules. The MCP facade exposes the same surface as `ccx_exec`.

## The guard pack enforces it

A budgeted command only helps if the agent reaches for it. The bundled [capt-hook](https://github.com/yasyf/captain-hook) guard pack makes that the path of least resistance. Its `PreToolUse`/`PostToolUse` hooks block `cat`, raw `grep`, unbounded full-file `Read`s, and pager-bound `git diff`s, then point the agent at the `ccx` equivalent. It also watches for JSON. A command flagged for JSON output (`--json`, `-o json`) gets rewritten to run through `ccx format`, and the pack learns which commands emit JSON so it can nudge you to wrap them next time.

## Commands

Each command is a token-bounded stand-in for a primitive an agent would otherwise reach for. Structural output is capped at `--budget` tokens, cut on a line boundary, with an explicit marker saying how much was dropped. The daily drivers:

| Command | What it does |
| --- | --- |
| `ccx repo overview` | Repository structure and entry points; start here |
| `ccx code search <query> [path]` | Search routed by query kind; natural language runs semantic, an ast-grep pattern (`$A`, `$$$`) runs structural |
| `ccx code symbol <name>` (alias `grok`) | Definition, doc, body, callers, callees, siblings, tests |
| `ccx code outline <file-or-dir>` | Token-budgeted structural outline of a file or directory |
| `ccx code read <file> --section A-B` | Read a line range or a `## Heading` instead of the whole file |
| `ccx vcs diff [uncommitted\|staged\|<ref>]` | VCS-aware structural diff; defaults to uncommitted |
| `ccx format [-- <cmd>]` | Re-encode a command's JSON/NDJSON output lean, or filter a pipe |
| `ccx exec [script]` | Compose ccx ops, `sh()`, and reflected MCP tools in a sandbox; only the return value enters context |

`ccx --help` catalogs the rest, including structural find-replace with a preview-first `--apply`, dependency maps, per-commit symbol history, and `ccx vcs ship` to commit, push, and watch CI in one call; `ccx <command> --help` has the flags. Three engines sit behind the one surface. semble handles semantic search, ast-grep handles structural search and rewrites, and tilth handles everything else; `ccx` routes each command for you.

## Configuration

`ccx` reads no config file; behavior is tuned through environment variables:

| Variable | Effect |
| --- | --- |
| `LOG_LEVEL` / `LOG_FORMAT` | `debug`, `info` (default), `warn`, or `error`, to stderr; set `LOG_FORMAT=json` for structured logs |
| `CCX_EXEC_MCP` | set to `off` to disable MCP auto-reflection in `ccx exec` |
| `CCX_EXEC_MCP_DENY` | comma-separated MCP server names to exclude from reflection; reflected servers run as fresh instances, so list any that need live session state |
| `CCX_EXEC_MCP_ALLOW` | comma-separated MCP server names to reflect even when classified stateful |

Licensed under [PolyForm Noncommercial 1.0.0](LICENSE).
