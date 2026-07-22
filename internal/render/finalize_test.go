package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

var update = flag.Bool("update", false, "regenerate golden fixtures under testdata/")

// readFixture reads a testdata fixture by name.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// checkGolden compares got against the named golden file, or rewrites it under -update.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("update golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if got != string(want) {
		t.Errorf("golden %s mismatch\n got = %q\nwant = %q", name, got, string(want))
	}
}

func TestFinalizeDefaultOpPassesThrough(t *testing.T) {
	// A non-anchoring op just caps; the payload is byte-identical below budget.
	in := "line one\nline two\n"
	got, err := Finalize(backend.OpFind, in, backend.Args{})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got != in {
		t.Errorf("Finalize(OpFind) = %q, want %q", got, in)
	}
}

func TestFinalizeSembleScoreLabels(t *testing.T) {
	in := readFixture(t, "semble_input.json")
	for _, tt := range []struct {
		op   backend.Op
		want string
	}{
		{backend.OpSearch, "(score=0.48)"},
		{backend.OpRelated, "(cos=0.48)"},
	} {
		t.Run(string(tt.op), func(t *testing.T) {
			got, err := Finalize(tt.op, in, backend.Args{})
			if err != nil {
				t.Fatalf("Finalize(%s): %v", tt.op, err)
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("Finalize(%s) = %q, want score label %q", tt.op, got, tt.want)
			}
		})
	}
}
