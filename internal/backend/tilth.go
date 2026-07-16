package backend

import (
	"context"
	"fmt"

	"github.com/yasyf/cc-context/internal/vendor"
)

// Tilth translates ops onto the tilth engine. tilth is query-dispatched: the
// first positional selects the mode (a path, a subcommand, or a search string).
type Tilth struct {
	// Bin is the resolved tilth binary path. Empty triggers resolution via
	// vendor.Resolve (configured bin → PATH → pinned download) on first use.
	Bin string
}

// Engine reports that Tilth is backed by the tilth engine.
func (t Tilth) Engine() Engine {
	return EngineTilth
}

func (t Tilth) bin(ctx context.Context) (string, error) {
	return vendor.Resolve(ctx, vendor.Tilth, t.Bin)
}

// CLIArgv translates op into a tilth child-process invocation. Every op now runs
// natively, so nothing routes here — the shell errors loudly on any op.
func (t Tilth) CLIArgv(_ context.Context, op Op, _ Args) (bin string, argv []string, err error) {
	return "", nil, fmt.Errorf("tilth: unsupported op %q", op)
}

// MCPSpec returns the command that launches tilth's MCP server over stdio,
// provisioning the pinned binary if needed. A resolution failure propagates
// rather than falling back to a bare PATH "tilth".
func (t Tilth) MCPSpec(ctx context.Context) (cmd string, argv []string, err error) {
	bin, err := t.bin(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("tilth: resolve binary: %w", err)
	}
	// --no-overview skips tilth's init project-fingerprint scan: the facade
	// re-exposes overview as its own op, so the auto-injection is wasted work
	// (and a per-call cost for one-shot CLI outline reads).
	return bin, []string{"--mcp", "--no-overview"}, nil
}

// MCPTool translates op into a tilth MCP tool name and its params. Every op now
// runs natively, so nothing routes here — the shell errors loudly on any op.
func (t Tilth) MCPTool(op Op, _ Args) (tool string, params map[string]any, err error) {
	return "", nil, fmt.Errorf("tilth: unsupported op %q", op)
}
