package symbol

import "testing"

func TestRenderCardGoldens(t *testing.T) {
	tests := []struct {
		name string
		card card
		want string
	}{
		{
			name: "terse",
			card: card{
				name:      "Resolve",
				kind:      "method",
				loc:       "internal/anchor/anchor.go:254-274#h4k2",
				signature: "func (f *File) Resolve(ref Ref) (Range, *Move, error)",
				doc:       "Resolve locates ref's content.",
				terse:     true,
				refs:      3,
				tests:     1,
				siblings:  12,
			},
			want: "# symbol Resolve — method — internal/anchor/anchor.go:254-274#h4k2\n" +
				"func (f *File) Resolve(ref Ref) (Range, *Move, error)\n" +
				"\n" +
				"Resolve locates ref's content.\n" +
				"\n" +
				"refs 3 · tests 1 · siblings 12 — --callers/--tests/--siblings/--body/--full\n",
		},
		{
			name: "full expansion",
			card: card{
				name:      "decorate",
				kind:      "function",
				loc:       "a.go:20-22#fqah",
				signature: "func decorate(s string) string",
				doc:       "decorate wraps s in brackets.",
				showBody:  true,
				body:      []string{"func decorate(s string) string {", "  return wrap(s)", "}"},
				callers: &refBlock{
					label: "callers", word: "decorate", total: 1, files: 1,
					groups: []refGroup{{path: "a.go", rows: []string{"[16#ysw3] return decorate(w.Name)   in Render"}}},
				},
				showCallees:  true,
				callees:      []string{"wrap"},
				showSiblings: true,
				siblingPath:  "a.go",
				siblingRows:  []string{"[10-12#gpks] type Widget struct", "[15-17#qdc2] func (w Widget) Render() string"},
				testBlock:    &refBlock{label: "tests", word: "decorate", total: 0, files: 0},
			},
			want: "# symbol decorate — function — a.go:20-22#fqah\n" +
				"func decorate(s string) string\n" +
				"\n" +
				"decorate wraps s in brackets.\n" +
				"\n" +
				"## body\n" +
				"func decorate(s string) string {\n" +
				"  return wrap(s)\n" +
				"}\n" +
				"\n" +
				"## callers (word refs — 1 in 1 files)\n" +
				"### a.go\n" +
				"[16#ysw3] return decorate(w.Name)   in Render\n" +
				"\n" +
				"## calls (syntactic)\n" +
				"wrap\n" +
				"\n" +
				"## siblings (a.go)\n" +
				"[10-12#gpks] type Widget struct\n" +
				"[15-17#qdc2] func (w Widget) Render() string\n" +
				"\n" +
				"## tests (word refs — 0 in 0 files)\n",
		},
		{
			name: "disambiguation footer",
			card: card{
				name:      "Foo",
				kind:      "function",
				loc:       "a.go:10-12#h1a2",
				signature: "func Foo() error",
				terse:     true,
				siblings:  2,
				also:      []string{"b.go:20#h2b3 (method)", "c.go:5#h3c4 (variable)"},
			},
			want: "# symbol Foo — function — a.go:10-12#h1a2\n" +
				"func Foo() error\n" +
				"\n" +
				"refs 0 · tests 0 · siblings 2 — --callers/--tests/--siblings/--body/--full\n" +
				"also defined: b.go:20#h2b3 (method) · c.go:5#h3c4 (variable) — narrow with --scope\n",
		},
		{
			name: "disambiguation with overflow",
			card: card{
				name:      "Foo",
				kind:      "function",
				loc:       "a.go:1#h1a2",
				signature: "func Foo()",
				terse:     true,
				also:      []string{"b.go:2#h2b3 (function)"},
				alsoMore:  4,
			},
			want: "# symbol Foo — function — a.go:1#h1a2\n" +
				"func Foo()\n" +
				"\n" +
				"refs 0 · tests 0 · siblings 0 — --callers/--tests/--siblings/--body/--full\n" +
				"also defined: b.go:2#h2b3 (function) · (+4 more) — narrow with --scope\n",
		},
		{
			name: "case-insensitive disclosure",
			card: card{
				name:       "Resolve",
				kind:       "method",
				loc:        "x.go:5-9#hhkm",
				caseFolded: true,
				query:      "resolve",
				signature:  "func (f *File) Resolve() error",
				terse:      true,
				refs:       1,
				siblings:   3,
			},
			want: "# symbol Resolve — method — x.go:5-9#hhkm (case-insensitive: queried \"resolve\")\n" +
				"func (f *File) Resolve() error\n" +
				"\n" +
				"refs 1 · tests 0 · siblings 3 — --callers/--tests/--siblings/--body/--full\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderCard(tt.card); got != tt.want {
				t.Errorf("renderCard mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tt.want)
			}
		})
	}
}

func TestRenderDegradedGolden(t *testing.T) {
	groups := []refGroup{
		{path: "lib.rb", rows: []string{"[12] def weirdsym", "[30] class WeirdSymHolder"}},
	}
	want := "# symbol weirdsym — no structural definition (not in ast-grep outline); definition-shaped text matches:\n" +
		"### lib.rb\n" +
		"[12] def weirdsym\n" +
		"[30] class WeirdSymHolder\n"
	if got := renderDegraded("weirdsym", groups, 0); got != want {
		t.Errorf("renderDegraded mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderDegradedOverflow(t *testing.T) {
	groups := []refGroup{{path: "a.rb", rows: []string{"[1] def foo"}}}
	want := "# symbol foo — no structural definition (not in ast-grep outline); definition-shaped text matches:\n" +
		"### a.rb\n" +
		"[1] def foo\n" +
		"… +7 more — ccx code grep foo\n"
	if got := renderDegraded("foo", groups, 7); got != want {
		t.Errorf("renderDegraded overflow mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
