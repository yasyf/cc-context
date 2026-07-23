package render

import (
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/semsearch"
)

func floatPtr(f float64) *float64 { return &f }

// searchFixture is two search hits: a semantic hit anchored to example.go and a
// BM25-only remote hit whose cos= is suppressed.
func searchFixture() []semsearch.Result {
	return []semsearch.Result{
		{FilePath: "example.go", StartLine: 19, EndLine: 21, Score: 0.481, SemanticScore: floatPtr(0.324), Content: "func (g *Greeter) Greet(name string) string {"},
		{FilePath: "remote/only.go", StartLine: 3, EndLine: 5, Score: 0.12, Content: "bm25 only"},
	}
}

func TestSearchResultsGolden(t *testing.T) {
	got := SearchResults(backend.OpSearch, searchFixture(), anchor.NewFiles("testdata"))
	checkGolden(t, "search_golden.txt", got)
}

func TestSearchResultsInvariants(t *testing.T) {
	got := SearchResults(backend.OpSearch, searchFixture(), anchor.NewFiles("testdata"))
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"header counts results", "# 2 results", true},
		{"local hit anchored", "example.go:19-21#", true},
		{"local hit carries score and cos", "(score=0.48 cos=0.32)", true},
		{"remote hit stays bare", "remote/only.go:3-5 ", true},
		{"bm25-only hit has score", "(score=0.12)", true},
		{"bm25-only hit suppresses cos", "0.12 cos=", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if strings.Contains(got, tt.line) != tt.want {
				verb := "contain"
				if !tt.want {
					verb = "not contain"
				}
				t.Errorf("want output to %s %q\n---\n%s", verb, tt.line, got)
			}
		})
	}
}

func TestRelatedResultsLabel(t *testing.T) {
	results := []semsearch.Result{
		{FilePath: "example.go", StartLine: 19, EndLine: 21, Score: 0.68, SemanticScore: floatPtr(0.68), Content: "x"},
	}
	got := SearchResults(backend.OpRelated, results, anchor.NewFiles("testdata"))
	if !strings.Contains(got, "(cos=0.68)") {
		t.Errorf("related label missing cos=: %q", got)
	}
	if strings.Contains(got, "score=") {
		t.Errorf("related label should not carry score=: %q", got)
	}
}

func TestSearchResultsEmpty(t *testing.T) {
	got := SearchResults(backend.OpSearch, nil, anchor.NewFiles("testdata"))
	if got != "# 0 results\n" {
		t.Errorf("empty results = %q, want %q", got, "# 0 results\n")
	}
}

func TestWithWeakResultsNote(t *testing.T) {
	const out = "# 1 results\n"
	prev := WeakResultThreshold
	WeakResultThreshold = 0.2
	t.Cleanup(func() { WeakResultThreshold = prev })

	tests := []struct {
		name     string
		results  []semsearch.Result
		wantNote bool
	}{
		{"weak best cosine", []semsearch.Result{{SemanticScore: floatPtr(0.1)}}, true},
		{"strong best cosine", []semsearch.Result{{SemanticScore: floatPtr(0.5)}}, false},
		{"mixed keeps best", []semsearch.Result{{SemanticScore: floatPtr(0.05)}, {SemanticScore: floatPtr(0.5)}}, false},
		{"no semantic score", []semsearch.Result{{Score: 0.9}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WithWeakResultsNote(out, tt.results)
			hasNote := strings.Contains(got, "# note: weak semantic match")
			if hasNote != tt.wantNote {
				t.Errorf("weak note = %v, want %v (out=%q)", hasNote, tt.wantNote, got)
			}
		})
	}
}

func TestWithSlowSearchNote(t *testing.T) {
	const out = "# 0 results\n"
	if got := WithSlowSearchNote(out, SlowSearchThreshold); got != out {
		t.Errorf("threshold output = %q, want unchanged %q", got, out)
	}
	want := out + "# note: slow search (11s) — first search builds the semantic index; repeats are fast\n"
	if got := WithSlowSearchNote(out, SlowSearchThreshold+500*time.Millisecond); got != want {
		t.Errorf("slow output = %q, want %q", got, want)
	}
}
