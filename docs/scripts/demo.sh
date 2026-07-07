#!/usr/bin/env bash
# Regenerates docs/assets/demo.png from a real run of ccx against this repo.
# Requires: go, freeze (https://github.com/charmbracelet/freeze).
set -euo pipefail
cd "$(dirname "$0")/../.."

go build -trimpath -o bin/ccx ./cmd/ccx

demo_cmd="ccx code outline internal/astgrep/run.go --budget 60"
capture_dir="$(mktemp -d)"
trap 'rm -rf "$capture_dir"' EXIT
capture="$capture_dir/demo.ansi"
{
  printf '$ %s\n' "$demo_cmd" | bat --plain --color=always --language bash
  ./bin/$demo_cmd 2>&1 | bat --plain --color=always --language go
} >"$capture"

freeze "$capture" --language ansi \
  --theme github-dark --background "#0d1117" --window --padding 24 --font.size 28 \
  --output docs/assets/demo.png
