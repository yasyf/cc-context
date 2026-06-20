// Package proxy hosts persistent MCP client sessions to the bundled tilth and
// semble servers, fronting their tools behind the stable logical-op surface.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/router"
	"github.com/yasyf/cc-context/internal/version"
)

// Proxy holds the long-lived child MCP sessions, one per backend engine. Each
// session opens once in New and stays resident until Close.
type Proxy struct {
	sessions map[backend.Engine]*mcp.ClientSession
}

// New spawns the tilth and semble MCP servers and opens a persistent client
// session to each, keyed by engine. Each backend resolves its own launch spec
// via MCPSpec, so provisioning failures surface here. Connect performs the MCP
// initialize handshake implicitly.
func New(ctx context.Context) (*Proxy, error) {
	tilth := backend.Tilth{}
	tilthCmd, tilthArgv, err := tilth.MCPSpec(ctx)
	if err != nil {
		return nil, fmt.Errorf("proxy: resolve tilth: %w", err)
	}
	tilthSession, err := connect(ctx, tilthCmd, tilthArgv...)
	if err != nil {
		return nil, fmt.Errorf("proxy: connect tilth: %w", err)
	}

	semble := backend.Semble{}
	sembleCmd, sembleArgv, err := semble.MCPSpec(ctx)
	if err != nil {
		_ = tilthSession.Close()
		return nil, fmt.Errorf("proxy: resolve semble: %w", err)
	}
	sembleSession, err := connect(ctx, sembleCmd, sembleArgv...)
	if err != nil {
		_ = tilthSession.Close()
		return nil, fmt.Errorf("proxy: connect semble: %w", err)
	}

	return &Proxy{sessions: map[backend.Engine]*mcp.ClientSession{
		tilth.Engine():  tilthSession,
		semble.Engine(): sembleSession,
	}}, nil
}

// connect spawns the named child as a stdio MCP server and returns its session.
func connect(ctx context.Context, bin string, argv ...string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "cc-context-proxy", Version: version.String()}, nil)
	return client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(bin, argv...)}, nil) //nolint:gosec // bin/argv come from trusted backend resolution (vendored tilth path, fixed semble MCPSpec), not user free-text
}

// Call routes op through its backend and returns budget-capped output. The diff
// and overview ops run as child-process CLI invocations to keep the jj-aware
// VCS translation and fallback that CLIArgv performs; every other op is a child
// MCP tool call against the resident session.
func (p *Proxy) Call(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
	b := router.For(op)

	switch op {
	case backend.OpDiff, backend.OpOverview:
		bin, argv, err := b.CLIArgv(ctx, op, a)
		if err != nil {
			return "", err
		}
		out, err := render.RunCLI(ctx, bin, argv)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	}

	tool, params, err := b.MCPTool(op, a)
	if err != nil {
		return "", err
	}

	res, err := p.sessions[b.Engine()].CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: params})
	if err != nil {
		return "", fmt.Errorf("proxy: call %q: %w", tool, err)
	}

	text := textOf(res)
	if res.IsError {
		return "", fmt.Errorf("proxy: tool %q failed: %s", tool, text)
	}
	return render.Cap(text, a.Budget), nil
}

// Close shuts down every child session, joining any close errors.
func (p *Proxy) Close() error {
	errs := make([]error, 0, len(p.sessions))
	for _, s := range p.sessions {
		errs = append(errs, s.Close())
	}
	return errors.Join(errs...)
}

// textOf concatenates the text content blocks of a tool result.
func textOf(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
