package codeexec

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Engine wires the sandbox runtime to the ccx builtins plus every reflected
// MCP tool, resolving tool catalogs through a CatalogStore.
type Engine struct {
	caller      Caller
	store       CatalogStore
	inventories InventoryStore
	discover    func(context.Context) (Inventory, error)
	connect     connectFunc
	reflector   *Reflector
	now         func() time.Time

	mu       sync.Mutex
	failedAt time.Time
	failErr  error
}

// Option configures an Engine's seams; defaults are the live Discover probe
// and the transport-appropriate connect.
type Option func(*Engine)

// WithDiscover replaces the live `claude mcp list` probe.
func WithDiscover(fn func(context.Context) (Inventory, error)) Option {
	return func(e *Engine) { e.discover = fn }
}

// WithConnect replaces the per-spec transport dialer.
func WithConnect(fn func(context.Context, ServerSpec) (*mcp.ClientSession, error)) Option {
	return func(e *Engine) { e.connect = fn }
}

// WithInventoryStore replaces the in-memory inventory cache; the CLI passes a
// disk store so a warm cache skips the probe across process boundaries.
func WithInventoryStore(s InventoryStore) Option {
	return func(e *Engine) { e.inventories = s }
}

// NewEngine builds an Engine over caller's ccx ops and store's catalog cache.
func NewEngine(caller Caller, store CatalogStore, opts ...Option) *Engine {
	e := &Engine{
		caller:      caller,
		store:       store,
		inventories: NewMemoryInventoryStore(),
		discover:    Discover,
		connect:     defaultConnect,
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(e)
	}
	e.reflector = NewReflector(e.connect)
	return e
}

// Exec runs script in the sandbox against the ccx builtins plus every
// reflected tool, returning the budget-capped output and the notes accumulated
// while resolving the reflected surface.
func (e *Engine) Exec(ctx context.Context, script string, budget int) (string, []string, error) {
	notes, reflected, err := e.resolve(ctx, script)
	if err != nil {
		return "", notes, err
	}
	funcs := e.builtins()
	if reflected {
		e.reflector.Prewarm(ctx, referenced(script, e.reflector.FuncServers()))
		var collisions []string
		funcs, collisions = merge(funcs, e.reflector.Funcs())
		notes = append(notes, collisions...)
	}
	out, err := NewRuntime(funcs).Run(ctx, script, budget)
	return out, notes, err
}

// Tools renders the full sandbox preamble — builtin plus reflected signatures
// — and the notes explaining every skipped or degraded server.
func (e *Engine) Tools(ctx context.Context) (string, []string, error) {
	notes, reflected, err := e.resolve(ctx, "")
	if err != nil {
		return "", notes, err
	}
	var sigs []ToolSig
	if reflected {
		sigs = e.reflector.Sigs()
	}
	return Preamble(sigs), notes, nil
}

// Close tears down every reflected session.
func (e *Engine) Close() error { return e.reflector.Close() }

// resolve readies the reflector for script's inventory, reporting whether any
// reflected tools are available. An empty script is ungated: Tools and
// --list-tools always refresh a cold or stale cache. A script naming no cached
// server skips discovery, catalog resolution, and every note. CCX_EXEC_MCP=off
// and an empty inventory both mean builtins only; refresh bypasses the cache.
func (e *Engine) resolve(ctx context.Context, script string) ([]string, bool, error) {
	mode := os.Getenv("CCX_EXEC_MCP")
	if mode == "off" {
		return nil, false, nil
	}
	refresh := mode == "refresh"
	entered := e.now()
	inv, probedAt, cached := e.inventories.Load()

	// Gate 1: any cached inventory short-circuits before a probe when the script
	// names no reflected tool. Stale or fresh, a builtins-only script never pays.
	if script != "" && cached && !refresh && !referencesMCP(script, inv.Servers) {
		return nil, false, nil
	}

	var notes []string
	if fresh := cached && e.now().Sub(probedAt) < inventoryTTL && !refresh; !fresh {
		probed, probeNotes, degraded, err := e.probe(ctx, inv, probedAt, entered, cached)
		if err != nil {
			return nil, false, err
		}
		if degraded {
			return probeNotes, false, nil
		}
		inv, notes = probed, probeNotes
		// Gate 2: a cold or refreshed probe gates on its own fresh result.
		if script != "" && !referencesMCP(script, inv.Servers) {
			return nil, false, nil
		}
	}

	notes = append(notes, inv.Notes...)
	if len(inv.Servers) == 0 {
		return notes, false, nil
	}
	cat, err := resolveCatalog(ctx, e.store, inv, e.connect)
	if err != nil {
		return notes, false, err
	}
	for _, sc := range cat.Servers {
		if sc.Note != "" {
			notes = append(notes, sc.Note)
		}
	}
	if err := e.reflector.SetCatalog(cat); err != nil {
		notes = append(notes, fmt.Sprintf("closing dropped servers: %v", err))
	}
	return notes, true, nil
}

// probe re-runs discovery under e.mu, first double-checking whether a racing
// resolve in this wave already refreshed the cache (adopt it) or already failed
// against the same claude (adopt that failure instead of serializing another
// probe — a later wave, entered after the failure, still re-probes). A bad
// CCX_EXEC_MCP_TIMEOUT is a hard error; any other probe failure falls back to
// the cached inventory with a note, or degrades to builtins when nothing is
// cached.
func (e *Engine) probe(ctx context.Context, cachedInv Inventory, probedAt, entered time.Time, cached bool) (Inventory, []string, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if latest, latestAt, ok := e.inventories.Load(); ok && latestAt.After(probedAt) {
		return latest, nil, false, nil
	}
	if !e.failedAt.IsZero() && e.failedAt.After(entered) {
		return e.probeFailure(cachedInv, probedAt, cached, e.failErr)
	}

	inv, err := e.discover(ctx)
	if err != nil && errors.Is(err, errBadTimeout) {
		return Inventory{}, nil, false, err
	}
	if err == nil {
		if serr := e.inventories.Save(inv, e.now()); serr != nil {
			return Inventory{}, nil, false, fmt.Errorf("save inventory: %w", serr)
		}
		return inv, nil, false, nil
	}
	e.failedAt, e.failErr = e.now(), err
	return e.probeFailure(cachedInv, probedAt, cached, err)
}

// probeFailure shapes a probe error into the stale-fallback (cached inventory
// keeps reflecting, with a note) or no-cache degrade (builtins only) result.
func (e *Engine) probeFailure(cachedInv Inventory, probedAt time.Time, cached bool, err error) (Inventory, []string, bool, error) {
	if cached {
		age := e.now().Sub(probedAt).Truncate(time.Second)
		return cachedInv, []string{fmt.Sprintf("%v; using mcp inventory cached %s ago", err, age)}, false, nil
	}
	return Inventory{}, []string{fmt.Sprintf("mcp reflection unavailable: %v", err)}, true, nil
}

func (e *Engine) builtins() map[string]HostFunc {
	funcs := Ops(e.caller)
	funcs["sh"] = Sh()
	return funcs
}

// merge overlays reflected onto builtin. A ccx builtin wins any name
// collision, each noted so the model knows the reflected tool is shadowed.
func merge(builtin, reflected map[string]HostFunc) (map[string]HostFunc, []string) {
	merged := make(map[string]HostFunc, len(builtin)+len(reflected))
	var notes []string
	for _, name := range slices.Sorted(maps.Keys(reflected)) {
		if _, ok := builtin[name]; ok {
			notes = append(notes, fmt.Sprintf("reflected tool %s is shadowed by the ccx builtin of the same name", name))
			continue
		}
		merged[name] = reflected[name]
	}
	maps.Copy(merged, builtin)
	return merged, notes
}
