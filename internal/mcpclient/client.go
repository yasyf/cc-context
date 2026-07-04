// Package mcpclient spawns stdio MCP servers as child processes and extracts
// text from their tool results.
package mcpclient

import (
	"context"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/internal/version"
)

// Connect spawns bin as a stdio MCP server child process and returns its
// session, identifying the client as clientName.
func Connect(ctx context.Context, clientName, bin string, argv ...string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: version.String()}, nil)
	return client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(bin, argv...)}, nil) //nolint:gosec // bin/argv come from trusted backend resolution (vendored tilth path, fixed semble MCPSpec), not user free-text
}

// TextOf concatenates the text content blocks of a tool result.
func TextOf(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
