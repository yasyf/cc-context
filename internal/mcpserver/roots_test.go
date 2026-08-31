package mcpserver

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yasyf/cc-context/internal/workspace"
)

func TestFirstLocalRoot(t *testing.T) {
	tests := []struct {
		name  string
		roots []*mcp.Root
		want  string
	}{
		{"none", nil, ""},
		{"file uri", []*mcp.Root{{URI: "file:///Users/x/repo"}}, "/Users/x/repo"},
		{"first of many", []*mcp.Root{{URI: "file:///private/tmp/p"}, {URI: "file:///tmp/p"}}, "/private/tmp/p"},
		{"skips non-file", []*mcp.Root{{URI: "https://example.com/repo"}, {URI: "file:///Users/x/repo"}}, "/Users/x/repo"},
		{"skips unparseable", []*mcp.Root{{URI: "://nope"}, {URI: "file:///Users/x/repo"}}, "/Users/x/repo"},
		{"skips pathless", []*mcp.Root{{URI: "file://"}, {URI: "file:///Users/x/repo"}}, "/Users/x/repo"},
		{"percent decoded", []*mcp.Root{{URI: "file:///Users/x/my%20repo"}}, "/Users/x/my repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstLocalRoot(tt.roots); got != tt.want {
				t.Errorf("firstLocalRoot = %q, want %q", got, tt.want)
			}
		})
	}
}

func lister(calls *int, roots []*mcp.Root, err error) rootLister {
	return func(context.Context) ([]*mcp.Root, error) {
		*calls++
		return roots, err
	}
}

func TestRootTrackerSyncPinsFirstRootOnce(t *testing.T) {
	t.Cleanup(func() { workspace.SetRoot("") })
	calls := 0
	tracker := newRootTracker()
	roots := lister(&calls, []*mcp.Root{{URI: "file:///Users/x/repo"}, {URI: "file:///Users/x/other"}}, nil)

	tracker.sync(context.Background(), roots)
	tracker.sync(context.Background(), roots)

	if calls != 1 {
		t.Errorf("roots/list issued %d times, want 1 (the resolution is memoized)", calls)
	}
	if got, err := workspace.Root(); err != nil || got != "/Users/x/repo" {
		t.Errorf("workspace.Root() = %q, %v, want /Users/x/repo", got, err)
	}
}

func TestRootTrackerRearmReResolves(t *testing.T) {
	t.Cleanup(func() { workspace.SetRoot("") })
	tracker := newRootTracker()
	calls := 0
	tracker.sync(context.Background(), lister(&calls, []*mcp.Root{{URI: "file:///Users/x/repo"}}, nil))

	tracker.rearm(context.Background(), &mcp.RootsListChangedRequest{})
	tracker.sync(context.Background(), lister(&calls, []*mcp.Root{{URI: "file:///Users/x/worktree"}}, nil))

	if calls != 2 {
		t.Errorf("roots/list issued %d times, want 2 (list_changed re-arms the resolution)", calls)
	}
	if got, _ := workspace.Root(); got != "/Users/x/worktree" {
		t.Errorf("workspace.Root() = %q, want /Users/x/worktree", got)
	}
}

func TestRootTrackerNoRootsLeavesCwd(t *testing.T) {
	t.Cleanup(func() { workspace.SetRoot("") })
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	calls := 0
	newRootTracker().sync(context.Background(), lister(&calls, nil, nil))

	if got, err := workspace.Root(); err != nil || got != cwd {
		t.Errorf("workspace.Root() = %q, %v, want the working directory %q", got, err, cwd)
	}
}

func TestRootTrackerListErrorKeepsPinAndStaysArmed(t *testing.T) {
	t.Cleanup(func() { workspace.SetRoot("") })
	workspace.SetRoot("/Users/x/repo")
	calls := 0
	flaky := func(context.Context) ([]*mcp.Root, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("no roots for you")
		}
		return []*mcp.Root{{URI: "file:///Users/x/worktree"}}, nil
	}
	tracker := newRootTracker()

	tracker.sync(context.Background(), flaky)
	if got, _ := workspace.Root(); got != "/Users/x/repo" {
		t.Errorf("workspace.Root() = %q, want the pin untouched at /Users/x/repo", got)
	}

	tracker.sync(context.Background(), flaky)
	if calls != 2 {
		t.Errorf("roots/list issued %d times, want 2 (a failed resolution stays armed)", calls)
	}
	if got, _ := workspace.Root(); got != "/Users/x/worktree" {
		t.Errorf("workspace.Root() = %q, want /Users/x/worktree once roots/list succeeds", got)
	}
}

func connectRootsServer(t *testing.T, client *mcp.Client) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	roots := newRootTracker()
	s := mcp.NewServer(&mcp.Implementation{Name: "cc-context-test", Version: "test"}, &mcp.ServerOptions{
		RootsListChangedHandler: roots.rearm,
	})
	s.AddReceivingMiddleware(roots.middleware)
	mcp.AddTool(s, &mcp.Tool{Name: "probe", Description: "no-op"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})

	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callProbe(t *testing.T, cs *mcp.ClientSession) {
	t.Helper()
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "probe"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
}

func TestServerAdoptsClientRoot(t *testing.T) {
	t.Cleanup(func() { workspace.SetRoot("") })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	client.AddRoots(&mcp.Root{URI: "file:///Users/x/repo"})
	cs := connectRootsServer(t, client)

	callProbe(t, cs)
	if got, _ := workspace.Root(); got != "/Users/x/repo" {
		t.Fatalf("workspace.Root() = %q, want /Users/x/repo", got)
	}

	client.RemoveRoots("file:///Users/x/repo")
	client.AddRoots(&mcp.Root{URI: "file:///Users/x/worktree"})
	callProbe(t, cs)
	if got, _ := workspace.Root(); got != "/Users/x/worktree" {
		t.Errorf("workspace.Root() = %q, want /Users/x/worktree after roots/list_changed", got)
	}
}

func TestServerLeavesRootUnsetWithoutRootsCapability(t *testing.T) {
	t.Cleanup(func() { workspace.SetRoot("") })
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{},
	})
	client.AddRoots(&mcp.Root{URI: "file:///Users/x/repo"})

	callProbe(t, connectRootsServer(t, client))

	if got, _ := workspace.Root(); got != cwd {
		t.Errorf("workspace.Root() = %q, want the working directory %q", got, cwd)
	}
}
