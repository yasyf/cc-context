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
	if got, err := gtmeta.Provider(f.Context(), common, "legacy"); err != nil || got != "graphite" {
		t.Fatalf("native default silently converted existing legacy branch: %q, %v", got, err)
	}
	f.Out(t, "git", "config", "--unset", "ccx.stack")
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

func TestForcedNativePlannerRefusesMultipleTrunksWithoutMutation(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	common := filepath.Join(f.Dir, ".git")
	f.Out(t, "git", "branch", "release")
	for _, trunk := range []string{"main", "release"} {
		if err := nativemeta.Init(f.Context(), common, trunk); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(common, "gh-stack"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gtmeta.Read(gtmeta.WithProvider(f.Context(), "github"), common); err == nil || !strings.Contains(err.Error(), "main, release") {
		t.Fatalf("multiple-trunk planning = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(common, "gh-stack"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("refused multi-trunk read changed catalog")
	}
	state, err := gtmeta.Read(f.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if !state["main"].Trunk || !state["release"].Trunk {
		t.Fatalf("generic inspection lost actual trunks: %+v", state)
	}
}
