package rank

import (
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch"
)

func TestFilePathPenalty(t *testing.T) {
	// Expected values from the semble 0.5.2 oracle (_file_path_penalty);
	// penalties compound multiplicatively across families.
	tests := []struct {
		path string
		want float64
	}{
		{"foo.py", 1.0},
		{"test_foo.py", 0.3},
		{"src/foo_test.go", 0.3},
		{"pkg/__init__.py", 0.5},
		{"compat/legacy_thing.py", 0.3},
		{"tests/__init__.py", 0.15},             // testDir 0.3 × reexport 0.5
		{"foo.d.ts", 0.7},                       // .d.ts mild
		{"examples/demo.d.ts", 0.21},            // examples 0.3 × typedef 0.7
		{"src/legacy/compat/foo_test.py", 0.09}, // test 0.3 × compat 0.3
		{"src/pkg/package-info.java", 0.5},      // reexport barrel
		{"__tests__/a.spec.ts", 0.3},            // testDir/testFile (same branch, single 0.3)
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := filePathPenalty(tt.path); !almostEqual(got, tt.want) {
				t.Errorf("filePathPenalty(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestRerankTopkSaturation(t *testing.T) {
	// Expected values from the semble 0.5.2 oracle (rerank_topk, penalise=False):
	// first chunk per file free, then ×0.5^excess, final re-sort by effective score.
	chunks := []semsearch.Chunk{
		mkChunk("a.py", 1, ""), // 0
		mkChunk("a.py", 2, ""), // 1
		mkChunk("a.py", 3, ""), // 2
		mkChunk("b.py", 1, ""), // 3
	}
	// candidates pre-ordered by (start_line, path)
	cands := []scored{{idx: 0, score: 1.0}, {idx: 3, score: 0.85}, {idx: 1, score: 0.9}, {idx: 2, score: 0.8}}
	out := rerankTopk(cands, chunks, 4, false)

	want := []scored{{idx: 0, score: 1.0}, {idx: 3, score: 0.85}, {idx: 1, score: 0.45}, {idx: 2, score: 0.2}}
	assertRanked(t, out, want)
}

func TestRerankTopkPenalties(t *testing.T) {
	// Expected values from the semble 0.5.2 oracle (rerank_topk, penalise=True):
	// the test-file penalty applies before the ranking sort, sinking test_t.py.
	chunks := []semsearch.Chunk{
		mkChunk("a.py", 1, ""),      // 0
		mkChunk("test_t.py", 1, ""), // 1
	}
	cands := []scored{{idx: 0, score: 1.0}, {idx: 1, score: 1.2}}
	out := rerankTopk(cands, chunks, 2, true)

	want := []scored{{idx: 0, score: 1.0}, {idx: 1, score: 0.36}}
	assertRanked(t, out, want)
}

func assertRanked(t *testing.T, got, want []scored) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].idx != want[i].idx || !almostEqual(got[i].score, want[i].score) {
			t.Errorf("[%d] = {idx:%d score:%v}, want {idx:%d score:%v}",
				i, got[i].idx, got[i].score, want[i].idx, want[i].score)
		}
	}
}
