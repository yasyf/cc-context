package gtmeta_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func providerFixture(t *testing.T) (*vcstest.Fixture, string, string) {
	t.Helper()
	f := vcstest.Repo(t, vcstest.Remote())
	common := filepath.Join(f.Dir, ".git")
	main := strings.TrimSpace(f.Out(t, "git", "rev-parse", "main"))
	for _, branch := range []string{"legacy", "native", "shared"} {
		f.Out(t, "git", "branch", branch)
	}
	vcstest.WriteGraphiteMeta(t, common, fmt.Sprintf(`{"main":{"trunk":true},"legacy":{"parents":[{"ref":"main","sha":%q}]},"shared":{"parents":[{"ref":"legacy","sha":%q}]}}`, main, main))
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"native", "shared"} {
		if err := nativemeta.AdoptRoot(f.Context(), common, branch, "main", main, main); err != nil {
			t.Fatal(err)
		}
	}
	return f, common, main
}

func TestProviderPrefersActualMembershipOverExplicitDefault(t *testing.T) {
	f, common, _ := providerFixture(t)
	f.Out(t, "git", "config", "ccx.stack", "graphite")
	for branch, want := range map[string]string{"native": "github", "shared": "github", "legacy": "graphite", "new-branch": "graphite", "main": "graphite"} {
		got, err := gtmeta.Provider(f.Context(), common, branch)
		if err != nil || got != want {
			t.Fatalf("Provider(%s) = %q, %v; want %q", branch, got, err, want)
		}
	}
	f.Out(t, "git", "config", "ccx.stack", "github")
	if got, err := gtmeta.Provider(f.Context(), common, "legacy"); err != nil || got != "github" {
		t.Fatalf("Provider(legacy) = %q, %v; want explicit ccx.stack=github over the Graphite row", got, err)
	}
	if got, err := gtmeta.Provider(gtmeta.WithProvider(f.Context(), "graphite"), common, "legacy"); err != nil || got != "graphite" {
		t.Fatalf("Provider(legacy) in a Graphite-bound lane = %q, %v; want graphite", got, err)
	}
	f.Out(t, "git", "config", "--unset", "ccx.stack")
	if got, err := gtmeta.Provider(f.Context(), common, "legacy"); err != nil || got != "graphite" {
		t.Fatalf("Provider(legacy) without ccx.stack = %q, %v; want the Graphite row to keep it", got, err)
	}
	if got, err := gtmeta.Provider(f.Context(), common, "main"); err != nil || got != "github" {
		t.Fatalf("legacy trunk forced new stack away from native default: %q, %v", got, err)
	}
	if tracked, err := gtmeta.NativeMembership(common, "main"); err != nil || tracked {
		t.Fatalf("native trunk counted as branch membership: %v, %v", tracked, err)
	}
}

func TestForcedMetadataReadsKeepProviderForestsSeparate(t *testing.T) {
	f, common, _ := providerFixture(t)
	mixed, err := gtmeta.Read(f.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if mixed["shared"].Parents[0].Ref != "main" {
		t.Fatalf("native metadata did not override legacy shared row: %+v", mixed["shared"])
	}
	native, err := gtmeta.Read(gtmeta.WithProvider(f.Context(), "github"), common)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := native["legacy"]; ok {
		t.Fatal("native lane read a legacy branch")
	}
	legacy, err := gtmeta.Read(gtmeta.WithProvider(f.Context(), "graphite"), common)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := legacy["native"]; ok {
		t.Fatal("legacy lane read a native-only branch")
	}
	if _, ok := legacy["shared"]; ok {
		t.Fatal("forced legacy read exposed a native-owned overlap")
	}
	if legacy["legacy"].Parents[0].Ref != "main" {
		t.Fatalf("forced legacy read changed legacy parent: %+v", legacy["legacy"])
	}
}

func TestMetadataWritesPartitionAndPreserveTheOtherStore(t *testing.T) {
	f, common, main := providerFixture(t)
	versions := map[string]gtmeta.Version{
		"legacy": {HeadSha: main, BaseSha: main, BaseName: "main"},
		"native": {HeadSha: main, BaseSha: main, BaseName: "main"},
	}
	if err := gtmeta.RecordSubmitted(f.Context(), common, versions); err != nil {
		t.Fatal(err)
	}
	for provider, branch := range map[string]string{"github": "native", "graphite": "legacy"} {
		recorded, err := gtmeta.LastSubmitted(gtmeta.WithProvider(f.Context(), provider), common)
		if err != nil {
			t.Fatal(err)
		}
		if len(recorded) != 1 || recorded[branch] != versions[branch] {
			t.Fatalf("%s receipts = %+v", provider, recorded)
		}
	}
	legacyBefore, err := os.ReadFile(filepath.Join(common, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gtmeta.Forget(gtmeta.WithProvider(f.Context(), "github"), common, []string{"native"}); err != nil {
		t.Fatal(err)
	}
	legacyAfter, err := os.ReadFile(filepath.Join(common, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(legacyAfter) != string(legacyBefore) {
		t.Fatal("native prune wrote legacy metadata")
	}
	native, err := gtmeta.NativeMembership(common, "native")
	if err != nil || native {
		t.Fatalf("native branch remains tracked after native prune: %v, %v", native, err)
	}
}

func TestMixedProviderReparentRefusesWithoutChangingEitherStore(t *testing.T) {
	f, common, _ := providerFixture(t)
	legacyBefore, err := os.ReadFile(filepath.Join(common, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	nativeBefore, err := os.ReadFile(filepath.Join(common, "gh-stack"))
	if err != nil {
		t.Fatal(err)
	}
	err = gtmeta.Reparent(f.Context(), common, map[string]string{"legacy": "native"})
	if err == nil || !strings.Contains(err.Error(), "conversion") {
		t.Fatalf("cross-provider reparent = %v", err)
	}
	for path, want := range map[string][]byte{".graphite_metadata.db": legacyBefore, "gh-stack": nativeBefore} {
		got, err := os.ReadFile(filepath.Join(common, path))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("refused conversion changed %s", path)
		}
	}
}

func TestNativeReadBeforeInitializationDoesNotTouchLegacyMetadata(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	common := filepath.Join(f.Dir, ".git")
	state, err := gtmeta.Read(gtmeta.WithProvider(f.Context(), "github"), common)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 || !state["main"].Trunk {
		t.Fatalf("pre-init native state = %+v", state)
	}
	trunk, err := gtmeta.ReadTrunk(gtmeta.WithProvider(f.Context(), "github"), common)
	if err != nil || trunk != "main" {
		t.Fatalf("pre-init trunk=%q error=%v", trunk, err)
	}
	for _, path := range []string{"gh-stack", ".graphite_repo_config", ".graphite_metadata.db"} {
		if _, err := os.Stat(filepath.Join(common, path)); !os.IsNotExist(err) {
			t.Fatalf("read created %s: %v", path, err)
		}
	}
}

func TestExplicitNativeContextDoesNotConvertTrackedGraphiteBranch(t *testing.T) {
	f, common, main := providerFixture(t)
	err := gtmeta.AdoptRoot(gtmeta.WithProvider(f.Context(), "github"), common, "legacy", "main", main, main)
	if err == nil || !strings.Contains(err.Error(), "conversion") {
		t.Fatalf("implicit conversion = %v", err)
	}
	provider, err := gtmeta.Provider(context.Background(), common, "legacy")
	if err != nil || provider != "graphite" {
		t.Fatalf("legacy ownership changed after refused conversion: %q, %v", provider, err)
	}
}

func TestLegacyForestLeavesOutOnlyTheStackOnANativeParent(t *testing.T) {
	f, common, main := providerFixture(t)
	f.Out(t, "git", "branch", "stranded")
	f.Out(t, "git", "branch", "above")
	db, err := sql.Open("sqlite", filepath.Join(common, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT INTO branch_metadata(branch_name,parent_branch_name,parent_branch_revision,validation_result,children) VALUES('stranded','shared',?,'VALID','["above"]'),('above','stranded',?,'VALID','[]')`, main, main); err != nil {
		t.Fatal(err)
	}
	legacy := gtmeta.WithProvider(f.Context(), "graphite")
	state, err := gtmeta.Read(legacy, common)
	if err != nil {
		t.Fatalf("read refused a forest with one stack on a native parent: %v", err)
	}
	if _, ok := state["legacy"]; !ok {
		t.Fatalf("read dropped legacy, a stack with no native parent: %v", state)
	}
	if _, ok := state["stranded"]; ok {
		t.Fatalf("read kept stranded, which sits on native shared: %v", state)
	}
	if _, ok := state["above"]; ok {
		t.Fatalf("read kept above, stacked on stranded: %v", state)
	}
	if _, orphans, err := gtmeta.ReadOriginOrphans(legacy, common); err != nil || slices.ContainsFunc(orphans, func(o gtmeta.Orphan) bool { return o.Branch == "stranded" || o.Branch == "above" }) {
		t.Fatalf("read origin = %v, %v; want the crossed stack left out", orphans, err)
	}
	rows, err := gtmeta.Rows(legacy, common)
	if err != nil || slices.ContainsFunc(rows, func(row gtmeta.Row) bool { return row.Branch == "stranded" || row.Branch == "above" }) {
		t.Fatalf("prune rows = %v, %v; want the crossed stack left out", rows, err)
	}
	if got, err := gtmeta.Provider(f.Context(), common, "stranded"); err != nil || got != "graphite" {
		t.Fatalf("reading the forest changed existing ownership: %q, %v", got, err)
	}
}

func TestForcedNativePlannerReadsOnlyTheCommonTrunkForest(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	common := filepath.Join(f.Dir, ".git")
	main := strings.TrimSpace(f.Out(t, "git", "rev-parse", "main"))
	for _, branch := range []string{"release", "feature", "fix"} {
		f.Out(t, "git", "branch", branch)
	}
	catalog := fmt.Sprintf(`{"schemaVersion":1,"stacks":[{"trunk":{"branch":"main"},"branches":[{"branch":"feature","base":%[1]q}]},{"trunk":{"branch":"release"},"branches":[{"branch":"fix","base":%[1]q}]}]}`, main)
	if err := os.WriteFile(filepath.Join(common, "gh-stack"), []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := gtmeta.Read(gtmeta.WithProvider(f.Context(), "github"), common)
	if err != nil {
		t.Fatal(err)
	}
	if !state["main"].Trunk || state["feature"].Parents[0].Ref != "main" {
		t.Fatalf("planner lost the common trunk's stack: %+v", state)
	}
	if _, ok := state["release"]; ok {
		t.Fatalf("planner read a second trunk: %+v", state)
	}
	if _, ok := state["fix"]; ok {
		t.Fatalf("planner read a stack on another trunk: %+v", state)
	}
	state, err = gtmeta.Read(f.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if !state["main"].Trunk || !state["release"].Trunk || state["fix"].Parents[0].Ref != "release" {
		t.Fatalf("generic inspection lost actual trunks: %+v", state)
	}
}

func mixedStackFixture(t *testing.T) (*vcstest.Fixture, string, map[string]string) {
	t.Helper()
	f := vcstest.Repo(t, vcstest.Remote())
	common := filepath.Join(f.Dir, ".git")
	heads := map[string]string{"main": strings.TrimSpace(f.Out(t, "git", "rev-parse", "main"))}
	for _, branch := range []string{"bottom", "middle", "tip"} {
		f.Out(t, "git", "checkout", "-q", "-b", branch)
		f.Out(t, "git", "commit", "-q", "--allow-empty", "-m", branch)
		heads[branch] = strings.TrimSpace(f.Out(t, "git", "rev-parse", branch))
	}
	f.Out(t, "git", "checkout", "-q", "main")
	vcstest.WriteGraphiteMeta(t, common, fmt.Sprintf(`{"main":{"trunk":true},"bottom":{"parents":[{"ref":"main","sha":%q}]},"middle":{"parents":[{"ref":"bottom","sha":%q}]}}`, heads["main"], heads["bottom"]))
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	f.Out(t, "git", "config", "ccx.stack", "github")
	return f, common, heads
}

func TestExplicitGitHubStackConvertsAGraphiteParentChainOnAdopt(t *testing.T) {
	f, common, heads := mixedStackFixture(t)
	native := gtmeta.WithProvider(f.Context(), "github")
	if err := gtmeta.AdoptRoot(native, common, "tip", "middle", heads["middle"], heads["tip"]); err != nil {
		t.Fatalf("AdoptRoot(tip onto Graphite middle) = %v; want the Graphite chain converted", err)
	}
	state, err := gtmeta.Read(native, common)
	if err != nil {
		t.Fatal(err)
	}
	for branch, parent := range map[string]string{"bottom": "main", "middle": "bottom", "tip": "middle"} {
		got := state[branch]
		if len(got.Parents) != 1 || got.Parents[0].Ref != parent || got.Parents[0].SHA != heads[parent] || got.Head != heads[branch] || got.NeedsRestack {
			t.Fatalf("native %s = %+v; want on %s at %s", branch, got, parent, heads[parent])
		}
	}
}

func TestConvertMovesOnlyTheRequestedDownstackAndLeavesGraphiteUntouched(t *testing.T) {
	f, common, _ := mixedStackFixture(t)
	legacyBefore, err := os.ReadFile(filepath.Join(common, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gtmeta.Convert(f.Context(), common, "bottom", "tip"); err != nil {
		t.Fatal(err)
	}
	for branch, want := range map[string]bool{"bottom": true, "middle": false, "tip": false} {
		if got, err := gtmeta.NativeMembership(common, branch); err != nil || got != want {
			t.Fatalf("native membership of %s = %v, %v; want %v", branch, got, err, want)
		}
	}
	legacyAfter, err := os.ReadFile(filepath.Join(common, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(legacyAfter) != string(legacyBefore) {
		t.Fatal("conversion wrote Graphite metadata")
	}
	f.Out(t, "git", "config", "ccx.stack", "graphite")
	if err := gtmeta.Convert(f.Context(), common, "middle"); err != nil {
		t.Fatal(err)
	}
	if got, err := gtmeta.NativeMembership(common, "middle"); err != nil || got {
		t.Fatalf("ccx.stack=graphite converted middle: %v, %v", got, err)
	}
}
