package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSymbolCommandExplicitBudgetCapsFullCard(t *testing.T) {
	for _, name := range []string{"ast-grep", "rg"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not on PATH", name)
		}
	}

	dir := t.TempDir()
	var body strings.Builder
	body.WriteString("package fixture\n\nfunc Flood() int {\n\ttotal := 0\n")
	for range 500 {
		body.WriteString("\ttotal++\n")
	}
	body.WriteString("\treturn total\n}\n")
	if err := os.WriteFile(filepath.Join(dir, "flood.go"), []byte(body.String()), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	got := runCCX(t, "code", "symbol", "Flood", "--full", "--budget", "100")
	if !strings.Contains(got, "tokens omitted — re-run with a larger --budget") {
		t.Errorf("budgeted symbol missing overflow footer:\n%s", got)
	}
}
