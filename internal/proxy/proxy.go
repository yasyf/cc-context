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
	"github.com/yasyf/cc-context/internal/edit"
	"github.com/yasyf/cc-context/internal/grep"
	"github.com/yasyf/cc-context/internal/grok"
	"github.com/yasyf/cc-context/internal/mcpclient"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/ripgrep"
	"github.com/yasyf/cc-context/internal/router"
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

// call routes op through its backend and returns budget-capped output. The edit
// and web ops run in-process (internal/edit, internal/web) without any engine,
// mirroring the CLI dispatch; a case-insensitive, word-boundary, regex, or
// multi-file grep (ripgrep.Handles) runs the in-process ripgrep engine (tilth
// expresses none of these), matching the CLI dispatch's OpGrep special-case; the
// diff and overview ops run as child-process
// CLI invocations to keep the jj-aware VCS translation and fallback that CLIArgv
// performs; the ast-grep ops run through the shared astgrep orchestration
// (ast-grep has no MCP server); every other op is a child MCP tool call against
// the engine's resident (lazily opened) session.
func (p *Proxy) call(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
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
	if op == backend.OpGrep && ripgrep.Handles(a) {
		return ripgrep.Run(ctx, a)
	}
	// The default OpGrep MCP route is the plain tilth_search tool call below. Like
	// internal/cli/run.go's internal/grep path, its clean tilth zero is re-verified
	// through a live ripgrep recheck after the call (further down): a stale index
	// must not report a confident "0 matches" on either surface. tilth's MCP server
	// has no CLI-style no-match path-fallback, so only the clean-zero shape reaches
	// the recheck here. Headers still differ by lane (tilth "# Search:" vs rg
	// "# grep:") exactly as they do for every ripgrep.Handles grep today.

	b := router.For(op)

	switch op {
	case backend.OpStructural, backend.OpReplace, backend.OpStructOutline:
		return astgrep.Run(ctx, op, a)
	case backend.OpDiff:
		bin, argv, err := b.CLIArgv(ctx, op, a)
		if err != nil {
			return "", err
		}
		return render.RunDiffCLI(ctx, bin, argv, a.Source, a.Scope, a.Budget)
	case backend.OpOverview:
		bin, argv, err := b.CLIArgv(ctx, op, a)
		if err != nil {
			return "", err
		}
		out, err := render.RunCLI(ctx, bin, argv)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
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
		// On an MCP grok miss, recover via the same ast-grep fallback the CLI uses.
		if op == backend.OpSymbol && grok.IsNotFoundText(text) {
			return grok.FallbackTypeDecl(ctx, a, fmt.Errorf("proxy: tool %q failed: %s", tool, text))
		}
		return "", fmt.Errorf("proxy: tool %q failed: %s", tool, text)
	}
	if op == backend.OpGrep && grep.ZeroMatches(text) {
		switch rechecked, found, rerr := grep.Recheck(ctx, a); {
		case rerr != nil && ctx.Err() != nil:
			return "", fmt.Errorf("proxy: grep zero recheck aborted: %w", ctx.Err())
		case rerr == nil && found:
			return rechecked, nil
		}
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
