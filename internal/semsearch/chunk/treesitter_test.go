package chunk

import "testing"

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
