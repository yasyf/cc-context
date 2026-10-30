package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
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
