---
name: ccx
description: >-
  Read code, find symbols, search a codebase, and review diffs with token-bounded
  outputs instead of raw file reads. Use whenever you need codebase context: reading
  a file, locating a symbol or definition, searching code by intent or text, listing
  files, or reviewing changes. Triggers on "read this file", "where is X", "find the
  Y function", "how does Z work", "search the code for", "show me the diff", "what
  calls this". Reach for ccx before Read, cat, sed, git diff, ls -R, or find, since the
  guard hooks block those on anything token-heavy.
---

# ccx — compact codebase context

`ccx` answers codebase questions with the fewest tokens that still carry the answer.
Every command keeps line numbers, prints a token count, and reports overflow without
silent truncation. Use it as the default path to any file, symbol, search, or diff.

The MCP tools mirror the CLI one-to-one: `mcp__cc-context__overview`,
`mcp__cc-context__search`, `mcp__cc-context__outline`, and so on take the same
arguments as the commands below. Use whichever is available; the workflow is
identical.

## Workflow

### 1. Orient

Start a new codebase, or a new area of one, with the map:

```
ccx overview
```

It reports the structure, languages, and entry points, enough to know where to look
before you look.

### 2. Find

Pick the tool by what you know.

- **Have the intent but not the name.** Semantic search by meaning:
  ```
  ccx search "how requests get authenticated"
  ```
- **Have the exact symbol.** Definition plus callers, callees, siblings, and tests in
  one shot:
  ```
  ccx symbol authenticate        # alias: ccx grok authenticate
  ```
- **Have literal text to match.** Error strings, env-var names, a verbatim token:
  ```
  ccx grep "RATE_LIMIT"
  ccx grep "func New" --glob "internal/**/*.go"
  ```
- **Have a path shape.** List files with per-file token counts:
  ```
  ccx find "internal/**/*.go"
  ```

### 3. Read

Outline before you read. The outline is the structure with line numbers, bounded to a
token budget:

```
ccx outline internal/router/router.go
```

Then read only the span you need, by line range or by heading:

```
ccx read internal/router/router.go --section 40-95
ccx read README.md --section "## Workflow"
```

When you need the whole file, because it is small or the outline says so, escape the
budget:

```
ccx read internal/router/router.go --full
```

### 4. Review

Inspect changes without dumping the entire working tree:

```
ccx diff                # uncommitted
ccx diff staged
ccx diff main           # against a ref
```

## Guarantees

These hold for every command, which is what makes ccx safe to trust over a raw read:

- **Line numbers stay.** Every span you get back carries its real line numbers, so a
  follow-up `ccx read --section A-B` or an `Edit` lands where you expect.
- **Token counts are shown.** Each output reports its own size — you always know what
  a result cost before deciding to read more.
- **Overflow is explicit.** When a result exceeds the budget, ccx says so and tells
  you what it left out. It never silently truncates.
- **There is always a raw fallback.** Hit overflow or want the unabridged source and
  you have an escape hatch. Use `ccx read --full` for a whole file, a path-scoped `ccx
  diff <ref>` for changes, or `Read` with an `offset` for a known line range.

## Why ccx first

The guard hooks block token-heavy primitives: a full-file `Read` of a large file, a
broad `git diff`, `sed -n`, a bare `cat`, `ls -R`, and `find` enumeration. Each has a
ccx equivalent that returns the same answer in a fraction of the tokens. Reach for
ccx and you stay inside the budget by default; reach for the raw tool and the hook
turns you back to ccx anyway.
