package backend

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vendor"
)

// Tilth translates ops onto the tilth engine. tilth is query-dispatched: the
// first positional selects the mode (a path, a subcommand, or a search string).
type Tilth struct {
	// Bin is the resolved tilth binary path. Empty triggers resolution via
	// vendor.Resolve (configured bin → PATH → pinned download) on first use.
	Bin string
}

// Engine reports that Tilth is backed by the tilth engine.
func (t Tilth) Engine() Engine {
	return EngineTilth
}

func (t Tilth) bin(ctx context.Context) (string, error) {
	return vendor.Resolve(ctx, vendor.Tilth, t.Bin)
}

// CLIArgv translates op into a tilth child-process invocation.
func (t Tilth) CLIArgv(ctx context.Context, op Op, a Args) (bin string, argv []string, err error) {
	if op == OpDiff {
		return t.diffArgv(ctx, a)
	}
	switch op {
	case OpOutline:
		argv = []string{a.Path}
		if a.Budget > 0 {
			argv = append(argv, "--budget", strconv.Itoa(a.Budget))
		}
	case OpRead:
		argv = []string{a.Path}
		switch {
		case a.Full:
			argv = append(argv, "--full")
		case a.Section != "":
			argv = append(argv, "--section", a.Section)
		}
		if a.Budget > 0 {
			argv = append(argv, "--budget", strconv.Itoa(a.Budget))
		}
	case OpSymbol:
		argv = []string{"grok", a.Query}
		if a.Scope != "" {
			argv = append(argv, "--scope", a.Scope)
		}
		if a.Full {
			argv = append(argv, "--full")
		}
	case OpDeps:
		argv = []string{a.Path, "--deps"}
		if a.Scope != "" {
			argv = append(argv, "--scope", a.Scope)
		}
		if a.Budget > 0 {
			argv = append(argv, "--budget", strconv.Itoa(a.Budget))
		}
	case OpGrep:
		argv = []string{a.Query}
		if a.Glob != "" {
			argv = append(argv, "--glob", a.Glob)
		}
		if a.Scope != "" {
			argv = append(argv, "--scope", a.Scope)
		}
		if a.Budget > 0 {
			argv = append(argv, "--budget", strconv.Itoa(a.Budget))
		}
		if a.Expand > 0 {
			argv = append(argv, "--expand="+strconv.Itoa(a.Expand))
		}
	case OpFind:
		argv = []string{a.Glob}
	case OpOverview:
		argv = []string{"overview"}
	default:
		return "", nil, fmt.Errorf("tilth: unsupported op %q", op)
	}
	bin, err = t.bin(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("tilth: resolve binary: %w", err)
	}
	return bin, argv, nil
}

// diffArgv resolves the diff source against the working-copy VCS. When the
// source translates to a git ref tilth can read, it returns a tilth invocation;
// otherwise it returns the jj fallback argv unchanged.
func (t Tilth) diffArgv(ctx context.Context, a Args) (bin string, argv []string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil, fmt.Errorf("tilth: resolve cwd: %w", err)
	}
	translated, useTilth, fallbackArgv, err := vcs.ResolveDiffSource(ctx, cwd, a.Source, a.Scope)
	if err != nil {
		return "", nil, fmt.Errorf("tilth: resolve diff source: %w", err)
	}
	if !useTilth {
		return fallbackArgv[0], fallbackArgv[1:], nil
	}
	bin, err = t.bin(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("tilth: resolve binary: %w", err)
	}
	return bin, tilthDiffArgv(translated, a.Scope, a.Budget), nil
}

// tilthDiffArgv assembles the `tilth diff` argv. An empty translated source is
// omitted entirely — tilth reads an empty positional as a bogus ref and reports
// no changes, so a working-tree diff must be the bare `diff` subcommand.
func tilthDiffArgv(translated, scope string, budget int) []string {
	argv := []string{"diff"}
	if translated != "" {
		argv = append(argv, translated)
	}
	if scope != "" {
		argv = append(argv, "--scope", scope)
	}
	if budget > 0 {
		argv = append(argv, "--budget", strconv.Itoa(budget))
	}
	return argv
}

// MCPSpec returns the command that launches tilth's MCP server over stdio,
// provisioning the pinned binary if needed. A resolution failure propagates
// rather than falling back to a bare PATH "tilth".
func (t Tilth) MCPSpec(ctx context.Context) (cmd string, argv []string, err error) {
	bin, err := t.bin(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("tilth: resolve binary: %w", err)
	}
	// --no-overview skips tilth's init project-fingerprint scan: the facade
	// re-exposes overview as its own op, so the auto-injection is wasted work
	// (and a per-call cost for one-shot CLI outline reads).
	return bin, []string{"--mcp", "--no-overview"}, nil
}

// MCPTool translates op into a tilth MCP tool name and its params.
func (t Tilth) MCPTool(op Op, a Args) (tool string, params map[string]any, err error) {
	switch op {
	case OpOutline:
		// signature mode = hash-prefixed declarations only (bodies elided), the
		// real token-saving outline. tilth's default "auto" mode returns full or
		// near-full content; only signature reliably elides.
		return "tilth_read", omitEmpty(map[string]any{
			"path":   a.Path,
			"mode":   "signature",
			"budget": a.Budget,
		}), nil
	case OpRead:
		p := map[string]any{"path": a.Path}
		switch {
		case a.Full:
			p["full"] = true
		case a.Section != "":
			p["section"] = a.Section
		}
		if a.Budget > 0 {
			p["budget"] = a.Budget
		}
		return "tilth_read", p, nil
	case OpSymbol:
		return "tilth_grok", omitEmpty(map[string]any{
			"target": a.Query,
			"scope":  a.Scope,
			"full":   a.Full,
		}), nil
	case OpDeps:
		return "tilth_deps", omitEmpty(map[string]any{
			"path":   a.Path,
			"scope":  a.Scope,
			"budget": a.Budget,
		}), nil
	case OpGrep:
		return "tilth_search", omitEmpty(map[string]any{
			"query":  a.Query,
			"glob":   a.Glob,
			"scope":  a.Scope,
			"kind":   a.Kind,
			"budget": a.Budget,
			"expand": a.Expand,
		}), nil
	case OpFind:
		return "tilth_files", omitEmpty(map[string]any{
			"pattern": a.Glob,
			"scope":   a.Scope,
		}), nil
	case OpDiff:
		return "tilth_diff", omitEmpty(map[string]any{
			"source": a.Source,
			"scope":  a.Scope,
			"budget": a.Budget,
		}), nil
	case OpOverview:
		return "", nil, fmt.Errorf("tilth: op %q has no MCP tool", op)
	default:
		return "", nil, fmt.Errorf("tilth: unsupported op %q", op)
	}
}

// omitEmpty drops zero-valued entries so the params map carries only the fields
// the caller actually set: empty strings, false bools, and zero ints fall out.
func omitEmpty(params map[string]any) map[string]any {
	for k, v := range params {
		switch val := v.(type) {
		case string:
			if val == "" {
				delete(params, k)
			}
		case bool:
			if !val {
				delete(params, k)
			}
		case int:
			if val == 0 {
				delete(params, k)
			}
		}
	}
	return params
}
