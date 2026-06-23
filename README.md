# cc-context

![cc-context banner](https://github.com/yasyf/cc-context/raw/main/.github/assets/readme-banner.webp)

[![Release](https://img.shields.io/github/v/release/yasyf/cc-context?sort=semver)](https://github.com/yasyf/cc-context/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/yasyf/cc-context/ci.yml?branch=main&label=ci)](https://github.com/yasyf/cc-context/actions/workflows/ci.yml)
[![License: PolyForm-Noncommercial-1.0.0](https://img.shields.io/badge/License-PolyForm--Noncommercial--1.0.0-blue.svg)](https://github.com/yasyf/cc-context/blob/main/LICENSE)

`cc-context` is a single Go binary — the `ccx` CLI and the `cc-context` MCP server — that feeds a coding agent the slice of a codebase it needs while spending as few tokens as it can. Its commands return compact, line-numbered views capped to a token budget, with an explicit marker when output is trimmed, so the context window goes to reasoning instead of re-reading whole files.

## Why

Reading entire files is the largest single drain on a coding agent's context, so `cc-context` swaps the token-heavy primitives for budgeted equivalents:

- `ccx outline` and `ccx read --section` return a file's structure or a single section, capped to a token budget, and print an explicit `… +N lines omitted` marker instead of truncating in silence.
- `ccx search` is one front door that routes by query kind: a natural-language question runs a semantic search, a code pattern with metavars like `$A` or `$$$` runs a structural one. You don't need the identifier up front, or a second command to switch modes.
- `ccx replace 'Add($A, $B)' 'Sum($A, $B)'` edits by structure and returns a diff — so an agent mutates code without first reading the file into context.
- `ccx symbol NewRootCmd` (alias `ccx grok`) returns a symbol's definition, doc, body, callers, callees, siblings, and tests in one call, in place of a dozen greps.
- `ccx diff` reviews changes structurally and reads both git and jj history.

To make an agent actually reach for these, the bundled [capt-hook](https://github.com/yasyf/captain-hook) guard pack blocks `cat`, raw `grep`, and full-file reads, then points it at the `ccx` equivalent.

## Install

Homebrew (macOS):

```bash
brew install yasyf/tap/ccx
```

Or with the Go toolchain:

```bash
go install github.com/yasyf/cc-context/cmd/ccx@latest
```

The semantic commands (`search`, `related`) shell out to [semble](https://github.com/MinishLab/semble) through `uvx`, so they need [uv](https://docs.astral.sh/uv/) on `PATH`. Structural search and `ccx replace` run on [ast-grep](https://ast-grep.github.io/), which the Homebrew formula installs as a dependency; off Homebrew, `ccx` finds it on `PATH` or downloads a pinned build on first use. The other structural commands use a pinned [tilth](https://github.com/jahala/tilth) binary, downloaded the same way. Nothing else to configure.

## Quickstart

Orient in a repository, search it by intent, then grok a result:

```console
$ ccx overview
[tilth] Go project — 31 source files, 4 directories
  dirs: cli/ backend/ render/ vcs/
  deps: asm, cobra, encoding, go-sdk, jsonschema-go, mousetrap, oauth2, pflag, sys, v3
  git: branch f7d52ab, 3 uncommitted files
  tests: _test.go
  manifest: go.mod (github.com/yasyf/cc-context)

$ ccx search "how does routing pick a backend"
{
  "query": "how does routing pick a backend",
  "results": [
    {
      "file_path": "internal/backend/backend.go",
      "start_line": 1,
      "end_line": 22,
      "score": 0.0204,
      "content": "// Package backend defines the stable logical-op surface ..."
    }
  ]
}

$ ccx symbol NewRootCmd
# grok: NewRootCmd [internal/cli/root.go:11]

## signature
func NewRootCmd() *cobra.Command

## doc
// NewRootCmd builds the root command and registers its subcommands.

## callees (12 internal, 3 extern)
  internal/cli/deps.go
    newDepsCmd   [9-23]   func newDepsCmd() *cobra.Command
  ...
```

The structural commands cap their output at `--budget` tokens. When one trims, it cuts on a line boundary and says how much it dropped:

```console
$ ccx outline internal/cli/root.go --budget 30
# internal/cli/root.go (36 lines, ~184 tokens) [full]

… +1 lines, ~12 tokens omitted — re-run with a larger --budget
```

### Search and edit by structure

A query that carries an ast-grep metavar (`$A`, `$$$`) routes to a structural search, which matches by shape and prints `file:line` per hit under a `# structural (ast-grep)` header:

```console
$ ccx search 'newReplaceCmd($$$)' internal/cli
# structural (ast-grep)
internal/cli/root.go:L29  newReplaceCmd()
```

`ccx replace` takes the same pattern plus a rewrite and previews the diff — it writes nothing until you add `--apply`. Try it on a throwaway file:

```console
$ printf 'package demo\n\nfunc f() { Add(1, 2) }\n' > /tmp/demo.go
$ ccx replace 'Add($A, $B)' 'Sum($A, $B)' /tmp/demo.go
# 1 matches across 1 files
/tmp/demo.go:3
- Add(1, 2)
+ Sum(1, 2)
```

Re-run with `--apply` to write the change, `--force` to bypass the 20-file safety cap. These three compose into a blind edit loop: `ccx symbol` to find a symbol, `ccx search` to locate its call sites by shape, `ccx replace` to rewrite them, all without reading a file into context.

## Commands

| Command | What it does |
| --- | --- |
| `ccx overview` | Repository structure and entry points; start here |
| `ccx search <query> [path]` | Search routed by query kind: natural-language runs semantic, a code pattern runs structural |
| `ccx replace <pattern> <rewrite> [paths...]` | Structural find-replace; previews a diff, writes only with `--apply` |
| `ccx related <file:line>` | Code semantically related to a location |
| `ccx symbol <name>` (alias `grok`) | Definition, doc, body, callers, callees, siblings, tests |
| `ccx outline <file>` | Token-budgeted structural outline of a file |
| `ccx read <file> --section A-B` | Read a line range, a `## Heading`, or the whole file with `--full` |
| `ccx deps <file>` | A file's imports and their resolved targets |
| `ccx grep <text> --glob G` | Literal text search, optionally globbed and budgeted |
| `ccx find <glob>` | List files matching a glob, with per-file token counts |
| `ccx diff [uncommitted\|staged\|<ref>]` | VCS-aware structural diff; defaults to uncommitted |

`search` picks its engine automatically, but `--semantic`, `--structural`, or `--literal` force one and `--explain` prints the routing decision to stderr. `replace` takes `--apply` to write, `--force` to bypass its file-count cap, and `--lang`/`--glob` to scope. `outline`, `read`, `deps`, `grep`, `diff`, and `replace` take `--budget N` to cap output; the semantic commands (`search`, `related`) use `-k` and `--max-snippet-lines` instead. Run `ccx <command> --help` for the full flag set, and `ccx --version` for the build version.

## Use it from Claude Code

The same commands are exposed as an MCP server. Add it to your `.mcp.json`:

```json
{
  "mcpServers": {
    "cc-context": {
      "command": "ccx",
      "args": ["mcp"]
    }
  }
}
```

Claude Code then sees eleven tools, one per command above: `mcp__cc-context__ccx_overview`, `mcp__cc-context__ccx_search`, `mcp__cc-context__ccx_symbol`, and the rest. `ccx_replace` previews by default, so an agent that omits `apply` gets a diff back and edits by structure without ever reading the file into context. Installed as a Claude Code plugin instead, the server runs `${CLAUDE_PLUGIN_ROOT}/bin/ccx mcp` and ships the capt-hook guard pack with it.

## How it works

Three engines sit behind one surface. semble answers the semantic ops (`search`, `related`); ast-grep answers structural search and `replace`; tilth answers the rest. `ccx search` classifies the query and routes it; every other command maps to a fixed engine. The mapping lives in `internal/router`, so there is nothing to select at runtime. Every result then passes through a render layer that estimates roughly four characters per token, trims on a line boundary when the output exceeds `--budget`, and appends the overflow marker. Output is never cut without saying so.

## Configuration

`ccx` reads no config file. Behavior is tuned through environment variables:

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
