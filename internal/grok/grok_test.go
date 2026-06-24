package grok

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

func TestIsNotFoundText(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"stderr miss", "grok error: not found: Router", true},
		{"mcp error text", "not found: Router\ngrok error: not found: Router", true},
		{"a real hit", "# grok: NewRouter [internal/router.go:10]", false},
		{"empty", "", false},
		{"unrelated error", "some other failure", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsNotFoundText(tt.text); got != tt.want {
				t.Errorf("IsNotFoundText(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

// TestRunFallsBackToAstGrepTypeDecl proves a tilth grok miss on a Go top-level
// type is recovered via the ast-grep structural fallback.
func TestRunFallsBackToAstGrepTypeDecl(t *testing.T) {
	fakeAstGrepMatch(t, "internal/backend/tilth.go")
	tilth := fakeTilthMiss(t, exitNonZero)

	got, err := Run(context.Background(), tilth, []string{"grok", "Router"}, backend.Args{Query: "Router"})
	if err != nil {
		t.Fatalf("Run() err: %v", err)
	}
	if !strings.Contains(got, "ast-grep type fallback") {
		t.Errorf("output missing fallback marker:\n%s", got)
	}
	if !strings.Contains(got, "internal/backend/tilth.go") {
		t.Errorf("output missing resolved location:\n%s", got)
	}
}

// TestRunExit0SentinelFallsBack proves the exit-0 stdout sentinel (CLI/MCP error
// parity bug) is normalized into the not-found path, then recovered by ast-grep.
func TestRunExit0SentinelFallsBack(t *testing.T) {
	fakeAstGrepMatch(t, "internal/backend/tilth.go")
	tilth := fakeTilthMiss(t, exitZeroStdout)

	got, err := Run(context.Background(), tilth, []string{"grok", "Router"}, backend.Args{Query: "Router"})
	if err != nil {
		t.Fatalf("Run() err: %v", err)
	}
	if !strings.Contains(got, "ast-grep type fallback") {
		t.Errorf("exit-0 sentinel was not normalized to the fallback path:\n%s", got)
	}
}

// TestRunStillErrorsWhenAstGrepAlsoMisses proves a genuinely-absent symbol is
// never masked: when both tilth and ast-grep miss, Run returns a loud error.
func TestRunStillErrorsWhenAstGrepAlsoMisses(t *testing.T) {
	fakeAstGrepEmpty(t)
	tilth := fakeTilthMiss(t, exitNonZero)

	_, err := Run(context.Background(), tilth, []string{"grok", "ZZZNope"}, backend.Args{Query: "ZZZNope"})
	if err == nil {
		t.Fatal("Run() err = nil, want a loud not-found error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error does not surface the miss: %v", err)
	}
}

// TestRunPassesThroughAHit proves a real tilth hit is returned untouched (no
// fallback, no error).
func TestRunPassesThroughAHit(t *testing.T) {
	tilth := writeFakeBin(t, "tilth",
		"#!/bin/sh\nprintf '# grok: NewRouter [internal/router.go:10]\\n'\n")

	got, err := Run(context.Background(), tilth, []string{"grok", "NewRouter"}, backend.Args{Query: "NewRouter"})
	if err != nil {
		t.Fatalf("Run() err: %v", err)
	}
	if !strings.Contains(got, "# grok: NewRouter") {
		t.Errorf("real hit not passed through:\n%s", got)
	}
	if strings.Contains(got, "ast-grep") {
		t.Errorf("a real hit must not trigger the fallback:\n%s", got)
	}
}

// TestFallbackThreadsScopeAsPath proves a grok --scope is forwarded to the
// ast-grep fallback as a path positional rather than silently dropped: the fake
// ast-grep echoes its argv into the match's file so the scope is observable.
func TestFallbackThreadsScopeAsPath(t *testing.T) {
	fakeAstGrep(t, "#!/bin/sh\n"+
		"printf '{\"file\":\"%s\",\"text\":\"type Router struct {\",\"range\":{\"start\":{\"line\":0},\"end\":{\"line\":0}}}\\n' \"$*\"\n")

	got, err := FallbackTypeDecl(context.Background(),
		backend.Args{Query: "Router", Scope: "internal/x"}, errors.New("miss"))
	if err != nil {
		t.Fatalf("FallbackTypeDecl() err: %v", err)
	}
	if !strings.Contains(got, "internal/x") {
		t.Errorf("scope not threaded into ast-grep argv:\n%s", got)
	}
}

func TestFallbackTypeDeclErrorsWhenAstGrepMisses(t *testing.T) {
	fakeAstGrepEmpty(t)
	miss := errors.New("tilth miss")
	_, err := FallbackTypeDecl(context.Background(), backend.Args{Query: "Gone"}, miss)
	if !errors.Is(err, miss) {
		t.Fatalf("FallbackTypeDecl() err = %v, want it to wrap the original miss", err)
	}
}

type tilthMissMode int

const (
	exitNonZero tilthMissMode = iota
	exitZeroStdout
)

// fakeTilthMiss writes a fake tilth that reproduces a grok miss in the given mode (nonzero-exit stderr or exit-0 stdout).
func fakeTilthMiss(t *testing.T, mode tilthMissMode) string {
	t.Helper()
	switch mode {
	case exitNonZero:
		return writeFakeBin(t, "tilth",
			"#!/bin/sh\necho 'grok error: not found: x' 1>&2\nexit 2\n")
	default:
		return writeFakeBin(t, "tilth",
			"#!/bin/sh\necho 'grok error: not found: x'\nexit 0\n")
	}
}

// fakeAstGrepMatch installs a fake ast-grep that emits one --json=stream match
// for file, so the structural type-decl fallback resolves.
func fakeAstGrepMatch(t *testing.T, file string) {
	t.Helper()
	line := `{"file":"` + file + `","text":"type Router struct {","range":{"start":{"line":4},"end":{"line":7}}}`
	fakeAstGrep(t, "#!/bin/sh\ncat <<'EOF'\n"+line+"\nEOF\n")
}

// fakeAstGrepEmpty installs a fake ast-grep that emits nothing and exits 1 — the
// clean no-match RunCLIAllowExit tolerates — so the fallback finds nothing.
func fakeAstGrepEmpty(t *testing.T) {
	t.Helper()
	fakeAstGrep(t, "#!/bin/sh\nexit 1\n")
}

func fakeAstGrep(t *testing.T, script string) {
	t.Helper()
	dir := filepath.Dir(writeFakeBin(t, "ast-grep", script))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeFakeBin(t *testing.T, name, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is POSIX-only")
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // fake binary must be owner-executable
		t.Fatalf("write fake %q: %v", name, err)
	}
	return path
}
