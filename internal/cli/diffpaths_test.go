package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestVCSDiffTakesSeveralPathsUnderOneBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		commit bool
		args   []string
	}{
		{name: "uncommitted", args: []string{"a.txt", "b.txt"}},
		{name: "a source, then paths", commit: true, args: []string{"HEAD^..HEAD", "a.txt", "b.txt"}},
		{name: "paths after --", args: []string{"--", "a.txt", "b.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := vcstest.Repo(t)
			write := func(contents string) {
				for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
					if err := os.WriteFile(filepath.Join(f.Dir, name), []byte(name+contents), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			write("\n")
			f.Out(t, "git", "add", "-A")
			f.Out(t, "git", "commit", "-m", "three files")
			write(" changed\n")
			if tc.commit {
				f.Out(t, "git", "commit", "-am", "change all three")
			}

			var out bytes.Buffer
			cmd := newDiffCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(append([]string{"--stat"}, tc.args...))
			if err := cmd.ExecuteContext(f.Context()); err != nil {
				t.Fatalf("diff %v: %v\n%s", tc.args, err, out.String())
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")[1:]
			rows := make([]string, 0, len(lines))
			for _, line := range lines {
				name, _, _ := strings.Cut(line, " | ")
				rows = append(rows, strings.TrimSpace(name))
			}
			if want := []string{"a.txt", "b.txt"}; !slices.Equal(rows, want) {
				t.Errorf("diff %v rows = %v, want only the two paths named\n%s", tc.args, rows, out.String())
			}
		})
	}
}
