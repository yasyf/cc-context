// Package vcs detects the working-copy VCS and translates diff sources.
package vcs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// defaultBranchFallback names the branch assumed when origin/HEAD cannot be
// resolved.
const defaultBranchFallback = "main"

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

// GraphiteRepo reports whether root has a live Graphite configuration
// (.git/.graphite_repo_config), the signal that routes ship to the gt lane —
// even over a colocated jj root, since the config lives under .git. A linked
// git worktree's .git is a file rather than a directory, so the join always
// misses there and GraphiteRepo reports false, falling back to the plain-git
// lane.
func GraphiteRepo(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".git", ".graphite_repo_config"))
	return err == nil
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
		return []string{"jj", "file", "show", "-r", "@-", "--", fmt.Sprintf("root:%q", path)}
	default:
		panic(fmt.Sprintf("vcs.ShowFileArgv: kind %d is not Git or JJ", kind))
	}
}

type translation int

const (
	// translationJJOnly marks a source git cannot express; fall back to jj.
	translationJJOnly translation = iota
	// translationWorkingTree maps the live working copy to the @-..@ commit range.
	translationWorkingTree
	// translationHEAD maps jj's @- (working vs @-) to the @-..@ commit range.
	translationHEAD
	// translationDefaultBranch maps trunk()..@ / main..@ / master..@ to a
	// branch..@ commit range.
	translationDefaultBranch
	// translationRefVsWorking maps a single git ref R to the R..@ commit range.
	translationRefVsWorking
	// translationStaged marks the staged (index vs @-) diff.
	translationStaged
	// translationPassthrough is a committed range diffed endpoint-to-endpoint as-is.
	translationPassthrough
)

// translateRevset classifies a diff source into a translation strategy. It is a
// pure function so the full matrix is table-testable.
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
	case "trunk()..@", "main..@", "master..@":
		return translationDefaultBranch
	}
	if isJJNativeRevset(source) {
		return translationJJOnly
	}
	if strings.Contains(source, "..") {
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
// resolve to several ids; a genuinely bogus ref exits nonzero.
func gitRefValid(ctx context.Context, dir, ref string) bool {
	return exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--quiet", ref).Run() == nil //nolint:gosec // fixed git argv; only the working dir and ref vary
}

func defaultBranch(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "symbolic-ref", "refs/remotes/origin/HEAD").Output() //nolint:gosec // fixed git argv; only the working dir varies
	if err != nil {
		return defaultBranchFallback, nil
	}
	ref := strings.TrimSpace(string(out))
	if name := strings.TrimPrefix(ref, "refs/remotes/origin/"); name != "" && name != ref {
		return name, nil
	}
	return defaultBranchFallback, nil
}
