package symbol

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

func TestParseQuery(t *testing.T) {
	tests := []struct {
		name          string
		query         string
		wantQualifier string
		wantName      string
	}{
		{"bare", "Render", "", "Render"},
		{"dot receiver", "Widget.Render", "Widget", "Render"},
		{"double colon", "Class::method", "Class", "method"},
		{"hash", "Class#method", "Class", "method"},
		{"nested dot last wins", "pkg.Widget.Render", "pkg.Widget", "Render"},
		{"colon then dot last wins", "A::b.c", "A::b", "c"},
		{"single colon not a separator", "a:b", "", "a:b"},
		{"trailing dot is bare", "Widget.", "", "Widget."},
		{"leading dot", ".field", "", "field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotQ, gotN := parseQuery(tt.query)
			if gotQ != tt.wantQualifier || gotN != tt.wantName {
				t.Errorf("parseQuery(%q) = (%q, %q), want (%q, %q)", tt.query, gotQ, gotN, tt.wantQualifier, tt.wantName)
			}
		})
	}
}

// TestCalleesExcludesDefinitions proves the callee scan drops definition-shaped
// lines, so a class's own methods never render as calls while a real call inside a
// function body is kept. It reads the fixture directly — no engine needed.
func TestCalleesExcludesDefinitions(t *testing.T) {
	tests := []struct {
		name string
		self string
		top  candidate
		want []string
	}{
		// Widget spans its class body (lines 4-15); render/build are member defs, and
		// the lone Widget() call is the symbol itself — so no callee survives.
		{"class members are not calls", "Widget", candidate{path: "testdata/src/sample.py", start: 4, end: 15}, nil},
		// helper's body has a real render_all() call past its own def line.
		{"real call survives", "helper", candidate{path: "testdata/src/sample.py", start: 18, end: 20}, []string{"render_all"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &resolver{name: tt.self, lineCache: map[string][]string{}}
			got := r.callees(tt.top)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("callees(%s) = %v, want %v", tt.self, got, tt.want)
			}
		})
	}
}

func TestRunMissIsNotFound(t *testing.T) {
	requireBins(t)
	_, err := Run(context.Background(), backend.Args{Query: "ZzNoSuchSymbolXx", Scope: "testdata/src"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Run miss = %v, want ErrNotFound", err)
	}
}

// TestLiveE2E resolves a symbol defined in the fixtures and prints the terse and
// --full cards, exercising the whole pipeline against real ast-grep and ripgrep.
func TestLiveE2E(t *testing.T) {
	requireBins(t)
	terse, err := Run(context.Background(), backend.Args{Query: "decorate", Scope: "testdata/src"})
	if err != nil {
		t.Fatalf("terse Run: %v", err)
	}
	if !strings.HasPrefix(terse, "# symbol decorate — function — testdata/src/sample.go:") {
		t.Errorf("terse card header unexpected:\n%s", terse)
	}
	if !strings.Contains(terse, "decorate wraps s in brackets.") {
		t.Errorf("terse card missing extracted doc:\n%s", terse)
	}
	if !strings.Contains(terse, "— --callers/--tests/--siblings/--body/--full") {
		t.Errorf("terse card missing counts trailer:\n%s", terse)
	}
	t.Logf("=== TERSE (decorate) ===\n%s", terse)

	full, err := Run(context.Background(), backend.Args{Query: "decorate", Scope: "testdata/src", Full: true})
	if err != nil {
		t.Fatalf("full Run: %v", err)
	}
	for _, want := range []string{"## body", "## callers", "## calls (syntactic)", "## siblings", "in Render"} {
		if !strings.Contains(full, want) {
			t.Errorf("full card missing %q:\n%s", want, full)
		}
	}
	t.Logf("=== FULL (decorate) ===\n%s", full)
}

// TestLiveDisambiguation resolves a name defined in all three fixtures, so the
// top hit renders a card and the rest collapse into the "also defined" footer.
func TestLiveDisambiguation(t *testing.T) {
	requireBins(t)
	out, err := Run(context.Background(), backend.Args{Query: "Widget", Scope: "testdata/src"})
	if err != nil {
		t.Fatalf("disambiguation Run: %v", err)
	}
	// Equal path lengths tie-break lexicographically, so sample.go wins the card.
	if !strings.HasPrefix(out, "# symbol Widget — struct — testdata/src/sample.go:") {
		t.Errorf("want Go struct as top hit, got:\n%s", out)
	}
	if !strings.Contains(out, "also defined:") ||
		!strings.Contains(out, "testdata/src/sample.py:") ||
		!strings.Contains(out, "testdata/src/sample.ts:") {
		t.Errorf("want disambiguation footer citing py and ts, got:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "— narrow with --scope") {
		t.Errorf("want footer to end with the --scope hint, got:\n%s", out)
	}
	t.Logf("=== DISAMBIGUATION (Widget) ===\n%s", out)
}

// TestLiveDegraded resolves a symbol that lives only in a file ast-grep does not
// outline, so the miss ladder falls through to the definition-keyword scan.
func TestLiveDegraded(t *testing.T) {
	requireBins(t)
	out, err := Run(context.Background(), backend.Args{Query: "orphan_sym", Scope: "testdata/degraded"})
	if err != nil {
		t.Fatalf("degraded Run: %v", err)
	}
	if !strings.HasPrefix(out, "# symbol orphan_sym — no structural definition") {
		t.Errorf("want degraded header, got:\n%s", out)
	}
	if !strings.Contains(out, "def orphan_sym(x):") {
		t.Errorf("want the def-shaped row, got:\n%s", out)
	}
	t.Logf("=== DEGRADED (orphan_sym) ===\n%s", out)
}

func requireBins(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ast-grep"); err != nil {
		t.Skip("ast-grep not on PATH")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
}
