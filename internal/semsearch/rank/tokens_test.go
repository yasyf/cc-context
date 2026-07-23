package rank

import (
	"reflect"
	"testing"
)

func TestSplitIdentifier(t *testing.T) {
	// Expected values from the semble 0.5.2 oracle (split_identifier).
	tests := []struct {
		name  string
		token string
		want  []string
	}{
		{"camelCase", "getHTTPResponse", []string{"gethttpresponse", "get", "http", "response"}},
		{"PascalCase", "HandlerStack", []string{"handlerstack", "handler", "stack"}},
		{"acronym prefix", "XMLParser", []string{"xmlparser", "xml", "parser"}},
		{"acronym mid", "ABCdef", []string{"abcdef", "ab", "cdef"}},
		{"snake", "my_func", []string{"my_func", "my", "func"}},
		{"snake multi", "a_b_c", []string{"a_b_c", "a", "b", "c"}},
		{"single part no subparts", "simple", []string{"simple"}},
		{"all-caps single", "HTTP", []string{"http"}},
		{"digits", "foo2Bar", []string{"foo2bar", "foo", "2", "bar"}},
		{"leading underscore", "_private", []string{"_private"}},
		{"dunder collapses", "__init__", []string{"__init__"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SplitIdentifier(tt.token); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitIdentifier(%q) = %v, want %v", tt.token, got, tt.want)
			}
		})
	}
}

func TestTokenize(t *testing.T) {
	// Expected values from the semble 0.5.2 oracle (tokenize).
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"call expr", "getHTTPResponse(x)", []string{"gethttpresponse", "get", "http", "response", "x"}},
		{"mixed", "my_func and simpleName", []string{"my_func", "my", "func", "and", "simplename", "simple", "name"}},
		{"sql", "CREATE TABLE users", []string{"create", "table", "users"}},
		{"empty", "", nil},
		{"punctuation only", "-->", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Tokenize(tt.text); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Tokenize(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}
