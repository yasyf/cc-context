package cli_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/cli"
)

func TestRootHelpListsAllOps(t *testing.T) {
	tests := []struct {
		name string
		args []string
		ops  []string
	}{
		{"root", []string{"--help"}, []string{"vcs", "code", "repo", "hello", "toon"}},
		{"vcs", []string{"vcs", "--help"}, []string{"diff"}},
		{"code", []string{"code", "--help"}, []string{
			"read", "outline", "search", "grep", "symbol", "deps", "related", "replace",
		}},
		{"repo", []string{"repo", "--help"}, []string{"overview", "find"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			root := cli.NewRootCmd()
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tt.args)
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute(%v) error = %v", tt.args, err)
			}
			help := out.String()
			for _, op := range tt.ops {
				if !strings.Contains(help, op) {
					t.Errorf("%v output missing subcommand %q\n%s", tt.args, op, help)
				}
			}
		})
	}
}

func TestSymbolAliasGrokRegistered(t *testing.T) {
	for _, group := range cli.NewRootCmd().Commands() {
		if group.Name() != "code" {
			continue
		}
		for _, c := range group.Commands() {
			if c.Name() == "symbol" {
				for _, a := range c.Aliases {
					if a == "grok" {
						return
					}
				}
				t.Fatalf("symbol command missing grok alias, aliases = %v", c.Aliases)
			}
		}
		t.Fatal("symbol command not registered under code group")
	}
	t.Fatal("code group not registered")
}

// TestSearchCommandInvokesBackend drives the full CLI->router->backend->render
// path. The semble engine is mocked with a fake script that echoes its argv, so
// no real engine or network is touched; the assertion proves the search command
// builds the expected argv and that --budget capping is applied to the output.
func TestSearchCommandInvokesBackend(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is POSIX-only")
	}
	writeFakeEngine(t, "semble")

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"code", "search", "auth flow", "src", "-k", "3"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(search) error = %v", err)
	}

	got := strings.TrimSpace(out.String())
	// A natural-language query routes to semble (semantic); the routing header is
	// prepended to the backend's output on stdout.
	want := "# semantic (semble)\nsearch auth flow src -k 3 --max-snippet-lines 10"
	if got != want {
		t.Errorf("backend argv = %q, want %q", got, want)
	}
}

// TestReadCommandResolvesAnchor drives an anchored --section through the full
// CLI seam: the fake tilth engine echoes its argv, proving the section reaches
// the backend already numeric, with the move note prepended to the output.
func TestReadCommandResolvesAnchor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is POSIX-only")
	}
	writeFakeEngine(t, "tilth")
	file := writeAnchorFixture(t)
	gamma := anchor.Of("gamma")

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"code", "read", file, "--section", anchor.Format(2, gamma)})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(read) error = %v", err)
	}

	want := fmt.Sprintf("# anchor %s: line 2 → 3\n%s --section 3-3", gamma, file)
	if got := strings.TrimSpace(out.String()); got != want {
		t.Errorf("read output = %q, want %q", got, want)
	}
}

// TestRelatedCommandResolvesAnchor drives an anchored file:line#hash location
// through the CLI seam: the fake semble engine echoes its argv, proving the
// location reaches the backend as plain file and line positionals.
func TestRelatedCommandResolvesAnchor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is POSIX-only")
	}
	writeFakeEngine(t, "semble")
	file := writeAnchorFixture(t)
	beta := anchor.Of("beta")

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"code", "related", file + ":" + anchor.Format(2, beta)})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(related) error = %v", err)
	}

	want := fmt.Sprintf("find-related %s 2", file)
	if got := strings.TrimSpace(out.String()); got != want {
		t.Errorf("related output = %q, want %q", got, want)
	}
}

// writeAnchorFixture writes a three-line file to anchor against.
func writeAnchorFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// writeFakeEngine puts an executable with the given engine name on PATH that
// echoes its arguments, so backend resolution picks it over a real binary.
func writeFakeEngine(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho \"$@\"\n"), 0o700); err != nil { //nolint:gosec // fake engine script must be owner-executable
		t.Fatalf("write fake engine: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
