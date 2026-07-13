package cli

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// runFind executes the find command against a fixture scope, returning stdout.
func runFind(t *testing.T, scope string, args ...string) string {
	t.Helper()
	cmd := newFindCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"*.txt", "--scope", scope}, args...))
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("find execute: %v\n%s", err, out.String())
	}
	return out.String()
}

func TestFindCmdBudgetDefaultAndFlag(t *testing.T) {
	scope := t.TempDir()
	for i := 0; i < 300; i++ {
		name := "f" + strconv.Itoa(1000+i) + ".txt"
		if err := os.WriteFile(filepath.Join(scope, name), []byte(strings.Repeat("x", 400)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// No --budget → the default applies and withholds rows.
	if def := runFind(t, scope); !strings.Contains(def, "more files") {
		t.Errorf("default budget should overflow with 300 files:\n%s", def)
	}

	// An explicit large --budget passes through and shows everything.
	if big := runFind(t, scope, "--budget", "100000"); strings.Contains(big, "more files") {
		t.Errorf("large --budget should show all rows:\n%s", big)
	}
}

func TestFindCmdHasBudgetFlag(t *testing.T) {
	if newFindCmd().Flags().Lookup("budget") == nil {
		t.Error("find command missing --budget flag")
	}
}

func TestFindCmdExplicitZeroUnlimited(t *testing.T) {
	scope := t.TempDir()
	for i := 0; i < 300; i++ {
		name := "f" + strconv.Itoa(1000+i) + ".txt"
		if err := os.WriteFile(filepath.Join(scope, name), []byte(strings.Repeat("x", 400)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// An explicit --budget 0 means unlimited (distinct from an unset flag).
	if out := runFind(t, scope, "--budget", "0"); strings.Contains(out, "more files") {
		t.Errorf("explicit --budget 0 should show all rows:\n%s", out)
	}
}

func TestFindCmdBudgetMaxIntNoPanic(t *testing.T) {
	scope := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(scope, "f"+strconv.Itoa(i)+".txt"), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The full CLI path (find.Run then render.Cap) must not overflow on MaxInt64.
	out := runFind(t, scope, "--budget", strconv.FormatInt(math.MaxInt64, 10))
	if !strings.Contains(out, "— 3 files") {
		t.Errorf("MaxInt64 budget should list every row:\n%s", out)
	}
}
