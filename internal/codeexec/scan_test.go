package codeexec

import (
	"reflect"
	"testing"
)

func TestReferenced(t *testing.T) {
	funcToServer := map[string]string{
		"fake_echo":  "fake",
		"other_tool": "other",
	}
	tests := []struct {
		name   string
		script string
		want   []string
	}{
		{"direct call", `await fake_echo(text="x")`, []string{"fake"}},
		{"method call excluded", `obj.fake_echo(text="x")`, nil},
		{"unknown calls only", `read("x") + nope()`, nil},
		{"duplicates collapse, result sorted", `other_tool(); fake_echo(); fake_echo()`, []string{"fake", "other"}},
		{"identifier without call", `fake_echo`, nil},
		{"longer identifier no match", `xfake_echo()`, nil},
		{"nested in gather", `asyncio.gather(fake_echo(text="a"), other_tool())`, []string{"fake", "other"}},
		{"string literal excluded", `await read("see fake_echo(x) for details")`, nil},
		{"single-quoted literal excluded", `s = 'fake_echo(' + "other_tool("`, nil},
		{"triple-quoted literal excluded", "doc = \"\"\"call fake_echo(x)\nthen other_tool()\"\"\"", nil},
		{"comment excluded", "x = 1  # try fake_echo(x) here\nother_tool()", []string{"other"}},
		{"call beside literal still counts", `fake_echo(text="other_tool(") `, []string{"fake"}},
		{"empty script", ``, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := referenced(tt.script, funcToServer)
			if len(got) == 0 {
				got = nil
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("referenced(%q) = %v, want %v", tt.script, got, tt.want)
			}
		})
	}
}
