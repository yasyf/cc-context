package anchor_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/backend"
)

func writeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestRewriteArgs(t *testing.T) {
	path := writeFixture(t)
	beta := anchor.Of("beta")
	gamma := anchor.Of("gamma")

	tests := []struct {
		name     string
		op       backend.Op
		args     backend.Args
		want     backend.Args
		wantNote string
		wantErr  string
	}{
		{
			name: "read exact anchor",
			op:   backend.OpRead,
			args: backend.Args{Path: path, Section: anchor.Format(2, beta)},
			want: backend.Args{Path: path, Section: "2-2"},
		},
		{
			name:     "read moved anchor",
			op:       backend.OpRead,
			args:     backend.Args{Path: path, Section: anchor.Format(1, gamma)},
			want:     backend.Args{Path: path, Section: "3-3"},
			wantNote: fmt.Sprintf("# anchor %s: line 1 → 3\n", gamma),
		},
		{
			name: "read range anchor",
			op:   backend.OpRead,
			args: backend.Args{Path: path, Section: anchor.FormatRange(2, 3, beta)},
			want: backend.Args{Path: path, Section: "2-3"},
		},
		{
			name: "read numeric passthrough",
			op:   backend.OpRead,
			args: backend.Args{Path: path, Section: "5-7"},
			want: backend.Args{Path: path, Section: "5-7"},
		},
		{
			name: "read comma range normalizes to dash",
			op:   backend.OpRead,
			args: backend.Args{Path: path, Section: "5,7"},
			want: backend.Args{Path: path, Section: "5-7"},
		},
		{
			name: "read comma range with space normalizes to dash",
			op:   backend.OpRead,
			args: backend.Args{Path: path, Section: "5, 7"},
			want: backend.Args{Path: path, Section: "5-7"},
		},
		{
			name: "read heading passthrough",
			op:   backend.OpRead,
			args: backend.Args{Path: path, Section: "## Heading"},
			want: backend.Args{Path: path, Section: "## Heading"},
		},
		{
			name: "read heading with comma passthrough",
			op:   backend.OpRead,
			args: backend.Args{Path: path, Section: "## Foo, Bar"},
			want: backend.Args{Path: path, Section: "## Foo, Bar"},
		},
		{
			name: "read three-part comma passthrough",
			op:   backend.OpRead,
			args: backend.Args{Path: path, Section: "5,7,9"},
			want: backend.Args{Path: path, Section: "5,7,9"},
		},
		{
			name: "read full skips anchor",
			op:   backend.OpRead,
			args: backend.Args{Path: path, Section: anchor.Format(2, beta), Full: true},
			want: backend.Args{Path: path, Section: anchor.Format(2, beta), Full: true},
		},
		{
			name:    "read malformed anchor",
			op:      backend.OpRead,
			args:    backend.Args{Path: path, Section: "120#zz"},
			wantErr: "invalid anchor",
		},
		{
			name:    "read stale anchor",
			op:      backend.OpRead,
			args:    backend.Args{Path: path, Section: "2#aaaa"},
			wantErr: "not found",
		},
		{
			name: "related exact anchor",
			op:   backend.OpRelated,
			args: backend.Args{Query: path + ":" + anchor.Format(3, gamma)},
			want: backend.Args{Query: path + ":3"},
		},
		{
			name:     "related moved anchor",
			op:       backend.OpRelated,
			args:     backend.Args{Query: path + ":" + anchor.Format(1, gamma)},
			want:     backend.Args{Query: path + ":3"},
			wantNote: fmt.Sprintf("# anchor %s: line 1 → 3\n", gamma),
		},
		{
			name: "related numeric passthrough",
			op:   backend.OpRelated,
			args: backend.Args{Query: "a.go:120"},
			want: backend.Args{Query: "a.go:120"},
		},
		{
			name:    "related malformed anchor",
			op:      backend.OpRelated,
			args:    backend.Args{Query: "a.go:120#zz"},
			wantErr: "invalid anchor",
		},
		{
			name: "grep ignores anchor-shaped fields",
			op:   backend.OpGrep,
			args: backend.Args{Query: "a3fk", Section: "120#a3fk"},
			want: backend.Args{Query: "a3fk", Section: "120#a3fk"},
		},
		{
			name: "diff passthrough",
			op:   backend.OpDiff,
			args: backend.Args{Source: "main"},
			want: backend.Args{Source: "main"},
		},
		{
			// OpEdit must NOT rewrite: edit resolves the anchor itself so it keeps the
			// *Move and Ref. RewriteArgs would double-resolve it away.
			name: "edit passthrough keeps anchor",
			op:   backend.OpEdit,
			args: backend.Args{Path: path, Section: anchor.Format(1, gamma), Content: "x"},
			want: backend.Args{Path: path, Section: anchor.Format(1, gamma), Content: "x"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, note, err := anchor.RewriteArgs(tt.op, tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("RewriteArgs() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RewriteArgs() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RewriteArgs() args = %+v, want %+v", got, tt.want)
			}
			if note != tt.wantNote {
				t.Errorf("RewriteArgs() note = %q, want %q", note, tt.wantNote)
			}
		})
	}
}

// TestRewriteArgsIdempotent proves a rewritten section never re-parses as an
// anchor: the second pass returns the args unchanged with no note.
func TestRewriteArgsIdempotent(t *testing.T) {
	path := writeFixture(t)
	args := backend.Args{Path: path, Section: anchor.Format(1, anchor.Of("gamma"))}

	once, note, err := anchor.RewriteArgs(backend.OpRead, args)
	if err != nil {
		t.Fatalf("first RewriteArgs() error = %v", err)
	}
	if note == "" {
		t.Fatal("first RewriteArgs() note is empty, want a move note")
	}

	twice, note, err := anchor.RewriteArgs(backend.OpRead, once)
	if err != nil {
		t.Fatalf("second RewriteArgs() error = %v", err)
	}
	if note != "" {
		t.Errorf("second RewriteArgs() note = %q, want empty", note)
	}
	if !reflect.DeepEqual(twice, once) {
		t.Errorf("second RewriteArgs() args = %+v, want unchanged %+v", twice, once)
	}
}
