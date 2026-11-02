package ripgrep

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/render"
)

func TestMissingGlobAnchorKeepsExplicitOperand(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name string
		glob string
		want string
	}{
		{"absolute file", filepath.Join(root, "missing.go"), filepath.Join(root, "missing.go")},
		{"absolute directory", filepath.Join(root, "missing", "*.go"), filepath.Join(root, "missing")},
		{"absolute recursive directory", filepath.Join(root, "missing", "**"), filepath.Join(root, "missing")},
		{"relative directory", "missing/*.go", "missing"},
		{"relative recursive directory", "missing/**", "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, eng := range []engine{engineRipgrep, engineGrep} {
				log := filepath.Join(t.TempDir(), "argv")
				bin := stubEngine(t, "printf '%s\\n' \"$@\" > \"$CCX_SCOPE_ARGS\"\nexit 1")
				ctx := render.WithEnv(t.Context(), "CCX_SCOPE_ARGS="+log)
				_, _, err := searchGroups(ctx, eng, bin, render.Dir(root), backend.Args{Query: "WithScope", Regex: true, Globs: []string{tc.glob}}, execEngine)
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				argv := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
				if len(argv) < 2 || argv[len(argv)-2] != "--" || argv[len(argv)-1] != tc.want {
					t.Fatalf("engine %d argv=%q, want only operand %q", eng, argv, tc.want)
				}
			}
		})
	}
}
