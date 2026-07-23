// Package dispatch runs every native ccx op in-process, collapsing what the CLI
// (cli.dispatch) and the MCP proxy (proxy.call) each did per-op into one switch.
// Native reports whether an op runs here — now every op, including the semantic
// search/related ops, which run the in-process semsearch engine instead of a
// semble subprocess. Run executes an op with its exact finalize/cap/binary-skip
// semantics.
package dispatch

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/astgrep"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/deps"
	"github.com/yasyf/cc-context/internal/diff"
	"github.com/yasyf/cc-context/internal/edit"
	"github.com/yasyf/cc-context/internal/find"
	"github.com/yasyf/cc-context/internal/outline"
	"github.com/yasyf/cc-context/internal/overview"
	"github.com/yasyf/cc-context/internal/read"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/ripgrep"
	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/embed"
	"github.com/yasyf/cc-context/internal/semsearch/engine"
	"github.com/yasyf/cc-context/internal/semsearch/index"
	"github.com/yasyf/cc-context/internal/symbol"
	"github.com/yasyf/cc-context/internal/web"
)

// Native reports whether op runs in-process here. Every op is native — the
// semantic ops search and related now run the in-process semsearch engine.
func Native(backend.Op) bool { return true }

// Run executes a native op in-process and returns its budget-capped output,
// preserving each op's finalize/cap/binary-skip semantics. It is the single
// dispatch shared by cli.dispatch and proxy.call; an unknown op fails loudly.
func Run(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
	if op == backend.OpOutline || op == backend.OpStructOutline {
		// Skip a binary target before dispatch, so a forced --lang still skips.
		// BinarySkip stats internally and no-ops on a dir or text file.
		if line, skipped := outline.BinarySkip(a.Path); skipped {
			return line, nil
		}
	}

	switch op {
	case backend.OpSearch, backend.OpRelated:
		return runSemantic(ctx, op, a)
	case backend.OpEdit:
		return edit.Run(a)
	case backend.OpStructural, backend.OpReplace, backend.OpStructOutline:
		return astgrep.Run(ctx, op, a)
	case backend.OpWebOutline, backend.OpWebRead, backend.OpWebSearch:
		out, err := web.Run(ctx, op, a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	case backend.OpFind:
		out, err := find.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	case backend.OpRead:
		out, err := read.Run(a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	case backend.OpOverview:
		out, err := overview.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	case backend.OpGrep:
		return ripgrep.Run(ctx, a)
	case backend.OpOutline:
		// The native fallback anchors its own output; cap only, never re-anchor.
		out, err := outline.Fallback(a.Path, a)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	case backend.OpDiff:
		// The diff renderer anchors its own output; cap only, never render.Finalize.
		out, err := diff.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	case backend.OpSymbol:
		// symbol self-anchors; cap only, never render.Finalize's symbol grammar.
		out, err := symbol.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	case backend.OpDeps:
		out, err := deps.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	default:
		return "", fmt.Errorf("dispatch: op %q is not native", op)
	}
}

// runSemantic runs the native semsearch engine for search/related: it embeds
// against the resident model2vec engine, ranks, then anchors and caps the result
// span list, appending the weak-match and slow-search notes after the cap so
// neither is truncated away.
func runSemantic(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
	// Time from before the embedder is constructed: a cold first request pays the
	// weight download, WASM compile, and model load, and the slow-search note must
	// reflect that latency rather than excluding it.
	start := time.Now()
	emb, err := sharedEmbedder(ctx)
	if err != nil {
		return "", err
	}
	repo := a.Path
	if repo == "" {
		if repo, err = os.Getwd(); err != nil {
			return "", fmt.Errorf("dispatch: resolve cwd: %w", err)
		}
	}

	var results []semsearch.Result
	if op == backend.OpSearch {
		results, err = engine.Search(ctx, emb, a)
	} else {
		results, err = engine.Related(ctx, emb, a)
	}
	if err != nil {
		return "", err
	}
	elapsed := time.Since(start)

	out := render.SearchResults(op, results, anchor.NewFiles(repo))
	out = render.Cap(out, a.Budget)
	out = render.WithWeakResultsNote(out, results)
	return render.WithSlowSearchNote(out, elapsed), nil
}

// embedder state: one resident model2vec engine per process, shared by every
// semantic op. newEmbedder is a var so tests inject a fake instead of the WASM
// engine.
var (
	embMu       sync.Mutex
	embEngine   index.Embedder
	newEmbedder = func(ctx context.Context) (index.Embedder, error) { return embed.New(ctx) }
)

// sharedEmbedder returns the process's resident embedder, constructing it on
// first use. A failed construction is not cached, so a later call retries.
func sharedEmbedder(ctx context.Context) (index.Embedder, error) {
	embMu.Lock()
	defer embMu.Unlock()
	if embEngine != nil {
		return embEngine, nil
	}
	e, err := newEmbedder(ctx)
	if err != nil {
		return nil, err
	}
	embEngine = e
	return e, nil
}

// CloseIndexCache frees the engine's resident index cache. The MCP proxy calls
// it on shutdown alongside CloseEmbedder; a one-shot CLI relies on process exit.
func CloseIndexCache() { engine.CloseIndexCache() }

// CloseEmbedder releases the resident embedder if one was constructed. The MCP
// proxy calls it on shutdown; a one-shot CLI relies on process exit.
func CloseEmbedder(ctx context.Context) error {
	embMu.Lock()
	defer embMu.Unlock()
	if embEngine == nil {
		return nil
	}
	closer, ok := embEngine.(interface {
		Close(context.Context) error
	})
	embEngine = nil
	if !ok {
		return nil
	}
	return closer.Close(ctx)
}

// SetEmbedderProvider overrides the process embedder provider and clears the
// cached engine, returning a restore func. It is a test seam for injecting a
// fake embedder in place of the resident WASM engine; not for production use.
func SetEmbedderProvider(p func(context.Context) (index.Embedder, error)) func() {
	embMu.Lock()
	defer embMu.Unlock()
	prevProvider, prevEngine := newEmbedder, embEngine
	newEmbedder, embEngine = p, nil
	return func() {
		embMu.Lock()
		defer embMu.Unlock()
		newEmbedder, embEngine = prevProvider, prevEngine
	}
}
