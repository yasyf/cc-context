#!/usr/bin/env bash
# Builds per-language tree-sitter grammar WASM modules for internal/semsearch/chunk
# and stages them gzipped for go:embed. Each module bundles the tree-sitter runtime
# (v0.25.2), one grammar's parser.c (+scanner.c) at semble's pinned rev, and
# bridge.c, compiled to wasm32-wasi with `zig cc`. Sources are fetched into a
# gitignored cache; pass language names to build a subset (default: all).
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
pkg="$root/internal/semsearch/chunk"
wasmdir="$pkg/wasm"
build="$wasmdir/.build"
out="$pkg/grammars"
core="$build/tree-sitter"
TS_RUNTIME_REV="v0.25.2"

mkdir -p "$build/grammars" "$out"

command -v zig >/dev/null 2>&1 || { echo "build-chunk-grammars: zig not found (needed for wasm32-wasi)" >&2; exit 1; }

# lang|repo|rev|srcsubdir|symbol|generate — revs pinned to tree-sitter-language-pack
# 1.6.2. generate=1 marks a grammar with no committed parser.c: it is produced from
# src/grammar.json with tree-sitter-cli (matching language-pack's --abi 14 regen).
grammars=(
	"bash|https://github.com/tree-sitter/tree-sitter-bash|a06c2e4415e9bc0346c6b86d401879ffb44058f7|src|tree_sitter_bash"
	"c|https://github.com/tree-sitter/tree-sitter-c|ae19b676b13bdcc13b7665397e6d9b14975473dd|src|tree_sitter_c"
	"cpp|https://github.com/tree-sitter/tree-sitter-cpp|8b5b49eb196bec7040441bee33b2c9a4838d6967|src|tree_sitter_cpp"
	"csharp|https://github.com/tree-sitter/tree-sitter-c-sharp|cac6d5fb595f5811a076336682d5d595ac1c9e85|src|tree_sitter_c_sharp|1"
	"elixir|https://github.com/elixir-lang/tree-sitter-elixir|7937d3b4d65fa574163cfa59394515d3c1cf16f4|src|tree_sitter_elixir"
	"go|https://github.com/tree-sitter/tree-sitter-go|2346a3ab1bb3857b48b29d779a1ef9799a248cd7|src|tree_sitter_go"
	"haskell|https://github.com/tree-sitter/tree-sitter-haskell|0975ef72fc3c47b530309ca93937d7d143523628|src|tree_sitter_haskell"
	"java|https://github.com/tree-sitter/tree-sitter-java|e10607b45ff745f5f876bfa3e94fbcc6b44bdc11|src|tree_sitter_java"
	"javascript|https://github.com/tree-sitter/tree-sitter-javascript|58404d8cf191d69f2674a8fd507bd5776f46cb11|src|tree_sitter_javascript"
	"kotlin|https://github.com/fwcd/tree-sitter-kotlin|55622a49bd59ca42cec5c01ba5251bb4da9b8930|src|tree_sitter_kotlin"
	"lua|https://github.com/MunifTanjim/tree-sitter-lua|4fbec840c34149b7d5fe10097c93a320ee4af053|src|tree_sitter_lua"
	"php|https://github.com/tree-sitter/tree-sitter-php|3f2465c217d0a966d41e584b42d75522f2a3149e|php/src|tree_sitter_php"
	"python|https://github.com/tree-sitter/tree-sitter-python|26855eabccb19c6abf499fbc5b8dc7cc9ab8bc64|src|tree_sitter_python"
	"ruby|https://github.com/tree-sitter/tree-sitter-ruby|ad907a69da0c8a4f7a943a7fe012712208da6dee|src|tree_sitter_ruby"
	"rust|https://github.com/tree-sitter/tree-sitter-rust|77a3747266f4d621d0757825e6b11edcbf991ca5|src|tree_sitter_rust"
	"scala|https://github.com/tree-sitter/tree-sitter-scala|a06047ee441bd02ca6ebcc0f913737b7c6e178e1|src|tree_sitter_scala"
	"swift|https://github.com/alex-pinkus/tree-sitter-swift|e2b381615811f0dc5b6fb3fbc1a1b5046c1348b3|src|tree_sitter_swift|1"
	"typescript|https://github.com/tree-sitter/tree-sitter-typescript|75b3874edb2dc714fb1fd77a32013d0f8699989f|typescript/src|tree_sitter_typescript"
	"zig|https://github.com/maxxnino/tree-sitter-zig|a80a6e9be81b33b182ce6305ae4ea28e29211bd5|src|tree_sitter_zig"
)
TS_CLI_VERSION="0.25.2"

fetch() { # dir repo rev
	local dir=$1 repo=$2 rev=$3
	[ -d "$dir/.git" ] && return 0
	rm -rf "$dir"; mkdir -p "$dir"
	( cd "$dir" && git init -q && git remote add origin "$repo" \
		&& git fetch -q --depth 1 origin "$rev" && git checkout -q FETCH_HEAD )
}

fetch "$core" https://github.com/tree-sitter/tree-sitter "$TS_RUNTIME_REV"

want=("$@")
built=0
for row in "${grammars[@]}"; do
	IFS='|' read -r lang repo rev sub symbol gen <<<"$row"
	if [ ${#want[@]} -gt 0 ] && [[ ! " ${want[*]} " == *" $lang "* ]]; then
		continue
	fi
	dest="$out/$lang.wasm.gz"
	# Sources are rev-pinned, so a staged module is stale only when the bridge
	# or endian shim changed. Skip the fetch+compile otherwise.
	if [ -f "$dest" ] && [ "$dest" -nt "$wasmdir/bridge.c" ] && [ "$dest" -nt "$wasmdir/wasm_endian.h" ]; then
		continue
	fi
	gdir="$build/grammars/$lang"
	fetch "$gdir" "$repo" "$rev"
	srcdir="$gdir/$sub"
	# generate=1 grammars are regenerated from src/grammar.json at --abi 14,
	# matching language-pack (which ignores any committed parser.c for these).
	if [ "${gen:-}" = "1" ]; then
		if ! command -v npx >/dev/null 2>&1; then
			echo "build-chunk-grammars: skipping $lang (needs npx/tree-sitter-cli to generate parser.c)" >&2
			continue
		fi
		( cd "$gdir" && rm -f "$sub/parser.c" \
			&& npx -y "tree-sitter-cli@$TS_CLI_VERSION" generate --abi 14 "$sub/grammar.json" >/dev/null 2>&1 )
	fi
	scanner=""
	[ -f "$srcdir/scanner.c" ] && scanner="$srcdir/scanner.c"
	zig cc -target wasm32-wasi -mexec-model=reactor -Os -flto \
		-include "$wasmdir/wasm_endian.h" \
		"-DTS_LANGUAGE=$symbol" \
		-I "$core/lib/include" -I "$core/lib/src" -I "$srcdir" \
		-Wl,--gc-sections -Wl,--export-memory \
		-o "$build/$lang.wasm" \
		"$core/lib/src/lib.c" "$srcdir/parser.c" $scanner "$wasmdir/bridge.c"
	gzip -9 -c "$build/$lang.wasm" >"$dest"
	printf 'build-chunk-grammars: %-11s raw=%8d gz=%8d\n' "$lang" \
		"$(wc -c <"$build/$lang.wasm")" "$(wc -c <"$dest")"
	built=$((built + 1))
done
echo "build-chunk-grammars: staged $built module(s) into $out"
