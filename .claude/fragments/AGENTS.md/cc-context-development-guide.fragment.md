# cc-context Development Guide

Tools for keeping Claude's context minimal: a single Go binary exposing the `ccx` CLI plus the `cc-context` facade MCP. Distributed via goreleaser → Homebrew: `brew install yasyf/tap/ccx`.

## Repository Structure

```
cc-context/
├── cmd/ccx/          # main package — the CLI entry point
├── internal/
│   ├── cli/          # cobra command tree (ccx subcommands)
│   ├── mcpserver/    # the cc-context facade MCP server
│   ├── mcpclient/    # spawns stdio MCP servers, extracts their tool inventories
│   ├── codeexec/     # ccx exec: uv-subprocess pydantic-monty sandbox, host ops, sh(), MCP auto-reflection
│   ├── backend/, router/, proxy/  # logical-op surface + engine routing and sessions
│   ├── render/, format/           # budget-capped output shaping; shape-classified JSON re-encoding
│   ├── search/, outline/, grok/, …  # one package per op family
│   ├── version/      # build version, stamped via -ldflags
│   └── log/          # slog setup
├── .github/          # GitHub Actions workflows
├── AGENTS.md         # This file — shared conventions
└── README.md         # Project overview
```

## Stack workspaces and cleanup

Stack replay requires Git 2.56 or newer. Internal conflict and rehearsal
workspaces are sparse. Git `ccx vcs worktree add` links a checkout to the
caller's repository. Follow [the stack conflict guide](docs/stack-rebase.md)
for sparse expansion, manual dependency setup, and explicit
`ccx vcs stack regenerate --include <dir>` or `--full`. Keep mutable install
state and build outputs private to each workspace; download and compiler
caches can remain shared.

Git `ccx vcs stack new <name>` defaults to thin lanes backed by
`~/.claude/stores/<key>/<repo>`, except a child of a non-trunk branch the
calling checkout holds, which is cut in that checkout's clone; tracked `.claude` and `.agents`
are checked out without running hooks or setup scripts. Thin stores share the
source checkout's cc-notes records through `cc-notes storage bind`, which
cc-notes must provide; existing unbound or differently bound stores are
refused.

Use `--full-history` from a full checkout or `CCX_STACK_NEW=full`
to retain that checkout's history; unset keeps jj behavior. A thin refusal
never falls back to full history.
ccx never deletes the store; unpushed branches survive lane removal. Deepening
requires explicit `--deepen`, bounded by `--max-depth`. Follow [the thin-lane guide](docs/thin-lanes.md)
for sparse expansion, published parents, and ancestry refusals.

Use `ccx vcs worktree rm <name>` or `--path <absolute-path>` for authorized
unused trees. On macOS, logical removal precedes queued physical deletion;
`--wait` waits for deletion. `--force` discards dirty work only and never
overrides active sessions, locked trees, or the main checkout. `--path` moves
an orphaned pool worktree, whose admin dir is gone, to `~/.Trash`. Inspect jobs
with `ccx vcs cleanup status [job-id] --json` and watchers with
`ccx vcs cleanup watchers --json`. Follow [the cleanup reference](docs/worktree-cleanup.md)
for completion checks and queue controls. Preserve occupied completed
conflict workspaces until inactive, and restrict watcher retirement to
authorized unused trees.
