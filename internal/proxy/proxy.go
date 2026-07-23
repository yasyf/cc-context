// Package proxy fronts the facade tools: it runs every ccx op in-process via
// internal/dispatch, including the semantic search/related ops now that they no
// longer need a resident semble MCP session.
package proxy

import (
	"context"
	"errors"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/dispatch"
	"github.com/yasyf/cc-context/internal/web"
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

// Close frees the resident index cache and releases both resident embedders (the
// code engine in dispatch and the web-search engine in web) if the process opened
// them.
func (p *Proxy) Close() error {
	dispatch.CloseIndexCache()
	return errors.Join(
		dispatch.CloseEmbedder(context.Background()),
		web.CloseEmbedder(context.Background()),
	)
}
