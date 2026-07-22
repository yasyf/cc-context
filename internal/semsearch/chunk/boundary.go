package chunk

import "unicode/utf8"

// Chunking constants ported verbatim from semble's chunking module.
const (
	desiredChunkLength = 750 // _DESIRED_CHUNK_LENGTH_CHARS
	minChunkSize       = 50  // _MIN_CHUNK_SIZE (bytes)
	recursionDepth     = 500 // _RECURSION_DEPTH
)

// boundary is a half-open byte span [start, end) into the source, the Go analog
// of semble's ChunkBoundary. Unlike semble it stays in byte space throughout;
// the byte→char conversion semble performs only for str slicing is a no-op here.
type boundary struct {
	start int
	end   int
}

// byteLen is the length metric semble's AST path uses (node.end_byte - start_byte).
func byteLen(b boundary) int { return b.end - b.start }

// mergeNodeInner greedily packs adjacent sibling nodes into spans targeting
// desired bytes, recursing into any single child that overruns it — a port of
// semble's _merge_node_inner. depth guards against pathological trees.
func mergeNodeInner(n node, desired, depth int) []boundary {
	if len(n.children) == 0 {
		return []boundary{{int(n.start), int(n.end)}}
	}
	if depth > recursionDepth {
		return []boundary{{int(n.start), int(n.end)}}
	}
	if int(n.end-n.start) < minChunkSize {
		return []boundary{{int(n.start), int(n.end)}}
	}

	var groups []boundary
	children := n.children
	index := 0
	for index < len(children) {
		child := children[index]
		start := int(child.start)
		end := int(child.end)
		length := int(child.end - child.start)
		index++
		if length > desired {
			groups = append(groups, mergeNodeInner(child, desired, depth+1)...)
			continue
		}
		for index < len(children) {
			c := children[index]
			cl := int(c.end - c.start)
			if length+cl > desired {
				break
			}
			end = int(c.end)
			length += cl
			index++
		}
		groups = append(groups, boundary{start, end})
	}
	return groups
}

// mergeAdjacent coalesces a sorted run of spans up to desired, summing each
// span's own length (lengthOf) rather than the coalesced extent so gaps between
// AST siblings are excluded from the budget — a port of _merge_adjacent_chunks.
func mergeAdjacent(chunks []boundary, desired int, lengthOf func(boundary) int) []boundary {
	var merged []boundary
	cur := chunks[0]
	curLen := lengthOf(cur)
	for _, g := range chunks[1:] {
		l := lengthOf(g)
		if curLen+l > desired {
			merged = append(merged, cur)
			cur = g
			curLen = l
			continue
		}
		cur.end = g.end
		curLen += l
	}
	return append(merged, cur)
}

// mergeNode turns a parse tree into merged byte-span boundaries.
func mergeNode(root node, desired int) []boundary {
	return mergeAdjacent(mergeNodeInner(root, desired, 0), desired, byteLen)
}

// chunkLines is the line-based fallback for unparsed or unsupported files. It
// packs whole lines up to desired, measured in characters to match semble's
// str-based chunk_lines. src must be valid UTF-8.
func chunkLines(src []byte, desired int) []boundary {
	if pythonStrip(string(src)) == "" {
		return nil
	}
	spans := splitLineSpans(src)
	return mergeAdjacent(spans, desired, func(b boundary) int {
		return utf8.RuneCount(src[b.start:b.end])
	})
}
