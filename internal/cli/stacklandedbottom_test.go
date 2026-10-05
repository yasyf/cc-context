package cli

import (
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackBottomLanded(t *testing.T) (*vcstest.Fixture, *gtAPIStub) {
	t.Helper()
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "mid", "side", "top")
	if _, errStr, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit = %v (stderr=%q)", err, errStr)
	}
	restackSquashRemote(t, f, "main", "base (#41)", "base")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	stubStackPRs(t, f, map[string]*stackPR{
		"base": {Number: 41, Title: "base", State: "MERGED", Landed: true, Head: gitAt(t, f.Env(), f.Dir, "rev-parse", "base")},
		"mid":  {Number: 42, Title: "mid", State: "OPEN", Base: "main"},
		"side": {Number: 43, Title: "side", State: "OPEN", Base: "mid"},
		"top":  {Number: 44, Title: "top", State: "OPEN", Base: "side"},
	})
	shipResetLog(t, f)
	return f, api
}

func stackAssertOwnsOnTrunk(t *testing.T, f *vcstest.Fixture, branch string) {
	t.Helper()
	if n := gitAt(t, f.Env(), f.RemoteDir, "rev-list", "--count", "main.."+branch); n != "1" {
		t.Errorf("origin %s holds %s commits over main, want its own 1", branch, n)
	}
	if !stackOnto(t, f, "origin/main", gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)) {
		t.Errorf("origin %s is not on the trunk its landed parent left it on", branch)
	}
}

func TestStackRebaseParentMovesTheChildOfALandedBottom(t *testing.T) {
	f, _ := stackBottomLanded(t)

	out, errStr, err := runStackCmd(t, f, "rebase", "--parent", "top=mid")
	if err != nil {
		t.Fatalf("stack rebase --parent top=mid = %v (stdout=%q stderr=%q)", err, out, errStr)
	}
	stackAssertOwnsOnTrunk(t, f, "mid")
}

func TestStackSubmitMovesABranchGtLeftOnItsLandedParentsHead(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "mid")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "mid")
	landed := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	restackSquashRemote(t, f, "main", "base (#41)", "base")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "mid", "--parent", "main", "--force", "--no-interactive")
	commonDir, err := gtCommonDir(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := gtmeta.RecordRestacked(t.Context(), commonDir, map[string]string{"mid": landed}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.Env(), f.Dir, "gt", "untrack", "base", "--force", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-D", "base")
	stubStackPRs(t, f, map[string]*stackPR{
		"base": {Number: 41, Title: "base", State: "MERGED", Landed: true, Head: landed},
		"mid":  {Number: 42, Title: "mid", State: "OPEN", Base: "main"},
	})
	shipResetLog(t, f)

	out, errStr, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit = %v (stdout=%q stderr=%q)", err, out, errStr)
	}
	stackAssertOwnsOnTrunk(t, f, "mid")
}
