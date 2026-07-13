package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOutlineCommandBinarySkip(t *testing.T) {
	t.Chdir(t.TempDir())
	tests := []struct {
		name    string
		path    string
		content []byte
		args    []string
		want    string
	}{
		{"png", "image.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), nil, "image.png (binary, 16B, image/png) [skipped]\n"},
		{"png forced go", "forced.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), []string{"--lang", "go"}, "forced.png (binary, 16B, image/png) [skipped]\n"},
		{"utf-16le", "utf16.txt", []byte("\xff\xfeh\x00i\x00\n\x00"), nil, "utf16.txt (binary, 8B, text/plain; charset=utf-16le) [skipped]\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(tt.path, tt.content, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := newOutlineCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(append([]string{tt.path}, tt.args...))
			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("outline execute: %v\n%s", err, out.String())
			}
			if got := out.String(); got != tt.want {
				t.Errorf("outline output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOutlineCommandNonBinaryTargets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ast-grep script is POSIX-only")
	}
	t.Chdir(t.TempDir())
	if err := os.WriteFile("source.go", []byte("package sample\n\ntype X struct{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("pkg", 0o700); err != nil {
		t.Fatal(err)
	}
	fakeOutlineAstGrep(t)

	tests := []struct {
		name string
		path string
	}{
		{"go file", "source.go"},
		{"directory", "pkg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newOutlineCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{tt.path})
			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("outline execute: %v\n%s", err, out.String())
			}
			got := out.String()
			if !strings.HasPrefix(got, "# ast-grep\n") || !strings.Contains(got, "type X struct {") {
				t.Errorf("outline output = %q, want ast-grep structural outline", got)
			}
		})
	}
}

func fakeOutlineAstGrep(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ast-grep")
	script := `#!/bin/sh
cat <<'EOF'
{"path":"source.go","language":"Go","items":[{"symbolType":"struct","name":"X","signature":"type X struct {","isExported":true,"range":{"start":{"line":2}},"members":[]}]}
EOF
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // fake engine must be owner-executable
		t.Fatalf("write fake ast-grep: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
