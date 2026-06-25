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
// consume. In a git repo the source passes through untouched. In a jj repo,
// working-tree and ref-relative revsets resolve to a commit-to-commit range
// against the @ commit (tilth's structural diff yields no symbols when the live
// working tree is one side), while genuinely jj-only revsets return
// useTilth=false plus a fallback argv that runs jj diff directly. scope is
// threaded into the jj fallback as a path filter so it is not silently dropped.
func ResolveDiffSource(ctx context.Context, dir, source, scope string) (translated string, useTilth bool, fallbackArgv []string, err error) {
	switch Detect(dir) {
	case Git:
		return source, true, nil, nil
	case JJ:
		return resolveJJ(ctx, dir, source, scope, defaultBranch, workingCopyCommit)
	default:
		return source, true, nil, nil
	}
}

// RawHunkArgv builds the `git diff`/`jj diff` argv that prints one file's raw
// textual hunk against source.
func RawHunkArgv(ctx context.Context, dir, source, file string) ([]string, error) {
	tilthSource, useTilth, _, err := ResolveDiffSource(ctx, dir, source, "")
	if err != nil {
		return nil, fmt.Errorf("resolve diff source for raw hunk: %w", err)
	}
	return RawHunkArgvFor(dir, source, tilthSource, useTilth, file), nil
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

func resolveJJ(ctx context.Context, dir, source, scope string, branch branchLookup, commit workingCopyLookup) (translated string, useTilth bool, fallbackArgv []string, err error) {
	switch translateRevset(source) {
	case translationWorkingTree, translationHEAD:
		parentID, atID, rerr := resolveAtRange(ctx, dir, commit)
		if rerr != nil {
			return "", false, nil, rerr
		}
		return parentID + ".." + atID, true, nil, nil
	case translationRefVsWorking:
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
	if strings.HasPrefix(source, "~") {
		return translationJJOnly
	}
	for _, op := range jjOnlyOperators {
		if strings.Contains(source, op) {
			return translationJJOnly
		}
	}
	if strings.Contains(source, "@") {
		return translationJJOnly
	}
	if strings.Contains(source, "..") {
		return translationPassthrough
	}
	return translationRefVsWorking
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
