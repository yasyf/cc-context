package web

import (
	"reflect"
	"testing"
)

func TestFuse(t *testing.T) {
	// n=4 chunks. Best-first orderings:
	//   dense = [0 2 3 1] -> denseRank: 0→1, 2→2, 3→3, 1→4
	//   lex   = [2 0 1 3] -> lexRank:   2→1, 0→2, 1→3, 3→4
	// score(c) = 3/(60+denseRank) + 1/(60+lexRank), ranks 1-based:
	//   chunk0 = 3/61 + 1/62 = 0.049180328 + 0.016129032 = 0.065309360
	//   chunk1 = 3/64 + 1/63 = 0.046875000 + 0.015873016 = 0.062748016
	//   chunk2 = 3/62 + 1/61 = 0.048387097 + 0.016393443 = 0.064780539
	//   chunk3 = 3/63 + 1/64 = 0.047619048 + 0.015625000 = 0.063244048
	// descending: chunk0 > chunk2 > chunk3 > chunk1.
	dense := []int{0, 2, 3, 1}
	lex := []int{2, 0, 1, 3}
	tests := []struct {
		name string
		k    int
		want []int
	}{
		{"full ordering", 4, []int{0, 2, 3, 1}},
		{"top-2", 2, []int{0, 2}},
		{"top-1", 1, []int{0}},
		{"k exceeds n clamps", 10, []int{0, 2, 3, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fuse(dense, lex, tt.k); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("fuse(k=%d) = %v, want %v", tt.k, got, tt.want)
			}
		})
	}
}

func TestFuseDenseWeightDominates(t *testing.T) {
	// dense weight 3 outweighs lex weight 1: the chunk ranked best by dense but
	// worst by lex beats the chunk ranked worst by dense but best by lex.
	//   chunk0: dense#1, lex#2 -> 3/61 + 1/62 = 0.065309360
	//   chunk1: dense#2, lex#1 -> 3/62 + 1/61 = 0.064780539
	dense := []int{0, 1}
	lex := []int{1, 0}
	if got := fuse(dense, lex, 2); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("fuse = %v, want [0 1] (dense rank dominates)", got)
	}
}

func TestFuseBM25OnlyDegraded(t *testing.T) {
	// nil or empty dense drops the dense term (weight 1.0 on lex alone). Since
	// 1/(60+rank) is monotonic in rank, the fused order equals the lex order.
	lex := []int{2, 0, 1, 3}
	for _, name := range []string{"nil", "empty"} {
		t.Run(name, func(t *testing.T) {
			var dense []int
			if name == "empty" {
				dense = []int{}
			}
			if got := fuse(dense, lex, 4); !reflect.DeepEqual(got, lex) {
				t.Errorf("degraded fuse = %v, want %v", got, lex)
			}
			if got := fuse(dense, lex, 2); !reflect.DeepEqual(got, []int{2, 0}) {
				t.Errorf("degraded fuse top-2 = %v, want [2 0]", got)
			}
		})
	}
}

func TestDenseOrder(t *testing.T) {
	q := []float32{1, 0}
	tests := []struct {
		name string
		vecs [][]float32
		want []int
	}{
		{
			// dots: [0·1, 1·1, 0.6·1] = [0, 1, 0.6] -> 1 > 2 > 0.
			"reorders by cosine",
			[][]float32{{0, 1}, {1, 0}, {0.6, 0.8}},
			[]int{1, 2, 0},
		},
		{
			// doc0 and doc1 identical -> dot tie (1.0) resolves to document order.
			"tie broken by document order",
			[][]float32{{1, 0}, {1, 0}, {0, 1}},
			[]int{0, 1, 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := denseOrder(tt.vecs, q); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("denseOrder = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFuseEndToEnd(t *testing.T) {
	// Full hybrid path: BM25 lexical order + dense order over the same chunks,
	// fused to top-2. Chunks:
	//   c0 "install the cli quickly"
	//   c1 "configure the api endpoint"
	//   c2 "cli reference for the api"
	docs := []string{
		"install the cli quickly",
		"configure the api endpoint",
		"cli reference for the api",
	}
	lex := newBM25(docs).rank("cli api")
	// Query vector nearest c2, then c1, then c0.
	vecs := [][]float32{{1, 0, 0}, {0, 1, 0}, {0.7, 0.7, 0}}
	dense := denseOrder(vecs, []float32{0.6, 0.8, 0})
	got := fuse(dense, lex, 2)
	if len(got) != 2 {
		t.Fatalf("fuse top-2 returned %d hits: %v", len(got), got)
	}
	// c2 mentions both cli and api and is dense-favored -> it must rank first.
	if got[0] != 2 {
		t.Errorf("fuse top hit = %d, want chunk 2; full=%v (lex=%v dense=%v)", got[0], got, lex, dense)
	}
}
