package symbol

import "strings"

// doc extracts the resolved symbol's documentation and reduces it: the first
// paragraph in terse mode, every paragraph joined in --full mode. An absent doc
// is the empty string.
func (r *resolver) doc(top candidate) string {
	lines := r.fileLines(top.path)
	if lines == nil {
		return ""
	}
	paras := extractDoc(lines, top.start, r.langOf(top.path))
	if len(paras) == 0 {
		return ""
	}
	if r.a.Full {
		return strings.Join(paras, "\n\n")
	}
	return paras[0]
}

// langOf returns the ast-grep language of path from the scope outline, empty when
// the scope did not cover it.
func (r *resolver) langOf(path string) string {
	if f, ok := r.defFile(path); ok {
		return f.Language
	}
	return ""
}

// extractDoc pulls a symbol's doc as paragraphs: a Python def or class takes its
// body docstring, falling back to leading "#" comments; every other language
// walks the comment lines immediately above the definition — a "/* */" block or a
// run of "//"/"///" lines.
func extractDoc(lines []string, startLine int, lang string) []string {
	if strings.EqualFold(lang, "python") {
		if ds := pythonDocstring(lines, startLine); len(ds) > 0 {
			return paragraphs(ds)
		}
		return paragraphs(lineCommentUp(lines, startLine-2, []string{"#"}))
	}
	above := startLine - 2 // 0-based index of the line directly above the definition
	if above < 0 || above >= len(lines) {
		return nil
	}
	if strings.HasSuffix(strings.TrimSpace(stripCR(lines[above])), "*/") {
		return paragraphs(blockUp(lines, above))
	}
	return paragraphs(lineCommentUp(lines, above, []string{"///", "//"}))
}

// lineCommentUp walks upward from index i collecting a contiguous run of line
// comments (matched by prefixes, longest first), each stripped of its marker and
// one leading space, returned top-to-bottom. It stops at the first non-comment.
func lineCommentUp(lines []string, i int, prefixes []string) []string {
	var rev []string
	for ; i >= 0; i-- {
		text, ok := stripComment(strings.TrimSpace(stripCR(lines[i])), prefixes)
		if !ok {
			break
		}
		rev = append(rev, text)
	}
	return reversed(rev)
}

// stripComment strips the first matching line-comment prefix and one leading
// space; ok is false when no prefix matches.
func stripComment(line string, prefixes []string) (string, bool) {
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(line, p); ok {
			return strings.TrimPrefix(rest, " "), true
		}
	}
	return "", false
}

// blockUp gathers a "/* … */" block ending at index end, walking up to the line
// opening it and cleaning each line of its "/*", "*/", and leading "*" markers.
// Empty lines are dropped so paragraph splitting stays meaningful.
func blockUp(lines []string, end int) []string {
	var rev []string
	for i := end; i >= 0; i-- {
		t := strings.TrimSpace(stripCR(lines[i]))
		if clean := cleanBlockLine(t); clean != "" {
			rev = append(rev, clean)
		}
		if strings.Contains(t, "/*") {
			break
		}
	}
	return reversed(rev)
}

// cleanBlockLine strips a block comment line's "*/" suffix, "/**"/"/*" prefix, and
// leading "*".
func cleanBlockLine(t string) string {
	t = strings.TrimSpace(strings.TrimSuffix(t, "*/"))
	if rest, ok := strings.CutPrefix(t, "/**"); ok {
		t = rest
	} else if rest, ok := strings.CutPrefix(t, "/*"); ok {
		t = rest
	}
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "*")
	return strings.TrimSpace(t)
}

// pythonDocstring extracts the triple-quoted docstring that opens a def or class
// body: it scans from the line after the definition to the first non-blank line
// and, when that line opens a triple-quoted string (either quote), collects
// through the close. A body that does not open with a docstring yields nothing.
func pythonDocstring(lines []string, startLine int) []string {
	for i := startLine; i < len(lines); i++ {
		t := strings.TrimSpace(stripCR(lines[i]))
		if t == "" {
			continue
		}
		quote := ""
		switch {
		case strings.HasPrefix(t, `"""`):
			quote = `"""`
		case strings.HasPrefix(t, "'''"):
			quote = "'''"
		default:
			return nil
		}
		rest := t[3:]
		if j := strings.Index(rest, quote); j >= 0 {
			return []string{strings.TrimSpace(rest[:j])}
		}
		out := []string{}
		if s := strings.TrimSpace(rest); s != "" {
			out = append(out, s)
		}
		for j := i + 1; j < len(lines); j++ {
			l := strings.TrimSpace(stripCR(lines[j]))
			if k := strings.Index(l, quote); k >= 0 {
				if s := strings.TrimSpace(l[:k]); s != "" {
					out = append(out, s)
				}
				return out
			}
			out = append(out, l)
		}
		return out
	}
	return nil
}

// paragraphs splits doc lines into paragraphs on blank lines, joining each
// paragraph's lines with a newline.
func paragraphs(lines []string) []string {
	var paras []string
	var cur []string
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			if len(cur) > 0 {
				paras = append(paras, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, l)
	}
	if len(cur) > 0 {
		paras = append(paras, strings.Join(cur, "\n"))
	}
	return paras
}

// stripCR trims a single trailing carriage return so a CRLF file is read like an
// LF file.
func stripCR(s string) string {
	return strings.TrimSuffix(s, "\r")
}

// reversed returns s reversed, so an upward-collected comment run reads top down.
func reversed(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}
