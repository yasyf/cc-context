// Package router maps logical ops onto their backing engine.
package router

import "github.com/yasyf/cc-context/internal/backend"

// For returns the backend that serves op. semble serves the semantic ops
// (search, related); ast-grep serves the structural ops (structural, replace,
// struct-outline); tilth serves the symbol and deps ops (the default). Outline,
// diff, find, and the web/read/grep ops are served natively before ever reaching
// here, so routing them is an impossible state.
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
