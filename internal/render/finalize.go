package render

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/backend"
)

// Finalize rewrites op's raw backend output into anchored form, then caps it to
// budget. Anchors (NN#hash) pin a span to its line's content so the model can
// echo it back into ccx_code_read even after edits shift line numbers. The
// rewrite is op-keyed: grep and symbol output gain frame anchors, search and
// related output is reshaped from raw semble JSON, and every other op passes
// through unchanged. OpDiff must never reach here — the diff pipeline anchors
// its own output. The anchor.Files cache is built fresh per call (the MCP proxy
// is resident, so a cached line table would resolve against pre-edit content).
// Budget double-counting is accepted: a backend may have pre-capped and Cap may
// stack a second overflow footer.
func Finalize(op backend.Op, out string, budget int) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("finalize: resolve cwd: %w", err)
	}
	files := anchor.NewFiles(cwd)

	switch op {
	case backend.OpGrep:
		return Cap(annotateGrep(out, files), budget), nil
	case backend.OpSymbol:
		return Cap(annotateSymbol(out, files), budget), nil
	case backend.OpSearch, backend.OpRelated:
		reshaped, err := SembleResults(out, files)
		if err != nil {
			return "", err
		}
		return Cap(reshaped, budget), nil
	default:
		return Cap(out, budget), nil
	}
}

var (
	// grepSectionRe matches a tilth grep "### path:64,75 [desc]" section header,
	// capturing the path. The lazy path stops at the last ':' before the line
	// list, so a Windows drive letter (never followed by a digit) stays in it.
	grepSectionRe = regexp.MustCompile(`^### (.+?):\d[\d,]*(?:[ \t].*)?$`)
	// grepFenceRe matches a tilth grep fenced-block header "```path:1-88",
	// capturing the path. Backticks force a double-quoted pattern.
	grepFenceRe = regexp.MustCompile("^```(.+?):\\d+(?:-\\d+)?$")
	// grepFrameRe matches a grep frame line "  [55-66] " or "→ [55] ": indent and
	// optional arrow, a numeric line or range in brackets, then a space. An open
	// range "[4-]" has no closing digit before ']', so it never matches.
	grepFrameRe = regexp.MustCompile(`^(\s*→?\s*)\[(\d+)(?:-(\d+))?\](\s)`)
)

// annotateGrep rewrites tilth grep frame lines to carry a content anchor,
// tracking the current file from "### path:…" and "```path:…" headers. Header,
// gutter ("NN │"), and open-range lines never match grepFrameRe, so they pass
// through byte-identical; a frame whose file or line the cache cannot resolve
// stays bare.
func annotateGrep(out string, files *anchor.Files) string {
	lines := strings.Split(out, "\n")
	var file string
	for i, line := range lines {
		match := strings.TrimSuffix(line, "\r")
		if m := grepSectionRe.FindStringSubmatch(match); m != nil {
			file = m[1]
			continue
		}
		if m := grepFenceRe.FindStringSubmatch(match); m != nil {
			file = m[1]
			continue
		}
		lines[i] = anchorFrameLine(line, grepFrameRe, file, files)
	}
	return strings.Join(lines, "\n")
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
