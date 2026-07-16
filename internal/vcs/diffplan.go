package vcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DiffPlan is the resolved shape of one logical diff: the files that changed and
// the before/after content of each. A symbolic plan (Symbolic true) carries
// Before and After byte accessors per file, from which the caller computes hunks
// and structural symbol changes; a non-symbolic plan (a jj revset spanning
// several commits or a set expression) leaves Before/After nil and carries Raw,
// which yields that file's `jj diff --git` text for the caller to emit as-is.
type DiffPlan struct {
	// Label is the human-readable diff source for the header (e.g. "uncommitted",
	// "staged", "main..feat").
	Label string
	// Files are the changed paths, repo-root-relative, in the VCS's own order.
	Files []string
	// Before and After read path's pre- and post-image bytes; both nil when
	// Symbolic is false. A path absent from a side (a file added or deleted across
	// the diff) yields empty bytes and a nil error.
	Before func(path string) ([]byte, error)
	After  func(path string) ([]byte, error)
	// Symbolic reports whether Before/After are populated (a clean before/after
	// pair per file). When false the caller renders Raw's output instead.
	Symbolic bool
	// Renames maps a renamed file's post-image path (its Files entry) to its
	// pre-image path; Before redirects to it and the renderer shows "old → new".
	// Nil when nothing is renamed.
	Renames map[string]string
	// Raw yields path's raw `jj diff --git` text; non-nil iff Symbolic is false.
	// It is a deviation from the four-field spec: a spanning revset has no single
	// before/after pair, so the renderer emits jj's own git-format hunks per file
	// (strictly better than the old --stat fallback).
	Raw func(path string) (string, error)
}

// ResolveDiffPlan resolves a logical diff source into a DiffPlan for the VCS
// managing dir, reusing the shared source-classification matrix (translateRevset /
// isJJNativeRevset / gitRefValid). Files and blob accessors are
// repo-root-relative: the repo root is resolved once and every child process runs
// there, so a jj working-directory-relative name-only listing lines up with the
// root-anchored blob reads.
func ResolveDiffPlan(ctx context.Context, dir, source string) (DiffPlan, error) {
	kind := Detect(dir)
	if kind == None {
		return DiffPlan{}, fmt.Errorf("diff: no git or jj repository in %q", dir)
	}
	root, err := diffRoot(ctx, dir, kind)
	if err != nil {
		return DiffPlan{}, err
	}
	if source == stagedSource {
		return stagedPlan(ctx, root)
	}
	switch kind {
	case Git:
		return gitDiffPlan(ctx, root, source)
	default:
		return jjDiffPlan(ctx, root, source)
	}
}

// diffLabel names the diff source for the rendered header, mapping the empty
// working-tree source to "uncommitted".
func diffLabel(source string) string {
	if source == "" {
		return "uncommitted"
	}
	return source
}

// gitDiffPlan builds a symbolic plan for a git working copy. The working-tree
// source diffs HEAD against the on-disk worktree; a range or bare ref reads both
// endpoints as committed blobs after validating each names a real revision.
func gitDiffPlan(ctx context.Context, root, source string) (DiffPlan, error) {
	if source == "" || source == "uncommitted" {
		files, renames, err := gitNameStatus(ctx, root, "diff", "--name-status", "-M", "HEAD")
		if err != nil {
			return DiffPlan{}, err
		}
		// git diff HEAD lists only tracked changes; append the untracked worktree
		// files so a brand-new file still renders (Before empty, After worktree).
		untracked, err := listLines(ctx, root, "git", "ls-files", "--others", "--exclude-standard")
		if err != nil {
			return DiffPlan{}, err
		}
		files = append(files, untracked...)
		return DiffPlan{
			Label:    diffLabel(source),
			Files:    files,
			Symbolic: true,
			Renames:  renames,
			Before:   renameAware(committedBlobFn(ctx, root, Git, "HEAD"), renames),
			After:    worktreeFn(root),
		}, nil
	}

	for _, ep := range splitDiffRange(source) {
		if ep != "" && !gitRefValid(ctx, root, ep) {
			return DiffPlan{}, fmt.Errorf("unknown git revision %q in diff source %q", ep, source)
		}
	}

	var beforeRef string
	var after func(string) ([]byte, error)
	var filesArgv []string
	switch {
	case strings.Contains(source, "..."):
		left, right, _ := strings.Cut(source, "...")
		base, err := gitMergeBase(ctx, root, orHEAD(left), orHEAD(right))
		if err != nil {
			return DiffPlan{}, err
		}
		beforeRef = base
		after = committedBlobFn(ctx, root, Git, orHEAD(right))
		filesArgv = []string{"diff", "--name-status", "-M", source}
	case strings.Contains(source, ".."):
		left, right, _ := strings.Cut(source, "..")
		beforeRef = orHEAD(left)
		if right == "" {
			after = worktreeFn(root)
			filesArgv = []string{"diff", "--name-status", "-M", beforeRef}
		} else {
			after = committedBlobFn(ctx, root, Git, right)
			filesArgv = []string{"diff", "--name-status", "-M", beforeRef, right}
		}
	default:
		beforeRef = source
		after = worktreeFn(root)
		filesArgv = []string{"diff", "--name-status", "-M", source}
	}

	files, renames, err := gitNameStatus(ctx, root, filesArgv...)
	if err != nil {
		return DiffPlan{}, err
	}
	return DiffPlan{
		Label:    diffLabel(source),
		Files:    files,
		Symbolic: true,
		Renames:  renames,
		Before:   renameAware(committedBlobFn(ctx, root, Git, beforeRef), renames),
		After:    after,
	}, nil
}

// stagedPlan builds a symbolic plan for the git index: HEAD against the staged
// tree (git show :0:path), reused verbatim in a colocated jj repo.
func stagedPlan(ctx context.Context, root string) (DiffPlan, error) {
	files, renames, err := gitNameStatus(ctx, root, "diff", "--cached", "--name-status", "-M")
	if err != nil {
		return DiffPlan{}, err
	}
	return DiffPlan{
		Label:    stagedSource,
		Files:    files,
		Symbolic: true,
		Renames:  renames,
		Before:   renameAware(committedBlobFn(ctx, root, Git, "HEAD"), renames),
		After:    committedBlobFn(ctx, root, Git, ":0"),
	}, nil
}

// jjDiffPlan builds a plan for a jj working copy, classifying source through the
// shared translateRevset matrix. Working-tree, ref, and default-branch sources
// resolve to a symbolic <base>..@ pair (after side is the live worktree); a git
// range reads both committed endpoints; a genuinely jj-only revset that may span
// several commits yields a non-symbolic plan whose Raw runs `jj diff --git`.
func jjDiffPlan(ctx context.Context, root, source string) (DiffPlan, error) {
	label := diffLabel(source)
	switch translateRevset(source) {
	case translationWorkingTree, translationHEAD:
		return symbolicJJ(ctx, root, label, "@-", "@", worktreeFn(root))
	case translationDefaultBranch:
		branch, err := defaultBranch(ctx, root)
		if err != nil {
			return DiffPlan{}, fmt.Errorf("resolve default branch for %q: %w", root, err)
		}
		return symbolicJJ(ctx, root, label, branch, "@", worktreeFn(root))
	case translationRefVsWorking:
		if gitOnlyRevSyntax(source) && colocatedGit(root) {
			return gitDiffPlan(ctx, root, source)
		}
		return symbolicJJ(ctx, root, label, source, "@", worktreeFn(root))
	case translationPassthrough:
		if colocatedGit(root) {
			return gitDiffPlan(ctx, root, source)
		}
		left, right, _ := strings.Cut(source, "..")
		return symbolicJJ(ctx, root, label, orHEAD(left), orHEAD(right), committedBlobFn(ctx, root, JJ, orHEAD(right)))
	default:
		files, err := listLines(ctx, root, "jj", "diff", "--name-only", "-r", source)
		if err != nil {
			return DiffPlan{}, err
		}
		return DiffPlan{Label: label, Files: files, Symbolic: false, Raw: jjRawFn(ctx, root, source)}, nil
	}
}

// symbolicJJ assembles a symbolic jj plan diffing fromRev against toRev, listing
// files via `jj diff --name-only --from --to` (root-relative because it runs at
// the repo root). after is supplied so the working-copy lanes can read the live
// worktree while a committed range reads toRev's blob.
func symbolicJJ(ctx context.Context, root, label, fromRev, toRev string, after func(string) ([]byte, error)) (DiffPlan, error) {
	files, renames, err := jjNameStatus(ctx, root, fromRev, toRev)
	if err != nil {
		return DiffPlan{}, err
	}
	return DiffPlan{
		Label:    label,
		Files:    files,
		Symbolic: true,
		Renames:  renames,
		Before:   renameAware(committedBlobFn(ctx, root, JJ, fromRev), renames),
		After:    after,
	}, nil
}

// gitNameStatus runs a `git <args…>` name-status enumeration (rename detection on
// via -M) at root, returning the changed post-image paths and a post→pre rename
// map. A rename or copy line is "R<sim>\told\tnew"; every other status is a single
// tab-separated path.
func gitNameStatus(ctx context.Context, root string, args ...string) (files []string, renames map[string]string, err error) {
	lines, err := listLines(ctx, root, "git", args...)
	if err != nil {
		return nil, nil, err
	}
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if s := fields[0]; (s[0] == 'R' || s[0] == 'C') && len(fields) >= 3 {
			old, dst := fields[1], fields[2]
			files = append(files, dst)
			if s[0] == 'R' {
				renames = putRename(renames, dst, old)
			}
			continue
		}
		files = append(files, fields[len(fields)-1])
	}
	return files, renames, nil
}

// jjNameStatus runs `jj diff --summary --from --to` at root, returning the changed
// post-image paths and a post→pre rename map. jj renders a rename as
// "R <prefix>{old => new}<suffix>" (a copy as "C …"); every other status is
// "<flag> <path>".
func jjNameStatus(ctx context.Context, root, fromRev, toRev string) (files []string, renames map[string]string, err error) {
	lines, err := listLines(ctx, root, "jj", "diff", "--summary", "--from", fromRev, "--to", toRev)
	if err != nil {
		return nil, nil, err
	}
	for _, line := range lines {
		status, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if old, dst, ok := parseJJRename(rest); ok {
			files = append(files, dst)
			if status == "R" {
				renames = putRename(renames, dst, old)
			}
			continue
		}
		files = append(files, rest)
	}
	return files, renames, nil
}

// parseJJRename expands jj's compact rename spec "<prefix>{old => new}<suffix>"
// into the full pre- and post-image paths; ok is false for a plain path.
func parseJJRename(spec string) (old, dst string, ok bool) {
	open := strings.IndexByte(spec, '{')
	end := strings.IndexByte(spec, '}')
	if open < 0 || end < open {
		return "", "", false
	}
	prefix, suffix := spec[:open], spec[end+1:]
	a, b, found := strings.Cut(spec[open+1:end], " => ")
	if !found {
		return "", "", false
	}
	return prefix + a + suffix, prefix + b + suffix, true
}

// putRename records a post→pre rename, allocating the map on first use.
func putRename(renames map[string]string, dst, old string) map[string]string {
	if renames == nil {
		renames = map[string]string{}
	}
	renames[dst] = old
	return renames
}

// renameAware wraps a blob accessor so a rename's post-image path reads its
// pre-image blob at the old path; a non-rename path passes through. base is
// returned unwrapped when nothing is renamed.
func renameAware(base func(string) ([]byte, error), renames map[string]string) func(string) ([]byte, error) {
	if len(renames) == 0 {
		return base
	}
	return func(path string) ([]byte, error) {
		if old, ok := renames[path]; ok {
			return base(old)
		}
		return base(path)
	}
}

// committedBlobFn returns a blob accessor for path at rev's committed tree,
// yielding empty bytes for a path absent from that tree (a file added or removed
// across the diff). The @-/HEAD base reuses ShowFileArgv; other revs build the
// equivalent `git show <rev>:path` / `jj file show -r <rev>` argv.
func committedBlobFn(ctx context.Context, root string, kind Kind, rev string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if !treeHasPath(ctx, root, kind, rev, path) {
			return nil, nil
		}
		argv := blobArgv(kind, rev, path)
		out, err := runIn(ctx, root, argv[0], argv[1:]...)
		if err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", path, rev, err)
		}
		return out, nil
	}
}

// blobArgv builds the argv that prints path's content at rev. The canonical base
// (git HEAD, jj @-) reuses ShowFileArgv; every other rev builds the equivalent.
func blobArgv(kind Kind, rev, path string) []string {
	switch {
	case kind == Git && rev == "HEAD":
		return ShowFileArgv(Git, path)
	case kind == JJ && rev == "@-":
		return ShowFileArgv(JJ, path)
	case kind == Git:
		return []string{"git", "show", "--end-of-options", rev + ":" + path}
	default:
		return []string{"jj", "file", "show", "-r", rev, "--", fmt.Sprintf("root:%q", path)}
	}
}

// treeHasPath reports whether path exists in rev's tree, so a blob accessor can
// yield empty bytes rather than error on a file one side lacks. git uses
// `cat-file -e` (silent, exit-coded); jj uses `file list`, whose stdout is empty
// for a path it does not track at rev.
func treeHasPath(ctx context.Context, root string, kind Kind, rev, path string) bool {
	switch kind {
	case Git:
		cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", rev+":"+path) //nolint:gosec // fixed git argv; only root, rev, and a VCS-enumerated path vary
		cmd.Dir = root
		return cmd.Run() == nil
	default:
		out, err := runIn(ctx, root, "jj", "file", "list", "-r", rev, "--", fmt.Sprintf("root:%q", path))
		return err == nil && len(bytes.TrimSpace(out)) > 0
	}
}

// worktreeFn returns a blob accessor reading path from the on-disk worktree,
// treating a missing file as empty bytes so a deletion still reads as empty.
func worktreeFn(root string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(root, path)) //nolint:gosec // path is a VCS-enumerated change target, not untrusted input
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read worktree %s: %w", path, err)
		}
		return data, nil
	}
}

// jjRawFn returns a Raw accessor running `jj diff --git` for one file of a
// spanning revset.
func jjRawFn(ctx context.Context, root, revset string) func(string) (string, error) {
	return func(path string) (string, error) {
		out, err := runIn(ctx, root, "jj", "diff", "--git", "-r", revset, "--", fmt.Sprintf("root:%q", path))
		if err != nil {
			return "", fmt.Errorf("jj diff --git -r %q: %w", revset, err)
		}
		return string(out), nil
	}
}

// gitMergeBase resolves the merge base of two revisions for a symmetric (A...B)
// range's before side.
func gitMergeBase(ctx context.Context, root, a, b string) (string, error) {
	out, err := runIn(ctx, root, "git", "merge-base", a, b)
	if err != nil {
		return "", fmt.Errorf("merge-base %s %s: %w", a, b, err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("merge-base %s %s: empty", a, b)
	}
	return id, nil
}

// diffRoot resolves dir's repository root so every child process runs there and
// path names stay root-relative across enumeration and blob reads.
func diffRoot(ctx context.Context, dir string, kind Kind) (string, error) {
	var argv []string
	switch kind {
	case Git:
		argv = []string{"git", "rev-parse", "--show-toplevel"}
	default:
		argv = []string{"jj", "workspace", "root"}
	}
	out, err := runIn(ctx, dir, argv[0], argv[1:]...)
	if err != nil {
		return "", fmt.Errorf("resolve repo root for %q: %w", dir, err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("resolve repo root for %q: empty", dir)
	}
	return root, nil
}

// gitOnlyRevSyntax reports whether source uses revision syntax only git parses
// (HEAD names, ^ parents, ~N ancestors) — jj rejects these outright.
func gitOnlyRevSyntax(source string) bool {
	return strings.ContainsAny(source, "^~") || strings.HasPrefix(source, "HEAD")
}

// colocatedGit reports whether root also carries a git store, so git can resolve
// git-syntax diff sources a jj working copy would reject.
func colocatedGit(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".git"))
	return err == nil
}

// orHEAD substitutes HEAD for an empty range endpoint, matching git's own default.
func orHEAD(ref string) string {
	if ref == "" {
		return "HEAD"
	}
	return ref
}

// listLines runs a name-listing command at dir and splits its stdout into
// non-empty, whitespace-trimmed lines.
func listLines(ctx context.Context, dir, name string, args ...string) ([]string, error) {
	out, err := runIn(ctx, dir, name, args...)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// runIn runs name at dir, returning stdout bytes and wrapping a nonzero exit with
// the child's stderr.
func runIn(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name/args are fixed VCS verbs; only dir, revs, and VCS-enumerated paths vary
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
