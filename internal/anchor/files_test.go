package anchor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/anchor"
)

func TestFilesLineAt(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	abs := filepath.Join(root, "f.txt")

	tests := []struct {
		name     string
		path     string
		n        int
		wantLine string
		wantOK   bool
	}{
		{"relative first", "f.txt", 1, "alpha", true},
		{"relative middle", "f.txt", 2, "beta", true},
		{"absolute last", abs, 3, "gamma", true},
		{"missing file", "nope.txt", 1, "", false},
		{"line zero", "f.txt", 0, "", false},
		{"negative line", "f.txt", -1, "", false},
		{"out of range", "f.txt", 4, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := anchor.NewFiles(root)
			line, ok := fs.LineAt(tt.path, tt.n)
			if ok != tt.wantOK {
				t.Fatalf("LineAt(%q, %d) ok = %v, want %v", tt.path, tt.n, ok, tt.wantOK)
			}
			if line != tt.wantLine {
				t.Errorf("LineAt(%q, %d) line = %q, want %q", tt.path, tt.n, line, tt.wantLine)
			}
		})
	}
}

// TestFilesLineAtLazyLoadsOnce proves the cache loads a path exactly once:
// after the first read the file is deleted, so a second read that returned a
// miss would mean the cache reloaded from disk.
func TestFilesLineAtLazyLoadsOnce(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	fs := anchor.NewFiles(root)

	if line, ok := fs.LineAt("f.txt", 1); !ok || line != "alpha" {
		t.Fatalf("first LineAt = %q, %v; want %q, true", line, ok, "alpha")
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove fixture: %v", err)
	}
	if line, ok := fs.LineAt("f.txt", 2); !ok || line != "beta" {
		t.Fatalf("cached LineAt = %q, %v; want %q, true", line, ok, "beta")
	}
}
