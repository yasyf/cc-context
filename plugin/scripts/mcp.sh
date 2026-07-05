#!/bin/sh
# MCP launcher: provision ccx (installer output to stderr — stdout is the MCP
# transport), then exec the stdio server. bin/ccx is a symlink by design; with
# nothing resolved the exec fails loudly.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
"$ROOT/scripts/install-binary.sh" >&2 || true
exec "$ROOT/bin/ccx" mcp
