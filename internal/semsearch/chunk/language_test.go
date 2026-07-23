package chunk

import "testing"

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path     string
		wantLang string
		wantOK   bool
	}{
		{"foo.py", "python", true},
		{"dir/foo.PY", "python", true},  // suffix is lowercased
		{"x.C", "c", true},              // .C -> c, not cpp
		{"foo.tar.gz", "", false},       // .gz is unmapped; only last suffix used
		{"README.md", "markdown", true}, // last suffix
		{".hidden.txt", "", false},      // .txt is unmapped, leading dot ignored
		{".bashrc", "", false},          // dotfile has no suffix
		{"Makefile", "", false},         // no suffix
		{"foo.", "", false},             // trailing dot is not a suffix
		{"state.d.ts", "typescript", true},
		{"data.json", "json", true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			lang, ok := DetectLanguage(tt.path)
			if lang != tt.wantLang || ok != tt.wantOK {
				t.Errorf("DetectLanguage(%q) = (%q, %v), want (%q, %v)", tt.path, lang, ok, tt.wantLang, tt.wantOK)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		lang string
		want ContentType
	}{
		{"python", ContentCode},
		{"go", ContentCode},
		{"markdown", ContentDocs},
		{"yaml", ContentConfig},
		{"toml", ContentConfig},
		{"json", ContentData},
		{"csv", ContentData},
	}
	for _, tt := range tests {
		t.Run(tt.lang, func(t *testing.T) {
			if got := Classify(tt.lang); got != tt.want {
				t.Errorf("Classify(%q) = %q, want %q", tt.lang, got, tt.want)
			}
		})
	}
}

func TestIsSupportedLanguage(t *testing.T) {
	// Every value in the extension map is supported (semble's ALL_LANGUAGES),
	// even those without a bundled grammar; unmapped names are not.
	for _, lang := range []string{"python", "markdown", "toml", "json", "haskell"} {
		if !isSupportedLanguage(lang) {
			t.Errorf("isSupportedLanguage(%q) = false, want true", lang)
		}
	}
	if isSupportedLanguage("not-a-language") {
		t.Error("isSupportedLanguage(not-a-language) = true, want false")
	}
}
