// Package search is the front door for `ccx code search`: it classifies a query and
// maps the chosen kind to the logical op that serves it. Both the CLI and the MCP
// handler route through it so the two surfaces behave identically.
package search

import (
	"fmt"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/querykind"
)

// Route classifies a.Query under the a.Mode override and returns the op that
// serves it together with the chosen kind, so the caller can render the routing
// header without reclassifying. Semantic→OpSearch, Structural→OpStructural,
// Literal→OpGrep. An unknown mode is an error.
func Route(a backend.Args) (backend.Op, querykind.Kind, error) {
	override, err := querykind.ParseKind(a.Mode)
	if err != nil {
		return "", querykind.KindAuto, err
	}
	kind := querykind.Classify(a.Query, override)
	switch kind {
	case querykind.KindSemantic:
		return backend.OpSearch, kind, nil
	case querykind.KindStructural:
		return backend.OpStructural, kind, nil
	case querykind.KindLiteral:
		return backend.OpGrep, kind, nil
	default:
		return "", kind, fmt.Errorf("search: unroutable kind %q", kind)
	}
}
