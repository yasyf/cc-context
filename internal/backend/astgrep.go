package backend

import (
	"context"
	"fmt"

	"github.com/yasyf/cc-context/internal/vendor"
)

// AstGrep translates the structural ops onto the ast-grep engine. ast-grep is a
// stateless single-pattern matcher driven by `ast-grep run`; both ops emit
// `--json=stream` so the facade reconstructs bounded output from JSON rather than
// ast-grep's colored text.
type AstGrep struct {
	// Bin is the resolved ast-grep binary path. Empty triggers resolution via
	// vendor.Resolve (configured bin → PATH → pinned download) on first use.
	Bin string
}

// Engine reports that AstGrep is backed by the ast-grep engine.
func (g AstGrep) Engine() Engine {
	return EngineAstGrep
}

func (g AstGrep) bin(ctx context.Context) (string, error) {
	return vendor.Resolve(ctx, vendor.AstGrep, g.Bin)
}

// CLIArgv translates op into an `ast-grep run` or `ast-grep outline` invocation.
// OpStructural searches for a.Query; OpReplace rewrites a.Pattern to a.Rewrite,
// writing in place only when a.Apply is set (-U); OpStructOutline outlines a.Path.
// Interactive mode is never used: it is TTY-only and dead in the MCP surface.
func (g AstGrep) CLIArgv(ctx context.Context, op Op, a Args) (bin string, argv []string, err error) {
	switch op {
	case OpStructural:
		argv = g.appendScope([]string{"run", "-p", a.Query, "--json=stream"}, a)
	case OpStructOutline:
		// --view expanded carries member signatures with line numbers (a class's
		// methods, a struct's fields) — the only view whose JSON includes members,
		// matching ccx's signatures+line-numbers contract; budget caps the size.
		// outline takes a single <path> positional, so it does not use the
		// run-shaped appendScope tail.
		argv = []string{"outline", a.Path, "--json=stream", "--view", "expanded"}
		if a.Items != "" {
			argv = append(argv, "--items", a.Items)
		}
		if a.Match != "" {
			argv = append(argv, "--match", a.Match)
		}
		if a.Lang != "" {
			argv = append(argv, "-l", a.Lang)
		}
	case OpReplace:
		argv = []string{"run", "-p", a.Pattern, "-r", a.Rewrite}
		// --json=stream and -U are mutually exclusive in ast-grep: with the stream
		// flag present, -U prints JSON and writes nothing. Preview parses the JSON;
		// apply must omit it so -U actually rewrites the files.
		if a.Apply {
			argv = append(argv, "-U")
		} else {
			argv = append(argv, "--json=stream")
		}
		argv = g.appendScope(argv, a)
	default:
		return "", nil, fmt.Errorf("ast-grep: unsupported op %q", op)
	}
	bin, err = g.bin(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("ast-grep: resolve binary: %w", err)
	}
	return bin, argv, nil
}

// appendScope appends the lang/glob/paths tail shared by both ops. Language is
// passed only when set (ast-grep infers it per file from the extension); the
// paths default to the repo root when none are given.
func (g AstGrep) appendScope(argv []string, a Args) []string {
	if a.Lang != "" {
		argv = append(argv, "-l", a.Lang)
	}
	if a.Glob != "" {
		argv = append(argv, "--globs", a.Glob)
	}
	if len(a.Paths) > 0 {
		argv = append(argv, a.Paths...)
	} else {
		argv = append(argv, ".")
	}
	return argv
}

// MCPSpec reports that ast-grep has no MCP server: it is driven only as a child
// process. The proxy routes the structural ops through the CLI path, so this is
// never called.
func (g AstGrep) MCPSpec(_ context.Context) (cmd string, argv []string, err error) {
	return "", nil, fmt.Errorf("ast-grep: no MCP server")
}

// MCPTool reports that ast-grep exposes no MCP tools; its ops run over the CLI.
func (g AstGrep) MCPTool(op Op, _ Args) (tool string, params map[string]any, err error) {
	return "", nil, fmt.Errorf("ast-grep: op %q has no MCP tool", op)
}
