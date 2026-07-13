// Package sniff classifies a file as text or binary from a short content probe,
// so callers can label binaries without a bytes/4 token estimate that a raw byte
// count would produce.
package sniff

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"unicode/utf8"
)

// probeBytes is the leading window read for the binary/text decision; it is wider
// than the media-type window so a file that turns binary past 512 bytes (ASCII
// header then a NUL) is still caught.
const probeBytes = 4096

// mimeBytes is the leading window handed to net/http.DetectContentType; it matches
// that function's own sniff length.
const mimeBytes = 512

// binaryInvalidRatio is the fraction of the probe that must fail UTF-8 decoding
// (absent a decisive NUL byte) for the file to count as binary.
const binaryInvalidRatio = 0.3

// Detect classifies the file at path from its first 4096 bytes, returning the
// net/http content type (sniffed over the first 512) and whether the probe looks
// binary. A NUL byte anywhere in the probe, or invalid UTF-8 dominating it, marks
// it binary; an empty or unreadable file reads as non-binary text.
func Detect(path string) (mime string, binary bool) {
	f, err := os.Open(path) //nolint:gosec // path is a file the caller's glob selected; sniffing it is the point
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, probeBytes)
	n, _ := io.ReadFull(f, buf)
	probe := buf[:n]
	mimeProbe := probe
	if len(mimeProbe) > mimeBytes {
		mimeProbe = probe[:mimeBytes]
	}
	return http.DetectContentType(mimeProbe), isBinary(probe)
}

// isBinary reports whether probe looks binary: a NUL byte is decisive, otherwise
// the fraction of bytes that fail UTF-8 decoding must exceed binaryInvalidRatio. An
// empty probe is text.
func isBinary(probe []byte) bool {
	if len(probe) == 0 {
		return false
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return true
	}
	invalid := 0
	for b := probe; len(b) > 0; {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			invalid++
		}
		b = b[size:]
	}
	return float64(invalid) > float64(len(probe))*binaryInvalidRatio
}
