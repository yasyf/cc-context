# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.10.0] - 2026-07-10

### Added
- Dependency-source search: an explicitly anchored `--glob` (a literal directory or file prefix, e.g. `.venv/lib/…/pkg/*.py`) is searched even where ignore rules would hide it, on both the tilth and ripgrep engines and across CLI, MCP, and `ccx exec`. The anchor composes with an explicit `--scope`, and a glob naming an exact file anchors to its parent directory — ripgrep's explicit-path semantics throughout (`--no-ignore-parent` on the rg route).
- `ccx repo locate` resolves Python import names (`cc_transcript` ⇄ `cc-transcript`) and emits both a `repo` row (sibling checkout) and a `package` row (installed site-packages directory + `importlib.metadata` version, interpreter resolved `$VIRTUAL_ENV` → `./.venv` → PATH). The Python row's kind is now `package` (was `python`).
- Guard pack 0.2.0: unpiped `rg` is gated at grep parity — literal-safe invocations rewrite to `ccx code grep` (context flags map to `--expand=<count>`), unmappable ones block with a dependency-source steer (`ccx repo locate <pkg>` → `ccx code grep/outline/read`) — with an exemption when every explicit target is a data file (`.log`/`.json`/`.yaml`/…). Hidden-segment and git-ignored path operands now block for both `grep` and `rg` instead of rewriting to a glob a stale binary would silently 0-match.

### Fixed
- A no-match literal `ccx code grep` no longer exits 2 with tilth's `not found: <path>/<query>` path-fallback error — it prints the house no-match output; a nonexistent `--scope` still fails loudly.
- `ccx repo find --scope` reaches the tilth CLI route (the scope was silently dropped).

## [0.9.0] - 2026-07-09

### Added
- Guard pack: two hooks protecting the cc-guides rendered-artifact regime. A direct `Edit`/`Write`/`MultiEdit`/`NotebookEdit` of a rendered artifact is **blocked** — the predicate fires only when a sibling `.claude/fragments/<repo-relative-target>/layout.toml` exists AND the target's first two lines carry the `cc-guides … | GENERATED` banner, so an unmanaged file or a file that merely contains the word GENERATED is never touched — with a message steering to the fragments plus `cc-guides render`. An edit to a render **source** (any file under `.claude/fragments/`, or `guides/` in the cc-skills content repo) draws a one-shot nudge to re-render and commit the fragments and regenerated artifact together.

## [0.8.0] - 2026-07-08

### Added
- `-i`/`--ignore-case` and `-w`/`--word` on `ccx code grep`, routed to PATH-resolved ripgrep (`--json`, fixed-strings) and reshaped into the house grep format so anchors and budget capping apply unchanged. System `grep -rnFI` is the fallback when `rg` is absent, with filesystem-validated line parsing; hidden and binary files are skipped and `.gitignore` is not applied, disclosed in an engine note. Wired across the CLI, MCP (`ignoreCase`/`word`/`scope` on `ccx_code_grep`), `ccx exec`'s `grep()`, and the proxy dispatch.
- `--scope <path>` on `ccx code grep`, passed through to tilth.
- The plugin installer best-effort ensures ripgrep (`brew install ripgrep`, backgrounded) at session start; skipped silently without brew.
- Guard pack: block-only hooks now rewrite mappable commands in place via `updatedInput`, each with a disclosure note — raw `grep` → `ccx code grep`, bare `git diff`/`git show`/`git log -p <path>`/`jj diff` → the `ccx vcs` equivalents, unpiped `curl`/`wget` page dumps → `ccx web read --full`, unbounded large `Read`s → a 100-line window. Unmappable shapes keep the original block message; rewrites that need a newer binary gate on a `ccx_supports` probe of the installed CLI.

### Fixed
- `ccx vcs show <ref>` resolves git symbolic refs (HEAD, HEAD~N, branches, tags) in jj-colocated repos instead of handing them to `jj log -r`; embedded-`@` sources (`release@1` vs `main@origin`) classify by attempted git resolution, consistently across the show and diff paths.
- rg engine hardening: positional paths ride behind `--` so a flag-like scope cannot be misparsed; base64 `bytes` payloads in `rg --json` output decode instead of emitting blank match lines; the grep fallback's path validation requires regular files, so a directory named like a path prefix cannot steal the split.

### Changed
- CI builds on Go 1.26.5 (GO-2026-5856), uses `actions/cache@v5`, and runs the guard-pack pytest suite over the whole `plugin/hooks/` directory.

## [0.7.0] - 2026-07-08

### Added
- `ccx web` op family: `outline <url>` (heading tree with stable `§` section refs), `read <url> --section <ref>` (budget-capped section subtree with prev/next nav; `--full` for the whole page), and `search <url> "<question>"` (top-k relevant chunks with `§` cites; hybrid BM25 + local embeddings). Pages cache for 24h; `--refresh` refetches. Mirrored over MCP as `ccx_web_outline`/`ccx_web_read`/`ccx_web_search`.

## [0.6.1] - 2026-07-07

### Fixed
- The `ccx exec` host-call size valve now covers structured returns (maps and lists), not only strings — a large non-string host return no longer slips the per-call limit.

## [0.6.0] - 2026-07-07

### Added
- `ccx exec` works on Intel Macs (darwin/amd64) — every platform with uv now runs the sandbox.

### Changed
- The exec sandbox runs pydantic-monty 0.0.18 in a per-run Python subprocess launched via uv (already a formula runtime dependency). The pinned interpreter is 9 releases newer than the embedded binding and includes the upstream fixes for the partial-future-resolution bugs it needed a workaround for.
- Binaries shrink from ~25–27 MB to ~11 MB with the embedded runtime gone.

### Removed
- The embedded gomonty/monty runtime and its dylib.
- macOS notarization and the disable-library-validation entitlement (no dylib left to exempt).

## [0.5.1] - 2026-07-06

### Changed
- Format classifier: a prose-like field of 2 KiB or more unwraps to prose regardless of its share of the payload — a big body (release notes, a PR description) reads better unwrapped than TRON-compressed, even when metadata rides along.
- `ccx format` auto mode keeps the classifier's ranking on near-ties: a later candidate must beat an earlier one by more than 5% in bytes to displace it. The guard that auto output never exceeds compact JSON is unchanged.
- The plugin installer provenance stamp points at the canonical cc-skills template.

### Fixed
- `ccx exec` host calls awaited via `asyncio.gather` could execute more than once: the embedded runtime re-awaits still-pending calls after a partial resume, re-running the host function — a duplicated side-effecting tool call (`sh()`, an MCP tool). Each waiter now memoizes its result, so every host call runs exactly once.

## [0.5.0] - 2026-07-05

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

[0.8.0]: https://github.com/yasyf/cc-context/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/yasyf/cc-context/compare/v0.6.1...v0.7.0
[0.6.1]: https://github.com/yasyf/cc-context/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/yasyf/cc-context/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/yasyf/cc-context/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/yasyf/cc-context/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/yasyf/cc-context/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/yasyf/cc-context/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/yasyf/cc-context/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/yasyf/cc-context/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/yasyf/cc-context/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/yasyf/cc-context/releases/tag/v0.1.0
