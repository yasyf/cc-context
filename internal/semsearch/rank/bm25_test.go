package rank

import "testing"

func TestEnrichForBM25(t *testing.T) {
	// Expected values from the semble 0.5.2 oracle (enrich_for_bm25).
	tests := []struct {
		name string
		path string
		want string
	}{
		{"nested last-3-dirs", "src/semble/index/foo.py", "def hi(): pass foo foo src semble index"},
		{"bare filename", "bar.py", "def hi(): pass bar bar "},
		{"deeper than 3 dirs", "a/b/c/d/e.py", "def hi(): pass e e b c d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := mkChunk(tt.path, 1, "def hi(): pass")
			if got := EnrichForBM25(c); got != tt.want {
				t.Errorf("EnrichForBM25(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestBM25GetScores(t *testing.T) {
	// 3-doc corpus; expected float64 scores hand-computed from the semble 0.5.2
	// formula (k1=1.5, b=0.75, idf=log(1+(N-df+0.5)/(df+0.5))). semble accumulates
	// in numpy float32; these float64 values agree with it to ~6 significant digits.
	bm := NewBM25()
	bm.AddDocument("0", Tokenize("the quick brown fox")) // len 4
	bm.AddDocument("1", Tokenize("quick fox jumps"))     // len 3
	bm.AddDocument("2", Tokenize("lazy dog sleeps"))     // len 3
	bm.SetDocOrder([]string{"0", "1", "2"})

	tests := []struct {
		name  string
		query string
		want  []float64
	}{
		{"single term", "quick", []float64{0.17247839605348098, 0.1968601588463814, 0.0}},
		{"other single term", "fox", []float64{0.17247839605348098, 0.1968601588463814, 0.0}},
		{"two terms sum", "quick fox", []float64{0.34495679210696195, 0.3937203176927628, 0.0}},
		{"no match", "nomatch", []float64{0.0, 0.0, 0.0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bm.GetScores(Tokenize(tt.query))
			if len(got) != len(tt.want) {
				t.Fatalf("GetScores(%q) len = %d, want %d", tt.query, len(got), len(tt.want))
			}
			for i := range got {
				if !almostEqual(got[i], tt.want[i]) {
					t.Errorf("GetScores(%q)[%d] = %v, want %v", tt.query, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestBM25DuplicateIDPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("AddDocument with duplicate id did not panic")
		}
	}()
	bm := NewBM25()
	bm.AddDocument("x", []string{"a"})
	bm.AddDocument("x", []string{"b"})
}
