# cc-context

![cc-context banner](https://github.com/yasyf/cc-context/raw/main/.github/assets/readme-banner.webp)

[![Release](https://img.shields.io/github/v/release/yasyf/cc-context?sort=semver)](https://github.com/yasyf/cc-context/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/yasyf/cc-context/ci.yml?branch=main&label=ci)](https://github.com/yasyf/cc-context/actions/workflows/ci.yml)
[![License: PolyForm-Noncommercial-1.0.0](https://img.shields.io/badge/License-PolyForm--Noncommercial--1.0.0-blue.svg)](https://github.com/yasyf/cc-context/blob/main/LICENSE)

Feed a coding agent only the codebase slice it needs, on a token budget.

Reading whole files is the largest single drain on a coding agent's context window. `cc-context` — the `ccx` CLI and the `cc-context` MCP, one Go binary — swaps the token-heavy primitives (`cat`, raw `grep`, full-file reads) for commands that return a file's structure, one section, or a symbol with its callers and callees. Every result is capped to a token budget and prints an explicit marker when it trims, so the window goes to reasoning instead of re-reading files.

## Install

The Claude Code plugin is the whole product. Install it and everything below arrives wired together, with nothing else to set up.

```
/plugin marketplace add yasyf/cc-skills
/plugin install cc-context@skills
```

That gets you:

- **The `ccx` binary, self-provisioning.** A shim downloads the pinned release on first use and caches it in `${CLAUDE_PLUGIN_DATA}`, which survives plugin updates. No Homebrew, no manual build.
- **The MCP server, auto-registered.** The `mcp__cc-context__ccx_*` tools (plus `BashToon`) appear the moment the plugin is enabled, and disable with it.
- **The guard pack, wired.** `PreToolUse`/`PostToolUse` hooks that block the token-heavy primitives and rewrite them to their `ccx` equivalents. See [the guard pack](#the-guard-pack-enforces-it).
- **The `ccx` skill.** Teaches the agent the reach-for-`ccx`-first workflow.

Requires [`uv`](https://docs.astral.sh/uv/) on `PATH`: the hooks run through `uvx capt-hook`, and semantic search shells out to [semble](https://github.com/MinishLab/semble) via `uvx`. ast-grep downloads a pinned build on first use.

### Standalone CLI

To run `ccx` outside Claude Code, install the binary with Homebrew. The formula pulls in ast-grep and uv:

```bash
brew install yasyf/tap/ccx
```

Without the plugin, register the MCP server by hand:

```bash
claude mcp add --scope user --transport stdio cc-context -- ccx mcp
```

## Quickstart

Orient in a repo, find code by intent, then grok the result:

```console
$ ccx overview
[tilth] Go project — 45 source files, 8 directories
  dirs: cli/ backend/ astgrep/ render/ querykind/ vcs/ mcpserver/ search/
  hot (× = importers): bench/ccxbench/types.py ×8, plugin/hooks/common.py ×4
  git: branch d6c15a5, clean
  manifest: go.mod (github.com/yasyf/cc-context)

$ ccx search "how is the mcp server started" -k 1 --max-snippet-lines 1
# semantic (semble)
{"query": "how is the mcp server started", "results": [{"file_path": "internal/mcpserver/server.go", "start_line": 103, "end_line": 115, "score": 0.032, "content": "func Serve(ctx context.Context) error {"}]}

$ ccx symbol NewRootCmd
# grok: NewRootCmd [internal/cli/root.go:11]
func NewRootCmd() *cobra.Command
# plus doc, body, callees (13 internal, 3 external), and callers (5)
```

Every structural command caps its output at `--budget` tokens, cuts on a line boundary, and says how much it dropped:

```console
$ ccx outline internal/astgrep/run.go --budget 60
# ast-grep
# internal/astgrep/run.go
L14  applyFileCap = 20
L18  astGrepExitNoMatch = 1
L24  func Run(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
L38  func runStructural(ctx context.Context, a backend.Args) (string, error) {
… +4 lines, ~85 tokens omitted — re-run with a larger --budget
```

A query carrying an ast-grep metavar (`$A`, `$$$`) routes to structural search. `ccx replace` rewrites the matches and previews a diff, writing nothing until `--apply` — so an agent edits code it never read into context:

```console
$ printf 'package demo\n\nfunc f() { Add(1, 2) }\n' > /tmp/demo.go
$ ccx replace 'Add($A, $B)' 'Sum($A, $B)' /tmp/demo.go
# 1 matches across 1 files
/tmp/demo.go:3
- Add(1, 2)
+ Sum(1, 2)
```

## The guard pack enforces it

A budgeted command only helps if the agent reaches for it. The bundled [capt-hook](https://github.com/yasyf/captain-hook) guard pack makes that the path of least resistance. Its `PreToolUse`/`PostToolUse` hooks block the token-heavy primitives — `cat`, raw `grep`, an unbounded full-file `Read`, a `git diff` through a pager — and point the agent at the `ccx` equivalent instead. Reach for the raw tool and the hook turns you back; reach for `ccx` and you stay inside the budget by default.

The pack also watches for JSON. A command flagged for JSON output (`--json`, `-o json`) gets rewritten to run through `ccx toon`, and the pack learns which commands emit JSON so it can nudge you to wrap them next time.

## Keep JSON output out of context

`gh --json`, `kubectl -o json`, and friends dump verbose, nested JSON that floods the context window. `ccx toon` re-encodes it: run any command as `ccx toon -- <cmd …>` and its JSON or NDJSON stdout comes back as TOON, a compact tabular encoding (or compact JSON when that is smaller), typically 40–60% fewer tokens on tabular data. Non-JSON output passes through verbatim, stderr streams live, and the command's exit code is propagated. It also works as a pipe filter:

```console
$ echo '[{"name":"api","status":"running","replicas":3},{"name":"web","status":"running","replicas":2}]' | ccx toon
[2]{name,status,replicas}:
  api,running,3
  web,running,2
```

The MCP `BashToon` tool is the same wrapper in tool form.

## Commands

Each command is a token-bounded stand-in for a primitive an agent would otherwise reach for:

| Command | What it does |
| --- | --- |
| `ccx overview` | Repository structure and entry points; start here |
| `ccx search <query> [path]` | Search routed by query kind: natural-language runs semantic, a code pattern runs structural |
| `ccx replace <pattern> <rewrite> [paths...]` | Structural find-replace; previews a diff, writes only with `--apply` |
| `ccx related <file:line>` | Code semantically related to a location |
| `ccx symbol <name>` (alias `grok`) | Definition, doc, body, callers, callees, siblings, tests |
| `ccx outline <file-or-dir>` | Token-budgeted structural outline of a file or directory |
| `ccx read <file> --section A-B` | Read a line range, a `## Heading`, or the whole file with `--full` |
| `ccx deps <file>` | Symbols a file uses, and what uses it back |
| `ccx grep <text> --glob G` | Literal text search, optionally globbed and budgeted |
| `ccx find <glob>` | List files matching a glob, with per-file token counts |
| `ccx diff [uncommitted\|staged\|<ref>]` | VCS-aware structural diff; defaults to uncommitted |
| `ccx toon [-- <cmd>]` | Re-encode a command's JSON/NDJSON output as TOON, or filter a pipe |

Run `ccx <command> --help` for the full flag set, and `ccx --version` for the build version. Three engines sit behind the one surface — semble for semantic search, ast-grep for structural search and rewrites, tilth for the rest — and `ccx` routes each command for you.

## Configuration

`ccx` reads no config file; behavior is tuned through environment variables:

| Variable | Effect |
| --- | --- |
| `LOG_LEVEL` | `debug`, `info` (default), `warn`, or `error`; logs go to stderr |
| `LOG_FORMAT` | set to `json` for structured logs |
| `CLAUDE_PLUGIN_DATA` | cache directory for the downloaded `ccx`, tilth, and ast-grep binaries |

## Development

```bash
task build   # -> bin/ccx
task test    # go test -race ./...
task lint    # golangci-lint
```

Conventions and architecture live in [AGENTS.md](AGENTS.md) and [STYLEGUIDE.md](STYLEGUIDE.md).

## License

[PolyForm Noncommercial License 1.0.0](LICENSE).
