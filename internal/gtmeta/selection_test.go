package gtmeta

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSelectedRowsPreserveOnlyRequestedStackTopology(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graphite.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, statement := range []string{
		`CREATE TABLE branch_metadata(branch_name TEXT PRIMARY KEY,parent_branch_name TEXT,parent_branch_revision TEXT,validation_result TEXT,state TEXT)`,
		`CREATE INDEX idx_branch_metadata_parent ON branch_metadata(parent_branch_name)`,
		`INSERT INTO branch_metadata VALUES ('main',NULL,NULL,'TRUNK',NULL),('base','main','a','VALID','none'),('selected','base','b','VALID','frozen'),('child','selected','c','BAD_PARENT_REVISION','merging'),('sibling','base','b','VALID','none'),('unrelated','main','a','VALID','none'),('orphan','deleted','d','VALID','none'),('cycle-a','cycle-b','e','VALID','none'),('cycle-b','cycle-a','f','VALID','none')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name     string
		branches []string
		want     []string
	}{
		{"selected", []string{"selected"}, []string{"base", "child", "main", "selected"}},
		{"trunk", []string{"main"}, []string{"main"}},
		{"missing parent", []string{"orphan"}, []string{"main", "orphan"}},
		{"cycle", []string{"cycle-a"}, []string{"cycle-a", "cycle-b", "main"}},
		{"shared ancestors", []string{"selected", "sibling", "selected"}, []string{"base", "child", "main", "selected", "sibling"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := readSelectedRows(context.Background(), path, tc.branches, "main")
			if err != nil {
				t.Fatal(err)
			}
			names := make([]string, len(rows))
			for i, row := range rows {
				names[i] = row.branch
				if row.branch == "child" && (row.validation != "BAD_PARENT_REVISION" || row.state != "merging") {
					t.Fatalf("selected row changed: %+v", row)
				}
			}
			slices.Sort(names)
			if !reflect.DeepEqual(names, tc.want) {
				t.Fatalf("selected rows = %v, want %v", names, tc.want)
			}
		})
	}
}

func TestSelectedRowsUsesMetadataIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graphite.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, statement := range []string{
		`CREATE TABLE branch_metadata(branch_name TEXT PRIMARY KEY,parent_branch_name TEXT,parent_branch_revision TEXT,validation_result TEXT,state TEXT)`,
		`CREATE INDEX idx_branch_metadata_parent ON branch_metadata(parent_branch_name)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	query, args := selectedRowsQuery([]string{"selected"}, "main")
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
		if strings.Contains(detail, "SCAN m") || strings.Contains(detail, "SCAN branch_metadata") {
			t.Fatalf("metadata query scans unrelated rows: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "USING INDEX idx_branch_metadata_parent") || !strings.Contains(plan, "USING INDEX sqlite_autoindex_branch_metadata_1") {
		t.Fatalf("metadata query does not use parent and branch indexes:\n%s", plan)
	}
	t.Log(plan)
}
