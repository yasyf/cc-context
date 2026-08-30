package mcpserver

import (
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/workspace"
)

// textResult wraps text as a tool result verbatim, for a tool that resolves
// against no project root, or whose result is a payload its caller chose rather
// than a view ccx rendered.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// rootHeaderResult is textResult for an op that answers out of a project root,
// naming that root in the header block. The MCP server serves whichever tree it
// was started in, and its cites are repo-relative, so without the root line an
// answer from the wrong worktree reads exactly like one from the right tree.
func rootHeaderResult(text string) (*mcp.CallToolResult, any, error) {
	root, err := workspace.Root()
	if err != nil {
		return nil, nil, err
	}
	return textResult(withRootLine(text, root)), nil, nil
}

// withRootLine joins "# root <abspath>" to text's header block: after the
// opening header line when text has one, at the top otherwise. It never scans
// past the first line — a read of a markdown file serves the document's own "#"
// headings as content, and those are not header lines.
func withRootLine(text, root string) string {
	line := "# root " + root + "\n"
	head, rest, split := strings.Cut(text, "\n")
	if !split || !strings.HasPrefix(head, "# ") {
		return line + text
	}
	return head + "\n" + line + rest
}

// opResult wraps text as op's tool result, rooted unless op answers from no
// project root. It is every op-dispatching handler's whole result shaping, so a
// handler that moves keeps the root line by carrying this one line with it.
func opResult(op backend.Op, text string) (*mcp.CallToolResult, any, error) {
	if rootless(op) {
		return textResult(text), nil, nil
	}
	return rootHeaderResult(text)
}

// rootless reports whether op answers from no project root. Only the web ops
// qualify: they resolve a URL, so no tree can be named as their source.
func rootless(op backend.Op) bool {
	switch op {
	case backend.OpWebOutline, backend.OpWebRead, backend.OpWebSearch:
		return true
	default:
		return false
	}
}
