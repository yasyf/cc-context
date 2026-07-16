package ripgrep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/backend"
)

func TestRipgrepArgv(t *testing.T) {
	sub, missing, file := anchorDirs(t)
	parent := filepath.Dir(sub)
	tests := []struct {
		name string
		args backend.Args
		want []string
	}{
		{"bare", backend.Args{Query: "foo"}, []string{"--json", "--fixed-strings", "-e", "foo"}},
		{"ignore-case + word", backend.Args{Query: "foo", IgnoreCase: true, Word: true}, []string{"--json", "--fixed-strings", "-i", "-w", "-e", "foo"}},
		{"unanchored glob + expand unchanged", backend.Args{Query: "foo", Glob: "*.go", Expand: 3}, []string{"--json", "--fixed-strings", "--glob", "*.go", "-C", "3", "-e", "foo"}},
		{"context flag", backend.Args{Query: "foo", Context: 2}, []string{"--json", "--fixed-strings", "-C", "2", "-e", "foo"}},
		{"after and before flags", backend.Args{Query: "foo", After: 2, Before: 1}, []string{"--json", "--fixed-strings", "-A", "2", "-B", "1", "-e", "foo"}},
		{"context beats after/before", backend.Args{Query: "foo", Context: 3, After: 9, Before: 9}, []string{"--json", "--fixed-strings", "-C", "3", "-e", "foo"}},
		{"scope adds --no-ignore-parent", backend.Args{Query: "foo", Scope: "internal"}, []string{"--json", "--fixed-strings", "--no-ignore-parent", "-e", "foo", "--", "internal"}},
		{"flag-like scope lands after -- with flag", backend.Args{Query: "foo", Scope: "--hidden"}, []string{"--json", "--fixed-strings", "--no-ignore-parent", "-e", "foo", "--", "--hidden"}},
		{"anchored existing-dir glob relativizes to rest + operand", backend.Args{Query: "foo", Glob: sub + "/*.go"}, []string{"--json", "--fixed-strings", "--glob", "*.go", "--no-ignore-parent", "-e", "foo", "--", sub}},
		{"dir-literal glob drops -g, operand alone filters", backend.Args{Query: "foo", Glob: sub}, []string{"--json", "--fixed-strings", "--no-ignore-parent", "-e", "foo", "--", sub}},
		{"explicit scope composes onto join", backend.Args{Query: "foo", Glob: "pkg/*.go", Scope: parent}, []string{"--json", "--fixed-strings", "--glob", "*.go", "--no-ignore-parent", "-e", "foo", "--", sub}},
		{"literal file glob → parent operand + basename", backend.Args{Query: "foo", Glob: file}, []string{"--json", "--fixed-strings", "--glob", "file.go", "--no-ignore-parent", "-e", "foo", "--", sub}},
		{"nonexistent anchor unchanged", backend.Args{Query: "foo", Glob: missing + "/*.go"}, []string{"--json", "--fixed-strings", "--glob", missing + "/*.go", "-e", "foo"}},
		{"leading-dash pattern", backend.Args{Query: "-foo"}, []string{"--json", "--fixed-strings", "-e", "-foo"}},
		{"regex drops --fixed-strings", backend.Args{Query: "foo", Regex: true}, []string{"--json", "-e", "foo"}},
		{"regex + ignore-case + word", backend.Args{Query: "foo", Regex: true, IgnoreCase: true, Word: true}, []string{"--json", "-i", "-w", "-e", "foo"}},
		{"literal paths keep --fixed-strings after --", backend.Args{Query: "foo", Paths: []string{"a.go", "b.go"}}, []string{"--json", "--fixed-strings", "-e", "foo", "--", "a.go", "b.go"}},
		{"scope + paths both ride after --", backend.Args{Query: "foo", Scope: "internal", Paths: []string{"a.go"}}, []string{"--json", "--fixed-strings", "--no-ignore-parent", "-e", "foo", "--", "internal", "a.go"}},
		{"regex + paths", backend.Args{Query: "^func ", Regex: true, Paths: []string{"a.go"}}, []string{"--json", "-e", "^func ", "--", "a.go"}},
		{"paths + glob keep --glob and operands, skip anchoring", backend.Args{Query: "foo", Paths: []string{"a.go", "sub"}, Glob: "*.go"}, []string{"--json", "--fixed-strings", "--glob", "*.go", "-e", "foo", "--", "a.go", "sub"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ripgrepArgv(tt.args); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ripgrepArgv() = %q, want %q", got, tt.want)
			}
		})
	}
}

// anchorDirs returns an existing directory, a sibling that does not exist, and a
// regular file inside the existing directory — all absolute so SplitGlobAnchor's
// prefix survives ripgrepArgv's os.Stat.
func anchorDirs(t *testing.T) (existing, missing, file string) {
	t.Helper()
	tmp := t.TempDir()
	existing = filepath.Join(tmp, "pkg")
	if err := os.MkdirAll(existing, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file = filepath.Join(existing, "file.go")
	if err := os.WriteFile(file, []byte("package pkg\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	return existing, filepath.Join(tmp, "nope"), file
}

func TestGrepArgv(t *testing.T) {
	sub, _, file := anchorDirs(t)
	parent := filepath.Dir(sub)
	tests := []struct {
		name    string
		args    backend.Args
		want    []string
		wantErr bool
	}{
		{"bare", backend.Args{Query: "foo"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "."}, false},
		{"ignore-case", backend.Args{Query: "foo", IgnoreCase: true}, []string{"-rnHFI", "--null", "-i", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "."}, false},
		{"word + expand", backend.Args{Query: "foo", Word: true, Expand: 2}, []string{"-rnHFI", "--null", "-w", "-C", "2", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "."}, false},
		{"context flag", backend.Args{Query: "foo", Context: 2}, []string{"-rnHFI", "--null", "-C", "2", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "."}, false},
		{"after and before flags", backend.Args{Query: "foo", After: 2, Before: 1}, []string{"-rnHFI", "--null", "-A", "2", "-B", "1", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "."}, false},
		{"basename glob", backend.Args{Query: "foo", Glob: "*.go"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", "."}, false},
		{"dir recurse glob", backend.Args{Query: "foo", Glob: "src/**"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "src"}, false},
		{"dir + ext glob", backend.Args{Query: "foo", Glob: "src/**/*.go"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", "src"}, false},
		{"scope", backend.Args{Query: "foo", Scope: "internal"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "internal"}, false},
		{"flag-like scope lands after --", backend.Args{Query: "foo", Scope: "--hidden"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "--hidden"}, false},
		{"scope + basename glob", backend.Args{Query: "foo", Glob: "*.go", Scope: "internal"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", "internal"}, false},
		{"leading-dash pattern", backend.Args{Query: "-foo"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "-foo", "--", "."}, false},
		{"regex swaps -rnFI for -rnEI", backend.Args{Query: "foo", Regex: true}, []string{"-rnHEI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "."}, false},
		{"paths keep excludes", backend.Args{Query: "foo", Paths: []string{"a.go", "b.go"}}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "a.go", "b.go"}, false},
		{"regex + paths keep excludes", backend.Args{Query: "^func ", Regex: true, Paths: []string{"a.go"}}, []string{"-rnHEI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "^func ", "--", "a.go"}, false},
		{"scope + paths keep excludes", backend.Args{Query: "foo", Scope: "internal", Paths: []string{"a.go"}}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "internal", "a.go"}, false},
		{"paths + basename glob → include", backend.Args{Query: "foo", Paths: []string{"a.go"}, Glob: "*.go"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", "a.go"}, false},
		{"paths + double-star glob → include", backend.Args{Query: "foo", Paths: []string{"a.go", "sub"}, Glob: "**/*.go"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", "a.go", "sub"}, false},
		{"paths + dir-rooted glob fails", backend.Args{Query: "foo", Paths: []string{"sub"}, Glob: "src/**"}, nil, true},
		{"scope + dir glob fails", backend.Args{Query: "foo", Glob: "src/**", Scope: "internal"}, nil, true},
		{"brace glob fails", backend.Args{Query: "foo", Glob: "{a,b}/**"}, nil, true},
		{"mid-path wildcard fails", backend.Args{Query: "foo", Glob: "src/*/x.go"}, nil, true},
		{"double-star-slash-star maps to include", backend.Args{Query: "foo", Glob: "**/*.go"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", "."}, false},
		{"anchored existing-dir glob peels to include + root", backend.Args{Query: "foo", Glob: sub + "/*.go"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", sub}, false},
		{"anchored dir-literal glob roots the search", backend.Args{Query: "foo", Glob: sub}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", sub}, false},
		{"anchored recursive glob peels to include + root", backend.Args{Query: "foo", Glob: sub + "/**/*.go"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", sub}, false},
		{"anchored glob composes with explicit scope", backend.Args{Query: "foo", Glob: "pkg/*.go", Scope: parent}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", sub}, false},
		{"anchored file glob → parent root + basename include", backend.Args{Query: "foo", Glob: file}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=file.go", "-e", "foo", "--", sub}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := grepArgv(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("grepArgv() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("grepArgv() = %q, want %q", got, tt.want)
			}
		})
	}
}

// globPathsFixture writes a.go, b.txt, and a sub/ directory into a fresh temp dir,
// chdirs into it, and returns the dir — the on-disk operands filterGlobPaths and
// buildArgv classify with os.Stat.
func globPathsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"a.go", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	t.Chdir(dir)
	return dir
}

func TestFilterGlobPaths(t *testing.T) {
	globPathsFixture(t)
	tests := []struct {
		name    string
		paths   []string
		glob    string
		want    []string
		wantErr bool
	}{
		{"file matching glob kept", []string{"a.go"}, "*.go", []string{"a.go"}, false},
		{"file not matching glob dropped, none left → error", []string{"b.txt"}, "*.go", nil, true},
		{"directory passes through to native filtering", []string{"sub"}, "*.go", []string{"sub"}, false},
		{"nonexistent operand passes through unchanged", []string{"missing.go"}, "*.go", []string{"missing.go"}, false},
		{"mixed: keep matching file, drop other, pass dir", []string{"a.go", "b.txt", "sub"}, "*.go", []string{"a.go", "sub"}, false},
		{"slash-less glob matches basename", []string{"a.go"}, "*.go", []string{"a.go"}, false},
		{"slashed glob matches whole path", []string{"a.go"}, "**/*.go", []string{"a.go"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := filterGlobPaths(tt.paths, tt.glob)
			if (err != nil) != tt.wantErr {
				t.Fatalf("filterGlobPaths() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !strings.Contains(err.Error(), "no paths match") {
					t.Errorf("filterGlobPaths() err = %v, want no-paths-match message", err)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("filterGlobPaths() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildArgvGlobPaths proves buildArgv prefilters explicit file operands
// Go-side against --glob (so rg's -g, which never filters explicit files, agrees
// with grep's --include), passes directory operands through to native filtering,
// and errors when every file operand is filtered out with no directory left.
func TestBuildArgvGlobPaths(t *testing.T) {
	globPathsFixture(t)
	tests := []struct {
		name    string
		eng     engine
		args    backend.Args
		want    []string
		wantErr bool
	}{
		{"rg file filtering drops non-glob file", engineRipgrep, backend.Args{Query: "foo", Paths: []string{"a.go", "b.txt"}, Glob: "*.go"}, []string{"--json", "--fixed-strings", "--glob", "*.go", "-e", "foo", "--", "a.go"}, false},
		{"fallback file filtering drops non-glob file", engineGrep, backend.Args{Query: "foo", Paths: []string{"a.go", "b.txt"}, Glob: "*.go"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", "a.go"}, false},
		{"rg dir keeps native --glob", engineRipgrep, backend.Args{Query: "foo", Paths: []string{"sub"}, Glob: "*.go"}, []string{"--json", "--fixed-strings", "--glob", "*.go", "-e", "foo", "--", "sub"}, false},
		{"fallback dir keeps native --include", engineGrep, backend.Args{Query: "foo", Paths: []string{"sub"}, Glob: "*.go"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", "sub"}, false},
		{"rg mixed file+dir operands", engineRipgrep, backend.Args{Query: "foo", Paths: []string{"a.go", "b.txt", "sub"}, Glob: "*.go"}, []string{"--json", "--fixed-strings", "--glob", "*.go", "-e", "foo", "--", "a.go", "sub"}, false},
		{"fallback mixed file+dir operands", engineGrep, backend.Args{Query: "foo", Paths: []string{"a.go", "b.txt", "sub"}, Glob: "*.go"}, []string{"-rnHFI", "--null", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", "a.go", "sub"}, false},
		{"rg empty after filter errors", engineRipgrep, backend.Args{Query: "foo", Paths: []string{"b.txt"}, Glob: "*.go"}, nil, true},
		{"fallback empty after filter errors", engineGrep, backend.Args{Query: "foo", Paths: []string{"b.txt"}, Glob: "*.go"}, nil, true},
		{"fallback dir-rooted glob with paths fails", engineGrep, backend.Args{Query: "foo", Paths: []string{"sub"}, Glob: "src/**"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildArgv(tt.eng, tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("buildArgv() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildArgv() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRipgrepNoFiles(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no-files signature", errors.New("rg: exit status 2: No files were searched, which means ripgrep probably applied a filter"), true},
		{"regex parse error is not the no-files signature", errors.New("rg: exit status 2: regex parse error: unclosed group"), false},
		{"other exit-2 is not the no-files signature", errors.New("rg: exit status 2: something else"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ripgrepNoFiles(tt.err); got != tt.want {
				t.Errorf("ripgrepNoFiles() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRipgrepRegexHint(t *testing.T) {
	parseErr := errors.New("rg: exit status 2: regex parse error:\n    (?:foo\\|bar()\n    ^\nerror: unclosed group")
	unclosedOnly := errors.New("rg: exit status 2: regex parse error:\n    (?:foo()\n    ^\nerror: unclosed group")
	noFiles := errors.New("rg: exit status 2: No files were searched")
	other := errors.New("rg: exit status 2: something else")
	tests := []struct {
		name     string
		err      error
		pattern  string
		wantHint bool
	}{
		{"BRE alternation on an unclosed-group parse error fires", parseErr, `foo\|bar(`, true},
		{"BRE group escape fires", parseErr, `foo\(bar`, true},
		{"genuine unclosed group without BRE escapes is silent", unclosedOnly, `foo(`, false},
		{"no-files-searched signature is silent (disjoint from parse errors)", noFiles, `foo\|bar`, false},
		{"non-regex exit-2 is silent", other, `foo\|bar`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ripgrepRegexHint(tt.err, tt.pattern)
			hasHint := strings.Contains(got.Error(), "hint: --regex is Rust syntax")
			if hasHint != tt.wantHint {
				t.Errorf("ripgrepRegexHint() hint = %v, want %v; got err = %q", hasHint, tt.wantHint, got.Error())
			}
			if !errors.Is(got, tt.err) {
				t.Errorf("ripgrepRegexHint() dropped the wrapped error: %v", got)
			}
		})
	}
}

// TestRun_RipgrepNoFilesNormalizes proves an rg exit-2 "no files were searched"
// failure is normalized to the clean no-match grep reports as exit 0, while a grep
// engine error is returned untouched.
func TestRun_RipgrepNoFilesNormalizes(t *testing.T) {
	noFiles := func(context.Context, string, []string) (string, error) {
		return "", errors.New("rg: exit status 2: No files were searched")
	}
	out, found, err := run(context.Background(), engineRipgrep, "rg", backend.Args{Query: "foo", Glob: "*.zzz"}, noFiles)
	if err != nil {
		t.Fatalf("run() err = %v, want nil (normalized no-match)", err)
	}
	if found {
		t.Errorf("run() found = true, want false")
	}
	if !strings.Contains(out, "no matches") {
		t.Errorf("run() = %q, want no-match header", out)
	}

	// The same signature from the grep engine is a real error, not a no-match.
	boom := func(context.Context, string, []string) (string, error) {
		return "", errors.New("grep: exit status 2: No files were searched")
	}
	if _, _, err := run(context.Background(), engineGrep, "grep", backend.Args{Query: "foo"}, boom); err == nil {
		t.Errorf("run() grep err = nil, want propagated error")
	}
}

// TestRun_RipgrepRegexHintPropagates proves run appends the BRE hint to a rg
// regex-parse error whose pattern carries a BRE escape, and leaves a bare parse
// error unhinted.
func TestRun_RipgrepRegexHintPropagates(t *testing.T) {
	parseErr := func(context.Context, string, []string) (string, error) {
		return "", errors.New("rg: exit status 2: regex parse error: unclosed group")
	}
	_, _, err := run(context.Background(), engineRipgrep, "rg", backend.Args{Query: `foo\|bar`, Regex: true}, parseErr)
	if err == nil || !strings.Contains(err.Error(), "hint: --regex is Rust syntax") {
		t.Errorf("run() err = %v, want BRE hint appended", err)
	}
	_, _, err = run(context.Background(), engineRipgrep, "rg", backend.Args{Query: "foo(", Regex: true}, parseErr)
	if err == nil || strings.Contains(err.Error(), "hint:") {
		t.Errorf("run() err = %v, want bare parse error without hint", err)
	}
}

func TestTranslateGlob(t *testing.T) {
	tests := []struct {
		name        string
		glob        string
		wantInclude string
		wantRoot    string
		wantErr     bool
	}{
		{"empty", "", "", "", false},
		{"basename", "*.go", "*.go", "", false},
		{"dir recurse", "src/**", "", "src", false},
		{"nested dir recurse", "internal/cli/**", "", "internal/cli", false},
		{"dir plus ext", "src/**/*.go", "*.go", "src", false},
		{"braces", "{a,b}", "", "", true},
		{"mid-path wildcard", "src/*/x.go", "", "", true},
		{"leading double-star", "**/*.go", "*.go", "", false},
		{"leading double-star deep tail", "**/a/b.go", "", "", true},
		{"deep tail", "a/**/b/c.go", "", "", true},
		{"wildcard dir on recurse", "sr*/**", "", "", true},
		{"bare double-star-slash", "**/", "", "", true},
		{"leading double-star hidden", "**/.*", "", "", true},
		{"leading double-star hidden ext", "**/.*.go", "", "", true},
		{"bare hidden basename", ".*.go", "", "", true},
		{"hidden under dir recurse", "src/**/.env", "", "", true},
		{"empty basename under dir recurse", "src/**/", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			include, root, err := translateGlob(tt.glob)
			if (err != nil) != tt.wantErr {
				t.Fatalf("translateGlob(%q) err = %v, wantErr %v", tt.glob, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if include != tt.wantInclude || root != tt.wantRoot {
				t.Errorf("translateGlob(%q) = (%q, %q), want (%q, %q)", tt.glob, include, root, tt.wantInclude, tt.wantRoot)
			}
		})
	}
}

func TestParseRipgrep(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"begin","data":{"path":{"text":"a.go"}}}`,
		`{"type":"match","data":{"path":{"text":"a.go"},"lines":{"text":"foo one\n"},"line_number":3,"submatches":[{"match":{"text":"foo"},"start":0,"end":3}]}}`,
		`{"type":"end","data":{"path":{"text":"a.go"}}}`,
		`{"type":"begin","data":{"path":{"text":"b.go"}}}`,
		`{"type":"context","data":{"path":{"text":"b.go"},"lines":{"text":"ctx before\n"},"line_number":9,"submatches":[]}}`,
		`{"type":"match","data":{"path":{"text":"b.go"},"lines":{"text":"foo two\n"},"line_number":10,"submatches":[]}}`,
		`{"type":"end","data":{"path":{"text":"b.go"}}}`,
		`{"data":{"stats":{}},"type":"summary"}`,
		"",
	}, "\n")

	got, err := parseRipgrep(raw)
	if err != nil {
		t.Fatalf("parseRipgrep() err = %v", err)
	}
	want := []fileGroup{
		{path: "a.go", lines: []grepLine{{num: 3, text: "foo one", isMatch: true}}},
		{path: "b.go", lines: []grepLine{{num: 9, text: "ctx before", isMatch: false}, {num: 10, text: "foo two", isMatch: true}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseRipgrep() = %+v, want %+v", got, want)
	}
}

func TestParseRipgrep_NoMatch(t *testing.T) {
	got, err := parseRipgrep("")
	if err != nil {
		t.Fatalf("parseRipgrep() err = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parseRipgrep(\"\") = %+v, want empty", got)
	}
}

func TestParseRipgrep_BadJSON(t *testing.T) {
	if _, err := parseRipgrep("not json"); err == nil {
		t.Fatal("parseRipgrep(bad) err = nil, want error")
	}
}

// TestParseRipgrep_BytesPayload proves a match event whose lines arrive as a
// base64 bytes field (non-UTF-8 content) decodes into a non-blank frame with the
// invalid byte rendered as the replacement rune.
func TestParseRipgrep_BytesPayload(t *testing.T) {
	raw := `{"type":"match","data":{"path":{"text":"a.go"},"lines":{"bytes":"YWJj/2Zvbwo="},"line_number":3}}`
	got, err := parseRipgrep(raw)
	if err != nil {
		t.Fatalf("parseRipgrep() err = %v", err)
	}
	want := []fileGroup{{path: "a.go", lines: []grepLine{{num: 3, text: "abc\uFFFDfoo", isMatch: true}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseRipgrep()\n got = %+v\nwant = %+v", got, want)
	}
	if strings.TrimSpace(got[0].lines[0].text) == "" {
		t.Errorf("decoded frame is blank: %q", got[0].lines[0].text)
	}
	if !strings.Contains(got[0].lines[0].text, "foo") {
		t.Errorf("decoded frame missing %q: %q", "foo", got[0].lines[0].text)
	}
}

// grepLn renders one `grep --null` output line: path, NUL, line number, the match
// ":" or context "-" separator, then text.
func grepLn(path string, num int, sep, text string) string {
	return fmt.Sprintf("%s\x00%d%s%s", path, num, sep, text)
}

func TestParseGrep(t *testing.T) {
	raw := strings.Join([]string{
		grepLn("./a.go", 3, ":", "foo one"),
		grepLn("b.go", 9, "-", "ctx before"),
		grepLn("b.go", 10, ":", "foo two"),
		"--",
		grepLn("c.go", 5, ":", "foo three"),
		"",
	}, "\n")

	got := parseGrep(raw)
	want := []fileGroup{
		{path: "a.go", lines: []grepLine{{num: 3, text: "foo one", isMatch: true}}},
		{path: "b.go", lines: []grepLine{{num: 9, text: "ctx before", isMatch: false}, {num: 10, text: "foo two", isMatch: true}}},
		{path: "c.go", lines: []grepLine{{num: 5, text: "foo three", isMatch: true}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGrep() = %+v, want %+v", got, want)
	}
}

func TestParseGrep_NoMatch(t *testing.T) {
	if got := parseGrep(""); len(got) != 0 {
		t.Errorf("parseGrep(\"\") = %+v, want empty", got)
	}
}

// TestParseGrep_DelimitersNeedNoHeuristic proves --null removes the field-split
// ambiguity the old stat heuristic resolved: match and context text embedding ":"
// or "-", a dash-digit filename, and a filename literally containing ":N:" all
// parse from the NUL alone, with no filesystem probe.
func TestParseGrep_DelimitersNeedNoHeuristic(t *testing.T) {
	raw := strings.Join([]string{
		grepLn("pkg/x.go", 42, ":", "\tm := map[string]int{\"a:b\": 1}"),
		grepLn("pkg/t.go", 2, "-", `const layout = "15:04:05"`),
		grepLn("2024-01-migrate.go", 5, ":", "foo migrate"),
		grepLn("pkg:12:x.go", 1, ":", "foo"),
		"",
	}, "\n")

	got := parseGrep(raw)
	want := []fileGroup{
		{path: "pkg/x.go", lines: []grepLine{{num: 42, text: "\tm := map[string]int{\"a:b\": 1}", isMatch: true}}},
		{path: "pkg/t.go", lines: []grepLine{{num: 2, text: `const layout = "15:04:05"`, isMatch: false}}},
		{path: "2024-01-migrate.go", lines: []grepLine{{num: 5, text: "foo migrate", isMatch: true}}},
		{path: "pkg:12:x.go", lines: []grepLine{{num: 1, text: "foo", isMatch: true}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGrep()\n got = %+v\nwant = %+v", got, want)
	}
}

// TestRunCore_GrepValidatesSplits proves the finalized fallback output carries the
// right match count and no phantom section when context text embeds a delimiter or
// a filename embeds a dash-digit run.
func TestRunCore_GrepValidatesSplits(t *testing.T) {
	grepOut := strings.Join([]string{
		grepLn("./pkg/t.go", 2, "-", `const layout = "15:04:05"`),
		grepLn("./pkg/t.go", 3, ":", "foo layout"),
		grepLn("2024-01-migrate.go", 5, ":", "foo migrate"),
		grepLn("2024-01-migrate.go", 6, "-", "// trailing"),
		"",
	}, "\n")
	fake := func(context.Context, string, []string) (string, error) { return grepOut, nil }

	got, found, err := run(context.Background(), engineGrep, "grep", backend.Args{Query: "foo", IgnoreCase: true, Expand: 1}, fake)
	if err != nil {
		t.Fatalf("run() err = %v", err)
	}
	// The first group's first line is context: found must survive a context line
	// preceding the match, never reduce to a first-line check.
	if !found {
		t.Errorf("run() found = false, want true:\n%s", got)
	}
	if !strings.Contains(got, "— 2 matches in 2 files") {
		t.Errorf("run() count wrong:\n%s", got)
	}
	if n := strings.Count(got, "\n### "); n != 2 {
		t.Errorf("run() section count = %d, want 2 (phantom sections):\n%s", n, got)
	}
	if !strings.Contains(got, "### pkg/t.go:3") || !strings.Contains(got, "### 2024-01-migrate.go:5") {
		t.Errorf("run() missing real sections:\n%s", got)
	}
}

func TestReshape(t *testing.T) {
	groups := []fileGroup{
		{path: "a.go", lines: []grepLine{{num: 3, text: "foo one", isMatch: true}}},
		{path: "b.go", lines: []grepLine{{num: 9, text: "ctx", isMatch: false}, {num: 10, text: "bar", isMatch: true}}},
	}
	tests := []struct {
		name   string
		query  string
		eng    engine
		groups []fileGroup
		want   string
	}{
		{
			name:   "rg matches",
			query:  "foo",
			eng:    engineRipgrep,
			groups: groups,
			want:   "# grep: \"foo\" — 2 matches in 2 files\n\n### a.go:3\n→ [3] foo one\n\n### b.go:10\n  [9] ctx\n→ [10] bar\n",
		},
		{
			name:   "grep engine note",
			query:  "foo",
			eng:    engineGrep,
			groups: []fileGroup{{path: "a.go", lines: []grepLine{{num: 3, text: "foo one", isMatch: true}}}},
			want:   "# grep: \"foo\" — 1 matches in 1 files\n# engine: system grep (ripgrep not found); hidden and binary files skipped; .gitignore not applied\n\n### a.go:3\n→ [3] foo one\n",
		},
		{
			name:   "rg no match",
			query:  "zzz",
			eng:    engineRipgrep,
			groups: nil,
			want:   "# grep: \"zzz\" — no matches\n",
		},
		{
			name:   "grep no match note",
			query:  "zzz",
			eng:    engineGrep,
			groups: nil,
			want:   "# grep: \"zzz\" — no matches\n# engine: system grep (ripgrep not found); hidden and binary files skipped; .gitignore not applied\n",
		},
	}
	// Synthetic paths miss the empty cache, so frames stay bare (see the wants).
	files := anchor.NewFiles(t.TempDir())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reshape(tt.query, tt.eng, tt.groups, files); got != tt.want {
				t.Errorf("reshape()\n got = %q\nwant = %q", got, tt.want)
			}
		})
	}
}

// TestReshapeAnchorsFrames proves reshape stamps a content anchor onto each frame
// at generation time, hashed from the real file's line, and leaves a frame bare
// when its line is out of range. The golden asserts the exact anchored shape.
func TestReshapeAnchorsFrames(t *testing.T) {
	dir := t.TempDir()
	const src = "package x\nvar match = 1\nvar ctx = 2\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	files := anchor.NewFiles(dir)

	ctxHash := anchor.Of("var ctx = 2")
	matchHash := anchor.Of("var match = 1")
	groups := []fileGroup{{path: "a.go", lines: []grepLine{
		{num: 3, text: "var ctx = 2", isMatch: false},
		{num: 2, text: "var match = 1", isMatch: true},
		{num: 99, text: "out of range", isMatch: true},
	}}}

	want := "# grep: \"match\" — 2 matches in 1 files\n" +
		"\n### a.go:2,99\n" +
		"  [3#" + string(ctxHash) + "] var ctx = 2\n" +
		"→ [2#" + string(matchHash) + "] var match = 1\n" +
		"→ [99] out of range\n"
	if got := reshape("match", engineRipgrep, groups, files); got != want {
		t.Errorf("reshape()\n got = %q\nwant = %q", got, want)
	}
}

func TestRunCore(t *testing.T) {
	rgOut := strings.Join([]string{
		`{"type":"match","data":{"path":{"text":"nope/a.go"},"lines":{"text":"foo one\n"},"line_number":3,"submatches":[]}}`,
		"",
	}, "\n")
	rgCtxOut := strings.Join([]string{
		`{"type":"context","data":{"path":{"text":"nope/a.go"},"lines":{"text":"ctx before\n"},"line_number":2,"submatches":[]}}`,
		`{"type":"match","data":{"path":{"text":"nope/a.go"},"lines":{"text":"foo one\n"},"line_number":3,"submatches":[]}}`,
		"",
	}, "\n")
	grepOut := "nope/a.go\x003:foo one\n"

	tests := []struct {
		name      string
		eng       engine
		args      backend.Args
		out       string
		runErr    error
		wantErr   bool
		wantFound bool
		contains  []string
	}{
		{"rg branch", engineRipgrep, backend.Args{Query: "foo", IgnoreCase: true}, rgOut, nil, false, true, []string{"### nope/a.go:3", "→ [3] foo one"}},
		{"grep branch", engineGrep, backend.Args{Query: "foo", IgnoreCase: true}, grepOut, nil, false, true, []string{"### nope/a.go:3", "→ [3] foo one", "system grep"}},
		{"clean no-match", engineRipgrep, backend.Args{Query: "foo", IgnoreCase: true}, "", nil, false, false, []string{`# grep: "foo" — no matches`}},
		{"context precedes match still found", engineRipgrep, backend.Args{Query: "foo", IgnoreCase: true, Expand: 1}, rgCtxOut, nil, false, true, []string{"  [2] ctx before", "→ [3] foo one"}},
		// found is structural: a Budget so small the cap byte-cuts the header
		// mid-word must not flip either verdict.
		{"match found survives tiny budget", engineRipgrep, backend.Args{Query: "foo", IgnoreCase: true, Budget: 1}, rgOut, nil, false, true, []string{"omitted"}},
		{"no-match stays not-found under tiny budget", engineRipgrep, backend.Args{Query: "foo", IgnoreCase: true, Budget: 1}, "", nil, false, false, nil},
		{"runner error propagates", engineRipgrep, backend.Args{Query: "foo", Word: true}, "", errors.New("boom"), true, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := func(context.Context, string, []string) (string, error) { return tt.out, tt.runErr }
			got, found, err := run(context.Background(), tt.eng, "bin", tt.args, fake)
			if (err != nil) != tt.wantErr {
				t.Fatalf("run() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if found != tt.wantFound {
				t.Errorf("run() found = %v, want %v:\n%s", found, tt.wantFound, got)
			}
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("run() output missing %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestRunCore_GlobFailFastSkipsRunner(t *testing.T) {
	called := false
	fake := func(context.Context, string, []string) (string, error) {
		called = true
		return "", nil
	}
	_, _, err := run(context.Background(), engineGrep, "grep", backend.Args{Query: "foo", Glob: "{a,b}"}, fake)
	if err == nil {
		t.Fatal("run() err = nil, want untranslatable-glob error")
	}
	if !strings.Contains(err.Error(), "cannot translate glob") {
		t.Errorf("run() err = %v, want translate-glob message", err)
	}
	if called {
		t.Error("runner invoked despite argv build failure")
	}
}

func TestResolveEngine_Neither(t *testing.T) {
	t.Setenv("PATH", "")
	_, _, err := resolveEngine()
	if err == nil {
		t.Fatal("resolveEngine() err = nil, want error when neither engine on PATH")
	}
	if !strings.Contains(err.Error(), "install ripgrep") {
		t.Errorf("resolveEngine() err = %v, want install hint", err)
	}
}

func TestAnchorGrepArgs(t *testing.T) {
	sub, missing, file := anchorDirs(t)
	parent := filepath.Dir(sub)
	tests := []struct {
		name      string
		args      backend.Args
		wantGlob  string
		wantScope string
	}{
		{"anchored existing dir → scope + rest", backend.Args{Query: "foo", Glob: sub + "/*.go"}, "*.go", sub},
		{"nonexistent prefix → unchanged", backend.Args{Query: "foo", Glob: missing + "/*.go"}, missing + "/*.go", ""},
		{"explicit scope composes onto join", backend.Args{Query: "foo", Glob: "pkg/*.go", Scope: parent}, "*.go", sub},
		{"explicit scope nonexistent join → unchanged", backend.Args{Query: "foo", Glob: "nope/*.go", Scope: parent}, "nope/*.go", parent},
		{"literal file glob → parent scope + basename", backend.Args{Query: "foo", Glob: file}, "file.go", sub},
		{"slash-less glob → unchanged", backend.Args{Query: "foo", Glob: "*.go"}, "*.go", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AnchorGrepArgs(tt.args)
			if got.Glob != tt.wantGlob || got.Scope != tt.wantScope {
				t.Errorf("AnchorGrepArgs glob=%q scope=%q, want glob=%q scope=%q", got.Glob, got.Scope, tt.wantGlob, tt.wantScope)
			}
		})
	}
}

func TestValidateContext(t *testing.T) {
	tests := []struct {
		name    string
		args    backend.Args
		wantErr bool
	}{
		{"zero ok", backend.Args{}, false},
		{"positive ok", backend.Args{After: 2, Before: 3, Context: 4}, false},
		{"at max ok", backend.Args{Context: maxContext}, false},
		{"over max errors", backend.Args{Before: maxContext + 1}, true},
		{"negative after errors", backend.Args{After: -1}, true},
		{"negative context errors", backend.Args{Context: -5}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateContext(tt.args); (err != nil) != tt.wantErr {
				t.Errorf("validateContext(%+v) err = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
		})
	}
}

// TestRunFoundness_Live proves run's found verdict is structural: with
// Budget 1 the finalized output is byte-cut to garbage either way, yet found
// still reports whether the engine matched. It skips when no engine is on PATH.
func TestRunFoundness_Live(t *testing.T) {
	_, rgErr := exec.LookPath("rg")
	_, grepErr := exec.LookPath("grep")
	if rgErr != nil && grepErr != nil {
		t.Skip("neither rg nor grep on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("var needle = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	tests := []struct {
		name      string
		query     string
		wantFound bool
	}{
		{"present needle found", "needle", true},
		{"absent needle not found", "zzz-absent", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eng, bin, err := resolveEngine()
			if err != nil {
				t.Fatalf("resolveEngine() err = %v", err)
			}
			_, found, err := run(context.Background(), eng, bin, backend.Args{Query: tt.query, IgnoreCase: true, Budget: 1}, execEngine)
			if err != nil {
				t.Fatalf("run() err = %v", err)
			}
			if found != tt.wantFound {
				t.Errorf("run() found = %v, want %v", found, tt.wantFound)
			}
		})
	}
}

// TestRun_LiveRipgrepRegex proves an anchored regex matches where the same query
// as a --fixed-strings literal cannot: "^func " hits the line starting with func,
// but no line contains the literal characters "^func ". It skips when rg is absent.
func TestRun_LiveRipgrepRegex(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	dir := t.TempDir()
	src := "package x\n// see func usage\nfunc Foo() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	got, err := Run(context.Background(), backend.Args{Query: "^func ", Regex: true})
	if err != nil {
		t.Fatalf("Run(regex) err = %v", err)
	}
	if !strings.Contains(got, "### sample.go:3") {
		t.Errorf("Run(regex) missing anchored func line:\n%s", got)
	}

	// Same query, forced onto the engine but as a literal: --fixed-strings makes
	// "^func " match nothing, so an anchored literal 0-matches.
	lit, err := Run(context.Background(), backend.Args{Query: "^func ", IgnoreCase: true})
	if err != nil {
		t.Fatalf("Run(literal) err = %v", err)
	}
	if !strings.Contains(lit, "no matches") {
		t.Errorf("Run(literal) expected no-match for the anchored literal:\n%s", lit)
	}
}

// TestRun_LiveRipgrepPaths proves a multi-file run returns hits only from the
// named files, never a sibling the search did not name. It skips when rg is absent.
func TestRun_LiveRipgrepPaths(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	dir := t.TempDir()
	for _, f := range []string{"a.go", "b.go", "c.go"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("var needle = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)

	got, err := Run(context.Background(), backend.Args{Query: "needle", Paths: []string{"a.go", "b.go"}})
	if err != nil {
		t.Fatalf("Run(paths) err = %v", err)
	}
	if !strings.Contains(got, "### a.go:") || !strings.Contains(got, "### b.go:") {
		t.Errorf("Run(paths) missing named-file sections:\n%s", got)
	}
	if strings.Contains(got, "c.go") {
		t.Errorf("Run(paths) leaked an unnamed file:\n%s", got)
	}

	// Paths + --glob: rg's -g does not filter the explicit files, so the Go-side
	// prefilter must drop notes.txt while keeping the .go operands.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("var needle = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	globbed, err := Run(context.Background(), backend.Args{Query: "needle", Paths: []string{"a.go", "notes.txt"}, Glob: "*.go"})
	if err != nil {
		t.Fatalf("Run(paths+glob) err = %v", err)
	}
	if !strings.Contains(globbed, "### a.go:") {
		t.Errorf("Run(paths+glob) missing .go section:\n%s", globbed)
	}
	if strings.Contains(globbed, "notes.txt") {
		t.Errorf("Run(paths+glob) leaked a non-.go operand:\n%s", globbed)
	}
}

// TestRun_LiveEnginesAgreeOnGlobPaths drives both real engines over one fixture of
// file and directory operands with --glob and proves they return the identical hit
// set: --glob's Go-side prefilter (rg) and --include (grep) must not diverge.
func TestRun_LiveEnginesAgreeOnGlobPaths(t *testing.T) {
	rgBin, rgErr := exec.LookPath("rg")
	grepBin, grepErr := exec.LookPath("grep")
	if rgErr != nil || grepErr != nil {
		t.Skip("need both rg and grep on PATH to compare engines")
	}
	dir := t.TempDir()
	for rel, content := range map[string]string{
		"a.go":      "var needle = 1\n",
		"b.txt":     "var needle = 1\n",
		"sub/c.go":  "var needle = 1\n",
		"sub/d.txt": "var needle = 1\n",
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)

	args := backend.Args{Query: "needle", Paths: []string{"a.go", "b.txt", "sub"}, Glob: "*.go"}
	rgOut, rgFound, err := run(context.Background(), engineRipgrep, rgBin, args, execEngine)
	if err != nil {
		t.Fatalf("rg run: %v", err)
	}
	grepOut, grepFound, err := run(context.Background(), engineGrep, grepBin, args, execEngine)
	if err != nil {
		t.Fatalf("grep run: %v", err)
	}
	if !rgFound || !grepFound {
		t.Fatalf("found mismatch: rg=%v grep=%v", rgFound, grepFound)
	}
	for name, out := range map[string]string{"rg": rgOut, "grep": grepOut} {
		if !strings.Contains(out, "### a.go:") || !strings.Contains(out, "### sub/c.go:") {
			t.Errorf("%s missing expected .go sections:\n%s", name, out)
		}
		if strings.Contains(out, "b.txt") || strings.Contains(out, "d.txt") {
			t.Errorf("%s leaked a non-.go file past --glob:\n%s", name, out)
		}
	}
}

// TestRun_LiveEnginesAgreeOnZeroGlob proves a --glob matching zero files yields the
// same clean no-match from both engines — the case where an older rg exits 2 with
// "No files were searched" while grep exits 0, reconciled by ripgrepNoFiles.
func TestRun_LiveEnginesAgreeOnZeroGlob(t *testing.T) {
	rgBin, rgErr := exec.LookPath("rg")
	grepBin, grepErr := exec.LookPath("grep")
	if rgErr != nil || grepErr != nil {
		t.Skip("need both rg and grep on PATH to compare engines")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("var needle = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	args := backend.Args{Query: "needle", Glob: "*.nomatchext"}
	rgOut, rgFound, err := run(context.Background(), engineRipgrep, rgBin, args, execEngine)
	if err != nil {
		t.Fatalf("rg run: %v", err)
	}
	grepOut, grepFound, err := run(context.Background(), engineGrep, grepBin, args, execEngine)
	if err != nil {
		t.Fatalf("grep run: %v", err)
	}
	if rgFound || grepFound {
		t.Errorf("expected no match from both: rg=%v grep=%v", rgFound, grepFound)
	}
	if !strings.Contains(rgOut, "no matches") || !strings.Contains(grepOut, "no matches") {
		t.Errorf("expected a no-match header from both:\nrg=%q\ngrep=%q", rgOut, grepOut)
	}
}

// TestRun_LiveRipgrep drives the real rg binary end to end — argv, --json parse,
// reshape, and content anchoring — proving the finalized output carries anchored
// frames. It skips when rg is absent.
func TestRun_LiveRipgrep(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	dir := t.TempDir()
	src := "package x\n\nfunc Foo() {}\nvar foo = 1\n"
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	out, err := Run(context.Background(), backend.Args{Query: "foo", IgnoreCase: true})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if !strings.Contains(out, "### sample.go:") {
		t.Errorf("Run() missing section header:\n%s", out)
	}
	// Case-insensitive: both Foo (line 3) and foo (line 4) hit, each anchored.
	anchored := regexp.MustCompile(`→ \[\d+#\w+\] `)
	if got := len(anchored.FindAllString(out, -1)); got != 2 {
		t.Errorf("Run() anchored frames = %d, want 2:\n%s", got, out)
	}
}
