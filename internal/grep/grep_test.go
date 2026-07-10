package grep

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

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
