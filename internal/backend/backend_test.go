package backend

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/vendor"
)

const (
	fakeTilth  = "/fake/tilth"
	fakeSemble = "/fake/semble"
)

func TestTilthCLIArgv(t *testing.T) {
	tl := Tilth{Bin: fakeTilth}
	tests := []struct {
		name string
		op   Op
		args Args
		argv []string
	}{
		{"outline", OpOutline, Args{Path: "a.go"}, []string{"a.go"}},
		{"outline budget", OpOutline, Args{Path: "a.go", Budget: 500}, []string{"a.go", "--budget", "500"}},
		{"read full", OpRead, Args{Path: "a.go", Full: true}, []string{"a.go", "--full"}},
		{"read section", OpRead, Args{Path: "a.go", Section: "10-20"}, []string{"a.go", "--section", "10-20"}},
		{"read full beats section", OpRead, Args{Path: "a.go", Full: true, Section: "10-20"}, []string{"a.go", "--full"}},
		{"read section budget", OpRead, Args{Path: "a.go", Section: "## H", Budget: 99}, []string{"a.go", "--section", "## H", "--budget", "99"}},
		{"symbol", OpSymbol, Args{Query: "Foo"}, []string{"grok", "Foo"}},
		{"symbol scope full", OpSymbol, Args{Query: "Foo", Scope: "pkg", Full: true}, []string{"grok", "Foo", "--scope", "pkg", "--full"}},
		{"deps", OpDeps, Args{Path: "a.go"}, []string{"a.go", "--deps"}},
		{"deps scope budget", OpDeps, Args{Path: "a.go", Scope: "pkg", Budget: 7}, []string{"a.go", "--deps", "--scope", "pkg", "--budget", "7"}},
		{"grep", OpGrep, Args{Query: "todo"}, []string{"todo"}},
		{"grep glob budget expand", OpGrep, Args{Query: "todo", Glob: "*.go", Budget: 3, Expand: 2}, []string{"todo", "--glob", "*.go", "--budget", "3", "--expand=2"}},
		{"grep glob scope budget expand", OpGrep, Args{Query: "todo", Glob: "*.go", Scope: "internal", Budget: 3, Expand: 2}, []string{"todo", "--glob", "*.go", "--scope", "internal", "--budget", "3", "--expand=2"}},
		{"grep scope only", OpGrep, Args{Query: "todo", Scope: "internal"}, []string{"todo", "--scope", "internal"}},
		{"grep ignore-case/word not routed to tilth", OpGrep, Args{Query: "todo", IgnoreCase: true, Word: true}, []string{"todo"}},
		{"grep regex/paths not routed to tilth", OpGrep, Args{Query: "todo", Regex: true, Paths: []string{"a.go", "b.go"}}, []string{"todo"}},
		{"overview", OpOverview, Args{}, []string{"overview"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, argv, err := tl.CLIArgv(context.Background(), tt.op, tt.args)
			if err != nil {
				t.Fatalf("CLIArgv: %v", err)
			}
			if bin != fakeTilth {
				t.Errorf("bin = %q, want %q", bin, fakeTilth)
			}
			if !reflect.DeepEqual(argv, tt.argv) {
				t.Errorf("argv = %v, want %v", argv, tt.argv)
			}
		})
	}
}

func TestTilthCLIArgvDiff(t *testing.T) {
	// In a plain-git repo vcs.ResolveDiffSource passes the source through and
	// selects tilth. Run inside a controlled git repo so the result does not
	// depend on the ambient VCS (the cc-context tree is itself a colocated jj
	// repo, where a single ref resolves to a ref..@ commit range instead).
	t.Chdir(initGitRepoForDiff(t))
	tl := Tilth{Bin: fakeTilth}
	bin, argv, err := tl.CLIArgv(context.Background(), OpDiff, Args{Source: "HEAD~1", Scope: "pkg", Budget: 4})
	if err != nil {
		t.Fatalf("CLIArgv: %v", err)
	}
	if bin != fakeTilth {
		t.Errorf("bin = %q, want %q", bin, fakeTilth)
	}
	want := []string{"diff", "HEAD~1", "--scope", "pkg", "--budget", "4"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
}

func initGitRepoForDiff(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git argv; dir is a test TempDir, args are literals
		cmd.Env = isolatedGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t.t")
	git("config", "user.name", "t")
	// Two commits so HEAD~1 resolves past the git-branch ref validation.
	seed := filepath.Join(dir, "seed.txt")
	for _, content := range []string{"one\n", "two\n"} {
		if err := os.WriteFile(seed, []byte(content), 0o600); err != nil {
			t.Fatalf("write seed: %v", err)
		}
		git("add", "-A")
		git("commit", "-qm", "c")
	}
	return dir
}

// isolatedGitEnv detaches git from the developer's ambient config so a global
// setting like commit.gpgsign cannot break the test-repo commits; identity comes
// from the repo-local user.name/user.email initGitRepoForDiff sets.
func isolatedGitEnv() []string {
	return append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
}

func TestTilthCLIArgvUnsupported(t *testing.T) {
	if _, _, err := (Tilth{Bin: fakeTilth}).CLIArgv(context.Background(), OpSearch, Args{}); err == nil {
		t.Fatal("expected error for unsupported op")
	}
}

// TestTilthFindUnsupported asserts tilth rejects OpFind on both surfaces, so any
// route that reaches it instead of the native find package fails loudly.
func TestTilthFindUnsupported(t *testing.T) {
	tl := Tilth{Bin: fakeTilth}
	if _, _, err := tl.CLIArgv(context.Background(), OpFind, Args{Glob: "**/*.go"}); err == nil {
		t.Error("CLIArgv(OpFind): expected unsupported-op error")
	}
	if _, _, err := tl.MCPTool(OpFind, Args{Glob: "**/*.go"}); err == nil {
		t.Error("MCPTool(OpFind): expected unsupported-op error")
	}
}

func TestTilthMCPSpec(t *testing.T) {
	cmd, argv, err := Tilth{Bin: fakeTilth}.MCPSpec(context.Background())
	if err != nil {
		t.Fatalf("MCPSpec: %v", err)
	}
	if cmd != fakeTilth {
		t.Errorf("cmd = %q, want %q", cmd, fakeTilth)
	}
	if !reflect.DeepEqual(argv, []string{"--mcp", "--no-overview"}) {
		t.Errorf("argv = %v, want [--mcp --no-overview]", argv)
	}
}

func TestTilthMCPTool(t *testing.T) {
	tl := Tilth{Bin: fakeTilth}
	tests := []struct {
		name   string
		op     Op
		args   Args
		tool   string
		params map[string]any
	}{
		{"outline", OpOutline, Args{Path: "a.go", Budget: 500}, "tilth_read", map[string]any{"path": "a.go", "mode": "signature", "budget": 500}},
		{"outline no budget", OpOutline, Args{Path: "a.go"}, "tilth_read", map[string]any{"path": "a.go", "mode": "signature"}},
		{"read full", OpRead, Args{Path: "a.go", Full: true}, "tilth_read", map[string]any{"path": "a.go", "full": true}},
		{"read section", OpRead, Args{Path: "a.go", Section: "## H", Budget: 9}, "tilth_read", map[string]any{"path": "a.go", "section": "## H", "budget": 9}},
		{"symbol", OpSymbol, Args{Query: "Foo", Scope: "pkg", Full: true}, "tilth_grok", map[string]any{"target": "Foo", "scope": "pkg", "full": true}},
		{"symbol minimal", OpSymbol, Args{Query: "Foo"}, "tilth_grok", map[string]any{"target": "Foo"}},
		{"deps", OpDeps, Args{Path: "a.go", Scope: "pkg", Budget: 7}, "tilth_deps", map[string]any{"path": "a.go", "scope": "pkg", "budget": 7}},
		{"grep", OpGrep, Args{Query: "todo", Glob: "*.go", Kind: "code", Budget: 3, Expand: 2}, "tilth_search", map[string]any{"query": "todo", "glob": "*.go", "kind": "code", "budget": 3, "expand": 2}},
		{"grep scope", OpGrep, Args{Query: "todo", Glob: "*.go", Scope: "internal", Kind: "code", Budget: 3, Expand: 2}, "tilth_search", map[string]any{"query": "todo", "glob": "*.go", "scope": "internal", "kind": "code", "budget": 3, "expand": 2}},
		{"grep ignore-case/word absent from tilth params", OpGrep, Args{Query: "todo", IgnoreCase: true, Word: true}, "tilth_search", map[string]any{"query": "todo"}},
		{"grep regex/paths absent from tilth params", OpGrep, Args{Query: "todo", Regex: true, Paths: []string{"a.go"}}, "tilth_search", map[string]any{"query": "todo"}},
		{"diff", OpDiff, Args{Source: "HEAD~1", Scope: "pkg", Budget: 4}, "tilth_diff", map[string]any{"source": "HEAD~1", "scope": "pkg", "budget": 4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, params, err := tl.MCPTool(tt.op, tt.args)
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

func TestTilthMCPToolOverviewErrors(t *testing.T) {
	if _, _, err := (Tilth{Bin: fakeTilth}).MCPTool(OpOverview, Args{}); err == nil {
		t.Fatal("expected error: overview has no MCP tool")
	}
}

func TestSembleCLIArgv(t *testing.T) {
	s := Semble{Bin: fakeSemble}
	tests := []struct {
		name string
		op   Op
		args Args
		argv []string
	}{
		{"search", OpSearch, Args{Query: "auth flow"}, []string{"search", "auth flow"}},
		{"search path k", OpSearch, Args{Query: "auth", Path: "src", K: 5}, []string{"search", "auth", "src", "-k", "5"}},
		{"search all flags", OpSearch, Args{Query: "auth", Path: "src", K: 3, MaxSnippetLines: 8, Kind: "code"}, []string{"search", "auth", "src", "-k", "3", "--max-snippet-lines", "8", "--content", "code"}},
		{"related", OpRelated, Args{Query: "a.go:42"}, []string{"find-related", "a.go", "42"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, argv, err := s.CLIArgv(context.Background(), tt.op, tt.args)
			if err != nil {
				t.Fatalf("CLIArgv: %v", err)
			}
			if bin != fakeSemble {
				t.Errorf("bin = %q, want %q", bin, fakeSemble)
			}
			if !reflect.DeepEqual(argv, tt.argv) {
				t.Errorf("argv = %v, want %v", argv, tt.argv)
			}
		})
	}
}

func TestSembleCLIArgvResolution(t *testing.T) {
	orig := vendor.LookPath
	defer func() { vendor.LookPath = orig }()

	vendor.LookPath = func(string) string { return "/usr/bin/semble" }
	if bin, _, _ := (Semble{}).CLIArgv(context.Background(), OpSearch, Args{Query: "x"}); bin != "semble" {
		t.Errorf("on-PATH bin = %q, want semble", bin)
	}

	vendor.LookPath = func(string) string { return "" }
	bin, argv, _ := (Semble{}).CLIArgv(context.Background(), OpSearch, Args{Query: "x"})
	if bin != "uvx" {
		t.Errorf("fallback bin = %q, want uvx", bin)
	}
	want := []string{"--from", "semble[mcp]", "semble", "search", "x"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("fallback argv = %v, want %v", argv, want)
	}
}

func TestSembleCLIArgvUnsupported(t *testing.T) {
	if _, _, err := (Semble{Bin: fakeSemble}).CLIArgv(context.Background(), OpDeps, Args{}); err == nil {
		t.Fatal("expected error for unsupported op")
	}
}

func TestSembleMCPSpec(t *testing.T) {
	orig := vendor.LookPath
	defer func() { vendor.LookPath = orig }()
	want := []string{"--from", "semble[mcp]>=0.5", "semble"}

	// The MCP launch must be uvx --from semble[mcp]>=0.5 regardless of an on-PATH
	// semble or a configured Bin: the bare CLI has no MCP-server mode, and the
	// floor guarantees per-query index revalidation. No positional path may ride
	// along — semble's argument parsing rejects one. Guard the regression where
	// MCPSpec was "unified" with the CLI's resolve().
	for _, tc := range []struct {
		name   string
		onPath string
		s      Semble
	}{
		{"semble not on PATH", "", Semble{}},
		{"semble on PATH", "/usr/bin/semble", Semble{}},
		{"Bin configured", "", Semble{Bin: fakeSemble}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vendor.LookPath = func(string) string { return tc.onPath }
			cmd, argv, err := tc.s.MCPSpec(context.Background())
			if err != nil {
				t.Fatalf("MCPSpec: %v", err)
			}
			if cmd != "uvx" || !reflect.DeepEqual(argv, want) {
				t.Errorf("MCPSpec = %q %v, want uvx %v", cmd, argv, want)
			}
		})
	}
}

func TestSembleMCPTool(t *testing.T) {
	s := Semble{}
	tests := []struct {
		name   string
		op     Op
		args   Args
		tool   string
		params map[string]any
	}{
		{"search", OpSearch, Args{Query: "auth", Path: "/repo"}, "search", map[string]any{"query": "auth", "repo": "/repo"}},
		{"search all", OpSearch, Args{Query: "auth", Path: "src", K: 4, MaxSnippetLines: 6}, "search", map[string]any{"query": "auth", "repo": "src", "top_k": 4, "max_snippet_lines": 6}},
		{"related", OpRelated, Args{Query: "a.go:42", Path: "/repo"}, "find_related", map[string]any{"file_path": "a.go", "line": 42, "repo": "/repo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, params, err := s.MCPTool(tt.op, tt.args)
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

func TestSembleRepoDefaultsToCwd(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []Op{OpSearch, OpRelated} {
		_, params, err := (Semble{}).MCPTool(op, Args{Query: "a.go:1"})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if params["repo"] != wd {
			t.Errorf("%s: repo = %v, want cwd %q", op, params["repo"], wd)
		}
	}
}

func TestSembleMCPToolRelatedBadLoc(t *testing.T) {
	if _, _, err := (Semble{}).MCPTool(OpRelated, Args{Query: "no-line-here"}); err == nil {
		t.Fatal("expected error for loc without line")
	}
	if _, _, err := (Semble{}).MCPTool(OpRelated, Args{Query: "a.go:notanumber"}); err == nil {
		t.Fatal("expected error for non-numeric line")
	}
}
