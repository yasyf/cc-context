package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestExpansionHint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		args []string
		want bool
	}{
		{"nil", nil, []string{"code", "outline", "~/x.md"}, false},
		{"stat on literal tilde", errors.New("outline: stat ~/.claude/cache/changelog.md: no such file or directory"), []string{"code", "outline", "~/.claude/cache/changelog.md"}, true},
		{"tilde glued to a joined path", errors.New("exit status 2: not found: /Users/u/Code/cc-skills/~/.claude/cache/changelog.md"), []string{"code", "read", "~/.claude/cache/changelog.md"}, true},
		{"unexpanded var", errors.New("exit status 2: not found: /Users/u/Code/claude-pool/$d/fuse/host.go"), []string{"code", "read", "$d/fuse/host.go"}, true},
		{"unexpanded flag value", errors.New("read ~/body.md: no such file or directory"), []string{"vcs", "ship", "--pr-body-file=~/body.md"}, true},
		{"plain not found", errors.New("exit status 2: not found: rust/src/mining.rs"), []string{"code", "read", "rust/src/mining.rs"}, false},
		{"regex end anchor", errors.New(`regex parse error: (foo|bar)$`), []string{"code", "grep", "(foo|bar)$"}, false},
		{"dollar digit", errors.New("bad capture $1 in replacement"), []string{"code", "replace", "$1"}, false},
		{"mid-word tilde", errors.New("not found: foo~bar.go"), []string{"code", "read", "foo~bar.go"}, false},
		{"tilde path in output only", errors.New("stack rebase: resolve it in ~/.claude/worktrees/repo/conflict-feature and run ccx vcs stack continue"), []string{"vcs", "stack", "submit"}, false},
		{"variable in output only", errors.New("git push: hook said: export $GITHUB_TOKEN first"), []string{"vcs", "ship", "-m", "fix"}, false},
		{"unexpanded arg the error never names", errors.New("stack rebase: a conflict stopped the run"), []string{"vcs", "stack", "rebase", "--parent=$b=main"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExpansionHint(tt.err, tt.args) != ""; got != tt.want {
				t.Errorf("ExpansionHint(%v, %q) fired = %v, want %v", tt.err, tt.args, got, tt.want)
			}
		})
	}
}

func TestOutlineUnexpandedTildeErrorCarriesHintSignal(t *testing.T) {
	t.Parallel()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"code", "outline", "~/definitely-missing.md"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an error for a literal ~ path")
	}
	if ExpansionHint(err, []string{"code", "outline", "~/definitely-missing.md"}) == "" {
		t.Errorf("ExpansionHint should fire on: %v", err)
	}
}
