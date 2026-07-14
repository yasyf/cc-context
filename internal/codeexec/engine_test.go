package codeexec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDiscovery serves a fixed inventory and counts probes, for TTL and gate
// assertions. A non-nil err makes every probe fail, exercising the stale
// fallback and degrade paths.
type fakeDiscovery struct {
	inv    Inventory
	err    error
	probes atomic.Int32
}

func (d *fakeDiscovery) discover(context.Context) (Inventory, error) {
	d.probes.Add(1)
	if d.err != nil {
		return Inventory{}, d.err
	}
	return d.inv, nil
}

func fakeInventory() Inventory {
	return Inventory{
		Hash:    "h1",
		Servers: []ServerSpec{{Name: "fake", Command: "fake-mcp", Prefix: "fake"}},
		Notes:   []string{"probe note"},
	}
}

// discoverer is the discovery seam a test engine drives — satisfied by both
// fakeDiscovery and blockingDiscovery.
type discoverer interface {
	discover(context.Context) (Inventory, error)
}

func newTestEngine(t *testing.T, d discoverer, opts ...Option) (*Engine, *fakeConnector) {
	t.Helper()
	t.Setenv("CCX_EXEC_MCP", "")
	conn := newFakeConnector()
	conn.servers["fake"] = echoServer()
	opts = append([]Option{WithDiscover(d.discover), WithConnect(conn.connect)}, opts...)
	e := NewEngine(&fakeCaller{}, NewMemoryStore(), opts...)
	t.Cleanup(func() { _ = e.Close() })
	return e, conn
}

// seedInventory returns a memory inventory store already holding fakeInventory
// probed at probedAt, for gate and freshness assertions.
func seedInventory(t *testing.T, inv Inventory, probedAt time.Time) InventoryStore {
	t.Helper()
	store := NewMemoryInventoryStore()
	if err := store.Save(inv, probedAt); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	return store
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

// TestEngineEmptyInventory pins a successful probe that found zero servers (all
// filtered): Tools is ungated and surfaces the skip note, while a non-referencing
// Exec is gated silently to builtins.
func TestEngineEmptyInventory(t *testing.T) {
	requireUV(t)
	ctx := context.Background()
	d := &fakeDiscovery{inv: Inventory{Notes: []string{"skipped railway: denied by CCX_EXEC_MCP_DENY"}}}
	e, conn := newTestEngine(t, d)

	_, notes, err := e.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if !containsSub(notes, "denied by CCX_EXEC_MCP_DENY") {
		t.Errorf("Tools notes = %q, missing inventory note", notes)
	}

	out, execNotes, err := e.Exec(ctx, "40 + 2", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "42" {
		t.Errorf("Exec = %q, want 42", out)
	}
	if len(execNotes) != 0 {
		t.Errorf("Exec notes = %q, want none (gated to builtins)", execNotes)
	}
	if n := conn.connectCount("fake"); n != 0 {
		t.Errorf("connects = %d, want 0", n)
	}
}

// TestEngineInventoryTTL drives the freshness window through Tools (ungated so
// the gate never masks the TTL) with an injected clock: two calls within the
// window probe once, a call past it re-probes.
func TestEngineInventoryTTL(t *testing.T) {
	ctx := context.Background()
	d := &fakeDiscovery{inv: fakeInventory()}
	base := time.Now()
	now := base
	e, _ := newTestEngine(t, d)
	e.now = func() time.Time { return now }

	for range 2 {
		if _, _, err := e.Tools(ctx); err != nil {
			t.Fatalf("Tools: %v", err)
		}
	}
	if n := d.probes.Load(); n != 1 {
		t.Fatalf("probes within TTL = %d, want 1", n)
	}

	now = base.Add(inventoryTTL + time.Second)
	if _, _, err := e.Tools(ctx); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if n := d.probes.Load(); n != 2 {
		t.Errorf("probes after TTL expiry = %d, want 2", n)
	}
}

// TestEngineGateSkipsDiscovery pins Gate 1: any cached inventory, fresh or
// stale, short-circuits a non-referencing script before any probe.
func TestEngineGateSkipsDiscovery(t *testing.T) {
	requireUV(t)
	ctx := context.Background()
	tests := []struct {
		name string
		age  time.Duration
	}{
		{"fresh cache", time.Minute},
		{"stale cache", inventoryTTL + time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDiscovery{err: errors.New("probe must not run")}
			base := time.Now()
			store := seedInventory(t, fakeInventory(), base.Add(-tt.age))
			e, conn := newTestEngine(t, d, WithInventoryStore(store))
			e.now = func() time.Time { return base }

			out, notes, err := e.Exec(ctx, "1 + 1", 0)
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if out != "2" {
				t.Errorf("Exec = %q, want 2", out)
			}
			if len(notes) != 0 {
				t.Errorf("notes = %q, want none (gate skips silently)", notes)
			}
			if n := d.probes.Load(); n != 0 {
				t.Errorf("probes = %d, want 0 (gate skips discovery)", n)
			}
			if n := conn.connectCount("fake"); n != 0 {
				t.Errorf("connects = %d, want 0", n)
			}
		})
	}
}

// TestEngineStaleFallback pins the stale-fallback path: a stale cache forces a
// probe, and when the probe fails the cached inventory still reflects, with a
// note naming the failure and the cache age.
func TestEngineStaleFallback(t *testing.T) {
	requireUV(t)
	ctx := context.Background()
	d := &fakeDiscovery{err: errors.New("claude mcp list timed out after 1s")}
	base := time.Now()
	age := inventoryTTL + 5*time.Minute
	store := seedInventory(t, fakeInventory(), base.Add(-age))
	e, conn := newTestEngine(t, d, WithInventoryStore(store))
	e.now = func() time.Time { return base }

	script := "import asyncio\nasyncio.run(fake_echo(text=\"hi\"))"
	out, notes, err := e.Exec(ctx, script, 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if want := `{"text":"hi"}`; out != want {
		t.Errorf("Exec = %q, want %q", out, want)
	}
	if n := d.probes.Load(); n != 1 {
		t.Errorf("probes = %d, want 1 (stale forces a probe)", n)
	}
	wantNote := "claude mcp list timed out after 1s; using mcp inventory cached " + age.String() + " ago"
	if !containsSub(notes, wantNote) {
		t.Errorf("notes = %q, want stale-fallback note %q", notes, wantNote)
	}
	if n := conn.connectCount("fake"); n != 2 {
		t.Errorf("connects = %d, want 2 (reflect via cached inventory)", n)
	}
}

// TestEngineNoCacheProbeFailure guards the Discover contract flip: with no
// cache, a probe failure degrades to a note plus builtins, never a hard error.
func TestEngineNoCacheProbeFailure(t *testing.T) {
	requireUV(t)
	ctx := context.Background()
	d := &fakeDiscovery{err: errors.New("claude not on PATH")}
	e, conn := newTestEngine(t, d)

	out, notes, err := e.Exec(ctx, "40 + 2", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "42" {
		t.Errorf("Exec = %q, want 42", out)
	}
	if !containsSub(notes, "mcp reflection unavailable: claude not on PATH") {
		t.Errorf("notes = %q, want degrade note", notes)
	}
	if n := d.probes.Load(); n != 1 {
		t.Errorf("probes = %d, want 1", n)
	}
	if n := conn.connectCount("fake"); n != 0 {
		t.Errorf("connects = %d, want 0", n)
	}
}

// TestEngineRefreshProbes pins CCX_EXEC_MCP=refresh: it bypasses Gate 1 and the
// TTL, so even a non-referencing Exec and Tools re-probe a fresh cache.
func TestEngineRefreshProbes(t *testing.T) {
	requireUV(t)
	ctx := context.Background()
	d := &fakeDiscovery{inv: fakeInventory()}
	base := time.Now()
	store := seedInventory(t, fakeInventory(), base)
	e, _ := newTestEngine(t, d, WithInventoryStore(store))
	e.now = func() time.Time { return base }
	t.Setenv("CCX_EXEC_MCP", "refresh")

	if _, _, err := e.Exec(ctx, "1 + 1", 0); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if n := d.probes.Load(); n != 1 {
		t.Errorf("probes after refresh Exec = %d, want 1", n)
	}
	if _, _, err := e.Tools(ctx); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if n := d.probes.Load(); n != 2 {
		t.Errorf("probes after refresh Tools = %d, want 2", n)
	}
}

// TestEngineToolsNeverGated pins that Tools is ungated: a stale cache forces a
// probe on Tools, and the fresh cache then gates a non-referencing Exec.
func TestEngineToolsNeverGated(t *testing.T) {
	requireUV(t)
	ctx := context.Background()
	d := &fakeDiscovery{inv: fakeInventory()}
	base := time.Now()
	store := seedInventory(t, fakeInventory(), base.Add(-inventoryTTL-time.Minute))
	e, _ := newTestEngine(t, d, WithInventoryStore(store))
	e.now = func() time.Time { return base }

	if _, _, err := e.Tools(ctx); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if n := d.probes.Load(); n != 1 {
		t.Fatalf("probes after Tools = %d, want 1 (Tools ungated)", n)
	}
	if _, _, err := e.Exec(ctx, "1 + 1", 0); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if n := d.probes.Load(); n != 1 {
		t.Errorf("probes after gated Exec = %d, want still 1", n)
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

// TestEngineBadTimeoutHardError pins that an invalid CCX_EXEC_MCP_TIMEOUT
// (errBadTimeout) fails Exec and Tools loudly, never degrading to a fallback note.
func TestEngineBadTimeoutHardError(t *testing.T) {
	ctx := context.Background()
	d := &fakeDiscovery{err: fmt.Errorf("probe timeout: %w", errBadTimeout)}
	base := time.Now()
	store := seedInventory(t, fakeInventory(), base.Add(-inventoryTTL-time.Minute))
	e, _ := newTestEngine(t, d, WithInventoryStore(store))
	e.now = func() time.Time { return base }

	script := "import asyncio\nasyncio.run(fake_echo(text=\"hi\"))"
	if _, _, err := e.Exec(ctx, script, 0); !errors.Is(err, errBadTimeout) {
		t.Errorf("Exec error = %v, want errBadTimeout (hard fail, no fallback)", err)
	}
	if _, _, err := e.Tools(ctx); !errors.Is(err, errBadTimeout) {
		t.Errorf("Tools error = %v, want errBadTimeout", err)
	}
}

// blockingDiscovery blocks each probe inside discover until released, so a test
// can hold one prober under e.mu while other waiters queue behind it.
type blockingDiscovery struct {
	probes  atomic.Int32
	started chan struct{}
	release chan struct{}
	err     error
}

func (d *blockingDiscovery) discover(context.Context) (Inventory, error) {
	d.probes.Add(1)
	d.started <- struct{}{}
	<-d.release
	return Inventory{}, d.err
}

// TestEngineConcurrentProbeCollapse pins that a wave of concurrent resolves
// against a hanging claude serializes to exactly one probe: the waiters adopt
// the winner's recorded failure instead of each re-probing.
func TestEngineConcurrentProbeCollapse(t *testing.T) {
	ctx := context.Background()
	d := &blockingDiscovery{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
		err:     errors.New("claude mcp list failed: hung"),
	}
	base := time.Now()
	store := seedInventory(t, fakeInventory(), base.Add(-inventoryTTL-time.Minute))
	e, _ := newTestEngine(t, d, WithInventoryStore(store))

	const callers = 4
	notes := make([][]string, callers)
	var wg sync.WaitGroup

	// The winner probes first and blocks inside discover, holding e.mu.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, notes[0], _ = e.Tools(ctx)
	}()
	<-d.started

	// The rest enter while the winner holds the lock, so their entry time
	// precedes the failure the winner is about to record.
	for i := 1; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, notes[i], _ = e.Tools(ctx)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(d.release)
	wg.Wait()

	if n := d.probes.Load(); n != 1 {
		t.Errorf("probes = %d, want 1 (the wave collapses to one probe)", n)
	}
	for i, ns := range notes {
		if !containsSub(ns, "using mcp inventory cached") {
			t.Errorf("caller %d notes = %q, want stale-fallback note", i, ns)
		}
	}
}
