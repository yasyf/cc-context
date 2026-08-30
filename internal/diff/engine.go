package diff

import (
	"context"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/internal/astgrep"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/hunk"
	"github.com/yasyf/cc-context/internal/outline"
	ccrender "github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/sniff"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/workspace"
)

// Run resolves a.Source into a diff plan against the project root's VCS,
// classifies each changed file a.Globs select, and renders the structural diff,
// each file's section secret-masked in that file's path context unless
// a.RevealSecrets. The fired rule ids come back in file order — the caller
// appends the shared footer after its cap. Output is uncapped — the caller
// budget-caps it — and a.Full inlines per-file hunks.
func Run(ctx context.Context, a backend.Args) (string, []string, error) {
	cwd, err := workspace.RootFrom(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("diff: resolve cwd: %w", err)
	}
	plan, err := vcs.ResolveDiffPlan(ctx, ccrender.Dir(cwd), a.Source)
	if err != nil {
		return "", nil, err
	}
	files, err := globFilter(plan.Files, a.Globs)
	if err != nil {
		return "", nil, err
	}
	model, err := buildModel(ctx, plan, files)
	if err != nil {
		return "", nil, err
	}
	out, ids := render(model, a.Full, a.RevealSecrets)
	return out, ids, nil
}

// ChangedSymbols returns the sigil-tagged changed symbols across source's diff,
// narrowed to one repo-root-relative path — "+name" added, "~name" changed,
// "-name" removed, in file then position order. It powers the history summary,
// which passes a committed range and a single file path; a non-symbolic plan
// yields none.
func ChangedSymbols(ctx context.Context, dir ccrender.Dir, source, path string) ([]string, error) {
	plan, err := vcs.ResolveDiffPlan(ctx, dir, source)
	if err != nil {
		return nil, err
	}
	if !plan.Symbolic {
		return nil, nil
	}
	files := pathFilter(plan.Files, path)
	cache := outlineCache{}
	var syms []string
	for _, path := range files {
		lang, covered := outline.LangForExt(path)
		if !covered {
			continue
		}
		before, after, err := blobs(plan, path)
		if err != nil {
			return nil, err
		}
		fc, err := classifyBlobs(ctx, before, after, lang, cache)
		if err != nil {
			return nil, err
		}
		for _, s := range fc.symbols {
			syms = append(syms, changedSymbolSigil(s.kind)+s.name)
		}
	}
	return syms, nil
}

// buildModel assembles the render model: per file it fetches blobs, computes
// hunks, and either classifies (covered), emits raw hunks (uncovered), notes a
// binary, or — beyond the classification cap — records hunk counts. A
// non-symbolic plan renders each file's raw `jj diff --git` text.
func buildModel(ctx context.Context, plan vcs.DiffPlan, files []string) (diffModel, error) {
	m := diffModel{label: plan.Label}
	cache := outlineCache{}
	classified := 0
	for _, path := range files {
		if !plan.Symbolic {
			raw, err := plan.Raw(path)
			if err != nil {
				return diffModel{}, err
			}
			m.files = append(m.files, fileReport{path: path, kind: fileKindRawText, raw: raw})
			continue
		}

		renamedFrom := plan.Renames[path]

		before, after, err := blobs(plan, path)
		if err != nil {
			return diffModel{}, err
		}
		if blobBinary(before, after) {
			m.files = append(m.files, fileReport{path: path, renamedFrom: renamedFrom, kind: fileKindBinary})
			continue
		}
		hunks := hunk.Compute(before, after)
		if len(hunks) == 0 {
			// A rename with no content change still renders (with the "old → new"
			// note); a non-rename no-op is nothing to report.
			if renamedFrom != "" {
				m.files = append(m.files, fileReport{path: path, renamedFrom: renamedFrom, kind: fileKindRenamed})
			}
			continue
		}

		lang, covered := outline.LangForExt(path)
		switch {
		case !covered:
			m.files = append(m.files, fileReport{
				path: path, renamedFrom: renamedFrom, kind: fileKindRawHunks, hunks: hunks, before: before, ext: filepath.Ext(path),
			})
		case classified >= classifyCap:
			m.files = append(m.files, fileReport{path: path, renamedFrom: renamedFrom, kind: fileKindCapped, hunks: hunks})
		default:
			fc, err := classifyBlobs(ctx, before, after, lang, cache)
			if err != nil {
				return diffModel{}, err
			}
			classified++
			added, changed, removed := countKinds(fc.symbols)
			m.added, m.changed, m.removed = m.added+added, m.changed+changed, m.removed+removed
			m.files = append(m.files, fileReport{
				path: path, renamedFrom: renamedFrom, kind: fileKindSymbols, class: fc, hunks: hunks, before: before, after: after,
			})
		}
	}
	return m, nil
}

// blobs reads path's before and after images from the plan.
func blobs(plan vcs.DiffPlan, path string) (before, after []byte, err error) {
	before, err = plan.Before(path)
	if err != nil {
		return nil, nil, err
	}
	after, err = plan.After(path)
	if err != nil {
		return nil, nil, err
	}
	return before, after, nil
}

// classifyBlobs outlines both blobs (memoized) and classifies path's change.
func classifyBlobs(ctx context.Context, before, after []byte, lang string, cache outlineCache) (fileClass, error) {
	oldOutline, err := cache.outline(ctx, before, lang)
	if err != nil {
		return fileClass{}, err
	}
	newOutline, err := cache.outline(ctx, after, lang)
	if err != nil {
		return fileClass{}, err
	}
	return classify(oldOutline, newOutline, hunk.Compute(before, after)), nil
}

// blobBinary reports whether either image sniffs as binary.
func blobBinary(before, after []byte) bool {
	if _, bin := sniff.DetectBytes(after); bin {
		return true
	}
	_, bin := sniff.DetectBytes(before)
	return bin
}

// pathFilter keeps the repo-root-relative changed paths at or under path. It
// compares bytes rather than compiling a glob, so a filename carrying glob
// metacharacters selects itself, and only itself.
func pathFilter(files []string, path string) []string {
	var kept []string
	for _, f := range files {
		if f == path || strings.HasPrefix(f, path+"/") {
			kept = append(kept, f)
		}
	}
	return kept
}

// globFilter keeps the repo-root-relative changed paths globs select; an empty
// list keeps everything. It never stats, so a path deleted by the diff still
// matches the glob naming it.
func globFilter(files, globs []string) ([]string, error) {
	if len(globs) == 0 {
		return files, nil
	}
	var kept []string
	for _, f := range files {
		ok, err := backend.MatchGlobs(f, globs)
		if err != nil {
			return nil, fmt.Errorf("diff: %w", err)
		}
		if ok {
			kept = append(kept, f)
		}
	}
	return kept, nil
}

// changedSymbolSigil maps a change kind to the ASCII sigil the history summary
// consumes ("-" for removed, "+" added, "~" changed).
func changedSymbolSigil(k changeKind) string {
	switch k {
	case changeAdded:
		return "+"
	case changeRemoved:
		return "-"
	default:
		return "~"
	}
}

// outlineCache memoizes blob outlines for one Run, keyed by content hash and
// language, so a blob shared across files (or the before/after of an unchanged
// side) is outlined once. It never persists beyond the call.
type outlineCache map[string][]astgrep.OutlineFile

// outline outlines src as lang, returning cached files on a repeat. An empty src
// (a file's absent side) has no symbols and spawns no process.
func (c outlineCache) outline(ctx context.Context, src []byte, lang string) ([]astgrep.OutlineFile, error) {
	if len(src) == 0 {
		return nil, nil
	}
	h := fnv.New64a()
	_, _ = h.Write(src)
	key := fmt.Sprintf("%s:%x", lang, h.Sum64())
	if files, ok := c[key]; ok {
		return files, nil
	}
	files, err := astgrep.OutlineStdin(ctx, src, lang)
	if err != nil {
		return nil, err
	}
	c[key] = files
	return files, nil
}
