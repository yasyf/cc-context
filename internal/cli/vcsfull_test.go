package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/execstub"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestVCSFullInlinesCommitHunks(t *testing.T) {
	t.Parallel()
	f := vcstest.Repo(t)
	bin := t.TempDir()
	execstub.Write(t, filepath.Join(bin, "ast-grep"), `#!/bin/sh
if [ "$1" = "--version" ]; then echo "ast-grep 0.44.0"; exit 0; fi
cat <<'EOF'
{"path":"sample.go","language":"Go","items":[{"symbolType":"function","name":"Value","signature":"func Value() int {","isExported":true,"range":{"start":{"line":2},"end":{"line":4}},"members":[]}]}
EOF
`)
	f.PrependPATH(bin)
	path := filepath.Join(f.Dir, "sample.go")
	if err := os.WriteFile(path, []byte("package sample\n\nfunc Value() int {\n\treturn 1\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.Out(t, "git", "add", "sample.go")
	f.Out(t, "git", "commit", "-m", "add value")
	if err := os.WriteFile(path, []byte("package sample\n\nfunc Value() int {\n\treturn 2\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.Out(t, "git", "add", "sample.go")
	f.Out(t, "git", "commit", "-m", "change value")

	for _, tc := range []struct {
		name string
		new  func() *cobra.Command
		ref  string
	}{
		{"show", newShowCmd, "HEAD"},
		{"diff", newDiffCmd, "HEAD^..HEAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, full := range []bool{false, true} {
				var out bytes.Buffer
				cmd := tc.new()
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				args := []string{tc.ref}
				if full {
					args = append(args, "--full")
				}
				cmd.SetArgs(args)
				if err := cmd.ExecuteContext(f.Context()); err != nil {
					t.Fatalf("full=%v: %v\n%s", full, err, out.String())
				}
				got := out.String()
				if !strings.Contains(got, "[~] Value") {
					t.Fatalf("full=%v: missing modified symbol\n%s", full, got)
				}
				if !full {
					if !strings.Contains(got, "hunks hidden") || strings.Contains(got, "@@") {
						t.Fatalf("default output should summarize symbols and offer full hunks\n%s", got)
					}
					continue
				}
				if strings.Contains(got, "hunks hidden") || !strings.Contains(got, "@@") || !strings.Contains(got, "-\treturn 1") || !strings.Contains(got, "+\treturn 2") {
					t.Fatalf("full output should inline changed lines without the hidden-hunks hint\n%s", got)
				}
			}
		})
	}
}
