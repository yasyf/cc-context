#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

artifacts=(
	internal/format/formatcore.wasm
	internal/semsearch/embed/embedcore.wasm
	internal/semsearch/chunk/grammars
)
inputs=(
	scripts/build-wasm.sh
	scripts/build-embed-wasm.sh
	scripts/build-chunk-grammars.sh
	format-core
	embed-core
	internal/semsearch/chunk/wasm/bridge.c
	internal/semsearch/chunk/wasm/wasm_endian.h
)

present() {
	case "$1" in
	*/grammars) compgen -G "$1/*.wasm.gz" >/dev/null ;;
	*) [ -f "$1" ] ;;
	esac
}

missing=()
for a in "${artifacts[@]}"; do
	present "$a" || missing+=("$a")
done
[ ${#missing[@]} -eq 0 ] && exit 0

key="$(git ls-files -z -- "${inputs[@]}" | xargs -0 git hash-object -- | shasum -a 256 | cut -c1-16)"
cache="${XDG_CACHE_HOME:-$HOME/.cache}/cc-context/wasm/$key"

if [ ! -d "$cache" ]; then
	echo "ensure-wasm: building the go:embed WASM artifacts (cache $key missing; later worktrees reuse it)" >&2
	bash scripts/build-wasm.sh >&2
	bash scripts/build-embed-wasm.sh >&2
	bash scripts/build-chunk-grammars.sh >&2
	mkdir -p "${cache%/*}"
	staging="$(mktemp -d "${cache%/*}/.staging.XXXXXX")"
	for a in "${artifacts[@]}"; do
		mkdir -p "$staging/$(dirname "$a")"
		cp -R "$a" "$staging/$a"
	done
	if [ -d "$cache" ]; then
		rm -rf "$staging"
	else
		mv "$staging" "$cache"
	fi
	exit 0
fi

for a in "${missing[@]}"; do
	mkdir -p "$(dirname "$a")"
	rm -rf "$a"
	cp -R "$cache/$a" "$a"
done
echo "ensure-wasm: restored ${missing[*]} from cache $key" >&2
