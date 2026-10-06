package cli

import (
	"strings"
	"testing"
)

func TestStackSubmitRefusesToStackOnAPinnedBranchItsParentLeft(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "p", "x", "c")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	published := map[string]string{}
	for _, name := range []string{"p", "x", "c"} {
		published[name] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", restackSiblingPath(t, "lane"), "x")
	writeShipFile(t, f.Dir, "p.txt", "amended\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "p.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "--no-edit")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatalf("stack submit succeeded with output %q, want a refusal: x is kept on p's old head", out)
	}
	for _, want := range []string{"x", "--include x"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
	for name, head := range published {
		if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name); got != head {
			t.Errorf("origin %s = %s, want it untouched at %s", name, got, head)
		}
	}
	if n := pushCount(t, f); n != 0 {
		t.Errorf("pushes = %d, want none before the refusal", n)
	}

	if _, _, err := runStackCmd(t, f, "submit", "--include", "x"); err != nil {
		t.Fatalf("stack submit --include x: %v", err)
	}
	for _, pair := range [][2]string{{"p", "x"}, {"x", "c"}} {
		parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", pair[0])
		child := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", pair[1])
		if !stackOnto(t, f, parent, child) {
			t.Errorf("published %s %s is not stacked on published %s %s", pair[1], child, pair[0], parent)
		}
	}
}
