package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/cli"
)

// allOps are the logical-op subcommands the root must expose.
var allOps = []string{
	"search", "related", "outline", "read", "symbol",
	"deps", "grep", "find", "diff", "overview",
}

func TestRootHelpListsAllOps(t *testing.T) {
	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(--help) error = %v", err)
	}
	help := out.String()
	for _, op := range append([]string{"hello"}, allOps...) {
		if !strings.Contains(help, op) {
			t.Errorf("--help output missing subcommand %q\n%s", op, help)
		}
	}
}

func TestSymbolAliasGrokRegistered(t *testing.T) {
	for _, c := range cli.NewRootCmd().Commands() {
		if c.Name() == "symbol" {
			for _, a := range c.Aliases {
				if a == "grok" {
					return
				}
			}
			t.Fatalf("symbol command missing grok alias, aliases = %v", c.Aliases)
		}
	}
	t.Fatal("symbol command not registered")
}

// TestSearchCommandInvokesBackend drives the full CLI->router->backend->render
// path. The semble engine is mocked with a fake script that echoes its argv, so
// no real engine or network is touched; the assertion proves the search command
// builds the expected argv and that --budget capping is applied to the output.
func TestSearchCommandInvokesBackend(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is POSIX-only")
	}
	fake := writeFakeEngine(t)
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"search", "auth flow", "src", "-k", "3"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(search) error = %v", err)
	}

	got := strings.TrimSpace(out.String())
	want := "search auth flow src -k 3 --max-snippet-lines 10"
	if got != want {
		t.Errorf("backend argv = %q, want %q", got, want)
	}
}

// writeFakeEngine writes an executable named "semble" that echoes its arguments,
// returning its absolute path. Stubbing PATH to its directory makes the semble
// backend resolve to it instead of a real binary.
func writeFakeEngine(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "semble")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho \"$@\"\n"), 0o700); err != nil { //nolint:gosec // fake engine script must be owner-executable
		t.Fatalf("write fake engine: %v", err)
	}
	return path
}
