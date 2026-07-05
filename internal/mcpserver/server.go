// Package mcpserver exposes the static ccx_* tool surface over stdio, proxying
// each tool to the bundled tilth and semble engines.
package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/codeexec"
	"github.com/yasyf/cc-context/internal/format"
	"github.com/yasyf/cc-context/internal/outline"
	"github.com/yasyf/cc-context/internal/proxy"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/search"
	"github.com/yasyf/cc-context/internal/version"
)

// defaultSnippetLines bounds ccx_code_search result snippets when the caller
// gives no explicit max — semble's own default returns the whole chunk.
const defaultSnippetLines = 10

// SearchIn is the input for ccx_code_search.
type SearchIn struct {
	Query           string `json:"query" jsonschema:"natural-language intent, structural pattern, or literal text"`
	Repo            string `json:"repo,omitempty" jsonschema:"repo root or https git URL to search; defaults to the project root"`
	Mode            string `json:"mode,omitempty" jsonschema:"routing override: auto|semantic|structural|literal (default auto)"`
	Lang            string `json:"lang,omitempty" jsonschema:"structural: language to parse as; inferred from extension when omitted"`
	K               int    `json:"k,omitempty" jsonschema:"max results to return"`
	MaxSnippetLines int    `json:"max_snippet_lines,omitempty" jsonschema:"max lines per result snippet"`
}

// ReplaceIn is the input for ccx_code_replace.
type ReplaceIn struct {
	Pattern string   `json:"pattern" jsonschema:"ast-grep structural pattern; metavars like $A and $$$ for variadic"`
	Rewrite string   `json:"rewrite" jsonschema:"replacement template; reference the same metavars"`
	Paths   []string `json:"paths,omitempty" jsonschema:"files or dirs to scope to; defaults to repo root"`
	Lang    string   `json:"lang,omitempty" jsonschema:"language; inferred from extension when omitted"`
	Glob    string   `json:"glob,omitempty" jsonschema:"gitignore-style include/exclude; ! to exclude"`
	Apply   bool     `json:"apply,omitempty" jsonschema:"WRITE the changes; omit/false returns a preview diff only"`
	Force   bool     `json:"force,omitempty" jsonschema:"bypass the apply file-count cap"`
	Budget  int      `json:"budget,omitempty" jsonschema:"token budget for the preview diff"`
}

// RelatedIn is the input for ccx_code_related.
type RelatedIn struct {
	Location string `json:"location" jsonschema:"a file:line location, or an anchor echoed from a producer tool (\"f.go:12#a3fk\"); a shifted anchor re-resolves by content and prepends a \"# anchor …\" note"`
}

// OutlineIn is the input for ccx_code_outline.
type OutlineIn struct {
	Path   string `json:"path" jsonschema:"file or directory to outline"`
	Items  string `json:"items,omitempty" jsonschema:"ast-grep: items to include (imports|exports|structure|all)"`
	Match  string `json:"match,omitempty" jsonschema:"ast-grep: keep only items whose name/signature matches this regex"`
	Lang   string `json:"lang,omitempty" jsonschema:"ast-grep: language to parse as; inferred from extension when omitted"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the outline"`
}

// ReadIn is the input for ccx_code_read.
type ReadIn struct {
	Path    string `json:"path" jsonschema:"file to read"`
	Section string `json:"section,omitempty" jsonschema:"a line range (\"40-95\"), a heading (\"## Heading\"), or an anchor echoed from a producer tool (\"15-27#k2fa\", or bare \"k2fa\"); a shifted anchor re-resolves by content and prepends a \"# anchor …\" note"`
	Full    bool   `json:"full,omitempty" jsonschema:"read the whole file"`
	Budget  int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// SymbolIn is the input for ccx_code_symbol.
type SymbolIn struct {
	Name  string `json:"name" jsonschema:"symbol to grok"`
	Scope string `json:"scope,omitempty" jsonschema:"directory to scope the lookup to"`
	Full  bool   `json:"full,omitempty" jsonschema:"include full bodies"`
}

// DepsIn is the input for ccx_code_deps.
type DepsIn struct {
	Path   string `json:"path" jsonschema:"file to analyze"`
	Scope  string `json:"scope,omitempty" jsonschema:"directory to scope the analysis to"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// GrepIn is the input for ccx_code_grep.
type GrepIn struct {
	Text   string `json:"text" jsonschema:"text to search for"`
	Glob   string `json:"glob,omitempty" jsonschema:"restrict to files matching this glob"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
	Expand int    `json:"expand,omitempty" jsonschema:"lines of context to expand around hits"`
}

// FindIn is the input for ccx_repo_find.
type FindIn struct {
	Glob  string `json:"glob" jsonschema:"glob to match files against"`
	Scope string `json:"scope,omitempty" jsonschema:"directory to scope the search to"`
}

// DiffIn is the input for ccx_vcs_diff.
type DiffIn struct {
	Source string `json:"source,omitempty" jsonschema:"diff source: uncommitted|staged|<ref> (default uncommitted)"`
	Scope  string `json:"scope,omitempty" jsonschema:"path to scope the diff to"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// OverviewIn is the input for ccx_repo_overview.
type OverviewIn struct{}

// ExecIn is the input for ccx_exec.
type ExecIn struct {
	Script string `json:"script" jsonschema:"Python in the monty subset; host functions are async — await them; end an entrypoint with asyncio.run(main()) or use a bare final expression"`
	Budget int    `json:"budget,omitempty" jsonschema:"max output tokens, 0 = default"`
}

// ExecToolsIn is the input for ccx_exec_tools.
type ExecToolsIn struct{}

// BashToonIn is the input for BashToon.
type BashToonIn struct {
	Command   []string `json:"command" jsonschema:"argv to RUN directly (no shell); argv[0] is the program, the rest its arguments"`
	Delimiter string   `json:"delimiter,omitempty" jsonschema:"array delimiter: comma|tab|pipe (default comma)"`
	Indent    int      `json:"indent,omitempty" jsonschema:"spaces per indentation level (default 2)"`
	ForceTOON bool     `json:"force_toon,omitempty" jsonschema:"always emit TOON, never compact JSON"`
	Budget    int      `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// Serve creates the proxy (engines connect lazily on first use) and the
// resident sandbox engine, registers the static ccx_* tools, and serves them
// over stdio until ctx is cancelled or the transport closes.
func Serve(ctx context.Context) error {
	p := proxy.New()
	defer func() { _ = p.Close() }()

	var eng *codeexec.Engine
	if codeexec.Supported {
		eng = codeexec.NewEngine(p, codeexec.NewMemoryStore())
		defer func() { _ = eng.Close() }()
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "cc-context", Version: version.String()}, &mcp.ServerOptions{
		Instructions: "Single question → the matching ccx_* tool. Pipeline, filter, fan-out, or output you would post-process → ccx_exec (get the catalog once via ccx_exec_tools).",
	})
	register(s, p, eng)

	return s.Run(ctx, &mcp.StdioTransport{})
}

// register wires every static tool to a handler that builds backend.Args and
// proxies the call to the matching engine.
func register(s *mcp.Server, p *proxy.Proxy, eng *codeexec.Engine) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_search",
		Description: "Code search routed by query kind — natural-language intent (semantic), structural pattern (ast-grep), or forced literal. Prefer over grep for \"where/how do we do X\". Results are anchored (path:12-40#a3fk) — echo into ccx_code_read section or ccx_code_related.",
	}, searchHandler(p))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_replace",
		Description: "Preview (default) or apply a structural find-replace WITHOUT reading the file into context. Pattern in, diff out.",
	}, handler(p, backend.OpReplace, func(in ReplaceIn) backend.Args {
		return backend.Args{
			Pattern: in.Pattern,
			Rewrite: in.Rewrite,
			Paths:   in.Paths,
			Lang:    in.Lang,
			Glob:    in.Glob,
			Apply:   in.Apply,
			Force:   in.Force,
			Budget:  in.Budget,
		}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_related",
		Description: "Find code semantically related to a file:line location — follow-up to a prior search hit. Takes an anchored location too (f.go:12#a3fk).",
	}, handler(p, backend.OpRelated, func(in RelatedIn) backend.Args {
		return backend.Args{Query: in.Location}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_outline",
		Description: "Compact outline of a file or directory (signatures + line numbers, budget-bounded) — prefer over reading whole files. Routes to ast-grep (supports items=imports|exports and match=<regex>) or tilth by language. Spans are anchored (L15#k2fa) — echo into ccx_code_read section.",
	}, outlineHandler(p))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_read",
		Description: "Read a file by section, heading, anchor, or whole — pass section to avoid pulling the entire file.",
	}, handler(p, backend.OpRead, func(in ReadIn) backend.Args {
		return backend.Args{Path: in.Path, Section: in.Section, Full: in.Full, Budget: in.Budget}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_symbol",
		Description: "Grok a symbol: definition, doc, callers, callees, siblings, tests — one call beats many greps. Locations are anchored (file:67#c4bk) — echo into ccx_code_read section or ccx_code_related.",
	}, handler(p, backend.OpSymbol, func(in SymbolIn) backend.Args {
		return backend.Args{Query: in.Name, Scope: in.Scope, Full: in.Full}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_deps",
		Description: "Dependencies of a file (imports and their resolved targets), budget-bounded.",
	}, handler(p, backend.OpDeps, func(in DepsIn) backend.Args {
		return backend.Args{Path: in.Path, Scope: in.Scope, Budget: in.Budget}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_grep",
		Description: "Literal text search across code, optionally globbed and budget-bounded — for exact strings. Frames are anchored (55-66#2eak) — echo into ccx_code_read section.",
	}, handler(p, backend.OpGrep, func(in GrepIn) backend.Args {
		return backend.Args{Query: in.Text, Glob: in.Glob, Budget: in.Budget, Expand: in.Expand}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_repo_find",
		Description: "List files matching a glob with per-file token counts — prefer over ls -R for enumeration.",
	}, handler(p, backend.OpFind, func(in FindIn) backend.Args {
		return backend.Args{Glob: in.Glob, Scope: in.Scope}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_vcs_diff",
		Description: "VCS-aware diff (uncommitted|staged|<ref>), budget-bounded — prefer over a raw git diff pager. Symbol rows are anchored (L11#b3dk) — echo into ccx_code_read section.",
	}, handler(p, backend.OpDiff, func(in DiffIn) backend.Args {
		source := in.Source
		if source == "" {
			source = "uncommitted"
		}
		return backend.Args{Source: source, Scope: in.Scope, Budget: in.Budget}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_repo_overview",
		Description: "Repository overview (structure and entry points) — orient here before diving into files.",
	}, handler(p, backend.OpOverview, func(OverviewIn) backend.Args {
		return backend.Args{}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name: "ccx_exec",
		Description: "RUN a Python script in a sandbox whose async host functions are every ccx op, a gated sh(cmd), " +
			"and every stateless MCP server's tools (auto-reflected) — the script awaits them, filters in-sandbox, " +
			"and ONLY its return value comes back. Reach for it over 2+ chained tool calls or any output you'd " +
			"post-process: project a large JSON, sweep files, fan out searches with asyncio.gather. Call " +
			"ccx_exec_tools once first for the catalog and subset rules.",
	}, execHandler(eng))

	mcp.AddTool(s, &mcp.Tool{
		Name: "ccx_exec_tools",
		Description: "List the host functions a ccx_exec script can call — ccx ops, sh, and the auto-reflected " +
			"tools of every stateless MCP server (mutating tools labeled, reflected servers noted) — plus the " +
			"allowed Python subset and a worked example. Call once per session; the catalog is cached until the " +
			"MCP inventory changes.",
	}, execToolsHandler(eng))

	mcp.AddTool(s, &mcp.Tool{
		Name: "BashToon",
		Description: "RUN a command (argv, no shell) and return its stdout token-compacted: JSON/NDJSON " +
			"is re-encoded as TOON (or compact JSON when smaller), other output passes through. Use for " +
			"commands that emit JSON (gh --json, kubectl -o json, …) so the raw JSON never enters context. " +
			"It executes the command — it is not a passive converter and does not take a JSON string.",
	}, bashToonHandler())
}

// searchHandler classifies the query through search.Route and dispatches the
// routed op, mirroring the CLI so both surfaces behave identically. The semantic
// path keeps semble's compact-snippet default; structural and literal ignore the
// snippet knobs.
func searchHandler(p *proxy.Proxy) func(context.Context, *mcp.CallToolRequest, SearchIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, any, error) {
		snippet := in.MaxSnippetLines
		if snippet == 0 {
			snippet = defaultSnippetLines
		}
		a := backend.Args{
			Query:           in.Query,
			Path:            in.Repo,
			Mode:            in.Mode,
			Lang:            in.Lang,
			K:               in.K,
			MaxSnippetLines: snippet,
		}
		op, _, err := search.Route(a)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", req.Params.Name, err)
		}
		if op == backend.OpStructural && in.Repo != "" {
			a.Paths = []string{in.Repo}
		}
		out, err := p.Call(ctx, op, a)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", req.Params.Name, err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil, nil
	}
}

// outlineHandler selects the engine through outline.Route and dispatches the
// routed op, mirroring the CLI so both surfaces behave identically: ast-grep for
// directories and the languages it outlines, tilth signature mode otherwise.
func outlineHandler(p *proxy.Proxy) func(context.Context, *mcp.CallToolRequest, OutlineIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in OutlineIn) (*mcp.CallToolResult, any, error) {
		a := backend.Args{Path: in.Path, Items: in.Items, Match: in.Match, Lang: in.Lang, Budget: in.Budget}
		op, err := outline.Route(a)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", req.Params.Name, err)
		}
		out, err := p.Call(ctx, op, a)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", req.Params.Name, err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil, nil
	}
}

// execHandler runs the script through the resident sandbox engine. MCP has no
// stderr, so engine notes ride along as a trailing [notes] block.
func execHandler(eng *codeexec.Engine) func(context.Context, *mcp.CallToolRequest, ExecIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in ExecIn) (*mcp.CallToolResult, any, error) {
		if !codeexec.Supported {
			return nil, nil, fmt.Errorf("%s: %s", req.Params.Name, codeexec.UnsupportedReason)
		}
		out, notes, err := eng.Exec(ctx, in.Script, in.Budget)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", req.Params.Name, err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: withNotes(out, notes)}}}, nil, nil
	}
}

// execToolsHandler renders the sandbox catalog — builtin plus reflected
// signatures and the subset rules — with engine notes as a trailing [notes]
// block.
func execToolsHandler(eng *codeexec.Engine) func(context.Context, *mcp.CallToolRequest, ExecToolsIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ ExecToolsIn) (*mcp.CallToolResult, any, error) {
		if !codeexec.Supported {
			return nil, nil, fmt.Errorf("%s: %s", req.Params.Name, codeexec.UnsupportedReason)
		}
		out, notes, err := eng.Tools(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", req.Params.Name, err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: withNotes(out, notes)}}}, nil, nil
	}
}

// withNotes appends notes to text as a trailing [notes] block, one note per
// line.
func withNotes(text string, notes []string) string {
	if len(notes) == 0 {
		return text
	}
	return text + "\n\n[notes]\n" + strings.Join(notes, "\n")
}

// handler adapts a per-tool Args builder into an MCP tool handler that proxies
// the op and wraps the result text in a tool result; errors carry the called
// tool's name as their prefix.
func handler[In any](p *proxy.Proxy, op backend.Op, args func(In) backend.Args) func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		out, err := p.Call(ctx, op, args(in))
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", req.Params.Name, err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil, nil
	}
}

// bashToonHandler runs the supplied argv through the shared format.Run, the
// in-MCP equivalent of `ccx toon -- <argv>`: stdout is converted and capped. Any
// captured stderr is appended as a neutral [stderr] section (many tools write to
// stderr on success), [exit N] is appended only on a non-zero exit, and only a
// non-zero exit flags the result as an error. A spawn failure is returned as an
// error.
func bashToonHandler() func(context.Context, *mcp.CallToolRequest, BashToonIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in BashToonIn) (*mcp.CallToolResult, any, error) {
		delim, err := bashToonDelimiter(in.Delimiter)
		if err != nil {
			return nil, nil, fmt.Errorf("BashToon: %w", err)
		}
		opts := format.Options{Format: format.FormatAuto, Indent: bashToonIndent(in.Indent), Delimiter: delim}
		if in.ForceTOON {
			opts.Format = format.FormatTOON
		}

		var stderr bytes.Buffer
		out, _, code, err := format.Run(ctx, in.Command, opts, nil, &stderr)
		if err != nil {
			return nil, nil, fmt.Errorf("BashToon: %w", err)
		}

		text := render.Cap(out, in.Budget)
		if stderr.Len() > 0 {
			text += "\n[stderr]\n" + strings.TrimRight(stderr.String(), "\n")
		}
		if code != 0 {
			text += fmt.Sprintf("\n[exit %d]", code)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
			IsError: code != 0,
		}, nil, nil
	}
}

// bashToonIndent defaults a zero indent to the spec's two spaces.
func bashToonIndent(indent int) int {
	if indent == 0 {
		return 2
	}
	return indent
}

// bashToonDelimiter resolves a delimiter name to its TOON delimiter, defaulting
// an empty name to comma.
func bashToonDelimiter(name string) (format.Delimiter, error) {
	switch name {
	case "", "comma":
		return format.DelimiterComma, nil
	case "tab":
		return format.DelimiterTab, nil
	case "pipe":
		return format.DelimiterPipe, nil
	default:
		return 0, fmt.Errorf("invalid delimiter %q: want comma|tab|pipe", name)
	}
}
