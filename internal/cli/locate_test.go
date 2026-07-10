package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeGoScript answers `go env GOMODCACHE` from $FAKE_GOMODCACHE and `go list`
// from $FAKE_GOLIST, so the locate command resolves a Go module without a real
// toolchain.
const fakeGoScript = `#!/bin/sh
case "$1" in
env) printf '%s\n' "$FAKE_GOMODCACHE" ;;
list) printf '%s\n' "$FAKE_GOLIST" ;;
*) exit 1 ;;
esac
`

func TestLocateCommandRepoHit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("PATH", t.TempDir()) // no go/python3 on PATH — repo resolver only

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "captain-hook"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	out := runLocate(t, "captain-hook", ws)
	want := "repo\t" + filepath.Join(ws, "captain-hook") + "\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestLocateCommandRepoAndPackageRows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	binDir := t.TempDir()
	writeFakeScript(t, binDir, "python3", "#!/bin/sh\nprintf '%s\\t%s\\n' /py/site-packages/foo 1.2.3\n")
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("PATH", binDir) // python3 resolves the package; no `go`, so no gomod row

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "foo"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	out := runLocate(t, "foo", ws)
	want := "repo\t" + filepath.Join(ws, "foo") + "\n" +
		"package\t/py/site-packages/foo\t1.2.3\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestLocateCommandGoModuleVersionColumn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	binDir := t.TempDir()
	writeFakeScript(t, binDir, "go", fakeGoScript)
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("PATH", binDir)
	t.Setenv("FAKE_GOMODCACHE", t.TempDir()) // empty cache — no glob hits
	t.Setenv("FAKE_GOLIST", "/fake/mod/dir@v1.2.3")

	out := runLocate(t, "example.com/mod", t.TempDir())
	want := "gomod\t/fake/mod/dir\tv1.2.3\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestLocateCommandNotFound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("PATH", t.TempDir()) // nothing resolves

	cmd := newLocateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"no-such-thing", "--workspace", t.TempDir()})

	err := cmd.Execute()
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Execute() error = %v, want ErrNotFound", err)
	}
	if got := ExitCode(err); got != 3 {
		t.Errorf("ExitCode = %d, want 3", got)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

func runLocate(t *testing.T, name, workspace string) string {
	t.Helper()
	cmd := newLocateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{name, "--workspace", workspace})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(%q) error = %v", name, err)
	}
	return out.String()
}

func writeFakeScript(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
		t.Fatalf("write fake %q: %v", name, err)
	}
}
