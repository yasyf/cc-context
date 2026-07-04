package codeexec

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// inventoryTTL is how long an Engine trusts one `claude mcp list` probe before
// re-running it: a resident caller (the MCP facade) shells out at most once a
// minute, while a fresh CLI Engine probes exactly once.
const inventoryTTL = 60 * time.Second

// Engine wires the sandbox runtime to the ccx builtins plus every reflected
// MCP tool, resolving tool catalogs through a CatalogStore.
type Engine struct {
	caller    Caller
	store     CatalogStore
	discover  func(context.Context) (Inventory, error)
	connect   connectFunc
	reflector *Reflector

	mu     sync.Mutex
	inv    Inventory
	probed time.Time
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

// NewEngine builds an Engine over caller's ccx ops and store's catalog cache.
func NewEngine(caller Caller, store CatalogStore, opts ...Option) *Engine {
	e := &Engine{caller: caller, store: store, discover: Discover, connect: defaultConnect}
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
	notes, reflected, err := e.resolve(ctx)
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
	notes, reflected, err := e.resolve(ctx)
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

// resolve readies the reflector for the current inventory, reporting whether
// any reflected tools are available. CCX_EXEC_MCP=off and an empty inventory
// both mean builtins only.
func (e *Engine) resolve(ctx context.Context) ([]string, bool, error) {
	if os.Getenv("CCX_EXEC_MCP") == "off" {
		return nil, false, nil
	}
	inv, err := e.inventory(ctx)
	if err != nil {
		return nil, false, err
	}
	notes := slices.Clone(inv.Notes)
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

// inventory returns the cached probe while it is younger than inventoryTTL,
// else re-probes.
func (e *Engine) inventory(ctx context.Context) (Inventory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.probed.IsZero() && time.Since(e.probed) < inventoryTTL {
		return e.inv, nil
	}
	inv, err := e.discover(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("discover mcp servers: %w", err)
	}
	e.inv, e.probed = inv, time.Now()
	return inv, nil
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
