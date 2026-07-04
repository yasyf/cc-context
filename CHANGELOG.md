# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `ccx exec [script]` runs a Python (monty-subset) script in a sandbox whose async host functions are every ccx query op, a gated `sh(cmd)`, and every stateless MCP server's tools (auto-reflected from `claude mcp list`, no flag needed). Only the script's return value enters context, rendered as TOON or compact JSON and capped at `--budget`. Scripts arrive as an argument, `--file`, or stdin; `--list-tools` prints the host-function catalog and the Python-subset rules. Unavailable on Intel Macs (darwin/amd64 — the embedded monty runtime ships no library there); every other command works.
- `ccx_exec` and `ccx_exec_tools` MCP tools expose the exec surface on the facade, backed by a resident engine.
- `CCX_EXEC_MCP=off` disables MCP auto-reflection; `CCX_EXEC_MCP_DENY` / `CCX_EXEC_MCP_ALLOW` (comma-separated server names) override the stateless classifier. Built-in denies: cc-context itself, `plugin:cc-review:*`, and any command whose basename is `ccx`.

### Changed
- Binaries grow from ~11 MB to ~25–27 MB on monty-supported targets (the embedded Python runtime).

### Fixed
- Command results now print to stdout, not stderr (a cobra wiring bug). Behavior change: scripts that captured results from stderr must read stdout instead.

[Unreleased]: https://github.com/yasyf/cc-context/commits/main
