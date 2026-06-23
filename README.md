# cc-context

![cc-context banner](https://github.com/yasyf/cc-context/raw/main/.github/assets/readme-banner.webp)

[![Release](https://img.shields.io/github/v/release/yasyf/cc-context?sort=semver)](https://github.com/yasyf/cc-context/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/yasyf/cc-context/ci.yml?branch=main&label=ci)](https://github.com/yasyf/cc-context/actions/workflows/ci.yml)
[![License: PolyForm-Noncommercial-1.0.0](https://img.shields.io/badge/License-PolyForm--Noncommercial--1.0.0-blue.svg)](https://github.com/yasyf/cc-context/blob/main/LICENSE)

The `ccx` CLI and `cc-context` MCP — feed a coding agent only the codebase slice it needs, on a token budget.

`cc-context` is a single Go binary. Its commands return compact, line-numbered views capped to a token budget and print an explicit marker when they trim, so an agent's context window goes to reasoning instead of re-reading whole files.

## Why

Reading whole files is the largest single drain on a coding agent's context. `cc-context` swaps the token-heavy primitives — full-file reads, `cat`, raw `grep` — for budgeted equivalents that return a file's structure, a single section, or a symbol with its callers and callees, never more than a `--budget` of tokens. To make an agent reach for them, the bundled [capt-hook](https://github.com/yasyf/captain-hook) guard pack blocks `cat`, raw `grep`, and full-file reads, then points it at the `ccx` equivalent.

## Install

Install with Homebrew (recommended):

```bash
brew install yasyf/tap/ccx
```

Or build it with the Go toolchain:

```bash
go install github.com/yasyf/cc-context/cmd/ccx@latest
```

The semantic commands (`search`, `related`) shell out to [semble](https://github.com/MinishLab/semble) through `uvx`; structural search and `ccx replace` run on [ast-grep](https://ast-grep.github.io/). The Homebrew formula installs both [uv](https://docs.astral.sh/uv/) and ast-grep as dependencies; off Homebrew, uv must be on `PATH` and `ccx` finds ast-grep there or downloads a pinned build on first use — the same as the pinned [tilth](https://github.com/jahala/tilth) binary it uses for everything else. Nothing else to configure.

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

Point `ccx outline` at a directory for a structural map across every file. The languages ast-grep outlines route to its `outline`; everything else falls back to tilth signatures.

A query carrying an ast-grep metavar (`$A`, `$$$`) routes to structural search; `ccx replace` rewrites the matches and previews a diff, writing nothing until `--apply` and stopping at 20 files unless you pass `--force`. An agent edits code it never read into context:

```console
$ printf 'package demo\n\nfunc f() { Add(1, 2) }\n' > /tmp/demo.go
$ ccx replace 'Add($A, $B)' 'Sum($A, $B)' /tmp/demo.go
# 1 matches across 1 files
/tmp/demo.go:3
- Add(1, 2)
+ Sum(1, 2)
```

## Commands

Eleven commands, each a token-bounded stand-in for a primitive an agent would otherwise reach for:

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

Run `ccx <command> --help` for the full flag set, and `ccx --version` for the build version.

## Use it from Claude Code

The same commands are exposed as an MCP server, one tool per command. Register it once, available in every project:

```bash
claude mcp add --scope user --transport stdio cc-context -- ccx mcp
```

The tools mirror the CLI 1:1 (`mcp__cc-context__ccx_overview` and the rest). `ccx_replace` previews by default, so an agent that omits `apply` gets a diff back and edits by structure without reading the file. Installed as a Claude Code plugin instead, the server runs `${CLAUDE_PLUGIN_ROOT}/bin/ccx mcp` and ships the capt-hook guard pack with it.

## How it works

Three engines sit behind one surface: [semble](https://github.com/MinishLab/semble) answers the semantic ops (`search`, `related`), [ast-grep](https://ast-grep.github.io/) answers structural search and `replace`, and [tilth](https://github.com/jahala/tilth) answers the rest. `ccx search` classifies the query and routes it; every other command maps to a fixed engine. Each result then passes through a render layer that estimates roughly four characters per token, trims on a line boundary when the output exceeds `--budget`, and appends the overflow marker. Output is never cut without saying so.

## Configuration

`ccx` reads no config file; behavior is tuned through environment variables:

| Variable | Effect |
| --- | --- |
| `LOG_LEVEL` | `debug`, `info` (default), `warn`, or `error`; logs go to stderr |
| `LOG_FORMAT` | set to `json` for structured logs |
| `CLAUDE_PLUGIN_DATA` | cache directory for the downloaded tilth and ast-grep binaries |

## Development

```bash
task build   # -> bin/ccx
task test    # go test -race ./...
task lint    # golangci-lint
```

Conventions and architecture live in [AGENTS.md](AGENTS.md) and [STYLEGUIDE.md](STYLEGUIDE.md).

## License

[PolyForm Noncommercial License 1.0.0](LICENSE).
