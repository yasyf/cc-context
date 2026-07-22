package index

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseContent(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    []ContentType
		wantErr bool
	}{
		{"empty defaults to code+docs", "", []ContentType{ContentCode, ContentDocs}, false},
		{"single", "code", []ContentType{ContentCode}, false},
		{"multiple", "code config", []ContentType{ContentCode, ContentConfig}, false},
		{"all expands", "all", []ContentType{ContentCode, ContentDocs, ContentConfig}, false},
		{"dedupes", "code code docs", []ContentType{ContentCode, ContentDocs}, false},
		{"unknown token errors", "code bogus", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseContent(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseContent(%q) err = %v, wantErr %v", tt.spec, err, tt.wantErr)
			}
			if err == nil && !slices.Equal(got, tt.want) {
				t.Errorf("ParseContent(%q) = %v, want %v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestExtensionsExcludesDataLanguages(t *testing.T) {
	exts := Extensions([]ContentType{ContentCode, ContentDocs})
	set := map[string]bool{}
	for _, e := range exts {
		set[e] = true
	}
	for _, want := range []string{".go", ".md", ".py"} {
		if !set[want] {
			t.Errorf("Extensions(code,docs) missing %q", want)
		}
	}
	// Data languages (json/csv/tsv) belong to no content family and are never indexed.
	for _, absent := range []string{".json", ".csv", ".tsv"} {
		if set[absent] {
			t.Errorf("Extensions(code,docs) unexpectedly includes data extension %q", absent)
		}
	}
	// Config is not in the requested set.
	if set[".toml"] {
		t.Errorf("Extensions(code,docs) should exclude config extension .toml")
	}
}

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"a/b/c.go", "go"},
		{"x.PY", "python"},
		{"data.json", "json"},
		{"Makefile", ""},
		{"no_ext", ""},
		{".gitignore", ""},                // a leading-dot-only name has no suffix (Path.suffix == "")
		{"config.gitignore", "gitignore"}, // but a real .gitignore suffix maps
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := DetectLanguage(tt.path); got != tt.want {
				t.Errorf("DetectLanguage(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestGetFileStatus(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, body []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	valid := write("valid.go", []byte("package main\nfunc main() {}\n"))
	small := write("small.go", []byte("x := 1\n"))                                        // <128 bytes, real content
	whitespace := write("blank.go", []byte("   \n\t\n"))                                  // <128 bytes, whitespace-only
	bigWhitespace := write("bigblank.go", []byte(strings.Repeat(" ", emptyFileBytes+10))) // >=128, whitespace but not gated
	tooLarge := write("big.go", make([]byte, maxFileBytes+1))

	tests := []struct {
		name string
		path string
		want fileStatus
	}{
		{"valid", valid, statusValid},
		{"small with content is valid", small, statusValid},
		{"small whitespace-only is empty", whitespace, statusEmpty},
		{"large whitespace is not gated", bigWhitespace, statusValid},
		{"over size cap is too large", tooLarge, statusTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getFileStatus(tt.path)
			if err != nil {
				t.Fatalf("getFileStatus(%s): %v", tt.name, err)
			}
			if got != tt.want {
				t.Errorf("getFileStatus(%s) = %d, want %d", tt.name, got, tt.want)
			}
		})
	}
}
