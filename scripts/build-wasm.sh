#!/usr/bin/env bash
# Builds the format-core WASM engine and copies it to internal/format for
# go:embed. Reads the artifact path from cargo JSON (honors a custom target dir).
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"

# rustup run + explicit RUSTC pin format-core/rust-toolchain.toml's toolchain
# even when a Homebrew cargo/rustc (no wasm32 std) shadows PATH.
cargo=(cargo)
if command -v rustup >/dev/null 2>&1; then
	toolchain="$(cd "$root/format-core" && rustup show active-toolchain | awk '{print $1}')"
	RUSTC="$(cd "$root/format-core" && rustup which rustc)"
	export RUSTC
	cargo=(rustup run "$toolchain" cargo)
elif [ -x "$HOME/.cargo/bin/cargo" ]; then
	cargo=("$HOME/.cargo/bin/cargo")
fi

artifact="$(
	"${cargo[@]}" build -p format-wasm --release --target wasm32-unknown-unknown \
		--manifest-path "$root/format-core/Cargo.toml" --message-format=json |
		grep -o '"[^"]*format_wasm\.wasm"' | tr -d '"' | head -n1
)"

if [ -z "$artifact" ]; then
	echo "build-wasm: could not locate format_wasm.wasm in cargo output" >&2
	exit 1
fi

cp "$artifact" "$root/internal/format/formatcore.wasm"
echo "build-wasm: embedded $artifact -> internal/format/formatcore.wasm"
