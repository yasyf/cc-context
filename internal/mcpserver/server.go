// Package mcpserver exposes the static ccx_* tool surface over stdio, proxying
// each tool to the bundled tilth and semble engines.
package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/proxy"
	"github.com/yasyf/cc-context/internal/version"
)

// SearchIn is the input for ccx_search.
type SearchIn struct {
	Query           string `json:"query" jsonschema:"natural-language or symbol query"`
	Repo            string `json:"repo,omitempty" jsonschema:"repo root or https git URL to search; defaults to the project root"`
	K               int    `json:"k,omitempty" jsonschema:"max results to return"`
	MaxSnippetLines int    `json:"max_snippet_lines,omitempty" jsonschema:"max lines per result snippet"`
}

// RelatedIn is the input for ccx_related.
type RelatedIn struct {
	Location string `json:"location" jsonschema:"a file:line location to find related code for"`
}

// OutlineIn is the input for ccx_outline.
type OutlineIn struct {
	Path   string `json:"path" jsonschema:"file to outline"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the outline"`
}

// ReadIn is the input for ccx_read.
type ReadIn struct {
	Path    string `json:"path" jsonschema:"file to read"`
	Section string `json:"section,omitempty" jsonschema:"a line range (\"A-B\") or a heading (\"## Heading\")"`
	Full    bool   `json:"full,omitempty" jsonschema:"read the whole file"`
	Budget  int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// SymbolIn is the input for ccx_symbol.
type SymbolIn struct {
	Name  string `json:"name" jsonschema:"symbol to grok"`
	Scope string `json:"scope,omitempty" jsonschema:"directory to scope the lookup to"`
	Full  bool   `json:"full,omitempty" jsonschema:"include full bodies"`
}

// DepsIn is the input for ccx_deps.
type DepsIn struct {
	Path   string `json:"path" jsonschema:"file to analyze"`
	Scope  string `json:"scope,omitempty" jsonschema:"directory to scope the analysis to"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// GrepIn is the input for ccx_grep.
type GrepIn struct {
	Text   string `json:"text" jsonschema:"text to search for"`
	Glob   string `json:"glob,omitempty" jsonschema:"restrict to files matching this glob"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
	Expand int    `json:"expand,omitempty" jsonschema:"lines of context to expand around hits"`
}

// FindIn is the input for ccx_find.
type FindIn struct {
	Glob  string `json:"glob" jsonschema:"glob to match files against"`
	Scope string `json:"scope,omitempty" jsonschema:"directory to scope the search to"`
}

// DiffIn is the input for ccx_diff.
type DiffIn struct {
	Source string `json:"source,omitempty" jsonschema:"diff source: uncommitted|staged|<ref> (default uncommitted)"`
	Scope  string `json:"scope,omitempty" jsonschema:"path to scope the diff to"`
	Budget int    `json:"budget,omitempty" jsonschema:"token budget for the output"`
}

// OverviewIn is the input for ccx_overview.
type OverviewIn struct{}

// Serve opens the proxy, registers the static ccx_* tools, and serves them over
// stdio until ctx is cancelled or the transport closes.
func Serve(ctx context.Context) error {
	p, err := proxy.New(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()

	s := mcp.NewServer(&mcp.Implementation{Name: "cc-context", Version: version.String()}, nil)
	register(s, p)

	return s.Run(ctx, &mcp.StdioTransport{})
}

// register wires every static tool to a handler that builds backend.Args and
// proxies the call to the matching engine.
func register(s *mcp.Server, p *proxy.Proxy) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_search",
		Description: "Semantic code search by intent or symbol — prefer over grep for \"where/how do we do X\" questions.",
	}, handler(p, backend.OpSearch, func(in SearchIn) backend.Args {
		return backend.Args{Query: in.Query, Path: in.Repo, K: in.K, MaxSnippetLines: in.MaxSnippetLines}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_related",
		Description: "Find code semantically related to a file:line location — follow-up to a prior search hit.",
	}, handler(p, backend.OpRelated, func(in RelatedIn) backend.Args {
		return backend.Args{Query: in.Location}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_outline",
		Description: "Compact file outline (signatures + line numbers, budget-bounded) — prefer over reading a whole file.",
	}, handler(p, backend.OpOutline, func(in OutlineIn) backend.Args {
		return backend.Args{Path: in.Path, Budget: in.Budget}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_read",
		Description: "Read a file by section, heading, or whole — pass section to avoid pulling the entire file.",
	}, handler(p, backend.OpRead, func(in ReadIn) backend.Args {
		return backend.Args{Path: in.Path, Section: in.Section, Full: in.Full, Budget: in.Budget}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_symbol",
		Description: "Grok a symbol: definition, doc, callers, callees, siblings, tests — one call beats many greps.",
	}, handler(p, backend.OpSymbol, func(in SymbolIn) backend.Args {
		return backend.Args{Query: in.Name, Scope: in.Scope, Full: in.Full}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_deps",
		Description: "Dependencies of a file (imports and their resolved targets), budget-bounded.",
	}, handler(p, backend.OpDeps, func(in DepsIn) backend.Args {
		return backend.Args{Path: in.Path, Scope: in.Scope, Budget: in.Budget}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_grep",
		Description: "Literal text search across code, optionally globbed and budget-bounded — for exact strings.",
	}, handler(p, backend.OpGrep, func(in GrepIn) backend.Args {
		return backend.Args{Query: in.Text, Glob: in.Glob, Budget: in.Budget, Expand: in.Expand}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_find",
		Description: "List files matching a glob with per-file token counts — prefer over ls -R for enumeration.",
	}, handler(p, backend.OpFind, func(in FindIn) backend.Args {
		return backend.Args{Glob: in.Glob, Scope: in.Scope}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_diff",
		Description: "VCS-aware diff (uncommitted|staged|<ref>), budget-bounded — prefer over a raw git diff pager.",
	}, handler(p, backend.OpDiff, func(in DiffIn) backend.Args {
		source := in.Source
		if source == "" {
			source = "uncommitted"
		}
		return backend.Args{Source: source, Scope: in.Scope, Budget: in.Budget}
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ccx_overview",
		Description: "Repository overview (structure and entry points) — orient here before diving into files.",
	}, handler(p, backend.OpOverview, func(OverviewIn) backend.Args {
		return backend.Args{}
	}))
}

// handler adapts a per-tool Args builder into an MCP tool handler that proxies
// the op and wraps the result text in a tool result.
func handler[In any](p *proxy.Proxy, op backend.Op, args func(In) backend.Args) func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		out, err := p.Call(ctx, op, args(in))
		if err != nil {
			return nil, nil, fmt.Errorf("ccx_%s: %w", op, err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil, nil
	}
}
