package cli_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/cli"
)

// TestAnchorHashArg proves `anchor hash <text>` prints exactly the content hash
// anchor.Of derives for that line.
func TestAnchorHashArg(t *testing.T) {
	got := runCCX(t, "anchor", "hash", "func Foo()")
	want := anchor.Of("func Foo()").String() + "\n"
	if got != want {
		t.Errorf("anchor hash arg = %q, want %q", got, want)
	}
}

// TestAnchorHashStdin proves an absent arg and a bare "-" both read the line from
// stdin, matching the --content - convention.
func TestAnchorHashStdin(t *testing.T) {
	for _, args := range [][]string{{"anchor", "hash"}, {"anchor", "hash", "-"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			root := cli.NewRootCmd()
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetIn(strings.NewReader("beta\n"))
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute(%v) error = %v", args, err)
			}
			want := anchor.Of("beta").String() + "\n"
			if out.String() != want {
				t.Errorf("anchor hash stdin = %q, want %q", out.String(), want)
			}
		})
	}
}

// TestAnchorResolve drives the resolve verb over a three-line fixture: an exact
// line+hash and a unique bare hash resolve silently, while a stale line hint
// re-anchors by content and prints the move note.
func TestAnchorResolve(t *testing.T) {
	file := writeAnchorFixture(t)
	beta := anchor.Of("beta")
	gamma := anchor.Of("gamma")

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"line+hash exact", anchor.Format(2, beta), fmt.Sprintf("2-2#%s\n", beta)},
		{"bare hash unique", beta.String(), fmt.Sprintf("2-2#%s\n", beta)},
		{"stale hint re-anchors", anchor.Format(2, gamma), fmt.Sprintf("3-3#%s\n# anchor %s: line 2 → 3\n", gamma, gamma)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runCCX(t, "anchor", "resolve", file, tt.ref); got != tt.want {
				t.Errorf("anchor resolve %q = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

// TestAnchorResolveErrors proves each malformed, missing, or ambiguous ref exits
// non-zero carrying the anchor package's own error text.
func TestAnchorResolveErrors(t *testing.T) {
	file := writeAnchorFixture(t)
	dup := filepath.Join(t.TempDir(), "dup.txt")
	if err := os.WriteFile(dup, []byte("same\nsame\n"), 0o600); err != nil {
		t.Fatalf("write dup fixture: %v", err)
	}

	tests := []struct {
		name string
		file string
		ref  string
		want string
	}{
		{"not an anchor ref", file, "40-95", "not an anchor ref"},
		{"malformed hash", file, "120#zz", "invalid anchor"},
		{"content not found", file, anchor.Of("nowhere near the fixture").String(), "not found"},
		{"ambiguous bare anchor", dup, anchor.Of("same").String(), "matches lines"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			root := cli.NewRootCmd()
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"anchor", "resolve", tt.file, tt.ref})
			err := root.Execute()
			if err == nil {
				t.Fatalf("Execute(anchor resolve %q) err = nil, want error containing %q\n%s", tt.ref, tt.want, out.String())
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.want)
			}
		})
	}
}
