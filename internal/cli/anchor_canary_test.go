package cli_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/cli"
	"github.com/yasyf/cc-context/internal/lookpath"
	"github.com/yasyf/cc-context/internal/vendor"
)

// hashClass is the character class of a 4-char content anchor: a leading letter
// (Crockford base32 minus i/l/o/u) then three base32 chars, matching anchor.Of.
const hashClass = `[a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{3}`

const greeterV1 = `package fix

// Greeter builds greetings.
type Greeter struct {
	Prefix string
}

// Greet returns a greeting for name.
func (g Greeter) Greet(name string) string {
	return g.Prefix + name
}

func helper(x int) int {
	return x * 2
}
`

const greeterV2 = `package fix

// Greeter builds greetings.
type Greeter struct {
	Prefix string
}

// Greet returns a greeting for name.
func (g Greeter) Greet(name string) string {
	return g.Prefix + name + "!"
}

func helper(x int) int {
	return x * 2
}
`

const utilGo = `package fix

func UseGreeter() string {
	g := Greeter{Prefix: "hi "}
	return g.Greet("world")
}
`

// TestContentAnchorsSurviveEngineGrammar is the drift canary for the content-anchor
// rewrites: it drives the real ripgrep, tilth, and ast-grep engines through the full
// CLI dispatch over a throwaway fixture repo and asserts every anchor rewrite still
// fired at least once. The rewrites in internal/ripgrep/ripgrep.go, internal/render/
// finalize.go, internal/render/diff.go, and internal/astgrep/outline.go are keyed to
// the engines' output grammar, so an engine version bump that reshapes that grammar
// degrades silently to "no anchors". This test converts that silent regression into a
// loud failure. The assertions check that a rewrite shape appeared, never a specific
// hash value.
func TestContentAnchorsSurviveEngineGrammar(t *testing.T) {
	if testing.Short() {
		t.Skip("provisions and runs the real tilth + ast-grep engines")
	}
	ctx := context.Background()
	forcePinnedEngines(ctx, t)

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
		{"symbol grok locator (finalize.go)", symbol, regexp.MustCompile(`\[[^\]]+:\d+#` + hashClass + `\]`)},
		{"symbol range frame (finalize.go)", symbol, regexp.MustCompile(`\[\d+-\d+#` + hashClass + `\]`)},
		{"outline item anchor (outline.go)", outline, regexp.MustCompile(`(?m)^L\d+#` + hashClass + `\b`)},
		{"deps group heading (finalize.go)", deps, regexp.MustCompile(`(?m)^### \S`)},
		{"deps row anchor (finalize.go)", deps, regexp.MustCompile(`(?m)^L\d+#` + hashClass + `\b`)},
		{"diff symbol-row anchor (diff.go)", diff, regexp.MustCompile(`L\d+#` + hashClass + `\b`)},
	}
	for _, p := range patterns {
		t.Run(p.name, func(t *testing.T) {
			if !p.re.MatchString(p.out) {
				t.Errorf("anchor rewrite did not fire — engine grammar drift?\nwant match: %s\n--- output ---\n%s", p.re, p.out)
			}
		})
	}

	t.Run("diff header SHAs shortened (diff.go)", func(t *testing.T) {
		if !strings.Contains(diff, sha1[:10]+".."+sha2[:10]) {
			t.Errorf("# Diff: header SHAs not shortened to 10 chars\n--- output ---\n%s", diff)
		}
		if strings.Contains(diff, sha1) {
			t.Errorf("full 40-hex SHA survived the shortening\n--- output ---\n%s", diff)
		}
	})

	t.Run("diff preamble collapsed (diff.go)", func(t *testing.T) {
		// notes.txt is a 0-symbol section, so its whole body is the raw git hunk
		// spliced in — which always opens with a "diff --git" preamble. The hunk
		// content proves the splice ran; the absent preamble proves the collapse did.
		if !strings.Contains(diff, "@@") || !strings.Contains(diff, "+second note line") {
			t.Errorf("raw hunk was not spliced into the 0-symbol section\n--- output ---\n%s", diff)
		}
		if strings.Contains(diff, "diff --git") {
			t.Errorf("per-file diff preamble was not collapsed\n--- output ---\n%s", diff)
		}
	})
}

// forcePinnedEngines stages both engines under a temp dir prepended to PATH so the
// CLI legs resolve a known version whose grammar the anchor regexes target: tilth
// from its pinned download, ast-grep from PATH (de-vendored). It skips when tilth
// cannot be provisioned (e.g. offline); a missing PATH ast-grep skips locally but
// hard-fails in CI, where the test job installs it (uv tool install ast-grep-cli).
func forcePinnedEngines(ctx context.Context, t *testing.T) {
	t.Helper()
	binDir := t.TempDir()

	tilthBin, err := vendor.Ensure(ctx, vendor.Tilth)
	if err != nil {
		t.Skipf("cannot provision the pinned tilth engine (offline?): %v", err)
	}
	if err := os.Symlink(tilthBin, filepath.Join(binDir, vendor.Tilth.Name)); err != nil {
		t.Fatalf("symlink pinned tilth: %v", err)
	}

	astGrepBin := lookpath.Find("ast-grep")
	if astGrepBin == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("ast-grep not on PATH in CI: the test job must install it (uv tool install ast-grep-cli)")
		}
		t.Skip("ast-grep not on PATH; install it to run the anchor canary (brew install ast-grep)")
	}
	if err := os.Symlink(astGrepBin, filepath.Join(binDir, "ast-grep")); err != nil {
		t.Fatalf("symlink ast-grep: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeCanaryRepo builds a two-commit git repo and returns its path and the two
// commit SHAs. greeter.go and util.go feed the grep, symbol, and outline legs; the
// second commit's body-only edit and notes.txt change drive the diff leg's symbol
// row, SHA shortening, and preamble collapse across the sha1..sha2 range.
func writeCanaryRepo(t *testing.T) (repo, sha1, sha2 string) {
	t.Helper()
	repo = t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.email", "t@t.t")
	runGit(t, repo, "config", "user.name", "t")

	writeCanaryFile(t, repo, "greeter.go", greeterV1)
	writeCanaryFile(t, repo, "util.go", utilGo)
	writeCanaryFile(t, repo, "notes.txt", "first note line\n")
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

// writeCanaryFile writes content to dir/name, failing the test on error.
func writeCanaryFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
