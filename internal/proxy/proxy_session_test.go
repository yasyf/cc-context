package proxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/internal/backend"
)

// TestMain doubles as the fake semble MCP engine: re-executed via the fakeSemble
// symlink as "uvx --from semble[mcp]>=0.5 semble", it serves a stdio MCP server
// whose search/find_related tools return canned semble JSON instead of indexing.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--from" {
		if err := serveFakeSemble(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeSearchIn mirrors semble's search param surface so the go-sdk validates real
// calls: MCPTool passes query/repo/top_k/max_snippet_lines.
type fakeSearchIn struct {
	Query           string `json:"query"`
	Repo            string `json:"repo,omitempty"`
	TopK            int    `json:"top_k,omitempty"`
	MaxSnippetLines int    `json:"max_snippet_lines,omitempty"`
}

// fakeRelatedIn mirrors semble's find_related param surface (file_path/line/repo).
type fakeRelatedIn struct {
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
	Repo     string `json:"repo,omitempty"`
}

// sembleJSON is the raw search/related response shape render.SembleResults parses.
const sembleSearchJSON = `{"results":[{"file_path":"a.go","start_line":1,"end_line":3,"score":0.42,"content":"canned search snippet"}]}`

const sembleRelatedJSON = `{"results":[{"file_path":"b.go","start_line":5,"end_line":7,"score":0.31,"content":"canned related snippet"}]}`

func serveFakeSemble() error {
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-semble", Version: "test"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "search", Description: "canned semble search JSON"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ fakeSearchIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: sembleSearchJSON}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "find_related", Description: "canned semble find_related JSON"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ fakeRelatedIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: sembleRelatedJSON}}}, nil, nil
		})
	return s.Run(context.Background(), &mcp.StdioTransport{})
}

// fakeSembleOnPath symlinks the test binary onto PATH as "uvx", so the proxy's
// semble MCPSpec launch (uvx --from semble[mcp]>=0.5 semble) spawns the TestMain
// fake instead of a real uvx.
func fakeSembleOnPath(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("re-exec fake semble is POSIX-only")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(dir, "uvx")); err != nil {
		t.Fatalf("symlink fake uvx: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestSembleSessionLane drives the semantic ops through the real proxy session
// lane — MCPSpec launch, resident session, CallTool, and Finalize's semble
// reshape — against the fake semble engine, the one lane native dispatch skips.
func TestSembleSessionLane(t *testing.T) {
	fakeSembleOnPath(t)
	p := New()
	t.Cleanup(func() { _ = p.Close() })

	got, err := p.Call(context.Background(), backend.OpSearch, backend.Args{Query: "auth flow"})
	if err != nil {
		t.Fatalf("Call(OpSearch): %v", err)
	}
	if !strings.Contains(got, "# 1 results") || !strings.Contains(got, "canned search snippet") {
		t.Errorf("search output = %q, want the canned semble result", got)
	}
	if p.session == nil {
		t.Fatal("proxy did not retain the semble session")
	}

	rel, err := p.Call(context.Background(), backend.OpRelated, backend.Args{Query: "b.go:5"})
	if err != nil {
		t.Fatalf("Call(OpRelated): %v", err)
	}
	if !strings.Contains(rel, "canned related snippet") {
		t.Errorf("related output = %q, want the canned related result", rel)
	}
}
