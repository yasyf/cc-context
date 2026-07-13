package grep

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/render"
)

// tilthZeroScript is a fake tilth that prints one of tilth's clean 3-line zero
// results (header ending "— 0 matches", blank, token footer) — the shape the live
// recheck must re-verify against the working tree.
const tilthZeroScript = "#!/bin/sh\nprintf '# Search: \"needle\" in /x — 0 matches\\n\\n(~5 tokens)\\n'\n"

// tilthZeroOut is exactly what tilthZeroScript prints, so a genuine-zero test can
// assert byte-identity against today's finalized tilth output.
const tilthZeroOut = "# Search: \"needle\" in /x — 0 matches\n\n(~5 tokens)\n"

// requireEngine skips when neither rg nor grep is on PATH; the recheck needs a
// live engine to re-verify a tilth zero.
func requireEngine(t *testing.T) {
	t.Helper()
	_, rgErr := exec.LookPath("rg")
	_, grepErr := exec.LookPath("grep")
	if rgErr != nil && grepErr != nil {
		t.Skip("neither rg nor grep on PATH")
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		script     string
		query      string
		wantErr    bool
		wantErrHas string
		wantOutHas string
	}{
		{
			name:       "match passthrough",
			script:     "#!/bin/sh\nprintf '### x.go:1\\n→ [1] foo here\\n'\n",
			query:      "foo",
			wantOutHas: "foo here",
		},
		{
			name:       "no-match path fallback normalized",
			script:     "#!/bin/sh\necho 'not found: /cwd/class ToolUse' 1>&2\nexit 2\n",
			query:      "class ToolUse",
			wantOutHas: `# grep: "class ToolUse" — no matches`,
		},
		{
			name:       "other error propagates",
			script:     "#!/bin/sh\necho 'tilth: boom' 1>&2\nexit 2\n",
			query:      "foo",
			wantErr:    true,
			wantErrHas: "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := writeFakeBin(t, tt.script)
			a := backend.Args{Query: tt.query}
			got, err := Run(context.Background(), bin, []string{tt.query}, a)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Run() err = nil, want error containing %q", tt.wantErrHas)
				}
				if !strings.Contains(err.Error(), tt.wantErrHas) {
					t.Errorf("Run() err = %v, want it to contain %q", err, tt.wantErrHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run() err: %v", err)
			}
			if !strings.Contains(got, tt.wantOutHas) {
				t.Errorf("Run() out = %q, want it to contain %q", got, tt.wantOutHas)
			}
		})
	}
}

// TestRunPreflightSkipsRunner proves a nonexistent scope fails fast before the
// engine is invoked: the fake bin touches a sentinel that must stay absent.
func TestRunPreflightSkipsRunner(t *testing.T) {
	tmp := t.TempDir()
	sentinel := filepath.Join(tmp, "ran")
	bin := writeFakeBin(t, "#!/bin/sh\ntouch '"+sentinel+"'\n")
	a := backend.Args{Query: "foo", Scope: filepath.Join(tmp, "no", "such", "dir")}

	_, err := Run(context.Background(), bin, []string{a.Query, "--scope", a.Scope}, a)
	if err == nil {
		t.Fatal("Run() err = nil, want a scope pre-flight error")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Run() err = %v, want it to mention the missing scope", err)
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Error("pre-flight failed to short-circuit: the engine was invoked")
	}
}

// TestRunFileScopePassthrough proves a --scope naming a regular file passes the
// existence-only preflight and reaches the engine. tilth searches a file scope
// cleanly (verified against the pinned binary: `tilth <query> --scope <file>`
// returns that file's matches), so the preflight must not reject a file scope as
// "not a directory".
func TestRunFileScopePassthrough(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "activity.py")
	if err := os.WriteFile(file, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	bin := writeFakeBin(t, "#!/bin/sh\nprintf '### x.go:1\\n→ [1] foo here\\n'\n")
	a := backend.Args{Query: "foo", Scope: file}

	got, err := Run(context.Background(), bin, []string{a.Query, "--scope", a.Scope}, a)
	if err != nil {
		t.Fatalf("Run() err = %v, want a file scope to pass the preflight", err)
	}
	if !strings.Contains(got, "foo here") {
		t.Errorf("Run() out = %q, want the engine output (preflight let the file scope through)", got)
	}
}

// TestRunRechecksStaleCleanZero proves a stale tilth clean zero is overridden by
// the live recheck: the needle planted in the cwd surfaces despite tilth's
// "0 matches".
func TestRunRechecksStaleCleanZero(t *testing.T) {
	requireEngine(t)
	bin := writeFakeBin(t, tilthZeroScript)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("var needle = 1\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	a := backend.Args{Query: "needle"}
	got, err := Run(context.Background(), bin, []string{"needle"}, a)
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if !strings.Contains(got, "### sample.go:") {
		t.Errorf("recheck should surface the live match, got:\n%s", got)
	}
	if strings.Contains(got, "0 matches") {
		t.Errorf("stale zero leaked through the recheck:\n%s", got)
	}
}

// TestRunRechecksStaleNotFoundZero proves the recheck also fires for tilth's
// no-match path-fallback shape (a "not found:" stderr with a non-zero exit),
// overriding it when the needle exists on disk.
func TestRunRechecksStaleNotFoundZero(t *testing.T) {
	requireEngine(t)
	bin := writeFakeBin(t, "#!/bin/sh\necho 'not found: /x/needle' 1>&2\nexit 2\n")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("var needle = 1\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	a := backend.Args{Query: "needle"}
	got, err := Run(context.Background(), bin, []string{"needle"}, a)
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if !strings.Contains(got, "### sample.go:") {
		t.Errorf("recheck should surface the live match for the not-found shape, got:\n%s", got)
	}
}

// TestRunGenuineZeroByteIdentical proves a genuine zero (needle absent from the
// cwd) stays byte-identical to today's finalized tilth output — the recheck finds
// nothing and its own no-match output is discarded.
func TestRunGenuineZeroByteIdentical(t *testing.T) {
	requireEngine(t)
	bin := writeFakeBin(t, tilthZeroScript)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("var absent = 1\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	a := backend.Args{Query: "needle"}
	got, err := Run(context.Background(), bin, []string{"needle"}, a)
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	want, err := render.Finalize(backend.OpGrep, tilthZeroOut, a)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got != want {
		t.Errorf("genuine zero not byte-identical:\n got = %q\nwant = %q", got, want)
	}
}

// TestRunGenuineZeroTinyBudgetByteIdentical proves a genuine zero survives an
// adversarially small budget: with Budget 1 the recheck's own capped no-match
// output is byte-cut garbage, and only a structural found verdict — never a
// string sniff of that garbage — keeps the tilth zero byte-identical to today.
func TestRunGenuineZeroTinyBudgetByteIdentical(t *testing.T) {
	requireEngine(t)
	bin := writeFakeBin(t, tilthZeroScript)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("var absent = 1\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	a := backend.Args{Query: "needle", Budget: 1}
	got, err := Run(context.Background(), bin, []string{"needle"}, a)
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	want, err := render.Finalize(backend.OpGrep, tilthZeroOut, a)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got != want {
		t.Errorf("genuine zero not byte-identical under Budget 1:\n got = %q\nwant = %q", got, want)
	}
}

// TestRunRecheckOverrideQueryEmbedsNoMatches proves a stale-zero override
// survives a query that embeds the literal "— no matches": Budget 7 cuts the
// recheck's matched header to exactly `# grep: "abcd — no matches`, the shape a
// string predicate would misread as a zero and discard; the structural found
// verdict returns the real (capped) matches instead of the stale tilth zero.
func TestRunRecheckOverrideQueryEmbedsNoMatches(t *testing.T) {
	requireEngine(t)
	bin := writeFakeBin(t, tilthZeroScript)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("var s = \"abcd — no matches\"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	a := backend.Args{Query: "abcd — no matches", Budget: 7}
	got, err := Run(context.Background(), bin, []string{a.Query}, a)
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if strings.Contains(got, "# Search:") {
		t.Errorf("stale tilth zero returned instead of the recheck's matches:\n%s", got)
	}
	if !strings.HasPrefix(got, "# grep: ") {
		t.Errorf("recheck override missing the engine match output:\n%s", got)
	}
}

// TestRunCanceledCtxPropagates proves a grep under a canceled context returns a
// cancellation error — never the possibly-stale zero as success — and that the
// error satisfies errors.Is(context.Canceled) for callers branching on it.
func TestRunCanceledCtxPropagates(t *testing.T) {
	bin := writeFakeBin(t, tilthZeroScript)
	t.Chdir(t.TempDir())
	t.Setenv("PATH", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Run(ctx, bin, []string{"needle"}, backend.Args{Query: "needle"})
	if err == nil {
		t.Fatal("Run() err = nil, want a cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run() err = %v, want errors.Is(context.Canceled)", err)
	}
}

// TestRecheckOverrideCanceledCtxAttribution proves the recheck-failure error
// under a dead context carries the cancellation itself, not the incidental
// engine error (an install-ripgrep hint would hide the cancellation from
// errors.Is). PATH is emptied so the recheck fails fast on engine resolution.
func TestRecheckOverrideCanceledCtxAttribution(t *testing.T) {
	t.Setenv("PATH", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, ok, err := recheckOverride(ctx, backend.Args{Query: "needle"})
	if ok {
		t.Fatal("recheckOverride() ok = true under a canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("recheckOverride() err = %v, want errors.Is(context.Canceled)", err)
	}
}

// TestRunDegradesWithoutEngine proves that with an empty PATH the recheck cannot
// resolve an engine, so the tilth zero passes through unchanged rather than
// erroring. The fake bin runs by absolute path via its /bin/sh shebang and uses a
// shell builtin only, so it still executes with PATH="".
func TestRunDegradesWithoutEngine(t *testing.T) {
	bin := writeFakeBin(t, tilthZeroScript)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("var needle = 1\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)
	t.Setenv("PATH", "")

	a := backend.Args{Query: "needle"}
	got, err := Run(context.Background(), bin, []string{"needle"}, a)
	if err != nil {
		t.Fatalf("Run() err = %v, want graceful degradation to the tilth zero", err)
	}
	want, err := render.Finalize(backend.OpGrep, tilthZeroOut, a)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got != want {
		t.Errorf("degraded output not the original tilth zero:\n got = %q\nwant = %q", got, want)
	}
}

func writeFakeBin(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is POSIX-only")
	}
	path := filepath.Join(t.TempDir(), "tilth")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // fake binary must be owner-executable
		t.Fatalf("write fake bin: %v", err)
	}
	return path
}
