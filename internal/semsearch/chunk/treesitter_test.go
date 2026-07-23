package chunk

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
)

// grammarSnippets is one small, valid program per required language.
var grammarSnippets = map[string]string{
	"bash":       "echo hello\nfor i in 1 2 3; do echo $i; done\n",
	"c":          "int main(void) { return 0; }\n",
	"cpp":        "#include <vector>\nint main() { std::vector<int> v; return v.size(); }\n",
	"csharp":     "class C { static void M() { System.Console.WriteLine(1); } }\n",
	"elixir":     "defmodule M do\n  def f(x), do: x + 1\nend\n",
	"go":         "package main\n\nfunc main() { println(1) }\n",
	"haskell":    "module Main where\nmain :: IO ()\nmain = putStrLn \"hi\"\n",
	"java":       "class C { public static void main(String[] a) { System.out.println(1); } }\n",
	"javascript": "function f(x) { return x + 1; }\nconst y = f(2);\n",
	"kotlin":     "fun main() { println(\"hi\") }\n",
	"lua":        "local function f(x) return x + 1 end\nprint(f(2))\n",
	"php":        "<?php\nfunction f($x) { return $x + 1; }\n",
	"python":     "def f(x):\n    return x + 1\n",
	"ruby":       "def f(x)\n  x + 1\nend\n",
	"rust":       "fn main() {\n    println!(\"{}\", 1);\n}\n",
	"scala":      "object M {\n  def f(x: Int): Int = x + 1\n}\n",
	"swift":      "func f(x: Int) -> Int {\n    return x + 1\n}\n",
	"typescript": "function f(x: number): number {\n  return x + 1;\n}\n",
	"zig":        "const std = @import(\"std\");\npub fn main() void {}\n",
}

// TestGrammarsParse confirms every required grammar's WASM module loads and
// parses — the per-language status check. ok=false means the grammar is missing
// or the module trapped, which would silently degrade to line chunking.
func TestGrammarsParse(t *testing.T) {
	for lang, src := range grammarSnippets {
		t.Run(lang, func(t *testing.T) {
			root, ok := defaultParser.parse(lang, []byte(src))
			if !ok {
				t.Fatalf("%s: parse ok=false (grammar missing or trapped)", lang)
			}
			if int(root.end) != len(src) {
				t.Errorf("%s: root end = %d, want %d (whole source)", lang, root.end, len(src))
			}
			if len(root.children) == 0 {
				t.Errorf("%s: root has no children", lang)
			}
		})
	}
}

// TestParseTreeStructure pins the reconstructed tree for a known Python snippet
// against tree-sitter's output, proving pre-order + child-count reconstruction
// (including anonymous nodes like `def`, `(`, `:`, `+`) is exact.
func TestParseTreeStructure(t *testing.T) {
	root, ok := defaultParser.parse("python", []byte("def f(x):\n    return x + 1\n"))
	if !ok {
		t.Fatal("python parse ok=false")
	}
	// module [0,27]
	if root.start != 0 || root.end != 27 || len(root.children) != 1 {
		t.Fatalf("module = [%d,%d] children=%d, want [0,27] children=1", root.start, root.end, len(root.children))
	}
	// function_definition [0,26] with 5 children incl. anonymous def/:.
	fn := root.children[0]
	if fn.start != 0 || fn.end != 26 || len(fn.children) != 5 {
		t.Fatalf("function_definition = [%d,%d] children=%d, want [0,26] children=5", fn.start, fn.end, len(fn.children))
	}
	if def := fn.children[0]; def.start != 0 || def.end != 3 || len(def.children) != 0 {
		t.Errorf("def keyword = [%d,%d] children=%d, want [0,3] leaf", def.start, def.end, len(def.children))
	}
	// parameters [5,8] with 3 children: ( identifier )
	if params := fn.children[2]; params.start != 5 || params.end != 8 || len(params.children) != 3 {
		t.Errorf("parameters = [%d,%d] children=%d, want [5,8] children=3", params.start, params.end, len(params.children))
	}
}

// nestedChainBuf builds a bridge.c-format flat buffer for n nodes forming a
// single-child chain (each node has one child except the innermost leaf).
func nestedChainBuf(n int) []byte {
	buf := make([]byte, 4+n*12)
	binary.LittleEndian.PutUint32(buf[0:], uint32(n)) //nolint:gosec // test fixture size
	for i := range n {
		base := 4 + i*12
		// start=0, end=n, plus the child count.
		binary.LittleEndian.PutUint32(buf[base:], 0)
		binary.LittleEndian.PutUint32(buf[base+4:], uint32(n)) //nolint:gosec // test fixture size
		nchildren := uint32(1)
		if i == n-1 {
			nchildren = 0
		}
		binary.LittleEndian.PutUint32(buf[base+8:], nchildren)
	}
	return buf
}

// chainDepth counts the edges from root down its first-child spine.
func chainDepth(root node) int {
	d := 0
	for n := root; len(n.children) > 0; n = n.children[0] {
		d++
	}
	return d
}

// TestReconstructTreeClampsDepth pins C3: reconstruction stops materializing at
// the chunker's recursionDepth guard, so a pathologically deep tree — which the
// chunker ignores below that depth anyway — is bounded to a constant number of
// stack frames and cannot exhaust the Go stack. Trees shallower than the guard
// are materialized in full, unchanged.
func TestReconstructTreeClampsDepth(t *testing.T) {
	tests := []struct {
		name      string
		nodes     int
		wantDepth int
	}{
		{"shallow materialized in full", 100, 99},
		{"past the guard is clamped", recursionDepth + 200, recursionDepth + 1},
		{"pathologically deep stays bounded", 200_000, recursionDepth + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := reconstructTree(nestedChainBuf(tt.nodes))
			if got := chainDepth(root); got != tt.wantDepth {
				t.Errorf("chain depth = %d, want %d", got, tt.wantDepth)
			}
		})
	}
}

// TestCompileUsesInitBudgetNotCallBudget pins C2: a grammar's first cold compile
// runs under the wide init budget, never the tight per-parse deadline, so a slow
// compile cannot silently downgrade a file to line chunking. The parse still
// succeeds (AST-chunked) and the compile context carries far more than the call
// budget's worth of time.
func TestCompileUsesInitBudgetNotCallBudget(t *testing.T) {
	eng, err := loadTSEngine()
	if err != nil {
		t.Fatalf("loadTSEngine: %v", err)
	}
	// Force a fresh compile so compiledFor actually invokes the compiler.
	eng.mu.Lock()
	delete(eng.compiled, "python")
	eng.mu.Unlock()

	var compileRemaining time.Duration
	var sawCompile bool
	orig := compileModule
	compileModule = func(ctx context.Context, rt wazero.Runtime, wasm []byte) (wazero.CompiledModule, error) {
		if deadline, ok := ctx.Deadline(); ok {
			compileRemaining = time.Until(deadline)
			sawCompile = true
		}
		return orig(ctx, rt, wasm)
	}
	defer func() { compileModule = orig }()

	_, ok := defaultParser.parse("python", []byte("def f(x):\n    return x + 1\n"))
	if !ok {
		t.Fatal("python parse ok=false; a slow-or-uncached compile must not downgrade to line chunking")
	}
	if !sawCompile {
		t.Fatal("compile seam not invoked; expected a fresh cold compile")
	}
	// A shared parse deadline would leave ~tsCallTimeout here; the separate init
	// budget leaves far more.
	if compileRemaining <= tsCallTimeout {
		t.Errorf("compile budget remaining = %v, want > tsCallTimeout (%v): compile must not share the parse deadline",
			compileRemaining, tsCallTimeout)
	}
}
