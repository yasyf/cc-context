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

// jjOnlyOperators are revset fragments tilth's git-only diff cannot express, so
// a source containing any of them must fall back to jj. Git's own ref suffixes
// (~N, ^) attach directly to a ref, whereas jj's set operators are spaced, so
// matching the spaced forms avoids misreading HEAD~1 as a jj revset.
var jjOnlyOperators = []string{"::", "|", "&", " ~ ", "@-:", "(", ")"}

// Detect reports which VCS manages dir, preferring jj when both are present
// (colocated repos). It walks up from dir looking for a .jj or .git entry,
// returning at the first directory that has either.
func Detect(dir string) Kind {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".jj")); err == nil {
			return JJ
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return Git
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return None
		}
		dir = parent
	}
}

// stagedSource is the tilth source string for a staged (index vs @-) diff; it is
// passed through to tilth verbatim and recognized when building the raw-hunk argv.
const stagedSource = "staged"

// ResolveDiffSource translates a logical diff source into a git ref tilth can
// consume. In a git repo the source passes through once each of its endpoints is
// validated as a real revision (a bogus ref errors loudly). In a jj repo,
// working-tree and ref-relative revsets resolve to a commit-to-commit range
// against the @ commit (tilth's structural diff yields no symbols when the live
// working tree is one side), while genuinely jj-only revsets return
// useTilth=false plus a fallback argv that runs jj diff directly. scope is
// threaded into the jj fallback as a path filter so it is not silently dropped.
func ResolveDiffSource(ctx context.Context, dir, source, scope string) (translated string, useTilth bool, fallbackArgv []string, err error) {
	switch Detect(dir) {
	case Git:
		return resolveGit(ctx, dir, source, gitRefValid)
	case JJ:
		return resolveJJ(ctx, dir, source, scope, defaultBranch, workingCopyCommit, gitCommitID)
	default:
		return source, true, nil, nil
	}
}

// resolveGit passes a git diff source through to tilth after checking every
// non-empty endpoint names a real git revision, so a bogus ref fails loudly
// instead of tilth silently rendering "No changes.". valid is a bare
// `git rev-parse --quiet` (not ^{commit}-coerced), so the multi-value endpoints
// git's diff accepts — HEAD^@, HEAD^!, HEAD^- — validate rather than erroring.
// The working-tree sentinels ("", "uncommitted", "staged") skip the check. valid
// is injectable so the validation is table-testable without a live repo.
func resolveGit(ctx context.Context, dir, source string, valid gitRefValidator) (translated string, useTilth bool, fallbackArgv []string, err error) {
	switch source {
	case "", "uncommitted", stagedSource:
		return source, true, nil, nil
	}
	for _, ep := range splitDiffRange(source) {
		if ep == "" {
			continue
		}
		if !valid(ctx, dir, ep) {
			return "", false, nil, fmt.Errorf("unknown git revision %q in diff source %q", ep, source)
		}
	}
	return source, true, nil, nil
}

// gitRefValidator reports whether ref names one or more real git revisions within
// dir. It is injectable so resolveGit's endpoint validation is table-testable
// without a live repo.
type gitRefValidator func(ctx context.Context, dir, ref string) bool

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

// RawHunkArgvFor builds the raw-hunk argv from an already-resolved diff source,
// so callers that resolve once per diff need not re-snapshot the jj working copy
// for every supplemented file. source is the original logical source (used for
// the jj fallback); tilthSource and useTilth come from ResolveDiffSource.
func RawHunkArgvFor(dir, source, tilthSource string, useTilth bool, file string) []string {
	if !useTilth {
		return []string{"jj", "diff", "--git", "-r", source, file}
	}
	if tilthSource == stagedSource {
		return []string{"git", "-C", dir, "diff", "--cached", "--", file}
	}
	argv := []string{"git", "-C", dir, "diff"}
	// git has no "uncommitted"/empty ref, so a working-tree diff must omit the ref.
	if ref := tilthSource; ref != "" && ref != "uncommitted" {
		argv = append(argv, ref)
	}
	return append(argv, "--", file)
}

// branchLookup resolves the repository's default branch name. It is injectable
// so the pure translation matrix can be tested without shelling out.
type branchLookup func(ctx context.Context, dir string) (string, error)

// workingCopyLookup resolves a jj revset to its git commit id. It is injectable
// so the resolution matrix can be tested without a live jj repo.
type workingCopyLookup func(ctx context.Context, dir, rev string) (string, error)

func resolveJJ(ctx context.Context, dir, source, scope string, branch branchLookup, commit workingCopyLookup, resolve gitRefResolver) (translated string, useTilth bool, fallbackArgv []string, err error) {
	switch translateRevset(source) {
	case translationWorkingTree, translationHEAD:
		parentID, atID, rerr := resolveAtRange(ctx, dir, commit)
		if rerr != nil {
			return "", false, nil, rerr
		}
		return parentID + ".." + atID, true, nil, nil
	case translationRefVsWorking:
		// An embedded-@ source is the one form git and jj can both name (a git ref
		// release@1 vs a jj bookmark@remote); only resolving it in git tells them
		// apart, and it falls back to jj when git cannot. A bare @ never reaches
		// here (isJJNativeRevset short-circuits it), so git is never handed the
		// @-resolves-to-HEAD footgun. Plain refs skip the rev-parse and let tilth
		// resolve them.
		if strings.Contains(source, "@") {
			if _, ok := resolve(ctx, dir, source); !ok {
				return "", false, jjFallbackArgv(source, scope), nil
			}
		}
		atID, rerr := commit(ctx, dir, "@")
		if rerr != nil {
			return "", false, nil, fmt.Errorf("resolve @ commit for %q: %w", dir, rerr)
		}
		return source + ".." + atID, true, nil, nil
	case translationDefaultBranch:
		branchName, lerr := branch(ctx, dir)
		if lerr != nil {
			return "", false, nil, fmt.Errorf("resolve default branch for %q: %w", dir, lerr)
		}
		atID, rerr := commit(ctx, dir, "@")
		if rerr != nil {
			return "", false, nil, fmt.Errorf("resolve @ commit for %q: %w", dir, rerr)
		}
		return branchName + ".." + atID, true, nil, nil
	case translationStaged:
		return stagedSource, true, nil, nil
	case translationPassthrough:
		return source, true, nil, nil
	default:
		return "", false, jjFallbackArgv(source, scope), nil
	}
}

func resolveAtRange(ctx context.Context, dir string, commit workingCopyLookup) (parentID, atID string, err error) {
	atID, err = commit(ctx, dir, "@")
	if err != nil {
		return "", "", fmt.Errorf("resolve @ commit for %q: %w", dir, err)
	}
	parentID, err = commit(ctx, dir, "@-")
	if err != nil {
		return "", "", fmt.Errorf("resolve @- commit for %q: %w", dir, err)
	}
	return parentID, atID, nil
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
	// translationStaged maps "staged" to tilth's staged (index) diff.
	translationStaged
	// translationPassthrough is a committed range handed to tilth as-is.
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
		// git range passes through to tilth.
		if strings.Contains(source, "@") {
			return translationJJOnly
		}
		return translationPassthrough
	}
	return translationRefVsWorking
}

// isJJNativeRevset reports whether source is a revset only jj can name and git
// can never resolve — the exact working-copy markers @ / @- / @+, a leading ~
// negation, and the set operators tilth's git-only diff also rejects. These
// short-circuit to jj without a git rev-parse. The exact-@ match matters most:
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

func jjFallbackArgv(source, scope string) []string {
	argv := []string{"jj", "diff", "--stat"}
	if source != "" {
		argv = append(argv, "-r", source)
	}
	if scope != "" {
		argv = append(argv, scope)
	}
	return argv
}

// gitRefValid reports whether ref parses to at least one real git revision via
// `git rev-parse --quiet`. Unlike a `--verify … ^{commit}` check it accepts the
// multi-value endpoints git's diff accepts (HEAD^@, HEAD^!, HEAD^-), which
// resolve to several ids; a genuinely bogus ref exits nonzero.
func gitRefValid(ctx context.Context, dir, ref string) bool {
	return exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--quiet", ref).Run() == nil //nolint:gosec // fixed git argv; only the working dir and ref vary
}

func workingCopyCommit(ctx context.Context, dir, rev string) (string, error) {
	cmd := exec.CommandContext(ctx, "jj", "log", "--no-graph", "-r", rev, "-T", "commit_id") //nolint:gosec // fixed jj argv; only the working dir and revset vary
	cmd.Dir = dir                                                                            // run inside the working copy so the @ revset snapshots the live tree
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("jj log -r %q: %w", rev, err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("jj log -r %q: empty commit id", rev)
	}
	return id, nil
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
