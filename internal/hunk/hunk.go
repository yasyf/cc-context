// Package hunk computes line-mode change hunks between two byte snapshots of a
// file and reapplies a chosen subset, so a caller can commit only some of a
// file's edits. Each hunk is one contiguous run of changed lines with zero
// context, addressed by a content digest that stays stable when the surrounding
// diff shifts.
package hunk

import (
	"fmt"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/yasyf/cc-context/internal/anchor"
)

// Hunk is one contiguous run of changed lines carrying zero context. Coordinates
// are 1-indexed inclusive: base lines OldStart..OldEnd become current lines
// NewStart..NewEnd. A pure insertion has len(Old)==0 and OldEnd==OldStart-1 (the
// new lines land before base line OldStart); a pure deletion has len(New)==0 and
// NewEnd==NewStart-1. Line text keeps any '\r' (CRLF lives inside the line); the
// terminating '\n' is not part of the text. Digest identifies the hunk by content
// alone, so an identical change digests the same wherever it sits.
type Hunk struct {
	OldStart int
	OldEnd   int
	NewStart int
	NewEnd   int
	Old      []string
	New      []string
	Digest   anchor.Hash

	// newTrailingNoNL marks that this hunk's final New line is the file's last
	// line with no terminating newline; it is the one bit the stripped New text
	// cannot carry, and Select needs it to rebuild the EOF-newline state.
	newTrailingNoNL bool
}

// Compute diffs base against current in line mode and returns the change hunks in
// ascending order, each a contiguous run with zero context. Output is
// deterministic. An empty base against a non-empty current yields one whole-file
// insertion hunk; equal inputs yield no hunks. Lines split on '\n' with any '\r'
// retained and a trailing newline modeled, not lost, mirroring internal/edit.
func Compute(base, current []byte) []Hunk {
	dmp := diffmatchpatch.New()
	dmp.DiffTimeout = 0 // zero deadline: a fully optimal, deterministic diff
	chars1, chars2, lineArray := dmp.DiffLinesToChars(string(base), string(current))
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(chars1, chars2, false), lineArray)

	var hunks []Hunk
	oldPos, newPos := 1, 1
	var cur *Hunk
	flush := func() {
		if cur != nil {
			hunks = append(hunks, finalize(*cur))
			cur = nil
		}
	}
	open := func() {
		if cur == nil {
			cur = &Hunk{OldStart: oldPos, NewStart: newPos}
		}
	}
	for _, d := range diffs {
		tokens := toTokens(d.Text)
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			flush()
			oldPos += len(tokens)
			newPos += len(tokens)
		case diffmatchpatch.DiffDelete:
			open()
			for _, t := range tokens {
				cur.Old = append(cur.Old, stripNL(t))
			}
			oldPos += len(tokens)
		case diffmatchpatch.DiffInsert:
			open()
			for _, t := range tokens {
				cur.New = append(cur.New, stripNL(t))
				cur.newTrailingNoNL = !strings.HasSuffix(t, "\n")
			}
			newPos += len(tokens)
		}
	}
	flush()
	return hunks
}

// finalize fills the end coordinates from the accumulated line counts and the
// content digest from the kind-marked lines: every Old line prefixed "-" then
// every New line prefixed "+", so deleting a line and adding the same line digest
// differently.
func finalize(h Hunk) Hunk {
	h.OldEnd = h.OldStart + len(h.Old) - 1
	h.NewEnd = h.NewStart + len(h.New) - 1
	marked := make([]string, 0, len(h.Old)+len(h.New))
	for _, o := range h.Old {
		marked = append(marked, "-"+o)
	}
	for _, n := range h.New {
		marked = append(marked, "+"+n)
	}
	h.Digest = anchor.OfLines(marked)
	return h
}

// Select reconstructs base with only the hunks keep(i) reports true applied,
// returning the exact bytes. hunks must be Compute's output for base (ascending,
// non-overlapping). Keeping every hunk reproduces the current passed to Compute;
// keeping none reproduces base; the EOF-newline state is correct across every
// transition.
func Select(base []byte, hunks []Hunk, keep func(i int) bool) []byte {
	baseTokens := toTokens(string(base))
	var out strings.Builder
	cursor := 1 // 1-indexed next base token to emit
	for i, h := range hunks {
		for p := cursor; p < h.OldStart; p++ {
			out.WriteString(baseTokens[p-1])
		}
		if keep(i) {
			for j, line := range h.New {
				out.WriteString(line)
				if j < len(h.New)-1 || !h.newTrailingNoNL {
					out.WriteString("\n")
				}
			}
		} else {
			for p := h.OldStart; p <= h.OldEnd; p++ {
				out.WriteString(baseTokens[p-1])
			}
		}
		cursor = h.OldEnd + 1
	}
	for p := cursor; p <= len(baseTokens); p++ {
		out.WriteString(baseTokens[p-1])
	}
	return []byte(out.String())
}

// ParseRef splits a "file:A-B#hash" hunk reference into its path and anchor. It
// wraps anchor.ParseLoc but additionally requires a line-qualified content hash:
// a bare numeric range (no #hash) and a hash with no line range are both rejected,
// since a hunk ref must pin both where and what.
func ParseRef(s string) (path string, ref anchor.Ref, err error) {
	path, ref, ok, err := anchor.ParseLoc(s)
	if err != nil {
		return "", anchor.Ref{}, fmt.Errorf("invalid hunk ref %q (expected file:A-B#hash): %w", s, err)
	}
	if !ok || ref.Line < 1 {
		return "", anchor.Ref{}, fmt.Errorf("invalid hunk ref %q: expected file:A-B#hash", s)
	}
	return path, ref, nil
}

// toTokens splits text into line tokens each retaining its terminating '\n',
// matching diffmatchpatch's own line split: a final line with no newline is its
// own token and empty text yields none, so token indexes align with base and
// current positions.
func toTokens(text string) []string {
	if text == "" {
		return nil
	}
	var tokens []string
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			tokens = append(tokens, text[start:i+1])
			start = i + 1
		}
	}
	if start < len(text) {
		tokens = append(tokens, text[start:])
	}
	return tokens
}

// stripNL drops a token's terminating '\n', leaving any '\r' in the line text.
func stripNL(token string) string {
	return strings.TrimSuffix(token, "\n")
}
