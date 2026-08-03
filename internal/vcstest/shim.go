package vcstest

import (
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
	name string
	path string
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

// resolveTools resolves each tool against the current PATH, skipping the test
// when one is not installed.
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
		resolved = append(resolved, resolvedTool{name: tool, path: path})
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
	t.Setenv("PATH", strings.Join(append([]string{binDir}, systemPATH...), string(os.PathListSeparator)))
	return binDir, logPath
}

// shellQuote single-quotes s for embedding in the shim script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
