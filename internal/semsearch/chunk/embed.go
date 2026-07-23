package chunk

import "embed"

// grammarFS holds the gzipped per-language tree-sitter WASM modules, staged by
// scripts/build-chunk-grammars.sh (task wasm). Each is named "<language>.wasm.gz"
// and bundles the tree-sitter runtime, one grammar, and wasm/bridge.c.
//
//go:embed grammars/*.wasm.gz
var grammarFS embed.FS
