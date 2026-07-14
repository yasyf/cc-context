package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
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
