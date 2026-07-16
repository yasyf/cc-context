package proxy

import (
	"context"
	"os"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

func TestCallBinaryOutlineSkipsBeforeDispatch(t *testing.T) {
	t.Chdir(t.TempDir())
	const path = "image.png"
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		op   backend.Op
		args backend.Args
	}{
		{"native outline", backend.OpOutline, backend.Args{Path: path}},
		{"forced go structural outline", backend.OpStructOutline, backend.Args{Path: path, Lang: "go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New()
			got, err := p.Call(context.Background(), tt.op, tt.args)
			if err != nil {
				t.Fatalf("Call() error = %v", err)
			}
			want := "image.png (binary, 16B, image/png) [skipped]"
			if got != want {
				t.Errorf("Call() = %q, want %q", got, want)
			}
			if p.session != nil {
				t.Error("Call() opened a semble session, want none")
			}
		})
	}
}
