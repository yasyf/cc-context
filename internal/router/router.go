// Package router maps logical ops onto their backing engine.
package router

import "github.com/yasyf/cc-context/internal/backend"

// For returns the backend that serves op. semble serves the semantic ops
// (search, related); ast-grep serves the structural ops (structural, replace,
// struct-outline). Symbol, deps, outline, diff, find, and the web/read/grep ops are
// now served natively before ever reaching here, so routing them is an impossible
// state; the tilth default is vestigial, retained for the machinery-teardown pass.
func For(op backend.Op) backend.Backend {
	switch op {
	case backend.OpSearch, backend.OpRelated:
		return backend.Semble{}
	case backend.OpStructural, backend.OpReplace, backend.OpStructOutline:
		return backend.AstGrep{}
	case backend.OpFind:
		panic("router: OpFind has no backend — file finding is served natively by internal/find (see proxy.call / cli.dispatch)")
	default:
		return backend.Tilth{}
	}
}
