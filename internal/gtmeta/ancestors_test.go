package gtmeta

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestAncestorRowsDoNotReadPublicationParentsDescendants(t *testing.T) {
	for _, size := range []int{32, 4096} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "graphite.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			for _, statement := range []string{
				`CREATE TABLE branch_metadata(branch_name TEXT PRIMARY KEY,parent_branch_name TEXT,parent_branch_revision TEXT,validation_result TEXT,state TEXT)`,
				`INSERT INTO branch_metadata VALUES ('main',NULL,NULL,'TRUNK',NULL),('base','main','a','VALID','none'),('publication','base','b','VALID','frozen'),('cycle-a','cycle-b','c','VALID','none'),('cycle-b','cycle-a','d','VALID','none')`,
				fmt.Sprintf(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<%d) INSERT INTO branch_metadata SELECT 'unrelated-'||i,'publication','b','VALID','none' FROM n`, size),
			} {
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			for _, tc := range []struct {
				branches []string
				want     []string
			}{
				{[]string{"publication", "publication", "missing"}, []string{"base", "main", "publication"}},
				{[]string{"cycle-a"}, []string{"cycle-a", "cycle-b", "main"}},
			} {
				query, args := ancestorRowsQuery(tc.branches, "main")
				rows, err := queryRows(context.Background(), path, query, args...)
				if err != nil {
					t.Fatal(err)
				}
				var names []string
				for _, row := range rows {
					names = append(names, row.branch)
					if row.branch == "publication" && (row.state != "frozen" || row.parentRevision != "b") {
						t.Fatalf("publication metadata changed: %+v", row)
					}
				}
				slices.Sort(names)
				if !reflect.DeepEqual(names, tc.want) {
					t.Fatalf("ancestor names = %v, want %v", names, tc.want)
				}
				plan, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
				if err != nil {
					t.Fatal(err)
				}
				var indexed bool
				for plan.Next() {
					var id, parent, unused int
					var detail string
					if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
						t.Fatal(err)
					}
					if strings.Contains(detail, "SCAN m") || strings.Contains(detail, "SCAN branch_metadata") || strings.Contains(detail, "AUTOMATIC") {
						t.Fatalf("ancestor query walks unrelated metadata: %s", detail)
					}
					indexed = indexed || strings.Contains(detail, "USING INDEX sqlite_autoindex_branch_metadata_1")
				}
				if err := plan.Err(); err != nil {
					t.Fatal(err)
				}
				if err := plan.Close(); err != nil {
					t.Fatal(err)
				}
				if !indexed {
					t.Fatal("ancestor query did not use the existing primary-key index")
				}
				t.Logf("unrelated_rows=%d returned_ancestors=%d existing_primary_key_index=true", size, len(rows))
			}
		})
	}
}
