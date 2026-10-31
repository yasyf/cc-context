package cli

import (
	"strings"
	"testing"
)

func TestStackSubmitFromADetachedHeadSubmitsTheNamedStackOtherWorktreesHold(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "--detach")
	holders := map[string]string{"base": restackSiblingPath(t, "lane-base"), "feature": restackSiblingPath(t, "lane-feature")}
	for branch, dir := range holders {
		mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", dir, branch)
	}
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	trunk := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main")

	out, errOut, err := runStackCmd(t, f, "submit", "--branch", "feature", "--all-lanes", "--restack")
	if err != nil {
		t.Fatalf("stack submit --branch feature from a detached HEAD: %v\n%s%s", err, out, errOut)
	}
	base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	if parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", base+"^"); parent != trunk {
		t.Errorf("origin base sits on %.12s, want the fetched trunk %.12s", parent, trunk)
	}
	feature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", feature+"^"); parent != base {
		t.Errorf("origin feature sits on %.12s, want origin base %.12s", parent, base)
	}
	for branch, dir := range holders {
		if got := gitAt(t, f.Env(), dir, "branch", "--show-current"); got != branch {
			t.Errorf("%s holds %q, want it still on %s", dir, got, branch)
		}
		if got, want := gitAt(t, f.Env(), dir, "rev-parse", "HEAD"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); got != want {
			t.Errorf("%s HEAD = %.12s, want it moved onto the published %s %.12s", dir, got, branch, want)
		}
	}
	if got := gitAt(t, f.Env(), f.Dir, "branch", "--show-current"); got != "" {
		t.Errorf("the invoking working copy is on %q, want it left detached", got)
	}
}

func TestStackSubmitBranchRefusals(t *testing.T) {
	for _, tt := range []struct {
		name   string
		detach bool
		branch string
		want   string
	}{
		{name: "on another branch", branch: "base", want: "stack submit: HEAD is on feature, so --branch base would work another branch's stack — run it from a detached HEAD or from base"},
		{name: "no such branch", detach: true, branch: "missing", want: "stack submit: --branch missing names no local branch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			f.Decorate(api.ctx)
			shipGTStack(t, f, "base", "feature")
			if tt.detach {
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "--detach")
			}
			_, _, err := runStackCmd(t, f, "submit", "--branch", tt.branch)
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
