package proxy

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

func TestCallMissingReadSkipsBeforeDispatch(t *testing.T) {
	p := New()
	missing := filepath.Join(t.TempDir(), "missing", "f.txt")
	got, err := p.Call(context.Background(), backend.OpRead, backend.Args{Path: missing, Full: true})
	if !errors.Is(err, backend.ErrPathNotFound) {
		t.Fatalf("Call() error = %v, want ErrPathNotFound", err)
	}
	if got != "" {
		t.Errorf("Call() = %q, want empty", got)
	}
}
