package anchor_test

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/anchor"
)

// TestOfGolden locks the hash contract — FNV-1a 32-bit over the trimmed line,
// folded to 22*32^3, letter-first base32 — with hard-coded values. A failure
// here means every anchor ever emitted has been invalidated.
func TestOfGolden(t *testing.T) {
	tests := []struct {
		name string
		line string
		want anchor.Hash
	}{
		{"empty", "", "v7e5"},
		{"whitespace only", "  \t ", "v7e5"},
		{"signature", "func main() {", "f6zy"},
		{"signature with surrounding space", "\tfunc main() {  ", "f6zy"},
		{"signature with trailing cr", "func main() {\r", "f6zy"},
		{"statement", "return nil", "xrkm"},
		{"brace", "}", "tj58"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := anchor.Of(tt.line); got != tt.want {
				t.Errorf("Of(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		section string
		want    anchor.Ref
		wantOK  bool
		wantErr bool
	}{
		{"line", "120", anchor.Ref{}, false, false},
		{"same-line range", "5-5", anchor.Ref{}, false, false},
		{"range", "12-40", anchor.Ref{}, false, false},
		{"numeric range passthrough", "5-7", anchor.Ref{}, false, false},
		{"zero-first numeric range", "0-2", anchor.Ref{}, false, false},
		{"reversed numeric range", "5-3", anchor.Ref{}, false, false},
		{"anchor", "120#a3fk", anchor.Ref{Line: 120, Hash: "a3fk"}, true, false},
		{"range anchor", "120-180#a3fk", anchor.Ref{Line: 120, End: 180, Hash: "a3fk"}, true, false},
		{"bare anchor", "a3fk", anchor.Ref{Hash: "a3fk"}, true, false},
		{"zero line anchor", "0#a3fk", anchor.Ref{}, false, true},
		{"zero-start range anchor", "0-2#a3fk", anchor.Ref{}, false, true},
		{"reversed range anchor", "2-1#a3fk", anchor.Ref{}, false, true},
		{"zero-end range anchor", "5-0#a3fk", anchor.Ref{}, false, true},
		{"short hash", "120#zz", anchor.Ref{}, false, true},
		{"uppercase hash", "120#ABCD", anchor.Ref{}, false, true},
		{"range garbage hash", "120-180#XXXX", anchor.Ref{}, false, true},
		{"excluded letters", "120#ilou", anchor.Ref{}, false, true},
		{"digit-first hash", "120#1abc", anchor.Ref{}, false, true},
		{"triple-segment range anchor", "10-20-30#a3fk", anchor.Ref{}, false, true},
		{"dangling-dash range anchor", "10-#a3fk", anchor.Ref{}, false, true},
		{"double-dash range anchor", "10--20#a3fk", anchor.Ref{}, false, true},
		{"heading", "## Heading", anchor.Ref{}, false, false},
		{"heading with anchor text", "## a3fk", anchor.Ref{}, false, false},
		{"empty", "", anchor.Ref{}, false, false},
		{"digit-first word", "3abc", anchor.Ref{}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := anchor.Parse(tt.section)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse(%q) error = %v, wantErr %v", tt.section, err, tt.wantErr)
			}
			if ok != tt.wantOK {
				t.Fatalf("Parse(%q) ok = %v, want %v", tt.section, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tt.section, got, tt.want)
			}
		})
	}
}

func TestNormalizeRange(t *testing.T) {
	tests := []struct {
		name    string
		section string
		want    string
	}{
		{"comma range", "30,40", "30-40"},
		{"comma range with space", "30, 40", "30-40"},
		{"comma range with spaces both sides", "30 , 40", "30-40"},
		{"dash range unchanged", "30-40", "30-40"},
		{"single line unchanged", "30", "30"},
		{"heading with comma unchanged", "## Foo, Bar", "## Foo, Bar"},
		{"anchor unchanged", "120-180#a3fk", "120-180#a3fk"},
		{"three-part comma unchanged", "30,40,50", "30,40,50"},
		{"trailing comma unchanged", "30,", "30,"},
		{"non-numeric comma unchanged", "a,b", "a,b"},
		{"empty unchanged", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := anchor.NormalizeRange(tt.section); got != tt.want {
				t.Errorf("NormalizeRange(%q) = %q, want %q", tt.section, got, tt.want)
			}
		})
	}
}

// TestFromBytes locks the snapshot split to Load's line split: lines keep any
// trailing '\r' and a final empty element from a trailing newline is dropped.
func TestFromBytes(t *testing.T) {
	tests := []struct {
		name string
		data string
		want []string
	}{
		{"trailing newline", "alpha\nbeta\ngamma\n", []string{"alpha", "beta", "gamma"}},
		{"no trailing newline", "alpha\nbeta\ngamma", []string{"alpha", "beta", "gamma"}},
		{"crlf keeps cr", "alpha\r\nbeta\r\n", []string{"alpha\r", "beta\r"}},
		{"empty", "", []string{}},
		{"lone newline", "\n", []string{""}},
		{"blank interior line", "a\n\nb\n", []string{"a", "", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := anchor.FromBytes("f.txt", []byte(tt.data)).Lines()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("FromBytes(%q).Lines() = %#v, want %#v", tt.data, got, tt.want)
			}
		})
	}
}

// TestLoadDelegatesToFromBytes proves Load and FromBytes split identically.
func TestLoadDelegatesToFromBytes(t *testing.T) {
	const content = "alpha\r\nbeta\ngamma"
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	loaded, err := anchor.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := loaded.Lines(), anchor.FromBytes(path, []byte(content)).Lines(); !reflect.DeepEqual(got, want) {
		t.Errorf("Load().Lines() = %#v, want %#v", got, want)
	}
}

func TestParseLoc(t *testing.T) {
	tests := []struct {
		name     string
		loc      string
		wantPath string
		wantRef  anchor.Ref
		wantOK   bool
		wantErr  bool
	}{
		{"anchored", "internal/cli/run.go:120#a3fk", "internal/cli/run.go", anchor.Ref{Line: 120, Hash: "a3fk"}, true, false},
		{"range anchored", "a.go:120-180#a3fk", "a.go", anchor.Ref{Line: 120, End: 180, Hash: "a3fk"}, true, false},
		{"bare anchored", "a.go:a3fk", "a.go", anchor.Ref{Hash: "a3fk"}, true, false},
		{"numeric", "a.go:120", "", anchor.Ref{}, false, false},
		{"windows drive anchored", `C:\src\a.go:120#a3fk`, `C:\src\a.go`, anchor.Ref{Line: 120, Hash: "a3fk"}, true, false},
		{"windows drive numeric", `C:\src\a.go:120`, "", anchor.Ref{}, false, false},
		{"no colon", "a.go", "", anchor.Ref{}, false, false},
		{"garbage anchor", "a.go:120#zz", "", anchor.Ref{}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, ref, ok, err := anchor.ParseLoc(tt.loc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseLoc(%q) error = %v, wantErr %v", tt.loc, err, tt.wantErr)
			}
			if ok != tt.wantOK {
				t.Fatalf("ParseLoc(%q) ok = %v, want %v", tt.loc, ok, tt.wantOK)
			}
			if path != tt.wantPath {
				t.Errorf("ParseLoc(%q) path = %q, want %q", tt.loc, path, tt.wantPath)
			}
			if ref != tt.wantRef {
				t.Errorf("ParseLoc(%q) ref = %+v, want %+v", tt.loc, ref, tt.wantRef)
			}
		})
	}
}

func TestFormat(t *testing.T) {
	if got := anchor.Format(120, "a3fk"); got != "120#a3fk" {
		t.Errorf("Format() = %q, want %q", got, "120#a3fk")
	}
	if got := anchor.FormatRange(120, 180, "a3fk"); got != "120-180#a3fk" {
		t.Errorf("FormatRange() = %q, want %q", got, "120-180#a3fk")
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		ref      anchor.Ref
		want     anchor.Range
		wantMove *anchor.Move
		wantErr  string
	}{
		{
			name:    "exact hit",
			content: "alpha\nbeta\ngamma\n",
			ref:     anchor.Ref{Line: 2, Hash: anchor.Of("beta")},
			want:    anchor.Range{Start: 2, End: 2},
		},
		{
			name:    "exact range hit",
			content: "alpha\nbeta\ngamma\n",
			ref:     anchor.Ref{Line: 1, End: 3, Hash: anchor.Of("alpha")},
			want:    anchor.Range{Start: 1, End: 3},
		},
		{
			name:     "moved by insert above",
			content:  "new\nalpha\nbeta\n",
			ref:      anchor.Ref{Line: 1, Hash: anchor.Of("alpha")},
			want:     anchor.Range{Start: 2, End: 2},
			wantMove: &anchor.Move{From: 1, To: 2},
		},
		{
			name:     "moved range shifts end",
			content:  "new\nalpha\nbeta\ngamma\n",
			ref:      anchor.Ref{Line: 1, End: 2, Hash: anchor.Of("alpha")},
			want:     anchor.Range{Start: 2, End: 3},
			wantMove: &anchor.Move{From: 1, To: 2},
		},
		{
			name:     "duplicates pick nearest",
			content:  "dup\nx\ny\ndup\nz\ndup\n",
			ref:      anchor.Ref{Line: 2, Hash: anchor.Of("dup")},
			want:     anchor.Range{Start: 1, End: 1},
			wantMove: &anchor.Move{From: 2, To: 1},
		},
		{
			name:     "duplicate tie goes earlier",
			content:  "dup\nx\ny\ndup\nz\ndup\n",
			ref:      anchor.Ref{Line: 5, Hash: anchor.Of("dup")},
			want:     anchor.Range{Start: 4, End: 4},
			wantMove: &anchor.Move{From: 5, To: 4},
		},
		{
			name:    "deleted content",
			content: "alpha\nbeta\n",
			ref:     anchor.Ref{Line: 1, Hash: anchor.Of("gone")},
			wantErr: "not found",
		},
		{
			name:    "crlf exact hit",
			content: "alpha\r\nbeta\r\ngamma\r\n",
			ref:     anchor.Ref{Line: 2, Hash: anchor.Of("beta")},
			want:    anchor.Range{Start: 2, End: 2},
		},
		{
			name:    "range end clamps to eof",
			content: "alpha\nbeta\ngamma\n",
			ref:     anchor.Ref{Line: 2, End: 99, Hash: anchor.Of("beta")},
			want:    anchor.Range{Start: 2, End: 3},
		},
		{
			name:    "range end clamps at missing trailing newline",
			content: "alpha\nbeta\ngamma",
			ref:     anchor.Ref{Line: 1, End: 99, Hash: anchor.Of("alpha")},
			want:    anchor.Range{Start: 1, End: 3},
		},
		{
			name:     "huge range end clamps to eof without overflow",
			content:  "new\nalpha\nb\nc\nd\n",
			ref:      anchor.Ref{Line: 1, End: math.MaxInt, Hash: anchor.Of("alpha")},
			want:     anchor.Range{Start: 2, End: 5},
			wantMove: &anchor.Move{From: 1, To: 2},
		},
		{
			name:    "bare anchor unique",
			content: "alpha\nbeta\ngamma\n",
			ref:     anchor.Ref{Hash: anchor.Of("gamma")},
			want:    anchor.Range{Start: 3, End: 3},
		},
		{
			name:    "bare anchor ambiguous",
			content: "dup\nx\ndup\n",
			ref:     anchor.Ref{Hash: anchor.Of("dup")},
			wantErr: "lines 1, 3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.txt")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			f, err := anchor.Load(path)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			got, move, err := f.Resolve(tt.ref)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Resolve() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Resolve() range = %+v, want %+v", got, tt.want)
			}
			switch {
			case tt.wantMove == nil && move != nil:
				t.Errorf("Resolve() move = %+v, want nil", move)
			case tt.wantMove != nil && move == nil:
				t.Errorf("Resolve() move = nil, want %+v", tt.wantMove)
			case tt.wantMove != nil && *move != *tt.wantMove:
				t.Errorf("Resolve() move = %+v, want %+v", move, tt.wantMove)
			}
		})
	}
}
