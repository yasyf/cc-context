package astgrep

import (
	"reflect"
	"slices"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

func TestArgvFor(t *testing.T) {
	tests := []struct {
		name string
		op   backend.Op
		args backend.Args
		argv []string
	}{
		{
			"structural default path",
			backend.OpStructural,
			backend.Args{Query: "return $A, nil"},
			[]string{"run", "-p", "return $A, nil", "--json=stream", "."},
		},
		{
			"structural lang glob paths",
			backend.OpStructural,
			backend.Args{Query: "$A.Foo($$$)", Lang: "go", Glob: "!*_test.go", Paths: []string{"internal", "cmd"}},
			[]string{"run", "-p", "$A.Foo($$$)", "--json=stream", "-l", "go", "--globs", "!*_test.go", "internal", "cmd"},
		},
		{
			"replace preview (no -U)",
			backend.OpReplace,
			backend.Args{Pattern: "Add($A,$B)", Rewrite: "Sum($A,$B)"},
			[]string{"run", "-p", "Add($A,$B)", "-r", "Sum($A,$B)", "--json=stream", "."},
		},
		{
			// Apply omits --json=stream: with it present, -U prints JSON and
			// writes nothing, so the rewrite would silently no-op.
			"replace apply (-U, no --json=stream)",
			backend.OpReplace,
			backend.Args{Pattern: "Add($A,$B)", Rewrite: "Sum($A,$B)", Apply: true},
			[]string{"run", "-p", "Add($A,$B)", "-r", "Sum($A,$B)", "-U", "."},
		},
		{
			"replace apply with lang glob paths",
			backend.OpReplace,
			backend.Args{Pattern: "Add($A)", Rewrite: "Inc($A)", Apply: true, Lang: "go", Glob: "*.go", Paths: []string{"pkg"}},
			[]string{"run", "-p", "Add($A)", "-r", "Inc($A)", "-U", "-l", "go", "--globs", "*.go", "pkg"},
		},
		{
			"struct-outline default path",
			backend.OpStructOutline,
			backend.Args{Path: "internal/backend"},
			[]string{"outline", "internal/backend", "--json=stream", "--view", "expanded"},
		},
		{
			"struct-outline items match lang",
			backend.OpStructOutline,
			backend.Args{Path: "src", Items: "exports", Match: "^New", Lang: "go"},
			[]string{"outline", "src", "--json=stream", "--view", "expanded", "--items", "exports", "--match", "^New", "-l", "go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argv, err := argvFor(tt.op, tt.args)
			if err != nil {
				t.Fatalf("argvFor: %v", err)
			}
			if !reflect.DeepEqual(argv, tt.argv) {
				t.Errorf("argv = %v, want %v", argv, tt.argv)
			}
			if slices.Contains(argv, "-i") || slices.Contains(argv, "--interactive") {
				t.Errorf("argv must never carry interactive mode: %v", argv)
			}
		})
	}
}

func TestArgvForUnsupported(t *testing.T) {
	if _, err := argvFor(backend.OpSearch, backend.Args{}); err == nil {
		t.Fatal("expected error for unsupported op")
	}
}
