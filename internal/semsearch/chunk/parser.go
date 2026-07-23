package chunk

// node mirrors the subset of tree_sitter.Node the chunker reads: a byte span
// and its ordered children, including anonymous nodes (matching Node.children).
// Node kind is irrelevant to chunking, so it is not carried.
type node struct {
	start    uint32
	end      uint32
	children []node
}

// parser turns source bytes for one language into a parse tree. ok is false when
// no grammar is available for lang; the caller then falls back to line chunking,
// matching semble's None-parser path.
type parser interface {
	parse(lang string, src []byte) (root node, ok bool)
}
