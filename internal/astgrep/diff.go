// Package astgrep renders ast-grep `run --json=stream` output into bounded,
// signature-grade text: a match list for structural search and a preview diff for
// replace. The functions are pure over the JSON bytes; budget capping and process
// execution live in the caller.
package astgrep

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/anchor"
)

// Match is one ast-grep `--json=stream` record. Only the fields the renderers
// consume are decoded; replacement is empty for a non-rewrite search.
type Match struct {
	File        string `json:"file"`
	Text        string `json:"text"`
	Lines       string `json:"lines"`
	Replacement string `json:"replacement"`
	Range       struct {
		Start struct {
			Line int `json:"line"`
		} `json:"start"`
		End struct {
			Line int `json:"line"`
		} `json:"end"`
	} `json:"range"`
}

// Parse decodes a `--json=stream` body (one JSON object per line) into matches.
// A blank stream (ast-grep's clean no-match output) parses to zero matches.
func Parse(stream []byte) ([]Match, error) {
	var matches []Match
	sc := bufio.NewScanner(bytes.NewReader(stream))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m Match
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode ast-grep json: %w", err)
		}
		matches = append(matches, m)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan ast-grep json: %w", err)
	}
	return matches, nil
}

// DistinctFiles counts the distinct files among matches. Phase 4's apply-cap
// gates on this count.
func DistinctFiles(matches []Match) int {
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		seen[m.File] = struct{}{}
	}
	return len(seen)
}

// RenderSearch renders matches as one `file:Lstart-Lend  <trimmed first match
// line>` per hit — locations and a one-line preview, never bodies.
func RenderSearch(matches []Match) string {
	var b strings.Builder
	for _, m := range matches {
		fmt.Fprintf(&b, "%s  %s\n", loc(m), firstLine(m.Text))
	}
	return b.String()
}

// RenderPreview renders a signature-grade diff: a `# N matches across M files`
// header, then per hit a `path:line#hash` anchor with `- <old>` / `+ <new>`
// lines drawn from text and replacement, making every preview hit addressable.
func RenderPreview(matches []Match) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %d matches across %d files\n", len(matches), DistinctFiles(matches))
	for _, m := range matches {
		fmt.Fprintf(&b, "%s:%s\n", m.File, anchor.Format(oneBased(m.Range.Start.Line), matchAnchor(m)))
		for _, line := range strings.Split(m.Text, "\n") {
			fmt.Fprintf(&b, "- %s\n", line)
		}
		for _, line := range strings.Split(m.Replacement, "\n") {
			fmt.Fprintf(&b, "+ %s\n", line)
		}
	}
	return b.String()
}

// loc formats a match's line span as file:Lstart-Lend#hash, collapsing to
// file:Lstart#hash when the match is single-line. The content anchor pins the
// start line. ast-grep reports 0-based lines; the ccx file:line convention is
// 1-based, so the lines are shifted to match.
func loc(m Match) string {
	start, end := oneBased(m.Range.Start.Line), oneBased(m.Range.End.Line)
	h := matchAnchor(m)
	if start == end {
		return fmt.Sprintf("%s:L%d#%s", m.File, start, h)
	}
	return fmt.Sprintf("%s:L%d-L%d#%s", m.File, start, end, h)
}

// matchAnchor derives a match's content anchor from the first of its own source
// lines. Match.Lines is byte-for-byte the source, so no file read is needed; Of
// trims, so leading indentation and a trailing CR do not perturb the hash.
func matchAnchor(m Match) anchor.Hash {
	line := m.Lines
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return anchor.Of(line)
}

// oneBased converts an ast-grep 0-based line number to the ccx 1-based convention.
func oneBased(line int) int {
	return line + 1
}

// firstLine returns the first line of s with surrounding whitespace trimmed.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
