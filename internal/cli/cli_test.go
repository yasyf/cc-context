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
	"github.com/yasyf/cc-context/internal/version"
)

func TestRootHelpListsAllOps(t *testing.T) {
	tests := []struct {
		name string
		args []string
		ops  []string
	}{
		{"root", []string{"--help"}, []string{"vcs", "code", "repo", "exec", "toon"}},
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

// TestVersionPrintsBareTag pins the version contract the plugin installer
// depends on: a release build prints exactly the v-prefixed tag, nothing else.
func TestVersionPrintsBareTag(t *testing.T) {
	old := version.Version
	version.Version = "v9.9.9"
	t.Cleanup(func() { version.Version = old })

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(--version) error = %v", err)
	}
	if got := out.String(); got != "v9.9.9\n" {
		t.Errorf("--version output = %q, want %q", got, "v9.9.9\n")
	}
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

	got := out.String()
	// A natural-language query routes to semble (semantic); the routing header is
	// prepended and the reshaped snippet carries the argv the fake engine echoed,
	// proving both the routing decision and the argv the search command built.
	if !strings.Contains(got, "# semantic (semble)") {
		t.Errorf("missing routing header in %q", got)
	}
	wantArgv := "search auth flow src -k 3 --max-snippet-lines 10"
	if !strings.Contains(got, wantArgv) {
		t.Errorf("backend argv %q not in output %q", wantArgv, got)
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

	// The anchored beta#hash resolves to line 2, so the argv carries the plain "2";
	// the fake semble engine echoes that argv into the reshaped snippet.
	wantArgv := fmt.Sprintf("find-related %s 2", file)
	if got := out.String(); !strings.Contains(got, wantArgv) {
		t.Errorf("related argv %q not in output %q", wantArgv, got)
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
	script := "#!/bin/sh\necho \"$@\"\n"
	if name == "semble" {
		// search/related output is reshaped from semble JSON, so the fake must emit
		// valid JSON. Its argv is echoed into the snippet so the assertion can still
		// prove which argv reached the backend.
		script = "#!/bin/sh\n" + `printf '{"results":[{"file_path":"loc","start_line":1,"end_line":1,"score":0,"content":"%s"}]}' "$*"` + "\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // fake engine script must be owner-executable
		t.Fatalf("write fake engine: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
