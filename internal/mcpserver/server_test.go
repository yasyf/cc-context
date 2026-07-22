package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/codeexec"
	"github.com/yasyf/cc-context/internal/proxy"
)

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
		"if [ \"$1\" = \"--version\" ]; then echo \"ast-grep 0.44.0\"; exit 0; fi\n" +
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
		"ccx_code_edit":    false,
		"ccx_code_outline": false, "ccx_code_read": false, "ccx_code_symbol": false,
		"ccx_code_deps": false, "ccx_code_grep": false, "ccx_repo_find": false,
		"ccx_vcs_diff": false, "ccx_repo_overview": false,
		"ccx_web_outline": false, "ccx_web_read": false, "ccx_web_search": false,
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

// TestAlwaysLoadMetaSurface proves exactly the common workhorse tools carry the
// anthropic/alwaysLoad _meta flag (eager-loaded past tool-search deferral) and
// every other registered tool stays deferred without it — asserting over the
// full ListTools surface so a new tool wrongly marked alwaysLoad fails here.
func TestAlwaysLoadMetaSurface(t *testing.T) {
	cs := connectTestServer(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	eager := map[string]bool{
		"ccx_code_read":    true,
		"ccx_code_grep":    true,
		"ccx_code_outline": true,
		"ccx_code_search":  true,
	}
	seen := 0
	for _, tool := range res.Tools {
		want := eager[tool.Name]
		got := tool.Meta[metaAlwaysLoad] == true
		if got != want {
			t.Errorf("tool %q alwaysLoad = %v, want %v", tool.Name, got, want)
		}
		if want {
			seen++
		}
	}
	if seen != len(eager) {
		t.Errorf("eager-loaded tools present = %d, want %d (renamed or unregistered)", seen, len(eager))
	}
}

// TestGrepToolSchemaHasEngineFields proves the ripgrep-engine flags and the
// scope filter are advertised on the ccx_code_grep input schema.
func TestGrepToolSchemaHasEngineFields(t *testing.T) {
	cs := connectTestServer(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var schema string
	for _, tool := range res.Tools {
		if tool.Name == "ccx_code_grep" {
			raw, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatalf("marshal input schema: %v", err)
			}
			schema = string(raw)
		}
	}
	if schema == "" {
		t.Fatal("ccx_code_grep not registered")
	}
	for _, field := range []string{`"scope"`, `"ignoreCase"`, `"word"`, `"regex"`, `"paths"`} {
		if !strings.Contains(schema, field) {
			t.Errorf("ccx_code_grep schema missing %s:\n%s", field, schema)
		}
	}
}

func TestScopeSchemaDescriptions(t *testing.T) {
	cs := connectTestServer(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]string{
		"ccx_code_symbol":  `"description":"directory (directories only) to scope the lookup to"`,
		"ccx_code_deps":    `"description":"directory (directories only) to scope the analysis to"`,
		"ccx_code_grep":    `"description":"directory (or a single file) to scope to"`,
		"ccx_code_related": `"description":"repo root or https git URL; default project root"`,
		"ccx_repo_find":    `"description":"directory (directories only) to scope the search to"`,
	}
	for _, tool := range res.Tools {
		description, ok := want[tool.Name]
		if !ok {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s input schema: %v", tool.Name, err)
		}
		if !strings.Contains(string(raw), description) {
			t.Errorf("%s scope schema missing %s:\n%s", tool.Name, description, raw)
		}
		delete(want, tool.Name)
	}
	for name := range want {
		t.Errorf("tool %q not registered", name)
	}
}

func TestServerInstructionsNameInstalledTools(t *testing.T) {
	want := "Tool names may appear under a client-assigned mcp__…__ prefix; call tools exactly as listed in your client's tool inventory."
	if !strings.Contains(serverInstructions, want) {
		t.Errorf("server instructions missing %q", want)
	}
}

func TestSemanticToolDescriptionsStateContentScope(t *testing.T) {
	cs := connectTestServer(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	// Per-request content narrowing is now supported over MCP (not CLI-only), so
	// both semantic tools advertise the content knob.
	want := "pass content to narrow (code|docs|config|all)"
	remaining := map[string]bool{"ccx_code_search": true, "ccx_code_related": true}
	for _, tool := range res.Tools {
		if !remaining[tool.Name] {
			continue
		}
		if !strings.Contains(tool.Description, want) {
			t.Errorf("%s description missing %q: %q", tool.Name, want, tool.Description)
		}
		delete(remaining, tool.Name)
	}
	for name := range remaining {
		t.Errorf("tool %q not registered", name)
	}
}

// TestGrepToolIgnoreCaseRoutesToEngine proves an MCP ccx_code_grep call with
// ignoreCase routes through the in-process ripgrep engine (rg or system grep):
// no MCP session is opened, yet a case-insensitive query returns anchored
// house-format frames for the uppercase match.
func TestGrepToolIgnoreCaseRoutesToEngine(t *testing.T) {
	_, rgErr := exec.LookPath("rg")
	_, grepErr := exec.LookPath("grep")
	if rgErr != nil && grepErr != nil {
		t.Skip("neither rg nor grep on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("var OpGrep = 1\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_grep", map[string]any{"text": "opgrep", "ignoreCase": true})
	if isErr {
		t.Fatalf("ccx_code_grep ignoreCase is error: %s", out)
	}
	if !strings.Contains(out, "### sample.go:") {
		t.Errorf("expected house-format section header for the engine match:\n%s", out)
	}
	if !strings.Contains(out, "OpGrep") {
		t.Errorf("expected the case-variant match text:\n%s", out)
	}
}

// TestGrepToolRegexRoutesToEngine proves an MCP ccx_code_grep call with regex
// routes through the in-process ripgrep engine: an anchored "^func " matches the
// line starting with func, which a literal search could never find.
func TestGrepToolRegexRoutesToEngine(t *testing.T) {
	_, rgErr := exec.LookPath("rg")
	_, grepErr := exec.LookPath("grep")
	if rgErr != nil && grepErr != nil {
		t.Skip("neither rg nor grep on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("// mentions func\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_grep", map[string]any{"text": "^func ", "regex": true})
	if isErr {
		t.Fatalf("ccx_code_grep regex is error: %s", out)
	}
	if !strings.Contains(out, "### sample.go:2") {
		t.Errorf("expected the anchored func line from the regex engine match:\n%s", out)
	}
}

func TestGrepToolAutoRegex(t *testing.T) {
	_, rgErr := exec.LookPath("rg")
	_, grepErr := exec.LookPath("grep")
	if rgErr != nil && grepErr != nil {
		t.Skip("neither rg nor grep on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("// mentions func\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_grep", map[string]any{"text": "^func "})
	if isErr {
		t.Fatalf("ccx_code_grep auto-regex is error: %s", out)
	}
	if !strings.Contains(out, `# grep: "^func " — 1 matches in 1 files (auto-regex)`) ||
		!strings.Contains(out, "### sample.go:2") {
		t.Errorf("expected the annotated anchored auto-regex match:\n%s", out)
	}
}

// TestGrepToolPathsRouteToEngine proves an MCP ccx_code_grep call with explicit
// paths routes through the engine and returns hits only from the named file.
func TestGrepToolPathsRouteToEngine(t *testing.T) {
	_, rgErr := exec.LookPath("rg")
	_, grepErr := exec.LookPath("grep")
	if rgErr != nil && grepErr != nil {
		t.Skip("neither rg nor grep on PATH")
	}
	dir := t.TempDir()
	for _, f := range []string{"named.go", "other.go"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("var needle = 1\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	t.Chdir(dir)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_grep", map[string]any{"text": "needle", "paths": []any{"named.go"}})
	if isErr {
		t.Fatalf("ccx_code_grep paths is error: %s", out)
	}
	if !strings.Contains(out, "### named.go:") {
		t.Errorf("expected the named file's match:\n%s", out)
	}
	if strings.Contains(out, "other.go") {
		t.Errorf("unnamed file leaked into results:\n%s", out)
	}
}

func TestGrepToolPathResolvesSibling(t *testing.T) {
	_, rgErr := exec.LookPath("rg")
	_, grepErr := exec.LookPath("grep")
	if rgErr != nil && grepErr != nil {
		t.Skip("neither rg nor grep on PATH")
	}
	t.Chdir(t.TempDir())
	if err := os.WriteFile("events.py", []byte("def old():\n    pass\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_grep", map[string]any{"text": "def old", "paths": []any{"events"}})
	if isErr {
		t.Fatalf("ccx_code_grep sibling is error: %s", out)
	}
	wantPrefix := "# note: events → events.py\n# grep: \"def old\""
	if !strings.HasPrefix(out, wantPrefix) || !strings.Contains(out, "### events.py:1") {
		t.Errorf("ccx_code_grep sibling output = %q, want prefix %q and resolved path hit", out, wantPrefix)
	}
}

// TestGrepToolDefaultRoutesToEngine proves a bare-literal ccx_code_grep (no
// engine flags) now runs the native ripgrep engine: the needle planted in the cwd
// surfaces as an anchored house-format section, with no MCP engine on PATH.
func TestGrepToolDefaultRoutesToEngine(t *testing.T) {
	_, rgErr := exec.LookPath("rg")
	_, grepErr := exec.LookPath("grep")
	if rgErr != nil && grepErr != nil {
		t.Skip("neither rg nor grep on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("var needle = 1\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_grep", map[string]any{"text": "needle"})
	if isErr {
		t.Fatalf("ccx_code_grep is error: %s", out)
	}
	if !strings.Contains(out, "### sample.go:") {
		t.Errorf("bare literal grep should route to the native engine:\n%s", out)
	}
	if strings.Contains(out, "0 matches") {
		t.Errorf("planted needle reported as no match:\n%s", out)
	}
}

// requireEngines skips when the native symbol/deps engines (ast-grep + rg) are not
// on PATH.
func requireEngines(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ast-grep"); err != nil {
		t.Skip("ast-grep not on PATH")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
}

// TestSymbolToolNative proves ccx_code_symbol runs the native symbol resolver
// through the proxy seam — no MCP engine on PATH — resolving a real fixture to
// the anchored locate card.
func TestSymbolToolNative(t *testing.T) {
	requireEngines(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "widget.go"),
		[]byte("package fix\n\n// Greet builds a greeting.\nfunc Greet(name string) string {\n\treturn name\n}\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_symbol", map[string]any{"name": "Greet"})
	if isErr {
		t.Fatalf("ccx_code_symbol is error: %s", out)
	}
	if !strings.HasPrefix(out, "# symbol Greet — function — widget.go:") {
		t.Errorf("native symbol card header wrong:\n%s", out)
	}
	if !strings.Contains(out, "Greet builds a greeting.") {
		t.Errorf("native symbol card missing doc:\n%s", out)
	}
}

// TestDepsToolNative proves ccx_code_deps runs the native deps analyzer through the
// proxy seam: a real Go fixture yields the anchored imports and used-by report.
func TestDepsToolNative(t *testing.T) {
	requireEngines(t)
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("go.mod", "module example.com/fix\n\ngo 1.23\n")
	write("lib/lib.go", "package lib\n\nimport \"strings\"\n\nfunc Trim(s string) string { return strings.TrimSpace(s) }\n")
	write("app/app.go", "package app\n\nimport \"example.com/fix/lib\"\n\nfunc Run() string { return lib.Trim(\" x \") }\n")
	t.Chdir(dir)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_deps", map[string]any{"path": "lib/lib.go"})
	if isErr {
		t.Fatalf("ccx_code_deps is error: %s", out)
	}
	for _, want := range []string{"# deps lib/lib.go —", "strings (std)", "## used by", "app/app.go:", "→ lib.Trim"} {
		if !strings.Contains(out, want) {
			t.Errorf("native deps output missing %q:\n%s", want, out)
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

	// A directory always routes to ast-grep; the terse default renders top-level
	// declarations only, hiding the struct's member behind a count and the flags.
	out, isErr := callText(t, cs, "ccx_code_outline", map[string]any{"path": t.TempDir()})
	if isErr {
		t.Fatalf("ccx_code_outline is error: %s", out)
	}
	if !strings.Contains(out, "# x.go") || !strings.Contains(out, "L5  type X struct {  (+1 member)") {
		t.Errorf("terse outline render wrong:\n%s", out)
	}
	if strings.Contains(out, "L6  Y int") {
		t.Errorf("terse outline should hide the member:\n%s", out)
	}

	// deep=true renders the member.
	deep, isErr := callText(t, cs, "ccx_code_outline", map[string]any{"path": t.TempDir(), "deep": true})
	if isErr {
		t.Fatalf("ccx_code_outline deep is error: %s", deep)
	}
	if !strings.Contains(deep, "\n  L6  Y int") {
		t.Errorf("deep outline should render the member:\n%s", deep)
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

// TestReadToolResolvesAnchor proves the proxy seam runs native read: an anchored
// section re-resolves to its content's current line, the move note is prepended,
// and the anchored native header carries the served line.
func TestReadToolResolvesAnchor(t *testing.T) {
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
	want := fmt.Sprintf("# anchor %s: line 2 → 3\n# read %s:3#%s (1 of 3 lines)\ngamma\n", gamma, file, gamma)
	if out != want {
		t.Errorf("ccx_code_read out = %q, want %q", out, want)
	}
}

func TestReadToolResolvesExtensionSibling(t *testing.T) {
	cs := connectTestServer(t)

	original := filepath.Join(t.TempDir(), "events")
	resolved := original + ".py"
	if err := os.WriteFile(resolved, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, isErr := callText(t, cs, "ccx_code_read", map[string]any{"path": original, "section": "1"})
	if isErr {
		t.Fatalf("ccx_code_read sibling is error: %s", out)
	}
	hash := anchor.Of("hello")
	want := fmt.Sprintf("# note: %s → %s\n# read %s:1#%s (1 of 1 lines)\nhello\n", original, resolved, resolved, hash)
	if out != want {
		t.Errorf("ccx_code_read sibling out = %q, want %q", out, want)
	}
}

// TestReadToolRejectsMalformedAnchor proves an anchor-shaped section with a
// garbage hash errors at the facade with the expected form instead of falling
// through to the engine.
func TestReadToolRejectsMalformedAnchor(t *testing.T) {
	cs := connectTestServer(t)
	file := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(file, []byte("package x\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, isErr := callText(t, cs, "ccx_code_read", map[string]any{"path": file, "section": "120#zz"})
	if !isErr {
		t.Fatalf("malformed anchor should be a tool error, got: %s", out)
	}
	if !strings.Contains(out, `invalid anchor "120#zz"`) || !strings.Contains(out, "120#a3fk") {
		t.Errorf("malformed-anchor error should name the anchor and the expected form: %s", out)
	}
}

// TestEditToolWritesFile proves the ccx_code_edit seam end to end: no engine is
// on PATH, so a successful write also proves proxy.call short-circuits OpEdit
// before provisioning any engine session. The anchored span is replaced in place
// and the report carries the pre/post anchors and the mini-diff.
func TestEditToolWritesFile(t *testing.T) {
	cs := connectTestServer(t)

	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	beta := anchor.Of("beta")

	out, isErr := callText(t, cs, "ccx_code_edit", map[string]any{
		"path": file, "at": anchor.Format(2, beta), "content": "BETA",
	})
	if isErr {
		t.Fatalf("ccx_code_edit is error: %s", out)
	}
	if got, _ := os.ReadFile(file); string(got) != "alpha\nBETA\ngamma\n" {
		t.Errorf("file after edit = %q", got)
	}
	want := fmt.Sprintf("%s:%s → %s:%s\n- beta\n+ BETA\n", file, anchor.Format(2, beta), file, anchor.Format(2, anchor.Of("BETA")))
	if out != want {
		t.Errorf("ccx_code_edit out = %q, want %q", out, want)
	}
}

// TestEditToolMatchWritesFile proves match mode writes replacement bytes
// verbatim, including trailing spaces and the replacement's trailing newline.
func TestEditToolMatchWritesFile(t *testing.T) {
	cs := connectTestServer(t)

	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, isErr := callText(t, cs, "ccx_code_edit", map[string]any{
		"path": file, "match": "beta", "content": "BETA  \n",
	})
	if isErr {
		t.Fatalf("ccx_code_edit match is error: %s", out)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}
	want := []byte("alpha\nBETA  \n\ngamma\n")
	if !bytes.Equal(got, want) {
		t.Errorf("file after match edit = %q, want %q", got, want)
	}
}

// TestEditToolMatchAmbiguousErrors proves an unscoped duplicate match reports
// each candidate anchor and leaves the file byte-identical.
func TestEditToolMatchAmbiguousErrors(t *testing.T) {
	cs := connectTestServer(t)
	file := filepath.Join(t.TempDir(), "f.txt")
	fixture := []byte("dup\nmiddle\ndup\n")
	if err := os.WriteFile(file, fixture, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, isErr := callText(t, cs, "ccx_code_edit", map[string]any{
		"path": file, "match": "dup", "content": "unique",
	})
	if !isErr {
		t.Fatalf("ambiguous match should be a tool error, got: %s", out)
	}
	dup := anchor.Of("dup")
	wantCandidates := fmt.Sprintf("(%s, %s)", anchor.Format(1, dup), anchor.Format(3, dup))
	if !strings.Contains(out, wantCandidates) {
		t.Errorf("ambiguous-match error missing candidates %s: %s", wantCandidates, out)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read rejected edit file: %v", err)
	}
	if !bytes.Equal(got, fixture) {
		t.Errorf("file changed on ambiguous match: %q", got)
	}
}

// TestEditToolMatchValidation proves the facade rejects missing or invalid match
// controls before the local edit engine can touch the file.
func TestEditToolMatchValidation(t *testing.T) {
	cs := connectTestServer(t)
	file := filepath.Join(t.TempDir(), "f.txt")
	fixture := []byte("alpha\nbeta\ngamma\n")
	if err := os.WriteFile(file, fixture, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"neither at nor match", map[string]any{"path": file, "content": "X"}, "provide at, match, or both"},
		{"empty match", map[string]any{"path": file, "match": "", "content": "X"}, "match must be non-empty"},
		{"all without match", map[string]any{"path": file, "at": "2", "content": "X", "all": true}, "all requires match"},
		{"all without at or match errors all first", map[string]any{"path": file, "content": "X", "all": true}, "all requires match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, isErr := callText(t, cs, "ccx_code_edit", tt.args)
			if !isErr {
				t.Fatalf("expected a tool error, got: %s", out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("error text missing %q: %s", tt.want, out)
			}
			got, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read rejected edit file: %v", err)
			}
			if !bytes.Equal(got, fixture) {
				t.Errorf("file changed on rejected match edit: %q", got)
			}
		})
	}
}

// TestEditToolDeleteWritesFile proves the delete path writes and reports the line
// now at the splice point as the new anchor.
func TestEditToolDeleteWritesFile(t *testing.T) {
	cs := connectTestServer(t)

	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, isErr := callText(t, cs, "ccx_code_edit", map[string]any{
		"path": file, "at": anchor.Format(2, anchor.Of("b")), "delete": true,
	})
	if isErr {
		t.Fatalf("ccx_code_edit delete is error: %s", out)
	}
	if got, _ := os.ReadFile(file); string(got) != "a\nc\n" {
		t.Errorf("file after delete = %q", got)
	}
	want := fmt.Sprintf("%s:%s → %s:%s\n- b\n", file, anchor.Format(2, anchor.Of("b")), file, anchor.Format(2, anchor.Of("c")))
	if out != want {
		t.Errorf("ccx_code_edit delete out = %q, want %q", out, want)
	}
}

// TestEditToolRequiresExactlyOne proves the facade rejects a call that supplies
// both or neither of content and delete without touching the file.
func TestEditToolRequiresExactlyOne(t *testing.T) {
	cs := connectTestServer(t)
	file := filepath.Join(t.TempDir(), "f.txt")
	const content = "a\nb\nc\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tests := []struct {
		name string
		args map[string]any
	}{
		{"neither", map[string]any{"path": file, "at": "2"}},
		{"both", map[string]any{"path": file, "at": "2", "content": "X", "delete": true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, isErr := callText(t, cs, "ccx_code_edit", tt.args)
			if !isErr {
				t.Fatalf("expected a tool error, got: %s", out)
			}
			if !strings.Contains(out, "exactly one of content or delete") {
				t.Errorf("error text wrong: %s", out)
			}
			if !strings.Contains(out, "ccx_code_edit:") {
				t.Errorf("error should carry the tool-name prefix: %s", out)
			}
			if got, _ := os.ReadFile(file); string(got) != content {
				t.Errorf("file changed on rejected edit: %q", got)
			}
		})
	}
}

func TestExecToolRoundTrip(t *testing.T) {
	if !codeexec.Supported() {
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
	if !codeexec.Supported() {
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
	if !codeexec.Supported() {
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

func TestBashFormatRegistered(t *testing.T) {
	cs := connectTestServer(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	found := false
	for _, tool := range res.Tools {
		if tool.Name == "BashFormat" {
			found = true
		}
		if tool.Name == "BashToon" {
			t.Error("BashToon still registered; the rename is a clean break")
		}
	}
	if !found {
		t.Error("BashFormat not registered")
	}
}

func TestBashFormatConvertsJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh -c is POSIX-only")
	}
	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "BashFormat", map[string]any{
		"command": []any{"sh", "-c", `printf '[{"a":1},{"a":2}]'`},
		"format":  "toon",
	})
	if isErr {
		t.Fatalf("BashFormat JSON command is error: %s", out)
	}
	if want := "[2]{a}:\n  1\n  2"; out != want {
		t.Errorf("BashFormat out = %q, want %q", out, want)
	}
}

func TestBashFormatAutoFloorsToCompactJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh -c is POSIX-only")
	}
	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "BashFormat", map[string]any{
		"command": []any{"sh", "-c", `printf '[{"a": 1}, {"a": 2}]'`},
	})
	if isErr {
		t.Fatalf("BashFormat JSON command is error: %s", out)
	}
	if want := `[{"a":1},{"a":2}]`; out != want {
		t.Errorf("BashFormat auto out = %q, want %q", out, want)
	}
}

func TestBashFormatInvalidFormat(t *testing.T) {
	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "BashFormat", map[string]any{
		"command": []any{"true"},
		"format":  "yaml",
	})
	if !isErr {
		t.Fatalf("invalid format should be a tool error, got: %s", out)
	}
	if !strings.Contains(out, "BashFormat:") || !strings.Contains(out, "yaml") {
		t.Errorf("error should carry the tool prefix and bad name: %s", out)
	}
}

func TestBashFormatSurfacesStderrAndExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh -c is POSIX-only")
	}
	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "BashFormat", map[string]any{
		"command": []any{"sh", "-c", `echo boom 1>&2; printf '[{"a":1}]'; exit 5`},
		"format":  "toon",
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

func TestBashFormatStderrOnSuccessIsNotAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh -c is POSIX-only")
	}
	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "BashFormat", map[string]any{
		"command": []any{"sh", "-c", `echo warn 1>&2; printf '[{"a":1}]'`},
		"format":  "toon",
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
