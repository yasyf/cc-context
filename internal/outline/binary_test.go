package outline

import (
	"os"
	"testing"
)

func TestBinarySkip(t *testing.T) {
	t.Chdir(t.TempDir())
	tests := []struct {
		name    string
		path    string
		content []byte
		want    string
		ok      bool
	}{
		{"png", "image.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), "image.png (binary, 16B, image/png) [skipped]", true},
		{"utf-16le", "utf16.txt", []byte("\xff\xfeh\x00i\x00\n\x00"), "utf16.txt (binary, 8B, text/plain; charset=utf-16le) [skipped]", true},
		{"go", "main.go", []byte("package main\n\nfunc main() {}\n"), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(tt.path, tt.content, 0o600); err != nil {
				t.Fatal(err)
			}
			got, ok := BinarySkip(tt.path)
			if got != tt.want || ok != tt.ok {
				t.Errorf("BinarySkip(%q) = %q, %v, want %q, %v", tt.path, got, ok, tt.want, tt.ok)
			}
		})
	}

	t.Run("directory", func(t *testing.T) {
		if err := os.Mkdir("pkg", 0o700); err != nil {
			t.Fatal(err)
		}
		got, ok := BinarySkip("pkg")
		if got != "" || ok {
			t.Errorf("BinarySkip(directory) = %q, %v, want \"\", false", got, ok)
		}
	})
}
