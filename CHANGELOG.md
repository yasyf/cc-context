# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- **`ccx vcs ship` auto-tracks an untracked trunk bookmark on jj.** In a fresh
  `jj git init --colocate` repo the remote trunk (`main@origin`) arrives untracked,
  so `jj git fetch` never advanced the local bookmark — leaving ship's divergence
  check blind — and `jj git push --bookmark exact:main` refused outright with
  "Non-tracking remote bookmark". Before fetching, ship now scans
  `jj bookmark list <target> --all-remotes` for an untracked same-name counterpart
  and runs `jj bookmark track <target> --remote=<remote>` against the remote the
  counterpart actually sits on — honoring a non-origin remote instead of a
  hard-coded `origin`, and tracking the push target when several remotes carry one.
  The name is passed as an exact string pattern, so a bookmark carrying an `@`
  tracks correctly. Tracking mutates no working-copy state, so a later push refusal
  still leaves the working copy untouched.
- **`ccx vcs ship` survives a remote that advances mid-ship, on both backends.** A
  concurrent push landing between ship's fetch and its push previously surfaced as a
  raw rejection (`... (non-fast-forward)`) — and on jj repos left the local bookmark
  advanced, so even a manual ship re-run tripped the conflicted-bookmark refusal.
  Ship now classifies a rejected push (git's `(non-fast-forward)`/`(fetch first)`
  per-ref reasons; jj's "unexpectedly moved") and re-fetches, re-rebases, and
  re-pushes, up to 3 attempts; the jj lane first reverts its own bookmark move via a
  targeted `jj op revert`, so retries and manual re-runs both start from a clean
  bookmark and a concurrent local session's operations are never rolled back with
  it. Exhausted retries fail with the exact manual recovery steps instead of raw
  stderr.
- **`ccx vcs ship --skip-hunk` refuses on snapshot drift instead of sweeping in a
  foreign hunk.** Skip mode ("commit everything except the named hunks") fail-opened:
  a hunk written to a selected file between `ccx vcs hunks` and the commit was
  silently committed anyway. Ship now fingerprints each hunk-scoped file by its
  listed hunk digests and, at commit time on both backends — the jj diff-tool lane
  carries the set through the selection plan, the git temp-index lane reads it in
  process — refuses a skip-mode commit that carries a hunk absent from that set,
  naming the foreign hunk(s). Only mode is unaffected: its foreign hunks stay
  uncommitted by construction, which is the commit-around-a-concurrent-session
  workflow.

### Changed
- **The git lane of `ccx vcs ship` gains the fetch-first flow the jj lane already
  had**: fetch, ancestor check, and `git rebase --autostash` onto `origin/<branch>`
  before the push; a conflicting rebase aborts back to the pre-rebase state and
  reports the conflicted files; an autostash that conflicts on pop is surfaced with
  `git stash pop` instructions instead of being parked silently. The lane targets
  the branch's configured remote (`branch.<name>.remote`, falling back to origin)
  for the fetch, the rebase base, the push, and the report, instead of hard-coding
  origin. A rejected `--amend` push never auto-retries on either backend, and the
  git amend push now tries a plain push first and force-pushes only with a lease
  pinned to the commit being rewritten — an externally refreshed tracking ref (an
  IDE background fetch, say) can no longer turn the lease into a silent overwrite
  of a concurrent push.

## [0.31.0] - 2026-07-23

### Added
- **AST chunking for TSX, CSS, SCSS, and Vue.** New tree-sitter grammars, pinned to
  the same upstream revisions as semble's language pack, replace line-window chunking
  for these extensions; the embedded grammar set grows ~330 KB to ~4.4 MB. The
  63-repo/1251-query quality gate re-ran benchmark-neutral (overall NDCG@10 delta vs
  semble unchanged at −0.0007).

### Changed
- **`ccx web search` hybrid embeddings now run the native in-process engine.** The
  per-call `uv`/Python model2vec subprocess is gone; web search shares the resident
  WASM engine machinery with code search, on the same pinned `potion-base-8M` model,
  so cached page vectors stay valid. `uv` is no longer required for hybrid ranking —
  without weights (empty cache, offline) web search degrades to BM25-only, and a
  stalled weights download now fails within a bounded window (~5 minutes, the old
  subprocess path's first-run bound, restored for the native engine) instead of
  hanging engine construction.
- **The model-weights cache is namespaced per model** (`models/<repo>/<revision>`),
  making room for the second (web) model; the code model re-downloads once (~30 MB)
  on the first search after upgrade.

### Fixed
- **Gitignore-negated files with unmapped extensions now index** (line-chunked)
  instead of silently vanishing; a repo whose only file is such a file no longer
  errors `no indexable files`.
- **Index-cache crash safety.** The persisted index's manifest, chunks, and vectors
  now share a per-write generation nonce; a crash between the three writes leaves a
  torn cache that rebuilds instead of silently pairing new chunks with stale
  vectors. Existing caches rebuild once (schema bump).
- **Malformed UTF-8 decodes with Python parity end to end.** Invalid byte sequences
  produce one U+FFFD per maximal invalid subsequence (Python `errors="replace"`
  semantics) — including on the production indexing path, which previously collapsed
  each invalid run into a single replacement — matching semble's chunk boundaries
  for malformed files.
- **Embedding calls honor context cancellation** promptly when queued behind other
  work (an in-flight sub-15 ms WASM encode still runs to completion by design).

## [0.30.0] - 2026-07-23

### Changed
- **`ccx code search` and `ccx code related` now run a native in-process semantic
  engine.** The previous implementation shelled out to the external Python `semble`
  package through `uvx`; the whole pipeline — tree-sitter AST chunking, model2vec
  embeddings via an embedded WASM module, hand-rolled BM25, RRF fusion, and a
  code-tuned ranking stack — now runs inside the `ccx` binary, so no external
  process is spawned and `uv` is no longer required for search. Model weights
  (`potion-code-16M-v2`, a pinned HuggingFace revision) download once into a local
  cache on first use. On the semble benchmark (63 repos, 1251 queries, same
  machine) the native engine matches semble's ranking quality (NDCG@10 0.853) and
  is faster on both cold index build and warm query latency. AST chunking covers 19
  languages; other file types fall back to line-window chunking.

### Added
- **Per-request `--content` filter on `ccx code search` and `ccx code related`**, and
  on the MCP `search`/`related` tools (space-separated `code|docs|config|all`,
  default `code docs`) — narrowing by content type is now available over MCP, not
  just the CLI.

## [0.29.0] - 2026-07-21

### Added
- **`ccx code grep --files-with-matches`.** The new `-l` mode prints only the
  relative paths of matching files, with the usual budget cap and explicit overflow
  footer. Glob, scope, case, word, and regex filters remain available; context flags
  are rejected because file-only output has no match frames to expand.
- **`ccx code grep` auto-escalates to regex on zero literal matches.** The literal pass
  runs exactly as before; when it finds nothing and the pattern carries a regex
  metacharacter and compiles, the search reruns as a regex and the header says so —
  `(auto-regex)` on a hit, `no matches (literal or regex)` after a double miss — so
  `a|b` works without `--regex`. Explicit `--regex` output is byte-identical to before,
  metachar-free patterns never rerun, a backslash-bearing pattern stays literal on the
  system-grep fallback (POSIX ERE reads `\v` as `v`), and a BRE-flavored miss (`\|`)
  gets the Rust-syntax hint appended after budget capping so it can't be truncated away.
- **Missing path operands resolve to their unique extension sibling.** The path-taking
  ops (`grep` operands and scopes, `read`, `outline`, `edit`, `history`, structural
  search/replace, `find`/`symbol`/`deps` scopes) resolve `pkg/events` to
  `pkg/events.py` when exactly one `pkg/events.*` exists, prepending a
  `# note: pkg/events → pkg/events.py` line; several candidates error listing them and
  a true miss errors clean at exit 3 instead of surfacing raw engine stderr.
  Glob-shaped operands pass through untouched, `vcs diff` scopes stay exempt (a deleted
  file's diff is legitimate), and `history` never hard-fails a missing path.
- **`ccx code symbol --budget`.** The symbol card joins the budget-capped commands:
  default 2000 tokens on the CLI and MCP surfaces, while `ccx exec` leaves it uncapped
  by contract like every other op.

## [0.28.0] - 2026-07-18

### Changed
- **The grep, rg, and json guard lanes are per-occurrence.** A `;`/`&&`/`|`-joined line
  now splices: the flood-risk grep/rg occurrence rewrites to its ccx equivalent (and the
  json-emitting occurrence wraps in `ccx format --`) while sibling commands survive
  byte-for-byte, where the old lanes blocked or skipped compound lines wholesale. Block
  messages are computed from the live command line via capt-hook 9.28.0's callable
  `block=`, and the search guards now see through wrapper prefixes: a wrapped flood
  search (`sudo grep foo .`) blocks instead of slipping past on the wrapper name —
  wrapped occurrences match-to-block, never match-to-rewrite. The plugin's capt-hook
  floor rises to 9.28.0 and the pack manifest to 0.8.0.

### Added
- **rg gains a grep-style bounded stat lane.** An rg whose operands all stat as regular
  files summing under the read budget runs raw instead of blocking — rg is
  recursive-by-default, so the stat lane doubles as the recursion check. Count/list-only
  flags escape the size cap; `-o`, `--json`, or a `RIPGREP_CONFIG_PATH` in the
  environment forfeit the lane.

## [0.27.0] - 2026-07-18

### Fixed
- **`ccx exec` propagates not-found across the sandbox boundary.** A script that dies on
  a missing path now exits 3 like the direct CLI read: host-op failures carry a
  structured `err_code` through the monty wire, and an uncaught not-found wraps
  `codeexec.ErrNotFound` into the exit taxonomy. A script that catches the error and
  continues still exits 0.
- **`ccx vcs ship` resolves its push target before committing** — a bookmark refusal
  leaves the working copy untouched instead of stranding a commit — and refuses an
  empty working copy instead of cutting an empty duplicate.

## [0.26.0] - 2026-07-17

### Changed
- **`ccx vcs ship` runs the repo's prek pre-commit hooks before committing.** Auto-fixes
  are re-verified and folded into the commit; a hook that still fails after the retry
  aborts the ship with nothing committed. `--no-verify` skips the gate, and hunk-scoped
  ships skip it with a `hooks hunk-skip` report segment.

## [0.25.0] - 2026-07-17


### Changed
- **`ccx code symbol` is native.** Definitions come from a whole-scope ast-grep outline
  index, which resolves the Go types, consts, and vars the old engine never could; extra
  hits rank deterministically behind an `also defined:` line. Docs are extracted from
  source comments and docstrings. `--callers` shows word references with
  enclosing-function attribution and says so in its header; `--callees` is labeled
  syntactic. A miss walks exact → case-insensitive (disclosed) → definition-shaped text
  before exiting 3 with `symbol not found`.
- **`ccx code deps` is native.** Imports come from ast-grep with per-family
  local/std/external classification and `(unresolved)` where resolution would be a guess.
  Dependents come from an import-shape-filtered ripgrep scan scoped to the importing
  language; a language without a sound needle, like Rust or C#, says `dependents not
  scanned` instead of guessing. The output ends with its method line — syntactic, not a
  build graph — and a missing path exits 3.

### Removed
- **The tilth engine is gone.** Every op it served now runs natively, computed fresh from
  the working tree on each call — the stale-index bug class cannot recur. With it went the
  router and `Backend` interface (one dispatch table serves the CLI, the MCP facade, and
  exec), the pinned-binary download machinery, and the regex layers that reparsed engine
  output. semble is the one resident MCP engine, now with a session-level test; the anchor
  canary survives as `TestAnchorsEmittedAcrossOps`, pinning that every native op emits
  content anchors at generation time.

## [0.24.0] - 2026-07-17

### Changed
- **`ccx vcs diff` is native.** Changed symbols come from ast-grep outlines of the
  before/after blobs intersected with natively computed hunks — no external diff engine,
  no regex post-processing. Renames render as `## old → new` (a clean rename says so
  instead of masquerading as a new file), untracked files appear on the git lane, jj-only
  revsets get real per-file hunks instead of `--stat` counts, git-syntax sources
  (`HEAD~1..HEAD`) resolve through git in a colocated repo, symbol classification past 30
  files discloses itself, and `--full` inlines per-file hunks. `ccx vcs show` inherits all
  of it.
- **`ccx code outline` fallback is native.** Markdown gets an anchored ATX heading
  outline; languages without ast-grep outline rules get an honestly-labeled head window
  with a precise `ccx code read --section` continuation pointer — tilth signature mode is
  gone.
- **`ccx vcs history` summarizes commits through the native diff classifier** — the
  per-commit tilth shell-out and its output-scraping regex are deleted; the `(+a/-b)`
  line-count degradation and `(added)` root-commit paths stay.

## [0.23.0] - 2026-07-17

### Added
- **Hunk-scoped ship.** `ccx vcs hunks [paths...]` lists every pending hunk as a stable
  `file:A-B#hash` ref; repeatable `ccx vcs ship --skip-hunk <ref>` / `--only-hunk <ref>`
  commit a file partially. jj cuts the partial commit through its diff-editor protocol in
  one transaction (ccx re-invokes itself as the tool); git goes through a temp index. The
  working copy is never rewritten, excluded hunks stay uncommitted in `@`, an empty or
  drifted selection refuses instead of committing, and refs round-trip from any
  subdirectory. CI installs jj so the live jj-lane tests run there.

### Changed
- **`ccx code read` is served natively.** One `os.ReadFile` shaped into anchored output
  whose header reads `# read path:A-B#hash (k of N lines)`, so stale line labels are
  impossible by construction; tilth's index is no longer consulted. Sections take line
  ranges, `#hash` anchors, and markdown ATX headings (markdown files only); a non-markdown
  symbol lookup is redirected to `ccx code symbol --body`.
- **`ccx repo overview` is native.** Languages, dirs, entry points, manifests, test counts,
  git state, and 90-day churn come from one gitignore-honoring walk plus live git; the MCP
  surface now carries the language census the CLI used to append on its own.
- **ripgrep is the only grep engine.** The tilth literal lane and its stale-zero recheck
  band-aid are gone; every `ccx code grep` runs live ripgrep (system `grep` fills in, with
  its disclosure note), so a zero is a real zero and results honor `.gitignore` on every
  lane. Content anchors are stamped at generation from a per-call line cache, `--expand`
  means context lines around each hit, and the shapes are unchanged from the flagged lane
  agents already saw.

## [0.22.0] - 2026-07-16

### Changed
- **ast-grep is a normal PATH dependency, not a vendored download.** ccx no longer fetches
  a pinned ast-grep release on first use; it resolves `ast-grep` from PATH, probes the
  0.44.0 version floor once per binary path, and errors with an install hint when it's
  absent. The Homebrew formula already installs it; other installs need
  `brew install ast-grep` or `uv tool install ast-grep-cli`.

## [0.21.0] - 2026-07-16

### Added
- **`ccx code edit --match` addresses by exact text instead of a span.** The needle is
  byte-exact and multi-line (a CRLF file normalizes needle and content to its own EOL); zero
  matches error before any write, several error listing each candidate's `line#hash` anchor
  for a scoped re-run, `--at` composes with `--match` to confine the scan to a hash-verified
  span, and `--all` replaces every occurrence with a per-match stanza report in final-file
  coordinates. Content is written verbatim — trailing spaces and a trailing newline land on
  disk untrimmed — and every error path leaves the file byte-identical. Mirrored on the MCP
  `ccx_code_edit` tool (`match`, `all` params).

### Changed
- **`ccx vcs ship` fetches first and auto-rebases onto the target bookmark.** When the trunk
  (or `--bookmark`) target is no longer an ancestor of `@-`, ship rebases the local stack onto
  it and reports a `rebased N commit(s) onto X` segment; any conflict across the rewritten set
  rolls back via `jj op restore` and errors with the conflicted commits plus manual recovery
  steps. A missing or multi-head target bookmark refuses before any mutation, an empty
  `target..@-` refuses to move the bookmark backwards, and a rerun after a failed push takes
  the no-divergence path — resume never rebases twice. Git lane unchanged; `--no-push` still
  skips the network entirely.

### Fixed
- The guard pack pins its three read-only approvers to `PermissionRequest` only, ahead of
  capt-hook flipping the `approve()` default to `PreToolUse | PermissionRequest` — they must
  never compose with `repo_find_nudge` in one PreToolUse dispatch nor override settings deny
  rules.

## [0.19.0] - 2026-07-16

### Added
- **`ccx code read` masks detected secrets by default.** gitleaks' default rules (vendored from
  v8.30.1) run over read output before budget capping on all three lanes — CLI, MCP
  `ccx_code_read`, and the exec `read` host function. A finding of 16+ bytes keeps a 4-byte stub
  and collapses to `…[masked:<rule-id>]` (a shorter finding masks whole), and a footer names the
  fired rules. The noisy `generic-api-key` entropy catch-all fires only on env-shaped files
  (`.env`, `.env.*`, `*.env`, `.envrc`, `credentials`, `.netrc`) — a high-entropy secret
  hardcoded in `.yaml`/`.json`/source is left raw, deliberately: the rule is documented-noisy on
  ordinary source and would repaint lockfile hashes. Masking covers `code read` only;
  `grep`/`outline`/`symbol`/`diff` still emit raw content (backlogged). `--reveal-secrets`
  (`reveal_secrets` on MCP/exec) prints raw and now trips a permission prompt via the guard pack.
  Pack 0.7.0.

## [0.18.0] - 2026-07-15

### Changed
- **`ccx code read` fails loudly on unresolvable paths.** A leading `~` expands textually, the
  target is stat'd before dispatch, and a missing file exits 3 with `path not found` instead of
  silently degrading into a tilth content search. A directory errors with a `ccx code outline`
  pointer — the MCP `ccx_code_read` directory listing is gone, deliberately. The CLI, MCP, and
  exec lanes share the one check.
- **Guard-pack rewrites are per-occurrence.** The cat/sed/head/find/ls/git-pager/curl-dump
  rewrite guards splice only the qualifying command of a compound line (capt-hook 9.15's
  `rewrite_command_occurrences`): `cat big.go; echo done` rewrites the `cat` and keeps the `echo`
  byte-for-byte, `cd src && find . -name '*.go'` rewrites after the `cd` so the glob roots
  correctly, and an unrewritable flood segment blocks the whole line rather than running raw
  beside a rewritten sibling. Any-occurrence conditions close the `git diff; echo done` and
  `cat go.mod; echo x` allow-holes, and `git diff > out.patch` now runs — a file redirect does
  not flood context.
- **The bare-`cat` guard is size-gated.** It rewrites only an existing file over the large-read
  cap, expanding `~` itself and emitting the quoted absolute path — no more frozen-tilde phantom
  searches; small, nonexistent, or `$`-carrying operands run untouched, and a multi-file `cat`
  blocks only when its stat-able operands sum past the cap. Pack 0.6.0; capt-hook floor
  `>=9.15.0`.

### Added
- The common ccx MCP tools — `ccx_code_read`, `ccx_code_grep`, `ccx_code_outline`, `ccx_code_search` — carry the `anthropic/alwaysLoad` tool `_meta` flag, so Claude Code keeps them in the prompt under tool-search deferral (`ENABLE_TOOL_SEARCH`) instead of hiding them behind a `ToolSearch` round-trip. A guard redirect to one lands on an already-loaded tool; the rest of the surface stays deferred, loaded on demand. Per-tool `_meta`, not a server-level `alwaysLoad`, keeps the eager set to the four workhorses and the server's connect non-blocking.

## [0.17.0] - 2026-07-14

### Added
- `ccx vcs ship` takes trailing `[paths...]` to scope the commit: in jj the paths pass through as filesets and the remainder stays in the working copy; in git the blanket `git add -A` gives way to a pathspec-scoped add plus a partial commit. A working copy shared with a concurrent session no longer forces manual `jj` steps.
- The jj push lane only auto-advances the trunk bookmark. A non-trunk nearest bookmark refuses with its name in the error, and the new `--bookmark <name>` flag advances one deliberately; in a plain-git repo the flag is an error. Bookmarks now move by exact name (`exact:` anchored — jj otherwise reads bare names as globs) and push with `--bookmark`, so a second bookmark parked on the same commit no longer rides along; a `--bookmark` name that doesn't resolve refuses with `bookmark not found` instead of jj's silent exit-0 no-op. A scoped jj commit whose fileset matches no changes still ships an empty commit — same as an unscoped ship on a clean working copy.
- `ccx exec` caches MCP discovery on disk per project for 15 minutes, so a warm cache spawns no `claude mcp list` probe; a script that references no reflected tool skips reflection entirely — no probe, no notes. Changing `CCX_EXEC_MCP_ALLOW`/`CCX_EXEC_MCP_DENY` invalidates the cache, `CCX_EXEC_MCP=refresh` forces a fresh probe, and `CCX_EXEC_MCP_TIMEOUT` (Go duration, default `30s`, up from 10s) bounds it — an invalid duration is a hard error.

### Fixed
- A discovery probe that fails past the cache TTL falls back to the last good inventory with a note, and a deadline kill reports `claude mcp list timed out after 30s` instead of the bare `signal: killed`.

## [0.14.0] - 2026-07-13

### Added
- The captain-hook dependency is explicit: `plugin.json` declares `{ "name": "captain-hook", "marketplace": "captain-hook", "version": ">=9.9.0" }` and the repo `marketplace.json` allows the cross-marketplace dependency via `allowCrossMarketplaceDependenciesOn`. The allowance is load-bearing — without it Claude Code silently skips the declared dependency at install and the attached guard pack runs with no dispatcher.
- CI vets the attach-only pack contract: `uvx 'capt-hook>=9.9.0' pack lint plugin` checks the manifest, the canonical attach entry, the dependency floor, the marketplace allowance, and that the pack loads clean.
- The ccx guard pack auto-approves the read-only ccx surface — the server-pinned `mcp__cc-context__ccx_*` query tools and a fail-closed CLI allowlist — so query calls stop hitting permission prompts.

### Fixed
- `install-binary.sh` and the release version check take the first `"version"` match in `plugin.json` — the dependencies block carries version floors of its own, which corrupted the pinned release tag into a multi-line string.
- `ccx vcs show` validates refs behind `--end-of-options`, closing a flag-injection path where a crafted ref could clobber files; `ccx web` refuses link-local and cloud-metadata hosts (SSRF).
- A tilth grep reporting zero matches is re-verified through the live rg engine before being trusted — hits in capped or minified files no longer vanish silently.
- The MCP launch floors the semantic-search dependency at `semble[mcp]>=0.5`.

### Changed
- The SessionStart pack attach runs the canonical attach-only prefix, `uvx --isolated capt-hook pack attach "${CLAUDE_PLUGIN_ROOT}/hooks"`; the `install-binary.sh` entry is unchanged.
- README: the plugin installs from its own marketplace (`cc-context@cc-context`), with the captain-hook marketplace added first so the dependency auto-installs, plus an upgrade note — `claude plugin update` silently skips newly added dependencies. The prior `yasyf/cc-skills` instructions pointed at a marketplace that no longer lists the plugin.
- Docs reposition on the measured benchmark record ([bench/FINDINGS.md](bench/FINDINGS.md)): the README and the ccx skill lead with bounded, structured output and measured accuracy on targeted questions, the `symbol`/`overview` examples are regenerated from the 0.13.0 terse defaults, the exhaustive-enumeration caveat is stated where it applies, and session-level token-savings claims are retired. The shared guides fragment (`cc-skills:ccx`) carries the same scoping to every consuming repo.

## [0.13.0] - 2026-07-12

### Fixed
- Guard pack 0.4.0: the grep guard judges each grep statement on its own flags and operands (per-occurrence, matching the rg guard) instead of requiring the whole Bash line to be a single command. Explicit data-file targets (`.log`/`.json`/…) pass textually with no stat, so files created earlier in the same compound command or addressed relative to an in-command `cd` now run as-is (`-o` is allowed on a data-file target — its output tracks the matched data). Tree-wide, directory, and recursive greps still block, and so do the flood shapes an over-broad allow would miss: `-o` over a source file (its per-match filename/line/byte prefixes multiply output past the size cap), a `GREP_OPTIONS` env that injects flags the parser never sees, a pipe-sink grep that names file operands (it searches the files, ignoring stdin), and flag-supplied empty or `-f`-file patterns.

## [0.12.0] - 2026-07-10

### Changed
- The cc-context plugin no longer registers the `PreToolUse`, `PostToolUse`, or `PreCompact` hook events in `plugin/hooks/hooks.json`. That job moves to the captain-hook plugin (capt-hook 9.0.0 or newer), now the sole registrar of the `uvx capt-hook run <Event>` dispatch commands. Claude Code does not dedupe identical hook commands across sources, so a duplicate registration double-fires every guard. The `SessionStart` entry is unchanged. It still runs pack attach and `install-binary.sh`, and the guard pack dispatches through captain-hook.

## [0.11.0] - 2026-07-10

### Added
- `--regex`/`-E` on `ccx code grep` treats the query as a regular expression (ripgrep by default, `grep -E` ERE as the fallback), and explicit file operands — `ccx code grep <pattern> file1 file2` — scope the search to named paths. Both route to the rg/grep engine, so anchored (`^class `) and multi-file queries that the literal tilth path silently 0-matched now resolve. Wired across the CLI, MCP (`regex`/`paths` on `ccx_code_grep`), and `ccx exec`'s `grep()`.
- Guard pack: a dialect-safe regex `grep` rewrites to `ccx code grep --regex` — a position-aware validator admits only constructs whose semantics are identical in grep's BRE/ERE and Rust's regex (anchors only at the ends, quantifiers never leading, digit-only intervals, no bracket expressions or backslashes); `-F` always stays literal. A bounded `grep` over explicit existing files that ccx cannot express passes straight through when the named files total under the pack's large-read threshold, with positionals emitted after `--` so flag-shaped filenames stay filenames. Tree-wide unmappable shapes still block with a pointer at the `ccx` equivalent.

### Fixed
- `ccx vcs diff --scope <file>` no longer drops the whole diff when tilth attributes zero symbol changes — the collapsed `0 symbols touched` header is expanded and the raw hunk spliced back in, on both the CLI and MCP surfaces, including paths with spaces.
- `ccx vcs diff <bogus-ref>` errors loudly instead of silently reporting no changes; each Git ref endpoint must parse via `git rev-parse`, so multi-value sources like `HEAD^@` keep working.

### Changed
- An unbudgeted `-i`/`-w`/`--regex`/multi-file `ccx code grep` defaults to a 2000-token output cap at the CLI and MCP surfaces; the uncapped `ccx exec` contract is unchanged.

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

[0.11.0]: https://github.com/yasyf/cc-context/compare/v0.10.0...v0.11.0
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
