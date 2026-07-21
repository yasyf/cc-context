package anchor

import (
	"fmt"
	"strconv"

	"github.com/yasyf/cc-context/internal/backend"
)

// RewriteArgs validates read targets, then resolves any content anchor in a's
// op-relevant field to plain line numbers before dispatch, so backends stay
// anchor-ignorant. It returns the rewritten args plus a note when the anchored
// content moved. Ops without an anchor-capable field pass through unchanged,
// and the numeric output never re-parses as an anchor, so the rewrite is
// idempotent.
func RewriteArgs(op backend.Op, a backend.Args) (backend.Args, string, error) {
	a, resolutionNote, err := backend.ResolvePath(op, a)
	if err != nil {
		return a, "", err
	}
	switch op {
	case backend.OpRead:
		a, anchorNote, err := rewriteRead(a)
		return a, resolutionNote + anchorNote, err
	case backend.OpRelated:
		a, anchorNote, err := rewriteRelated(a)
		return a, resolutionNote + anchorNote, err
	default:
		return a, resolutionNote, nil
	}
}

func rewriteRead(a backend.Args) (backend.Args, string, error) {
	if a.Full {
		return a, "", nil
	}
	a.Section = NormalizeRange(a.Section)
	ref, ok, err := Parse(a.Section)
	if err != nil {
		return a, "", err
	}
	if !ok {
		return a, "", nil
	}
	f, err := Load(a.Path)
	if err != nil {
		return a, "", err
	}
	rng, move, err := f.Resolve(ref)
	if err != nil {
		return a, "", err
	}
	a.Section = fmt.Sprintf("%d-%d", rng.Start, rng.End)
	return a, MoveNote(ref.Hash, move), nil
}

func rewriteRelated(a backend.Args) (backend.Args, string, error) {
	path, ref, ok, err := ParseLoc(a.Query)
	if err != nil {
		return a, "", err
	}
	if !ok {
		return a, "", nil
	}
	f, err := Load(path)
	if err != nil {
		return a, "", err
	}
	rng, move, err := f.Resolve(ref)
	if err != nil {
		return a, "", err
	}
	a.Query = path + ":" + strconv.Itoa(rng.Start)
	return a, MoveNote(ref.Hash, move), nil
}

// MoveNote renders the one-line relocation note for a moved anchor, or "" when
// the anchor did not move (m is nil).
func MoveNote(h Hash, m *Move) string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("# anchor %s: line %d → %d\n", h, m.From, m.To)
}
