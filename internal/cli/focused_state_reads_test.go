package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/execstub"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestFocusedStateRefReadsFollowRequestedTopology(t *testing.T) {
	for _, unrelated := range []int{32, 4096} {
		t.Run(fmt.Sprintf("unrelated%d", unrelated), func(t *testing.T) {
			f := vcstest.Repo(t, vcstest.Remote())
			common := filepath.Join(f.Dir, ".git")
			head := strings.TrimSpace(f.Out(t, "git", "rev-parse", "main"))
			var metadata, refs strings.Builder
			fmt.Fprintf(&metadata, `{"main":{"trunk":true},"requested":{"parents":[{"ref":"main","sha":%q}]},"requested-child":{"parents":[{"ref":"requested","sha":%q}]}`, head, head)
			fmt.Fprintf(&refs, "update refs/heads/requested %s\nupdate refs/heads/requested-child %s\n", head, head)
			for i := range unrelated {
				name := fmt.Sprintf("unrelated-%d", i)
				fmt.Fprintf(&metadata, `,%q:{"parents":[{"ref":"main","sha":%q}]}`, name, head)
				fmt.Fprintf(&refs, "update refs/heads/%s %s\n", name, head)
			}
			metadata.WriteString("}")
			vcstest.WriteGraphiteMeta(t, common, metadata.String())
			if _, err := render.RunCLIStdin(f.Context(), render.Dir(f.Dir), "git", []string{"update-ref", "--stdin"}, []byte(refs.String())); err != nil {
				t.Fatal(err)
			}
			original := filepath.Join(f.ShimBin, "git-original")
			if err := os.Rename(filepath.Join(f.ShimBin, "git"), original); err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(t.TempDir(), "requested-refs")
			audit := filepath.Join(t.TempDir(), "all-requested-refs")
			script := "#!/bin/sh\ncase \" $* \" in\n*for-each-ref*--stdin*)\n/bin/cat > " + shQuote(input) + "\nprintf '%s\\n' BEGIN >> " + shQuote(audit) + "\n/bin/cat " + shQuote(input) + " >> " + shQuote(audit) + "\nexec " + shQuote(original) + " \"$@\" < " + shQuote(input) + "\n;;\nesac\nexec " + shQuote(original) + " \"$@\"\n"
			execstub.Write(t, filepath.Join(f.ShimBin, "git"), script)
			state, err := gtStateAtFocused(withStackProvider(f.Context(), false), common, "test", "requested", "")
			if err != nil {
				t.Fatal(err)
			}
			if !state["main"].Trunk || state["requested"].Head != head || state["requested-child"].Head != head {
				t.Fatalf("requested topology = %+v", state)
			}
			payload, err := os.ReadFile(audit)
			if err != nil {
				t.Fatal(err)
			}
			var calls, resolved int
			for _, entry := range strings.Fields(string(payload)) {
				if entry == "BEGIN" {
					calls++
				} else {
					resolved++
				}
			}
			t.Logf("unrelated=%d focused_branches=1 ref_calls=%d resolved_refs=%d", unrelated, calls, resolved)
			if calls != 1 || resolved != 4 || strings.Contains(string(payload), "unrelated-") {
				t.Fatalf("resolved %d refs across %d calls for one focused branch: %s", resolved, calls, payload)
			}
		})
	}
}
