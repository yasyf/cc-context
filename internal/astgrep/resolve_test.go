package astgrep

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

// writeVersionFake writes a POSIX ast-grep stub in a fresh temp dir that answers
// `--version` with versionOut and exits 0 for any other argv, returning its path.
func writeVersionFake(t *testing.T, versionOut string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ast-grep")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '" + versionOut + "'; exit 0; fi\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // fake engine must be owner-executable
		t.Fatalf("write fake ast-grep: %v", err)
	}
	return path
}

func TestResolveBin(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("fake ast-grep scripts are POSIX-only")
	}
	tests := []struct {
		name            string
		versionOut      string // --version output; "" means not on PATH (no fake written)
		configured      bool   // pass the fake as the configured bin, not via PATH
		wantErrSubstr   string // non-empty asserts an error carrying this substring
		wantErrNamesBin bool   // the error must also name the binary path
	}{
		{name: "configured passthrough", versionOut: "ast-grep 0.44.0", configured: true},
		{name: "missing from PATH", wantErrSubstr: "need ast-grep on PATH"},
		{name: "version below floor", versionOut: "ast-grep 0.43.9", wantErrSubstr: "ccx needs ast-grep >= 0.44.0"},
		{name: "version at floor", versionOut: "ast-grep 0.44.0"},
		{name: "version above floor", versionOut: "ast-grep 0.45.2"},
		{name: "prerelease suffix parses to X.Y.Z", versionOut: "ast-grep 0.44.0-beta.1"},
		{name: "unparseable output", versionOut: "not a version line", wantErrSubstr: "unparseable", wantErrNamesBin: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var configured, made string
			pathDir := t.TempDir()
			switch {
			case tt.versionOut == "":
			case tt.configured:
				made = writeVersionFake(t, tt.versionOut)
				configured = made
			default:
				made = writeVersionFake(t, tt.versionOut)
				pathDir = filepath.Dir(made)
			}

			got, err := resolveBin(render.WithEnv(t.Context(), "PATH="+pathDir), configured)
			if tt.wantErrSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Fatalf("resolveBin err = %v, want substring %q", err, tt.wantErrSubstr)
				}
				if tt.wantErrNamesBin && !strings.Contains(err.Error(), made) {
					t.Errorf("error must name the binary path %q: %v", made, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBin: %v", err)
			}
			if got != made {
				t.Errorf("resolveBin = %q, want %q", got, made)
			}
		})
	}
}

func TestResolveBinReprobesAfterFailure(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("fake ast-grep scripts are POSIX-only")
	}
	path := writeVersionFake(t, "ast-grep 0.43.0")
	ctx := render.WithEnv(t.Context(), "PATH="+filepath.Dir(path))

	if _, err := resolveBin(ctx, ""); err == nil || !strings.Contains(err.Error(), "ccx needs ast-grep >= 0.44.0") {
		t.Fatalf("first resolve err = %v, want below-floor error", err)
	}

	upgraded := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'ast-grep 0.44.1'; exit 0; fi\nexit 0\n"
	if err := os.WriteFile(path, []byte(upgraded), 0o700); err != nil { //nolint:gosec // fake engine must be owner-executable
		t.Fatalf("upgrade fake ast-grep in place: %v", err)
	}
	got, err := resolveBin(ctx, "")
	if err != nil {
		t.Fatalf("resolve after in-place upgrade: %v", err)
	}
	if got != path {
		t.Errorf("resolveBin = %q, want %q", got, path)
	}
}
