package vcstest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yasyf/cc-context/internal/execstub"
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

// hostEnv is the PATH as it stood before vcstest first replaced it in the
// running test, and every tool resolved against it. A second fixture in the
// same test would otherwise resolve through the shim directory the first one
// installed, where a tool it did not request is unreachable — reported as
// "not installed" for a tool that is — and one it did request is the first
// fixture's shim, which the second would then wrap.
type hostEnv struct {
	path  string
	tools []resolvedTool
}

var (
	hostMu sync.Mutex
	hosts  = map[string]*hostEnv{}
)

// hostEnvFor captures the PATH the first time the running test resolves a
// tool, before any vcstest call has replaced it, and drops it when that test
// ends — t.Setenv restores PATH at the same point, so the next test captures
// its own. It is keyed on the top-level test so a subtest shares its parent's,
// and held under a lock because parallel tests reach it at once: a single
// package-level capture let one test's cleanup drop the entry a concurrent
// test was still resolving through, and the shim it then wrote held only the
// tools that survived the drop.
func hostEnvFor(t *testing.T) *hostEnv {
	t.Helper()
	key, _, _ := strings.Cut(t.Name(), "/")
	hostMu.Lock()
	defer hostMu.Unlock()
	if h, ok := hosts[key]; ok {
		return h
	}
	h := &hostEnv{path: os.Getenv("PATH")}
	hosts[key] = h
	t.Cleanup(func() {
		hostMu.Lock()
		defer hostMu.Unlock()
		delete(hosts, key)
	})
	return h
}

func (h *hostEnv) resolve(t *testing.T, name string) resolvedTool {
	t.Helper()
	hostMu.Lock()
	for _, tool := range h.tools {
		if tool.name == name {
			hostMu.Unlock()
			return tool
		}
	}
	path := h.path
	hostMu.Unlock()

	resolved, err := lookPath(path, name)
	if err != nil {
		t.Skipf("%s not installed: %v", name, err)
	}
	tool := resolvedTool{name: name, path: resolved, interpreter: shebangInterpreter(t, path, resolved)}
	hostMu.Lock()
	defer hostMu.Unlock()
	for _, have := range h.tools {
		if have.name == name {
			return have
		}
	}
	h.tools = append(h.tools, tool)
	return tool
}

// snapshot returns what the running test has resolved so far, for a caller
// that walks the set while a parallel sibling may still be adding to it.
func (h *hostEnv) snapshot() []resolvedTool {
	hostMu.Lock()
	defer hostMu.Unlock()
	return slices.Clone(h.tools)
}

// Shim installs a recording passthrough for each tool and puts its bin
// directory at the head of a brew-free PATH. Each invocation appends one
// NUL-framed record to the returned log — depth, working directory, argc, then
// the argv — and execs the real binary, which was resolved against the host
// PATH; children the tool spawns re-enter the shim one depth deeper via
// CCX_SHIM_DEPTH.
func Shim(t *testing.T, tools ...string) (binDir, logPath string) {
	t.Helper()
	resolveTools(t, tools)
	_, binDir, logPath = installShim(t)
	t.Setenv("PATH", toolPATH(binDir))
	t.Setenv("CCX_ARGV_LOG", logPath)
	return binDir, logPath
}

// LinkPATH points PATH at a directory of symlinks to exactly the named tools
// and to the interpreters their shebangs name, ahead of the brew-free system
// directories, skipping the test when a tool is not installed. It records
// nothing — it is for tests that need the real tools reachable by name
// without their own directories, Homebrew's among them, rejoining PATH.
// Unlike the shim it links only what it was asked for, never the tools the
// test resolved earlier: narrowing PATH until a tool is missing, to prove ccx
// refuses without it, is what callers use this for.
func LinkPATH(t *testing.T, tools ...string) {
	t.Helper()
	linked := resolveTools(t, tools)
	dir := filepath.Join(realTempDir(t), "bin")
	mkdir(t, dir)
	for _, tool := range linked {
		symlink(t, tool.path, filepath.Join(dir, tool.name))
	}
	linkInterpreters(t, dir, linked)
	t.Setenv("PATH", toolPATH(dir))
}

// resolveTools resolves each tool and its script interpreter against the host
// PATH, skipping the test when one is not installed.
func resolveTools(t *testing.T, tools []string) []resolvedTool {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the recording shim is POSIX-only")
	}
	h := hostEnvFor(t)
	resolved := make([]resolvedTool, 0, len(tools))
	for _, tool := range tools {
		resolved = append(resolved, h.resolve(t, tool))
	}
	return resolved
}

// installShim writes the shim scripts for every tool the running test has
// resolved — not just the caller's — and replaces PATH with the shim
// directory ahead of the base system directories, so a second fixture's PATH
// still reaches the first fixture's tools. Each call mints its own directory
// and log: a fixture's log holds the invocations made while its shim led
// PATH, and the next call's takes over from there. The directory is removed
// best-effort rather than through t.TempDir, because gt leaves a refresher
// spawning git under the shim for seconds after it exits and RemoveAll
// reports that race as a cleanup failure.
func (f *Fixture) installShim(t *testing.T) {
	t.Helper()
	f.shimDir, f.ShimBin, f.ArgvLog = installShim(t)
	f.env = append(f.env, "PATH="+toolPATH(f.ShimBin), "CCX_ARGV_LOG="+f.ArgvLog)
}

func installShim(t *testing.T) (base, binDir, logPath string) {
	t.Helper()
	base = detachedDir(t, "ccx-shim")
	binDir = filepath.Join(base, "bin")
	mkdir(t, binDir)
	logPath = filepath.Join(base, argvLogName(0))
	tools := hostEnvFor(t).snapshot()
	for _, tool := range tools {
		script := "#!/bin/sh\n" + RecordArgv(tool.name) +
			"CCX_SHIM_DEPTH=$((d+1)) exec " + shellQuote(tool.path) + ` "$@"` + "\n"
		execstub.Write(t, filepath.Join(binDir, tool.name), script)
	}
	linkInterpreters(t, binDir, tools)
	return base, binDir, logPath
}

func argvLogName(generation int) string {
	return fmt.Sprintf("argv.%d.log", generation)
}

// RecordArgv is the shim's own framing — depth, working directory, argc, then
// the argv — as shell a faked process prepends to its script, so its calls
// land beside the real tools'. The destination comes from the environment
// rather than being baked in, which is what makes [Fixture.RotateLog] a
// boundary a running process cannot cross. It leaves d set for the caller's
// own exec line.
func RecordArgv(name string) string {
	return `d="${CCX_SHIM_DEPTH:-0}"` + "\n" +
		`printf '%s\0' "$d" "$PWD" "$(($#+1))" ` + shellQuote(name) + ` "$@" >> "$CCX_ARGV_LOG"` + "\n"
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
// form resolves prog by search, so it resolves against searchPATH too.
func shebangInterpreter(t *testing.T, searchPATH, path string) string {
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
	interpreter, err := lookPath(searchPATH, prog)
	if err != nil {
		t.Fatalf("resolve %s interpreter %q: %v", path, prog, err)
	}
	return interpreter
}

// lookPath resolves name against searchPATH, following exec.LookPath's rules:
// a name holding a separator resolves to itself, a bare name is searched
// directory by directory. exec.LookPath itself reads the live PATH, which a
// fixture installed earlier in the same test has already replaced with its
// shim directory.
func lookPath(searchPATH, name string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		if executable(name) {
			return name, nil
		}
		return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
	}
	for _, dir := range filepath.SplitList(searchPATH) {
		if dir == "" {
			continue
		}
		if candidate := filepath.Join(dir, name); executable(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
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
