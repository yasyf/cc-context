package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/symbol"
)

func TestSymbolArgsBudget(t *testing.T) {
	tests := []struct {
		name string
		in   SymbolIn
		want int
	}{
		{name: "default", in: SymbolIn{Name: "Run"}, want: symbol.DefaultBudget},
		{name: "explicit", in: SymbolIn{Name: "Run", Budget: 42}, want: 42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := symbolArgs(tt.in).Budget; got != tt.want {
				t.Errorf("budget = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSymbolToolSchemaHasBudget(t *testing.T) {
	cs := connectTestServer(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var schema string
	for _, tool := range res.Tools {
		if tool.Name == "ccx_code_symbol" {
			raw, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatalf("marshal input schema: %v", err)
			}
			schema = string(raw)
		}
	}
	if schema == "" {
		t.Fatal("ccx_code_symbol not registered")
	}
	if !strings.Contains(schema, `"budget"`) {
		t.Errorf("ccx_code_symbol schema missing budget:\n%s", schema)
	}
}
