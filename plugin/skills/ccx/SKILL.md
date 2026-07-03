---
name: ccx
description: >-
  Read code, find symbols, search a codebase, and review diffs with token-bounded
  outputs instead of raw file reads. Use whenever you need codebase context: reading
  a file, locating a symbol or definition, searching code by intent or text, listing
  files, or reviewing changes. Triggers on "read this file", "where is X", "find the
  Y function", "how does Z work", "search the code for", "show me the diff", "what
  calls this". Reach for ccx before Read, cat, sed, grep, git diff, ls -R, or find, since
  the guard hooks block those on anything token-heavy.
---

# ccx — compact codebase context

`ccx` answers codebase questions with the fewest tokens that still carry the answer.
Every command keeps line numbers, prints a token count, and reports overflow without
silent truncation. Use it as the default path to any file, symbol, search, or diff.

The MCP tools mirror the ccx query surface: `mcp__cc-context__ccx_repo_overview`,
`mcp__cc-context__ccx_code_search`, `mcp__cc-context__ccx_code_outline`, and the rest of
the read, search, and diff commands take the same arguments as their CLI counterparts.
Use whichever is available; the workflow is identical. `ccx vcs ship`, `ccx vcs show`,
`ccx vcs history`, and `ccx repo locate` are CLI-only — there is no MCP tool for them.

## Workflow

### 1. Orient

Start a new codebase, or a new area of one, with the map:

```
ccx repo overview
```

It reports the structure, languages, and entry points, enough to know where to look
before you look.

### 2. Find

Pick the tool by what you know.

- **Have the intent but not the name.** Semantic search by meaning:
  ```
  ccx code search "how requests get authenticated"
  ```
- **Have the exact symbol.** Definition plus callers, callees, siblings, and tests in
  one shot:
  ```
  ccx code symbol authenticate   # alias: ccx code grok authenticate
  ```
- **Have literal text to match.** Error strings, env-var names, a verbatim token:
  ```
  ccx code grep "RATE_LIMIT"
  ccx code grep "func New" --glob "internal/**/*.go"
  ```
- **Have a path shape.** List files with per-file token counts:
  ```
  ccx repo find "internal/**/*.go"
  ```

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

### 4. Review

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

### 5. Locate

Resolve a repo, Go module, or Python package to its on-disk path instead of scanning
`~/Code` or the module cache by hand:

```
ccx repo locate captain-hook               # a sibling repo under ~/Code
ccx repo locate github.com/spf13/cobra     # a Go module in the cache
```

Each match prints a tab-separated `kind  path  version` line — one per cached module
version — and the command exits 3 when nothing resolves.

### 6. Ship

Commit, push, and watch CI in one call. `ship` runs a jj-aware commit (plain git
otherwise), pushes, then blocks on `gh run watch --exit-status`, reporting the commit,
the push, and the CI conclusion in one summary line:

```
ccx vcs ship -m "fix: budget overflow marker"   # commit + push + watch CI
ccx vcs ship -m "wip" --no-push                  # commit only, skip push and CI
ccx vcs ship --amend                             # fold the working copy into the parent
```

## Guarantees

These hold for every command, which is what makes ccx safe to trust over a raw read:

- **Spans stay valid, or report they moved.** Every span you get back carries a short
  content anchor, like `L15#k2fa`. Echo it into `ccx code read --section 15-27#k2fa` and
  it resolves by content, not by line count. An exact hit comes back silently, a shifted
  span re-anchors and prepends `# anchor k2fa: line 15 → 22`, and vanished content errors,
  telling you to re-run `ccx code outline`.
- **Token counts are shown.** Each output reports its own size — you always know what
  a result cost before deciding to read more.
- **Overflow is explicit.** When a result exceeds the budget, ccx says so and tells
  you what it left out. It never silently truncates.
- **There is always a raw fallback.** Hit overflow or want the unabridged source and
  you have an escape hatch. Use `ccx code read --full` for a whole file, a path-scoped
  `ccx vcs diff <ref>` for changes, or `Read` with an `offset` for a known line range.

## Why ccx first

The guard hooks block token-heavy primitives: a full-file `Read` of a large file, a
broad `git diff`, raw `grep`, `sed -n`, a bare `cat`, `ls -R`, and `find` enumeration.
Each has a ccx equivalent that returns the same answer in a fraction of the tokens.
Reach for ccx and you stay inside the budget by default; reach for the raw tool and the
hook turns you back to ccx anyway.
