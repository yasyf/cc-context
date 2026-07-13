package sniff

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDetect(t *testing.T) {
	tests := []struct {
		name       string
		content    []byte
		wantMime   string
		wantBinary bool
	}{
		{"png magic", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), "image/png", true},
		{"jpeg magic", []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), "image/jpeg", true},
		{"utf-8 text", []byte("package main\n\nfunc main() {}\n"), "text/plain; charset=utf-8", false},
		{"utf-16le text", []byte("\xff\xfeh\x00i\x00\n\x00"), "text/plain; charset=utf-16le", true},
		{"empty file", []byte{}, "text/plain; charset=utf-8", false},
		{"nul in text", []byte("abc\x00def"), "application/octet-stream", true},
		// DetectContentType reads high bytes as text; the UTF-8-dominance check is
		// what marks this binary, so the label stays text while binary is true.
		{"high-bit dominant", []byte{0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8}, "text/plain; charset=utf-8", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "probe")
			if err := os.WriteFile(path, tt.content, 0o600); err != nil {
				t.Fatal(err)
			}
			mime, binary := Detect(path)
			if mime != tt.wantMime {
				t.Errorf("mime = %q, want %q", mime, tt.wantMime)
			}
			if binary != tt.wantBinary {
				t.Errorf("binary = %v, want %v", binary, tt.wantBinary)
			}
		})
	}
}

func TestDetectMissingFile(t *testing.T) {
	mime, binary := Detect(filepath.Join(t.TempDir(), "does-not-exist"))
	if mime != "" || binary {
		t.Errorf("Detect(missing) = %q, %v, want \"\", false", mime, binary)
	}
}

func TestDetectLateNUL(t *testing.T) {
	// 512 ASCII bytes then a NUL at 513: the 512-byte media window reads text, but
	// the wider binary probe must catch the NUL and mark it binary.
	content := append(bytes.Repeat([]byte("a"), 512), 0)
	path := filepath.Join(t.TempDir(), "late-nul")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, binary := Detect(path); !binary {
		t.Error("a NUL at offset 512 must classify the file as binary")
	}
}
