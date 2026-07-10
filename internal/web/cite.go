package web

import (
	"fmt"
	"regexp"
	"strings"
)

// citeSep separates the normalized URL from the section reference in a cite:
// "<url> §2.3#k7fq". The section marker is a literal U+00A7.
const citeSep = " §"

// Cite is a parsed reference to one chunk of a page: the normalized page URL,
// the chunk's section ID, and the chunk's content hash. A search hit prints a
// Cite so it can be echoed back into `ccx web read --section <sec>#<hash>`.
type Cite struct {
	URL     string
	Section string
	Hash    string
}

// FormatCite renders a chunk reference as "<url> §<section>#<hash>". It is the
// exact form ParseCite reads back.
func FormatCite(url, section, hash string) string {
	return fmt.Sprintf("%s%s%s#%s", url, citeSep, section, hash)
}

// ParseCite splits a "<url> §<section>#<hash>" cite. A leading "§" on the
// section portion is tolerated so a hand-copied "§2.3#k7fq" reference parses,
// and the section may itself be empty (the preamble is "0", never blank). It
// errors when the URL/section or section/hash separators are missing.
func ParseCite(s string) (Cite, error) {
	i := strings.Index(s, citeSep)
	if i < 0 {
		return Cite{}, fmt.Errorf("invalid cite %q: want %q", s, "<url> §<section>#<hash>")
	}
	url := s[:i]
	sec, hash, err := splitSectionRef(s[i+len(citeSep):])
	if err != nil {
		return Cite{}, fmt.Errorf("invalid cite %q: %w", s, err)
	}
	return Cite{URL: url, Section: sec, Hash: hash}, nil
}

// splitSectionRef splits a "<section>#<hash>" reference — the portion a
// `--section` flag carries — into its section ID and 4-char hash, stripping a
// leading "§". A bare section ID (no "#") returns an empty hash.
func splitSectionRef(ref string) (section, hash string, err error) {
	ref = strings.TrimPrefix(ref, "§")
	i := strings.LastIndexByte(ref, '#')
	if i < 0 {
		return ref, "", nil
	}
	section, hash = ref[:i], ref[i+1:]
	if hash == "" {
		return "", "", fmt.Errorf("section ref %q: empty hash after #", ref)
	}
	return section, hash, nil
}

// DriftedCiteError reports that a cite's hash no longer resolves to a single
// chunk on the page: the content it pinned was removed or rewritten, or the same
// hash now matches chunks in more than one distinct section. It names the nearest
// surviving section — or, when ambiguous, the candidate sections — so the caller
// can re-orient with `ccx web outline`.
type DriftedCiteError struct {
	Section    string   // the cited section, now unresolvable by hash
	Hash       string   // the cited hash
	Nearest    string   // the nearest section ID that still exists
	Candidates []string // when non-empty, the distinct sections the hash matches: the ref is ambiguous
}

func (e *DriftedCiteError) Error() string {
	if len(e.Candidates) > 0 {
		ids := make([]string, len(e.Candidates))
		for i, s := range e.Candidates {
			ids[i] = "§" + s
		}
		return fmt.Sprintf("cite §%s#%s is ambiguous: that hash matches chunks in sections %s; re-run ccx web outline to pick one", e.Section, e.Hash, strings.Join(ids, ", "))
	}
	return fmt.Sprintf("cite §%s#%s drifted: no chunk carries that hash anymore; nearest surviving section is §%s (re-run ccx web outline)", e.Section, e.Hash, e.Nearest)
}

// Resolve locates the chunk a cite points at. An exact hit — a chunk in the
// cited section carrying the hash — resolves silently. When the section moved but
// the content survives in a single section, a hash scan re-anchors the cite and
// returns the chunk at its actual current section. When the hash is gone
// entirely, or matches chunks across more than one distinct section, it returns a
// *DriftedCiteError — naming the nearest surviving section, or the ambiguous
// candidates.
func Resolve(page *Page, section, hash string) (Chunk, error) {
	for _, c := range page.Chunks {
		if c.Section == section && c.Hash == hash {
			return c, nil
		}
	}
	var matches []Chunk
	var candidates []string
	seen := map[string]bool{}
	for _, c := range page.Chunks {
		if c.Hash != hash {
			continue
		}
		matches = append(matches, c)
		if !seen[c.Section] {
			seen[c.Section] = true
			candidates = append(candidates, c.Section)
		}
	}
	switch {
	case len(matches) == 0:
		return Chunk{}, &DriftedCiteError{Section: section, Hash: hash, Nearest: nearestSection(page, section)}
	case len(candidates) > 1:
		return Chunk{}, &DriftedCiteError{Section: section, Hash: hash, Nearest: nearestSection(page, section), Candidates: candidates}
	default:
		return matches[0], nil
	}
}

// printedNumberRe matches a document's own printed section number at the head of
// a Section.Title — "5.6.7. Date/Time Formats", "1) Overview" — capturing the
// dotted numeral. It is distinct from the chunker's §id: a title numbers itself.
var printedNumberRe = regexp.MustCompile(`^(\d+(?:\.\d+)*)[.)]?(?:\s|$)`)

// sectionIDRe matches a bare chunker §id shape: a dotted numeral with no trailing
// punctuation ("2", "2.3.1").
var sectionIDRe = regexp.MustCompile(`^\d+(\.\d+)*$`)

// inlineLinkRe matches a markdown inline link "[text](target)", capturing the
// visible text; the target may carry one level of nested parentheses (a URL ending
// in "(y)") or a backslash-escaped paren (a Wikipedia-style "\(" / "\)") so the
// match spans the whole target, not up to the first ")".
var inlineLinkRe = regexp.MustCompile(`\[([^\]]*)\]\((?:[^()\\]|\\.|\([^()]*\))*\)`)

// refLinkRe matches a reference-style markdown link "[text][label]", capturing the
// visible text so plainTitle can unwrap it before stray brackets are stripped.
var refLinkRe = regexp.MustCompile(`\[([^\]]*)\]\[[^\]]*\]`)

// plainTitle strips the markdown markup a heading title may carry so a printed
// number at its head matches and the resolve note reads cleanly: it unwraps inline
// links "[text](target)" and reference-style links "[text][label]" to their text,
// drops emphasis and code markers (*, _, and backtick), removes stray brackets, and
// collapses whitespace runs.
func plainTitle(title string) string {
	title = inlineLinkRe.ReplaceAllString(title, "$1")
	title = refLinkRe.ReplaceAllString(title, "$1")
	title = strings.NewReplacer("*", "", "_", "", "`", "", "[", "", "]", "").Replace(title)
	return strings.Join(strings.Fields(title), " ")
}

// resolvePrintedNumber returns every section whose title opens with the printed
// number input — the document's own heading numeral, which the chunker's §ids do
// not follow. input is trimmed of one trailing "." or ")" so "5.6.7." resolves
// like "5.6.7".
func resolvePrintedNumber(sections []Section, input string) []Section {
	if n := len(input); n > 0 && (input[n-1] == '.' || input[n-1] == ')') {
		input = input[:n-1]
	}
	var matches []Section
	for _, s := range sections {
		if m := printedNumberRe.FindStringSubmatch(plainTitle(s.Title)); m != nil && m[1] == input {
			matches = append(matches, s)
		}
	}
	return matches
}

// looksLikeSectionID reports whether input has the shape of a chunker §id, so a
// not-found miss can offer the nearest surviving section only for a §id-shaped
// input, not a stray heading-text fragment.
func looksLikeSectionID(input string) bool {
	return sectionIDRe.MatchString(input)
}

// nearestSection returns the surviving section ID closest to section: the
// section itself when it still exists, else its deepest surviving ancestor along
// the dotted path ("2.3" -> "2" -> "0"), else the page's first section.
func nearestSection(page *Page, section string) string {
	present := make(map[string]bool, len(page.Sections))
	for _, s := range page.Sections {
		present[s.ID] = true
	}
	for cur := section; cur != ""; {
		if present[cur] {
			return cur
		}
		i := strings.LastIndexByte(cur, '.')
		if i < 0 {
			break
		}
		cur = cur[:i]
	}
	if len(page.Sections) > 0 {
		return page.Sections[0].ID
	}
	return ""
}
