package vcstest

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// systemPATH is the brew-free base PATH every shimmed process resolves
// through: gt's first run under a fresh HOME shells out to brew when it can
// find one, racing its bootsnap cache write against test cleanup.
var systemPATH = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}

type resolvedTool struct {
	name        string
	path        string
	interpreter string
}

// Shim installs a recording passthrough for each tool and puts its bin
// directory at the head of a brew-free PATH. Each invocation appends one
// argc-prefixed NUL-framed record to the returned log — depth first, then
// argc, then the argv — and execs the real binary, which was resolved before
// PATH changed; children the tool spawns re-enter the shim one depth deeper
// via CCX_SHIM_DEPTH.
func Shim(t *testing.T, tools ...string) (binDir, logPath string) {
	t.Helper()
	return installShim(t, resolveTools(t, tools))
}

// LinkPATH points PATH at a directory of symlinks to each named tool and to
// the interpreters their shebangs name, ahead of the brew-free system
// directories, skipping the test when a tool is not installed. It records
// nothing — it is for tests that need the real tools reachable by name
// without their own directories, Homebrew's among them, rejoining PATH.
func LinkPATH(t *testing.T, tools ...string) {
	t.Helper()
	resolved := resolveTools(t, tools)
	dir := filepath.Join(realTempDir(t), "bin")
	mkdir(t, dir)
	for _, tool := range resolved {
		symlink(t, tool.path, filepath.Join(dir, tool.name))
	}
	linkInterpreters(t, dir, resolved)
	t.Setenv("PATH", toolPATH(dir))
}

// resolveTools resolves each tool and its script interpreter against the
// current PATH, skipping the test when one is not installed.
func resolveTools(t *testing.T, tools []string) []resolvedTool {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the recording shim is POSIX-only")
	}
	resolved := make([]resolvedTool, 0, len(tools))
	for _, tool := range tools {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not installed", tool)
		}
		resolved = append(resolved, resolvedTool{name: tool, path: path, interpreter: shebangInterpreter(t, path)})
	}
	return resolved
}

// installShim writes the shim scripts for already-resolved tools and replaces
// PATH with the shim directory ahead of the base system directories.
func installShim(t *testing.T, tools []resolvedTool) (binDir, logPath string) {
	t.Helper()
	base := realTempDir(t)
	binDir = filepath.Join(base, "bin")
	mkdir(t, binDir)
	logPath = filepath.Join(base, "argv.log")
	for _, tool := range tools {
		if tool.name == "gt" {
			// gt's detached cache refresher outlives gt itself; let it
			// drain before TempDir removal races its writes.
			t.Cleanup(func() { waitQuiet(logPath) })
		}
		script := "#!/bin/sh\n" +
			`d="${CCX_SHIM_DEPTH:-0}"` + "\n" +
			`printf '%s\0' "$d" "$(($#+1))" ` + shellQuote(tool.name) + ` "$@" >> ` + shellQuote(logPath) + "\n" +
			"CCX_SHIM_DEPTH=$((d+1)) exec " + shellQuote(tool.path) + ` "$@"` + "\n"
		if err := os.WriteFile(filepath.Join(binDir, tool.name), []byte(script), 0o700); err != nil { //nolint:gosec // the shim must be owner-executable to serve as a PATH entry
			t.Fatalf("write shim %s: %v", tool.name, err)
		}
	}
	linkInterpreters(t, binDir, tools)
	t.Setenv("PATH", toolPATH(binDir))
	return binDir, logPath
}

// linkInterpreters symlinks the interpreter each script tool's shebang names
// into dir. npm's gt is a `#!/usr/bin/env node` script, so node has to be
// reachable on the replaced PATH or the exec fails with 127 — and node's own
// directory cannot simply join PATH, since on a dev machine it is often
// Homebrew's, which is what systemPATH exists to keep out.
func linkInterpreters(t *testing.T, dir string, tools []resolvedTool) {
	t.Helper()
	mkdir(t, dir)
	linked := map[string]bool{}
	for _, tool := range tools {
		if tool.interpreter == "" || linked[tool.interpreter] {
			continue
		}
		linked[tool.interpreter] = true
		symlink(t, tool.interpreter, filepath.Join(dir, filepath.Base(tool.interpreter)))
	}
}

// shebangInterpreter returns the resolved path of the program a script's
// shebang runs, or "" when path is not a script. The `#!/usr/bin/env prog`
// form resolves prog against the current PATH, so this runs before PATH is
// replaced.
func shebangInterpreter(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // path is a LookPath-resolved vcs binary, not untrusted input
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, 512)
	n, err := f.Read(head)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read %s: %v", path, err)
	}
	line, _, _ := strings.Cut(string(head[:n]), "\n")
	if !strings.HasPrefix(line, "#!") {
		return ""
	}
	fields := strings.Fields(line[2:])
	prog := fields[0]
	if filepath.Base(prog) == "env" {
		prog = fields[1]
	}
	interpreter, err := exec.LookPath(prog)
	if err != nil {
		t.Fatalf("resolve %s interpreter %q: %v", path, prog, err)
	}
	return interpreter
}

// toolPATH joins lead ahead of the brew-free system directories.
func toolPATH(lead ...string) string {
	return strings.Join(append(lead, systemPATH...), string(os.PathListSeparator))
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

// shellQuote single-quotes s for embedding in the shim script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
