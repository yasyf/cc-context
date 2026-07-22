package semble

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/lookpath"
)

func TestCLIArgv(t *testing.T) {
	orig := lookpath.Find
	t.Cleanup(func() { lookpath.Find = orig })
	lookpath.Find = func(string) string { return "/usr/bin/semble" }

	tests := []struct {
		name string
		op   backend.Op
		args backend.Args
		argv []string
	}{
		{"search", backend.OpSearch, backend.Args{Query: "auth flow"}, []string{"search", "auth flow"}},
		{"search path k", backend.OpSearch, backend.Args{Query: "auth", Path: "src", K: 5}, []string{"search", "auth", "src", "-k", "5"}},
		{"search all flags", backend.OpSearch, backend.Args{Query: "auth", Path: "src", K: 3, MaxSnippetLines: 8, Kind: "code"}, []string{"search", "auth", "src", "-k", "3", "--max-snippet-lines", "8", "--content", "code"}},
		{"search multiple content kinds", backend.OpSearch, backend.Args{Query: "auth", Kind: "code docs"}, []string{"search", "auth", "--content", "code", "docs"}},
		{"related", backend.OpRelated, backend.Args{Query: "a.go:42"}, []string{"find-related", "a.go", "42"}},
		{"related path and content", backend.OpRelated, backend.Args{Query: "a.go:42", Path: "/repo", Kind: "code docs"}, []string{"find-related", "a.go", "42", "/repo", "--content", "code", "docs"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, argv, err := CLIArgv(context.Background(), tt.op, tt.args)
			if err != nil {
				t.Fatalf("CLIArgv: %v", err)
			}
			if bin != "semble" {
				t.Errorf("bin = %q, want semble", bin)
			}
			if !reflect.DeepEqual(argv, tt.argv) {
				t.Errorf("argv = %v, want %v", argv, tt.argv)
			}
		})
	}
}

func TestCLIArgvResolution(t *testing.T) {
	orig := lookpath.Find
	t.Cleanup(func() { lookpath.Find = orig })

	lookpath.Find = func(string) string { return "/usr/bin/semble" }
	if bin, _, _ := CLIArgv(context.Background(), backend.OpSearch, backend.Args{Query: "x"}); bin != "semble" {
		t.Errorf("on-PATH bin = %q, want semble", bin)
	}

	lookpath.Find = func(string) string { return "" }
	bin, argv, _ := CLIArgv(context.Background(), backend.OpSearch, backend.Args{Query: "x"})
	if bin != "uvx" {
		t.Errorf("fallback bin = %q, want uvx", bin)
	}
	want := []string{"--from", "semble[mcp]", "semble", "search", "x"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("fallback argv = %v, want %v", argv, want)
	}
}

func TestCLIArgvUnsupported(t *testing.T) {
	if _, _, err := CLIArgv(context.Background(), backend.OpDeps, backend.Args{}); err == nil {
		t.Fatal("expected error for unsupported op")
	}
}

// TestMCPSpec guards the regression where MCPSpec was "unified" with resolve():
// the MCP launch is always uvx --from semble[mcp]>=0.5 semble (no path, no PATH
// resolution), since the bare CLI has no MCP-server mode.
func TestMCPSpec(t *testing.T) {
	orig := lookpath.Find
	t.Cleanup(func() { lookpath.Find = orig })
	want := []string{"--from", "semble[mcp]>=0.5", "semble", "--content", "code", "docs"}

	for _, onPath := range []string{"", "/usr/bin/semble"} {
		lookpath.Find = func(string) string { return onPath }
		cmd, argv, err := MCPSpec(context.Background())
		if err != nil {
			t.Fatalf("MCPSpec: %v", err)
		}
		if cmd != "uvx" || !reflect.DeepEqual(argv, want) {
			t.Errorf("MCPSpec = %q %v, want uvx %v", cmd, argv, want)
		}
	}
}

func TestMCPTool(t *testing.T) {
	tests := []struct {
		name   string
		op     backend.Op
		args   backend.Args
		tool   string
		params map[string]any
	}{
		{"search", backend.OpSearch, backend.Args{Query: "auth", Path: "/repo"}, "search", map[string]any{"query": "auth", "repo": "/repo"}},
		{"search all", backend.OpSearch, backend.Args{Query: "auth", Path: "src", K: 4, MaxSnippetLines: 6}, "search", map[string]any{"query": "auth", "repo": "src", "top_k": 4, "max_snippet_lines": 6}},
		{"related", backend.OpRelated, backend.Args{Query: "a.go:42", Path: "/repo"}, "find_related", map[string]any{"file_path": "a.go", "line": 42, "repo": "/repo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, params, err := MCPTool(tt.op, tt.args)
			if err != nil {
				t.Fatalf("MCPTool: %v", err)
			}
			if tool != tt.tool {
				t.Errorf("tool = %q, want %q", tool, tt.tool)
			}
			if !reflect.DeepEqual(params, tt.params) {
				t.Errorf("params = %v, want %v", params, tt.params)
			}
		})
	}
}

func TestRepoDefaultsToCwd(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []backend.Op{backend.OpSearch, backend.OpRelated} {
		_, params, err := MCPTool(op, backend.Args{Query: "a.go:1"})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if params["repo"] != wd {
			t.Errorf("%s: repo = %v, want cwd %q", op, params["repo"], wd)
		}
	}
}

func TestMCPToolRelatedBadLoc(t *testing.T) {
	if _, _, err := MCPTool(backend.OpRelated, backend.Args{Query: "no-line-here"}); err == nil {
		t.Fatal("expected error for loc without line")
	}
	if _, _, err := MCPTool(backend.OpRelated, backend.Args{Query: "a.go:notanumber"}); err == nil {
		t.Fatal("expected error for non-numeric line")
	}
}
