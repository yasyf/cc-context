package codeexec

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeConnector dials in-memory MCP servers by spec name, counting connects
// and retaining the client sessions it handed out. Names in fail error
// immediately; names in hang block until the dial context expires.
type fakeConnector struct {
	mu       sync.Mutex
	servers  map[string]*mcp.Server
	fail     map[string]bool
	hang     map[string]bool
	connects map[string]int
	sessions map[string][]*mcp.ClientSession
}

func newFakeConnector() *fakeConnector {
	return &fakeConnector{
		servers:  map[string]*mcp.Server{},
		fail:     map[string]bool{},
		hang:     map[string]bool{},
		connects: map[string]int{},
		sessions: map[string][]*mcp.ClientSession{},
	}
}

func (f *fakeConnector) connect(ctx context.Context, spec ServerSpec) (*mcp.ClientSession, error) {
	f.mu.Lock()
	f.connects[spec.Name]++
	server, fail, hang := f.servers[spec.Name], f.fail[spec.Name], f.hang[spec.Name]
	f.mu.Unlock()
	if fail {
		return nil, errors.New("dial refused")
	}
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if server == nil {
		return nil, fmt.Errorf("no fake server %q", spec.Name)
	}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "codeexec-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.sessions[spec.Name] = append(f.sessions[spec.Name], session)
	f.mu.Unlock()
	return session, nil
}

func (f *fakeConnector) connectCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connects[name]
}

func (f *fakeConnector) session(t *testing.T, name string, i int) *mcp.ClientSession {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions[name]) <= i {
		t.Fatalf("no session %d for %q (have %d)", i, name, len(f.sessions[name]))
	}
	return f.sessions[name][i]
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func objSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// echoServer serves an echo tool that returns its raw arguments JSON and a
// boom tool that always fails with IsError.
func echoServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "test"}, nil)
	s.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "Echoes the arguments back as JSON.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: objSchema(map[string]any{"text": map[string]any{"type": "string"}}, "text"),
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return textResult(string(req.Params.Arguments)), nil
	})
	s.AddTool(&mcp.Tool{
		Name:        "boom",
		Description: "Always fails.",
		InputSchema: objSchema(map[string]any{}),
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res := textResult("kaboom")
		res.IsError = true
		return res, nil
	})
	return s
}

// storeServer serves a read-only get-item and a destructive delete-item, for
// asserting ToolInfo extraction.
func storeServer() *mcp.Server {
	destructive := true
	s := mcp.NewServer(&mcp.Implementation{Name: "healthy", Version: "test"}, nil)
	s.AddTool(&mcp.Tool{
		Name:        "get-item",
		Description: "Gets an item from the store. The second sentence must be dropped.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: objSchema(map[string]any{
			"id":      map[string]any{"type": "string"},
			"verbose": map[string]any{"type": "boolean"},
			"scope":   map[string]any{"type": "string"},
		}, "id"),
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return textResult("item"), nil
	})
	s.AddTool(&mcp.Tool{
		Name:        "delete-item",
		Description: "Deletes an item permanently.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive},
		InputSchema: objSchema(map[string]any{"id": map[string]any{"type": "string"}}, "id"),
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return textResult("gone"), nil
	})
	return s
}

func sampleCatalog(hash string) *Catalog {
	destructive := true
	return &Catalog{
		Hash:  hash,
		Built: time.Now().UTC(),
		Servers: []ServerCatalog{{
			Spec: ServerSpec{Name: "healthy", Command: "healthy-mcp", Argv: []string{"serve"}, Prefix: "healthy"},
			Tools: []ToolInfo{
				{Server: "healthy", Tool: "get-item", FuncName: "healthy_get_item", Params: []string{"id", "scope"}, Summary: "Gets.", ReadOnly: true},
				{Server: "healthy", Tool: "delete-item", FuncName: "healthy_delete_item", Params: []string{"id"}, Summary: "Deletes.", Destructive: &destructive},
			},
			Note: "reflected healthy as a fresh instance",
		}},
	}
}

func TestStoreRoundtrip(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	disk, err := NewDiskStore()
	if err != nil {
		t.Fatalf("NewDiskStore: %v", err)
	}
	tests := []struct {
		name  string
		store CatalogStore
	}{
		{"memory", NewMemoryStore()},
		{"disk", disk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := tt.store.Load(); ok {
				t.Fatal("Load on empty store = true, want miss")
			}
			cat := sampleCatalog("h1")
			if err := tt.store.Save(cat); err != nil {
				t.Fatalf("Save: %v", err)
			}
			got, ok := tt.store.Load()
			if !ok {
				t.Fatal("Load after Save = miss")
			}
			if got.Hash != cat.Hash || !got.Built.Equal(cat.Built) {
				t.Errorf("Load = hash %q built %v, want hash %q built %v", got.Hash, got.Built, cat.Hash, cat.Built)
			}
			if !reflect.DeepEqual(got.Servers, cat.Servers) {
				t.Errorf("Load servers = %+v, want %+v", got.Servers, cat.Servers)
			}
		})
	}
}

func TestResolveCatalogHashHitSpawnsNothing(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	cached := sampleCatalog("live")
	if err := store.Save(cached); err != nil {
		t.Fatalf("Save: %v", err)
	}
	conn := newFakeConnector()

	got, err := resolveCatalog(ctx, store, Inventory{Hash: "live", Servers: cached.serverSpecs()}, conn.connect)
	if err != nil {
		t.Fatalf("resolveCatalog: %v", err)
	}
	if got.Hash != "live" {
		t.Errorf("Hash = %q, want live", got.Hash)
	}
	if n := conn.connectCount("healthy"); n != 0 {
		t.Errorf("hash hit spawned %d connects, want 0", n)
	}
}

func TestResolveCatalogStaleHashRebuilds(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Save(sampleCatalog("stale")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	conn := newFakeConnector()
	conn.servers["healthy"] = storeServer()
	inv := Inventory{Hash: "fresh", Servers: []ServerSpec{{Name: "healthy", Command: "healthy-mcp", Prefix: "healthy"}}}

	got, err := resolveCatalog(ctx, store, inv, conn.connect)
	if err != nil {
		t.Fatalf("resolveCatalog: %v", err)
	}
	if got.Hash != "fresh" {
		t.Errorf("Hash = %q, want fresh", got.Hash)
	}
	if n := conn.connectCount("healthy"); n != 1 {
		t.Errorf("connects = %d, want 1", n)
	}
	saved, ok := store.Load()
	if !ok || saved.Hash != "fresh" {
		t.Errorf("store after rebuild = %+v, want saved fresh catalog", saved)
	}
}

func TestResolveCatalogConcurrentBuildOnce(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	conn := newFakeConnector()
	conn.servers["healthy"] = storeServer()
	inv := Inventory{Hash: "fresh", Servers: []ServerSpec{{Name: "healthy", Command: "healthy-mcp", Prefix: "healthy"}}}

	const resolvers = 4
	var wg sync.WaitGroup
	cats := make([]*Catalog, resolvers)
	errs := make([]error, resolvers)
	for i := range resolvers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cats[i], errs[i] = resolveCatalog(ctx, store, inv, conn.connect)
		}()
	}
	wg.Wait()

	for i := range resolvers {
		if errs[i] != nil {
			t.Fatalf("resolver %d: %v", i, errs[i])
		}
		if cats[i].Hash != "fresh" {
			t.Errorf("resolver %d hash = %q, want fresh", i, cats[i].Hash)
		}
	}
	if n := conn.connectCount("healthy"); n != 1 {
		t.Errorf("concurrent resolves connected %d times, want 1 (losers must re-Load)", n)
	}
}

func TestBuildCatalog(t *testing.T) {
	ctx := context.Background()
	conn := newFakeConnector()
	conn.servers["healthy"] = storeServer()
	conn.fail["broken"] = true
	conn.hang["hang"] = true
	inv := Inventory{Hash: "h", Servers: []ServerSpec{
		{Name: "broken", Command: "broken-mcp", Prefix: "broken"},
		{Name: "hang", Command: "hang-mcp", Prefix: "hang"},
		{Name: "healthy", Command: "healthy-mcp", Prefix: "healthy"},
	}}

	cat := buildCatalog(ctx, inv, conn.connect, 200*time.Millisecond)
	if cat.Hash != "h" || len(cat.Servers) != 3 {
		t.Fatalf("catalog = hash %q, %d servers; want h, 3", cat.Hash, len(cat.Servers))
	}

	broken := cat.Servers[0]
	if len(broken.Tools) != 0 || !strings.Contains(broken.Note, "connect broken") {
		t.Errorf("broken = %d tools, note %q; want 0 tools and a connect note", len(broken.Tools), broken.Note)
	}
	hang := cat.Servers[1]
	if len(hang.Tools) != 0 || !strings.Contains(hang.Note, "context deadline exceeded") {
		t.Errorf("hang = %d tools, note %q; want 0 tools and a timeout note", len(hang.Tools), hang.Note)
	}

	healthy := cat.Servers[2]
	destructive := true
	wantTools := map[string]ToolInfo{
		"healthy_get_item": {
			Server: "healthy", Tool: "get-item", FuncName: "healthy_get_item",
			Params:   []string{"id", "scope", "verbose"},
			Summary:  "Gets an item from the store.",
			ReadOnly: true,
		},
		"healthy_delete_item": {
			Server: "healthy", Tool: "delete-item", FuncName: "healthy_delete_item",
			Params:      []string{"id"},
			Summary:     "Deletes an item permanently.",
			Destructive: &destructive,
		},
	}
	if len(healthy.Tools) != len(wantTools) {
		t.Fatalf("healthy tools = %+v, want %d tools", healthy.Tools, len(wantTools))
	}
	for _, tool := range healthy.Tools {
		want, ok := wantTools[tool.FuncName]
		if !ok {
			t.Errorf("unexpected tool %q", tool.FuncName)
			continue
		}
		if tool.Destructive != nil && want.Destructive != nil {
			if *tool.Destructive != *want.Destructive {
				t.Errorf("%s Destructive = %v, want %v", tool.FuncName, *tool.Destructive, *want.Destructive)
			}
			tool.Destructive, want.Destructive = nil, nil
		}
		if !reflect.DeepEqual(tool, want) {
			t.Errorf("tool = %+v, want %+v", tool, want)
		}
	}
	wantNote := "reflected healthy as a fresh instance — if it needs live session state, set CCX_EXEC_MCP_DENY=healthy"
	if healthy.Note != wantNote {
		t.Errorf("healthy note = %q, want %q", healthy.Note, wantNote)
	}
}

func TestClassify(t *testing.T) {
	open := true
	stdio := ServerSpec{Name: "s", Command: "s-mcp"}
	remote := ServerSpec{Name: "s", URL: "https://example.com/mcp"}
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	openWorld := &mcp.ToolAnnotations{OpenWorldHint: &open}
	toolsWith := func(anns ...*mcp.ToolAnnotations) []*mcp.Tool {
		tools := make([]*mcp.Tool, len(anns))
		for i, a := range anns {
			tools[i] = &mcp.Tool{Name: fmt.Sprintf("t%d", i), Annotations: a}
		}
		return tools
	}
	warning := "reflected s as a fresh instance — if it needs live session state, set CCX_EXEC_MCP_DENY=s"

	tests := []struct {
		name  string
		spec  ServerSpec
		tools []*mcp.Tool
		want  string
	}{
		{"all read-only", stdio, toolsWith(readOnly, readOnly), ""},
		{"predominantly open-world", stdio, toolsWith(openWorld, openWorld, nil), ""},
		{"mixed stateful-looking", stdio, toolsWith(readOnly, nil), warning},
		{"no annotations at all", stdio, toolsWith(nil, nil), warning},
		{"url server", remote, toolsWith(nil), ""},
		{"no tools", stdio, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.spec, tt.tools); got != tt.want {
				t.Errorf("classify() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFirstSentence(t *testing.T) {
	tests := []struct {
		name string
		desc string
		want string
	}{
		{"single sentence", "Reads a file.", "Reads a file."},
		{"first of two", "Reads a file. Then more.", "Reads a file."},
		{"newline cut", "Reads a file\nwith details", "Reads a file"},
		{"empty", "", ""},
		{"long truncated", strings.Repeat("very long words ", 20), strings.TrimSpace(strings.Repeat("very long words ", 20)[:119]) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstSentence(tt.desc); got != tt.want {
				t.Errorf("firstSentence(%q) = %q, want %q", tt.desc, got, tt.want)
			}
		})
	}
}

func TestSchemaParams(t *testing.T) {
	tests := []struct {
		name   string
		schema any
		want   []string
	}{
		{
			"required first then optional, each sorted",
			objSchema(map[string]any{"z": 1, "a": 1, "m": 1, "b": 1}, "z", "m"),
			[]string{"m", "z", "a", "b"},
		},
		{"no properties", map[string]any{"type": "object"}, nil},
		{"required missing from properties ignored", objSchema(map[string]any{"a": 1}, "ghost"), []string{"a"}},
		{"unmarshalable schema", "not-an-object", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := schemaParams(tt.schema); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("schemaParams() = %v, want %v", got, tt.want)
			}
		})
	}
}

// serverSpecs projects a catalog back to its inventory specs for tests.
func (c *Catalog) serverSpecs() []ServerSpec {
	specs := make([]ServerSpec, len(c.Servers))
	for i, sc := range c.Servers {
		specs[i] = sc.Spec
	}
	return specs
}
