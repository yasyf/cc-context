package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/workspace"
)

func TestReadCommandNotFound(t *testing.T) {
	cmd := newReadCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{filepath.Join(t.TempDir(), "missing", "zzz.txt"), "--full"})

	err := cmd.Execute()
	if !errors.Is(err, backend.ErrPathNotFound) {
		t.Fatalf("Execute() error = %v, want ErrPathNotFound", err)
	}
	if got := ExitCode(err); got != 3 {
		t.Errorf("ExitCode = %d, want 3", got)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

// TestReadCommandNamesNoRoot proves the "# root" line is the MCP surface's
// alone: the CLI already answers in the shell whose cwd names the root, and a
// root line there would be noise the goldens must carry.
func TestReadCommandNamesNoRoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("alpha\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	workspace.SetRoot(dir)
	t.Cleanup(func() { workspace.SetRoot("") })

	cmd := newReadCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{file, "--full"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.Contains(out.String(), "# root ") {
		t.Errorf("cli read stdout names a root: %q", out.String())
	}
}
