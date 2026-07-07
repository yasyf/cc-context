package codeexec

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDiscovery serves a fixed inventory and counts probes, for TTL assertions.
type fakeDiscovery struct {
	inv    Inventory
	probes atomic.Int32
}

func (d *fakeDiscovery) discover(context.Context) (Inventory, error) {
	d.probes.Add(1)
	return d.inv, nil
}

func fakeInventory() Inventory {
	return Inventory{
		Hash:    "h1",
		Servers: []ServerSpec{{Name: "fake", Command: "fake-mcp", Prefix: "fake"}},
		Notes:   []string{"probe note"},
	}
}

func newTestEngine(t *testing.T, d *fakeDiscovery) (*Engine, *fakeConnector) {
	t.Helper()
	t.Setenv("CCX_EXEC_MCP", "")
	conn := newFakeConnector()
	conn.servers["fake"] = echoServer()
	e := NewEngine(&fakeCaller{}, NewMemoryStore(), WithDiscover(d.discover), WithConnect(conn.connect))
	t.Cleanup(func() { _ = e.Close() })
	return e, conn
}

func TestEngineExecReflected(t *testing.T) {
	requireUV(t)
	ctx := context.Background()
	d := &fakeDiscovery{inv: fakeInventory()}
	e, conn := newTestEngine(t, d)

	script := "import asyncio\nasyncio.run(fake_echo(text=\"hello\"))"
	out, notes, err := e.Exec(ctx, script, 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if want := `{"text":"hello"}`; out != want {
		t.Errorf("Exec = %q, want %q", out, want)
	}
	if !containsSub(notes, "probe note") {
		t.Errorf("notes = %q, missing inventory note", notes)
	}
	if n := conn.connectCount("fake"); n != 2 {
		t.Errorf("connects = %d, want 2 (catalog probe + resident session)", n)
	}
}

func TestEngineExecUnscannedReflected(t *testing.T) {
	requireUV(t)
	ctx := context.Background()
	d := &fakeDiscovery{inv: fakeInventory()}
	e, conn := newTestEngine(t, d)

	script := "import asyncio\nf = fake_echo\nasyncio.run(f(text=\"hi\"))"
	if got := referenced(script, map[string]string{"fake_echo": "fake"}); len(got) != 0 {
		t.Fatalf("referenced = %v, want none (aliased call must evade the scan)", got)
	}
	out, _, err := e.Exec(ctx, script, 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if want := `{"text":"hi"}`; out != want {
		t.Errorf("Exec = %q, want %q", out, want)
	}
	if n := conn.connectCount("fake"); n != 2 {
		t.Errorf("connects = %d, want 2 (catalog probe + lazy resident, no pre-warm)", n)
	}
}

func TestEngineOff(t *testing.T) {
	requireUV(t)
	ctx := context.Background()
	d := &fakeDiscovery{inv: fakeInventory()}
	e, conn := newTestEngine(t, d)
	t.Setenv("CCX_EXEC_MCP", "off")

	out, notes, err := e.Exec(ctx, "40 + 2", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "42" || len(notes) != 0 {
		t.Errorf("Exec = %q notes %q, want 42 and no notes", out, notes)
	}
	if _, _, err := e.Exec(ctx, "import asyncio\nasyncio.run(fake_echo(text=\"x\"))", 0); err == nil {
		t.Error("reflected call with CCX_EXEC_MCP=off = nil error, want typecheck failure")
	}
	preamble, _, err := e.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if strings.Contains(preamble, "fake_echo") {
		t.Error("Tools preamble lists reflected tools despite CCX_EXEC_MCP=off")
	}
	if d.probes.Load() != 0 || conn.connectCount("fake") != 0 {
		t.Errorf("probes = %d connects = %d, want 0 and 0", d.probes.Load(), conn.connectCount("fake"))
	}
}

func TestEngineEmptyInventory(t *testing.T) {
	requireUV(t)
	ctx := context.Background()
	d := &fakeDiscovery{inv: Inventory{Notes: []string{"mcp reflection unavailable: claude not on PATH"}}}
	e, conn := newTestEngine(t, d)

	out, notes, err := e.Exec(ctx, "40 + 2", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "42" {
		t.Errorf("Exec = %q, want 42", out)
	}
	if !containsSub(notes, "claude not on PATH") {
		t.Errorf("notes = %q, missing discovery note", notes)
	}
	if n := conn.connectCount("fake"); n != 0 {
		t.Errorf("connects = %d, want 0", n)
	}
}

func TestEngineInventoryTTL(t *testing.T) {
	requireUV(t)
	ctx := context.Background()
	d := &fakeDiscovery{inv: fakeInventory()}
	e, _ := newTestEngine(t, d)

	for range 2 {
		if _, _, err := e.Exec(ctx, "1 + 1", 0); err != nil {
			t.Fatalf("Exec: %v", err)
		}
	}
	if n := d.probes.Load(); n != 1 {
		t.Fatalf("probes within TTL = %d, want 1", n)
	}

	e.mu.Lock()
	e.probed = time.Now().Add(-inventoryTTL - time.Second)
	e.mu.Unlock()
	if _, _, err := e.Exec(ctx, "1 + 1", 0); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if n := d.probes.Load(); n != 2 {
		t.Errorf("probes after TTL expiry = %d, want 2", n)
	}
}

func TestEngineToolsPreamble(t *testing.T) {
	ctx := context.Background()
	d := &fakeDiscovery{inv: fakeInventory()}
	e, _ := newTestEngine(t, d)

	preamble, notes, err := e.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	for _, want := range []string{
		"fake_echo(text=...)",
		"fake_boom() [writes]",
		"[fake]",
		"sh(cmd)",
		"Reflected MCP tools (this session):",
	} {
		if !strings.Contains(preamble, want) {
			t.Errorf("preamble missing %q", want)
		}
	}
	if !containsSub(notes, "probe note") {
		t.Errorf("notes = %q, missing inventory note", notes)
	}
	if !containsSub(notes, "reflected fake as a fresh instance") {
		t.Errorf("notes = %q, missing classifier fresh-instance note", notes)
	}
}

func TestMergeBuiltinWins(t *testing.T) {
	marker := func(out string) HostFunc {
		return func(context.Context, Call) (any, error) { return out, nil }
	}
	builtin := map[string]HostFunc{"read": marker("builtin")}
	reflected := map[string]HostFunc{"read": marker("reflected"), "fake_x": marker("fake_x")}

	merged, notes := merge(builtin, reflected)
	if len(merged) != 2 {
		t.Fatalf("merged has %d entries, want 2", len(merged))
	}
	val, err := merged["read"](context.Background(), Call{})
	if err != nil {
		t.Fatalf("merged read: %v", err)
	}
	if got := val.(string); got != "builtin" {
		t.Errorf("merged read = %q, want the builtin", got)
	}
	if _, ok := merged["fake_x"]; !ok {
		t.Error("non-colliding reflected tool missing from merge")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "reflected tool read is shadowed") {
		t.Errorf("notes = %q, want one shadowing note for read", notes)
	}
}
