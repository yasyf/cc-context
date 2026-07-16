// Package proxy fronts the facade tools: it runs the native ccx ops in-process
// via internal/dispatch and holds the one resident semble MCP session, opened
// lazily on first use and kept alive until Close.
package proxy

import (
	"context"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/dispatch"
	"github.com/yasyf/cc-context/internal/mcpclient"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/semble"
)

// Proxy fronts the op surface: native ops run in-process via internal/dispatch,
// and the two semantic ops (search, related) call the resident semble MCP
// session, which opens lazily on first use and stays resident until Close.
type Proxy struct {
	mu      sync.Mutex
	session *mcp.ClientSession
}

// New returns a proxy with no semble session yet; it connects on first use.
func New() *Proxy {
	return &Proxy{}
}

// Call resolves content anchors in a to plain line numbers, dispatches op, and
// prepends the anchor-move note after capping so the note is never truncated away.
func (p *Proxy) Call(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
	a, note, err := anchor.RewriteArgs(op, a)
	if err != nil {
		return "", err
	}
	out, err := p.call(ctx, op, a)
	if err != nil {
		return "", err
	}
	return note + out, nil
}

// call runs a native op in-process via dispatch.Run, and the two semantic ops
// (search, related) as a child MCP tool call against the resident semble session.
func (p *Proxy) call(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
	if dispatch.Native(op) {
		return dispatch.Run(ctx, op, a)
	}

	tool, params, err := semble.MCPTool(op, a)
	if err != nil {
		return "", err
	}
	session, err := p.sembleSession(ctx)
	if err != nil {
		return "", err
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: params})
	if err != nil {
		return "", fmt.Errorf("proxy: call %q: %w", tool, err)
	}
	text := mcpclient.TextOf(res)
	if res.IsError {
		return "", fmt.Errorf("proxy: tool %q failed: %s", tool, text)
	}
	return render.Finalize(op, text, a)
}

// sembleSession returns the resident semble session, connecting it on first use.
// The connection outlives the triggering call (context.WithoutCancel) so it stays
// resident for the proxy's lifetime; a failed connect is not cached, so the next
// call retries.
func (p *Proxy) sembleSession(ctx context.Context) (*mcp.ClientSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != nil {
		return p.session, nil
	}
	cmd, argv, err := semble.MCPSpec(ctx)
	if err != nil {
		return nil, fmt.Errorf("proxy: resolve semble: %w", err)
	}
	session, err := mcpclient.Connect(context.WithoutCancel(ctx), "cc-context-proxy", cmd, argv...)
	if err != nil {
		return nil, fmt.Errorf("proxy: connect semble: %w", err)
	}
	p.session = session
	return session, nil
}

// Close shuts down the semble session if it was opened.
func (p *Proxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session == nil {
		return nil
	}
	return p.session.Close()
}
