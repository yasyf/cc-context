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

	"github.com/yasyf/cc-context/anchor"
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

func TestGrepCommandAutoRegex(t *testing.T) {
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
	root.SetArgs([]string{"code", "grep", "^func ", "sample.go"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(grep auto-regex) error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `# grep: "^func " — 1 matches in 1 files (auto-regex)`) ||
		!strings.Contains(got, "### sample.go:2") {
		t.Errorf("grep auto-regex output missing annotated anchored match:\n%s", got)
	}
}

func TestSearchCommandLiteralAutoRegexHeader(t *testing.T) {
	skipWithoutGrepEngine(t)
	t.Chdir(t.TempDir())
	if err := os.WriteFile("sample.go", []byte("func Foo() {}\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"code", "search", "--literal", "^func "})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(search --literal) error = %v", err)
	}
	wantPrefix := "# literal (grep)\n# grep: \"^func \" — 1 matches in 1 files (auto-regex)\n"
	if got := out.String(); !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("search --literal output = %q, want prefix %q", got, wantPrefix)
	}
}

func TestGrepCommandResolvesExtensionSibling(t *testing.T) {
	skipWithoutGrepEngine(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "events.py"), []byte("def old():\n    pass\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"code", "grep", "def old", "events"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(grep sibling) error = %v", err)
	}
	got := out.String()
	wantPrefix := "# note: events → events.py\n# grep: \"def old\""
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("grep sibling output = %q, want prefix %q", got, wantPrefix)
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
		{"root", []string{"--help"}, []string{"vcs", "code", "anchor", "repo", "web", "exec", "format"}},
		{"vcs", []string{"vcs", "--help"}, []string{"diff"}},
		{"anchor", []string{"anchor", "--help"}, []string{"hash", "resolve"}},
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

// TestGroupCommandsRejectUnknownSubcommand proves each group command errors on an
// unknown positional token instead of swallowing it and printing help with exit 0,
// while a bare group invocation still prints help.
func TestGroupCommandsRejectUnknownSubcommand(t *testing.T) {
	for _, group := range []string{"code", "repo", "vcs", "web", "anchor"} {
		t.Run(group+" unknown subcommand errors", func(t *testing.T) {
			var out bytes.Buffer
			root := cli.NewRootCmd()
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{group, "definitely-not-a-command"})
			err := root.Execute()
			if err == nil {
				t.Fatalf("Execute(%s definitely-not-a-command) err = nil, want unknown-command error\n%s", group, out.String())
			}
			if !strings.Contains(err.Error(), "definitely-not-a-command") {
				t.Errorf("error does not name the bad token: %v", err)
			}
		})
		t.Run(group+" bare prints help", func(t *testing.T) {
			var out bytes.Buffer
			root := cli.NewRootCmd()
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{group})
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute(%s) err = %v, want help with no error", group, err)
			}
			if !strings.Contains(out.String(), "Usage:") {
				t.Errorf("bare %q did not print help:\n%s", group, out.String())
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

// TestReadCommandResolvesAnchor drives an anchored --section through the full CLI
// seam onto native read: the anchor re-resolves to its content's current line, the
// move note is prepended, and the anchored native header carries the served line.
func TestReadCommandResolvesAnchor(t *testing.T) {
	file := writeAnchorFixture(t)
	gamma := anchor.Of("gamma")

	got := runCCX(t, "code", "read", file, "--section", anchor.Format(2, gamma))
	want := fmt.Sprintf("# anchor %s: line 2 → 3\n# read %s:3#%s (1 of 3 lines)\ngamma\n", gamma, file, gamma)
	if got != want {
		t.Errorf("read output = %q, want %q", got, want)
	}
}

// TestReadCommandLinesAlias proves --lines is a hidden alias for --section: the
// range reaches native read, which serves lines 1-2 of the fixture.
func TestReadCommandLinesAlias(t *testing.T) {
	file := writeAnchorFixture(t)

	got := runCCX(t, "code", "read", file, "--lines", "1-2")
	first := anchor.Of("alpha")
	want := fmt.Sprintf("# read %s:1-2#%s (2 of 3 lines)\nalpha\nbeta\n", file, first)
	if got != want {
		t.Errorf("read --lines output = %q, want %q", got, want)
	}
}

// TestOutlineSectionRejectsFallbackLane proves `outline --section` on a file that
// routes to the native fallback lane fails before dispatch with a precise error
// pointing the caller at ccx code read.
func TestOutlineSectionRejectsFallbackLane(t *testing.T) {
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
		t.Fatal("Execute(outline --section on fallback-routed file) err = nil, want fallback error")
	}
	if !strings.Contains(err.Error(), "ccx code read") {
		t.Errorf("error %q should point at ccx code read", err)
	}
}

// TestOutlineLinesAlias proves --lines is a hidden alias for --section on the
// outline command: it reaches a.Section, so a fallback-routed file hits the same
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
		t.Fatal("Execute(outline --lines on fallback-routed file) err = nil, want fallback error")
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

// fakeAWSKey is a well-known example AWS access key id the aws-access-token
// masking rule fires on.
const fakeAWSKey = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec // AWS's documented example key id, not a credential

// writeSecretFixture writes a one-line file embedding a detectable AWS key, so the
// read tests prove masking against native read at the CLI seam.
func writeSecretFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "creds.txt")
	if err := os.WriteFile(path, []byte("KEY = \""+fakeAWSKey+"\"\n"), 0o600); err != nil {
		t.Fatalf("write secret fixture: %v", err)
	}
	return path
}

// TestReadCommandMasksSecretOutput proves a secret in the file comes out masked
// with the footer when --reveal-secrets is absent.
func TestReadCommandMasksSecretOutput(t *testing.T) {
	file := writeSecretFixture(t)
	h := anchor.Of("KEY = \"" + fakeAWSKey + "\"")

	got := runCCX(t, "code", "read", file, "--full")
	want := fmt.Sprintf("# read %s:1#%s (1 lines)\nKEY = \"AKIA…[masked:aws-access-token]\"\n# 1 secret(s) masked (aws-access-token) — --reveal-secrets prints raw\n", file, h)
	if got != want {
		t.Errorf("read output = %q, want %q", got, want)
	}
}

// TestReadCommandRevealSecrets proves --reveal-secrets passes the file's secret
// through raw, with no footer.
func TestReadCommandRevealSecrets(t *testing.T) {
	file := writeSecretFixture(t)
	h := anchor.Of("KEY = \"" + fakeAWSKey + "\"")

	got := runCCX(t, "code", "read", file, "--full", "--reveal-secrets")
	want := fmt.Sprintf("# read %s:1#%s (1 lines)\nKEY = \"%s\"\n", file, h, fakeAWSKey)
	if got != want {
		t.Errorf("read --reveal-secrets output = %q, want %q", got, want)
	}
}
