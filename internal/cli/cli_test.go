package cli_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/cli"
	"github.com/yasyf/cc-context/internal/version"
)

// skipWithoutGrepEngine skips a test unless rg or system grep is on PATH, since
// the regex/multi-file/ignore-case grep routes run one of them.
func skipWithoutGrepEngine(t *testing.T) {
	t.Helper()
	_, rgErr := exec.LookPath("rg")
	_, grepErr := exec.LookPath("grep")
	if rgErr != nil && grepErr != nil {
		t.Skip("neither rg nor grep on PATH")
	}
}

// TestGrepCommandRegex drives the full CLI seam for an anchored regex over an
// explicit file operand: --regex routes to the rg/grep engine and "^func " hits
// the line starting with func, which a literal search could not.
func TestGrepCommandRegex(t *testing.T) {
	skipWithoutGrepEngine(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("// mentions func\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"code", "grep", "--regex", "^func ", "sample.go"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(grep --regex) error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "### sample.go:2") {
		t.Errorf("grep --regex output missing anchored func line:\n%s", got)
	}
}

// TestGrepCommandUnbudgetedCaps proves an unbudgeted engine grep (here -i) over a
// many-match fixture applies ripgrep.DefaultBudget and ends with the overflow
// footer instead of flooding the whole match set.
func TestGrepCommandUnbudgetedCaps(t *testing.T) {
	skipWithoutGrepEngine(t)
	dir := t.TempDir()
	var body strings.Builder
	for i := 0; i < 1000; i++ {
		body.WriteString("var needle = 1\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "many.go"), []byte(body.String()), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"code", "grep", "-i", "needle"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(grep -i) error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "tokens omitted — re-run with a larger --budget") {
		t.Errorf("unbudgeted engine grep missing overflow footer:\n%s", got[max(0, len(got)-400):])
	}
}

func TestRootHelpListsAllOps(t *testing.T) {
	tests := []struct {
		name string
		args []string
		ops  []string
	}{
		{"root", []string{"--help"}, []string{"vcs", "code", "repo", "web", "exec", "format"}},
		{"vcs", []string{"vcs", "--help"}, []string{"diff"}},
		{"code", []string{"code", "--help"}, []string{
			"read", "outline", "search", "grep", "symbol", "deps", "related", "replace", "edit",
		}},
		{"repo", []string{"repo", "--help"}, []string{"overview", "find"}},
		{"web", []string{"web", "--help"}, []string{"outline", "read", "search"}},
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

// TestReadCommandLinesAlias proves --lines is a hidden alias for --section: the
// fake tilth engine echoes its argv, so the range must reach the backend as
// --section.
func TestReadCommandLinesAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is POSIX-only")
	}
	writeFakeEngine(t, "tilth")
	file := writeAnchorFixture(t)

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"code", "read", file, "--lines", "1-2"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(read --lines) error = %v", err)
	}
	want := fmt.Sprintf("%s --section 1-2", file)
	if got := out.String(); !strings.Contains(got, want) {
		t.Errorf("read --lines argv %q not in output %q", want, got)
	}
}

// TestOutlineSectionRejectsTilthLane proves `outline --section` on a file that
// routes to tilth signature mode fails before dispatch with a precise error
// pointing the caller at ccx code read.
func TestOutlineSectionRejectsTilthLane(t *testing.T) {
	dir := t.TempDir()
	rb := filepath.Join(dir, "a.rb")
	if err := os.WriteFile(rb, []byte("x = 1\n"), 0o600); err != nil {
		t.Fatalf("write rb: %v", err)
	}

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"code", "outline", rb, "--section", "1-1"})
	err := root.Execute()
	if err == nil {
		t.Fatal("Execute(outline --section on tilth-routed file) err = nil, want fallback error")
	}
	if !strings.Contains(err.Error(), "ccx code read") {
		t.Errorf("error %q should point at ccx code read", err)
	}
}

// TestOutlineLinesAlias proves --lines is a hidden alias for --section on the
// outline command: it reaches a.Section, so a tilth-routed file hits the same
// read-fallback guard --section does.
func TestOutlineLinesAlias(t *testing.T) {
	dir := t.TempDir()
	rb := filepath.Join(dir, "a.rb")
	if err := os.WriteFile(rb, []byte("x = 1\n"), 0o600); err != nil {
		t.Fatalf("write rb: %v", err)
	}

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"code", "outline", rb, "--lines", "1-1"})
	err := root.Execute()
	if err == nil {
		t.Fatal("Execute(outline --lines on tilth-routed file) err = nil, want fallback error")
	}
	if !strings.Contains(err.Error(), "ccx code read") {
		t.Errorf("error %q should point at ccx code read", err)
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
