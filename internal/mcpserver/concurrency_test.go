package mcpserver

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/proxy"
	"github.com/yasyf/cc-context/internal/workspace"
)

// namedRoot returns the root the "# root" header line names.
func namedRoot(t *testing.T, text string) string {
	t.Helper()
	for line := range strings.SplitSeq(text, "\n") {
		if root, ok := strings.CutPrefix(line, "# root "); ok {
			return root
		}
	}
	t.Fatalf("no root header line in:\n%s", text)
	return ""
}

func TestRootHeaderNamesTheTreeThatAnswered(t *testing.T) {
	answering, _ := writeTree(t, "f.txt", "answering\n")
	other, _ := writeTree(t, "f.txt", "other\n")
	pinRoot(t, answering)

	ctx := pinCall(context.Background())
	a, err := readArgs(ctx, ReadIn{Path: "f.txt"})
	if err != nil {
		t.Fatalf("readArgs: %v", err)
	}
	p := proxy.New()
	t.Cleanup(func() { _ = p.Close() })
	out, err := p.Call(ctx, backend.OpRead, a)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	workspace.SetRoot(other)

	res, _, err := opResult(ctx, backend.OpRead, out)
	if err != nil {
		t.Fatalf("opResult: %v", err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if got := namedRoot(t, text); got != answering {
		t.Errorf("header names %q, want the tree that answered %q:\n%s", got, answering, text)
	}
	if !strings.Contains(text, "answering\n") {
		t.Errorf("content should come from the tree the header names:\n%s", text)
	}
}

func TestConcurrentRepinKeepsEveryHeaderWithItsContent(t *testing.T) {
	first, _ := writeTree(t, "f.txt", "first\n")
	second, _ := writeTree(t, "f.txt", "second\n")
	body := map[string]string{first: "first\n", second: "second\n"}
	pinRoot(t, first)
	cs := connectTestServer(t)

	stop := make(chan struct{})
	var repinner sync.WaitGroup
	repinner.Add(1)
	go func() {
		defer repinner.Done()
		for {
			select {
			case <-stop:
				return
			default:
				workspace.SetRoot(first)
				workspace.SetRoot(second)
			}
		}
	}()

	texts := make([]string, 64)
	var calls sync.WaitGroup
	for i := range texts {
		calls.Add(1)
		go func() {
			defer calls.Done()
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ccx_code_read", Arguments: map[string]any{"path": "f.txt"}})
			if err != nil {
				texts[i] = "CallTool: " + err.Error()
				return
			}
			texts[i] = res.Content[0].(*mcp.TextContent).Text
		}()
	}
	calls.Wait()
	close(stop)
	repinner.Wait()

	for _, text := range texts {
		root := namedRoot(t, text)
		want, ok := body[root]
		if !ok {
			t.Fatalf("header names an unknown root %q:\n%s", root, text)
		}
		if !strings.Contains(text, want) {
			t.Errorf("header names %q but the content came from the other tree:\n%s", root, text)
		}
	}
}
