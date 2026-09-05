package vcstest

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	// modernc.org/sqlite registers the "sqlite" driver the metadata database is
	// written through, and is the pure-Go one: a cgo driver would break ccx's
	// cross-compiled release build.
	_ "modernc.org/sqlite"
)

const (
	graphiteMetadataDB = ".graphite_metadata.db"
	graphiteRepoConfig = ".graphite_repo_config"

	// GraphiteRefsFile is the branch listing WriteGraphiteMeta leaves in the
	// metadata directory, one "<branch> <sha>" line per branch, for a fake git to
	// serve as for-each-ref.
	GraphiteRefsFile = "refs.txt"
)

// graphiteBranchMetadata is gt's own schema for the table it tracks branches in.
const graphiteBranchMetadata = `CREATE TABLE IF NOT EXISTS "branch_metadata" ("branch_name" text not null primary key, ` +
	`"parent_branch_name" text, "parent_branch_revision" text, "last_submitted_version" text, "state" text, ` +
	`"children" text, "branch_revision" text, "validation_result" text, "parent_head_revision" text)`

const (
	graphiteValidationTrunk = "TRUNK"
	graphiteValidationValid = "VALID"
)

// GraphiteLeafSHA stands at the head of a branch no other branch names as its
// parent, where no revision has to agree with anything.
const GraphiteLeafSHA = "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"

type graphiteBranch struct {
	Trunk   bool          `json:"trunk"`
	Parents []GraphiteRef `json:"parents"`
}

// GraphiteRef is one parent entry of the state object WriteGraphiteMeta reads:
// the parent branch's name and the revision the child was restacked onto.
type GraphiteRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// WriteGraphiteMeta materializes the tracked state gt reports into the files gt
// itself keeps in a git common dir: the repo config naming the trunk, the
// SQLite database holding one branch_metadata row per branch, and a refs.txt a
// fake git serves as for-each-ref. stateJSON is gt state's own object, mapping
// each branch to {"trunk":true} or to its parents.
//
// A branch's head is whatever its children record as the revision they were
// restacked onto: any other value reads as a stack needing a restack.
func WriteGraphiteMeta(t *testing.T, commonDir, stateJSON string) {
	t.Helper()
	var state map[string]graphiteBranch
	if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
		t.Fatalf("parse gt state %q: %v", stateJSON, err)
	}

	trunk := ""
	heads := make(map[string]string, len(state))
	for name, branch := range state {
		if branch.Trunk {
			trunk = name
		}
		heads[name] = GraphiteLeafSHA
	}
	if trunk == "" {
		t.Fatalf("gt state %q names no trunk branch", stateJSON)
	}
	for _, branch := range state {
		for _, parent := range branch.Parents {
			heads[parent.Ref] = parent.SHA
		}
	}

	config, err := json.Marshal(map[string]string{"trunk": trunk})
	if err != nil {
		t.Fatalf("marshal graphite repo config: %v", err)
	}
	writeGraphiteFile(t, filepath.Join(commonDir, graphiteRepoConfig), string(config))

	var refs strings.Builder
	for _, name := range slices.Sorted(maps.Keys(heads)) {
		fmt.Fprintf(&refs, "%s %s\n", name, heads[name])
	}
	writeGraphiteFile(t, filepath.Join(commonDir, GraphiteRefsFile), refs.String())

	writeGraphiteRows(t, filepath.Join(commonDir, graphiteMetadataDB), state)
}

func writeGraphiteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// graphiteChildren renders each branch's children column, the tree gt walks in
// gt log: a row absent from its parent's children is a branch gt never reaches,
// however the parent pointers read.
func graphiteChildren(t *testing.T, state map[string]graphiteBranch) map[string]string {
	t.Helper()
	kids := make(map[string][]string, len(state))
	for name := range state {
		kids[name] = []string{}
	}
	for _, name := range slices.Sorted(maps.Keys(state)) {
		for _, parent := range state[name].Parents {
			kids[parent.Ref] = append(kids[parent.Ref], name)
		}
	}
	encoded := make(map[string]string, len(kids))
	for name, list := range kids {
		payload, err := json.Marshal(list)
		if err != nil {
			t.Fatalf("marshal children of %s: %v", name, err)
		}
		encoded[name] = string(payload)
	}
	return encoded
}

func writeGraphiteRows(t *testing.T, path string, state map[string]graphiteBranch) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(graphiteBranchMetadata); err != nil {
		t.Fatalf("create branch_metadata in %s: %v", path, err)
	}
	children := graphiteChildren(t, state)
	const insert = `INSERT INTO branch_metadata (branch_name, parent_branch_name, parent_branch_revision, validation_result, children) VALUES (?, ?, ?, ?, ?)`
	for _, name := range slices.Sorted(maps.Keys(state)) {
		branch := state[name]
		if branch.Trunk {
			if _, err := db.Exec(insert, name, nil, nil, graphiteValidationTrunk, children[name]); err != nil {
				t.Fatalf("insert trunk %s into %s: %v", name, path, err)
			}
			continue
		}
		parent := branch.Parents[0]
		if _, err := db.Exec(insert, name, parent.Ref, parent.SHA, graphiteValidationValid, children[name]); err != nil {
			t.Fatalf("insert %s into %s: %v", name, path, err)
		}
	}
}
