package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/codeexec"
	"github.com/yasyf/cc-context/internal/proxy"
)

// TestMain doubles as the fake tilth MCP engine: when the test binary is
// re-executed as "tilth --mcp" (via the fakeTilthOnPath symlink), it serves a
// stdio MCP server whose tilth_read echoes its params instead of running tests.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--mcp" {
		if err := serveFakeTilth(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type fakeReadIn struct {
	Path    string `json:"path"`
	Section string `json:"section,omitempty"`
	Full    bool   `json:"full,omitempty"`
	Budget  int    `json:"budget,omitempty"`
}

func serveFakeTilth() error {
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-tilth", Version: "test"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "tilth_read", Description: "echo the read params"},
		func(_ context.Context, _ *mcp.CallToolRequest, in fakeReadIn) (*mcp.CallToolResult, any, error) {
			text := fmt.Sprintf("read %s section=%s", in.Path, in.Section)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
	return s.Run(context.Background(), &mcp.StdioTransport{})
}

// fakeTilthOnPath symlinks the test binary onto PATH as "tilth", so the proxy's
// engine resolution spawns the TestMain fake instead of a real engine.
func fakeTilthOnPath(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("re-exec fake tilth is POSIX-only")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(dir, "tilth")); err != nil {
		t.Fatalf("symlink fake tilth: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// connectTestServer registers the ccx tools on a server and returns a connected
// in-memory client session. Reflection is hard-off so no test shells out to
// `claude mcp list`.
func connectTestServer(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	t.Setenv("CCX_EXEC_MCP", "off")
	s := mcp.NewServer(&mcp.Implementation{Name: "cc-context-test", Version: "test"}, nil)
	p := proxy.New()
	eng := codeexec.NewEngine(p, codeexec.NewMemoryStore())
	register(s, p, eng)
	t.Cleanup(func() {
		_ = eng.Close()
		_ = p.Close()
	})

	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// fakeAstGrepOnPath installs an "ast-grep" that emits one JSON match per file in
// files on a preview run and exits 0 on an apply run (argv carries -U).
func fakeAstGrepOnPath(t *testing.T, files []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ast-grep script is POSIX-only")
	}
	dir := t.TempDir()
	var lines strings.Builder
	for i, f := range files {
		// lines carries the raw source line the anchor hashes, as real ast-grep does.
		fmt.Fprintf(&lines, `{"file":"%s","text":"old%d","lines":"old%d","replacement":"new%d","range":{"start":{"line":%d},"end":{"line":%d}}}`+"\n", f, i, i, i, i, i)
	}
	const outline = `{"path":"x.go","language":"Go","items":[{"symbolType":"struct","name":"X","signature":"type X struct {","isExported":true,"range":{"start":{"line":4}},"members":[{"symbolType":"field","name":"Y","signature":"Y int","range":{"start":{"line":5}}}]}]}`
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = outline ]; then\n" +
		"cat <<'EOF'\n" + outline + "\nEOF\n" +
		"exit 0\n" +
		"fi\n" +
		"for a in \"$@\"; do [ \"$a\" = \"-U\" ] && exit 0; done\n" +
		"cat <<'EOF'\n" + lines.String() + "EOF\n"
	if err := os.WriteFile(filepath.Join(dir, "ast-grep"), []byte(script), 0o700); err != nil { //nolint:gosec // fake engine must be owner-executable
		t.Fatalf("write fake ast-grep: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func callText(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

func TestRegisteredToolSurface(t *testing.T) {
	cs := connectTestServer(t)
	want := map[string]bool{
		"ccx_code_search": false, "ccx_code_replace": false, "ccx_code_related": false,
		"ccx_code_outline": false, "ccx_code_read": false, "ccx_code_symbol": false,
		"ccx_code_deps": false, "ccx_code_grep": false, "ccx_repo_find": false,
		"ccx_vcs_diff": false, "ccx_repo_overview": false,
		"ccx_exec": false, "ccx_exec_tools": false,
	}
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q not registered", name)
		}
	}
}

func TestReplaceToolPreviewVsApply(t *testing.T) {
	fakeAstGrepOnPath(t, []string{"a.go", "b.go"})
	cs := connectTestServer(t)

	// Omitting apply → preview (diff, no apply summary).
	out, isErr := callText(t, cs, "ccx_code_replace", map[string]any{"pattern": "old($A)", "rewrite": "new($A)"})
	if isErr {
		t.Fatalf("ccx_code_replace preview is error: %s", out)
	}
	if !strings.HasPrefix(out, "# 2 matches across 2 files") {
		t.Errorf("preview wrong:\n%s", out)
	}

	// apply:true → apply summary.
	out, isErr = callText(t, cs, "ccx_code_replace", map[string]any{"pattern": "old($A)", "rewrite": "new($A)", "apply": true})
	if isErr {
		t.Fatalf("ccx_code_replace apply is error: %s", out)
	}
	if out != "# applied 2 rewrites across 2 files\n" {
		t.Errorf("apply summary wrong: %q", out)
	}
}

func TestReplaceToolForceOverCap(t *testing.T) {
	files := make([]string, 21)
	for i := range files {
		files[i] = fmt.Sprintf("f%d.go", i)
	}
	fakeAstGrepOnPath(t, files)
	cs := connectTestServer(t)

	// apply over the 20-file cap without force → tool error.
	out, isErr := callText(t, cs, "ccx_code_replace", map[string]any{"pattern": "old($A)", "rewrite": "new($A)", "apply": true})
	if !isErr {
		t.Fatalf("over-cap apply should be a tool error, got: %s", out)
	}
	if !strings.Contains(out, "exceeding the cap of 20") {
		t.Errorf("cap error text wrong: %s", out)
	}
	if !strings.Contains(out, "ccx_code_replace:") {
		t.Errorf("cap error should carry the tool-name prefix: %s", out)
	}

	// force:true → applies.
	out, isErr = callText(t, cs, "ccx_code_replace", map[string]any{"pattern": "old($A)", "rewrite": "new($A)", "apply": true, "force": true})
	if isErr {
		t.Fatalf("forced apply is error: %s", out)
	}
	if out != "# applied 21 rewrites across 21 files\n" {
		t.Errorf("forced apply summary wrong: %q", out)
	}
}

func TestSearchToolStructuralMode(t *testing.T) {
	fakeAstGrepOnPath(t, []string{"a.go", "a.go"})
	cs := connectTestServer(t)

	// A metavar query auto-routes structural; the result is the search list.
	out, isErr := callText(t, cs, "ccx_code_search", map[string]any{"query": "old($A)"})
	if isErr {
		t.Fatalf("ccx_code_search structural is error: %s", out)
	}
	// 0-based line 0 renders as the 1-based L1, anchored by Of("old0") = jtrj.
	if !strings.Contains(out, "a.go:L1#jtrj  old0") {
		t.Errorf("structural search list wrong:\n%s", out)
	}
}

func TestOutlineToolRoutesToAstGrep(t *testing.T) {
	fakeAstGrepOnPath(t, nil)
	cs := connectTestServer(t)

	// A directory always routes to ast-grep; the result is the rendered outline.
	out, isErr := callText(t, cs, "ccx_code_outline", map[string]any{"path": t.TempDir()})
	if isErr {
		t.Fatalf("ccx_code_outline is error: %s", out)
	}
	if !strings.Contains(out, "# x.go") || !strings.Contains(out, "L5  type X struct {") || !strings.Contains(out, "\n  L6  Y int") {
		t.Errorf("outline render wrong:\n%s", out)
	}
}

func TestSearchToolInvalidMode(t *testing.T) {
	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_search", map[string]any{"query": "x", "mode": "bogus"})
	if !isErr {
		t.Fatalf("invalid mode should be a tool error, got: %s", out)
	}
	if !strings.Contains(out, "bogus") {
		t.Errorf("invalid-mode error should name the bad mode: %s", out)
	}
	if !strings.Contains(out, "ccx_code_search:") {
		t.Errorf("invalid-mode error should carry the tool-name prefix: %s", out)
	}
}

// TestReadToolResolvesAnchor proves the proxy seam: an anchored section reaches
// the engine's tilth_read params already numeric, and the move note is
// prepended to the tool output.
func TestReadToolResolvesAnchor(t *testing.T) {
	fakeTilthOnPath(t)
	cs := connectTestServer(t)

	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	gamma := anchor.Of("gamma")

	out, isErr := callText(t, cs, "ccx_code_read", map[string]any{"path": file, "section": anchor.Format(2, gamma)})
	if isErr {
		t.Fatalf("ccx_code_read is error: %s", out)
	}
	want := fmt.Sprintf("# anchor %s: line 2 → 3\nread %s section=3-3", gamma, file)
	if out != want {
		t.Errorf("ccx_code_read out = %q, want %q", out, want)
	}
}

// TestReadToolRejectsMalformedAnchor proves an anchor-shaped section with a
// garbage hash errors at the facade with the expected form instead of falling
// through to the engine.
func TestReadToolRejectsMalformedAnchor(t *testing.T) {
	fakeTilthOnPath(t)
	cs := connectTestServer(t)

	out, isErr := callText(t, cs, "ccx_code_read", map[string]any{"path": "x.go", "section": "120#zz"})
	if !isErr {
		t.Fatalf("malformed anchor should be a tool error, got: %s", out)
	}
	if !strings.Contains(out, `invalid anchor "120#zz"`) || !strings.Contains(out, "120#a3fk") {
		t.Errorf("malformed-anchor error should name the anchor and the expected form: %s", out)
	}
}

func TestExecToolRoundTrip(t *testing.T) {
	if !codeexec.Supported {
		t.Skip(codeexec.UnsupportedReason)
	}
	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_exec", map[string]any{"script": "40+2"})
	if isErr {
		t.Fatalf("ccx_exec is error: %s", out)
	}
	if out != "42" {
		t.Errorf("ccx_exec out = %q, want %q", out, "42")
	}
}

func TestExecToolsListsCatalog(t *testing.T) {
	if !codeexec.Supported {
		t.Skip(codeexec.UnsupportedReason)
	}
	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_exec_tools", map[string]any{})
	if isErr {
		t.Fatalf("ccx_exec_tools is error: %s", out)
	}
	if !strings.Contains(out, "search(") {
		t.Errorf("catalog missing builtin signature:\n%s", out)
	}
	if !strings.Contains(out, "Allowed Python:") {
		t.Errorf("catalog missing subset rules:\n%s", out)
	}
}

func TestExecToolBadScript(t *testing.T) {
	if !codeexec.Supported {
		t.Skip(codeexec.UnsupportedReason)
	}
	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_exec", map[string]any{"script": "def f(:"})
	if !isErr {
		t.Fatalf("bad script should be a tool error, got: %s", out)
	}
	if !strings.Contains(out, "ccx_exec:") {
		t.Errorf("bad-script error should carry the tool-name prefix: %s", out)
	}
}

func TestBashToonRegistered(t *testing.T) {
	cs := connectTestServer(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "BashToon" {
			return
		}
	}
	t.Error("BashToon not registered")
}

func TestBashToonConvertsJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh -c is POSIX-only")
	}
	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "BashToon", map[string]any{
		"command":    []any{"sh", "-c", `printf '[{"a":1},{"a":2}]'`},
		"force_toon": true,
	})
	if isErr {
		t.Fatalf("BashToon JSON command is error: %s", out)
	}
	if want := "[2]{a}:\n  1\n  2"; out != want {
		t.Errorf("BashToon out = %q, want %q", out, want)
	}
}

func TestBashToonSurfacesStderrAndExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh -c is POSIX-only")
	}
	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "BashToon", map[string]any{
		"command":    []any{"sh", "-c", `echo boom 1>&2; printf '[{"a":1}]'; exit 5`},
		"force_toon": true,
	})
	if !isErr {
		t.Fatalf("non-zero exit should set IsError, got: %s", out)
	}
	if !strings.Contains(out, "[1]{a}:") {
		t.Errorf("converted stdout missing from output:\n%s", out)
	}
	if !strings.Contains(out, "\n[stderr]\nboom\n[exit 5]") {
		t.Errorf("stderr/exit section wrong:\n%s", out)
	}
}

func TestBashToonStderrOnSuccessIsNotAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh -c is POSIX-only")
	}
	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "BashToon", map[string]any{
		"command":    []any{"sh", "-c", `echo warn 1>&2; printf '[{"a":1}]'`},
		"force_toon": true,
	})
	if isErr {
		t.Fatalf("stderr on a zero exit must not flag IsError, got: %s", out)
	}
	if !strings.Contains(out, "[1]{a}:") {
		t.Errorf("converted stdout missing from output:\n%s", out)
	}
	if !strings.Contains(out, "\n[stderr]\nwarn") {
		t.Errorf("informational stderr section missing:\n%s", out)
	}
	if strings.Contains(out, "[exit") {
		t.Errorf("no [exit] line should appear on a successful run:\n%s", out)
	}
}
