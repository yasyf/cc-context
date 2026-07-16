package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yasyf/cc-context/internal/hunk"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// shipCommitGitSelect commits a hunk selection through a throwaway temp index, so
// neither the real index nor the worktree is rewritten: it seeds the temp index
// from HEAD, stages the fully-shipped paths whole, replaces each hunk-scoped file
// with a blob of only its selected hunks, commits the temp-index tree with NO
// pathspec (a pathspec would commit worktree state and smuggle the excluded hunks
// back in), then resyncs the real index for the shipped paths to the new HEAD.
// resolveShipSelection has already validated every ref against a fresh diff, so a
// drift or all-excluded refusal fires before any of this runs.
func shipCommitGitSelect(ctx context.Context, o shipOpts, sel *shipSelection) error {
	idxFile, err := os.CreateTemp("", "ccx-ship-index-*")
	if err != nil {
		return fmt.Errorf("ship: create temp index: %w", err)
	}
	idxPath := idxFile.Name()
	_ = idxFile.Close()
	defer func() { _ = os.Remove(idxPath) }()
	env := []string{"GIT_INDEX_FILE=" + idxPath}

	if _, err := render.RunCLIEnv(ctx, "git", []string{"read-tree", "HEAD"}, env); err != nil {
		return fmt.Errorf("ship: git read-tree: %w", err)
	}
	if addArgv, ok := gitSelectAddArgv(o.paths, sel); ok {
		if _, err := render.RunCLIEnv(ctx, "git", addArgv, env); err != nil {
			return fmt.Errorf("ship: git add: %w", err)
		}
	}
	for _, path := range sortedSelectionFiles(sel) {
		if err := gitStageSelected(ctx, path, sel, env); err != nil {
			return err
		}
	}
	if _, err := render.RunCLIEnv(ctx, "git", gitSelectCommitArgv(o), env); err != nil {
		return fmt.Errorf("ship: git commit: %w", err)
	}

	restoreArgv := append([]string{"restore", "--staged", "--"}, gitRestorePaths(o.paths)...)
	if _, err := render.RunCLI(ctx, "git", restoreArgv); err != nil {
		return fmt.Errorf("ship: git restore --staged: %w", err)
	}
	return nil
}

// gitStageSelected writes path's selected hunks as a blob and points the temp
// index at it. It re-reads the committed base (resolveShipSelection discarded its
// copy) and recomputes deterministically, so listing, pre-flight, and commit all
// agree on the same hunks.
func gitStageSelected(ctx context.Context, path string, sel *shipSelection, env []string) error {
	base, err := showFileBase(ctx, vcs.Git, path)
	if err != nil {
		return err
	}
	current, err := os.ReadFile(filepath.Join(sel.root, path))
	if err != nil {
		return fmt.Errorf("ship: read %s: %w", path, err)
	}
	hunks, keep, err := resolveFileKeep(path, base, current, sel.files[path], sel.mode)
	if err != nil {
		return fmt.Errorf("ship: %w", err)
	}
	selected := hunk.Select(base, hunks, keep)
	mode, err := gitFileMode(ctx, sel.root, path)
	if err != nil {
		return err
	}
	oid, err := render.RunCLIStdin(ctx, "git", []string{"hash-object", "-w", "--stdin"}, selected)
	if err != nil {
		return fmt.Errorf("ship: git hash-object %s: %w", path, err)
	}
	cacheinfo := fmt.Sprintf("%s,%s,%s", mode, strings.TrimSpace(oid), path)
	if _, err := render.RunCLIEnv(ctx, "git", []string{"update-index", "--add", "--cacheinfo", cacheinfo}, env); err != nil {
		return fmt.Errorf("ship: git update-index %s: %w", path, err)
	}
	return nil
}

// gitSelectAddArgv builds the git-add argv that stages the fully-shipped paths —
// those shipped whole, not hunk-scoped — into the temp index; ok is false when a
// scoped ship names no whole file, so no add runs. An empty ship-path set stages
// the whole worktree (git add -A), and the per-file update-index pass then
// overwrites each hunk-scoped file with its selected blob.
func gitSelectAddArgv(paths []string, sel *shipSelection) ([]string, bool) {
	if len(paths) == 0 {
		return []string{"add", "-A"}, true
	}
	var whole []string
	for _, p := range paths {
		if !selectionScoped(sel, p) {
			whole = append(whole, p)
		}
	}
	if len(whole) == 0 {
		return nil, false
	}
	return append([]string{"add", "-A", "--"}, whole...), true
}

// selectionScoped reports whether the ship path p (typed cwd-relative) is one of
// the hunk-scoped files, normalizing p to the selection's root-relative frame.
func selectionScoped(sel *shipSelection, p string) bool {
	rel, err := rootRel(sel.root, p)
	if err != nil {
		return false
	}
	_, ok := sel.files[rel]
	return ok
}

// gitSelectCommitArgv builds the temp-index commit argv, carrying NO pathspec so
// git commits the index tree and not worktree state.
func gitSelectCommitArgv(o shipOpts) []string {
	switch {
	case o.amend && o.message != "":
		return []string{"commit", "--amend", "-m", o.message}
	case o.amend:
		return []string{"commit", "--amend", "--no-edit"}
	default:
		return []string{"commit", "-m", o.message}
	}
}

// gitRestorePaths returns the pathspec that resyncs the real index to the new
// HEAD; an empty ship-path set (whole-repo ship) syncs the whole index (":/").
func gitRestorePaths(paths []string) []string {
	if len(paths) == 0 {
		return []string{":/"}
	}
	return paths
}

// gitFileMode reports the root-relative path's mode ("100644"/"100755") from
// HEAD's tree (--full-tree resolves the root-relative path from any working
// directory), falling back to the worktree exec bit for a file absent from HEAD —
// a newly added file. The temp index is seeded from HEAD, so HEAD's mode is the
// mode the staged blob inherits.
func gitFileMode(ctx context.Context, root, path string) (string, error) {
	out, err := render.RunCLI(ctx, "git", []string{"ls-tree", "--full-tree", "HEAD", "--", path})
	if err != nil {
		return "", fmt.Errorf("ship: git ls-tree %s: %w", path, err)
	}
	if mode := firstField(out); mode != "" {
		return mode, nil
	}
	info, err := os.Stat(filepath.Join(root, path))
	if err != nil {
		return "", fmt.Errorf("ship: stat %s: %w", path, err)
	}
	if info.Mode().Perm()&0o100 != 0 {
		return "100755", nil
	}
	return "100644", nil
}

// sortedSelectionFiles returns the hunk-scoped file paths in a stable order so a
// multi-file selection stages deterministically.
func sortedSelectionFiles(sel *shipSelection) []string {
	files := make([]string, 0, len(sel.files))
	for path := range sel.files {
		files = append(files, path)
	}
	sort.Strings(files)
	return files
}

// firstField returns the first whitespace-delimited token of s, or "" when s is
// blank.
func firstField(s string) string {
	if fields := strings.Fields(s); len(fields) > 0 {
		return fields[0]
	}
	return ""
}
