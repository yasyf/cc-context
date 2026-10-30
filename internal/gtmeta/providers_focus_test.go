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
