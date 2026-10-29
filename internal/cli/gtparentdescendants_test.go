package cli

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

func TestGTRecordedDescendantsMatchesFirstParentWalk(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  gtState
		target string
	}{
		{name: "empty", state: gtState{}, target: "missing"},
		{name: "missing target", state: gtState{"a": {Parents: []gtRef{{Ref: "missing"}}}, "b": {Parents: []gtRef{{Ref: "a"}}}}, target: "missing"},
		{name: "disconnected cycle", state: gtState{"a": {Parents: []gtRef{{Ref: "b"}}}, "b": {Parents: []gtRef{{Ref: "a"}}}, "target": {}}, target: "target"},
		{name: "cycle through target", state: gtState{"a": {Parents: []gtRef{{Ref: "b"}}}, "b": {Parents: []gtRef{{Ref: "a"}}}, "c": {Parents: []gtRef{{Ref: "b"}}}}, target: "a"},
		{name: "first parent only", state: gtState{"a": {Parents: []gtRef{{Ref: "other"}, {Ref: "target"}}}, "b": {Parents: []gtRef{{Ref: "target"}, {Ref: "other"}}}, "c": {Parents: []gtRef{{Ref: "b"}}}}, target: "target"},
		{name: "empty name", state: gtState{"": {Parents: []gtRef{{Ref: "a"}}}, "a": {Parents: []gtRef{{Ref: ""}}}}, target: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := gtRecordedDescendants(tc.state, tc.target)
			for name := range tc.state {
				if want := gtRecordedAboveOracle(tc.state, name, tc.target); got[name] != want {
					t.Errorf("descendant %q = %t, want %t", name, got[name], want)
				}
			}
		})
	}
}

func TestGTRecordedDescendantsRandomGraphs(t *testing.T) {
	rng := rand.New(rand.NewPCG(17, 29)) //nolint:gosec // deterministic test vectors, not security
	const n = 64
	for graph := 0; graph < 32; graph++ {
		state := make(gtState, n)
		for i := 0; i < n; i++ {
			var parents []gtRef
			for j := rng.IntN(3); j > 0; j-- {
				parents = append(parents, gtRef{Ref: fmt.Sprint(rng.IntN(n + 1))})
			}
			state[fmt.Sprint(i)] = gtBranchState{Parents: parents}
		}
		for target := 0; target <= n; target++ {
			branch := fmt.Sprint(target)
			got := gtRecordedDescendants(state, branch)
			for name := range state {
				if want := gtRecordedAboveOracle(state, name, branch); got[name] != want {
					t.Fatalf("graph %d target %s descendant %s = %t, want %t", graph, branch, name, got[name], want)
				}
			}
		}
	}
}

func TestGTRecordedDescendantsDeepChainAllocations(t *testing.T) {
	const n = 4096
	state := make(gtState, n)
	for i := 1; i <= n; i++ {
		state[fmt.Sprint(i)] = gtBranchState{Parents: []gtRef{{Ref: fmt.Sprint(i - 1)}}}
	}
	var got map[string]bool
	allocs := testing.AllocsPerRun(1, func() { got = gtRecordedDescendants(state, "0") })
	if len(got) != n {
		t.Fatalf("descendants = %d, want %d", len(got), n)
	}
	for name := range state {
		if !got[name] {
			t.Fatalf("missing descendant %s", name)
		}
	}
	if allocs > 2*n {
		t.Errorf("allocations = %.0f, want <= %d", allocs, 2*n)
	}
}

func gtRecordedAboveOracle(state gtState, name, branch string) bool {
	seen := map[string]bool{}
	for !seen[name] {
		seen[name] = true
		s, tracked := state[name]
		if !tracked || len(s.Parents) == 0 {
			return false
		}
		name = s.Parents[0].Ref
		if name == branch {
			return true
		}
	}
	return false
}
