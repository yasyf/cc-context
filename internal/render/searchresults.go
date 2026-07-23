package render

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/semsearch"
)

// SearchResults renders native search/related results as an anchored span list:
// a "# N results" header, then per result a "path:start-end#hash (labels)"
// locator over the verbatim snippet, results separated by a blank line. A result
// whose start line the cache resolves locally gains a content anchor; one it
// cannot (a stale path) stays a bare "path:start-end". Search results carry
// "score=" (the fused score) plus "cos=" (the raw cosine) when the hit is in the
// semantic candidate set — cos= is suppressed for a BM25-only hit; related
// results carry "cos=" (their cosine similarity). Scores round to two
// significant figures.
func SearchResults(op backend.Op, results []semsearch.Result, files *anchor.Files) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %d results\n", len(results))
	for _, r := range results {
		b.WriteByte('\n')
		b.WriteString(searchLoc(op, r, files))
		b.WriteByte('\n')
		b.WriteString(r.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// searchLoc renders one result's "path:start-end#hash (labels)" locator,
// hashing the start line when the cache resolves it and dropping the anchor
// otherwise.
func searchLoc(op backend.Op, r semsearch.Result, files *anchor.Files) string {
	loc := fmt.Sprintf("%s:%d-%d", r.FilePath, r.StartLine, r.EndLine)
	if text, ok := files.LineAt(r.FilePath, r.StartLine); ok {
		loc = r.FilePath + ":" + anchor.FormatRange(r.StartLine, r.EndLine, anchor.Of(text))
	}
	return fmt.Sprintf("%s (%s)", loc, scoreLabels(op, r))
}

// scoreLabels formats a result's score labels. Related uses the cosine score
// directly as "cos="; search uses "score=" for the fused score and appends
// "cos=" only when the hit carries a semantic (cosine) score.
func scoreLabels(op backend.Op, r semsearch.Result) string {
	if op == backend.OpRelated {
		return fmt.Sprintf("cos=%.2g", r.Score)
	}
	label := fmt.Sprintf("score=%.2g", r.Score)
	if r.SemanticScore != nil {
		label += fmt.Sprintf(" cos=%.2g", *r.SemanticScore)
	}
	return label
}

// WeakResultThreshold is the absolute-cosine floor below which the best semantic
// hit is reported as weak. The 2fc4ffc PR makes absolute cosine first-class;
// this threshold is a tunable var (not grounded in that PR, which carries no
// threshold of its own).
var WeakResultThreshold = 0.15

// WithWeakResultsNote appends a note when the strongest semantic hit's absolute
// cosine falls below WeakResultThreshold — the results are likely off-topic. It
// is a no-op when no result carries a semantic score.
func WithWeakResultsNote(out string, results []semsearch.Result) string {
	best, ok := bestCosine(results)
	if !ok || best >= WeakResultThreshold {
		return out
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return fmt.Sprintf("%s# note: weak semantic match (best cos=%.2g) — results may be off-topic; refine the query\n", out, best)
}

// bestCosine returns the maximum semantic (cosine) score across results, and
// false when none carries one.
func bestCosine(results []semsearch.Result) (float64, bool) {
	best := math.Inf(-1)
	found := false
	for _, r := range results {
		if r.SemanticScore == nil {
			continue
		}
		found = true
		if *r.SemanticScore > best {
			best = *r.SemanticScore
		}
	}
	return best, found
}

// SlowSearchThreshold controls when semantic search latency guidance appears.
var SlowSearchThreshold = 10 * time.Second

// WithSlowSearchNote appends guidance when a semantic search exceeds the
// expected warm-index latency — the first search builds the index.
func WithSlowSearchNote(out string, elapsed time.Duration) string {
	rounded := elapsed.Round(time.Second)
	if elapsed <= SlowSearchThreshold {
		return out
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return fmt.Sprintf("%s# note: slow search (%ds) — first search builds the semantic index; repeats are fast\n", out, int(rounded.Seconds()))
}
