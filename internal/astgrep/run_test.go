package astgrep

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

// fakeAstGrep installs an executable named "ast-grep" on PATH that emits canned
// --json=stream output. An `outline` run emits one canned outline file object; a
// preview run emits one JSON match per file in files (space-separated); an apply
// run (argv carries -U) emits nothing. vendor.Resolve finds it via
// LookPath("ast-grep").
func fakeAstGrep(t *testing.T, files []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ast-grep script is POSIX-only")
	}
	dir := t.TempDir()
	var lines strings.Builder
	for i, f := range files {
		// 0-based line i; matches the ast-grep convention the renderer shifts +1.
		// lines carries the raw source line the anchor hashes, as real ast-grep does.
		fmt.Fprintf(&lines, `{"file":"%s","text":"old%d","lines":"old%d","replacement":"new%d","range":{"start":{"line":%d},"end":{"line":%d}}}`+"\n", f, i, i, i, i, i)
	}
	// 0-based struct line 4 and member line 5 render as the 1-based L5 and L6.
	const outline = `{"path":"x.go","language":"Go","items":[{"symbolType":"struct","name":"X","signature":"type X struct {","isExported":true,"range":{"start":{"line":4}},"members":[{"symbolType":"field","name":"Y","signature":"Y int","range":{"start":{"line":5}}}]}]}`
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = outline ]; then\n" +
		"cat <<'EOF'\n" + outline + "\nEOF\n" +
		"exit 0\n" +
		"fi\n" +
		"for a in \"$@\"; do [ \"$a\" = \"-U\" ] && exit 0; done\n" +
		"cat <<'EOF'\n" + lines.String() + "EOF\n"
	path := filepath.Join(dir, "ast-grep")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // fake engine must be owner-executable
		t.Fatalf("write fake ast-grep: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func filesN(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("f%d.go", i)
	}
	return out
}

func TestRunReplacePreviewLeavesDiff(t *testing.T) {
	fakeAstGrep(t, []string{"a.go", "b.go"})
	got, err := Run(context.Background(), backend.OpReplace, backend.Args{Pattern: "old($A)", Rewrite: "new($A)"})
	if err != nil {
		t.Fatalf("Run preview: %v", err)
	}
	if !strings.HasPrefix(got, "# 2 matches across 2 files\n") {
		t.Errorf("preview header wrong:\n%s", got)
	}
	// Line 0→1, anchored by Of("old0") = jtrj.
	if !strings.Contains(got, "a.go:1#jtrj\n- old0\n+ new0\n") {
		t.Errorf("preview missing first hit (1-based line):\n%s", got)
	}
}

func TestRunReplaceNoMatch(t *testing.T) {
	fakeAstGrep(t, nil) // empty stream → no matches
	got, err := Run(context.Background(), backend.OpReplace, backend.Args{Pattern: "missing($A)", Rewrite: "x($A)"})
	if err != nil {
		t.Fatalf("Run no-match: %v", err)
	}
	if !strings.HasPrefix(got, "# no matches for missing($A)") {
		t.Errorf("no-match message wrong: %q", got)
	}
	if !strings.Contains(got, "--debug-query=ast") {
		t.Errorf("no-match missing debug-query hint: %q", got)
	}
}

func TestRunReplaceApplyUnderCap(t *testing.T) {
	fakeAstGrep(t, []string{"a.go", "b.go", "c.go"})
	got, err := Run(context.Background(), backend.OpReplace, backend.Args{Pattern: "old($A)", Rewrite: "new($A)", Apply: true})
	if err != nil {
		t.Fatalf("Run apply: %v", err)
	}
	if got != "# applied 3 rewrites across 3 files\n" {
		t.Errorf("apply summary wrong: %q", got)
	}
}

func TestRunReplaceApplyOverCapBlocked(t *testing.T) {
	fakeAstGrep(t, filesN(applyFileCap+1)) // 21 distinct files > cap 20
	_, err := Run(context.Background(), backend.OpReplace, backend.Args{Pattern: "old($A)", Rewrite: "new($A)", Apply: true})
	if err == nil {
		t.Fatal("apply over cap without --force must error")
	}
	if !strings.Contains(err.Error(), "exceeding the cap of 20") {
		t.Errorf("cap error wrong: %v", err)
	}
}

func TestRunReplaceApplyOverCapForced(t *testing.T) {
	fakeAstGrep(t, filesN(applyFileCap+1))
	got, err := Run(context.Background(), backend.OpReplace, backend.Args{Pattern: "old($A)", Rewrite: "new($A)", Apply: true, Force: true})
	if err != nil {
		t.Fatalf("Run apply --force: %v", err)
	}
	if got != fmt.Sprintf("# applied %d rewrites across %d files\n", applyFileCap+1, applyFileCap+1) {
		t.Errorf("forced apply summary wrong: %q", got)
	}
}

func TestRunStructural(t *testing.T) {
	fakeAstGrep(t, []string{"a.go", "a.go"}) // two hits, one file
	got, err := Run(context.Background(), backend.OpStructural, backend.Args{Query: "old($A)"})
	if err != nil {
		t.Fatalf("Run structural: %v", err)
	}
	// 0-based lines 0 and 1 render as the 1-based L1 and L2, anchored by
	// Of("old0") = jtrj and Of("old1") = rv55.
	if !strings.Contains(got, "a.go:L1#jtrj  old0") || !strings.Contains(got, "a.go:L2#rv55  old1") {
		t.Errorf("structural list wrong:\n%s", got)
	}
}

func TestRunStructOutline(t *testing.T) {
	fakeAstGrep(t, nil)
	// Terse default: top-level struct only, its member collapsed to a count.
	got, err := Run(context.Background(), backend.OpStructOutline, backend.Args{Path: "x.go"})
	if err != nil {
		t.Fatalf("Run struct-outline: %v", err)
	}
	if !strings.Contains(got, "# x.go\n") || !strings.Contains(got, "L5  type X struct {  (+1 member)\n") {
		t.Errorf("terse struct-outline render wrong:\n%s", got)
	}
	if strings.Contains(got, "L6  Y int") {
		t.Errorf("terse struct-outline should hide the member:\n%s", got)
	}
	// --deep renders the member: 0-based struct line 4 and member line 5 as L5 and the indented L6.
	deep, err := Run(context.Background(), backend.OpStructOutline, backend.Args{Path: "x.go", Deep: true})
	if err != nil {
		t.Fatalf("Run struct-outline deep: %v", err)
	}
	if !strings.Contains(deep, "L5  type X struct {\n") || !strings.Contains(deep, "\n  L6  Y int\n") {
		t.Errorf("deep struct-outline render wrong:\n%s", deep)
	}
}

func TestRunStructOutlineBudget(t *testing.T) {
	fakeAstGrep(t, nil)
	got, err := Run(context.Background(), backend.OpStructOutline, backend.Args{Path: "x.go", Budget: 1})
	if err != nil {
		t.Fatalf("Run struct-outline budget: %v", err)
	}
	if !strings.Contains(got, "omitted — re-run with a larger --budget") {
		t.Errorf("tiny budget must show the overflow footer:\n%s", got)
	}
}

func TestRunUnsupportedOp(t *testing.T) {
	if _, err := Run(context.Background(), backend.OpGrep, backend.Args{}); err == nil {
		t.Fatal("Run: want error for non-ast-grep op")
	}
}
