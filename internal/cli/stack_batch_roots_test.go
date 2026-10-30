package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackFromStateUsesTheRequestedBranchTrunk(t *testing.T) {
	state := gtState{
		"dev": {Trunk: true},
		"other": {Trunk: true},
		"legacy": {Parents: []gtRef{{Ref: "dev"}}},
		"child": {Parents: []gtRef{{Ref: "legacy"}}},
		"native": {Parents: []gtRef{{Ref: "other"}}},
	}
	for range 128 {
		for branch, want := range map[string][]string{
			"legacy": {"legacy", "child"},
			"child": {"legacy", "child"},
			"native": {"native"},
		} {
			got, err := gtStackFromState(context.Background(), render.Dir("."), "stack list", state, branch, nil)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("branch %s: stack=%v error=%v; want %v", branch, got, err, want)
			}
		}
	}
}

func TestStackListBatchKeepsNativeAndLegacyTrunksSeparate(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	common := filepath.Join(f.Dir, ".git")
	head := strings.TrimSpace(f.Out(t, "git", "rev-parse", "main"))
	f.Out(t, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	var metadata, refs strings.Builder
	metadata.WriteString(`{"main":{"trunk":true}`)
	for i := range 12 {
		name := fmt.Sprintf("legacy-%d", i)
		fmt.Fprintf(&metadata, `,%q:{"parents":[{"ref":"main","sha":%q}]}`, name, head)
		fmt.Fprintf(&refs, "update refs/heads/%s %s\n", name, head)
	}
	metadata.WriteString("}")
	vcstest.WriteGraphiteMeta(t, common, metadata.String())
	fmt.Fprintf(&refs, "update refs/heads/other %s\nupdate refs/heads/native-other %s\n", head, head)
	if _, err := render.RunCLIStdin(f.Context(), render.Dir(f.Dir), "git", []string{"update-ref", "--stdin"}, []byte(refs.String())); err != nil {
		t.Fatal(err)
	}
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	if err := nativemeta.AdoptRoot(f.Context(), common, "native-other", "other", head, head); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 13)
	for i := range paths {
		branch := fmt.Sprintf("legacy-%d", i)
		if i == 12 {
			branch = "native-other"
		}
		paths[i] = filepath.Join(t.TempDir(), branch)
		f.Out(t, "git", "worktree", "add", "-q", paths[i], branch)
	}
	results := stackBatchReports(t, f, paths...)
	for i, result := range results {
		branch := fmt.Sprintf("legacy-%d", i)
		if i == 12 {
			branch = "native-other"
		}
		if result.Error != "" || result.Listing == nil || len(result.Listing.Branches) != 1 || result.Listing.Branches[0].Branch != branch || !result.Listing.Branches[0].Current {
			t.Fatalf("requested %s: result=%+v", branch, result)
		}
	}
}
