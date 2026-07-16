package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/cli"
	"github.com/yasyf/cc-context/internal/lookpath"
)

// hashClass is the character class of a 4-char content anchor: a leading letter
// (Crockford base32 minus i/l/o/u) then three base32 chars, matching anchor.Of.
const hashClass = `[a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{3}`

const greeterV1 = `package fix

import "strconv"

// Greeter builds greetings.
type Greeter struct {
	Prefix string
}

// Greet returns a greeting for name.
func (g Greeter) Greet(name string) string {
	return g.Prefix + name
}

func helper(x int) string {
	return strconv.Itoa(x * 2)
}
`

const greeterV2 = `package fix

import "strconv"

// Greeter builds greetings.
type Greeter struct {
	Prefix string
}

// Greet returns a greeting for name.
func (g Greeter) Greet(name string) string {
	return g.Prefix + name + "!"
}

func helper(x int) string {
	return strconv.Itoa(x * 2)
}
`

const utilGo = `package fix

func UseGreeter() string {
	g := Greeter{Prefix: "hi "}
	return g.Greet("world")
}
`

// subUseGo lives in a sub-package importing the fixture's root package, so it is a
// dependent of greeter.go for the native deps used-by leg.
const subUseGo = `package sub

import "example.com/fix"

func Run() string {
	return fix.UseGreeter()
}
`

// TestAnchorsEmittedAcrossOps is the permanent integration test that every native
// op emits content anchors at generation time: it drives the real ripgrep and
// ast-grep engines plus the native symbol, deps, diff, and outline-fallback
// renderers through the full CLI dispatch over a throwaway fixture repo and asserts
// every anchor still fired at least once. The grammar-keyed rewrite in
// internal/ripgrep/ripgrep.go and the ast-grep outline parse in
// internal/astgrep/outline.go degrade silently to "no anchors" on an engine version
// bump; the self-generated anchors in internal/symbol, internal/deps,
// internal/diff/render.go, and internal/outline/fallback.go are canaried here too.
// The assertions check that an anchor shape appeared, never a specific hash value.
func TestAnchorsEmittedAcrossOps(t *testing.T) {
	if testing.Short() {
		t.Skip("provisions and runs the real ast-grep engine")
	}
	if lookpath.Find("ast-grep") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("ast-grep not on PATH in CI: the test job must install it (uv tool install ast-grep-cli)")
		}
		t.Skip("ast-grep not on PATH; install it to run the anchor canary (brew install ast-grep)")
	}

	repo, sha1, sha2 := writeCanaryRepo(t)
	t.Chdir(repo)

	grep := runCCX(t, "code", "grep", "func")
	// Both the bare and -i greps run the native ripgrep engine (ripgrep.Run →
	// reshape, which stamps anchors at generation time); the flag only toggles case.
	grepRG := runCCX(t, "code", "grep", "func", "-i")
	// --full so the caller/sibling frame anchors this canary protects are present;
	// the terse default drops those sections (covered by TestTerseSymbol).
	symbol := runCCX(t, "code", "symbol", "Greet", "--full")
	outline := runCCX(t, "code", "outline", "greeter.go")
	outlineMD := runCCX(t, "code", "outline", "guide.md")
	deps := runCCX(t, "code", "deps", "greeter.go")
	diff := runCCX(t, "vcs", "diff", sha1+".."+sha2)

	// Each pattern proves one rewrite fired. A locator carries "path:line" before
	// the "#"; a frame carries a bare line or range, so the two never overlap.
	patterns := []struct {
		name string
		out  string
		re   *regexp.Regexp
	}{
		{"grep frame anchor (ripgrep)", grep, regexp.MustCompile(`\[\d+(?:-\d+)?#` + hashClass + `\]`)},
		{"grep frame anchor via -i (ripgrep)", grepRG, regexp.MustCompile(`\[\d+(?:-\d+)?#` + hashClass + `\]`)},
		{"symbol locator (symbol pkg)", symbol, regexp.MustCompile(`(?m)^# symbol \S.*:\d+(?:-\d+)?#` + hashClass)},
		{"symbol frame anchor (symbol pkg)", symbol, regexp.MustCompile(`\[\d+(?:-\d+)?#` + hashClass + `\]`)},
		{"outline item anchor (astgrep outline.go)", outline, regexp.MustCompile(`(?m)^L\d+#` + hashClass + `\b`)},
		{"fallback heading anchor (outline fallback.go)", outlineMD, regexp.MustCompile(`(?m)^L\d+#` + hashClass + `\b`)},
		{"deps used-by anchor (deps pkg)", deps, regexp.MustCompile(`(?m)^[\w./-]+:\d+(?:-\d+)?#` + hashClass)},
		{"deps uses-row anchor (deps pkg)", deps, regexp.MustCompile(`(?m)^L\d+#` + hashClass + `\b`)},
		{"diff symbol-row anchor (native diff render.go)", diff, regexp.MustCompile(`\[~\][^\n]*L\d+(?:-\d+)?#` + hashClass + `\b`)},
	}
	for _, p := range patterns {
		t.Run(p.name, func(t *testing.T) {
			if !p.re.MatchString(p.out) {
				t.Errorf("anchor rewrite did not fire — engine grammar drift?\nwant match: %s\n--- output ---\n%s", p.re, p.out)
			}
		})
	}

	t.Run("native diff classifies the covered file (render.go)", func(t *testing.T) {
		// greeter.go's body-only edit classifies Greet as a changed symbol.
		if !strings.Contains(diff, "## greeter.go") || !strings.Contains(diff, "[~] Greet") {
			t.Errorf("native diff did not classify greeter.go's changed symbol\n--- output ---\n%s", diff)
		}
	})

	t.Run("native diff renders raw hunks for an uncovered file (render.go)", func(t *testing.T) {
		// notes.txt has no ast-grep rules, so the native diff builds its own hunk —
		// never a git "diff --git" preamble.
		if !strings.Contains(diff, "@@") || !strings.Contains(diff, "+second note line") || !strings.Contains(diff, "-first note line") {
			t.Errorf("raw hunk missing from the uncovered-file section\n--- output ---\n%s", diff)
		}
		if strings.Contains(diff, "diff --git") {
			t.Errorf("native diff must not emit a git preamble\n--- output ---\n%s", diff)
		}
	})
}

// writeCanaryRepo builds a two-commit git repo and returns its path and the two
// commit SHAs. go.mod, greeter.go, util.go, and sub/use.go feed the grep, symbol,
// deps, and outline legs (greeter.go imports strconv for a deps use row, sub/use.go
// imports the root package as its deps dependent); the second commit's body-only
// edit and notes.txt change drive the diff leg's symbol row, SHA shortening, and
// preamble collapse across the sha1..sha2 range.
func writeCanaryRepo(t *testing.T) (repo, sha1, sha2 string) {
	t.Helper()
	repo = t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.email", "t@t.t")
	runGit(t, repo, "config", "user.name", "t")

	writeCanaryFile(t, repo, "go.mod", "module example.com/fix\n\ngo 1.23\n")
	writeCanaryFile(t, repo, "greeter.go", greeterV1)
	writeCanaryFile(t, repo, "util.go", utilGo)
	writeCanaryFile(t, repo, "sub/use.go", subUseGo)
	writeCanaryFile(t, repo, "notes.txt", "first note line\n")
	writeCanaryFile(t, repo, "guide.md", "# Title\n\nintro\n\n## Section One\n\nbody\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")
	sha1 = revParse(t, repo, "HEAD")

	writeCanaryFile(t, repo, "greeter.go", greeterV2)
	writeCanaryFile(t, repo, "notes.txt", "second note line\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "tweak body and note")
	sha2 = revParse(t, repo, "HEAD")
	return repo, sha1, sha2
}

// runCCX drives one ccx subcommand through the real CLI dispatch and returns its
// captured output, failing the test on any execution error.
func runCCX(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("ccx %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// runGit runs a git command in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git argv; dir is a TempDir, args are literals
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// revParse resolves a git revision in dir to its full object id.
func revParse(t *testing.T, dir, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", rev).Output() //nolint:gosec // fixed git argv; dir is a TempDir
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(string(out))
}

// writeCanaryFile writes content to dir/name, creating parent directories, failing
// the test on error.
func writeCanaryFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
