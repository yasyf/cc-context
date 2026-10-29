package cli

import (
	"slices"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestGTNearestTrackedReadsOnlyUnresolvedHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(*testing.T, *vcstest.Fixture)
		parent  string
		history []string
	}{
		{
			name: "current parent",
			setup: func(t *testing.T, f *vcstest.Fixture) {
				shipGTStack(t, f, "a", "b")
				shipGTUntracked(t, f, "c")
			},
			parent: "b",
		},
		{
			name: "at trunk with unrelated tracked branches",
			setup: func(t *testing.T, f *vcstest.Fixture) {
				shipGTStack(t, f, "a", "b")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "c", "main")
			},
			parent: "main",
		},
		{
			name: "rewritten parent retains historical ancestry",
			setup: func(t *testing.T, f *vcstest.Fixture) {
				shipParentRewritten(t, f, "")
			},
			parent:  "p",
			history: []string{"refs/heads/p"},
		},
		{
			name: "mixed current and unresolved branches",
			setup: func(t *testing.T, f *vcstest.Fixture) {
				shipGTStack(t, f, "a", "b")
				shipGTUntracked(t, f, "c")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
				shipGTStack(t, f, "x")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "c")
			},
			parent:  "b",
			history: []string{"refs/heads/x"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shipGTRepo(t)
			tc.setup(t, f)
			state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
			if err != nil {
				t.Fatal(err)
			}
			shipResetLog(t, f)
			got, err := gtNearestTracked(f.Context(), render.Dir(f.Dir), state, "main", "c")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.parent {
				t.Fatalf("parent = %q, want %q", got, tc.parent)
			}
			var history []string
			walks := 0
			for _, inv := range shipGTInvocations(t, f) {
				if len(inv) < 3 || inv[0] != "git" || inv[1] != "log" || inv[2] != "--walk-reflogs" {
					continue
				}
				walks++
				history = append(history, inv[5:len(inv)-1]...)
			}
			if !slices.Equal(history, tc.history) {
				t.Errorf("walked reflogs = %v, want %v", history, tc.history)
			}
			if want := min(1, len(tc.history)); walks != want {
				t.Errorf("reflog processes = %d, want %d", walks, want)
			}
		})
	}
}
