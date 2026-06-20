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
	Paths           []string
	Full            bool
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
