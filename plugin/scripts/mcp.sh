#!/bin/sh
# MCP launcher: exec ccx's stdio server. bin/ccx is the binrun wrapper — it
# resolves the version-exact artifact (bootstrapping the runner if needed) and
# execs it, so a bare invocation both provisions and runs. stdout is the MCP
# transport; the wrapper and binrun log to stderr, and SessionStart pre-warms
# the cache so a cold resolve never races the first request.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
exec "$ROOT/bin/ccx" mcp
