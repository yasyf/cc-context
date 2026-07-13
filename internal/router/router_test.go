package router_test

import (
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/router"
)

func TestForFindPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("For(OpFind) should panic: file finding has no backend")
		}
	}()
	router.For(backend.OpFind)
}

func TestForRoutes(t *testing.T) {
	tests := []struct {
		op   backend.Op
		want backend.Engine
	}{
		{backend.OpSearch, backend.Semble{}.Engine()},
		{backend.OpRelated, backend.Semble{}.Engine()},
		{backend.OpStructural, backend.AstGrep{}.Engine()},
		{backend.OpReplace, backend.AstGrep{}.Engine()},
		{backend.OpStructOutline, backend.AstGrep{}.Engine()},
		{backend.OpGrep, backend.Tilth{}.Engine()},
	}
	for _, tt := range tests {
		t.Run(string(tt.op), func(t *testing.T) {
			if got := router.For(tt.op).Engine(); got != tt.want {
				t.Errorf("For(%s).Engine() = %v, want %v", tt.op, got, tt.want)
			}
		})
	}
}
