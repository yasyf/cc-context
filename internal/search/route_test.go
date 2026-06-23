package search

import (
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/querykind"
)

func TestRoute(t *testing.T) {
	tests := []struct {
		name     string
		args     backend.Args
		wantOp   backend.Op
		wantKind querykind.Kind
		wantErr  bool
	}{
		{"auto metavar → structural", backend.Args{Query: "$A.Foo($$$)"}, backend.OpStructural, querykind.KindStructural, false},
		{"auto prose → semantic", backend.Args{Query: "how does auth work"}, backend.OpSearch, querykind.KindSemantic, false},
		{"auto bare $$$ → semantic", backend.Args{Query: "$$$"}, backend.OpSearch, querykind.KindSemantic, false},
		{"mode literal forces grep on metavar query", backend.Args{Query: "$A", Mode: "literal"}, backend.OpGrep, querykind.KindLiteral, false},
		{"mode semantic forces semble on metavar query", backend.Args{Query: "$A", Mode: "semantic"}, backend.OpSearch, querykind.KindSemantic, false},
		{"mode structural forces ast-grep on prose", backend.Args{Query: "plain words", Mode: "structural"}, backend.OpStructural, querykind.KindStructural, false},
		{"invalid mode errors", backend.Args{Query: "x", Mode: "bogus"}, "", querykind.KindAuto, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op, kind, err := Route(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Route err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if op != tt.wantOp {
				t.Errorf("Route op = %q, want %q", op, tt.wantOp)
			}
			if kind != tt.wantKind {
				t.Errorf("Route kind = %v, want %v", kind, tt.wantKind)
			}
		})
	}
}
