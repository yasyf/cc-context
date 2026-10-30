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

func TestStackListBatchRefReadsFollowRequestedTopology(t *testing.T) {
	for _, unrelated := range []int{32, 4096} {
		t.Run(fmt.Sprintf("unrelated%d", unrelated), func(t *testing.T) {
			f := vcstest.Repo(t, vcstest.Remote())
			common := filepath.Join(f.Dir, ".git")
			head := strings.TrimSpace(f.Out(t, "git", "rev-parse", "main"))
			var metadata, refs strings.Builder
			metadata.WriteString(`{"main":{"trunk":true}`)
			for i := range 12 + unrelated {
				name := fmt.Sprintf("requested-%d", i)
				if i >= 12 {
					name = fmt.Sprintf("unrelated-%d", i)
				}
				fmt.Fprintf(&metadata, `,%q:{"parents":[{"ref":"main","sha":%q}]}`, name, head)
				fmt.Fprintf(&refs, "update refs/heads/%s %s\n", name, head)
			}
			metadata.WriteString("}")
			vcstest.WriteGraphiteMeta(t, common, metadata.String())
			if _, err := render.RunCLIStdin(f.Context(), render.Dir(f.Dir), "git", []string{"update-ref", "--stdin"}, []byte(refs.String())); err != nil {
				t.Fatal(err)
			}
			paths := make([]string, 12)
			for i := range paths {
				paths[i] = filepath.Join(t.TempDir(), fmt.Sprintf("worktree-%d", i))
				f.Out(t, "git", "worktree", "add", "-q", paths[i], fmt.Sprintf("requested-%d", i))
			}
			original := filepath.Join(f.ShimBin, "git-original")
			if err := os.Rename(filepath.Join(f.ShimBin, "git"), original); err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(t.TempDir(), "requested-refs")
			audit := filepath.Join(t.TempDir(), "all-requested-refs")
			script := "#!/bin/sh\ncase \" $* \" in\n*for-each-ref*--stdin*)\n/bin/cat > " + shQuote(input) + "\nprintf '%s\\n' BEGIN >> " + shQuote(audit) + "\n/bin/cat " + shQuote(input) + " >> " + shQuote(audit) + "\nexec " + shQuote(original) + " \"$@\" < " + shQuote(input) + "\n;;\nesac\nexec " + shQuote(original) + " \"$@\"\n"
			execstub.Write(t, filepath.Join(f.ShimBin, "git"), script)
			results := stackBatchReports(t, f, paths...)
			for i, result := range results {
				if result.Error != "" || result.Listing == nil || len(result.Listing.Branches) != 1 || result.Listing.Branches[0].Branch != fmt.Sprintf("requested-%d", i) {
					t.Fatalf("result %d = %+v", i, result)
				}
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
			t.Logf("unrelated=%d requested_worktrees=12 ref_calls=%d resolved_refs=%d", unrelated, calls, resolved)
			if calls != 1 || resolved != 14 || strings.Contains(string(payload), "unrelated-") {
				t.Fatalf("resolved %d refs across %d calls for 12 worktrees: %s", resolved, calls, payload)
			}
		})
	}
}
