package rank

import (
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch"
)

func scoreOf(t *testing.T, cands []scored, idx int) float64 {
	t.Helper()
	for _, c := range cands {
		if c.idx == idx {
			return c.score
		}
	}
	t.Fatalf("no cand with idx %d", idx)
	return 0
}

func TestIsSymbolQuery(t *testing.T) {
	// Expected values from the semble 0.5.2 oracle (is_symbol_query).
	tests := []struct {
		query string
		want  bool
	}{
		{"Session", true},
		{"session", false},
		{"HandlerStack", true},
		{"get_user", true},
		{"Sinatra::Base", true},
		{"my->method", true},
		{"foo.bar", true},
		{"_private", true},
		{"http", false},
		{"CREATE", true},
		{"getHTTP", true},
		{"MyClass", true},
		{"a", false},
		{"AB", true},
		{"abc123", false},
		{"Foo1Bar", true},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			if got := IsSymbolQuery(tt.query); got != tt.want {
				t.Errorf("IsSymbolQuery(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestResolveAlpha(t *testing.T) {
	explicit := 0.7
	tests := []struct {
		name  string
		query string
		alpha *float64
		want  float64
	}{
		{"nl auto", "session", nil, alphaNL},
		{"symbol auto", "MyClass", nil, alphaSymbol},
		{"explicit override", "session", &explicit, 0.7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveAlpha(tt.query, tt.alpha); got != tt.want {
				t.Errorf("ResolveAlpha(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestExtractSymbolName(t *testing.T) {
	tests := []struct{ query, want string }{
		{"Sinatra::Base", "Base"},
		{"my->method", "method"},
		{"foo.bar", "bar"},
		{"Client", "Client"},
		{`a\b`, "b"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			if got := extractSymbolName(tt.query); got != tt.want {
				t.Errorf("extractSymbolName(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}

func TestStemMatches(t *testing.T) {
	tests := []struct {
		stem, name string
		want       bool
	}{
		{"session", "session", true},
		{"sessions", "session", true},
		{"my_class", "myclass", true},
		{"other", "session", false},
	}
	for _, tt := range tests {
		t.Run(tt.stem+"~"+tt.name, func(t *testing.T) {
			if got := stemMatches(tt.stem, tt.name); got != tt.want {
				t.Errorf("stemMatches(%q,%q) = %v, want %v", tt.stem, tt.name, got, tt.want)
			}
		})
	}
}

func TestBoostMultiChunkFiles(t *testing.T) {
	// Expected values from the semble 0.5.2 oracle (boost_multi_chunk_files).
	chunks := []semsearch.Chunk{
		mkChunk("a.py", 1, ""),  // 0
		mkChunk("a.py", 10, ""), // 1
		mkChunk("b.py", 1, ""),  // 2
	}
	cands := []scored{{idx: 0, score: 1.0}, {idx: 2, score: 0.8}, {idx: 1, score: 0.5}}
	boostMultiChunkFiles(cands, chunks)

	want := map[int]float64{0: 1.2, 1: 0.5, 2: 0.9066666666666667}
	for idx, w := range want {
		if got := scoreOf(t, cands, idx); !almostEqual(got, w) {
			t.Errorf("idx %d = %v, want %v", idx, got, w)
		}
	}
}

func TestApplyQueryBoostSymbol(t *testing.T) {
	// Expected values from the semble 0.5.2 oracle (apply_query_boost, symbol branch).
	chunks := []semsearch.Chunk{
		mkChunk("helpers.py", 1, "def helper(): pass"),                         // 0
		mkChunk("myclass.py", 1, "class MyClass:\n    def method(self): pass"), // 1
		mkChunk("sub/MyClass.py", 1, "class MyClass:\n    pass"),               // 2 non-candidate
	}
	cands := []scored{{idx: 0, score: 1.0}, {idx: 1, score: 0.6}}
	out := applyQueryBoost(cands, "MyClass", chunks)

	want := map[int]float64{0: 1.0, 1: 5.1, 2: 4.5}
	if len(out) != 3 {
		t.Fatalf("out len = %d, want 3 (non-candidate appended)", len(out))
	}
	for idx, w := range want {
		if got := scoreOf(t, out, idx); !almostEqual(got, w) {
			t.Errorf("idx %d = %v, want %v", idx, got, w)
		}
	}
}

func TestApplyQueryBoostNLStem(t *testing.T) {
	// Expected values from the semble 0.5.2 oracle (apply_query_boost, NL stem branch).
	chunks := []semsearch.Chunk{
		mkChunk("session.py", 1, "def login(): pass"), // 0
		mkChunk("other.py", 1, "def foo(): pass"),     // 1
	}
	cands := []scored{{idx: 0, score: 1.0}, {idx: 1, score: 0.8}}
	out := applyQueryBoost(cands, "user session handler", chunks)

	want := map[int]float64{0: 1.3333333333333333, 1: 0.8}
	for idx, w := range want {
		if got := scoreOf(t, out, idx); !almostEqual(got, w) {
			t.Errorf("idx %d = %v, want %v", idx, got, w)
		}
	}
}

func TestApplyQueryBoostNLEmbedded(t *testing.T) {
	// Expected values from the semble 0.5.2 oracle (apply_query_boost, NL embedded-symbol branch).
	chunks := []semsearch.Chunk{
		mkChunk("state.py", 1, "class StateManager:\n pass"), // 0
		mkChunk("misc.py", 1, "x=1"),                         // 1
	}
	cands := []scored{{idx: 0, score: 0.5}, {idx: 1, score: 0.9}}
	out := applyQueryBoost(cands, "find the StateManager please", chunks)

	want := map[int]float64{0: 2.1500000000000004, 1: 0.9}
	for idx, w := range want {
		if got := scoreOf(t, out, idx); !almostEqual(got, w) {
			t.Errorf("idx %d = %v, want %v", idx, got, w)
		}
	}
}
