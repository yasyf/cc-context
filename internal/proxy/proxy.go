// Package proxy fronts the facade tools: it runs every ccx op in-process via
// internal/dispatch, including the semantic search/related ops now that they no
// longer need a resident semble MCP session.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/dispatch"
	"github.com/yasyf/cc-context/internal/web"
)

const (
	idleTTL       = 15 * time.Minute
	sweepInterval = time.Minute
)

// Proxy fronts the op surface: every op runs in-process via internal/dispatch.
// It carries no engine state — the resident model2vec engine behind the semantic
// ops lives in dispatch and is released by Close.
type Proxy struct{}

// New returns a proxy; the resident embedder connects lazily on first semantic
// call, inside dispatch.
func New() *Proxy { return &Proxy{} }

// Call resolves content anchors in a to plain line numbers, dispatches op, and
// prepends the anchor-move note after capping so the note is never truncated away.
func (p *Proxy) Call(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
	a, note, err := anchor.RewriteArgs(op, a)
	if err != nil {
		return "", err
	}
	out, err := dispatch.Run(ctx, op, a)
	if err != nil {
		return "", err
	}
	return note + out, nil
}

// StartIdleSweeper prunes the on-disk index cache once, then runs SweepIdle every
// sweepInterval until ctx is cancelled. The MCP server starts it so a long-lived
// process gives back what it has stopped using; the one-shot CLI never calls it.
func (p *Proxy) StartIdleSweeper(ctx context.Context) {
	go func() {
		dispatch.PruneIndexCache(ctx)
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				if err := p.SweepIdle(ctx, now.Add(-idleTTL)); err != nil {
					slog.Warn("proxy: sweep idle engines", "err", err)
				}
			}
		}
	}()
}

// SweepIdle drops the resident indexes and releases the embedders unused since
// cutoff, then returns the freed pages to the OS. Go's scavenger reclaims a
// several hundred MB index only over many minutes otherwise, so the release would
// not show up as memory relief without the explicit call.
func (p *Proxy) SweepIdle(ctx context.Context, cutoff time.Time) error {
	code, codeErr := dispatch.SweepIdle(ctx, cutoff)
	page, pageErr := web.SweepIdle(ctx, cutoff)
	if code || page {
		debug.FreeOSMemory()
	}
	return errors.Join(codeErr, pageErr)
}

// Close frees the resident index cache and releases both resident embedders (the
// code engine in dispatch and the web-search engine in web) if the process opened
// them, resolving process state off ctx.
func (p *Proxy) Close(ctx context.Context) error {
	dispatch.CloseIndexCache()
	return errors.Join(
		dispatch.CloseEmbedder(ctx),
		web.CloseEmbedder(ctx),
	)
}
