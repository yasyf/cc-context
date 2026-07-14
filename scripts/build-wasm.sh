#!/usr/bin/env bash
# Builds the format-core WASM engine and copies it to internal/format for
# go:embed. Reads the artifact path from cargo JSON (honors a custom target dir).
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"

# rustup's cargo honors format-core/rust-toolchain.toml; a Homebrew cargo
# shadowing it on PATH ignores the pin and lacks the wasm32 std.
cargo="cargo"
[ -x "$HOME/.cargo/bin/cargo" ] && cargo="$HOME/.cargo/bin/cargo"

artifact="$(
	"$cargo" build -p format-wasm --release --target wasm32-unknown-unknown \
		--manifest-path "$root/format-core/Cargo.toml" --message-format=json |
		grep -o '"[^"]*format_wasm\.wasm"' | tr -d '"' | head -n1
)"

if [ -z "$artifact" ]; then
	echo "build-wasm: could not locate format_wasm.wasm in cargo output" >&2
	exit 1
fi

cp "$artifact" "$root/internal/format/formatcore.wasm"
echo "build-wasm: embedded $artifact -> internal/format/formatcore.wasm"
