package render

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/secrets"
)

// Finalize rewrites op's raw backend output into anchored form, then caps it to
// a.Budget. Anchors (NN#hash) pin a span to its line's content so the model can
// echo it back into ccx_code_read even after edits shift line numbers. The
// rewrite is op-keyed: symbol output gains frame anchors, deps output
// regroups its "## Used by" rows under per-file "### path" headings with anchored
// line spans, search and related output is reshaped from raw semble JSON, read
// output is secret-masked before capping (unless a.RevealSecrets) with a footer
// naming the fired rules, and every other op passes through unchanged. Grep never
// reaches here — internal/ripgrep anchors and caps its own output. OpWebRead passes through too: web.Run
// applies its own content-aware budget+offset (byte-exact continuation) before
// Finalize sees it. Every other op caps through Cap. OpDiff must never reach here — the diff pipeline
// anchors its own output. The anchor.Files cache is built fresh per call (the MCP
// proxy is resident, so a cached line table would resolve against pre-edit content).
// Budget double-counting is accepted: a backend may have pre-capped and Cap may
// stack a second overflow footer.
func Finalize(op backend.Op, out string, a backend.Args) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("finalize: resolve cwd: %w", err)
	}
	files := anchor.NewFiles(cwd)

	switch op {
	case backend.OpSymbol:
		return Cap(terseSymbol(annotateSymbol(out, files), a), a.Budget), nil
	case backend.OpDeps:
		return Cap(annotateDeps(out, files), a.Budget), nil
	case backend.OpSearch, backend.OpRelated:
		reshaped, err := SembleResults(out, files)
		if err != nil {
			return "", err
		}
		return Cap(reshaped, a.Budget), nil
	case backend.OpRead:
		if a.RevealSecrets {
			return Cap(out, a.Budget), nil
		}
		masked, ids := secrets.Mask(out, a.Path)
		return withSecretsFooter(Cap(masked, a.Budget), ids), nil
	case backend.OpWebRead:
		return out, nil
	default:
		return Cap(out, a.Budget), nil
	}
}

// withSecretsFooter appends the one-line masked-secrets notice after capping, so
// it is never truncated away; ids are the fired rules in span order, deduped for
// the notice. No ids means no footer.
func withSecretsFooter(out string, ids []string) string {
	if len(ids) == 0 {
		return out
	}
	seen := make(map[string]bool, len(ids))
	var uniq []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return fmt.Sprintf("%s# %d secret(s) masked (%s) — --reveal-secrets prints raw\n", out, len(ids), strings.Join(uniq, ", "))
}

var (
	// grokHeaderRe matches the "[path:line]" locator in a grok header, splitting
	// at the last ':' before the line so a Windows path keeps its drive colon.
	grokHeaderRe = regexp.MustCompile(`\[([^\]]*):(\d+)\]`)
	// siblingHeadRe matches a "## siblings (path)" heading, capturing the path.
	siblingHeadRe = regexp.MustCompile(`^## siblings \((.+)\)$`)
	// siblingLineRe matches a sibling row "Name   [18-27]   signature": a name,
	// whitespace, then the range in brackets. The lazy name stops at the first
	// bracketed range, so an "[]string" in the trailing signature is untouched.
	siblingLineRe = regexp.MustCompile(`^(\S.*?\s+)\[(\d+)(?:-(\d+))?\](\s)`)
	// callerLineRe matches a caller row "    [43]   in Foo()": indent, then the
	// range in brackets.
	callerLineRe = regexp.MustCompile(`^(\s+)\[(\d+)(?:-(\d+))?\](\s)`)
)

// symbolSection tracks which grok section the walker is inside; only the
// siblings and callers sections carry anchorable line references.
type symbolSection int

const (
	sectionOther symbolSection = iota
	sectionSiblings
	sectionCallers
)

// annotateSymbol rewrites the anchorable locators in tilth grok/symbol output: the
// grok header "[file:67]", sibling ranges under a "## siblings (path)" heading,
// and caller lines under "## callers" (file from the preceding path row). It is a
// stateful walker so the "## body"/"## signature" sections — whose code may hold
// bracket syntax like "s[:cut]" — are never rewritten.
func annotateSymbol(out string, files *anchor.Files) string {
	lines := strings.Split(out, "\n")
	section := sectionOther
	var siblingFile, callerFile string
	for i, line := range lines {
		match := strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(match, "# grok:"):
			lines[i] = anchorGrokHeader(line, files)
			continue
		case siblingHeadRe.MatchString(match):
			siblingFile = siblingHeadRe.FindStringSubmatch(match)[1]
			section = sectionSiblings
			continue
		case strings.HasPrefix(match, "## callers"):
			section = sectionCallers
			callerFile = ""
			continue
		case strings.HasPrefix(match, "## "):
			section = sectionOther
			continue
		}
		switch section {
		case sectionSiblings:
			lines[i] = anchorFrameLine(line, siblingLineRe, siblingFile, files)
		case sectionCallers:
			if callerLineRe.MatchString(match) {
				lines[i] = anchorFrameLine(line, callerLineRe, callerFile, files)
			} else if trimmed := strings.TrimSpace(match); strings.HasPrefix(match, "  ") && trimmed != "" && !strings.HasPrefix(trimmed, "[") {
				callerFile = trimmed
			}
		}
	}
	return strings.Join(lines, "\n")
}

// anchorFrameLine rewrites the first bracketed range on line — matched by re,
// whose groups are (prefix, start, end?, trailing) — to carry a content anchor
// hashed from file's start line. An empty file, a non-matching line, or a cache
// miss returns line byte-identical.
func anchorFrameLine(line string, re *regexp.Regexp, file string, files *anchor.Files) string {
	if file == "" {
		return line
	}
	m := re.FindStringSubmatch(line)
	if m == nil {
		return line
	}
	start, _ := strconv.Atoi(m[2])
	text, ok := files.LineAt(file, start)
	if !ok {
		return line
	}
	span := m[2]
	if m[3] != "" {
		span += "-" + m[3]
	}
	return m[1] + "[" + span + "#" + string(anchor.Of(text)) + "]" + m[4] + line[len(m[0]):]
}

// anchorGrokHeader rewrites the "[path:line]" locator in a grok header to
// "[path:line#hash]", leaving it bare when the cache cannot resolve the line.
func anchorGrokHeader(line string, files *anchor.Files) string {
	return grokHeaderRe.ReplaceAllStringFunc(line, func(match string) string {
		m := grokHeaderRe.FindStringSubmatch(match)
		n, _ := strconv.Atoi(m[2])
		text, ok := files.LineAt(m[1], n)
		if !ok {
			return match
		}
		return fmt.Sprintf("[%s:%s#%s]", m[1], m[2], anchor.Of(text))
	})
}

// terseSymbolDefault selects the compact locate form of grok output — header,
// signature, and doc, with body/callers/callees/siblings/tests dropped to a counts
// trailer — when no expansion flag is set. It is the single default switch: the
// accuracy gate flips it to false to restore the rich default, leaving the flags
// intact.
const terseSymbolDefault = true

// sectionSpec is one optional grok section a flag can expand; list marks the
// sections whose count belongs in the terse trailer.
type sectionSpec struct {
	name string
	flag string
	list bool
}

// symbolSections orders the optional grok sections for the trailer.
var symbolSections = []sectionSpec{
	{"body", "--body", false},
	{"callers", "--callers", true},
	{"callees", "--callees", true},
	{"siblings", "--siblings", true},
	{"tests", "--tests", true},
}

// terseSymbol reshapes anchored grok output to the compact locate form: it keeps
// the header, signature, and doc, drops each optional section its flag does not
// keep (nor --full, nor a disabled terseSymbolDefault), and appends a trailer
// naming the dropped list sections' counts and the flags that expand them. Output
// without the "## signature" grammar — the ast-grep type fallback — passes through.
func terseSymbol(out string, a backend.Args) string {
	if a.Full || strings.Contains(out, "(ast-grep type fallback)") {
		return out
	}
	head, sections, ok := splitGrokSections(out)
	if !ok {
		return out
	}
	hasSig := false
	for _, s := range sections {
		if s.name == "signature" {
			hasSig = true
		}
	}
	kept := head
	dropped := map[string][]string{}
	for _, s := range sections {
		if symbolSectionKept(s.name, a) {
			kept = append(kept, s.lines...)
			continue
		}
		dropped[s.name] = s.lines
		// A class/type has no "## signature" section — its declaration is the first
		// line of the dropped body. Synthesize a signature so the terse form still
		// shows what the symbol is.
		if s.name == "body" && !hasSig {
			if decl := firstNonBlank(s.lines[1:]); decl != "" {
				kept = append(kept, "## signature", decl, "")
			}
		}
	}
	var counts, flags []string
	for _, spec := range symbolSections {
		lines, isDropped := dropped[spec.name]
		if !isDropped {
			continue
		}
		flags = append(flags, spec.flag)
		if spec.list {
			counts = append(counts, fmt.Sprintf("%s %d", spec.name, grokSectionCount(lines)))
		}
	}
	if len(flags) == 0 {
		return out
	}
	body := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	trailer := strings.Join(append(flags, "--full"), "/")
	if len(counts) > 0 {
		trailer = strings.Join(counts, " · ") + " — " + trailer
	}
	return body + "\n\n" + trailer + "\n"
}

// grokSection is one "## <name>" block of grok output, its header line included.
type grokSection struct {
	name  string
	lines []string
}

// splitGrokSections splits grok output into the header (lines before the first
// "## ") and the "## <name>" sections. ok is false when there are no sections —
// the ast-grep fallback shape terseSymbol leaves untouched.
func splitGrokSections(out string) (head []string, sections []grokSection, ok bool) {
	lines := strings.Split(out, "\n")
	first := -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, "## ") {
			first = i
			break
		}
	}
	if first < 0 {
		return nil, nil, false
	}
	head = lines[:first]
	for _, ln := range lines[first:] {
		if name, is := grokSectionName(ln); is {
			sections = append(sections, grokSection{name: name})
		}
		cur := &sections[len(sections)-1]
		cur.lines = append(cur.lines, ln)
	}
	return head, sections, true
}

// firstNonBlank returns the first line with non-space content, trimmed of a
// trailing carriage return, or "" when every line is blank.
func firstNonBlank(lines []string) string {
	for _, ln := range lines {
		if strings.TrimSpace(ln) != "" {
			return strings.TrimRight(ln, "\r")
		}
	}
	return ""
}

// grokSectionName returns the first word after "## " (e.g. "callees" from
// "## callees (1 internal, 4 extern)").
func grokSectionName(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "## ")
	if !ok {
		return "", false
	}
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		return rest[:i], true
	}
	return rest, true
}

// symbolSectionKept reports whether a section survives the terse filter: signature
// and doc always do, an optional section does when its flag is set or
// terseSymbolDefault is off, and an unrecognized section is kept rather than
// silently dropped.
func symbolSectionKept(name string, a backend.Args) bool {
	switch name {
	case "signature", "doc":
		return true
	case "body":
		return a.Body || !terseSymbolDefault
	case "callers":
		return a.Callers || !terseSymbolDefault
	case "callees":
		return a.Callees || !terseSymbolDefault
	case "siblings":
		return a.Siblings || !terseSymbolDefault
	case "tests":
		return a.Tests || !terseSymbolDefault
	default:
		return true
	}
}

// grokSectionCount reports how many items a dropped list section holds: the total
// from its "## name (…)" header when it carries numbers (the value after "of" in a
// "5 of 10" callers header, else the sum, so "1 internal, 4 extern" is 5), else
// the count of non-empty body rows (a siblings header names no number).
func grokSectionCount(lines []string) int {
	header := lines[0]
	if open := strings.IndexByte(header, '('); open >= 0 {
		if width := strings.IndexByte(header[open:], ')'); width >= 0 {
			inside := header[open+1 : open+width]
			if nums := grokInts(inside); len(nums) > 0 {
				if strings.Contains(inside, " of ") {
					return nums[len(nums)-1]
				}
				sum := 0
				for _, n := range nums {
					sum += n
				}
				return sum
			}
		}
	}
	rows := 0
	for _, ln := range lines[1:] {
		if strings.TrimSpace(ln) != "" {
			rows++
		}
	}
	return rows
}

// grokInts extracts the decimal integers embedded in s, in order.
func grokInts(s string) []int {
	var out []int
	for i := 0; i < len(s); {
		if s[i] < '0' || s[i] > '9' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		n, _ := strconv.Atoi(s[i:j])
		out = append(out, n)
		i = j
	}
	return out
}

var (
	// depsRowRe matches a tilth deps "## Used by" row "path:line<pad>name<pad>→ syms",
	// capturing path, line, the path-column pad (dropped), name, the name-column pad
	// (kept verbatim), and the trailing symbols verbatim. A "## Uses (local)" row has
	// no ":line" and no arrow, so it never matches.
	depsRowRe = regexp.MustCompile(`^(\S+):(\d+)(\s+)(\S+)(\s+)→ (.*)$`)
	// depsFooterRe matches the tilth token-count footer "[~198 tokens]".
	depsFooterRe = regexp.MustCompile(`^\[~\d+ tokens\]$`)
)

// depsSection tracks whether the walker is inside the "## Used by" block, the
// only deps section whose rows carry anchorable line references.
type depsSection int

const (
	depsOther depsSection = iota
	depsUsedBy
)

// depsGroup collects one dependent file's rewritten rows under its "### path"
// heading; both carry the original row's trailing "\r" when the input was CRLF.
type depsGroup struct {
	heading string
	rows    []string
}

// annotateDeps regroups a tilth deps "## Used by" block. Consecutive dependent
// rows accumulate into per-file groups — files in first-appearance order, a later
// row for an already-seen file joining that group — then flush as "### <path>"
// headings with content-anchored "L<line>#hash <name> → <syms>" spans (a row whose
// line the cache cannot resolve keeps a bare "L<line>"). Any line that is not a
// parseable dependent row flushes the accumulated groups and passes through verbatim
// at its original position, so a blank, prose, or space-path line between two rows
// of one file splits it into two groups with a repeated heading. A "## " heading,
// "... and N more" tail, or "[~NNN tokens]" footer also ends the block; a blank or
// prose line leaves it open so later rows regroup. Lines outside the block pass
// through byte-identical.
func annotateDeps(out string, files *anchor.Files) string {
	lines := strings.Split(out, "\n")
	result := make([]string, 0, len(lines))
	section := depsOther
	var order []string
	groups := map[string]*depsGroup{}
	flush := func() {
		for _, file := range order {
			g := groups[file]
			result = append(result, g.heading)
			result = append(result, g.rows...)
		}
		order, groups = nil, map[string]*depsGroup{}
	}
	for _, line := range lines {
		match := strings.TrimSuffix(line, "\r")
		if section == depsUsedBy {
			if m := depsRowRe.FindStringSubmatch(match); m != nil {
				suffix := ""
				if strings.HasSuffix(line, "\r") {
					suffix = "\r"
				}
				g, ok := groups[m[1]]
				if !ok {
					g = &depsGroup{heading: "### " + m[1] + suffix}
					groups[m[1]] = g
					order = append(order, m[1])
				}
				g.rows = append(g.rows, depsRow(m, suffix, files))
				continue
			}
			// A non-row line ends the current run of groupable rows: flush the
			// buffer, then emit the line verbatim in place. A heading, tail, or
			// footer also closes the block; a blank or prose line leaves it open.
			flush()
			if strings.HasPrefix(match, "## ") || strings.HasPrefix(match, "... and ") || depsFooterRe.MatchString(match) {
				section = depsOther
			}
		}
		if match == "## Used by" {
			section = depsUsedBy
		}
		result = append(result, line)
	}
	if section == depsUsedBy {
		flush()
	}
	return strings.Join(result, "\n")
}

// depsRow rewrites one "## Used by" row — matched by depsRowRe into (path, line,
// pathPad, name, namePad, symbols) — into "L<line>#hash <name><namePad>→ <symbols>",
// dropping the path column. The line's content anchors the span; a cache miss
// leaves it bare "L<line>". suffix carries the row's original "\r", if any.
func depsRow(m []string, suffix string, files *anchor.Files) string {
	n, _ := strconv.Atoi(m[2])
	span := "L" + m[2]
	if text, ok := files.LineAt(m[1], n); ok {
		span += "#" + string(anchor.Of(text))
	}
	return span + " " + m[4] + m[5] + "→ " + m[6] + suffix
}
