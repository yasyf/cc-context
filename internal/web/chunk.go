package web

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"github.com/yasyf/cc-context/internal/anchor"
)

// maxChunkTokens is the leaf-chunk ceiling: a chunk never estimates over this
// many tokens (768 tokens ≈ 3072 bytes under estimateTokens).
const maxChunkTokens = 768

// estimateTokens approximates the token count of s. It is intentionally the one
// small chars-per-token function in the package so the estimator is swappable in
// a single place (the phase-1 spike target); it mirrors render.Cap's ratio.
func estimateTokens(s string) int {
	return len(s) / 4
}

// ChunkPage parses page markdown into its heading-tree Sections and a
// token-bounded sequence of Chunks. Every byte of markdown belongs to exactly
// one Chunk: concatenating the Chunks' spans in document order reproduces
// markdown exactly, and each heading line falls in its section's first chunk.
func ChunkPage(markdown string) ([]Section, []Chunk) {
	src := []byte(markdown)
	headings, codeRanges := parseStructure(src)
	sections := buildSections(markdown, headings)
	chunks := buildChunks(markdown, sections, codeRanges)
	return sections, chunks
}

// headingInfo is one heading's level, title, and the byte offset of its line
// start (the '#' or the setext text line, never the text after the '#').
type headingInfo struct {
	level     int
	title     string
	lineStart int
}

// byteRange is a half-open [start, stop) span into the page markdown.
type byteRange struct {
	start int
	stop  int
}

// parseStructure walks the goldmark AST to collect headings (in document order,
// with line-start offsets) and the content ranges of fenced and indented code
// blocks, whose interior blank lines must not split a chunk.
func parseStructure(src []byte) ([]headingInfo, []byteRange) {
	doc := goldmark.New().Parser().Parse(text.NewReader(src))
	var headings []headingInfo
	var codeRanges []byteRange
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Heading:
			lines := v.Lines()
			if lines.Len() == 0 {
				// A bare "##" heading carries no title and no navigable body;
				// leave its line as ordinary content of the preceding section.
				return ast.WalkContinue, nil
			}
			seg := lines.At(0)
			start := seg.Start
			for start > 0 && src[start-1] != '\n' {
				start--
			}
			headings = append(headings, headingInfo{
				level:     v.Level,
				title:     strings.TrimSpace(string(lines.Value(src))),
				lineStart: start,
			})
		case *ast.FencedCodeBlock:
			if r, ok := codeContentRange(v.Lines()); ok {
				codeRanges = append(codeRanges, r)
			}
		case *ast.CodeBlock:
			if r, ok := codeContentRange(v.Lines()); ok {
				codeRanges = append(codeRanges, r)
			}
		}
		return ast.WalkContinue, nil
	})
	return headings, codeRanges
}

// codeContentRange is the byte span of a code block's content lines (the fence
// markers are non-blank, so protecting the content alone shields every interior
// blank line). It reports ok=false for an empty block, which has no interior.
func codeContentRange(lines *text.Segments) (byteRange, bool) {
	if lines.Len() == 0 {
		return byteRange{}, false
	}
	return byteRange{start: lines.At(0).Start, stop: lines.At(lines.Len() - 1).Stop}, true
}

// buildSections turns the heading list into the dotted-path section tree. The
// content before the first heading (or the whole document when it has none) is
// the preamble section "0" at level 0; each heading's section runs from its line
// start to the next heading of any level, so the sections partition the markdown.
func buildSections(markdown string, headings []headingInfo) []Section {
	var sections []Section

	firstStart := len(markdown)
	if len(headings) > 0 {
		firstStart = headings[0].lineStart
	}
	if firstStart > 0 {
		sections = append(sections, Section{ID: "0", Level: 0, Start: 0, End: firstStart})
	}

	type frame struct {
		level      int
		id         string
		childCount int
	}
	var stack []frame
	rootCount := 0

	for i, h := range headings {
		for len(stack) > 0 && stack[len(stack)-1].level >= h.level {
			stack = stack[:len(stack)-1]
		}
		var id, parent string
		if len(stack) == 0 {
			rootCount++
			id = strconv.Itoa(rootCount)
		} else {
			last := len(stack) - 1
			stack[last].childCount++
			parent = stack[last].id
			id = parent + "." + strconv.Itoa(stack[last].childCount)
		}
		stack = append(stack, frame{level: h.level, id: id})

		end := len(markdown)
		if i+1 < len(headings) {
			end = headings[i+1].lineStart
		}
		sections = append(sections, Section{
			ID:     id,
			Level:  h.level,
			Title:  h.title,
			Parent: parent,
			Start:  h.lineStart,
			End:    end,
		})
	}
	return sections
}

// buildChunks packs each section's blocks into token-bounded chunks in document
// order, tagging every chunk with its section ID and breadcrumb.
func buildChunks(markdown string, sections []Section, codeRanges []byteRange) []Chunk {
	byID := make(map[string]Section, len(sections))
	for _, s := range sections {
		byID[s.ID] = s
	}
	var chunks []Chunk
	for _, sec := range sections {
		crumb := breadcrumb(byID, sec)
		blocks := splitBlocks(markdown, sec.Start, sec.End, codeRanges)
		packChunks(markdown, blocks, sec.ID, crumb, &chunks)
	}
	return chunks
}

// breadcrumb is the " > "-joined heading titles from the root section down to
// sec. The preamble (level 0) has no breadcrumb.
func breadcrumb(byID map[string]Section, sec Section) string {
	if sec.Level == 0 {
		return ""
	}
	var titles []string
	for cur := sec; ; {
		titles = append(titles, cur.Title)
		if cur.Parent == "" {
			break
		}
		cur = byID[cur.Parent]
	}
	for i, j := 0, len(titles)-1; i < j; i, j = i+1, j-1 {
		titles[i], titles[j] = titles[j], titles[i]
	}
	return strings.Join(titles, " > ")
}

// splitBlocks partitions markdown[start:stop) into contiguous blocks divided by
// blank lines that lie outside any code range. Each block absorbs the blank
// line(s) that follow it, so the blocks concatenate back to the exact span.
func splitBlocks(markdown string, start, stop int, codeRanges []byteRange) []byteRange {
	var blocks []byteRange
	blockStart := start
	i := start
	for i < stop {
		lineEnd := lineEndAt(markdown, i, stop)
		if isBlankLine(markdown[i:lineEnd]) && !inCodeRange(i, codeRanges) {
			j := lineEnd
			for j < stop {
				le := lineEndAt(markdown, j, stop)
				if !isBlankLine(markdown[j:le]) || inCodeRange(j, codeRanges) {
					break
				}
				j = le
			}
			blocks = append(blocks, byteRange{blockStart, j})
			blockStart = j
			i = j
			continue
		}
		i = lineEnd
	}
	if blockStart < stop {
		blocks = append(blocks, byteRange{blockStart, stop})
	}
	return blocks
}

// packChunks greedily packs blocks into chunks up to maxChunkTokens, emitting to
// out. An oversized block (a fence or paragraph that alone exceeds the ceiling)
// is flushed on its own and split at line then rune boundaries.
func packChunks(markdown string, blocks []byteRange, sectionID, crumb string, out *[]Chunk) {
	chunkStart := -1
	for _, b := range blocks {
		if estimateTokens(markdown[b.start:b.stop]) > maxChunkTokens {
			if chunkStart >= 0 {
				emitChunk(markdown, chunkStart, b.start, sectionID, crumb, out)
				chunkStart = -1
			}
			for _, piece := range splitOversized(markdown, b.start, b.stop) {
				emitChunk(markdown, piece.start, piece.stop, sectionID, crumb, out)
			}
			continue
		}
		if chunkStart < 0 {
			chunkStart = b.start
			continue
		}
		if estimateTokens(markdown[chunkStart:b.stop]) > maxChunkTokens {
			emitChunk(markdown, chunkStart, b.start, sectionID, crumb, out)
			chunkStart = b.start
		}
	}
	if chunkStart >= 0 {
		emitChunk(markdown, chunkStart, blocks[len(blocks)-1].stop, sectionID, crumb, out)
	}
}

// splitOversized breaks an over-ceiling block into pieces at line boundaries,
// greedily packing lines; a single line that alone exceeds the ceiling is split
// at rune boundaries.
func splitOversized(markdown string, start, stop int) []byteRange {
	var pieces []byteRange
	pieceStart := -1
	i := start
	for i < stop {
		lineEnd := lineEndAt(markdown, i, stop)
		if estimateTokens(markdown[i:lineEnd]) > maxChunkTokens {
			if pieceStart >= 0 {
				pieces = append(pieces, byteRange{pieceStart, i})
				pieceStart = -1
			}
			pieces = append(pieces, splitRunes(markdown, i, lineEnd)...)
			i = lineEnd
			continue
		}
		if pieceStart < 0 {
			pieceStart = i
			i = lineEnd
			continue
		}
		if estimateTokens(markdown[pieceStart:lineEnd]) > maxChunkTokens {
			pieces = append(pieces, byteRange{pieceStart, i})
			pieceStart = i
		}
		i = lineEnd
	}
	if pieceStart >= 0 {
		pieces = append(pieces, byteRange{pieceStart, stop})
	}
	return pieces
}

// splitRunes cuts markdown[start:stop) into ceiling-bounded pieces at rune
// boundaries, so a multi-byte sequence is never split.
func splitRunes(markdown string, start, stop int) []byteRange {
	var pieces []byteRange
	pieceStart := start
	for i := start; i < stop; {
		_, size := utf8.DecodeRuneInString(markdown[i:stop])
		if i > pieceStart && estimateTokens(markdown[pieceStart:i+size]) > maxChunkTokens {
			pieces = append(pieces, byteRange{pieceStart, i})
			pieceStart = i
		}
		i += size
	}
	pieces = append(pieces, byteRange{pieceStart, stop})
	return pieces
}

// emitChunk appends a chunk covering markdown[start:stop) with the next index.
func emitChunk(markdown string, start, stop int, sectionID, crumb string, out *[]Chunk) {
	*out = append(*out, Chunk{
		Index:      len(*out),
		Section:    sectionID,
		Breadcrumb: crumb,
		Start:      start,
		End:        stop,
		Hash:       anchor.Of(markdown[start:stop]).String(),
	})
}

// lineEndAt returns the offset just past the newline ending the line at i,
// clamped to stop for a final line with no trailing newline.
func lineEndAt(s string, i, stop int) int {
	if nl := strings.IndexByte(s[i:stop], '\n'); nl >= 0 {
		return i + nl + 1
	}
	return stop
}

// isBlankLine reports whether line (a single line, trailing newline included) is
// empty or all whitespace.
func isBlankLine(line string) bool {
	return strings.TrimSpace(line) == ""
}

// inCodeRange reports whether pos falls inside any code content range.
func inCodeRange(pos int, ranges []byteRange) bool {
	for _, r := range ranges {
		if pos >= r.start && pos < r.stop {
			return true
		}
	}
	return false
}
