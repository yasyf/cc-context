// Package backend defines the stable logical-op vocabulary — the Op set and the
// Args every op consumes — shared across the CLI and MCP surfaces.
package backend

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
	// OpEdit replaces or deletes an anchored or positional line range in place. It
	// resolves and writes locally without dispatching to any engine.
	OpEdit Op = "edit"
	// OpStructural is an ast-grep structural pattern search.
	OpStructural Op = "structural"
	// OpReplace is an ast-grep structural find-replace.
	OpReplace Op = "replace"
	// OpStructOutline is an ast-grep structural outline (file or directory). It
	// serves `ccx code outline` for the languages ast-grep outlines; OpOutline serves
	// the rest via the native fallback. outline.Route picks between them.
	OpStructOutline Op = "struct-outline"
	// OpWebOutline, OpWebRead, and OpWebSearch fetch, chunk, and serve a web page
	// as a token-bounded outline, section read, or hybrid search. Like OpEdit they
	// run in-process (internal/web) without dispatching to any engine.
	//
	// WARNING: never add these ops to anchor.RewriteArgs. Their section refs ride
	// the Args field but follow web's §<id>#<hash> chunk scheme, not the
	// filesystem line-anchor scheme RewriteArgs resolves — rewriting them would
	// corrupt a URL section ref into a bogus file range.
	OpWebOutline Op = "web-outline"
	OpWebRead    Op = "web-read"
	OpWebSearch  Op = "web-search"
)

// Args carries every flag and positional an op may consume. Each op reads only
// the fields relevant to it.
type Args struct {
	Path             string
	URL              string
	Query            string
	Glob             string
	Section          string
	Scope            string
	Source           string
	Kind             string
	Pattern          string
	Rewrite          string
	Content          string
	Lang             string
	Items            string
	Match            string
	Mode             string
	Paths            []string
	Full             bool
	RevealSecrets    bool
	Apply            bool
	Force            bool
	Delete           bool
	All              bool
	IgnoreCase       bool
	Word             bool
	Regex            bool
	FilesWithMatches bool
	Body             bool
	Callers          bool
	Callees          bool
	Siblings         bool
	Tests            bool
	Deep             bool
	Budget           int
	Offset           int
	K                int
	MaxSnippetLines  int
	Expand           int
	After            int
	Before           int
	Context          int
}
