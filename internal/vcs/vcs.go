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

// ResolveDiffSource translates a logical diff source into a git ref tilth can
// consume. In a git repo the source passes through untouched. In a jj repo,
// common revsets are mapped to git refs (the colocated git HEAD tracks jj's @-);
// genuinely jj-only revsets return useTilth=false plus a fallback argv that runs
// jj diff directly. scope is threaded into the jj fallback as a path filter so
// it is not silently dropped.
func ResolveDiffSource(ctx context.Context, dir, source, scope string) (translated string, useTilth bool, fallbackArgv []string, err error) {
	switch Detect(dir) {
	case Git:
		return source, true, nil, nil
	case JJ:
		return resolveJJ(ctx, dir, source, scope, defaultBranch)
	default:
		return source, true, nil, nil
	}
}

// RawHunkArgv builds the `git diff`/`jj diff` argv that prints one file's raw textual hunk against source.
func RawHunkArgv(ctx context.Context, dir, source, file string) ([]string, error) {
	translated, useTilth, _, err := ResolveDiffSource(ctx, dir, source, "")
	if err != nil {
		return nil, fmt.Errorf("resolve diff source for raw hunk: %w", err)
	}
	if !useTilth {
		return []string{"jj", "diff", "--git", "-r", source, file}, nil
	}
	argv := []string{"git", "-C", dir, "diff"}
	// git has no "uncommitted"/empty ref, so a working-tree diff must omit the ref.
	if ref := translated; ref != "" && ref != "uncommitted" {
		argv = append(argv, ref)
	}
	return append(argv, "--", file), nil
}

// branchLookup resolves the repository's default branch name. It is injectable
// so the pure translation matrix can be tested without shelling out.
type branchLookup func(ctx context.Context, dir string) (string, error)

func resolveJJ(ctx context.Context, dir, source, scope string, lookup branchLookup) (translated string, useTilth bool, fallbackArgv []string, err error) {
	switch translateRevset(source) {
	case translationWorkingTree:
		return "", true, nil, nil
	case translationHEAD:
		return "HEAD", true, nil, nil
	case translationDefaultBranch:
		branch, lerr := lookup(ctx, dir)
		if lerr != nil {
			return "", false, nil, fmt.Errorf("resolve default branch for %q: %w", dir, lerr)
		}
		return branch, true, nil, nil
	case translationPassthrough:
		return source, true, nil, nil
	default:
		return "", false, jjFallbackArgv(source, scope), nil
	}
}

type translation int

const (
	// translationJJOnly marks a source git cannot express; fall back to jj.
	translationJJOnly translation = iota
	// translationWorkingTree maps to tilth's bare working-tree diff.
	translationWorkingTree
	// translationHEAD maps to the git ref "HEAD" (jj's @-, the parent of the
	// working copy, which the colocated git HEAD tracks).
	translationHEAD
	// translationDefaultBranch maps to the repo's default branch name.
	translationDefaultBranch
	// translationPassthrough is a plain git-looking ref handed to tilth as-is.
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
	return translationPassthrough
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
