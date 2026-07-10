package backend

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestTilthGrepArgv(t *testing.T) {
	sub, missing, file := anchorFixture(t)
	parent := filepath.Dir(sub)
	tilth := Tilth{Bin: "tilth"}
	tests := []struct {
		name string
		args Args
		want []string
	}{
		{"anchored existing dir → scope + rest", Args{Query: "foo", Glob: sub + "/*.go"}, []string{"foo", "--glob", "*.go", "--scope", sub}},
		{"nonexistent prefix → unchanged", Args{Query: "foo", Glob: missing + "/*.go"}, []string{"foo", "--glob", missing + "/*.go"}},
		{"explicit scope composes onto join", Args{Query: "foo", Glob: "pkg/*.go", Scope: parent}, []string{"foo", "--glob", "*.go", "--scope", sub}},
		{"explicit scope nonexistent join → unchanged", Args{Query: "foo", Glob: "nope/*.go", Scope: parent}, []string{"foo", "--glob", "nope/*.go", "--scope", parent}},
		{"literal file glob → parent scope + basename", Args{Query: "foo", Glob: file}, []string{"foo", "--glob", "file.go", "--scope", sub}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, argv, err := tilth.CLIArgv(context.Background(), OpGrep, tt.args)
			if err != nil {
				t.Fatalf("CLIArgv() err: %v", err)
			}
			if !slices.Equal(argv, tt.want) {
				t.Errorf("CLIArgv argv = %v, want %v", argv, tt.want)
			}
		})
	}
}

func TestTilthGrepMCPTool(t *testing.T) {
	sub, missing, file := anchorFixture(t)
	parent := filepath.Dir(sub)
	var tilth Tilth
	tests := []struct {
		name      string
		args      Args
		wantGlob  string
		wantScope string
	}{
		{"anchored existing dir → scope + rest", Args{Query: "foo", Glob: sub + "/*.go"}, "*.go", sub},
		{"nonexistent prefix → unchanged", Args{Query: "foo", Glob: missing + "/*.go"}, missing + "/*.go", ""},
		{"explicit scope composes onto join", Args{Query: "foo", Glob: "pkg/*.go", Scope: parent}, "*.go", sub},
		{"explicit scope nonexistent join → unchanged", Args{Query: "foo", Glob: "nope/*.go", Scope: parent}, "nope/*.go", parent},
		{"literal file glob → parent scope + basename", Args{Query: "foo", Glob: file}, "file.go", sub},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, params, err := tilth.MCPTool(OpGrep, tt.args)
			if err != nil {
				t.Fatalf("MCPTool() err: %v", err)
			}
			if tool != "tilth_search" {
				t.Fatalf("tool = %q, want tilth_search", tool)
			}
			gotGlob, _ := params["glob"].(string)
			gotScope, _ := params["scope"].(string)
			if gotGlob != tt.wantGlob || gotScope != tt.wantScope {
				t.Errorf("params glob=%q scope=%q, want glob=%q scope=%q", gotGlob, gotScope, tt.wantGlob, tt.wantScope)
			}
		})
	}
}

// anchorFixture returns an existing directory, a sibling path that does not
// exist, and a regular file inside the existing directory — all absolute so
// SplitGlobAnchor's prefix survives os.Stat.
func anchorFixture(t *testing.T) (existing, missing, file string) {
	t.Helper()
	tmp := t.TempDir()
	existing = filepath.Join(tmp, "pkg")
	if err := os.MkdirAll(existing, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file = filepath.Join(existing, "file.go")
	if err := os.WriteFile(file, []byte("package pkg\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	return existing, filepath.Join(tmp, "nope"), file
}
