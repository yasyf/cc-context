// Package router maps logical ops onto their backing engine.
package router

import "github.com/yasyf/cc-context/internal/backend"

// For returns the backend that serves op. semble serves the semantic ops
// (search, related); tilth serves everything else.
func For(op backend.Op) backend.Backend {
	switch op {
	case backend.OpSearch, backend.OpRelated:
		return backend.Semble{}
	default:
		return backend.Tilth{}
	}
}
