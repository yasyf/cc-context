package edit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/edit"
)

// ref renders a resolved range the way edit's report does, so expected output is
// built from the same anchors the code emits.
func ref(start, end int, h anchor.Hash) string {
	if start == end {
		return anchor.Format(start, h)
	}
	return anchor.FormatRange(start, end, h)
}

func TestRun(t *testing.T) {
	of := anchor.Of
	tests := []struct {
		name     string
		content  string
		section  string
		editArg  string
		delete   bool
		wantFile string
		wantOut  func(path string) string
		wantErr  string
	}{
		{
			name:     "exact anchored hit",
			content:  "alpha\nbeta\ngamma\n",
			section:  anchor.Format(2, of("beta")),
			editArg:  "BETA",
			wantFile: "alpha\nBETA\ngamma\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("beta")) + " → " + p + ":" + ref(2, 2, of("BETA")) + "\n- beta\n+ BETA\n"
			},
		},
		{
			name:     "moved anchor prepends note",
			content:  "new\nalpha\nbeta\n",
			section:  anchor.Format(1, of("alpha")),
			editArg:  "ALPHA",
			wantFile: "new\nALPHA\nbeta\n",
			wantOut: func(p string) string {
				return "# anchor " + of("alpha").String() + ": line 1 → 2\n" +
					p + ":" + ref(2, 2, of("alpha")) + " → " + p + ":" + ref(2, 2, of("ALPHA")) + "\n- alpha\n+ ALPHA\n"
			},
		},
		{
			name:     "numeric range edit",
			content:  "a\nb\nc\nd\n",
			section:  "2-3",
			editArg:  "X\nY\nZ",
			wantFile: "a\nX\nY\nZ\nd\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 3, of("b")) + " → " + p + ":" + ref(2, 4, of("X")) + "\n- b\n- c\n+ X\n+ Y\n+ Z\n"
			},
		},
		{
			name:     "comma range edit aliases dash range",
			content:  "a\nb\nc\nd\n",
			section:  "2, 3",
			editArg:  "X\nY\nZ",
			wantFile: "a\nX\nY\nZ\nd\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 3, of("b")) + " → " + p + ":" + ref(2, 4, of("X")) + "\n- b\n- c\n+ X\n+ Y\n+ Z\n"
			},
		},
		{
			name:     "trailing newline terminates content without adding a blank line",
			content:  "a\nb\nc\n",
			section:  "2",
			editArg:  "X\nY\n",
			wantFile: "a\nX\nY\nc\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 3, of("X")) + "\n- b\n+ X\n+ Y\n"
			},
		},
		{
			name:     "delete splice-point anchor",
			content:  "a\nb\nc\n",
			section:  anchor.Format(2, of("b")),
			delete:   true,
			wantFile: "a\nc\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 2, of("c")) + "\n- b\n"
			},
		},
		{
			name:     "delete last line anchors new last",
			content:  "a\nb\nc\n",
			section:  anchor.Format(3, of("c")),
			delete:   true,
			wantFile: "a\nb\n",
			wantOut: func(p string) string {
				return p + ":" + ref(3, 3, of("c")) + " → " + p + ":" + ref(2, 2, of("b")) + "\n- c\n"
			},
		},
		{
			name:     "delete all leaves empty file",
			content:  "a\nb\n",
			section:  "1-2",
			delete:   true,
			wantFile: "",
			wantOut: func(p string) string {
				return p + ":" + ref(1, 2, of("a")) + " → (empty)\n- a\n- b\n"
			},
		},
		{
			name:     "crlf round-trip",
			content:  "alpha\r\nbeta\r\ngamma\r\n",
			section:  anchor.Format(2, of("beta")),
			editArg:  "BETA",
			wantFile: "alpha\r\nBETA\r\ngamma\r\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("beta")) + " → " + p + ":" + ref(2, 2, of("BETA")) + "\n- beta\n+ BETA\n"
			},
		},
		{
			name:     "no trailing newline round-trip",
			content:  "a\nb\nc",
			section:  "2",
			editArg:  "B",
			wantFile: "a\nB\nc",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 2, of("B")) + "\n- b\n+ B\n"
			},
		},
		{
			name:     "blank edit inserts one empty line",
			content:  "a\nb\nc\n",
			section:  "2",
			editArg:  "",
			wantFile: "a\n\nc\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 2, of("")) + "\n- b\n+ \n"
			},
		},
		{
			name:     "trailing spaces on a single line land on disk",
			content:  "a\nb\nc\n",
			section:  "2",
			editArg:  "B  ",
			wantFile: "a\nB  \nc\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 2, of("B  ")) + "\n- b\n+ B  \n"
			},
		},
		{
			name:     "interior trailing space and trailing tab land on disk",
			content:  "a\nb\nc\n",
			section:  "2",
			editArg:  "X \nY\t",
			wantFile: "a\nX \nY\t\nc\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 3, of("X ")) + "\n- b\n+ X \n+ Y\t\n"
			},
		},
		{
			name:    "vanished anchor errors before write",
			content: "alpha\nbeta\n",
			section: anchor.Format(1, of("gone")),
			editArg: "X",
			wantErr: "not found",
		},
		{
			name:    "ambiguous bare anchor errors before write",
			content: "dup\nx\ndup\n",
			section: of("dup").String(),
			editArg: "X",
			wantErr: "matches lines",
		},
		{
			name:    "numeric range out of bounds errors",
			content: "a\nb\nc\n",
			section: "5-7",
			editArg: "X",
			wantErr: "out of bounds",
		},
		{
			name:    "non-anchor non-numeric section errors naming both range forms",
			content: "a\nb\n",
			section: "## Heading",
			editArg: "X",
			wantErr: `("A-B" or "A,B")`,
		},
		{
			name:    "three-part comma is still invalid",
			content: "a\nb\nc\n",
			section: "1,2,3",
			editArg: "X",
			wantErr: `("A-B" or "A,B")`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.txt")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			prePerm := statPerm(t, path)
			a := backend.Args{Path: path, Section: tt.section, Content: tt.editArg, Delete: tt.delete}

			out, err := edit.Run(a)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Run() error = nil, want containing %q", tt.wantErr)
				}
				if got := err.Error(); !strings.Contains(got, tt.wantErr) {
					t.Fatalf("Run() error = %q, want containing %q", got, tt.wantErr)
				}
				assertFileBytes(t, path, tt.content)
				return
			}
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if want := tt.wantOut(path); out != want {
				t.Errorf("Run() output =\n%q\nwant\n%q", out, want)
			}
			assertFileBytes(t, path, tt.wantFile)
			if got := statPerm(t, path); got != prePerm {
				t.Errorf("file perm = %o, want preserved %o", got, prePerm)
			}
		})
	}
}

func TestRunMatch(t *testing.T) {
	of := anchor.Of
	tests := []struct {
		name     string
		content  string
		match    string
		editArg  string
		section  string
		all      bool
		delete   bool
		wantFile string
		wantOut  func(path string) string
		wantErr  string
	}{
		{
			name:     "whole-line match replace",
			content:  "alpha\nbeta\ngamma\n",
			match:    "beta",
			editArg:  "BETA",
			wantFile: "alpha\nBETA\ngamma\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("beta")) + " → " + p + ":" + ref(2, 2, of("BETA")) + "\n- beta\n+ BETA\n"
			},
		},
		{
			name:     "substring match shows whole affected line",
			content:  "hello world\n",
			match:    "world",
			editArg:  "there",
			wantFile: "hello there\n",
			wantOut: func(p string) string {
				return p + ":" + ref(1, 1, of("hello world")) + " → " + p + ":" + ref(1, 1, of("hello there")) + "\n- hello world\n+ hello there\n"
			},
		},
		{
			name:     "multi-line needle",
			content:  "a\nb\nc\nd\n",
			match:    "b\nc",
			editArg:  "X",
			wantFile: "a\nX\nd\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 3, of("b")) + " → " + p + ":" + ref(2, 2, of("X")) + "\n- b\n- c\n+ X\n"
			},
		},
		{
			name:     "multi-line needle normalizes to CRLF",
			content:  "a\r\nb\r\nc\r\n",
			match:    "a\nb",
			editArg:  "X",
			wantFile: "X\r\nc\r\n",
			wantOut: func(p string) string {
				return p + ":" + ref(1, 2, of("a")) + " → " + p + ":" + ref(1, 1, of("X")) + "\n- a\n- b\n+ X\n"
			},
		},
		{
			name:     "replacement trailing spaces land byte-exact",
			content:  "a\nb\nc\n",
			match:    "b",
			editArg:  "B  ",
			wantFile: "a\nB  \nc\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 2, of("B  ")) + "\n- b\n+ B  \n"
			},
		},
		{
			name:     "content ending in newline written verbatim",
			content:  "a\nb\nc\n",
			match:    "b",
			editArg:  "B\n",
			wantFile: "a\nB\n\nc\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 2, of("B")) + "\n- b\n+ B\n"
			},
		},
		{
			name:    "zero matches errors",
			content: "a\nb\nc\n",
			match:   "zzz",
			editArg: "X",
			wantErr: "--match not found",
		},
		{
			name:    "ambiguous match lists candidate anchors",
			content: "x\ny\nx\n",
			match:   "x",
			editArg: "Z",
			wantErr: "--match found 2 matches (" + anchor.Format(1, of("x")) + ", " + anchor.Format(3, of("x")) + "); scope with --at, extend the match, or pass --all",
		},
		{
			name:     "all replaces every occurrence",
			content:  "x\ny\nx\n",
			match:    "x",
			editArg:  "Z",
			all:      true,
			wantFile: "Z\ny\nZ\n",
			wantOut: func(p string) string {
				return "# 2 occurrences replaced\n" +
					p + ":" + ref(1, 1, of("x")) + " → " + p + ":" + ref(1, 1, of("Z")) + "\n- x\n+ Z\n\n" +
					p + ":" + ref(3, 3, of("x")) + " → " + p + ":" + ref(3, 3, of("Z")) + "\n- x\n+ Z\n"
			},
		},
		{
			name:     "all with two hits on same line shows final line in both stanzas",
			content:  "a a\n",
			match:    "a",
			editArg:  "X",
			all:      true,
			wantFile: "X X\n",
			wantOut: func(p string) string {
				stanza := p + ":" + ref(1, 1, of("a a")) + " → " + p + ":" + ref(1, 1, of("X X")) + "\n- a a\n+ X X\n"
				return "# 2 occurrences replaced\n" + stanza + "\n" + stanza
			},
		},
		{
			name:     "at-scoped match replaces only the in-span occurrence",
			content:  "x\ny\nx\n",
			match:    "x",
			editArg:  "Z",
			section:  "1",
			wantFile: "Z\ny\nx\n",
			wantOut: func(p string) string {
				return p + ":" + ref(1, 1, of("x")) + " → " + p + ":" + ref(1, 1, of("Z")) + "\n- x\n+ Z\n"
			},
		},
		{
			name:     "at-scoped moved anchor prepends the move note",
			content:  "new\nalpha\nbeta\n",
			match:    "alpha",
			editArg:  "ALPHA",
			section:  anchor.Format(1, of("alpha")),
			wantFile: "new\nALPHA\nbeta\n",
			wantOut: func(p string) string {
				return "# anchor " + of("alpha").String() + ": line 1 → 2\n" +
					p + ":" + ref(2, 2, of("alpha")) + " → " + p + ":" + ref(2, 2, of("ALPHA")) + "\n- alpha\n+ ALPHA\n"
			},
		},
		{
			name:    "scoped zero-in-span error names the span",
			content: "x\ny\nx\n",
			match:   "x",
			editArg: "Z",
			section: "2",
			wantErr: "--match not found in " + anchor.Format(2, of("y")),
		},
		{
			name:     "delete via match removes the bytes",
			content:  "a\nb\nc\n",
			match:    "b\n",
			delete:   true,
			wantFile: "a\nc\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 2, of("c")) + "\n- b\n"
			},
		},
		{
			name:    "content equal to match errors",
			content: "a\nb\nc\n",
			match:   "b",
			editArg: "b",
			wantErr: "nothing to change",
		},
		{
			name:     "aa in aaa replaces once",
			content:  "aaa\n",
			match:    "aa",
			editArg:  "b",
			wantFile: "ba\n",
			wantOut: func(p string) string {
				return p + ":" + ref(1, 1, of("aaa")) + " → " + p + ":" + ref(1, 1, of("ba")) + "\n- aaa\n+ ba\n"
			},
		},
		{
			name:     "fused delete-all no panic",
			content:  "a\nba\nb\n",
			match:    "a\nb",
			delete:   true,
			all:      true,
			wantFile: "\n",
			wantOut: func(p string) string {
				s1 := p + ":" + ref(1, 2, of("a")) + " → " + p + ":" + ref(1, 1, of("")) + "\n- a\n- ba\n"
				s2 := p + ":" + ref(2, 3, of("ba")) + " → " + p + ":" + ref(1, 1, of("")) + "\n- ba\n- b\n"
				return "# 2 occurrences replaced\n" + s1 + "\n" + s2
			},
		},
		{
			name:     "fused replace-all shows final lines",
			content:  "AAAA\nb AAAA\nb\n",
			match:    "AAAA\nb",
			editArg:  "X",
			all:      true,
			wantFile: "X X\n",
			wantOut: func(p string) string {
				s1 := p + ":" + ref(1, 2, of("AAAA")) + " → " + p + ":" + ref(1, 1, of("X X")) + "\n- AAAA\n- b AAAA\n+ X X\n"
				s2 := p + ":" + ref(2, 3, of("b AAAA")) + " → " + p + ":" + ref(1, 1, of("X X")) + "\n- b AAAA\n- b\n+ X X\n"
				return "# 2 occurrences replaced\n" + s1 + "\n" + s2
			},
		},
		{
			name:     "fused single-match shows final line",
			content:  "a\nb\nc\n",
			match:    "b\n",
			editArg:  "B",
			section:  "2",
			wantFile: "a\nBc\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 2, of("Bc")) + "\n- b\n+ Bc\n"
			},
		},
		{
			name:     "crlf standalone cr in needle matches bare cr",
			content:  "a\r\nb\r\n",
			match:    "b\r",
			editArg:  "X",
			wantFile: "a\r\nX\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 2, of("X")) + "\n- b\n+ X\n"
			},
		},
		{
			name:     "crlf standalone cr in content preserved",
			content:  "a\r\nb\r\n",
			match:    "b",
			editArg:  "B\r",
			wantFile: "a\r\nB\r\r\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 2, of("B")) + "\n- b\n+ B\r\n"
			},
		},
		{
			name:     "scoped moved anchor note carries the supplied hash not the match line",
			content:  "new\nscope\ninside\n",
			match:    "inside",
			editArg:  "INSIDE",
			section:  anchor.FormatRange(1, 2, of("scope")),
			wantFile: "new\nscope\nINSIDE\n",
			wantOut: func(p string) string {
				return "# anchor " + of("scope").String() + ": line 1 → 2\n" +
					p + ":" + ref(3, 3, of("inside")) + " → " + p + ":" + ref(3, 3, of("INSIDE")) + "\n- inside\n+ INSIDE\n"
			},
		},
		{
			name:     "all with scoped moved anchor emits note before header",
			content:  "new\nalpha\nx\nx\n",
			match:    "x",
			editArg:  "Z",
			section:  anchor.FormatRange(1, 3, of("alpha")),
			all:      true,
			wantFile: "new\nalpha\nZ\nZ\n",
			wantOut: func(p string) string {
				s1 := p + ":" + ref(3, 3, of("x")) + " → " + p + ":" + ref(3, 3, of("Z")) + "\n- x\n+ Z\n"
				s2 := p + ":" + ref(4, 4, of("x")) + " → " + p + ":" + ref(4, 4, of("Z")) + "\n- x\n+ Z\n"
				return "# anchor " + of("alpha").String() + ": line 1 → 2\n" +
					"# 2 occurrences replaced\n" + s1 + "\n" + s2
			},
		},
		{
			name:     "all with one match emits singular header",
			content:  "x\n",
			match:    "x",
			editArg:  "Z",
			all:      true,
			wantFile: "Z\n",
			wantOut: func(p string) string {
				return "# 1 occurrence replaced\n" +
					p + ":" + ref(1, 1, of("x")) + " → " + p + ":" + ref(1, 1, of("Z")) + "\n- x\n+ Z\n"
			},
		},
		{
			name:     "all unequal-length replacement keeps final coordinates",
			content:  "aaa\nbbb\naaa\n",
			match:    "aaa",
			editArg:  "LONGREPL",
			all:      true,
			wantFile: "LONGREPL\nbbb\nLONGREPL\n",
			wantOut: func(p string) string {
				s1 := p + ":" + ref(1, 1, of("aaa")) + " → " + p + ":" + ref(1, 1, of("LONGREPL")) + "\n- aaa\n+ LONGREPL\n"
				s2 := p + ":" + ref(3, 3, of("aaa")) + " → " + p + ":" + ref(3, 3, of("LONGREPL")) + "\n- aaa\n+ LONGREPL\n"
				return "# 2 occurrences replaced\n" + s1 + "\n" + s2
			},
		},
		{
			name:     "crlf multi-line replacement normalizes each new line",
			content:  "a\r\nb\r\nc\r\n",
			match:    "b",
			editArg:  "X\nY",
			wantFile: "a\r\nX\r\nY\r\nc\r\n",
			wantOut: func(p string) string {
				return p + ":" + ref(2, 2, of("b")) + " → " + p + ":" + ref(2, 3, of("X")) + "\n- b\n+ X\n+ Y\n"
			},
		},
		{
			name:    "all without match errors",
			content: "a\nb\n",
			all:     true,
			wantErr: "--all requires --match",
		},
		{
			name:    "neither at nor match errors",
			content: "a\nb\n",
			wantErr: "provide --at, --match, or both",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.txt")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			prePerm := statPerm(t, path)
			a := backend.Args{Path: path, Section: tt.section, Match: tt.match, Content: tt.editArg, All: tt.all, Delete: tt.delete}

			out, err := edit.Run(a)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Run() error = nil, want containing %q", tt.wantErr)
				}
				if got := err.Error(); !strings.Contains(got, tt.wantErr) {
					t.Fatalf("Run() error = %q, want containing %q", got, tt.wantErr)
				}
				assertFileBytes(t, path, tt.content)
				return
			}
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if want := tt.wantOut(path); out != want {
				t.Errorf("Run() output =\n%q\nwant\n%q", out, want)
			}
			assertFileBytes(t, path, tt.wantFile)
			if got := statPerm(t, path); got != prePerm {
				t.Errorf("file perm = %o, want preserved %o", got, prePerm)
			}
		})
	}
}

// TestRunDeleteWithContentRejected proves the only representable invalid state —
// a delete carrying replacement content — errors without reading or writing.
func TestRunDeleteWithContentRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	const content = "a\nb\nc\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := edit.Run(backend.Args{Path: path, Section: "2", Content: "X", Delete: true})
	if err == nil || !strings.Contains(err.Error(), "--delete takes no content") {
		t.Fatalf("Run() error = %v, want containing %q", err, "--delete takes no content")
	}
	assertFileBytes(t, path, content)
}

// TestRunThroughSymlink proves editing a symlink writes through to the real
// target and leaves the symlink intact — the atomic temp+rename must not replace
// the link inode with a regular file (the repo ships plugin/bin/ccx as a symlink).
func TestRunThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	// 0o400 differs from os.CreateTemp's 0o600 default, so the assertion below
	// catches a regression that drops cache.Store's mode restore.
	if err := os.WriteFile(target, []byte("alpha\nbeta\ngamma\n"), 0o400); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	prePerm := statPerm(t, target)

	beta := anchor.Of("beta")
	if _, err := edit.Run(backend.Args{Path: link, Section: anchor.Format(2, beta), Content: "BETA"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	assertFileBytes(t, target, "alpha\nBETA\ngamma\n")

	li, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if li.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link is no longer a symlink: mode %v", li.Mode())
	}
	if got := statPerm(t, target); got != prePerm {
		t.Errorf("target perm = %o, want preserved %o", got, prePerm)
	}
}

// TestRunDanglingSymlink proves a symlink to a nonexistent target errors cleanly
// — the path names the caller's a.Path — and never creates the missing target.
func TestRunDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.txt")
	link := filepath.Join(dir, "dangling.txt")
	if err := os.Symlink(missing, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := edit.Run(backend.Args{Path: link, Section: "1", Content: "X"})
	if err == nil {
		t.Fatal("Run() error = nil, want an error for a dangling symlink")
	}
	if !strings.Contains(err.Error(), link) {
		t.Errorf("Run() error = %q, want it to name the caller path %q", err.Error(), link)
	}
	if _, statErr := os.Lstat(missing); !os.IsNotExist(statErr) {
		t.Errorf("target should not have been created, lstat err = %v", statErr)
	}
}

func assertFileBytes(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("file bytes = %q, want %q", got, want)
	}
}

func statPerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
