package gtmeta_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
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

func TestFocusedGraphiteReadPreservesGlobalProviderValidation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		childLive  bool
		parentLive bool
		validation string
		native     bool
		wantError  bool
	}{
		{"unrelated live cross-provider parent", true, true, "VALID", false, true},
		{"unrelated missing child head", false, true, "VALID", false, false},
		{"unrelated missing native parent head", true, false, "VALID", false, true},
		{"unrelated invalid child with live parent", true, true, "UNKNOWN", false, false},
		{"unrelated invalid orphan", true, false, "UNKNOWN", false, true},
		{"native-owned cross-provider row", true, true, "VALID", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, common, head := providerFixture(t)
			branch := "stranded"
			if tc.native {
				branch = "shared"
			} else if tc.childLive {
				f.Out(t, "git", "branch", branch)
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
			ctx := gtmeta.WithProvider(f.Context(), gtmeta.ProviderGraphite)
			_, _, fullErr := gtmeta.ReadOriginOrphans(ctx, common)
			state, _, selectedErr := gtmeta.ReadOriginOrphansForBranches(ctx, common, []string{"legacy"})
			if (fullErr != nil) != tc.wantError || (selectedErr != nil) != tc.wantError {
				t.Fatalf("full error = %v, selected error = %v, want error %t", fullErr, selectedErr, tc.wantError)
			}
			if tc.wantError {
				if fullErr.Error() != selectedErr.Error() {
					t.Fatalf("selected error = %v, want %v", selectedErr, fullErr)
				}
				return
			}
			if state["legacy"].Head != head {
				t.Fatalf("legacy branch changed: %+v", state["legacy"])
			}
		})
	}
}
