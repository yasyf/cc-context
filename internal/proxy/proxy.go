// Package proxy hosts persistent MCP client sessions to the bundled tilth and
// semble servers, fronting their tools behind the stable logical-op surface.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/astgrep"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/deps"
	"github.com/yasyf/cc-context/internal/diff"
	"github.com/yasyf/cc-context/internal/edit"
	"github.com/yasyf/cc-context/internal/find"
	"github.com/yasyf/cc-context/internal/mcpclient"
	"github.com/yasyf/cc-context/internal/outline"
	"github.com/yasyf/cc-context/internal/overview"
	"github.com/yasyf/cc-context/internal/read"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/ripgrep"
	"github.com/yasyf/cc-context/internal/router"
	"github.com/yasyf/cc-context/internal/symbol"
	"github.com/yasyf/cc-context/internal/web"
)

// Proxy fronts the bundled engines behind the stable op surface. Each engine's
// child MCP session opens lazily on first use and stays resident until Close, so
// the facade starts serving immediately instead of blocking on a cold engine.
type Proxy struct {
	mu      sync.Mutex
	engines map[backend.Engine]*engineSession
}

// engineSession guards one engine's resident session. Its own mutex serializes
// that engine's first connect without blocking a different engine's.
type engineSession struct {
	mu      sync.Mutex
	session *mcp.ClientSession
}

// New returns a proxy with no child sessions yet; each connects on first use.
func New() *Proxy {
	return &Proxy{engines: map[backend.Engine]*engineSession{}}
}

// Call resolves content anchors in a to plain line numbers, dispatches op
// through its backend, and prepends the anchor-move note after capping so the
// note is never truncated away.
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

// call routes op through its backend and returns budget-capped output. The edit,
// web, find, read, overview, grep, outline, and diff ops run in-process
// (internal/edit, internal/web, internal/find, internal/read, internal/overview,
// internal/ripgrep, internal/outline, internal/diff) without any engine, mirroring
// the CLI dispatch; the ast-grep ops run through the shared astgrep orchestration
// (ast-grep has no MCP server); every other op is a child MCP tool call against the
// engine's resident (lazily opened) session.
func (p *Proxy) call(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
	if op == backend.OpOutline || op == backend.OpStructOutline {
		// Skip a binary target before dispatch (both engines), so a forced --lang
		// still skips. BinarySkip stats internally and no-ops on a dir or text file.
		if line, skipped := outline.BinarySkip(a.Path); skipped {
			return line, nil
		}
	}
	if op == backend.OpEdit {
		return edit.Run(a)
	}
	if op == backend.OpWebOutline || op == backend.OpWebRead || op == backend.OpWebSearch {
		out, err := web.Run(ctx, op, a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	}
	if op == backend.OpFind {
		out, err := find.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	}
	if op == backend.OpRead {
		out, err := read.Run(a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	}
	if op == backend.OpOverview {
		out, err := overview.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	}
	if op == backend.OpGrep {
		return ripgrep.Run(ctx, a)
	}
	if op == backend.OpOutline {
		// The native fallback anchors its own output; cap only, never re-anchor.
		out, err := outline.Fallback(a.Path, a)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	}
	if op == backend.OpDiff {
		// The diff renderer anchors its own output; cap only, never render.Finalize.
		out, err := diff.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	}
	if op == backend.OpSymbol {
		// symbol self-anchors; cap only, never render.Finalize's symbol grammar.
		out, err := symbol.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	}
	if op == backend.OpDeps {
		out, err := deps.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	}

	b := router.For(op)

	switch op {
	case backend.OpStructural, backend.OpReplace, backend.OpStructOutline:
		return astgrep.Run(ctx, op, a)
	}

	tool, params, err := b.MCPTool(op, a)
	if err != nil {
		return "", err
	}

	session, err := p.session(ctx, b)
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

// session returns b's engine session, connecting it on first use. The connection
// outlives the triggering call (context.WithoutCancel) so it stays resident for
// the proxy's lifetime; a failed connect is not cached, so the next call retries.
func (p *Proxy) session(ctx context.Context, b backend.Backend) (*mcp.ClientSession, error) {
	es := p.engineSlot(b.Engine())

	es.mu.Lock()
	defer es.mu.Unlock()
	if es.session != nil {
		return es.session, nil
	}

	cmd, argv, err := b.MCPSpec(ctx)
	if err != nil {
		return nil, fmt.Errorf("proxy: resolve %s: %w", b.Engine(), err)
	}
	session, err := mcpclient.Connect(context.WithoutCancel(ctx), "cc-context-proxy", cmd, argv...)
	if err != nil {
		return nil, fmt.Errorf("proxy: connect %s: %w", b.Engine(), err)
	}
	es.session = session
	return session, nil
}

// engineSlot returns the per-engine session guard, creating it once.
func (p *Proxy) engineSlot(eng backend.Engine) *engineSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	es, ok := p.engines[eng]
	if !ok {
		es = &engineSession{}
		p.engines[eng] = es
	}
	return es
}

// Close shuts down every opened child session, joining any close errors.
func (p *Proxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	errs := make([]error, 0, len(p.engines))
	for _, es := range p.engines {
		es.mu.Lock()
		if es.session != nil {
			errs = append(errs, es.session.Close())
		}
		es.mu.Unlock()
	}
	return errors.Join(errs...)
}
