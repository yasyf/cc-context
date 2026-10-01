package cli

import (
	"errors"
	"slices"
	"testing"
)

func TestGTCommitBeforeMoveScopesOverlapStatus(t *testing.T) {
	for _, tt := range []struct {
		name     string
		replayed string
		pending  string
		selected string
		want     bool
	}{
		{name: "selected replay path", replayed: "new.txt", pending: "new.txt", selected: "new.txt", want: true},
		{name: "foreign replay path", replayed: "new.txt", pending: "new.txt", selected: "f.txt", want: false},
		{name: "unrelated foreign path", replayed: "new.txt", pending: "scratch.txt", selected: "f.txt", want: true},
		{name: "literal replay path", replayed: "[new].txt", pending: "[new].txt", selected: "f.txt", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t)
			was := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
			writeShipFile(t, f.Dir, tt.replayed, "replayed\n")
			mustRun(t, f.Env(), f.Dir, "git", "add", "--", tt.replayed)
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "replayed")
			head := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
			mustRun(t, f.Env(), f.Dir, "git", "reset", "--hard", was)
			writeShipFile(t, f.Dir, "f.txt", "selected pending\n")
			writeShipFile(t, f.Dir, tt.pending, "pending\n")
			l, err := resolveLane(f.Context(), "ship", f.Dir, true)
			if err != nil {
				t.Fatal(err)
			}
			dirty := &errReplayDirty{shipRefusal: &shipRefusal{msg: "replay is dirty"}, was: was, head: head}
			shipResetLog(t, f)
			got, err := gtCommitBeforeMove(f.Context(), l, shipOpts{paths: []string{tt.selected}, rootPaths: []string{tt.selected}}, branchPlan{action: branchAppend}, dirty)
			if got != tt.want || (tt.want && err != nil) || (!tt.want && !errors.Is(err, dirty)) {
				t.Fatalf("commit before move = %t, %v; want %t", got, err, tt.want)
			}
			found := false
			for _, inv := range shipGTInvocations(t, f) {
				if slices.Contains(inv, "--untracked-files=all") {
					found = true
					at := slices.Index(inv, "--")
					if at < 0 || !slices.Equal(inv[at+1:], []string{tt.replayed}) {
						t.Errorf("overlap status = %v, want only %s", inv, tt.replayed)
					}
				}
			}
			if !found {
				t.Fatal("missing replay overlap status")
			}
		})
	}
}
