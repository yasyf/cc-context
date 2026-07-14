# format-wasm

WASM (`wasm32-unknown-unknown`) envelope adapter over `format-core`, with zero host imports (no WASI, no I/O) so a Go embedder can instantiate an import-free module; `panic = "abort"` turns any host-contract violation into a trap.

**ABI** — `fc_alloc(len: u32) -> u32` leaks a buffer for the host to write the request into; `fc_format(ptr: u32, len: u32) -> u64` returns the response's `(out_ptr << 32) | out_len`. Instances are one-shot, so both allocations leak. Request `{"src", "format"?, "indent"?, "delimiter"?, "allow"?}` (JSON), response `{"ok":{"format","text"}}` or `{"err":{"kind","msg"}}` where `kind` ∈ `not_json | unknown_format | unsupported_shape | unsafe_number`.

**Build** — `cargo build -p format-wasm --release --target wasm32-unknown-unknown`.

**Size** — ~250 KB stripped (`opt-level="z"`, `lto`, `strip`, `panic="abort"`). The adapter itself is tiny; the bulk is `format-core`'s own graph compiled to wasm — `num-bigint`/`num-rational` (TOON number safety) and `toon-format`'s transitive `serde_json`/`indexmap`.
