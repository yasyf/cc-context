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
	"github.com/yasyf/cc-context/internal/find"
	"github.com/yasyf/cc-context/internal/format"
	"github.com/yasyf/cc-context/internal/outline"
	"github.com/yasyf/cc-context/internal/proxy"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/ripgrep"
	"github.com/yasyf/cc-context/internal/search"
	"github.com/yasyf/cc-context/internal/version"
)

// defaultSnippetLines bounds ccx_code_search result snippets when the caller
// gives no explicit max — semble's own default returns the whole chunk.
const defaultSnippetLines = 10

// SearchIn is the input for ccx_code_search.
type SearchIn struct {
	Query           string `json:"query" jsonschema:"natural-language intent, structural pattern, or literal text"`
	Repo            string `json:"repo,omitempty" jsonschema:"repo root or https git URL; default project root"`
	Mode            string `json:"mode,omitempty" jsonschema:"routing override: auto|semantic|structural|literal (default auto)"`
	Lang            string `json:"lang,omitempty" jsonschema:"structural: language; inferred from extension"`
	K               int    `json:"k,omitempty" jsonschema:"max results to return"`
	MaxSnippetLines int    `json:"max_snippet_lines,omitempty" jsonschema:"max lines per result snippet"`
}

// ReplaceIn is the input for ccx_code_replace.
type ReplaceIn struct {
	Pattern string   `json:"pattern" jsonschema:"ast-grep structural pattern; metavars like $A and $$$ for variadic"`
	Rewrite string   `json:"rewrite" jsonschema:"replacement template; reference the same metavars"`
	Paths   []string `json:"paths,omitempty" jsonschema:"files or dirs to scope to; default repo root"`
	Lang    string   `json:"lang,omitempty" jsonschema:"language; inferred from extension"`
	Glob    string   `json:"glob,omitempty" jsonschema:"gitignore-style include/exclude; ! to exclude"`
	Apply   bool     `json:"apply,omitempty" jsonschema:"WRITE the changes; omit for a preview diff"`
	Force   bool     `json:"force,omitempty" jsonschema:"bypass the apply file-count cap"`
	Budget  int      `json:"budget,omitempty" jsonschema:"token budget for the preview"`
}

// EditIn is the input for ccx_code_edit. Content is a pointer so an explicitly
// empty replacement (blank the range) is distinct from an omitted one.
type EditIn struct {
	Path    string  `json:"path" jsonschema:"file to edit in place"`
	At      string  `json:"at" jsonschema:"span to replace: line range (\"40-95\"), single line, or anchor (\"15-27#k2fa\" or bare \"k2fa\"); a shifted anchor re-resolves by content"`
	Content *string `json:"content,omitempty" jsonschema:"replacement text; give exactly one of content or delete"`
	Delete  bool    `json:"delete,omitempty" jsonschema:"delete the span; give exactly one of content or delete"`
	Budget  int     `json:"budget,omitempty" jsonschema:"token budget for the report"`
}

// RelatedIn is the input for ccx_code_related.
type RelatedIn struct {
	Location string `json:"location" jsonschema:"file:line, or an anchor (\"f.go:12#a3fk\"); a shifted anchor re-resolves by content and prepends a \"# anchor …\" note"`
}

// OutlineIn is the input for ccx_code_outline.
type OutlineIn struct {
	Path    string `json:"path" jsonschema:"file or directory to outline"`
	Section string `json:"section,omitempty" jsonschema:"window a single-file (ast-grep) outline to a line range (\"40-95\" or \"40,95\")"`
	Deep    bool   `json:"deep,omitempty" jsonschema:"ast-grep: include members (struct fields, class methods); default is top-level only"`
	Full    bool   `json:"full,omitempty" jsonschema:"alias for deep: include members"`
	Items   string `json:"items,omitempty" jsonschema:"ast-grep: items to include (imports|exports|structure|all)"`
	Match   string `json:"match,omitempty" jsonschema:"ast-grep: keep items whose name/signature matches this regex"`
	Lang    string `json:"lang,omitempty" jsonschema:"ast-grep: language; inferred from extension"`
	Budget  int    `json:"budget,omitempty" jsonschema:"token budget for the outline"`
}

// ReadIn is the input for ccx_code_read.
type ReadIn struct {
	Path          string `json:"path" jsonschema:"file to read"`
	Section       string `json:"section,omitempty" jsonschema:"line range (\"40-95\"), heading (\"## Heading\"), or anchor (\"15-27#k2fa\" or bare \"k2fa\"); a shifted anchor re-resolves by content and prepends a \"# anchor …\" note"`
	Full          bool   `json:"full,omitempty" jsonschema:"read the whole file"`
	RevealSecrets bool   `json:"reveal_secrets,omitempty" jsonschema:"print detected secrets raw instead of masked"`
	Budget        int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// SymbolIn is the input for ccx_code_symbol.
type SymbolIn struct {
	Name     string `json:"name" jsonschema:"symbol to grok"`
	Scope    string `json:"scope,omitempty" jsonschema:"directory to scope the lookup to"`
	Body     bool   `json:"body,omitempty" jsonschema:"include the definition body"`
	Callers  bool   `json:"callers,omitempty" jsonschema:"include the callers list"`
	Callees  bool   `json:"callees,omitempty" jsonschema:"include the callees list"`
	Siblings bool   `json:"siblings,omitempty" jsonschema:"include the siblings list"`
	Tests    bool   `json:"tests,omitempty" jsonschema:"include the tests list"`
	Full     bool   `json:"full,omitempty" jsonschema:"the full rich output: body plus every list"`
}

// DepsIn is the input for ccx_code_deps.
type DepsIn struct {
	Path   string `json:"path" jsonschema:"file to analyze"`
	Scope  string `json:"scope,omitempty" jsonschema:"directory to scope the analysis to"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// GrepIn is the input for ccx_code_grep.
type GrepIn struct {
	Text       string   `json:"text" jsonschema:"text to search for"`
	Glob       string   `json:"glob,omitempty" jsonschema:"files matching this glob; an anchored dir glob searches even under ignore rules"`
	Scope      string   `json:"scope,omitempty" jsonschema:"directory to scope to"`
	IgnoreCase bool     `json:"ignoreCase,omitempty" jsonschema:"case-insensitive; runs the rg/grep engine"`
	Word       bool     `json:"word,omitempty" jsonschema:"whole words only; runs the rg/grep engine"`
	Regex      bool     `json:"regex,omitempty" jsonschema:"treat text as regex; runs the rg/grep engine"`
	Paths      []string `json:"paths,omitempty" jsonschema:"search these files, not the tree; runs the rg/grep engine"`
	Budget     int      `json:"budget,omitempty" jsonschema:"token budget for the output"`
	Expand     int      `json:"expand,omitempty" jsonschema:"rg engine: context lines per hit; default engine: inlines full source of top matches"`
	After      int      `json:"after,omitempty" jsonschema:"context lines after each match (-A)"`
	Before     int      `json:"before,omitempty" jsonschema:"context lines before each match (-B)"`
	Context    int      `json:"context,omitempty" jsonschema:"context lines around each match (-C)"`
}

// FindIn is the input for ccx_repo_find.
type FindIn struct {
	Glob   string `json:"glob" jsonschema:"glob to match files against"`
	Scope  string `json:"scope,omitempty" jsonschema:"directory to scope the search to"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the output; 0 or omitted = default 2000; unlimited listing is the codeexec lane"`
}

// findArgs builds the find backend.Args from a ccx_repo_find call, applying the
// default budget when the caller sets none (the codeexec path leaves it zero).
func findArgs(in FindIn) backend.Args {
	a := backend.Args{Glob: in.Glob, Scope: in.Scope, Budget: in.Budget}
	if a.Budget == 0 {
		a.Budget = find.DefaultBudget
	}
	return a
}

// DiffIn is the input for ccx_vcs_diff.
type DiffIn struct {
	Source string `json:"source,omitempty" jsonschema:"diff source: uncommitted|staged|<ref> (default uncommitted)"`
	Scope  string `json:"scope,omitempty" jsonschema:"path to scope the diff to"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// OverviewIn is the input for ccx_repo_overview.
type OverviewIn struct{}

// WebOutlineIn is the input for ccx_web_outline.
type WebOutlineIn struct {
	URL    string `json:"url" jsonschema:"web page URL to outline"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// WebReadIn is the input for ccx_web_read.
type WebReadIn struct {
	URL     string `json:"url" jsonschema:"web page URL to read"`
	Section string `json:"section,omitempty" jsonschema:"section ref from ccx_web_outline (\"2.3\" or \"2.3#k7fq\"); omit or set full for the whole page"`
	Full    bool   `json:"full,omitempty" jsonschema:"read the whole page"`
	Offset  int    `json:"offset,omitempty" jsonschema:"skip this many tokens into the section to page past a budget cap (the footer names the next offset)"`
	Budget  int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// WebSearchIn is the input for ccx_web_search.
type WebSearchIn struct {
	URL    string `json:"url" jsonschema:"web page URL to search"`
	Query  string `json:"query" jsonschema:"the question to ask of the page"`
	K      int    `json:"k,omitempty" jsonschema:"max results to return"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// ExecIn is the input for ccx_exec.
type ExecIn struct {
	Script string `json:"script" jsonschema:"Python (monty subset); host functions are async — await them; end with asyncio.run(main()) or a bare final expression"`
	Budget int    `json:"budget,omitempty" jsonschema:"max output tokens, 0 = default"`
}

// ExecToolsIn is the input for ccx_exec_tools.
type ExecToolsIn struct{}

// BashFormatIn is the input for BashFormat.
type BashFormatIn struct {
	Command   []string `json:"command" jsonschema:"argv to RUN (no shell); argv[0] is the program, the rest its args"`
	Format    string   `json:"format,omitempty" jsonschema:"output format: auto|toon|tron|csv|tsv|markdown|jsonl|prose|json (default auto = leanest for the shape)"`
	Delimiter string   `json:"delimiter,omitempty" jsonschema:"array delimiter, TOON output only: comma|tab|pipe (default comma)"`
	Indent    int      `json:"indent,omitempty" jsonschema:"spaces per indent level, TOON only (default 2)"`
	Budget    int      `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// Serve creates the proxy (engines connect lazily on first use) and the
// resident sandbox engine, registers the static ccx_* tools, and serves them
// over stdio until ctx is cancelled or the transport closes.
func Serve(ctx context.Context) error {
	p := proxy.New()
	defer func() { _ = p.Close() }()

	var eng *codeexec.Engine
	if codeexec.Supported() {
		inventories, err := codeexec.NewDiskInventoryStore()
		if err != nil {
			return err
		}
		eng = codeexec.NewEngine(p, codeexec.NewMemoryStore(), codeexec.WithInventoryStore(inventories))
		defer func() { _ = eng.Close() }()
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "cc-context", Version: version.String()}, &mcp.ServerOptions{
		Instructions: "Single question → the matching ccx_* tool; pipeline, filter, fan-out, or post-processed output → ccx_exec (catalog once via ccx_exec_tools). Producer outputs carry anchors (path:12#a3fk) and web refs (§2.3) — echo them into ccx_code_read or ccx_web_read to chain.",
	})
	register(s, p, eng)

	return s.Run(ctx, &mcp.StdioTransport{})
}

// metaAlwaysLoad is the tool _meta key Claude Code reads to exempt a tool from
// tool-search deferral (ENABLE_TOOL_SEARCH).
const metaAlwaysLoad = "anthropic/alwaysLoad"

// alwaysLoad marks the common ccx tools eager-loaded at session start, so a
// guard redirect to one costs no ToolSearch round-trip.
var alwaysLoad = mcp.Meta{metaAlwaysLoad: true}

// register wires every static tool to a handler that builds backend.Args and
// proxies the call to the matching engine.
func register(s *mcp.Server, p *proxy.Proxy, eng *codeexec.Engine) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_search",
		Description: "Search code by intent (semantic), ast-grep structural pattern, or literal text — routed by query kind. Prefer over grep for where/how.",
		Meta:        alwaysLoad,
	}, searchHandler(p))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_replace",
		Description: "Structural find-replace (ast-grep): preview a diff by default, or apply — without reading the file into context.",
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
		Name:        "ccx_code_edit",
		Description: "WRITE an in-place edit: replace or delete a line range or anchored span. An anchored 'at' is hash-verified — refuses if the span vanished or resolves ambiguously; a bare numeric range is bounds-checked only. Applies immediately (no preview); a moved anchor comes back as a \"# anchor …\" note. Give exactly one of content or delete.",
	}, editHandler(p))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_related",
		Description: "Find code semantically related to a file:line — follow-up to a search hit. Takes an anchored location too (f.go:12#a3fk).",
	}, handler(p, backend.OpRelated, func(in RelatedIn) backend.Args {
		return backend.Args{Query: in.Location}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_outline",
		Description: "Outline a file or directory — top-level signatures + line numbers, budget-bounded; prefer over reading whole files. --deep/--full adds members. Routes to ast-grep (items, match, section window) or tilth by language.",
		Meta:        alwaysLoad,
	}, outlineHandler(p))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_read",
		Description: "Read a file by section, heading, anchor, or whole; pass section to avoid the entire file.",
		Meta:        alwaysLoad,
	}, handler(p, backend.OpRead, func(in ReadIn) backend.Args {
		return backend.Args{Path: in.Path, Section: in.Section, Full: in.Full, RevealSecrets: in.RevealSecrets, Budget: in.Budget}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_symbol",
		Description: "Grok a symbol: signature, path:line#anchor, and doc by default — one call beats many greps. Body and caller/callee/sibling/test lists behind --body/--callers/--callees/--siblings/--tests/--full.",
	}, handler(p, backend.OpSymbol, func(in SymbolIn) backend.Args {
		return backend.Args{Query: in.Name, Scope: in.Scope, Body: in.Body, Callers: in.Callers, Callees: in.Callees, Siblings: in.Siblings, Tests: in.Tests, Full: in.Full}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_deps",
		Description: "Dependencies of a file (imports and their resolved targets), budget-bounded.",
	}, handler(p, backend.OpDeps, func(in DepsIn) backend.Args {
		return backend.Args{Path: in.Path, Scope: in.Scope, Budget: in.Budget}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_code_grep",
		Description: "Grep literal or regex text across code — globbed, scoped, or over explicit files; budget-bounded.",
		Meta:        alwaysLoad,
	}, handler(p, backend.OpGrep, func(in GrepIn) backend.Args {
		a := backend.Args{Query: in.Text, Glob: in.Glob, Scope: in.Scope, IgnoreCase: in.IgnoreCase, Word: in.Word, Regex: in.Regex, Paths: in.Paths, Budget: in.Budget, Expand: in.Expand, After: in.After, Before: in.Before, Context: in.Context}
		if ripgrep.Handles(a) && a.Budget == 0 {
			a.Budget = ripgrep.DefaultBudget
		}
		return a
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_repo_find",
		Description: "List files matching a glob with per-file token counts — gitignore-honoring, budget-capped; for orientation prefer ccx_repo_overview.",
	}, handler(p, backend.OpFind, findArgs))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_vcs_diff",
		Description: "VCS-aware diff (uncommitted|staged|<ref>), budget-bounded — prefer over a raw git diff pager.",
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
		Name:        "ccx_web_outline",
		Description: "Web page heading tree with stable section refs, budget-bounded — orient before reading.",
	}, handler(p, backend.OpWebOutline, func(in WebOutlineIn) backend.Args {
		return backend.Args{URL: in.URL, Budget: in.Budget}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_web_read",
		Description: "Read a web page by section ref (from ccx_web_outline) or whole — pass a section to avoid the whole page.",
	}, handler(p, backend.OpWebRead, func(in WebReadIn) backend.Args {
		return backend.Args{URL: in.URL, Section: in.Section, Full: in.Full, Offset: in.Offset, Budget: in.Budget}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_web_search",
		Description: "Ask a web page a question — top-k relevant chunks with section-ref cites, budget-bounded.",
	}, handler(p, backend.OpWebSearch, func(in WebSearchIn) backend.Args {
		return backend.Args{URL: in.URL, Query: in.Query, K: in.K, Budget: in.Budget}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name: "ccx_exec",
		Description: "RUN a Python script in a sandbox; async host functions are every ccx op, a gated sh(cmd), and " +
			"reflected MCP tools. It filters in-sandbox — only the return value comes back. Use over 2+ chained " +
			"calls or post-processed output (sweep files, fan out with asyncio.gather). Call ccx_exec_tools first " +
			"for the catalog and subset rules.",
	}, execHandler(eng))

	mcp.AddTool(s, &mcp.Tool{
		Name: "ccx_exec_tools",
		Description: "List the host functions a ccx_exec script can call — ccx ops, sh, and reflected MCP tools " +
			"(mutating tools labeled) — plus the allowed Python subset and an example. Call once per session; " +
			"cached until the MCP inventory changes.",
	}, execToolsHandler(eng))

	mcp.AddTool(s, &mcp.Tool{
		Name: "BashFormat",
		Description: "RUN a command (argv, no shell) and token-compact its stdout — JSON/NDJSON re-encoded to the " +
			"leanest shape (prose, CSV/TSV, TOON, TRON, JSONL, or compact JSON), other output passes through. " +
			"Use for JSON-emitting commands (gh --json, kubectl -o json) so raw JSON never enters context. It " +
			"executes the command — not a passive converter; it does not take a JSON string.",
	}, bashFormatHandler())
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

// editHandler enforces the write's exactly-one-of-content-or-delete contract —
// the presence check the CLI makes with cobra's Changed — then proxies the edit
// op, which resolves and writes locally without touching an engine.
func editHandler(p *proxy.Proxy) func(context.Context, *mcp.CallToolRequest, EditIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in EditIn) (*mcp.CallToolResult, any, error) {
		if (in.Content != nil) == in.Delete {
			return nil, nil, fmt.Errorf("%s: provide exactly one of content or delete", req.Params.Name)
		}
		a := backend.Args{Path: in.Path, Section: in.At, Delete: in.Delete, Budget: in.Budget}
		if in.Content != nil {
			a.Content = *in.Content
		}
		out, err := p.Call(ctx, backend.OpEdit, a)
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
		a := backend.Args{Path: in.Path, Section: in.Section, Deep: in.Deep, Full: in.Full, Items: in.Items, Match: in.Match, Lang: in.Lang, Budget: in.Budget}
		op, err := outline.Route(a)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", req.Params.Name, err)
		}
		if _, _, err := outline.ValidateSection(a, op); err != nil {
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
		if !codeexec.Supported() {
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
		if !codeexec.Supported() {
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

// bashFormatHandler runs the supplied argv through the shared format.Run, the
// in-MCP equivalent of `ccx format -- <argv>`: stdout is converted and capped.
// Any captured stderr is appended as a neutral [stderr] section (many tools
// write to stderr on success), [exit N] is appended only on a non-zero exit, and
// only a non-zero exit flags the result as an error. A spawn failure is returned
// as an error.
func bashFormatHandler() func(context.Context, *mcp.CallToolRequest, BashFormatIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in BashFormatIn) (*mcp.CallToolResult, any, error) {
		fm, err := format.ParseFormat(in.Format)
		if err != nil {
			return nil, nil, fmt.Errorf("BashFormat: %w", err)
		}
		delim, err := bashFormatDelimiter(in.Delimiter)
		if err != nil {
			return nil, nil, fmt.Errorf("BashFormat: %w", err)
		}
		opts := format.Options{Format: fm, Indent: bashFormatIndent(in.Indent), Delimiter: delim}

		var stderr bytes.Buffer
		out, _, code, err := format.Run(ctx, in.Command, opts, nil, &stderr)
		if err != nil {
			return nil, nil, fmt.Errorf("BashFormat: %w", err)
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

// bashFormatIndent defaults a zero indent to the spec's two spaces.
func bashFormatIndent(indent int) int {
	if indent == 0 {
		return 2
	}
	return indent
}

// bashFormatDelimiter resolves a delimiter name to its TOON delimiter,
// defaulting an empty name to comma.
func bashFormatDelimiter(name string) (format.Delimiter, error) {
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
