// Package backend defines the stable logical-op surface and the Backend
// interface that translates ops into concrete CLI/MCP invocations.
package backend

import "context"

// Engine identifies the concrete engine backing a logical op. It keys the
// proxy's per-engine MCP sessions so router.For remains the single source of
// truth for op->engine.
type Engine string

const (
	// EngineTilth is the tilth engine.
	EngineTilth Engine = "tilth"
	// EngineSemble is the semble engine.
	EngineSemble Engine = "semble"
	// EngineAstGrep is the ast-grep engine.
	EngineAstGrep Engine = "ast-grep"
)

// Op is a logical context operation, stable across the CLI and MCP surfaces.
type Op string

// The logical ops. Each maps to exactly one cobra subcommand and one MCP tool.
const (
	OpSearch   Op = "search"
	OpRelated  Op = "related"
	OpOutline  Op = "outline"
	OpRead     Op = "read"
	OpSymbol   Op = "symbol"
	OpDeps     Op = "deps"
	OpGrep     Op = "grep"
	OpFind     Op = "find"
	OpDiff     Op = "diff"
	OpOverview Op = "overview"
	// OpStructural is an ast-grep structural pattern search.
	OpStructural Op = "structural"
	// OpReplace is an ast-grep structural find-replace.
	OpReplace Op = "replace"
	// OpStructOutline is an ast-grep structural outline (file or directory). It
	// serves `ccx code outline` for the languages ast-grep outlines; OpOutline keeps
	// tilth signature mode for the rest. outline.Route picks between them.
	OpStructOutline Op = "struct-outline"
)

// Args carries every flag and positional an op may consume. Each backend reads
// only the fields relevant to the op it is asked to translate.
type Args struct {
	Path            string
	Query           string
	Glob            string
	Section         string
	Scope           string
	Source          string
	Kind            string
	Pattern         string
	Rewrite         string
	Lang            string
	Items           string
	Match           string
	Mode            string
	Paths           []string
	Full            bool
	Apply           bool
	Force           bool
	Budget          int
	K               int
	MaxSnippetLines int
	Expand          int
}

// Backend translates a logical Op plus Args into a concrete invocation, either
// as a child-process argv or as an MCP tool call.
type Backend interface {
	Engine() Engine
	CLIArgv(ctx context.Context, op Op, a Args) (bin string, argv []string, err error)
	MCPSpec(ctx context.Context) (cmd string, argv []string, err error)
	MCPTool(op Op, a Args) (tool string, params map[string]any, err error)
}
