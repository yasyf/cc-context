package astgrep

import (
	"context"
	"fmt"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/outline"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/workspace"
)

// applyFileCap bounds how many distinct files a single `replace --apply` may
// rewrite without --force. It guards against a too-broad pattern silently
// mutating the tree.
const applyFileCap = 20

// astGrepExitNoMatch is the exit code ast-grep `run` returns for a clean
// no-match; it is tolerated and distinguished from a real failure by empty stdout.
const astGrepExitNoMatch = 1

// searchDir is the project root every ast-grep child runs in and every path it
// prints is relative to; one resolution feeds both so they cannot drift.
func searchDir(ctx context.Context) (render.Dir, error) {
	root, err := workspace.RootFrom(ctx)
	if err != nil {
		return "", fmt.Errorf("astgrep: resolve cwd: %w", err)
	}
	return render.Dir(root), nil
}

// Run executes an ast-grep op (OpStructural, OpReplace, or OpStructOutline) end
// to end — argv translation, child process, JSON parse, render, and budget cap —
// and returns the bounded output. It is the single orchestration shared by the
// CLI and the MCP proxy so the two surfaces behave identically.
func Run(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
	dir, err := searchDir(ctx)
	if err != nil {
		return "", err
	}
	switch op {
	case backend.OpStructural:
		return runStructural(ctx, dir, a)
	case backend.OpReplace:
		return runReplace(ctx, dir, a)
	case backend.OpStructOutline:
		return runStructOutline(ctx, dir, a)
	default:
		return "", fmt.Errorf("astgrep: unsupported op %q", op)
	}
}

// runStructural renders the search match list for a.Query.
func runStructural(ctx context.Context, dir render.Dir, a backend.Args) (string, error) {
	matches, err := matchesFor(ctx, dir, backend.OpStructural, a)
	if err != nil {
		return "", err
	}
	return render.Cap(RenderSearch(matches), a.Budget), nil
}

// runStructOutline renders the structural outline of a.Path (file or directory).
func runStructOutline(ctx context.Context, dir render.Dir, a backend.Args) (string, error) {
	out, err := runArgv(ctx, dir, backend.OpStructOutline, a)
	if err != nil {
		return "", err
	}
	files, err := ParseOutline([]byte(out))
	if err != nil {
		return "", err
	}
	if a.Section != "" {
		start, end, err := outline.ValidateSection(a, backend.OpStructOutline)
		if err != nil {
			return "", err
		}
		files = WindowOutline(files, start, end)
	}
	out, ids := RenderOutline(files, anchor.NewFiles(string(dir)), DepthFor(a), a.RevealSecrets)
	return render.WithSecretsFooter(render.Cap(out, a.Budget), ids), nil
}

// OutlineStdin outlines src fed on stdin as language lang, returning the parsed
// outline files. It drives `ast-grep outline --stdin -l <lang> --json=stream
// --view expanded` — the same view runStructOutline uses — so a VCS blob is
// outlined without staging it to disk. An empty or symbol-less src yields one
// file with no items; outline exits 0 even on no match, so no exit is tolerated.
func OutlineStdin(ctx context.Context, src []byte, lang string) ([]OutlineFile, error) {
	bin, err := resolveBin(ctx, "")
	if err != nil {
		return nil, err
	}
	dir, err := searchDir(ctx)
	if err != nil {
		return nil, err
	}
	argv := []string{"outline", "--stdin", "-l", lang, "--json=stream", "--view", "expanded"}
	out, err := render.RunCLIStdin(ctx, dir, bin, argv, src)
	if err != nil {
		return nil, err
	}
	return ParseOutline([]byte(out))
}

// runReplace previews a.Pattern→a.Rewrite, or applies it when a.Apply is set. An
// apply first counts the distinct files the preview would touch and refuses to
// write more than applyFileCap of them unless a.Force is set.
func runReplace(ctx context.Context, dir render.Dir, a backend.Args) (string, error) {
	preview := a
	preview.Apply = false
	matches, err := matchesFor(ctx, dir, backend.OpReplace, preview)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return fmt.Sprintf("# no matches for %s\nhint: confirm the pattern parsed with `ast-grep run -p '%s' --debug-query=ast`\n", a.Pattern, a.Pattern), nil
	}

	if !a.Apply {
		return render.Cap(RenderPreview(matches), a.Budget), nil
	}

	files := DistinctFiles(matches)
	if files > applyFileCap && !a.Force {
		return "", fmt.Errorf("replace would modify %d files, exceeding the cap of %d; re-run with --force", files, applyFileCap)
	}

	if _, err := runArgv(ctx, dir, backend.OpReplace, a); err != nil {
		return "", err
	}
	return render.Cap(fmt.Sprintf("# applied %d rewrites across %d files\n", len(matches), files), a.Budget), nil
}

// matchesFor runs op and parses its --json=stream output into matches. A clean
// no-match (tolerated exit, empty stdout) parses to zero matches.
func matchesFor(ctx context.Context, dir render.Dir, op backend.Op, a backend.Args) ([]Match, error) {
	out, err := runArgv(ctx, dir, op, a)
	if err != nil {
		return nil, err
	}
	return Parse([]byte(out))
}

// runArgv translates op into the ast-grep argv, resolves the binary on PATH
// (enforcing the version floor), and runs it in dir, tolerating the no-match exit
// so an empty result is not mistaken for a failure.
func runArgv(ctx context.Context, dir render.Dir, op backend.Op, a backend.Args) (string, error) {
	argv, err := argvFor(op, a)
	if err != nil {
		return "", err
	}
	resolved, err := resolveBin(ctx, "")
	if err != nil {
		return "", err
	}
	return render.RunCLIAllowExit(ctx, dir, resolved, argv, astGrepExitNoMatch)
}

// argvFor builds the `ast-grep run`/`outline` argv for op (the binary is
// resolved separately at the run chokepoint). OpStructural searches a.Query;
// OpReplace rewrites a.Pattern to a.Rewrite, writing in place only when a.Apply
// is set (-U); OpStructOutline outlines a.Path. Interactive mode is never used:
// TTY-only and dead in the MCP surface.
func argvFor(op backend.Op, a backend.Args) ([]string, error) {
	switch op {
	case backend.OpStructural:
		return appendScope([]string{"run", "-p", a.Query, "--json=stream"}, a)
	case backend.OpStructOutline:
		// --view expanded is the only view whose JSON carries members; outline takes
		// one <path> positional, so it skips the run-shaped appendScope tail.
		argv := []string{"outline", a.Path, "--json=stream", "--view", "expanded"}
		if a.Items != "" {
			argv = append(argv, "--items", a.Items)
		}
		if a.Match != "" {
			argv = append(argv, "--match", a.Match)
		}
		if a.Lang != "" {
			argv = append(argv, "-l", a.Lang)
		}
		return argv, nil
	case backend.OpReplace:
		argv := []string{"run", "-p", a.Pattern, "-r", a.Rewrite}
		// -U and --json=stream are mutually exclusive: with the stream flag, -U
		// prints JSON and writes nothing, so apply omits it to actually rewrite.
		if a.Apply {
			argv = append(argv, "-U")
		} else {
			argv = append(argv, "--json=stream")
		}
		return appendScope(argv, a)
	default:
		return nil, fmt.Errorf("ast-grep: unsupported op %q", op)
	}
}

// appendScope appends the lang/globs/paths tail shared by the run-shaped ops.
// Language is passed only when set (ast-grep infers it per file from the
// extension); the paths default to the repo root when none are given. --globs
// never filters an explicit file operand, so those are prefiltered in Go.
func appendScope(argv []string, a backend.Args) ([]string, error) {
	if a.Lang != "" {
		argv = append(argv, "-l", a.Lang)
	}
	for _, g := range a.Globs {
		argv = append(argv, "--globs", g)
	}
	if len(a.Paths) == 0 {
		return append(argv, "."), nil
	}
	paths := a.Paths
	if len(a.Globs) > 0 {
		kept, err := backend.FilterGlobPaths(a.Paths, a.Globs)
		if err != nil {
			return nil, err
		}
		paths = kept
	}
	return append(argv, paths...), nil
}
