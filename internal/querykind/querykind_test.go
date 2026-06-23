package querykind

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		override Kind
		want     Kind
	}{
		{"natural language prose", "how does routing work", KindAuto, KindSemantic},
		{"single metavar", "$A", KindAuto, KindStructural},
		{"variadic in call", "foo($$$ARGS)", KindAuto, KindStructural},
		{"underscore-led metavar", "$_X", KindAuto, KindStructural},
		{"double metavar", "$$NAME", KindAuto, KindStructural},
		{"qualified call with metavar", "$A.Foo($$$)", KindAuto, KindStructural},
		{"bare triple metavar whole query", "$$$", KindAuto, KindSemantic},
		{"bare triple metavar trimmed", "  $$$  ", KindAuto, KindSemantic},
		{"lowercase sigil ident not a metavar", "$foo", KindAuto, KindSemantic},
		{"kebab after sigil not a metavar", "$kebab-case", KindAuto, KindSemantic},
		{"digits after sigil not a metavar", "$123", KindAuto, KindSemantic},
		{"uppercase env-looking token is structural by design", "$PATH", KindAuto, KindStructural},
		{"override semantic on metavar query", "$A", KindSemantic, KindSemantic},
		{"override structural on prose", "plain words", KindStructural, KindStructural},
		{"override literal on prose", "plain words", KindLiteral, KindLiteral},
		{"override literal on env token", "$PATH", KindLiteral, KindLiteral},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.query, tt.override); got != tt.want {
				t.Errorf("Classify(%q, %v) = %v, want %v", tt.query, tt.override, got, tt.want)
			}
		})
	}
}

func TestParseKind(t *testing.T) {
	tests := []struct {
		in      string
		want    Kind
		wantErr bool
	}{
		{"", KindAuto, false},
		{"auto", KindAuto, false},
		{"semantic", KindSemantic, false},
		{"structural", KindStructural, false},
		{"literal", KindLiteral, false},
		{"bogus", KindAuto, true},
		{"Semantic", KindAuto, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseKind(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseKind(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseKind(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestKindString(t *testing.T) {
	tests := []struct {
		k    Kind
		want string
	}{
		{KindAuto, "auto"},
		{KindSemantic, "semantic"},
		{KindStructural, "structural"},
		{KindLiteral, "literal"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.k.String(); got != tt.want {
				t.Errorf("Kind(%d).String() = %q, want %q", int(tt.k), got, tt.want)
			}
		})
	}
}
