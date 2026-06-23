package backend

import (
	"context"
	"reflect"
	"slices"
	"testing"
)

const fakeAstGrep = "/fake/ast-grep"

func TestAstGrepCLIArgv(t *testing.T) {
	g := AstGrep{Bin: fakeAstGrep}
	tests := []struct {
		name string
		op   Op
		args Args
		argv []string
	}{
		{
			"structural default path",
			OpStructural,
			Args{Query: "return $A, nil"},
			[]string{"run", "-p", "return $A, nil", "--json=stream", "."},
		},
		{
			"structural lang glob paths",
			OpStructural,
			Args{Query: "$A.Foo($$$)", Lang: "go", Glob: "!*_test.go", Paths: []string{"internal", "cmd"}},
			[]string{"run", "-p", "$A.Foo($$$)", "--json=stream", "-l", "go", "--globs", "!*_test.go", "internal", "cmd"},
		},
		{
			"replace preview (no -U)",
			OpReplace,
			Args{Pattern: "Add($A,$B)", Rewrite: "Sum($A,$B)"},
			[]string{"run", "-p", "Add($A,$B)", "-r", "Sum($A,$B)", "--json=stream", "."},
		},
		{
			// Apply omits --json=stream: with it present, -U prints JSON and
			// writes nothing, so the rewrite would silently no-op.
			"replace apply (-U, no --json=stream)",
			OpReplace,
			Args{Pattern: "Add($A,$B)", Rewrite: "Sum($A,$B)", Apply: true},
			[]string{"run", "-p", "Add($A,$B)", "-r", "Sum($A,$B)", "-U", "."},
		},
		{
			"replace apply with lang glob paths",
			OpReplace,
			Args{Pattern: "Add($A)", Rewrite: "Inc($A)", Apply: true, Lang: "go", Glob: "*.go", Paths: []string{"pkg"}},
			[]string{"run", "-p", "Add($A)", "-r", "Inc($A)", "-U", "-l", "go", "--globs", "*.go", "pkg"},
		},
		{
			"struct-outline default path",
			OpStructOutline,
			Args{Path: "internal/backend"},
			[]string{"outline", "internal/backend", "--json=stream", "--view", "expanded"},
		},
		{
			"struct-outline items match lang",
			OpStructOutline,
			Args{Path: "src", Items: "exports", Match: "^New", Lang: "go"},
			[]string{"outline", "src", "--json=stream", "--view", "expanded", "--items", "exports", "--match", "^New", "-l", "go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, argv, err := g.CLIArgv(context.Background(), tt.op, tt.args)
			if err != nil {
				t.Fatalf("CLIArgv: %v", err)
			}
			if bin != fakeAstGrep {
				t.Errorf("bin = %q, want %q", bin, fakeAstGrep)
			}
			if !reflect.DeepEqual(argv, tt.argv) {
				t.Errorf("argv = %v, want %v", argv, tt.argv)
			}
			if slices.Contains(argv, "-i") || slices.Contains(argv, "--interactive") {
				t.Errorf("argv must never carry interactive mode: %v", argv)
			}
		})
	}
}

func TestAstGrepCLIArgvUnsupported(t *testing.T) {
	if _, _, err := (AstGrep{Bin: fakeAstGrep}).CLIArgv(context.Background(), OpSearch, Args{}); err == nil {
		t.Fatal("expected error for unsupported op")
	}
}

func TestAstGrepEngine(t *testing.T) {
	if got := (AstGrep{}).Engine(); got != EngineAstGrep {
		t.Errorf("Engine() = %q, want %q", got, EngineAstGrep)
	}
}

func TestAstGrepMCPErrors(t *testing.T) {
	if _, _, err := (AstGrep{}).MCPSpec(context.Background()); err == nil {
		t.Fatal("MCPSpec: want error (ast-grep has no MCP server)")
	}
	if _, _, err := (AstGrep{}).MCPTool(OpStructural, Args{}); err == nil {
		t.Fatal("MCPTool: want error (ast-grep has no MCP tool)")
	}
}
