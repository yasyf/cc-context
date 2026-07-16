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
