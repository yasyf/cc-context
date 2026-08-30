package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/search"
	"github.com/yasyf/cc-context/internal/workspace"
)

// pinRoot declares dir as the client's project root for one test, clearing the
// process-global pin afterwards.
func pinRoot(t *testing.T, dir string) {
	t.Helper()
	workspace.SetRoot(dir)
	t.Cleanup(func() { workspace.SetRoot("") })
}

// writeTree writes one file under a fresh directory and returns both paths.
func writeTree(t *testing.T, name, content string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir, path
}

func requireGrepEngine(t *testing.T) {
	t.Helper()
	_, rgErr := exec.LookPath("rg")
	_, grepErr := exec.LookPath("grep")
	if rgErr != nil && grepErr != nil {
		t.Skip("neither rg nor grep on PATH")
	}
}

func TestRepoSchemaSurface(t *testing.T) {
	cs := connectTestServer(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]string{
		"ccx_code_read":    `"repo":{"description":"repo root a relative path resolves against; default project root","type":"string"}`,
		"ccx_code_outline": `"repo":{"description":"repo root a relative path resolves against; default project root","type":"string"}`,
		"ccx_code_grep":    `"repo":{"description":"repo root to search, and the root relative paths resolve against; default project root","type":"string"}`,
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
			t.Errorf("%s schema missing %s:\n%s", tool.Name, description, raw)
		}
		delete(want, tool.Name)
	}
	for name := range want {
		t.Errorf("tool %q not registered", name)
	}
}

func TestReadToolRepoBeatsPinnedRoot(t *testing.T) {
	named, _ := writeTree(t, "f.txt", "named\n")
	pinned, _ := writeTree(t, "f.txt", "pinned\n")
	cwd, _ := writeTree(t, "f.txt", "cwd\n")
	t.Chdir(cwd)
	pinRoot(t, pinned)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_read", map[string]any{"path": "f.txt", "repo": named})
	if isErr {
		t.Fatalf("ccx_code_read repo is error: %s", out)
	}
	if !strings.Contains(out, "named\n") {
		t.Errorf("explicit repo should beat the pinned root:\n%s", out)
	}
}

func TestReadToolPinnedRootBeatsCwd(t *testing.T) {
	pinned, _ := writeTree(t, "f.txt", "pinned\n")
	cwd, _ := writeTree(t, "f.txt", "cwd\n")
	t.Chdir(cwd)
	pinRoot(t, pinned)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_read", map[string]any{"path": "f.txt"})
	if isErr {
		t.Fatalf("ccx_code_read pinned is error: %s", out)
	}
	if !strings.Contains(out, "pinned\n") {
		t.Errorf("pinned root should beat the working directory:\n%s", out)
	}
}

func TestReadToolWithoutRootReadsCwd(t *testing.T) {
	cwd, _ := writeTree(t, "f.txt", "cwd\n")
	t.Chdir(cwd)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_read", map[string]any{"path": "f.txt"})
	if isErr {
		t.Fatalf("ccx_code_read cwd is error: %s", out)
	}
	if !strings.Contains(out, "cwd\n") {
		t.Errorf("no root declared should read the working directory:\n%s", out)
	}
	if !strings.HasPrefix(out, "# read f.txt:") {
		t.Errorf("an unrooted path should reach the op verbatim:\n%s", out)
	}
}

func TestReadToolRepoLeavesAbsolutePathAlone(t *testing.T) {
	named, _ := writeTree(t, "f.txt", "named\n")
	cwd, path := writeTree(t, "f.txt", "cwd\n")
	t.Chdir(cwd)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_read", map[string]any{"path": path, "repo": named})
	if isErr {
		t.Fatalf("ccx_code_read absolute is error: %s", out)
	}
	if !strings.Contains(out, "cwd\n") {
		t.Errorf("an absolute path names its own file:\n%s", out)
	}
}

func TestOutlineToolRepoRootsRelativePath(t *testing.T) {
	named, _ := writeTree(t, "notes.md", "# Named heading\n")
	cwd, _ := writeTree(t, "notes.md", "# Cwd heading\n")
	t.Chdir(cwd)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_outline", map[string]any{"path": "notes.md", "repo": named})
	if isErr {
		t.Fatalf("ccx_code_outline repo is error: %s", out)
	}
	if !strings.Contains(out, "# Named heading") {
		t.Errorf("outline should read the named repo's file:\n%s", out)
	}
}

func TestOutlineToolPinnedRootBeatsCwd(t *testing.T) {
	pinned, _ := writeTree(t, "notes.md", "# Pinned heading\n")
	cwd, _ := writeTree(t, "notes.md", "# Cwd heading\n")
	t.Chdir(cwd)
	pinRoot(t, pinned)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_outline", map[string]any{"path": "notes.md"})
	if isErr {
		t.Fatalf("ccx_code_outline pinned is error: %s", out)
	}
	if !strings.Contains(out, "# Pinned heading") {
		t.Errorf("outline should read the pinned root's file:\n%s", out)
	}
}

func TestGrepToolRepoSearchesNamedRoot(t *testing.T) {
	requireGrepEngine(t)
	named, hit := writeTree(t, "named.go", "var needle = 1\n")
	pinned, _ := writeTree(t, "pinned.go", "var needle = 2\n")
	cwd, _ := writeTree(t, "cwd.go", "var needle = 3\n")
	t.Chdir(cwd)
	pinRoot(t, pinned)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_grep", map[string]any{"text": "needle", "repo": named})
	if isErr {
		t.Fatalf("ccx_code_grep repo is error: %s", out)
	}
	if !strings.Contains(out, "### "+hit+":") {
		t.Errorf("grep should search the named repo:\n%s", out)
	}
	if strings.Contains(out, "pinned.go") || strings.Contains(out, "cwd.go") {
		t.Errorf("grep leaked outside the named repo:\n%s", out)
	}
}

func TestGrepToolPinnedRootBeatsCwd(t *testing.T) {
	requireGrepEngine(t)
	pinned, hit := writeTree(t, "pinned.go", "var needle = 1\n")
	cwd, _ := writeTree(t, "cwd.go", "var needle = 2\n")
	t.Chdir(cwd)
	pinRoot(t, pinned)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_grep", map[string]any{"text": "needle"})
	if isErr {
		t.Fatalf("ccx_code_grep pinned is error: %s", out)
	}
	if !strings.Contains(out, "### "+hit+":") {
		t.Errorf("grep should search the pinned root:\n%s", out)
	}
	if strings.Contains(out, "cwd.go") {
		t.Errorf("grep leaked into the working directory:\n%s", out)
	}
}

func TestSearchToolLiteralSearchesNamedRepo(t *testing.T) {
	requireGrepEngine(t)
	named, hit := writeTree(t, "named.go", "var needle = 1\n")
	cwd, _ := writeTree(t, "cwd.go", "var needle = 2\n")
	t.Chdir(cwd)

	args := backend.Args{Query: "needle", Mode: "literal"}
	op, _, err := search.Route(args)
	if err != nil {
		t.Fatalf("search.Route: %v", err)
	}
	if op != backend.OpGrep {
		t.Fatalf("literal mode routes to %q, not %q — this test no longer covers the grep branch", op, backend.OpGrep)
	}

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_search", map[string]any{"query": args.Query, "mode": args.Mode, "repo": named})
	if isErr {
		t.Fatalf("ccx_code_search literal is error: %s", out)
	}
	if !strings.Contains(out, "### "+hit+":") {
		t.Errorf("a literal search should search the named repo:\n%s", out)
	}
	if strings.Contains(out, "cwd.go") {
		t.Errorf("literal search leaked into the working directory:\n%s", out)
	}
}

func TestGrepToolRepoRootsExplicitPaths(t *testing.T) {
	requireGrepEngine(t)
	named, hit := writeTree(t, "named.go", "var needle = 1\n")
	cwd, _ := writeTree(t, "named.go", "var needle = 2\n")
	t.Chdir(cwd)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_grep", map[string]any{"text": "needle", "repo": named, "paths": []any{"named.go"}})
	if isErr {
		t.Fatalf("ccx_code_grep repo paths is error: %s", out)
	}
	if !strings.Contains(out, "### "+hit+":") {
		t.Errorf("an operand should resolve against the named repo:\n%s", out)
	}
}
