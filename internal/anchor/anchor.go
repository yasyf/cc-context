// Package anchor implements content anchors: 4-character content-derived line
// hashes ("120#a3fk") that pin a section reference to what a line says rather
// than where it sits, so the reference survives edits that shift line numbers.
package anchor

import (
	"fmt"
	"hash/fnv"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	letters  = "abcdefghjkmnpqrstvwxyz"
	fold     = uint32(len(letters) * 32 * 32 * 32)
)

// Hash is one line's 4-character content hash in lowercase Crockford-style
// base32. The first character is always a letter, so a hash can never parse as
// a line number.
type Hash string

// String returns the hash's textual form.
func (h Hash) String() string {
	return string(h)
}

// Of hashes one raw file line (no trailing newline): FNV-1a 32-bit over the
// whitespace-trimmed line, folded to 22*32^3 values and encoded letter-first.
func Of(line string) Hash {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.TrimSpace(line))) // fnv.Write never fails
	v := h.Sum32() % fold
	return Hash([]byte{
		letters[v>>15],
		alphabet[(v>>10)&31],
		alphabet[(v>>5)&31],
		alphabet[v&31],
	})
}

// OfLines hashes an ordered group of lines as one content digest: each line is
// trimmed as Of trims, the trimmed lines are joined with '\n', and the join is
// folded and encoded exactly as Of. OfLines of a single line equals Of of it.
func OfLines(lines []string) Hash {
	h := fnv.New32a()
	for i, line := range lines {
		if i > 0 {
			_, _ = h.Write([]byte{'\n'}) // fnv.Write never fails
		}
		_, _ = h.Write([]byte(strings.TrimSpace(line)))
	}
	v := h.Sum32() % fold
	return Hash([]byte{
		letters[v>>15],
		alphabet[(v>>10)&31],
		alphabet[(v>>5)&31],
		alphabet[v&31],
	})
}

// Ref is a parsed anchor reference. Line 0 marks a bare anchor (hash only);
// End 0 marks a single-line reference.
type Ref struct {
	Line int
	End  int
	Hash Hash
}

// Range is a resolved 1-indexed inclusive line range.
type Range struct {
	Start int
	End   int
}

// Move records that an anchored line resolved away from its hinted position.
type Move struct {
	From int
	To   int
}

var (
	numericRe    = regexp.MustCompile(`^\d+(?:-\d+)?$`)
	anchorRe     = regexp.MustCompile(`^(?:(\d+)(?:-(\d+))?#)?([a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{3})$`)
	anchorishRe  = regexp.MustCompile(`^\d[\d-]*#`)
	commaRangeRe = regexp.MustCompile(`^\s*(\d+)\s*,\s*(\d+)\s*$`)
)

// NormalizeRange rewrites a comma-separated numeric range ("A,B" or "A, B") to
// the canonical "A-B" form; every other section string, including a heading that
// contains a comma, passes through unchanged.
func NormalizeRange(section string) string {
	if m := commaRangeRe.FindStringSubmatch(section); m != nil {
		return m[1] + "-" + m[2]
	}
	return section
}

// ParseNumericRange parses a line range — a plain "A", a dash "A-B", or the comma
// alias "A,B" — into 1-indexed inclusive bounds. Bounds are digits only: a signed
// value like "16--20" is not a range. ok is false with a nil err for anything not
// range-shaped (a heading, an anchor, a signed or empty value); a range-shaped
// value whose start exceeds its end is ok false with a non-nil err.
func ParseNumericRange(section string) (start, end int, ok bool, err error) {
	section = NormalizeRange(section)
	dash := strings.IndexByte(section, '-')
	if dash < 0 {
		n, digits := parsePositiveInt(section)
		if !digits {
			return 0, 0, false, nil
		}
		return n, n, true, nil
	}
	start, startOK := parsePositiveInt(section[:dash])
	end, endOK := parsePositiveInt(section[dash+1:])
	if !startOK || !endOK {
		return 0, 0, false, nil
	}
	if start > end {
		return 0, 0, false, fmt.Errorf("line range %q is reversed: start %d is after end %d", section, start, end)
	}
	return start, end, true, nil
}

// parsePositiveInt parses a run of ASCII digits into a non-negative int; ok is
// false for an empty string, a sign, or any non-digit character.
func parsePositiveInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Parse classifies a section string. A numeric line or range and anything not
// anchor-shaped pass through (ok false, nil error); a well-formed anchor
// returns its Ref (ok true); an anchor-shaped string with a malformed hash or
// an invalid line qualifier — a zero line (lines are 1-indexed) or a reversed
// range (end before start) — errors with the expected form, so it never reaches
// a backend that would reject it without a hint.
func Parse(section string) (Ref, bool, error) {
	if numericRe.MatchString(section) {
		return Ref{}, false, nil
	}
	if m := anchorRe.FindStringSubmatch(section); m != nil {
		ref := Ref{Hash: Hash(m[3])}
		var err error
		if m[1] != "" {
			if ref.Line, err = strconv.Atoi(m[1]); err != nil {
				return Ref{}, false, fmt.Errorf("anchor %q: line out of range: %w", section, err)
			}
			if ref.Line == 0 {
				return Ref{}, false, fmt.Errorf("invalid anchor %q: line 0 is invalid — line numbers are 1-indexed (re-run ccx code outline for fresh anchors)", section)
			}
		}
		if m[2] != "" {
			if ref.End, err = strconv.Atoi(m[2]); err != nil {
				return Ref{}, false, fmt.Errorf("anchor %q: end line out of range: %w", section, err)
			}
			if ref.End < ref.Line {
				return Ref{}, false, fmt.Errorf("invalid anchor %q: end line %d before start line %d — a range needs 1-indexed start ≤ end (re-run ccx code outline for fresh anchors)", section, ref.End, ref.Line)
			}
		}
		return ref, true, nil
	}
	if anchorishRe.MatchString(section) {
		return Ref{}, false, fmt.Errorf("invalid anchor %q: want \"120#a3fk\" or \"120-180#a3fk\" — 4 lowercase base32 chars after #, letter first (re-run ccx code outline for fresh anchors)", section)
	}
	return Ref{}, false, nil
}

// ParseLoc splits a "path:section" location whose section is an anchor. It
// splits at the last ':' — the anchor grammar admits no ':', so Windows drive
// letters and any earlier colons stay in the path. A location whose suffix is
// numeric or not anchor-shaped passes through (ok false, nil error).
func ParseLoc(loc string) (path string, ref Ref, ok bool, err error) {
	i := strings.LastIndexByte(loc, ':')
	if i < 0 {
		return "", Ref{}, false, nil
	}
	ref, ok, err = Parse(loc[i+1:])
	if err != nil {
		return "", Ref{}, false, fmt.Errorf("location %q: %w", loc, err)
	}
	if !ok {
		return "", Ref{}, false, nil
	}
	return loc[:i], ref, true, nil
}

// Format renders a single-line anchor reference like "120#a3fk".
func Format(line int, h Hash) string {
	return fmt.Sprintf("%d#%s", line, h)
}

// FormatRange renders a line-range anchor reference like "120-180#a3fk".
func FormatRange(start, end int, h Hash) string {
	return fmt.Sprintf("%d-%d#%s", start, end, h)
}

// File holds one file's lines, loaded fresh for a single anchor resolution.
type File struct {
	path  string
	lines []string
}

// Load reads path and splits it into lines. Lines keep any trailing '\r' — Of
// trims it, so CRLF files hash identically to LF files.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the caller's own read target, not untrusted input
	if err != nil {
		return nil, fmt.Errorf("load anchors: %w", err)
	}
	return FromBytes(path, data), nil
}

// FromBytes splits an already-read snapshot into a File, so a caller that must
// resolve and rewrite from one atomic read shares Load's line split exactly:
// lines keep any trailing '\r', and a final empty element from a trailing
// newline is dropped.
func FromBytes(path string, data []byte) *File {
	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return &File{path: path, lines: lines}
}

// Lines returns the file's lines, each keeping any trailing '\r'.
func (f *File) Lines() []string {
	return f.lines
}

// At returns the content hash of the 1-indexed line.
func (f *File) At(line int) Hash {
	return Of(f.lines[line-1])
}

// Resolve locates ref's content: an exact line+hash hit resolves silently; a
// miss falls back to the line with the same content nearest the hint (ties go
// earlier) and reports the Move; content found nowhere is an error, as is a
// bare anchor matching more than one line.
func (f *File) Resolve(ref Ref) (Range, *Move, error) {
	if ref.Line >= 1 && ref.Line <= len(f.lines) && f.At(ref.Line) == ref.Hash {
		return f.rangeAt(ref, ref.Line), nil, nil
	}
	var candidates []int
	for i, line := range f.lines {
		if Of(line) == ref.Hash {
			candidates = append(candidates, i+1)
		}
	}
	switch {
	case len(candidates) == 0:
		return Range{}, nil, fmt.Errorf("anchor %s not found in %s: content changed — re-run ccx code outline %s", ref.Hash, f.path, f.path)
	case ref.Line == 0 && len(candidates) == 1:
		return f.rangeAt(ref, candidates[0]), nil, nil
	case ref.Line == 0:
		return Range{}, nil, fmt.Errorf("anchor %s matches lines %s in %s: qualify it with a line, like %s", ref.Hash, joinLines(candidates), f.path, Format(candidates[0], ref.Hash))
	}
	to := nearest(candidates, ref.Line)
	return f.rangeAt(ref, to), &Move{From: ref.Line, To: to}, nil
}

// rangeAt builds the resolved range starting at start: a single-line ref ends
// where it starts, a range ref shifts its end by the same distance the start
// moved. The parsed end is clamped to EOF before the shift — a grammatically
// valid but out-of-range end can never be meaningful, and clamping first keeps
// the shift from overflowing int — then the shifted end is re-clamped to
// [start, len(lines)].
func (f *File) rangeAt(ref Ref, start int) Range {
	end := start
	if ref.End > 0 {
		end = min(ref.End, len(f.lines)) + (start - ref.Line)
		if end < start {
			end = start
		}
		if end > len(f.lines) {
			end = len(f.lines)
		}
	}
	return Range{Start: start, End: end}
}

// nearest picks the candidate closest to line; candidates are ascending, so a
// strict comparison keeps the earlier line on a tie.
func nearest(candidates []int, line int) int {
	best := candidates[0]
	for _, c := range candidates[1:] {
		if abs(c-line) < abs(best-line) {
			best = c
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func joinLines(lines []int) string {
	parts := make([]string, len(lines))
	for i, n := range lines {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}
