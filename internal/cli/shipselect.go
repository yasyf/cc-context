package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/hunk"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// selectMode is the hunk-selection sense: skip commits every hunk except the
// named ones; only commits nothing but the named ones.
type selectMode int

const (
	selectSkip selectMode = iota
	selectOnly
)

// String renders the mode as it appears in a selection plan file.
func (m selectMode) String() string {
	if m == selectOnly {
		return "only"
	}
	return "skip"
}

// shipSelection is the parsed, path-validated hunk selection for a ship: the
// repo root, one mode across the whole ship, and the refs grouped by the
// root-relative file they address. It carries no file content —
// resolveShipSelection reads and resolves against fresh hunks before any commit
// runs, and the jj diff tool re-resolves in-tree.
type shipSelection struct {
	root  string
	mode  selectMode
	files map[string][]anchor.Ref

	// preflight fingerprints each hunk-scoped file by how many times each content
	// digest was present at listing time, so a commit-time snapshot carrying a
	// foreign hunk — a digest absent here, or one appearing more times than it did
	// here — is refused in skip mode instead of silently swept in.
	preflight map[string]map[string]int
}

// parseShipSelection validates the hunk flags and their refs, returning nil when
// neither --skip-hunk nor --only-hunk is set. The mutually-exclusive and
// malformed-ref guards need no repository read and fire first; resolving the repo
// root (so ref and ship paths normalize to the same frame) is the first VCS call,
// after which an out-of-scope ref is refused.
func parseShipSelection(ctx context.Context, kind vcs.Kind, o shipOpts) (*shipSelection, error) {
	if len(o.skipHunks) > 0 && len(o.onlyHunks) > 0 {
		return nil, errors.New("ship: --skip-hunk and --only-hunk cannot be combined")
	}
	var (
		raws []string
		mode selectMode
	)
	switch {
	case len(o.skipHunks) > 0:
		raws, mode = o.skipHunks, selectSkip
	case len(o.onlyHunks) > 0:
		raws, mode = o.onlyHunks, selectOnly
	default:
		return nil, nil
	}

	type parsed struct {
		path string
		ref  anchor.Ref
	}
	refs := make([]parsed, 0, len(raws))
	for _, raw := range raws {
		path, ref, err := hunk.ParseRef(raw)
		if err != nil {
			return nil, fmt.Errorf("ship: invalid hunk ref %q (expected file:A-B#hash, from ccx vcs hunks): %w", raw, err)
		}
		refs = append(refs, parsed{path: cleanRel(path), ref: ref})
	}

	root, err := repoRoot(ctx, kind)
	if err != nil {
		return nil, fmt.Errorf("ship: %w", err)
	}
	files := make(map[string][]anchor.Ref)
	for i, p := range refs {
		if !pathWithinShip(root, p.path, o.paths) {
			return nil, fmt.Errorf("ship: hunk ref %s is outside the shipped paths", raws[i])
		}
		files[p.path] = append(files[p.path], p.ref)
	}
	return &shipSelection{root: root, mode: mode, files: files}, nil
}

// pathWithinShip reports whether the root-relative refPath is covered by the
// shipped paths: an empty path set is a whole-repo ship (everything covered),
// otherwise refPath must equal a shipped path or sit beneath a shipped directory,
// both normalized to the root-relative frame (ship paths are typed cwd-relative).
func pathWithinShip(root, refPath string, shipPaths []string) bool {
	if len(shipPaths) == 0 {
		return true
	}
	rp := cleanRel(refPath)
	for _, p := range shipPaths {
		sp, err := rootRel(root, p)
		if err != nil {
			continue
		}
		if sp == "." || rp == sp {
			return true
		}
		if strings.HasPrefix(rp, sp+"/") {
			return true
		}
	}
	return false
}

// repoRoot returns the absolute working-copy root, the frame git blobs and jj
// trees address files by; every selection path normalizes to it so a ship from a
// subdirectory addresses the same file the VCS stores.
func repoRoot(ctx context.Context, kind vcs.Kind) (string, error) {
	switch kind {
	case vcs.Git:
		out, err := render.RunCLI(ctx, "git", []string{"rev-parse", "--show-toplevel"})
		if err != nil {
			return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
		}
		return strings.TrimSpace(out), nil
	case vcs.JJ:
		out, err := render.RunCLI(ctx, "jj", []string{"root"})
		if err != nil {
			return "", fmt.Errorf("jj root: %w", err)
		}
		return strings.TrimSpace(out), nil
	default:
		return "", errors.New("repo root: unsupported vcs")
	}
}

// rootRel converts a path as typed — cwd-relative or absolute — to a
// slash-separated path relative to root. Both sides resolve their symlinks first
// so a /var vs /private/var frame split (macOS, where the VCS reports the physical
// root but os.Getwd reports the logical cwd) does not yield a ../.. path.
func rootRel(root, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	rel, err := filepath.Rel(evalSymlinks(root), evalSymlinks(abs))
	if err != nil {
		return "", fmt.Errorf("resolve %s against repo root: %w", path, err)
	}
	return filepath.ToSlash(rel), nil
}

// evalSymlinks resolves p's symlinks, falling back to its resolved parent joined
// with the final element when p itself does not exist (a deleted file), and to p
// unchanged when even that fails — enough to put p in the same frame as a sibling.
func evalSymlinks(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(dir, filepath.Base(p))
	}
	return p
}

// cleanRel normalizes an already-root-relative path (a hunk ref's file, emitted
// root-relative by ccx vcs hunks) to slash form without re-anchoring it to cwd.
func cleanRel(path string) string {
	return filepath.ToSlash(filepath.Clean(path))
}

// resolveShipSelection recomputes each hunk-scoped file's hunks from its
// committed base and working content and resolves every ref, surfacing drift or
// an all-excluded file before any commit side effect. The jj lane re-runs the
// identical resolution inside its diff tool against jj's own snapshot; this pass
// is the pre-flight half of that double guard.
func resolveShipSelection(ctx context.Context, kind vcs.Kind, sel *shipSelection) error {
	sel.preflight = make(map[string]map[string]int, len(sel.files))
	for path, refs := range sel.files {
		base, err := showFileBase(ctx, kind, path)
		if err != nil {
			return err
		}
		current, err := os.ReadFile(filepath.Join(sel.root, path)) //nolint:gosec // the path is the caller's own ship target under the repo root, not untrusted input
		if err != nil {
			return fmt.Errorf("ship: read %s: %w", path, err)
		}
		hunks, _, err := resolveFileKeep(path, base, current, refs, sel.mode)
		if err != nil {
			return fmt.Errorf("ship: %w", err)
		}
		sel.preflight[path] = hunkDigestCounts(hunks)
	}
	return nil
}

// hunkDigestCounts tallies how many times each content digest occurs across hunks —
// the pre-flight fingerprint refuseForeignHunks checks a later snapshot against. It
// counts rather than sets membership so an identical change appearing twice at
// commit time but once at listing time is still caught as one foreign hunk.
func hunkDigestCounts(hunks []hunk.Hunk) map[string]int {
	counts := make(map[string]int, len(hunks))
	for i := range hunks {
		counts[hunks[i].Digest.String()]++
	}
	return counts
}

// refuseForeignHunks refuses a skip-mode commit when the snapshot carries a hunk
// absent at listing time — a foreign change written to the file between
// `ccx vcs hunks` and the commit that skip mode ("everything except the named
// hunks") would otherwise sweep in silently. A digest is foreign once the snapshot
// carries it more times than the pre-flight fingerprint did, so a duplicated
// identical change is caught, not masked by the original's digest. Only mode is
// never guarded: its foreign hunks stay uncommitted by construction, which is the
// whole point of committing around a concurrent session's work. Both commit lanes
// call this after resolveFileKeep recomputes the snapshot's hunks.
func refuseForeignHunks(path string, mode selectMode, hunks []hunk.Hunk, known map[string]int) error {
	if mode != selectSkip {
		return nil
	}
	seen := make(map[string]int, len(hunks))
	var foreign []string
	for i := range hunks {
		digest := hunks[i].Digest.String()
		seen[digest]++
		if seen[digest] > known[digest] {
			foreign = append(foreign, hunkListRef(path, hunks[i]))
		}
	}
	if len(foreign) == 0 {
		return nil
	}
	return fmt.Errorf("foreign hunk(s) appeared in %s since listing: %s; --skip-hunk would sweep them in — re-run: ccx vcs hunks %s", path, strings.Join(foreign, ", "), path)
}

// showFileBase returns the root-relative path's committed base image — git's HEAD
// blob or jj's @- revision — for hunk computation. A file absent from the parent
// commit (a newly added file) has an empty base: the whole working file becomes
// one insertion hunk. fileInBase distinguishes that legitimate absence from a
// genuine VCS failure (an unreadable base tree), which propagates rather than
// masquerading as an empty base.
func showFileBase(ctx context.Context, kind vcs.Kind, path string) ([]byte, error) {
	present, err := fileInBase(ctx, kind, path)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	argv := vcs.ShowFileArgv(kind, path)
	out, err := render.RunCLI(ctx, argv[0], argv[1:])
	if err != nil {
		return nil, fmt.Errorf("read base %s: %w", path, err)
	}
	return []byte(out), nil
}

// fileInBase reports whether the root-relative path exists in the committed base.
// It leans on commands that resolve the base tree from any working directory and
// report absence as empty output, not an error: git ls-tree --full-tree and jj
// file list. An error resolving the tree itself propagates.
func fileInBase(ctx context.Context, kind vcs.Kind, path string) (bool, error) {
	switch kind {
	case vcs.Git:
		out, err := render.RunCLI(ctx, "git", []string{"ls-tree", "--full-tree", "HEAD", "--", path})
		if err != nil {
			return false, fmt.Errorf("read base tree %s: %w", path, err)
		}
		return strings.TrimSpace(out) != "", nil
	case vcs.JJ:
		out, err := render.RunCLI(ctx, "jj", []string{"file", "list", "-r", "@-", "--", fmt.Sprintf("root:%q", path)})
		if err != nil {
			return false, fmt.Errorf("read base tree %s: %w", path, err)
		}
		return strings.TrimSpace(out) != "", nil
	default:
		return false, errors.New("file in base: unsupported vcs")
	}
}

// resolveFileKeep computes the hunks between base and current, matches every ref
// to a hunk, and returns the hunks plus the keep predicate for mode (the set of
// hunks that will be committed). It errors on a ref that drifted out of the
// diff, a digest whose nearest match ties, or a keep set that would commit
// nothing (which must never cut an empty commit).
func resolveFileKeep(path string, base, current []byte, refs []anchor.Ref, mode selectMode) ([]hunk.Hunk, func(int) bool, error) {
	hunks := hunk.Compute(base, current)
	matched := make(map[int]bool, len(refs))
	for _, ref := range refs {
		idx, err := matchHunkRef(path, hunks, ref)
		if err != nil {
			return nil, nil, err
		}
		matched[idx] = true
	}

	keep := make([]bool, len(hunks))
	kept := 0
	for i := range hunks {
		if mode == selectOnly {
			keep[i] = matched[i]
		} else {
			keep[i] = !matched[i]
		}
		if keep[i] {
			kept++
		}
	}
	if kept == 0 {
		return nil, nil, fmt.Errorf("all changes excluded in %s; drop the file from the ship instead", path)
	}
	return hunks, func(i int) bool { return keep[i] }, nil
}

// matchHunkRef finds the hunk carrying ref's content digest. A unique digest
// identifies its hunk by content alone. A duplicated digest (an identical change
// appearing more than once) is disambiguated by an exact post-image start match —
// a fresh ref anchors at distance 0 — and anything else is drift; the nearest
// non-exact match is never guessed. A digest present in no hunk is drift too.
func matchHunkRef(path string, hunks []hunk.Hunk, ref anchor.Ref) (int, error) {
	var candidates []int
	for i := range hunks {
		if hunks[i].Digest == ref.Hash {
			candidates = append(candidates, i)
		}
	}
	refStr := formatHunkRef(path, ref)
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	for _, i := range candidates {
		if hunks[i].NewStart == ref.Line {
			return i, nil
		}
	}
	return -1, fmt.Errorf("hunk %s not found — the diff changed since listing; re-run: ccx vcs hunks %s", refStr, path)
}

// formatHunkRef renders a "path:A-B#hash" (or "path:A#hash") reference for
// diagnostics, mirroring the form ccx vcs hunks prints.
func formatHunkRef(path string, ref anchor.Ref) string {
	if ref.End > ref.Line {
		return path + ":" + anchor.FormatRange(ref.Line, ref.End, ref.Hash)
	}
	return path + ":" + anchor.Format(ref.Line, ref.Hash)
}
