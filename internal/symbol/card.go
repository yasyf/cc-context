package symbol

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/ripgrep"
)

// card is the render-ready model of a resolved symbol: the header fields, the
// signature and doc, the terse counts, the requested expansions, and the
// disambiguation footer. Building it does the I/O (outline, ripgrep, anchoring);
// renderCard is pure over it.
type card struct {
	name, kind, loc string
	caseFolded      bool
	query           string
	signature       string
	doc             string

	terse                 bool
	refs, tests, siblings int

	showBody bool
	body     []string

	callers *refBlock

	showCallees bool
	callees     []string

	showSiblings bool
	siblingPath  string
	siblingRows  []string

	testBlock *refBlock

	also     []string
	alsoMore int
}

// refBlock is a rendered reference section: its label, the total references and
// files, the per-file row groups, and the omitted-remainder count.
type refBlock struct {
	label   string
	word    string
	total   int
	files   int
	groups  []refGroup
	omitted int
}

// refGroup is one file's reference rows under a "### path" heading.
type refGroup struct {
	path string
	rows []string
}

// buildCard assembles the card model for the top-ranked candidate: it scans
// references once (feeding the terse counts and any caller/test section), extracts
// the doc, and populates each requested expansion.
func (r *resolver) buildCard(cands []candidate, fold bool) (card, error) {
	top := cands[0]
	c := card{
		name:       top.name,
		kind:       top.kind,
		loc:        r.loc(top),
		caseFolded: fold,
		query:      r.a.Query,
		signature:  strings.TrimSuffix(top.signature, " {"),
		doc:        r.doc(top),
	}

	refs, err := r.refScan(top)
	if err != nil {
		return card{}, err
	}

	full := r.a.Full
	showBody := full || r.a.Body
	showCallers := full || r.a.Callers
	showCallees := full || r.a.Callees
	showSiblings := full || r.a.Siblings
	showTests := full || r.a.Tests
	anyExpansion := showBody || showCallers || showCallees || showSiblings || showTests

	if showBody {
		c.showBody = true
		c.body = r.bodyLines(top)
	}
	if showCallers {
		blk, err := r.refBlock("callers", top, refs)
		if err != nil {
			return card{}, err
		}
		c.callers = &blk
	}
	if showCallees {
		c.showCallees = true
		c.callees = r.callees(top)
	}
	if showSiblings {
		c.showSiblings = true
		c.siblingPath = top.path
		c.siblingRows = r.siblingRows(top)
	}
	if showTests {
		blk, err := r.refBlock("tests", top, filterTestRefs(refs))
		if err != nil {
			return card{}, err
		}
		c.testBlock = &blk
	}
	if !anyExpansion {
		c.terse = true
		c.refs = len(refs)
		c.tests = len(filterTestRefs(refs))
		c.siblings = len(r.siblingItems(top))
	}
	if len(cands) > 1 {
		c.also, c.alsoMore = r.alsoEntries(cands[1:])
	}
	return c, nil
}

// alsoCap bounds the disambiguation footer's inline entries before it collapses
// the remainder into a "+N more" count.
const alsoCap = 8

// alsoEntries formats the non-top candidates as "path:line#hash (kind)" entries,
// capped at alsoCap with the overflow returned as more.
func (r *resolver) alsoEntries(rest []candidate) (entries []string, more int) {
	for i, c := range rest {
		if i >= alsoCap {
			return entries, len(rest) - alsoCap
		}
		span := fmt.Sprintf("%d", c.start)
		if text, ok := r.files.LineAt(c.path, c.start); ok {
			span += "#" + anchor.Of(text).String()
		}
		entries = append(entries, fmt.Sprintf("%s:%s (%s)", c.path, span, c.kind))
	}
	return entries, 0
}

// loc renders the resolved symbol's "path:A-B#hash" locator, anchoring the span on
// its start line.
func (r *resolver) loc(top candidate) string {
	span := spanText(top.start, top.end)
	if text, ok := r.files.LineAt(top.path, top.start); ok {
		span += "#" + anchor.Of(text).String()
	}
	return top.path + ":" + span
}

// renderCard renders the card model to text: header, signature, doc, the ordered
// expansions, the terse counts trailer, and the disambiguation footer. It is pure.
func renderCard(c card) string {
	var b strings.Builder
	b.WriteString(cardHeader(c) + "\n")
	b.WriteString(c.signature + "\n")
	if c.doc != "" {
		b.WriteString("\n" + c.doc + "\n")
	}
	if c.showBody {
		b.WriteString("\n## body\n")
		for _, l := range c.body {
			b.WriteString(l + "\n")
		}
	}
	if c.callers != nil {
		b.WriteString(renderBlock(*c.callers))
	}
	if c.showCallees {
		b.WriteString("\n## calls (syntactic)\n")
		if len(c.callees) == 0 {
			b.WriteString("(none)\n")
		} else {
			b.WriteString(strings.Join(c.callees, " · ") + "\n")
		}
	}
	if c.showSiblings {
		fmt.Fprintf(&b, "\n## siblings (%s)\n", c.siblingPath)
		for _, row := range c.siblingRows {
			b.WriteString(row + "\n")
		}
	}
	if c.testBlock != nil {
		b.WriteString(renderBlock(*c.testBlock))
	}
	if c.terse {
		fmt.Fprintf(&b, "\nrefs %d · tests %d · siblings %d — --callers/--tests/--siblings/--body/--full\n",
			c.refs, c.tests, c.siblings)
	}
	if len(c.also) > 0 {
		b.WriteString(alsoLine(c) + "\n")
	}
	return b.String()
}

// cardHeader renders the "# symbol …" header, disclosing a case-insensitive
// resolution and the original query when the exact pass missed.
func cardHeader(c card) string {
	h := fmt.Sprintf("# symbol %s — %s — %s", c.name, c.kind, c.loc)
	if c.caseFolded {
		h += fmt.Sprintf(" (case-insensitive: queried %q)", c.query)
	}
	return h
}

// alsoLine renders the disambiguation footer.
func alsoLine(c card) string {
	line := "also defined: " + strings.Join(c.also, " · ")
	if c.alsoMore > 0 {
		line += fmt.Sprintf(" · (+%d more)", c.alsoMore)
	}
	return line + " — narrow with --scope"
}

// renderBlock renders a reference section: a "## label (word refs — N in K files)"
// header, then "### path" groups, then the omitted-remainder disclosure.
func renderBlock(rb refBlock) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n## %s (word refs — %d in %d files)\n", rb.label, rb.total, rb.files)
	for _, g := range rb.groups {
		b.WriteString("### " + g.path + "\n")
		for _, row := range g.rows {
			b.WriteString(row + "\n")
		}
	}
	if rb.omitted > 0 {
		fmt.Fprintf(&b, "… +%d more refs — ccx code grep -w %s\n", rb.omitted, rb.word)
	}
	return b.String()
}

// degradedKeywords is the definition-keyword alternation the degraded scan
// anchors ahead of the query name.
const degradedKeywords = `func|def|class|fn|type|const|var|let|interface|struct|impl|module|trait`

// degraded is the miss ladder's last non-error rung: it regex-scans for
// definition-shaped text mentioning the name — the lane for a symbol with no
// outline rules — and renders anchored rows, returning "" when nothing matches.
func (r *resolver) degraded() (string, error) {
	pattern := `^\s*(` + degradedKeywords + `)\b.*\b` + regexp.QuoteMeta(r.name) + `\b`
	fms, err := ripgrep.Matches(r.ctx, backend.Args{Query: pattern, Regex: true, Scope: r.refScope})
	if err != nil {
		return "", fmt.Errorf("symbol: degraded scan %q: %w", r.name, err)
	}
	var rows []ref
	for _, fm := range fms {
		for _, l := range fm.Lines {
			if l.IsMatch {
				rows = append(rows, ref{path: fm.Path, line: l.Num, text: l.Text})
			}
		}
	}
	if len(rows) == 0 {
		return "", nil
	}
	groups, omitted := r.degradedGroups(rows)
	return renderDegraded(r.name, groups, omitted), nil
}

// degradedGroups buckets the degraded rows per file and anchors each, capping to
// rowsPerFile rows and filesShown files and returning the omitted remainder.
func (r *resolver) degradedGroups(rows []ref) (groups []refGroup, omitted int) {
	grouped := groupRefs(rows)
	order := orderRefFiles(grouped, "")
	shown := 0
	for i, p := range order {
		if i >= filesShown {
			break
		}
		g := refGroup{path: p}
		for j, rf := range grouped[p] {
			if j >= rowsPerFile {
				break
			}
			g.rows = append(g.rows, fmt.Sprintf("[%s] %s", r.anchoredLine(p, rf.line), strings.TrimSpace(rf.text)))
			shown++
		}
		groups = append(groups, g)
	}
	return groups, len(rows) - shown
}

// renderDegraded renders the degraded result: the no-structural-definition header,
// the anchored "### path" groups, and the omitted-remainder disclosure. It is pure.
func renderDegraded(name string, groups []refGroup, omitted int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# symbol %s — no structural definition (not in ast-grep outline); definition-shaped text matches:\n", name)
	for _, g := range groups {
		b.WriteString("### " + g.path + "\n")
		for _, row := range g.rows {
			b.WriteString(row + "\n")
		}
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "… +%d more — ccx code grep %s\n", omitted, name)
	}
	return b.String()
}
