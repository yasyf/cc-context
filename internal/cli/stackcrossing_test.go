package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func graphiteChildOnNativeParentRepo(t *testing.T) *vcstest.Fixture {
	t.Helper()
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	common := filepath.Join(f.Dir, ".git")
	trunk := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "base")
	writeShipFile(t, f.Dir, "base.txt", "base\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "base.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "base")
	base := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	writeShipFile(t, f.Dir, "feature.txt", "feature\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "feature.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "feature")
	feature := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	vcstest.WriteGraphiteMeta(t, common, fmt.Sprintf(`{"main":{"trunk":true},"base":{"parents":[{"ref":"main","sha":%q}]},"feature":{"parents":[{"ref":"base","sha":%q}]}}`, trunk, base))
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	if err := nativemeta.AdoptRoot(f.Context(), common, "base", "main", trunk, base); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(stackPublication{Branch: "feature", Source: feature, SourceBase: base, Head: feature, Base: base, Parent: "base"})
	if err != nil {
		t.Fatal(err)
	}
	oid, err := render.RunCLIStdin(f.Context(), render.Dir(f.Dir), "git", []string{"hash-object", "-w", "--stdin"}, payload)
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "update-ref", stackPublicationRef("feature", "receipt"), strings.TrimSpace(oid))
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	writeShipFile(t, f.Dir, "base.txt", "base, shipped again\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qam", "base again")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base")
	api := newGTAPIStub(t)
	api.openPRs("base", "feature")
	f.Decorate(api.ctx)
	stubOpenPRs(t, f, nil, "base", "feature")
	stubStackStanding(t, f, nil)
	return f
}

func TestStackSubmitFromANativeParentConvertsItsGraphiteChild(t *testing.T) {
	f := graphiteChildOnNativeParentRepo(t)
	common := filepath.Join(f.Dir, ".git")
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	l, err := resolveLane(f.Context(), "stack rebase", f.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	run, err := stackPlan(f.Context(), l, common, stackRebaseOpts{members: []string{"base", "feature"}, submit: true, dryRun: true})
	if err != nil {
		t.Fatalf("stackPlan(base, Graphite feature on native base) = %v; want feature converted and replanned", err)
	}
	if b := run.branch("feature"); b == nil || b.Parent != "base" {
		t.Fatalf("feature in the plan = %+v; want it restacked on base", b)
	}
	if native, err := gtmeta.NativeMembership(common, "feature"); err != nil || !native {
		t.Fatalf("native membership of feature = %v, %v; want converted", native, err)
	}
}

func TestStackSubmitRefusesAMemberTheNativeCatalogDoesNotTrack(t *testing.T) {
	f := graphiteChildOnNativeParentRepo(t)
	l, err := resolveLane(f.Context(), "stack rebase", f.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = stackPlan(f.Context(), l, filepath.Join(f.Dir, ".git"), stackRebaseOpts{members: []string{"base", "feature"}, submit: true, dryRun: true})
	if err == nil || !strings.Contains(err.Error(), "does not track feature, a branch of this run") {
		t.Fatalf("stackPlan(Graphite feature without ccx.stack=github) = %v; want a refusal naming feature", err)
	}
}
