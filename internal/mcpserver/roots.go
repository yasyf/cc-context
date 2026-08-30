package mcpserver

import (
	"context"
	"net/url"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yasyf/cc-context/internal/workspace"
)

type rootLister func(context.Context) ([]*mcp.Root, error)

// rootTracker pins the project root to the one its client declares. roots/list
// is a server->client request, so it cannot be issued from initialize: the
// tracker defers it to the first tool call and re-arms on roots/list_changed.
type rootTracker struct {
	mu      sync.Mutex
	pending bool
}

func newRootTracker() *rootTracker { return &rootTracker{pending: true} }

func (t *rootTracker) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if call, ok := req.(*mcp.CallToolRequest); ok {
			t.sync(ctx, sessionRoots(call.Session))
		}
		return next(ctx, method, req)
	}
}

func (t *rootTracker) rearm(context.Context, *mcp.RootsListChangedRequest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending = true
}

func (t *rootTracker) sync(ctx context.Context, roots rootLister) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.pending {
		return
	}
	list, err := roots(ctx)
	if err != nil {
		return
	}
	t.pending = false
	workspace.SetRoot(firstLocalRoot(list))
}

func sessionRoots(ss *mcp.ServerSession) rootLister {
	return func(ctx context.Context) ([]*mcp.Root, error) {
		caps := ss.InitializeParams().Capabilities
		if caps == nil || caps.RootsV2 == nil {
			return nil, nil
		}
		res, err := ss.ListRoots(ctx, nil)
		if err != nil {
			return nil, err
		}
		return res.Roots, nil
	}
}

func firstLocalRoot(roots []*mcp.Root) string {
	for _, root := range roots {
		u, err := url.Parse(root.URI)
		if err != nil || u.Scheme != "file" || u.Path == "" {
			continue
		}
		return u.Path
	}
	return ""
}
