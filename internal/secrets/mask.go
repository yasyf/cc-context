package secrets

import (
	"path/filepath"
	"sort"
	"strings"
)

// genericAPIKeyRule is gitleaks' entropy catch-all, too noisy for ordinary
// source; Mask applies it only to env-shaped paths.
const genericAPIKeyRule = "generic-api-key"

// span is a half-open [start, end) byte range slated for masking.
type span struct {
	start, end int
	ruleID     string
}

// Mask replaces every detected secret in text with a masked placeholder and
// returns the fired rule ids in span order, one per masked span. path scopes the
// generic-api-key rule to env-shaped files; every other rule applies to all
// paths. A span of at least 16 bytes keeps its first 4 bytes
// ("AKIA…[masked:aws-access-token]"); a shorter span masks whole. Text with no
// findings is returned unchanged.
func Mask(text, path string) (string, []string) {
	spans := findSpans(text, path)
	if len(spans) == 0 {
		return text, nil
	}
	ids := make([]string, len(spans))
	var b strings.Builder
	prev := 0
	for i, s := range spans {
		ids[i] = s.ruleID
		b.WriteString(text[prev:s.start])
		b.WriteString(replacement(text[s.start:s.end], s.ruleID))
		prev = s.end
	}
	b.WriteString(text[prev:])
	return b.String(), ids
}

// MaskedLine is one output line of MaskLines: its masked text, the 0-based
// index of the input line it began as, and the rule ids of the masked spans
// that begin on it.
type MaskedLine struct {
	Text  string
	Src   int
	Rules []string
}

// MaskLines masks lines joined as one text — so a multiline rule (private-key)
// fires across them — and re-splits the result. A masked span that swallows
// line breaks folds the swallowed lines into the span's first: each output
// line records the input line it began as, so a caller keeping per-line
// bookkeeping (grep frames, an outline window) can re-map. Lines with no
// findings come back unchanged.
func MaskLines(lines []string, path string) []MaskedLine {
	text := strings.Join(lines, "\n")
	spans := findSpans(text, path)
	out := make([]MaskedLine, 0, len(lines))
	if len(spans) == 0 {
		for i, l := range lines {
			out = append(out, MaskedLine{Text: l, Src: i})
		}
		return out
	}
	var buf strings.Builder
	inLine := 0
	cur := MaskedLine{Src: 0}
	emitRaw := func(seg string) {
		for {
			i := strings.IndexByte(seg, '\n')
			if i < 0 {
				buf.WriteString(seg)
				return
			}
			buf.WriteString(seg[:i])
			cur.Text = buf.String()
			out = append(out, cur)
			buf.Reset()
			inLine++
			cur = MaskedLine{Src: inLine}
			seg = seg[i+1:]
		}
	}
	prev := 0
	for _, s := range spans {
		emitRaw(text[prev:s.start])
		cur.Rules = append(cur.Rules, s.ruleID)
		buf.WriteString(replacement(text[s.start:s.end], s.ruleID))
		inLine += strings.Count(text[s.start:s.end], "\n")
		prev = s.end
	}
	emitRaw(text[prev:])
	cur.Text = buf.String()
	return append(out, cur)
}

// findSpans collects each rule's match spans across text, then folds overlaps
// via mergeSpans. Every span is edge-trimmed of line breaks; a whole-match span
// (no secretGroup) additionally sheds the trailing delimiter its rule swallowed.
func findSpans(text, path string) []span {
	lower := strings.ToLower(text)
	var spans []span
	for _, r := range rules() {
		if r.id == genericAPIKeyRule && !envShaped(path) {
			continue
		}
		if !keywordHit(r, lower) {
			continue
		}
		for _, m := range r.re.FindAllStringSubmatchIndex(text, -1) {
			start, end, whole := secretSpan(m, r.secretGroup)
			start, end = trimEdges(text, start, end, whole)
			if start >= end {
				continue
			}
			if r.entropy > 0 && shannonEntropy(text[start:end]) <= r.entropy {
				continue
			}
			spans = append(spans, span{start: start, end: end, ruleID: r.id})
		}
	}
	if len(spans) == 0 {
		return nil
	}
	return mergeSpans(spans)
}

// mergeSpans sorts spans by start (the longer span first on a tie) and folds
// overlaps: a span starting inside its predecessor extends that predecessor's
// end to the farther of the two — so a staggered tail is never dropped — and the
// earlier-start span's rule id labels the merged mask. spans must be non-empty.
func mergeSpans(spans []span) []span {
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	kept := spans[:1]
	for _, s := range spans[1:] {
		last := &kept[len(kept)-1]
		if s.start < last.end {
			if s.end > last.end {
				last.end = s.end
			}
			continue
		}
		kept = append(kept, s)
	}
	return kept
}

// secretSpan picks the byte range to mask from a FindAllStringSubmatchIndex row:
// the secretGroup submatch when set and matched, else the whole match. whole
// reports the latter — its trailing boundary bytes are trimEdges' to shed.
// loadRules validates secretGroup against the pattern, so only the optional
// group's participation (m[2*group] >= 0) is checked here.
func secretSpan(m []int, group int) (start, end int, whole bool) {
	if g := 2 * group; group > 0 && m[g] >= 0 {
		return m[g], m[g+1], false
	}
	return m[0], m[1], true
}

// trimEdges shrinks [start, end) past the leading and trailing line breaks
// gitleaks excludes from the secret (masking one would reflow the surrounding
// lines). For a whole-match span it also trims a trailing quote, backtick,
// semicolon, or whitespace the rule's boundary class swallowed past the secret,
// so masking a delimited match (gcp-api-key's "AIza…") keeps its closing quote.
func trimEdges(text string, start, end int, whole bool) (int, int) {
	for start < end && (text[start] == '\n' || text[start] == '\r') {
		start++
	}
	for end > start && (text[end-1] == '\n' || text[end-1] == '\r') {
		end--
	}
	if whole {
		for end > start && trailingBoundary(text[end-1]) {
			end--
		}
	}
	return start, end
}

// trailingBoundary reports whether b is a delimiter a whole-match rule may
// swallow past the secret: a quote, backtick, semicolon, or whitespace.
func trailingBoundary(b byte) bool {
	switch b {
	case '`', '\'', '"', ';', ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// keywordHit reports whether any of r's keywords appear in the lowercased
// text; a rule without keywords always applies.
func keywordHit(r rule, lower string) bool {
	if len(r.keywords) == 0 {
		return true
	}
	for _, kw := range r.keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// replacement masks one secret span: a span of at least 16 bytes keeps its first
// 4 bytes as an identifying stub; a shorter span masks whole, since a 4-byte stub
// would leak up to a quarter of a 9–12 byte secret.
func replacement(secret, ruleID string) string {
	if len(secret) >= 16 {
		return secret[:4] + "…[masked:" + ruleID + "]"
	}
	return "[masked:" + ruleID + "]"
}

// envShaped reports whether path's base name marks a credentials-style file:
// .env, .env.*, *.env, .envrc, credentials, or .netrc.
func envShaped(path string) bool {
	base := filepath.Base(path)
	switch base {
	case ".env", ".envrc", "credentials", ".netrc":
		return true
	}
	return strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env")
}
