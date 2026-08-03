package vcs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
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
// Every endpoint a user typed reaches git as a GitRef through GitArgs, which
// interposes --end-of-options ahead of it, so a source spelled like an option
// ("--output=/tmp/x") is refused rather than obeyed.
func gitDiffPlan(ctx context.Context, root, source string) (DiffPlan, error) {
	if source == "" || source == "uncommitted" {
		files, renames, err := gitDiffFiles(ctx, GitArgs{Dir: root, Sub: []string{"diff", "-M"}, Revs: []GitRef{HeadRef}})
		if err != nil {
			return DiffPlan{}, err
		}
		// git diff HEAD lists only tracked changes; append the untracked worktree
		// files so a brand-new file still renders (Before empty, After worktree).
		untracked, err := GitPaths(ctx, GitArgs{Dir: root, Sub: []string{"ls-files", "--others", "--exclude-standard"}})
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
		if ep == "" {
			continue
		}
		valid, err := gitRefValid(ctx, root, ep)
		if err != nil {
			return DiffPlan{}, err
		}
		if !valid {
			return DiffPlan{}, fmt.Errorf("unknown git revision %q in diff source %q", ep, source)
		}
	}

	var beforeRef string
	var after func(string) ([]byte, error)
	var revs []GitRef
	switch {
	case strings.Contains(source, "..."):
		left, right, _ := strings.Cut(source, "...")
		base, err := gitMergeBase(ctx, root, UnsafeRef(orHEAD(left)), UnsafeRef(orHEAD(right)))
		if err != nil {
			return DiffPlan{}, err
		}
		beforeRef = base
		after = committedBlobFn(ctx, root, Git, orHEAD(right))
		revs = []GitRef{UnsafeRef(source)}
	case strings.Contains(source, ".."):
		left, right, _ := strings.Cut(source, "..")
		beforeRef = orHEAD(left)
		if right == "" {
			after = worktreeFn(root)
			revs = []GitRef{UnsafeRef(beforeRef)}
		} else {
			after = committedBlobFn(ctx, root, Git, right)
			revs = []GitRef{UnsafeRef(beforeRef), UnsafeRef(right)}
		}
	default:
		beforeRef = source
		after = worktreeFn(root)
		revs = []GitRef{UnsafeRef(source)}
	}

	files, renames, err := gitDiffFiles(ctx, GitArgs{Dir: root, Sub: []string{"diff", "-M"}, Revs: revs})
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

// gitIndexRev addresses git's staged tree as a revision: `git show :0:path` reads
// the stage-0 index entry.
const gitIndexRev = ":0"

// stagedPlan builds a symbolic plan for the git index: HEAD against the staged
// tree (git show :0:path), reused verbatim in a colocated jj repo.
func stagedPlan(ctx context.Context, root string) (DiffPlan, error) {
	files, renames, err := gitDiffFiles(ctx, GitArgs{Dir: root, Sub: []string{"diff", "--cached", "-M"}})
	if err != nil {
		return DiffPlan{}, err
	}
	return DiffPlan{
		Label:    stagedSource,
		Files:    files,
		Symbolic: true,
		Renames:  renames,
		Before:   renameAware(committedBlobFn(ctx, root, Git, "HEAD"), renames),
		After:    committedBlobFn(ctx, root, Git, gitIndexRev),
	}, nil
}

// jjDiffPlan builds a plan for a jj working copy, classifying source through the
// shared translateRevset matrix. Working-tree, ref, and trunk sources resolve to
// a symbolic <base>..@ pair (after side is the live worktree); a git range reads
// both committed endpoints; a genuinely jj-only revset that may span several
// commits yields a non-symbolic plan whose Raw runs `jj diff --git`. Only
// trunk() consults the repository's designated default branch, and only through
// ResolveTrunk against origin, the remote whose HEAD names it; a source naming a
// branch names that branch.
func jjDiffPlan(ctx context.Context, root, source string) (DiffPlan, error) {
	label := diffLabel(source)
	switch translateRevset(source) {
	case translationWorkingTree, translationHEAD:
		return symbolicJJ(ctx, root, label, "@-", "@", worktreeFn(root))
	case translationDefaultBranch:
		trunk, err := ResolveTrunk(ctx, root, "origin")
		if err != nil {
			return DiffPlan{}, fmt.Errorf("resolve trunk() for %q: %w", root, err)
		}
		return symbolicJJ(ctx, root, label, trunk.Name(), "@", worktreeFn(root))
	case translationRangeVsWorking:
		left, _, _ := strings.Cut(source, "..")
		return symbolicJJ(ctx, root, label, left, "@", worktreeFn(root))
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
		files, err := jjLines(ctx, root, "diff", "--name-only", "-r", source)
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

// gitDiffFiles runs a's name-status diff, returning the changed post-image paths
// and a post→pre rename map. A copy names a pre-image too but is no rename: its
// destination is new content, so Before reads the destination's own (absent)
// blob rather than the source it was copied from.
func gitDiffFiles(ctx context.Context, a GitArgs) (files []string, renames map[string]string, err error) {
	entries, err := GitNameStatus(ctx, a)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		files = append(files, e.New)
		if e.Renamed() {
			renames = putRename(renames, e.New, e.Old)
		}
	}
	return files, renames, nil
}

// jjNameStatus runs `jj diff --summary --from --to` at root, returning the changed
// post-image paths and a post→pre rename map. jj renders a rename as
// "R <prefix>{old => new}<suffix>" (a copy as "C …"); every other status is
// "<flag> <path>".
func jjNameStatus(ctx context.Context, root, fromRev, toRev string) (files []string, renames map[string]string, err error) {
	lines, err := jjLines(ctx, root, "diff", "--summary", "--from", fromRev, "--to", toRev)
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
		has, err := treeHasPath(ctx, root, kind, rev, path)
		if err != nil {
			return nil, err
		}
		if !has {
			return nil, nil
		}
		argv := blobArgv(kind, rev, path)
		out, err := render.RunCLIDir(ctx, root, argv[0], argv[1:])
		if err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", path, rev, err)
		}
		return []byte(out), nil
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
		return []string{"jj", "--ignore-working-copy", "file", "show", "-r", rev, "--", JJRootPattern(path)}
	}
}

// treeHasPath reports whether path exists in rev's tree, so a blob accessor can
// yield empty bytes rather than error on a file one side lacks. Every backend
// enumerates rather than probes, because only an empty listing reads as absence:
// `git cat-file -e` exits 128 both for a path the tree lacks and for a tree it
// cannot read, and a failure read as "absent from the base" renders a
// modification as a whole-file addition. The index is no tree to walk, so the
// staged side lists the index itself.
func treeHasPath(ctx context.Context, root string, kind Kind, rev, path string) (bool, error) {
	switch {
	case kind == Git && rev == gitIndexRev:
		records, err := GitTreeRecords(ctx, GitArgs{Dir: root, Sub: []string{"ls-files", "--stage"}, Paths: []string{path}})
		if err != nil {
			return false, fmt.Errorf("list %s in the index: %w", path, err)
		}
		return hasStageZero(records), nil
	case kind == Git:
		records, err := GitTreeRecords(ctx, GitArgs{
			Dir:   root,
			Sub:   []string{"ls-tree", "--full-tree"},
			Revs:  []GitRef{UnsafeRef(rev)},
			Paths: []string{path},
		})
		if err != nil {
			return false, fmt.Errorf("list %s at %s: %w", path, rev, err)
		}
		return len(records) > 0, nil
	default:
		out, err := render.RunCLIDir(ctx, root, "jj", []string{"--ignore-working-copy", "file", "list", "-r", rev, "--", JJRootPattern(path)})
		if err != nil {
			return false, fmt.Errorf("list %s at %s: %w", path, rev, err)
		}
		return out != "", nil
	}
}

// hasStageZero reports whether a `git ls-files --stage` listing carries a stage-0
// entry; a conflicted path is listed at stages 1-3 and has no staged blob to read.
// Each record's attribute field is "<mode> <object> <stage>".
func hasStageZero(records []TreeRecord) bool {
	for _, rec := range records {
		if strings.HasSuffix(rec.Attrs, " 0") {
			return true
		}
	}
	return false
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
		out, err := render.RunCLIDir(ctx, root, "jj", []string{"diff", "--git", "-r", revset, "--", JJRootPattern(path)})
		if err != nil {
			return "", fmt.Errorf("jj diff --git -r %q: %w", revset, err)
		}
		return out, nil
	}
}

// gitMergeBase resolves the merge base of two revisions for a symmetric (A...B)
// range's before side. --end-of-options keeps an endpoint spelled like an option
// from reaching merge-base's own flag surface.
func gitMergeBase(ctx context.Context, root string, a, b GitRef) (string, error) {
	out, err := render.RunCLIDir(ctx, root, "git", []string{"merge-base", "--end-of-options", string(a), string(b)})
	if err != nil {
		return "", fmt.Errorf("merge-base %s %s: %w", a, b, err)
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return "", fmt.Errorf("merge-base %s %s: empty", a, b)
	}
	return id, nil
}

// diffRoot resolves dir's repository root so every child process runs there and
// path names stay root-relative across enumeration and blob reads. git offers no
// NUL-framed form of --show-toplevel, so a root whose own name ends in
// whitespace comes back trimmed.
func diffRoot(ctx context.Context, dir string, kind Kind) (string, error) {
	var argv []string
	switch kind {
	case Git:
		argv = []string{"git", "rev-parse", "--show-toplevel"}
	default:
		argv = []string{"jj", "--ignore-working-copy", "workspace", "root"}
	}
	out, err := render.RunCLIDir(ctx, dir, argv[0], argv[1:])
	if err != nil {
		return "", fmt.Errorf("resolve repo root for %q: %w", dir, err)
	}
	root := strings.TrimSpace(out)
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

// jjLines runs jj at root and splits its stdout into non-empty,
// whitespace-trimmed lines. Only jj comes through here: git's own listings are
// NUL-framed by the shape helpers, so no git path in this package is ever split
// on a newline it might itself contain.
func jjLines(ctx context.Context, root string, args ...string) ([]string, error) {
	out, err := render.RunCLIDir(ctx, root, "jj", args)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}
