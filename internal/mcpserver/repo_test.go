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

// writeNested writes one file at a relative path under a fresh directory and
// returns the root and the written file.
func writeNested(t *testing.T, rel, content string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("make fixture dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir, path
}

func TestReadToolRelativeRepoResolvesAgainstThePinnedRoot(t *testing.T) {
	pinned, _ := writeNested(t, "vendor/f.txt", "pinned vendor\n")
	cwd, _ := writeNested(t, "vendor/f.txt", "cwd vendor\n")
	t.Chdir(cwd)
	pinRoot(t, pinned)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_read", map[string]any{"path": "f.txt", "repo": "vendor"})
	if isErr {
		t.Fatalf("ccx_code_read relative repo is error: %s", out)
	}
	if !strings.Contains(out, "pinned vendor\n") {
		t.Errorf("a relative repo should resolve against the pinned root:\n%s", out)
	}
}

func TestReadToolRelativeRepoFallsBackToCwdWithoutAPin(t *testing.T) {
	cwd, _ := writeNested(t, "vendor/f.txt", "cwd vendor\n")
	t.Chdir(cwd)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_read", map[string]any{"path": "f.txt", "repo": "vendor"})
	if isErr {
		t.Fatalf("ccx_code_read relative repo is error: %s", out)
	}
	if !strings.Contains(out, "cwd vendor\n") {
		t.Errorf("with nothing pinned a relative repo should resolve against the working directory:\n%s", out)
	}
}

func TestGrepToolRelativeRepoSearchesUnderThePinnedRoot(t *testing.T) {
	requireGrepEngine(t)
	pinned, hit := writeNested(t, "vendor/pinned.go", "var needle = 1\n")
	cwd, _ := writeNested(t, "vendor/cwd.go", "var needle = 2\n")
	t.Chdir(cwd)
	pinRoot(t, pinned)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_grep", map[string]any{"text": "needle", "repo": "vendor"})
	if isErr {
		t.Fatalf("ccx_code_grep relative repo is error: %s", out)
	}
	if !strings.Contains(out, "### "+hit+":") {
		t.Errorf("a relative repo should search under the pinned root:\n%s", out)
	}
	if strings.Contains(out, "cwd.go") {
		t.Errorf("grep leaked into the working directory's vendor tree:\n%s", out)
	}
}

func TestSearchToolLiteralRelativeRepoSearchesUnderThePinnedRoot(t *testing.T) {
	requireGrepEngine(t)
	pinned, hit := writeNested(t, "vendor/pinned.go", "var needle = 1\n")
	cwd, _ := writeNested(t, "vendor/cwd.go", "var needle = 2\n")
	t.Chdir(cwd)
	pinRoot(t, pinned)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_search", map[string]any{"query": "needle", "mode": "literal", "repo": "vendor"})
	if isErr {
		t.Fatalf("ccx_code_search relative repo is error: %s", out)
	}
	if !strings.Contains(out, "### "+hit+":") {
		t.Errorf("a literal search's relative repo should search under the pinned root:\n%s", out)
	}
	if strings.Contains(out, "cwd.go") {
		t.Errorf("literal search leaked into the working directory's vendor tree:\n%s", out)
	}
}

func TestDepsToolPinnedRootRootsRelativePath(t *testing.T) {
	pinned, hit := writeTree(t, "a.go", "package a\n\nimport \"github.com/yasyf/pinnedpkg\"\n\nvar _ = pinnedpkg.X\n")
	cwd, _ := writeTree(t, "a.go", "package a\n\nimport \"github.com/yasyf/cwdpkg\"\n\nvar _ = cwdpkg.X\n")
	t.Chdir(cwd)
	pinRoot(t, pinned)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_deps", map[string]any{"path": "a.go"})
	if isErr {
		t.Fatalf("ccx_code_deps pinned is error: %s", out)
	}
	if !strings.Contains(out, hit) {
		t.Errorf("deps should analyze the pinned root's file:\n%s", out)
	}
	if !strings.Contains(out, "pinnedpkg") || strings.Contains(out, "cwdpkg") {
		t.Errorf("deps read the working directory's file instead of the pinned root's:\n%s", out)
	}
}

func TestDepsToolReachesAFileOnlyThePinnedRootHas(t *testing.T) {
	pinned, hit := writeNested(t, "sub/a.go", "package a\n\nimport \"github.com/yasyf/pinnedpkg\"\n\nvar _ = pinnedpkg.X\n")
	t.Chdir(t.TempDir())
	pinRoot(t, pinned)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_deps", map[string]any{"path": "sub/a.go"})
	if isErr {
		t.Fatalf("ccx_code_deps should resolve the path against the pinned root: %s", out)
	}
	if !strings.Contains(out, hit) || !strings.Contains(out, "pinnedpkg") {
		t.Errorf("deps should analyze the pinned root's file:\n%s", out)
	}
}

func TestDepsToolWithoutARootReadsCwd(t *testing.T) {
	cwd, _ := writeTree(t, "a.go", "package a\n\nimport \"github.com/yasyf/cwdpkg\"\n\nvar _ = cwdpkg.X\n")
	t.Chdir(cwd)

	cs := connectTestServer(t)
	out, isErr := callText(t, cs, "ccx_code_deps", map[string]any{"path": "a.go"})
	if isErr {
		t.Fatalf("ccx_code_deps cwd is error: %s", out)
	}
	if !strings.Contains(out, "cwdpkg") {
		t.Errorf("no root declared should analyze the working directory's file:\n%s", out)
	}
}
