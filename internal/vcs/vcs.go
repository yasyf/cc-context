// Package vcs detects the working-copy VCS and translates diff sources.
package vcs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
)

// Kind identifies the VCS managing a working directory.
type Kind int

const (
	// Git is a plain git working copy.
	Git Kind = iota
	// JJ is a jj working copy (which may be colocated over git).
	JJ
	// None is no recognized VCS.
	None
)

// jjOnlyOperators are revset fragments git cannot express, so a source containing
// any of them is a jj-only revset. Git's own ref suffixes
// (~N, ^) attach directly to a ref, whereas jj's set operators are spaced, so
// matching the spaced forms avoids misreading HEAD~1 as a jj revset.
var jjOnlyOperators = []string{"::", "|", "&", " ~ ", "@-:", "(", ")"}

// Detect reports which VCS manages dir, preferring jj when both are present
// (colocated repos). It walks up from dir looking for a .jj or .git entry,
// returning at the first directory that has either.
func Detect(dir string) Kind {
	kind, _ := DetectRoot(dir)
	return kind
}

// DetectRoot reports which VCS manages dir and the directory holding the .jj or
// .git marker, preferring jj when both are present (colocated repos). It walks
// up from dir looking for either entry, returning at the first directory that
// has one; root is "" when neither is found.
func DetectRoot(dir string) (Kind, string) {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".jj")); err == nil {
			return JJ, dir
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return Git, dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return None, ""
		}
		dir = parent
	}
}

// GraphiteRepo reports whether c's repository has a live Graphite configuration
// (.graphite_repo_config in the git common dir), the signal that routes ship to
// the gt lane — even over a colocated jj root, since the config lives under
// .git, and from a linked worktree, whose own admin dir never holds it. A jj
// repository with no git backing has no common dir and so no config. Only a
// missing config answers false: a config that exists but cannot be stat'd is an
// error, since routing a mutation off the gt lane because a probe failed is a
// different answer from routing it off because Graphite is not here.
func GraphiteRepo(c Checkout) (bool, error) {
	if c.CommonDir == "" {
		return false, nil
	}
	config := filepath.Join(c.CommonDir, ".graphite_repo_config")
	if _, err := os.Stat(config); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat graphite config %q: %w", config, err)
	}
	return true, nil
}

// stagedSource is the source string for a staged (index vs @-) diff; ResolveDiffPlan
// routes it to stagedPlan and translateRevset classifies it as translationStaged.
const stagedSource = "staged"

// splitDiffRange splits a git diff source into its endpoints, honoring both the
// symmetric "A...B" and the "A..B" range forms; a bare ref yields itself.
func splitDiffRange(source string) []string {
	if strings.Contains(source, "...") {
		return strings.SplitN(source, "...", 2)
	}
	if strings.Contains(source, "..") {
		return strings.SplitN(source, "..", 2)
	}
	return []string{source}
}

// ShowFileArgv builds the argv that prints path's committed content — git's HEAD
// blob or jj's @- revision — as the base image of a hunk diff. path is
// repo-root-relative: git's HEAD:<path> is a root-anchored tree path and jj's
// root:"<path>" fileset pins the root frame, so both resolve from any working
// directory. --end-of-options keeps a flag-like path from being parsed as a flag
// (the git-show injection fix). kind must be Git or JJ; anything else panics.
func ShowFileArgv(kind Kind, path string) []string {
	switch kind {
	case Git:
		return []string{"git", "show", "--end-of-options", "HEAD:" + path}
	case JJ:
		return []string{"jj", "--ignore-working-copy", "file", "show", "-r", "@-", "--", JJRootPattern(path)}
	default:
		panic(fmt.Sprintf("vcs.ShowFileArgv: kind %d is not Git or JJ", kind))
	}
}

// jjStringLiteral escaper: jj 0.43's revset and fileset grammars share one string
// literal, and its escape vocabulary is \t \r \n \0 \e \xHH — \a, \b, \f, \v, \u,
// and \U are syntax errors. Raw UTF-8 is legal inside the quotes, so backslash and
// double quote are the only bytes that need escaping. Go's %q is the trap: it
// spells every unprintable rune \uXXXX, so a zero-width joiner or a non-breaking
// space in a path or bookmark name renders a pattern jj refuses to parse. \xHH is
// no substitute either — jj reads it as a per-rune Latin-1 codepoint, not a byte.
var jjStringLiteral = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// JJRootPattern renders a repo-root-relative path as jj's root-anchored fileset
// pattern, root:"…".
func JJRootPattern(path string) string {
	return `root:"` + jjStringLiteral.Replace(path) + `"`
}

// JJExactPattern renders name as jj's exact string pattern, exact:"…", so a
// bookmark name carrying an '@' (or any character jj would otherwise read as a
// bookmark@remote symbol or a glob metacharacter) is matched literally.
func JJExactPattern(name string) string {
	return `exact:"` + jjStringLiteral.Replace(name) + `"`
}

type translation int

const (
	// translationJJOnly marks a source git cannot express; fall back to jj.
	translationJJOnly translation = iota
	// translationWorkingTree maps the live working copy to the @-..@ commit range.
	translationWorkingTree
	// translationHEAD maps jj's @- (working vs @-) to the @-..@ commit range.
	translationHEAD
	// translationDefaultBranch maps trunk()..@ to a <trunk>..@ commit range,
	// trunk being the branch the repository designates as its default.
	translationDefaultBranch
	// translationRangeVsWorking maps an explicit <rev>..@ range to that same
	// range, the left endpoint read exactly as written.
	translationRangeVsWorking
	// translationRefVsWorking maps a single git ref R to the R..@ commit range.
	translationRefVsWorking
	// translationStaged marks the staged (index vs @-) diff.
	translationStaged
	// translationPassthrough is a committed range diffed endpoint-to-endpoint as-is.
	translationPassthrough
)

// translateRevset classifies a diff source into a translation strategy. It is a
// pure function so the full matrix is table-testable. trunk() is the one source
// whose branch this package resolves; every other name is the name the user
// meant, so "master..@" diffs the branch called master rather than whichever
// branch the repository designates as its trunk.
func translateRevset(source string) translation {
	switch source {
	case "", "uncommitted":
		return translationWorkingTree
	case "@-":
		return translationHEAD
	case "@":
		return translationJJOnly
	case stagedSource:
		return translationStaged
	case "trunk()..@":
		return translationDefaultBranch
	}
	if isJJNativeRevset(source) {
		return translationJJOnly
	}
	if left, right, ok := strings.Cut(source, ".."); ok {
		// A right endpoint of @ names the live working copy, which only jj can
		// read: git resolves @ to HEAD, jj's @-, and would silently drop every
		// uncommitted change from the after side.
		if left != "" && right == "@" {
			return translationRangeVsWorking
		}
		// git cannot rev-parse a range to disambiguate, so a range with an
		// embedded-@ endpoint (a jj bookmark@remote) stays routed to jj; a plain
		// git range passes through as a committed range.
		if strings.Contains(source, "@") {
			return translationJJOnly
		}
		return translationPassthrough
	}
	return translationRefVsWorking
}

// isJJNativeRevset reports whether source is a revset only jj can name and git
// can never resolve — the exact working-copy markers @ / @- / @+, a leading ~
// negation, and the jj set operators git cannot express. These short-circuit to
// the non-symbolic jj plan without a git rev-parse. The exact-@ match matters most:
// git resolves a bare @ to HEAD (jj's @-), so @ must never be handed to git.
// Everything else is a git candidate the resolver disambiguates — a plain ref
// (HEAD~N, branch, sha) or an embedded-@ form that could be either a git ref
// (release@1) or a jj bookmark@remote (main@origin), told apart only by whether
// git actually resolves it.
func isJJNativeRevset(source string) bool {
	switch source {
	case "@", "@-", "@+":
		return true
	}
	if strings.HasPrefix(source, "~") {
		return true
	}
	for _, op := range jjOnlyOperators {
		if strings.Contains(source, op) {
			return true
		}
	}
	return false
}

// gitRefValid reports whether ref parses to at least one real git revision via
// `git rev-parse --quiet`. Unlike a `--verify … ^{commit}` check it accepts the
// multi-value endpoints git's diff accepts (HEAD^@, HEAD^!, HEAD^-), which
// resolve to several ids; a genuinely bogus ref exits nonzero. --end-of-options
// is what makes the answer honest: without it rev-parse echoes an unrecognized
// option back and exits 0, so "--output=/tmp/x" reads as a valid revision and
// travels on into a diff that writes the file.
//
// The exit code answers only the ref question, so a child that could not run at
// all — no git on PATH, a working directory that vanished — comes back as an
// error rather than as a ref that does not exist.
func gitRefValid(ctx context.Context, dir, ref string) (bool, error) {
	_, code, _, err := render.RunCLIExitCodeDir(ctx, dir, "git", []string{"rev-parse", "--quiet", "--end-of-options", ref})
	if err != nil {
		return false, fmt.Errorf("rev-parse %q: %w", ref, err)
	}
	return code == 0, nil
}
