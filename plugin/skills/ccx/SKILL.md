---
name: ccx
description: >-
  Read code, find symbols, search a codebase, review diffs, edit a span in place,
  outline and search web pages, and compose multi-call pipelines with token-bounded
  outputs instead of raw file reads or whole-page fetches. Use whenever you need
  codebase context: reading a file, locating a symbol
  or definition, searching code by intent or text, listing files, reviewing changes,
  editing a line range you already have an anchor for, re-encoding a command's
  JSON output, or chaining tool calls and keeping only the distilled result — and
  whenever you need web content: reading or fetching a web page or docs URL, or
  asking a question of a page. Triggers
  on "read this file", "where is X", "find the Y function", "how does Z work",
  "search the code for", "show me the diff", "what calls this", "change line N",
  "replace this span", "filter this output", "for each file", "combine results
  from", "fetch this page", "read these docs", "what does the page say about X",
  or running any command that emits JSON.
  Reach for ccx before Read, cat, sed, grep, git diff, ls -R, find, WebFetch, or a
  curl page dump, since the guard hooks block those on anything token-heavy.
---

# ccx — compact codebase context

`ccx` answers codebase questions with bounded, structured output: line-numbered,
budget-capped, explicit about overflow, never silently truncated. Across two 450-run
benchmark campaigns that structure matched or beat native-tool accuracy on locate,
trace, and diff questions, and held after the terse defaults (CLI 100% on the gated
re-run; MCP 96.0% vs baseline 96.2%). Use it as the first stop for a targeted
question: a file section, a symbol, a search, a diff. One boundary: when the answer
must be a complete set ("every subclass of X", "every module importing Y"), read the
candidate files — bounded views can withhold members, and measured accuracy reversed
on exactly those enumeration tasks.

The MCP tools mirror the ccx query surface: `ccx_repo_overview`,
`ccx_code_search`, `ccx_code_outline`, and the rest of
the read, search, edit, and diff commands take the same arguments as their CLI counterparts;
`ccx_exec` and `ccx_exec_tools` mirror `ccx exec`; `BashFormat` mirrors
`ccx format -- <cmd>`. Clients may show these under a client-assigned `mcp__…__`
prefix; use the names as listed in the tool inventory.
Use whichever is available; the workflow is identical. `ccx vcs ship`, `ccx vcs hunks`,
`ccx vcs show`, `ccx vcs history`, and `ccx repo locate` are CLI-only — there is no MCP
tool for them.

## Workflow

### 1. Orient

Facing an untargeted task in an unfamiliar codebase? Draw the map:

```
ccx repo overview
```

It reports the structure, languages, and entry points, enough to know where to look
before you look. Skip it when you already know what you're after — starting with a
search or symbol lookup costs one round-trip less than starting with a tour.

### 2. Find

Pick the tool by what you know.

- **Have the intent but not the name.** Semantic search by meaning — a native in-process
  engine, no external service. `--content` narrows the corpus (space-separated
  `code|docs|config|all`, default `code docs`), the same flag on `ccx code related` and over the MCP:
  ```
  ccx code search "how requests get authenticated"
  ccx code search "retry backoff policy" --content "code docs config"
  ```
- **Have the exact symbol.** Location, signature, and doc, with a counts trailer;
  `--callers`/`--callees`/`--body`/`--full` expand the layer you need:
  ```
  ccx code symbol authenticate   # alias: ccx code grok authenticate
  ```
- **Have literal text to match.** Error strings, env-var names, a verbatim token:
  ```
  ccx code grep "RATE_LIMIT"
  ccx code grep "func New" --glob "internal/**/*.go"
  ccx code grep -i -w "ratelimit"   # case-insensitive, whole-word (runs on ripgrep)
  ccx code grep TODO cmd/main.go internal/cli/run.go --glob "*.go"   # --glob filters within explicit paths
  ```
- **Have a path shape.** List files with per-file token counts — gitignore-honoring,
  VCS stores skipped, sorted, budget-capped (`--budget N` raises the cap; a glob
  anchored at a real path, like `.venv/**/*.py`, lists files ignore rules would hide):
  ```
  ccx repo find "internal/**/*.go"
  ```
  Orienting a whole repo is `ccx repo overview`, not a `"**/*"` listing.

### 3. Read

Outline before you read. The outline is the structure with line numbers, each span
carrying a content anchor (`L15#k2fa`), bounded to a token budget:

```
ccx code outline internal/router/router.go
```

Then read only the span you need. Echo an anchor back from any producer command, or pass
a plain line range or a heading:

```
ccx code read internal/router/router.go --section 40-95#k2fa   # anchored span from outline
ccx code read README.md --section "## Workflow"
```

When you need the whole file, because it is small or the outline says so, escape the
budget:

```
ccx code read internal/router/router.go --full
```

### 4. Edit

Write through the same anchor you read with. The hash is the verification: `ccx code
edit` (MCP: the `ccx_code_edit` tool) refuses to write unless the anchored
content still matches. A span that merely moved re-anchors, applies, and prepends
`# anchor k2fa: line 40 → 44`; a vanished or ambiguous anchor errors before any write,
leaving the file byte-identical.

```
ccx code edit internal/router/router.go --at 40-43#k2fa --content 'func route(p string) Handler {
	return lookup(p)
}'
```

The report maps the old cite to the new (`40-43#k2fa → 40-42#s45e`, plus a `-`/`+` diff
of the span); the returned anchor chains into the next edit without a re-read. There is
no preview round-trip — `code replace` previews because a structural pattern can
over-match, but an anchor names exactly one verified site, so the edit applies
immediately. `--content -` reads stdin (a single trailing newline terminates the last
line); `--delete` removes the range instead; a plain numeric `--at A-B` is legal but
unverified. Untouched lines round-trip byte-identical — CRLF and a missing trailing
newline survive, and the file mode is preserved.

When you know the exact text but not its span, `--match` addresses by content instead:

```
ccx code edit internal/router/router.go --match 'return lookup(p)' --content 'return lookup(canon(p))'
```

The needle is byte-exact — trailing spaces, tabs, and multi-line spans all count, with
newlines matched to the file's EOL convention — and the content is written verbatim:
nothing is trimmed, and a trailing newline lands on disk. Zero matches error before any write; several error listing each candidate's
`line#hash`, so the recovery is `--at 40#k2fa --match …`, which confines the scan to
that verified span. `--all` replaces every occurrence and reports one stanza per match,
refs in final-file coordinates. The report echoes each written line back with its fresh
anchor — the `+` rows are what the file now contains.

### 5. Review

Inspect changes without dumping the entire working tree:

```
ccx vcs diff            # uncommitted
ccx vcs diff staged
ccx vcs diff main       # against a ref
```

Inspect a single commit — its message plus a structural per-file diff — with `show`,
and trace how one file changed across its recent commits with `history`:

```
ccx vcs show                               # the last commit (@-/HEAD)
ccx vcs show a1b2c3d                        # a named commit
ccx vcs history internal/cli/root.go       # per-commit sha · date · subject + changed symbols
ccx vcs history internal/cli/root.go -n 5  # cap the commit count
```

### 6. Locate

Resolve a repo, Go module, or Python package to its on-disk path instead of scanning
`~/Code` or the module cache by hand:

```
ccx repo locate captain-hook               # a sibling repo under ~/Code
ccx repo locate github.com/spf13/cobra     # a Go module in the cache
```

Each match prints a tab-separated `kind  path  version` line — one per cached module
version — and the command exits 3 when nothing resolves.

### 7. Ship

Commit, push, and watch CI in one call. `ship` runs a jj-aware commit (plain git
otherwise), pushes, then watches every workflow run on the pushed commit — found via
`gh run list --commit`, retrying registration for up to a minute. Trailing paths
scope the commit to just those files; the rest of the working copy — a concurrent
session's edits included — stays put. The push only auto-advances the trunk
bookmark: parked on any other bookmark, ship refuses until `--bookmark <name>`
names the target deliberately. The first line is the summary; each watched run adds
a `workflow · conclusion · duration · url` line,
and a red run adds its failing jobs and a budget-capped `--log-failed` excerpt with a
`full log:` pointer. `CI failure` means a run went red; `CI error` means the watch
itself failed after a successful push (the `check:` line says how to resume — don't
re-run ship, that cuts a new commit). On a terminal, progress streams live:

```
ccx vcs ship -m "fix: budget overflow marker"   # commit + push + watch CI
ccx vcs ship -m "fix: route" internal/cli xs.md  # scoped: commit only these paths
ccx vcs hunks f.go                               # list pending hunks as file:A-B#hash refs
ccx vcs ship -m "fix: x" --skip-hunk f.go:530-536#k2fa f.go  # ship f.go minus one hunk
ccx vcs ship -m "wip" --no-push                  # commit only, skip push and CI
ccx vcs ship --amend                             # fold the working copy into the parent
ccx vcs ship -m "spike" --bookmark me/probe      # advance a non-trunk bookmark
ccx vcs ship -m "fix: x" --budget 0              # uncapped failure-log excerpt
```

### 8. Re-encode

JSON tool output enters context through `ccx format` — the default wrapper for any
command that emits JSON or NDJSON (`gh --json`, `kubectl -o json`, `terraform output
-json`), and a filter for pipes:

```
ccx format -- gh pr list --json number,title,author
kubectl get pods -o json | ccx format
```

A classifier reads the payload's shape and emits the leanest accurate encoding:

| Payload shape | What you get |
| --- | --- |
| Under 200 bytes | Compact JSON — format deltas are noise at this size |
| Prose-dominant (one text field at ⅔ of the payload, or any 2 KiB+ prose field) | The prose itself, other fields as XML-ish metadata tags |
| Uniform array of objects, small | Markdown table |
| Uniform array of objects, large | CSV/TSV byte shootout; TOON enters at 100+ rows and wins only when more than 5% smaller |
| Repeated nested shapes | TRON — class declarations for the repeated key-sets |
| Heterogeneous or log-like array | JSONL |
| Anything else | Compact JSON |

Near-ties go to the classifier's preferred encoding — a later candidate must beat an
earlier one by more than 5% in bytes to displace it. Auto output never exceeds
compact JSON by bytes; `--format=X` forces one encoder even
when it's larger. Non-JSON output passes through verbatim and the exit code is
propagated. Over MCP, the `BashFormat` tool runs the command and returns the
compacted output — a `format` param forces an encoder.

### 9. Web pages

Web pages get the file treatment: outline first, then read one section, or ask the page
a question instead of paging through it top to bottom.

```
ccx web outline https://example.com/docs
ccx web read https://example.com/docs --section 2.3
ccx web search https://example.com/docs "how do I configure retries"
```

`outline` returns the heading tree with stable section refs. Echo one into `ccx web
read --section` for that section's subtree with a `§prev`/`§next` footer, or take the
whole page with `--full`. `search` answers with the top-k relevant chunks, each carrying
a `<url> §2.3#k7fq` cite whose ref echoes back into `read`; ranking is hybrid BM25 +
local embeddings, degrading to BM25-only (and saying so) when `uv` is off `PATH`.
On a page with no heading structure the outline collapses to one section — lead with
`search`, or page through it with `read --offset <tokens>`, echoing the next offset
from the overflow footer.
A page that fetches as a JS app shell — near-empty content from a client-side app —
escalates automatically to a rendered fetch when a lane is available: keyed jina
(`JINA_API_KEY`), firecrawl (`FIRECRAWL_API_KEY`), or the `agent-browser` CLI on `PATH`.
The jina lane also appends the page's links as a `## Links` section, so nav slugs land
in outline and search. With no lane available the thin copy is served with a note naming
what to set or install — set it, then re-run with `--refresh`.
Fetched pages and their indexes persist in the ccx cache for 24 hours — `--refresh` on
any of the three bypasses the TTL. The MCP mirrors are `ccx_web_outline`,
`ccx_web_read`, and `ccx_web_search`.
A one-shot question about a page can skip the loop entirely: spawn the
`cc-context:web-fetch` agent with the URL and your prompt, and only the cited answer
enters your context — see [Reader subagents](#reader-subagents).

### 10. Compose

One question takes one call from steps 1–9. When the work is a pipeline — two or more
chained calls, output you'd immediately filter or project, a fan-out across files —
write the pipeline as a script instead. `ccx exec` (MCP: the `ccx_exec` tool)
runs a short Python script in a sandbox where every ccx query op above is an async host
function, alongside a gated `sh(cmd)` and the tools of every stateless MCP server,
auto-reflected with no flag needed. Intermediate output stays in the sandbox; only the
script's return value enters context. A shell pipe is not the bound: ccx output is
already budget-capped, and `| head`/`| tail` re-introduces silent truncation while
eating the overflow footer — raise `--budget`, narrow with `--section`/`--scope`, or
write the exec script.

```
ccx exec 'import asyncio
async def main():
    raw = await grep("TODO", glob="internal/**/*.go")
    return [ln for ln in raw.splitlines() if "FIXME" not in ln]
asyncio.run(main())'
```

The script comes in as an argument, `--file <path>`, or stdin (`--file -`); `--budget`
caps the result size.

- **Print the catalog once per session, before the first script.** `ccx exec
  --list-tools` (MCP: the `ccx_exec_tools` tool) lists every host function
  signature — ccx ops, `sh`, and the reflected MCP tools — plus the Python-subset
  rules and a worked example. Once is enough; don't re-run it per script.
- **The sandbox speaks a Python subset.** No classes, no `match`; imports are limited
  to `re`, `json`, `datetime`, and `asyncio`, one module per `import` line. A top-level
  `return` is illegal — wrap the logic in `async def main()` and end the script with
  `asyncio.run(main())`. Every host function is async: await it, and run independent
  calls concurrently with `asyncio.gather(...)`.
- **Reflected servers are fresh instances.** Each stateless MCP server is spawned anew
  for the sandbox, so a tool that needs live session state will misbehave — exclude it
  with `CCX_EXEC_MCP_DENY=<name>` (comma-separated; `CCX_EXEC_MCP_ALLOW` overrides the
  classifier the other way, and `CCX_EXEC_MCP=off` disables reflection entirely).
- **Discovery is cached per project for 15 minutes.** A script that references no
  reflected tool skips reflection entirely — no probe, no notes. Changing
  `CCX_EXEC_MCP_ALLOW`/`CCX_EXEC_MCP_DENY` invalidates the cache; after adding an MCP
  server, force a fresh probe with `CCX_EXEC_MCP=refresh`. `CCX_EXEC_MCP_TIMEOUT`
  (Go duration, default `30s`) bounds the `claude mcp list` health check.

## Guarantees

These hold for every command, which is what makes ccx safe to trust over a raw read:

- **Spans stay valid, or report they moved.** Every span you get back carries a short
  content anchor, like `L15#k2fa`. Echo it into `ccx code read --section 15-27#k2fa` and
  it resolves by content, not by line count. An exact hit comes back silently, a shifted
  span re-anchors and prepends `# anchor k2fa: line 15 → 22`, and vanished content errors,
  telling you to re-run `ccx code outline`.
- **Anchored cites outlive the file.** Durable prose — plans, reviews, memory files —
  may cite code as `path:line#hash` (e.g. `internal/render/finalize.go:31#k2fa`).
  Resolution is stateless: any later session resolves the cite, even after the file has
  drifted, because the hash re-anchors by content — which is why anchored cites beat
  bare line numbers in anything durable. The same check gates writes: `ccx code edit`
  refuses to touch a span whose content no longer matches its anchor.
- **Budgets are enforced.** Structural output is capped to the token budget, and most
  results report their own size — you know what a result cost before reading more.
- **Overflow is explicit.** When a result exceeds the budget, ccx says so and tells
  you what it left out. It never silently truncates.
- **There is always a raw fallback.** Hit overflow or want the unabridged source and
  you have an escape hatch. Use `ccx code read --full` for a whole file, a path-scoped
  `ccx vcs diff <ref>` for changes, or `Read` with an `offset` for a known line range.
- **Exec returns are budget-capped.** A script can touch megabytes across dozens of
  calls; only its return value comes back, run through the `ccx format` shape
  classifier and capped to the token budget like any other command.
- **`sh()` is a sanctioned bypass of the read guards.** It runs host-side inside the
  ccx process, out of the guard hooks' sight, with its own denylist in
  `internal/codeexec/sh.go`. That is safe because only the script's filtered return
  enters context, never the raw command output.

## Why ccx first

The guard hooks block token-heavy primitives: a full-file `Read` of a large file, a
broad `git diff`, raw `grep`, `sed -n`, a bare `cat`, `ls -R`, `find` enumeration, a
whole-page `WebFetch`, and an unpiped `curl`/`wget` page dump (a URL `ccx web` can't
serve stays reachable — the same WebFetch passes on a deliberate re-run).
Each has a ccx equivalent that returns a bounded, structured answer — line-numbered,
budget-capped, explicit about what was cut — where native output can be unbounded and
says nothing about what's missing. Measured head-to-head across two 450-run
campaigns, that structure bought correctness: ccx arms matched or beat native
accuracy on locate/trace/diff tasks; under the terse defaults the CLI scored 100% on
the gated re-run (MCP: 96.0% vs baseline 96.2%); and the structural diff caught a
misattributed hunk that raw hunks missed. Reach for ccx on targeted questions; reach
for the raw tool and the hook turns you back anyway.

The honest exception is exhaustive enumeration. When the deliverable is a complete
set, compact views can withhold exactly the members you need — measured on
enumeration tasks, ccx-cli's accuracy fell to 76.9% against the baseline's 93.3%.
Read the candidate files (sectioned or `--full`) and treat ccx as the locator, not
the enumerator — or spawn the `enumerator` agent below, which packages that
verification loop.

## Reader subagents

The plugin ships five read-only agents that run the flows above in their own
context and return only conclusions. Spawn them as `cc-context:<name>` whenever
the raw material — a web page, a CI log, a dependency's source — has no business
entering yours:

- `web-fetch` — the WebFetch drop-in: one URL plus your prompt in, the cited
  answer out. The WebFetch and page-dump guard messages name it.
- `web-researcher` — a research question plus seed URLs; discovers pages with
  WebSearch, reads across them via `ccx web`, returns findings with a
  `<url> §ref` cite per claim.
- `ci-triage` — a red run id; digs `gh run view --log-failed` where it can't
  flood anything and returns the root cause, a minimal excerpt, and the next
  step. `ccx vcs ship`'s red-run report points at it.
- `dep-reader` — "how does `<pkg>` implement X"; resolves the installed source
  with `ccx repo locate` and returns the answer with `path:line#hash` cites.
- `enumerator` — complete-set questions ("every subclass of X"); sweeps LSP,
  call-graph, and textual lanes, verifies each candidate by reading it, and
  returns the proven set with its blind spots named.

All five default to `model: sonnet`; pass `model: opus` (or higher) at spawn
time when the page, log, or set warrants it.
