package backend

import "testing"

func TestSplitGlobAnchor(t *testing.T) {
	tests := []struct {
		name     string
		glob     string
		wantDir  string
		wantRest string
	}{
		{"anchored", "a/b/*.py", "a/b", "*.py"},
		{"recursive tail", "a/**/*.py", "a", "**/*.py"},
		{"trailing doublestar", "dir/**", "dir", "**"},
		{"leading doublestar", "**/x/*.py", "", "**/x/*.py"},
		{"slash-less glob", "*.py", "", "*.py"},
		{"slash-less literal", "foo", "", "foo"},
		{"exclusion", "!*.py", "", "!*.py"},
		{"metachar in prefix", "a*/b/*.py", "", "a*/b/*.py"},
		{"absolute", "/abs/dir/*.py", "/abs/dir", "*.py"},
		{"fully literal path", "a/b/c", "a/b/c", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, rest := SplitGlobAnchor(tt.glob)
			if dir != tt.wantDir || rest != tt.wantRest {
				t.Errorf("SplitGlobAnchor(%q) = (%q, %q), want (%q, %q)", tt.glob, dir, rest, tt.wantDir, tt.wantRest)
			}
		})
	}
}
