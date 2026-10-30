package gtmeta_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
)

func TestFocusedProviderReadPreservesNativeOwnershipAndRelatedLegacyBranches(t *testing.T) {
	f, common, head := providerFixture(t)
	for _, branch := range []string{"legacy-child", "unrelated"} {
		f.Out(t, "git", "branch", branch)
	}
	db, err := sql.Open("sqlite", filepath.Join(common, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for branch, parent := range map[string]string{"legacy-child": "native", "unrelated": "main"} {
		if _, err := db.Exec(`INSERT INTO branch_metadata(branch_name,parent_branch_name,parent_branch_revision,validation_result,children) VALUES(?,?,?,'VALID','[]')`, branch, parent, head); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(common, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	full, fullOrphans, err := gtmeta.ReadOriginOrphans(f.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	selected, orphans, err := gtmeta.ReadOriginOrphansForBranches(f.Context(), common, []string{"legacy"})
	if err != nil {
		t.Fatal(err)
	}
	delete(full, "unrelated")
	if !reflect.DeepEqual(selected, full) || !reflect.DeepEqual(orphans, fullOrphans) {
		t.Fatalf("focused state=%+v orphans=%+v; want state=%+v orphans=%+v", selected, orphans, full, fullOrphans)
	}
	if selected["shared"].Parents[0].Ref != "main" || selected["legacy-child"].Parents[0].Ref != "native" {
		t.Fatalf("native ownership or related legacy child changed: %+v", selected)
	}
	after, err := os.ReadFile(filepath.Join(common, ".graphite_metadata.db"))
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("focused read changed Graphite metadata: %v", err)
	}
}

func TestFocusedGraphiteReadPreservesStackLocalProviderPruning(t *testing.T) {
	for _, tc := range []struct {
		name       string
		childLive  bool
		parentLive bool
		validation string
		native     bool
	}{
		{"live cross-provider parent", true, true, "VALID", false},
		{"missing child head", false, true, "VALID", false},
		{"missing native parent head", true, false, "VALID", false},
		{"invalid child with live parent", true, true, "UNKNOWN", false},
		{"invalid orphan", true, false, "UNKNOWN", false},
		{"native-owned cross-provider row", true, true, "VALID", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, common, head := providerFixture(t)
			branch := "stranded"
			if tc.native {
				branch = "shared"
			} else if tc.childLive {
				f.Out(t, "git", "branch", branch)
			}
			for _, child := range []string{"above", "top"} {
				f.Out(t, "git", "branch", child)
			}
			if !tc.parentLive {
				f.Out(t, "git", "branch", "-D", "native")
			}
			db, err := sql.Open("sqlite", filepath.Join(common, ".graphite_metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec(`INSERT INTO branch_metadata(branch_name,parent_branch_name,parent_branch_revision,validation_result,children) VALUES(?,'native',?,?,'[]') ON CONFLICT(branch_name) DO UPDATE SET parent_branch_name = excluded.parent_branch_name, parent_branch_revision = excluded.parent_branch_revision, validation_result = excluded.validation_result`, branch, head, tc.validation); err != nil {
				t.Fatal(err)
			}
			for child, parent := range map[string]string{"above": branch, "top": "above"} {
				if _, err := db.Exec(`INSERT INTO branch_metadata(branch_name,parent_branch_name,parent_branch_revision,validation_result,children) VALUES(?,?,?,'VALID','[]')`, child, parent, head); err != nil {
					t.Fatal(err)
				}
			}
			ctx := gtmeta.WithProvider(f.Context(), gtmeta.ProviderGraphite)
			full, fullOrphans, err := gtmeta.ReadOriginOrphans(ctx, common)
			if err != nil {
				t.Fatal(err)
			}
			for _, requested := range []string{"legacy", branch} {
				state, orphans, err := gtmeta.ReadOriginOrphansForBranches(ctx, common, []string{requested})
				if err != nil {
					t.Fatal(err)
				}
				scope := map[string]bool{"main": true, requested: true}
				if requested == branch {
					scope["above"], scope["top"] = true, true
				}
				want := gtmeta.State{}
				for name, entry := range full {
					if scope[name] {
						want[name] = entry
					}
				}
				var wantOrphans []gtmeta.Orphan
				for _, orphan := range fullOrphans {
					if scope[orphan.Branch] {
						wantOrphans = append(wantOrphans, orphan)
					}
				}
				if !reflect.DeepEqual(state, want) || !slices.Equal(orphans, wantOrphans) {
					t.Fatalf("requested %s: state=%+v orphans=%+v, want state=%+v orphans=%+v", requested, state, orphans, want, wantOrphans)
				}
			}
		})
	}
}
