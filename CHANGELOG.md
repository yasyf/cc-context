# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Brew-first self-provisioning plugin installer: at session start the plugin resolves `ccx` via Homebrew when available, otherwise downloads the bare release binary and verifies its sha256 checksum. The downloaded payload lives under `CLAUDE_PLUGIN_DATA`, durable across plugin updates; `plugin/bin` holds only symlinks.
- Bare per-arch binaries published on each release alongside the archives, with sha256 checksums — the artifact the installer downloads and verifies.
- `ccx format [-- <cmd>]` re-encodes JSON/NDJSON (a wrapped command's stdout, or stdin as a pipe filter) into the leanest encoding for its shape, picked by a classifier: payloads under 200 bytes stay compact JSON; a prose-dominant payload unwraps to the prose plus XML-ish metadata tags; a uniform array of objects becomes a markdown table (small) or a CSV/TSV byte shootout (large), with TOON entering only at 100+ rows when it beats both; repeated nested shapes become TRON; heterogeneous arrays become JSONL; everything else stays compact JSON. Auto output never exceeds compact JSON by bytes; `--format=X` forces one encoder. Exposed over MCP as `BashFormat`.
- A TRON encoder: repeated key-sets compile to class declarations (`class A: region,zone,tier`) with each instance a positional call.

### Changed
- `ccx --version` on release builds prints the exact release tag (e.g. `v0.5.0`).
- `ccx exec` structured returns ride the same classifier as `ccx format` instead of always rendering as TOON or compact JSON.

### Removed
- The version-pinned bootstrap shim (`plugin/bin/ccx`); the self-provisioning installer replaces it.
- `ccx toon`, the `BashToon` MCP tool, and `--force-toon`; `ccx format`, `BashFormat`, and `--format=toon` replace them with no back-compat alias.
- The `ccx hello` placeholder command.

## [0.4.0] - 2026-07-03

### Added
- `ccx exec [script]` runs a Python (monty-subset) script in a sandbox whose async host functions are every ccx query op, a gated `sh(cmd)`, and every stateless MCP server's tools (auto-reflected from `claude mcp list`, no flag needed). Only the script's return value enters context, rendered as TOON or compact JSON and capped at `--budget`. Scripts arrive as an argument, `--file`, or stdin; `--list-tools` prints the host-function catalog and the Python-subset rules. Unavailable on Intel Macs (darwin/amd64 — the embedded monty runtime ships no library there); every other command works.
- `ccx_exec` and `ccx_exec_tools` MCP tools expose the exec surface on the facade, backed by a resident engine.
- `CCX_EXEC_MCP=off` disables MCP auto-reflection; `CCX_EXEC_MCP_DENY` / `CCX_EXEC_MCP_ALLOW` (comma-separated server names) override the stateless classifier. Built-in denies: cc-context itself, `plugin:cc-review:*`, and any command whose basename is `ccx`.

### Changed
- Binaries grow from ~11 MB to ~25–27 MB on monty-supported targets (the embedded Python runtime).

### Fixed
- Command results now print to stdout, not stderr (a cobra wiring bug). Behavior change: scripts that captured results from stderr must read stdout instead.
- Plugin hooks no longer run twice when the host project also wires capt-hook: the plugin attaches its pack once at SessionStart and dispatches every event through the canonical command Claude Code can dedup.

## [0.3.0] - 2026-07-03

### Added
- `ccx vcs ship [-m <msg>]` — jj-aware commit, push, and `gh run watch --exit-status` in one call.
- `ccx vcs show [ref]` — commit message plus a structural per-file diff of one commit.
- `ccx vcs history <path>` — per-commit changed symbols for a file, rename-aware.
- `ccx repo locate <name>` — resolve a sibling repo, Go module, or Python package to its on-disk path; exit 3 when unresolved.
- `ccx toon [-- <cmd>]` re-encodes a command's JSON/NDJSON stdout as TOON (or compact JSON when smaller), passes non-JSON through verbatim, and propagates the exit code; also a pipe filter. Exposed over MCP as `BashToon`. A guard auto-rewrites JSON-flagged commands (`--json`, `-o json`) to run through it.
- Content-hash anchors (`#hash`) on spans across outline, grep, symbol, diff, and search output.
- Guard pack: new blocks on `head`/`tail` of files, `git show`, `git`/`jj log -p`, pager diffs, and manifest `cat`, plus a stateful session gate against full-file and post-edit re-reads.

### Changed
- Breaking: commands are namespaced into `ccx code` / `ccx repo` / `ccx vcs` groups (`ccx outline` becomes `ccx code outline`, `ccx diff` becomes `ccx vcs diff`, and so on) with no back-compat aliases; `ccx toon` and `ccx mcp` stay top-level. MCP tools renamed to match (`ccx_code_read`, `ccx_vcs_diff`, and the rest).

## [0.2.1] - 2026-06-25

### Changed
- `ccx outline` routes through ast-grep, falling back to tilth.

### Fixed
- Structural diffs splice raw textual hunks under tilth's empty `(0 symbols)` sections, jj-aware.
- `ccx symbol` falls back to an ast-grep lookup when tilth misses a Go top-level type declaration.
- tilth's silent "not found" result now surfaces as a real error on both the CLI and MCP surfaces.

## [0.2.0] - 2026-06-23

### Added
- `ccx replace <pattern> <rewrite>` — ast-grep structural find-replace: preview by default, `--apply` to write, `--force` past the 20-file cap. Exposed over MCP as `ccx_replace`.
- `ccx search` routes by query kind: natural language runs semantic (semble), an ast-grep metavar pattern runs structural. `--semantic`/`--structural`/`--literal` override the route; `--explain` prints it.
- ast-grep is bundled through the same resolver as tilth: configured binary, then PATH, then a checksum-verified pinned download.
- Guard pack blocks raw `grep` and auto-rewrites `cat`, `sed` ranges, `ls -R`, and `find` enumeration to their ccx equivalents.

### Changed
- Distribution moved from a goreleaser cask to a Homebrew formula (`brew install yasyf/tap/ccx`) with ast-grep and uv as dependencies.

## [0.1.1] - 2026-06-21

### Changed
- `ccx outline` elides function bodies (signature mode) — roughly 75% smaller output.
- Search snippets default to 10 lines instead of the full chunk.
- `ccx overview` appends a language census.
- The unbounded-Read guard threshold dropped from 50 KB to 20 KB.

### Fixed
- `ccx related` splits `file:line` into the two arguments semble expects.
- The plugin bootstrap shim requested the wrong release asset name and 404'd.

## [0.1.0] - 2026-06-20

### Added
- Initial release: the `ccx` CLI and the `cc-context` facade MCP, a single Go binary over swappable backends — semble for semantic search, tilth for symbols, outlines, and diffs — with token-budgeted output, line numbers, and explicit overflow markers.
- jj-aware diff translation.
- Claude Code plugin: facade-only MCP registration, a bootstrap shim that provisions the `ccx` binary, a capt-hook guard pack that blocks token-heavy primitives (unbounded `Read`, bare `cat`, `ls -R`, broad `git diff`) with escape hatches, and the `ccx` skill.

[Unreleased]: https://github.com/yasyf/cc-context/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/yasyf/cc-context/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/yasyf/cc-context/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/yasyf/cc-context/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/yasyf/cc-context/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/yasyf/cc-context/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/yasyf/cc-context/releases/tag/v0.1.0
