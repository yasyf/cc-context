package render

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yasyf/cc-context/anchor"
)

// sembleResult is one chunk in a semble search or find-related response.
type sembleResult struct {
	FilePath  string  `json:"file_path"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Score     float64 `json:"score"`
	Content   string  `json:"content"`
}

// sembleResponse is the top-level shape of semble's search/related JSON; the
// query echo is parsed but deliberately dropped from the rendering.
type sembleResponse struct {
	Results []sembleResult `json:"results"`
}

// SembleResults reshapes semble's raw search/find-related JSON into an
// anchored span list: a "# N results" header, then per result a
// "path:start-end#hash (label=0.019)" locator over the verbatim snippet, results
// separated by a blank line. The query echo is dropped and scores round to two
// significant figures. A result whose start line the cache resolves locally
// gains a content anchor; one it cannot — a remote-repo hit or a stale path —
// stays a bare "path:start-end". A JSON parse failure returns a wrapped error
// rather than falling back to the raw payload.
func SembleResults(jsonText string, files *anchor.Files, scoreLabel string) (string, error) {
	var resp sembleResponse
	if err := json.Unmarshal([]byte(jsonText), &resp); err != nil {
		return "", fmt.Errorf("render semble results: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %d results\n", len(resp.Results))
	for _, r := range resp.Results {
		b.WriteByte('\n')
		b.WriteString(sembleLoc(r, files, scoreLabel))
		b.WriteByte('\n')
		b.WriteString(r.Content)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// sembleLoc renders one result's "path:start-end#hash (label=score)" locator, hashing
// the start line when the cache resolves it and dropping the anchor otherwise.
func sembleLoc(r sembleResult, files *anchor.Files, scoreLabel string) string {
	loc := fmt.Sprintf("%s:%d-%d", r.FilePath, r.StartLine, r.EndLine)
	if text, ok := files.LineAt(r.FilePath, r.StartLine); ok {
		loc = r.FilePath + ":" + anchor.FormatRange(r.StartLine, r.EndLine, anchor.Of(text))
	}
	return fmt.Sprintf("%s (%s=%.2g)", loc, scoreLabel, r.Score)
}

// SlowSearchThreshold controls when semantic search latency guidance appears.
var SlowSearchThreshold = 10 * time.Second

// WithSlowSearchNote appends guidance when a semantic search exceeds the
// expected warm-index latency.
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
