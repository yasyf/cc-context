package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestExpansionHint(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"stat on literal tilde", errors.New("outline: stat ~/.claude/cache/changelog.md: no such file or directory"), true},
		{"tilde glued to a joined path", errors.New("exit status 2: not found: /Users/u/Code/cc-skills/~/.claude/cache/changelog.md"), true},
		{"unexpanded var", errors.New("exit status 2: not found: /Users/u/Code/claude-pool/$d/fuse/host.go"), true},
		{"plain not found", errors.New("exit status 2: not found: rust/src/mining.rs"), false},
		{"regex end anchor", errors.New(`regex parse error: (foo|bar)$`), false},
		{"dollar digit", errors.New("bad capture $1 in replacement"), false},
		{"mid-word tilde", errors.New("not found: foo~bar.go"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExpansionHint(tt.err) != ""; got != tt.want {
				t.Errorf("ExpansionHint(%v) fired = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestOutlineUnexpandedTildeErrorCarriesHintSignal(t *testing.T) {
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"code", "outline", "~/definitely-missing.md"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an error for a literal ~ path")
	}
	if ExpansionHint(err) == "" {
		t.Errorf("ExpansionHint should fire on: %v", err)
	}
}
