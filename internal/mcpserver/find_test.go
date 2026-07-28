package mcpserver

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/find"
)

func TestFindArgsBudget(t *testing.T) {
	if got := findArgs(FindIn{Globs: []string{"*.go"}}); got.Budget != find.DefaultBudget {
		t.Errorf("unset budget = %d, want default %d", got.Budget, find.DefaultBudget)
	}
	if got := findArgs(FindIn{Globs: []string{"*.go"}, Budget: 42}); got.Budget != 42 {
		t.Errorf("explicit budget = %d, want 42 (passthrough)", got.Budget)
	}
	if got := findArgs(FindIn{Globs: []string{"*.go", "!*_test.go"}, Budget: 7}); !slices.Equal(got.Globs, []string{"*.go", "!*_test.go"}) {
		t.Errorf("ordered globs passthrough = %+v", got)
	}
	if got := findArgs(FindIn{}); got.Globs != nil {
		t.Errorf("absent globs = %q, want no globs", got.Globs)
	}
}

// TestFindToolAppliesDefaultBudget drives ccx_repo_find through the registered MCP
// handler: with no budget the default caps the listing (overflow footer), while an
// explicit large budget passes through and shows every row.
func TestFindToolAppliesDefaultBudget(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 300; i++ {
		name := "f" + strconv.Itoa(1000+i) + ".txt"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("x", 400)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	cs := connectTestServer(t)

	def, isErr := callText(t, cs, "ccx_repo_find", map[string]any{"globs": []any{"*.txt"}})
	if isErr {
		t.Fatalf("ccx_repo_find default is error: %s", def)
	}
	if !strings.Contains(def, "more files") {
		t.Errorf("default budget should overflow with 300 files:\n%s", def)
	}

	big, isErr := callText(t, cs, "ccx_repo_find", map[string]any{"globs": []any{"*.txt"}, "budget": 100000})
	if isErr {
		t.Fatalf("ccx_repo_find budgeted is error: %s", big)
	}
	if strings.Contains(big, "more files") {
		t.Errorf("large budget should show all rows:\n%s", big)
	}
}
