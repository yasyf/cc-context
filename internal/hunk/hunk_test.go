package hunk_test

import (
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/hunk"
)

// projected is the exported view of a Hunk (no unexported bookkeeping), so the
// table can assert coordinates and text without reaching into the struct's
// EOF-newline flag, which the Select round-trips exercise instead.
type projected struct {
	OldStart, OldEnd, NewStart, NewEnd int
	Old, New                           []string
}

func project(h hunk.Hunk) projected {
	return projected{h.OldStart, h.OldEnd, h.NewStart, h.NewEnd, h.Old, h.New}
}

// mark rebuilds a hunk's digest input independently of the package: every Old
// line prefixed "-", every New line prefixed "+", old first.
func mark(old, added []string) []string {
	m := make([]string, 0, len(old)+len(added))
	for _, o := range old {
		m = append(m, "-"+o)
	}
	for _, n := range added {
		m = append(m, "+"+n)
	}
	return m
}

type computeCase struct {
	name string
	base string
	cur  string
	want []projected
}

// computeCases is the shared corpus for TestCompute and TestSelectRoundTrip: the
// hunk projections are pinned here and the same base/cur drive the round-trip
// invariants.
var computeCases = []computeCase{
	{
		name: "single hunk",
		base: "a\nb\nc\n", cur: "a\nB\nc\n",
		want: []projected{{2, 2, 2, 2, []string{"b"}, []string{"B"}}},
	},
	{
		name: "adjacent changes merge into one hunk",
		base: "a\nb\nc\nd\n", cur: "a\nB\nC\nd\n",
		want: []projected{{2, 3, 2, 3, []string{"b", "c"}, []string{"B", "C"}}},
	},
	{
		name: "separated changes are distinct hunks",
		base: "a\nb\nc\nd\ne\n", cur: "a\nB\nc\nD\ne\n",
		want: []projected{
			{2, 2, 2, 2, []string{"b"}, []string{"B"}},
			{4, 4, 4, 4, []string{"d"}, []string{"D"}},
		},
	},
	{
		name: "add-only edit",
		base: "a\nb\nc\n", cur: "a\nb\nX\nY\nc\n",
		want: []projected{{3, 2, 3, 4, nil, []string{"X", "Y"}}},
	},
	{
		name: "delete-only edit",
		base: "a\nb\nc\nd\n", cur: "a\nd\n",
		want: []projected{{2, 3, 2, 1, []string{"b", "c"}, nil}},
	},
	{
		name: "pure insertion at start",
		base: "b\nc\n", cur: "a\nb\nc\n",
		want: []projected{{1, 0, 1, 1, nil, []string{"a"}}},
	},
	{
		name: "pure insertion at eof",
		base: "a\nb\n", cur: "a\nb\nc\n",
		want: []projected{{3, 2, 3, 3, nil, []string{"c"}}},
	},
	{
		name: "pure deletion at start",
		base: "a\nb\nc\n", cur: "b\nc\n",
		want: []projected{{1, 1, 1, 0, []string{"a"}, nil}},
	},
	{
		name: "pure deletion at eof",
		base: "a\nb\nc\n", cur: "a\nb\n",
		want: []projected{{3, 3, 3, 2, []string{"c"}, nil}},
	},
	{
		name: "eof newline added",
		base: "a\nb", cur: "a\nb\n",
		want: []projected{{2, 2, 2, 2, []string{"b"}, []string{"b"}}},
	},
	{
		name: "eof newline removed",
		base: "a\nb\n", cur: "a\nb",
		want: []projected{{2, 2, 2, 2, []string{"b"}, []string{"b"}}},
	},
	{
		name: "insertion at eof of newline-less base",
		base: "a\nb", cur: "a\nb\nc",
		want: []projected{{2, 2, 2, 3, []string{"b"}, []string{"b", "c"}}},
	},
	{
		name: "crlf stays inside line text",
		base: "a\r\nb\r\n", cur: "a\r\nB\r\n",
		want: []projected{{2, 2, 2, 2, []string{"b\r"}, []string{"B\r"}}},
	},
	{
		name: "empty base is one whole-file insertion",
		base: "", cur: "a\nb\n",
		want: []projected{{1, 0, 1, 2, nil, []string{"a", "b"}}},
	},
	{
		name: "empty current deletes the whole file",
		base: "a\nb\n", cur: "",
		want: []projected{{1, 2, 1, 0, []string{"a", "b"}, nil}},
	},
	{
		name: "identical inputs yield no hunks",
		base: "a\nb\n", cur: "a\nb\n",
		want: nil,
	},
}

func TestCompute(t *testing.T) {
	for _, tt := range computeCases {
		t.Run(tt.name, func(t *testing.T) {
			got := hunk.Compute([]byte(tt.base), []byte(tt.cur))
			if len(got) != len(tt.want) {
				t.Fatalf("Compute produced %d hunks, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, h := range got {
				if p := project(h); !reflect.DeepEqual(p, tt.want[i]) {
					t.Errorf("hunk %d = %+v, want %+v", i, p, tt.want[i])
				}
				if wantDigest := anchor.OfLines(mark(tt.want[i].Old, tt.want[i].New)); h.Digest != wantDigest {
					t.Errorf("hunk %d digest = %q, want %q", i, h.Digest, wantDigest)
				}
			}
		})
	}
}

// TestSelectRoundTrip pins the two hard invariants for every corpus case: keeping
// all hunks reproduces current byte-for-byte, keeping none reproduces base.
func TestSelectRoundTrip(t *testing.T) {
	for _, tt := range computeCases {
		t.Run(tt.name, func(t *testing.T) {
			base, cur := []byte(tt.base), []byte(tt.cur)
			hunks := hunk.Compute(base, cur)

			if all := hunk.Select(base, hunks, func(int) bool { return true }); string(all) != tt.cur {
				t.Errorf("Select(all) = %q, want current %q", all, tt.cur)
			}
			if none := hunk.Select(base, hunks, func(int) bool { return false }); string(none) != tt.base {
				t.Errorf("Select(none) = %q, want base %q", none, tt.base)
			}
		})
	}
}

// TestSelectPartial pins per-hunk selection on the two-hunk corpus case: keeping
// exactly one hunk applies only that change.
func TestSelectPartial(t *testing.T) {
	base := []byte("a\nb\nc\nd\ne\n")
	cur := []byte("a\nB\nc\nD\ne\n")
	hunks := hunk.Compute(base, cur)
	if len(hunks) != 2 {
		t.Fatalf("expected 2 hunks, got %d", len(hunks))
	}
	tests := []struct {
		name string
		keep map[int]bool
		want string
	}{
		{"first only", map[int]bool{0: true}, "a\nB\nc\nd\ne\n"},
		{"second only", map[int]bool{1: true}, "a\nb\nc\nD\ne\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hunk.Select(base, hunks, func(i int) bool { return tt.keep[i] })
			if string(got) != tt.want {
				t.Errorf("Select = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDigest locks the digest contract: it is exactly OfLines over the kind-marked
// lines, distinguishes a deletion from an addition of the same text, is stable
// when the hunk shifts position, and is sensitive to content.
func TestDigest(t *testing.T) {
	del := hunk.Compute([]byte("X\n"), []byte(""))[0]
	add := hunk.Compute([]byte(""), []byte("X\n"))[0]

	if want := anchor.OfLines([]string{"-X"}); del.Digest != want {
		t.Errorf("delete digest = %q, want %q", del.Digest, want)
	}
	if want := anchor.OfLines([]string{"+X"}); add.Digest != want {
		t.Errorf("add digest = %q, want %q", add.Digest, want)
	}
	if del.Digest == add.Digest {
		t.Errorf("delete-X and add-X share digest %q — kind must matter", del.Digest)
	}

	near := hunk.Compute([]byte("a\nX\n"), []byte("a\nY\n"))[0]
	far := hunk.Compute([]byte("a\nb\nc\nX\n"), []byte("a\nb\nc\nY\n"))[0]
	if near.NewStart == far.NewStart {
		t.Fatalf("test setup: hunks must sit at different positions (%d)", near.NewStart)
	}
	if near.Digest != far.Digest {
		t.Errorf("same change at different positions digested %q vs %q — must match", near.Digest, far.Digest)
	}

	other := hunk.Compute([]byte("a\nX\n"), []byte("a\nZ\n"))[0]
	if near.Digest == other.Digest {
		t.Errorf("different content shares digest %q — must differ", near.Digest)
	}
}

func TestParseRef(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantPath string
		wantRef  anchor.Ref
		wantErr  bool
	}{
		{"range and hash", "a.go:120-180#a3fk", "a.go", anchor.Ref{Line: 120, End: 180, Hash: "a3fk"}, false},
		{"single line and hash", "internal/cli/ship.go:530#k2fa", "internal/cli/ship.go", anchor.Ref{Line: 530, Hash: "k2fa"}, false},
		{"missing hash", "a.go:10-20", "", anchor.Ref{}, true},
		{"no range bare hash", "a.go:a3fk", "", anchor.Ref{}, true},
		{"bad path no colon", "README", "", anchor.Ref{}, true},
		{"malformed hash", "a.go:10#zz", "", anchor.Ref{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, ref, err := hunk.ParseRef(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRef(%q) = (%q, %+v, nil), want error", tt.in, path, ref)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRef(%q) error: %v", tt.in, err)
			}
			if path != tt.wantPath || ref != tt.wantRef {
				t.Errorf("ParseRef(%q) = (%q, %+v), want (%q, %+v)", tt.in, path, ref, tt.wantPath, tt.wantRef)
			}
		})
	}
}
