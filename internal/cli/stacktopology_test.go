package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackTopologyQueriesMissingNamesOnlyOnceAndPreservesEdits(t *testing.T) {
	state := gtState{
		"main":   {Trunk: true, Head: "main"},
		"edited": {Head: "edited", Parents: []gtRef{{Ref: "manual", SHA: "original"}}},
	}
	retargeted := gtState{"edited": {Head: "edited", Parents: []gtRef{{Ref: "override", SHA: "original"}}}}
	topology := &stackTopology{ctx: t.Context(), state: state, retargeted: retargeted, overrides: map[string]string{"new": "manual"}, rewound: map[string]string{}, queried: map[string]bool{}}
	var calls int
	read := func(_ context.Context, _ string, branches []string) (gtmeta.State, []gtmeta.Orphan, error) {
		calls++
		if !reflect.DeepEqual(branches, []string{"new", "absent"}) {
			t.Fatalf("queried names = %v", branches)
		}
		return gtmeta.State{
			"main":   {Trunk: true, Head: "changed-main"},
			"edited": {Head: "changed-edited"},
			"new":    {Head: "new-head", Parents: []gtmeta.Ref{{Ref: "main", SHA: "new-base"}}},
		}, nil, nil
	}
	for range 2 {
		if err := topology.load(read, []string{"edited", "new", "new", "absent"}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || state["main"].Head != "main" || state["edited"].Head != "edited" || state["edited"].Parents[0].Ref != "manual" || retargeted["edited"].Parents[0].Ref != "override" || retargeted["new"].Parents[0].Ref != "manual" {
		t.Fatalf("calls=%d state=%+v retargeted=%+v", calls, state, retargeted)
	}
}

func TestStackTopologyNormalizesOnlyNewEntriesAgainstExistingParents(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	base := strings.TrimSpace(f.Out(t, "git", "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(f.Dir, "child.txt"), []byte("child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.Out(t, "git", "add", "child.txt")
	f.Out(t, "git", "commit", "-qm", "child")
	head := strings.TrimSpace(f.Out(t, "git", "rev-parse", "HEAD"))
	state := gtState{
		"main":   {Trunk: true, Head: base},
		"parent": {Head: base, Parents: []gtRef{{Ref: "main", SHA: base}}},
		"edited": {Head: head, Parents: []gtRef{{Ref: "parent", SHA: head}}},
	}
	topology := &stackTopology{ctx: f.Context(), dir: render.Dir(f.Dir), state: state, rewound: map[string]string{}}
	fresh := gtState{
		"edited": {Head: head, Parents: []gtRef{{Ref: "main", SHA: "changed"}}},
		"new":    {Head: head, Parents: []gtRef{{Ref: "parent", SHA: head}}},
	}
	if err := topology.merge(fresh); err != nil {
		t.Fatal(err)
	}
	if state["new"].Parents[0].SHA != base || topology.rewound["new"] != "parent" || state["edited"].Parents[0].SHA != head || len(topology.rewound) != 1 {
		t.Fatalf("hydrated state=%+v rewound=%v", state, topology.rewound)
	}
}

func TestCompleteNativePlanConvertsOnlyRequestedGraphiteAncestry(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	common := filepath.Join(f.Dir, ".git")
	head := strings.TrimSpace(f.Out(t, "git", "rev-parse", "main"))
	for _, branch := range []string{"native-anchor", "base", "publication", "publication-child", "unrelated"} {
		f.Out(t, "git", "branch", branch)
	}
	vcstest.WriteGraphiteMeta(t, common, fmt.Sprintf(`{"main":{"trunk":true},"base":{"parents":[{"ref":"main","sha":%q}]},"publication":{"parents":[{"ref":"base","sha":%q}]},"publication-child":{"parents":[{"ref":"publication","sha":%q}]},"unrelated":{"parents":[{"ref":"main","sha":%q}]}}`, head, head, head, head))
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	if err := nativemeta.AdoptRoot(f.Context(), common, "native-anchor", "main", head, head); err != nil {
		t.Fatal(err)
	}
	f.Out(t, "git", "config", "ccx.stack", "github")
	ctx := gtmeta.WithProvider(f.Context(), gtmeta.ProviderGitHub)
	state, err := gtStateAt(ctx, common, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := state["publication"]; present {
		t.Fatal("publication was converted before it was requested")
	}
	before, err := os.ReadFile(filepath.Join(common, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	topology := &stackTopology{ctx: ctx, dir: render.Dir(f.Dir), commonDir: common, state: state, retargeted: maps.Clone(state), rewound: map[string]string{}, queried: map[string]bool{}, expanded: map[string]bool{}, complete: true, native: true}
	if err := topology.include("publication"); err != nil {
		t.Fatal(err)
	}
	entry := state["publication"]
	if entry.Head != head || len(entry.Parents) != 1 || entry.Parents[0].Ref != "base" || topology.retargeted["publication"].Head != head {
		t.Fatalf("requested native ancestry = %+v", state)
	}
	names, err := nativemeta.BranchNames(common)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"base", "native-anchor", "publication"}) {
		t.Fatalf("converted native branches = %v", names)
	}
	after, err := os.ReadFile(filepath.Join(common, ".graphite_metadata.db"))
	if err != nil || !slices.Equal(before, after) {
		t.Fatalf("conversion changed Graphite source metadata: %v", err)
	}
}
