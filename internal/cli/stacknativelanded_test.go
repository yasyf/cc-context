package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// TestNativeStackReplaysAChildFromItsPublishedBaseAfterItsParentsSquashLand is
// a lane's submit after a and b squash-landed under c: GitHub retargeted c onto
// trunk, so its recorded fork fell to trunk's old head, and c held a commit past
// its publication, so the plan replayed a's and b's commits as c's own.
func TestNativeStackReplaysAChildFromItsPublishedBaseAfterItsParentsSquashLand(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	common := filepath.Join(f.Dir, ".git")
	trunk := shipHead(t, f)
	for _, name := range []string{"a", "b", "c"} {
		shipGTUntracked(t, f, name)
	}
	a := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")
	b := gitAt(t, f.Env(), f.Dir, "rev-parse", "b")
	published := gitAt(t, f.Env(), f.Dir, "rev-parse", "c")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a", "b", "c")
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	for _, link := range []struct{ branch, parent, fork, head string }{{"a", "main", trunk, a}, {"b", "a", a, b}, {"c", "b", b, published}} {
		if err := nativemeta.AdoptRoot(f.Context(), common, link.branch, link.parent, link.fork, link.head); err != nil {
			t.Fatal(err)
		}
	}
	payload, err := json.Marshal(stackPublication{Branch: "c", Source: published, SourceBase: b, Head: published, Base: b, Parent: "b"})
	if err != nil {
		t.Fatal(err)
	}
	oid, err := render.RunCLIStdin(f.Context(), render.Dir(f.Dir), "git", []string{"hash-object", "-w", "--stdin"}, payload)
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "update-ref", stackPublicationRef("c", "receipt"), strings.TrimSpace(oid))
	writeShipFile(t, f.Dir, "c2.txt", "c2\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "c2.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "c2")
	restackSquashRemote(t, f, "main", "a (#41)", "a")
	restackSquashRemote(t, f, "main", "b (#42)", "b")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	api := newGTAPIStub(t)
	api.openPRs("c")
	api.bases["c"], api.stacks["c"] = "main", 46
	f.Decorate(api.ctx)
	stubOpenPRs(t, f, map[string]*stackPR{
		"a": {Number: 41, Title: "a", State: "MERGED", Landed: true, Head: a},
		"b": {Number: 42, Title: "b", State: "MERGED", Landed: true, Head: b},
	}, "c")
	stubStackStanding(t, f, nil)
	l, err := resolveLane(f.Context(), "stack rebase", f.Dir, false)
	if err != nil {
		t.Fatal(err)
	}

	run, err := stackPlan(f.Context(), l, common, stackRebaseOpts{tip: "c", submit: true, dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := run.branch("c").OldBase; got != b {
		t.Errorf("c replays from %s, want b's last head %s, past a's and b's commits", shortOID(got), shortOID(b))
	}
}
