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
		{"string literal counts (fail-open)", `await read("see fake_echo(x) for details")`, []string{"fake"}},
		{"single-quoted literal counts", `s = 'fake_echo(' + "other_tool("`, []string{"fake", "other"}},
		{"triple-quoted literal counts", "doc = \"\"\"call fake_echo(x)\nthen other_tool()\"\"\"", []string{"fake", "other"}},
		{"comment counts", "x = 1  # try fake_echo(x) here\nother_tool()", []string{"fake", "other"}},
		{"call beside literal, both count", `fake_echo(text="other_tool(") `, []string{"fake", "other"}},
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

func TestReferencesMCP(t *testing.T) {
	servers := []ServerSpec{{Name: "fake", Prefix: "fake"}, {Name: "other", Prefix: "other"}}
	tests := []struct {
		name    string
		script  string
		servers []ServerSpec
		want    bool
	}{
		{"builtin only", `read("x") + len("y")`, servers, false},
		{"direct call", `await fake_echo(text="x")`, servers, true},
		{"aliased bare identifier", `f = fake_echo`, servers, true},
		{"prefix in string literal counts (fail-open)", `s = "call fake_echo(x) then other_tool("`, servers, true},
		{"prefix in comment counts", "x = 1  # try fake_echo(x) here", servers, true},
		{"comment-paired quotes no longer swallow code", "# a '''\nfake_echo(text=\"x\")\n# b '''", servers, true},
		{"f-string interpolation counts", `msg = f"{fake_echo(text='x')}"`, servers, true},
		{"method access excluded", `obj.fake_echo(text="x")`, servers, false},
		{"collision-suffix prefix", `foo_bar_2_run()`, []ServerSpec{{Name: "foo.bar", Prefix: "foo_bar_2"}}, true},
		{"empty servers", `fake_echo()`, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := referencesMCP(tt.script, tt.servers); got != tt.want {
				t.Errorf("referencesMCP(%q) = %v, want %v", tt.script, got, tt.want)
			}
		})
	}
}
