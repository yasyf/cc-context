package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/workspace"
)

// rootLine is the "# root <abspath>\n" a rooted tool result carries for the
// process working directory, for tests that pin an exact result.
func rootLine(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return "# root " + cwd + "\n"
}

// pinRoot pins root for the duration of the test, clearing the process-global
// pin afterwards.
func pinRoot(t *testing.T, root string) {
	t.Helper()
	workspace.SetRoot(root)
	t.Cleanup(func() { workspace.SetRoot("") })
}

func TestWithRootLineJoinsHeaderBlock(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			"after the header line",
			"# read a.go:1-2#ab12 (2 of 9 lines)\npackage a\n",
			"# read a.go:1-2#ab12 (2 of 9 lines)\n# root /r\npackage a\n",
		},
		{
			"at the top when there is no header",
			"a.go:2#ns67 → a.go:2#s507\n- beta\n+ BETA\n",
			"# root /r\na.go:2#ns67 → a.go:2#s507\n- beta\n+ BETA\n",
		},
		{
			"a served markdown heading is content, not a header",
			"# read r.md:1-2#ab12 (2 of 9 lines)\n# Title\ntext\n",
			"# read r.md:1-2#ab12 (2 of 9 lines)\n# root /r\n# Title\ntext\n",
		},
		{"single unterminated line", "42", "# root /r\n42"},
		{"empty output", "", "# root /r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withRootLine(tt.text, "/r"); got != tt.want {
				t.Errorf("withRootLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRootlessCoversOnlyWebOps(t *testing.T) {
	rooted := []backend.Op{
		backend.OpSearch, backend.OpRelated, backend.OpOutline, backend.OpRead,
		backend.OpSymbol, backend.OpDeps, backend.OpGrep, backend.OpFind,
		backend.OpDiff, backend.OpOverview, backend.OpEdit, backend.OpStructural,
		backend.OpReplace, backend.OpStructOutline,
	}
	for _, op := range rooted {
		if rootless(op) {
			t.Errorf("rootless(%s) = true, want false", op)
		}
	}
	for _, op := range []backend.Op{backend.OpWebOutline, backend.OpWebRead, backend.OpWebSearch} {
		if !rootless(op) {
			t.Errorf("rootless(%s) = false, want true", op)
		}
	}
}

func TestRootHeaderResultNamesThePinnedRoot(t *testing.T) {
	pinRoot(t, "/pinned/elsewhere")

	res, _, err := rootHeaderResult("# read a.go:1#ab12 (1 of 9 lines)\npackage a\n")
	if err != nil {
		t.Fatalf("rootHeaderResult: %v", err)
	}
	want := "# read a.go:1#ab12 (1 of 9 lines)\n# root /pinned/elsewhere\npackage a\n"
	if got := res.Content[0].(*mcp.TextContent).Text; got != want {
		t.Errorf("rootHeaderResult text = %q, want %q", got, want)
	}
}

func TestReadToolNamesThePinnedRoot(t *testing.T) {
	dir := t.TempDir()
	pinRoot(t, dir)
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("alpha\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_read", map[string]any{"path": file, "full": true})
	if isErr {
		t.Fatalf("ccx_code_read is error: %s", out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 2 || lines[1] != "# root "+dir {
		t.Errorf("ccx_code_read out = %q, want %q as its second line", out, "# root "+dir)
	}
}

func TestWebToolsCarryNoRootLine(t *testing.T) {
	pinRoot(t, t.TempDir())
	isolateWeb(t)
	srv := startWebFixture(t)

	cs := connectTestServer(t)
	for _, tool := range []string{"ccx_web_outline", "ccx_web_read"} {
		out, isErr := callText(t, cs, tool, map[string]any{"url": srv.URL})
		if isErr {
			t.Fatalf("%s is error: %s", tool, out)
		}
		if strings.Contains(out, "# root ") {
			t.Errorf("%s out names a root: %q", tool, out)
		}
	}
}

func TestSearchToolNamesTheRoot(t *testing.T) {
	fakeAstGrepOnPath(t, []string{"a.go"})
	cs := connectTestServer(t)

	out, isErr := callText(t, cs, "ccx_code_search", map[string]any{"query": "old($A)"})
	if isErr {
		t.Fatalf("ccx_code_search is error: %s", out)
	}
	if !strings.Contains(out, rootLine(t)) {
		t.Errorf("ccx_code_search out = %q, want it to carry %q", out, rootLine(t))
	}
}
