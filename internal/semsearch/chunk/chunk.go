// Package chunk splits source files into semble-compatible chunks: a bit-exact
// port of semble 0.5.2's chunking policy (chunking/*.py, index/files.py).
// Supported languages are AST-chunked via tree-sitter grammars loaded as
// wazero WASM modules; every other file falls back to line chunking.
package chunk

import (
	"unicode/utf8"

	"github.com/yasyf/cc-context/internal/semsearch"
)

// File gates from semble's files.py.
const (
	maxFileBytes   = 1_000_000 // _MAX_FILE_BYTES: files larger are skipped
	emptyFileBytes = 128       // _EMPTY_FILE_BYTES: below this, whitespace-only is skipped
)

// Chunk splits one file's raw bytes into chunks, applying semble's per-file
// policy: files over 1 MB, whitespace-only files under 128 bytes, data-language
// files (JSON, CSV, …), and files whose extension semble does not map all yield
// no chunks. content is the raw file bytes; it is decoded as UTF-8 with invalid
// sequences replaced by U+FFFD, matching semble's read_file_text.
func Chunk(path string, content []byte) []semsearch.Chunk {
	lang, ok := DetectLanguage(path)
	if !ok {
		return nil
	}
	if Classify(lang) == ContentData {
		return nil
	}

	source := decodeReplace(content)
	if len(content) > maxFileBytes {
		return nil
	}
	if len(content) < emptyFileBytes && pythonStrip(source) == "" {
		return nil
	}
	return chunkSource(source, path, lang, defaultParser)
}

// chunkSource ports semble's chunk_source: AST-chunk when the language is
// supported and a grammar is available, else fall back to line chunking, then
// materialize each boundary into a Chunk.
func chunkSource(source, path, lang string, p parser) []semsearch.Chunk {
	if pythonStrip(source) == "" {
		return nil
	}

	src := []byte(source)
	var bounds []boundary
	if lang != "" && isSupportedLanguage(lang) {
		if root, ok := p.parse(lang, src); ok {
			bounds = mergeNode(root, desiredChunkLength)
		}
	}
	if bounds == nil {
		bounds = chunkLines(src, desiredChunkLength)
	}

	chunks := make([]semsearch.Chunk, 0, len(bounds))
	for _, b := range bounds {
		content, endLine := extractChunk(src, b)
		chunks = append(chunks, semsearch.Chunk{
			Path:      path,
			StartLine: countNewlines(src, b.start) + 1,
			EndLine:   endLine,
			Content:   content,
		})
	}
	return chunks
}

// extractChunk returns a boundary's content and end line. It reproduces
// semble's end_index = max(end-1, start) clamp: the content is src[start:end],
// and the end line is that of the content's last character. A zero-length
// boundary yields the single rune at start (empty at end of file).
func extractChunk(src []byte, b boundary) (content string, endLine int) {
	if b.end > b.start {
		_, sz := utf8.DecodeLastRune(src[:b.end])
		return string(src[b.start:b.end]), countNewlines(src, b.end-sz) + 1
	}
	end := b.start
	if b.start < len(src) {
		_, sz := utf8.DecodeRune(src[b.start:])
		end = b.start + sz
	}
	return string(src[b.start:end]), countNewlines(src, b.start) + 1
}
