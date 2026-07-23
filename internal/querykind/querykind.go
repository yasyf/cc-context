// Package querykind classifies a search query into the engine that should serve
// it: semantic (native), structural (ast-grep), or literal (grep).
package querykind

import (
	"fmt"
	"regexp"
	"strings"
)

// Kind names the engine a query routes to.
type Kind int

const (
	// KindAuto defers to Classify's detection; it is never a result.
	KindAuto Kind = iota
	// KindSemantic routes to semble's natural-language search.
	KindSemantic
	// KindStructural routes to ast-grep's structural pattern match.
	KindStructural
	// KindLiteral routes to grep's literal/regex search. Reachable only via an
	// explicit override, never by auto-detection.
	KindLiteral
)

// metavar matches an ast-grep metavariable token: a single ($A), double ($$NAME),
// or triple ($$$NAME) sigil before an uppercase/underscore-led identifier. The
// bare variadic $$$ is handled separately in Classify.
var metavar = regexp.MustCompile(`\$\$?\$?[A-Z_][A-Z0-9_]*`)

// Classify returns the engine a query routes to. An explicit override wins. With
// no override, a query carrying an ast-grep metavar token classifies Structural —
// except a query whose entire trimmed content is the bare variadic "$$$", which
// ast-grep 0.44.0 rejects at the root, so it falls through to Semantic. Every
// other query is Semantic. Literal is never auto-detected: a bare "$PATH" query
// classifies Structural by design, and --literal is the escape hatch.
func Classify(q string, override Kind) Kind {
	if override != KindAuto {
		return override
	}
	if strings.TrimSpace(q) == "$$$" {
		return KindSemantic
	}
	if metavar.MatchString(q) || strings.Contains(q, "$$$") {
		return KindStructural
	}
	return KindSemantic
}

// ParseKind maps a mode string to a Kind: "" and "auto" → KindAuto, "semantic",
// "structural", "literal" → their kinds. Any other value is an error.
func ParseKind(s string) (Kind, error) {
	switch s {
	case "", "auto":
		return KindAuto, nil
	case "semantic":
		return KindSemantic, nil
	case "structural":
		return KindStructural, nil
	case "literal":
		return KindLiteral, nil
	default:
		return KindAuto, fmt.Errorf("unknown query mode %q (want auto|semantic|structural|literal)", s)
	}
}

// String renders a Kind as its lowercase mode name.
func (k Kind) String() string {
	switch k {
	case KindAuto:
		return "auto"
	case KindSemantic:
		return "semantic"
	case KindStructural:
		return "structural"
	case KindLiteral:
		return "literal"
	default:
		return fmt.Sprintf("Kind(%d)", int(k))
	}
}
