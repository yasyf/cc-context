package ripgrep

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

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
	tests := []struct {
		name    string
		args    backend.Args
		want    []string
		wantErr bool
	}{
		{"bare", backend.Args{Query: "foo"}, []string{"-rnFI", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "."}, false},
		{"ignore-case", backend.Args{Query: "foo", IgnoreCase: true}, []string{"-rnFI", "-i", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "."}, false},
		{"word + expand", backend.Args{Query: "foo", Word: true, Expand: 2}, []string{"-rnFI", "-w", "-C", "2", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "."}, false},
		{"basename glob", backend.Args{Query: "foo", Glob: "*.go"}, []string{"-rnFI", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", "."}, false},
		{"dir recurse glob", backend.Args{Query: "foo", Glob: "src/**"}, []string{"-rnFI", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "src"}, false},
		{"dir + ext glob", backend.Args{Query: "foo", Glob: "src/**/*.go"}, []string{"-rnFI", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", "src"}, false},
		{"scope", backend.Args{Query: "foo", Scope: "internal"}, []string{"-rnFI", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "internal"}, false},
		{"flag-like scope lands after --", backend.Args{Query: "foo", Scope: "--hidden"}, []string{"-rnFI", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "--hidden"}, false},
		{"scope + basename glob", backend.Args{Query: "foo", Glob: "*.go", Scope: "internal"}, []string{"-rnFI", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "--include=*.go", "-e", "foo", "--", "internal"}, false},
		{"leading-dash pattern", backend.Args{Query: "-foo"}, []string{"-rnFI", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "-foo", "--", "."}, false},
		{"regex swaps -rnFI for -rnEI", backend.Args{Query: "foo", Regex: true}, []string{"-rnEI", "--exclude-dir=.[!./]*", "--exclude=.[!./]*", "-e", "foo", "--", "."}, false},
		{"paths omit excludes", backend.Args{Query: "foo", Paths: []string{"a.go", "b.go"}}, []string{"-rnFI", "-e", "foo", "--", "a.go", "b.go"}, false},
		{"regex + paths omit excludes", backend.Args{Query: "^func ", Regex: true, Paths: []string{"a.go"}}, []string{"-rnEI", "-e", "^func ", "--", "a.go"}, false},
		{"scope + paths omit excludes", backend.Args{Query: "foo", Scope: "internal", Paths: []string{"a.go"}}, []string{"-rnFI", "-e", "foo", "--", "internal", "a.go"}, false},
		{"paths + glob errors", backend.Args{Query: "foo", Paths: []string{"a.go"}, Glob: "*.go"}, nil, true},
		{"scope + dir glob fails", backend.Args{Query: "foo", Glob: "src/**", Scope: "internal"}, nil, true},
		{"brace glob fails", backend.Args{Query: "foo", Glob: "{a,b}/**"}, nil, true},
		{"mid-path wildcard fails", backend.Args{Query: "foo", Glob: "src/*/x.go"}, nil, true},
		{"double-star-slash-star fails", backend.Args{Query: "foo", Glob: "**/*.go"}, nil, true},
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

func TestBuildArgvRejectsPathsAndGlob(t *testing.T) {
	args := backend.Args{Query: "foo", Glob: "*.go", Paths: []string{"a.go"}}
	for _, eng := range []engine{engineRipgrep, engineGrep} {
		if _, err := buildArgv(eng, args); err == nil {
			t.Errorf("buildArgv(%v) err = nil, want paths-plus-glob error", eng)
		}
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
		{"leading double-star", "**/*.go", "", "", true},
		{"deep tail", "a/**/b/c.go", "", "", true},
		{"wildcard dir on recurse", "sr*/**", "", "", true},
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

func TestParseGrep(t *testing.T) {
	raw := strings.Join([]string{
		"./a.go:3:foo one",
		"b.go-9-ctx before",
		"b.go:10:foo two",
		"--",
		"c.go:5:foo three",
		"",
	}, "\n")

	got := parseGrep(raw, true, allExist)
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
	if got := parseGrep("", true, allExist); len(got) != 0 {
		t.Errorf("parseGrep(\"\") = %+v, want empty", got)
	}
}

func TestParseGrep_ColonInText(t *testing.T) {
	got := parseGrep("pkg/x.go:42:\tm := map[string]int{\"a:b\": 1}", true, allExist)
	want := []fileGroup{{path: "pkg/x.go", lines: []grepLine{{num: 42, text: "\tm := map[string]int{\"a:b\": 1}", isMatch: true}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGrep() = %+v, want %+v", got, want)
	}
}

// allExist is the always-true path seam for parser tests whose file paths are
// synthetic; the filesystem-validated cases live in TestParseGrep_ValidatedSplits.
func allExist(string) bool { return true }

// mkFixture writes an empty file (creating parent dirs) so os.Stat validation in
// parseGrep resolves a candidate path to a real file.
func mkFixture(t *testing.T, dir, rel string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestParseGrep_ValidatedSplits drives the real os.Stat seam against a temp tree,
// pinning the three shapes a purely lexical split misparses: a context line whose
// text embeds ":04:" (`15:04:05`), a context line whose text embeds a 3-index
// slice ":2:" (`data[0:2:4]`), and a dash-digit filename whose match and context
// lines carry "-01-" before the real "-N-" boundary.
func TestParseGrep_ValidatedSplits(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"pkg/t.go", "pkg/x.go", "2024-01-migrate.go"} {
		mkFixture(t, dir, rel)
	}
	t.Chdir(dir)

	raw := strings.Join([]string{
		`./pkg/t.go-2-const layout = "15:04:05"`,
		`./pkg/t.go:3:foo layout`,
		"./pkg/x.go-11-\tv := data[0:2:4]",
		`./pkg/x.go:12:foo slice`,
		`2024-01-migrate.go:5:foo migrate`,
		`2024-01-migrate.go-6-// trailing`,
		"",
	}, "\n")

	got := parseGrep(raw, true, statRegular)
	want := []fileGroup{
		{path: "pkg/t.go", lines: []grepLine{
			{num: 2, text: `const layout = "15:04:05"`, isMatch: false},
			{num: 3, text: "foo layout", isMatch: true},
		}},
		{path: "pkg/x.go", lines: []grepLine{
			{num: 11, text: "\tv := data[0:2:4]", isMatch: false},
			{num: 12, text: "foo slice", isMatch: true},
		}},
		{path: "2024-01-migrate.go", lines: []grepLine{
			{num: 5, text: "foo migrate", isMatch: true},
			{num: 6, text: "// trailing", isMatch: false},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGrep()\n got = %+v\nwant = %+v", got, want)
	}
}

// TestParseGrep_DirStealsSplit reproduces the split-validation footgun: a
// directory named like a leading path prefix (`pkg`) satisfies mere existence, so
// grep output for a colon-named regular file (`pkg:12:x.go`) is misattributed to
// the directory (path "pkg", line 12, text "x.go:1:foo"). Requiring a regular
// file rejects the directory candidate and lands the true file split.
func TestParseGrep_DirStealsSplit(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg:12:x.go"), []byte("foo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	got := parseGrep("./pkg:12:x.go:1:foo", false, statRegular)
	want := []fileGroup{{path: "pkg:12:x.go", lines: []grepLine{{num: 1, text: "foo", isMatch: true}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGrep()\n got = %+v\nwant = %+v", got, want)
	}
}

// TestRunCore_GrepValidatesSplits proves the finalized fallback output carries the
// right match count and no phantom section when context text embeds a delimiter or
// a filename embeds a dash-digit run.
func TestRunCore_GrepValidatesSplits(t *testing.T) {
	dir := t.TempDir()
	mkFixture(t, dir, "pkg/t.go")
	mkFixture(t, dir, "2024-01-migrate.go")
	t.Chdir(dir)

	grepOut := strings.Join([]string{
		`./pkg/t.go-2-const layout = "15:04:05"`,
		`./pkg/t.go:3:foo layout`,
		`2024-01-migrate.go:5:foo migrate`,
		`2024-01-migrate.go-6-// trailing`,
		"",
	}, "\n")
	fake := func(context.Context, string, []string) (string, error) { return grepOut, nil }

	got, err := run(context.Background(), engineGrep, "grep", backend.Args{Query: "foo", IgnoreCase: true, Expand: 1}, fake, statRegular)
	if err != nil {
		t.Fatalf("run() err = %v", err)
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
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reshape(tt.query, tt.eng, tt.groups); got != tt.want {
				t.Errorf("reshape()\n got = %q\nwant = %q", got, tt.want)
			}
		})
	}
}

func TestReshapeFramesMatchRenderRegexes(t *testing.T) {
	// The section headers and frames reshape emits must satisfy the exact regexes
	// render.annotateGrep anchors on, or the output would pass through unanchored.
	sectionRe := regexp.MustCompile(`^### (.+?):\d[\d,]*(?:[ \t].*)?$`)
	frameRe := regexp.MustCompile(`^(\s*→?\s*)\[(\d+)(?:-(\d+))?\](\s)`)
	out := reshape("foo", engineRipgrep, []fileGroup{
		{path: "internal/backend/backend.go", lines: []grepLine{{num: 9, text: "ctx", isMatch: false}, {num: 10, text: "match", isMatch: true}}},
	})
	var sawSection, sawMatchFrame, sawCtxFrame bool
	for _, line := range strings.Split(out, "\n") {
		switch {
		case sectionRe.MatchString(line):
			sawSection = true
			if got := sectionRe.FindStringSubmatch(line)[1]; got != "internal/backend/backend.go" {
				t.Errorf("section path = %q", got)
			}
		case strings.HasPrefix(line, "→ ") && frameRe.MatchString(line):
			sawMatchFrame = true
		case strings.HasPrefix(line, "  [") && frameRe.MatchString(line):
			sawCtxFrame = true
		}
	}
	if !sawSection || !sawMatchFrame || !sawCtxFrame {
		t.Errorf("regex coverage: section=%v matchFrame=%v ctxFrame=%v\n%s", sawSection, sawMatchFrame, sawCtxFrame, out)
	}
}

func TestRunCore(t *testing.T) {
	rgOut := strings.Join([]string{
		`{"type":"match","data":{"path":{"text":"nope/a.go"},"lines":{"text":"foo one\n"},"line_number":3,"submatches":[]}}`,
		"",
	}, "\n")
	grepOut := "nope/a.go:3:foo one\n"

	tests := []struct {
		name     string
		eng      engine
		args     backend.Args
		out      string
		runErr   error
		wantErr  bool
		contains []string
	}{
		{"rg branch", engineRipgrep, backend.Args{Query: "foo", IgnoreCase: true}, rgOut, nil, false, []string{"### nope/a.go:3", "→ [3] foo one"}},
		{"grep branch", engineGrep, backend.Args{Query: "foo", IgnoreCase: true}, grepOut, nil, false, []string{"### nope/a.go:3", "→ [3] foo one", "system grep"}},
		{"clean no-match", engineRipgrep, backend.Args{Query: "foo", IgnoreCase: true}, "", nil, false, []string{`# grep: "foo" — no matches`}},
		{"runner error propagates", engineRipgrep, backend.Args{Query: "foo", Word: true}, "", errors.New("boom"), true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := func(context.Context, string, []string) (string, error) { return tt.out, tt.runErr }
			got, err := run(context.Background(), tt.eng, "bin", tt.args, fake, allExist)
			if (err != nil) != tt.wantErr {
				t.Fatalf("run() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
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
	_, err := run(context.Background(), engineGrep, "grep", backend.Args{Query: "foo", Glob: "{a,b}"}, fake, allExist)
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

func TestHandles(t *testing.T) {
	tests := []struct {
		name string
		args backend.Args
		want bool
	}{
		{"bare literal stays tilth", backend.Args{Query: "foo"}, false},
		{"glob-only stays tilth", backend.Args{Query: "foo", Glob: "*.go"}, false},
		{"scope-only stays tilth", backend.Args{Query: "foo", Scope: "internal"}, false},
		{"ignore-case routes to engine", backend.Args{Query: "foo", IgnoreCase: true}, true},
		{"word routes to engine", backend.Args{Query: "foo", Word: true}, true},
		{"regex routes to engine", backend.Args{Query: "foo", Regex: true}, true},
		{"paths route to engine", backend.Args{Query: "foo", Paths: []string{"a.go"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Handles(tt.args); got != tt.want {
				t.Errorf("Handles(%+v) = %v, want %v", tt.args, got, tt.want)
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
