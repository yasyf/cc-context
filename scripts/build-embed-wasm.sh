#!/usr/bin/env bash
# Builds the embed-core WASM engine (model2vec-rs static-embedding inference)
# and copies it to internal/semsearch/embed for go:embed. Reads the artifact
# path from cargo JSON (honors a custom target dir).
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"

# rustup run + explicit RUSTC pin embed-core/rust-toolchain.toml's toolchain
# even when a Homebrew cargo/rustc (no wasm32 std) shadows PATH.
cargo=(cargo)
if command -v rustup >/dev/null 2>&1; then
	toolchain="$(cd "$root/embed-core" && rustup show active-toolchain | awk '{print $1}')"
	RUSTC="$(cd "$root/embed-core" && rustup which rustc)"
	export RUSTC
	cargo=(rustup run "$toolchain" cargo)
elif [ -x "$HOME/.cargo/bin/cargo" ]; then
	cargo=("$HOME/.cargo/bin/cargo")
fi

# CWD must be embed-core so cargo reads embed-core/.cargo/config.toml (the
# getrandom `custom` backend rustflags that keep the module import-free).
artifact="$(
	cd "$root/embed-core" &&
		"${cargo[@]}" build -p embed-wasm --release --target wasm32-unknown-unknown \
			--message-format=json |
		grep -o '"[^"]*embed_wasm\.wasm"' | tr -d '"' | head -n1
)"

if [ -z "$artifact" ]; then
	echo "build-embed-wasm: could not locate embed_wasm.wasm in cargo output" >&2
	exit 1
fi

cp "$artifact" "$root/internal/semsearch/embed/embedcore.wasm"
echo "build-embed-wasm: embedded $artifact -> internal/semsearch/embed/embedcore.wasm"
