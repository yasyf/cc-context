package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/cli"
)

// TestFlagErrorHints drives the full CLI seam for the FlagErrorFunc
// registered on the root command: a near-miss typo on the erroring
// command's own flags, a curated cross-command confusion, and a junk flag
// with neither, which must fall back to cobra's plain error unchanged.
func TestFlagErrorHints(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    string
		notWant string
	}{
		{
			name: "near miss on same command",
			args: []string{"code", "grep", "foo", "--budge"},
			want: "unknown flag: --budge (did you mean --budget?)",
		},
		{
			name: "cross command hint: section on grep",
			args: []string{"code", "grep", "foo", "--section", "1-5"},
			want: "unknown flag: --section (grep takes -A/-B/-C for context; --section belongs to read/outline)",
		},
		{
			name: "cross command hint: full on grep",
			args: []string{"code", "grep", "foo", "--full"},
			want: "unknown flag: --full (read/symbol take --full)",
		},
		{
			name: "cross command hint: glob on read",
			args: []string{"code", "read", "foo.go", "--glob", "*.go"},
			want: "unknown flag: --glob (grep takes --glob)",
		},
		{
			name: "cross command hint: glob on outline",
			args: []string{"code", "outline", "foo.go", "--glob", "*.go"},
			want: "unknown flag: --glob (grep takes --glob)",
		},
		{
			name: "cross command hint: budget missing lists commands that have it",
			args: []string{"code", "related", "foo.go:1", "--budget", "100"},
			want: "unknown flag: --budget (commands with --budget: ",
		},
		{
			name: "shorthand belongs elsewhere",
			args: []string{"code", "read", "foo.go", "-A", "3"},
			want: "unknown shorthand flag: 'A' in -A (-A belongs to code grep (--after-context))",
		},
		{
			name:    "unknown junk flag falls back to plain cobra error",
			args:    []string{"code", "grep", "foo", "--zzzqqq"},
			want:    "unknown flag: --zzzqqq",
			notWant: "(",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			root := cli.NewRootCmd()
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tt.args)
			err := root.Execute()
			if err == nil {
				t.Fatalf("Execute(%v) error = nil, want unknown flag error", tt.args)
			}
			if got := err.Error(); !strings.Contains(got, tt.want) {
				t.Errorf("Execute(%v) error = %q, want substring %q", tt.args, got, tt.want)
			}
			if tt.notWant != "" && strings.Contains(err.Error(), tt.notWant) {
				t.Errorf("Execute(%v) error = %q, want no substring %q", tt.args, err.Error(), tt.notWant)
			}
		})
	}
}

// TestLevenshteinNearMissThreshold exercises the shared edit-distance helper
// via its only public effect — near-miss flag suggestions — since the
// function itself is unexported.
func TestLevenshteinNearMissThreshold(t *testing.T) {
	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	// "budge" is distance 1 from "budget": suggested.
	root.SetArgs([]string{"code", "grep", "foo", "--budge"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "did you mean --budget?") {
		t.Fatalf("Execute(--budge) error = %v, want a --budget suggestion", err)
	}
}
