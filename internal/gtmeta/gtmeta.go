// Package gtmeta reads a Graphite repository's tracked branch state straight
// from the SQLite database gt keeps in the git common dir.
package gtmeta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/internal/render"

	_ "modernc.org/sqlite"
)

const (
	metadataDB = ".graphite_metadata.db"
	repoConfig = ".graphite_repo_config"
)

// validationValid is what gt records for a branch whose parent it resolved.
const validationValid = "VALID"

// Ref is one parent entry: the parent branch's name, and the revision it stood
// at when the child was last restacked onto it.
type Ref struct {
	Ref string
	SHA string
}

// BranchState is one branch's tracked state.
type BranchState struct {
	Trunk        bool
	NeedsRestack bool
	Parents      []Ref
}

// State maps a branch name to its tracked state.
type State map[string]BranchState

// Read returns the tracked state of every branch in the Graphite repository
// whose git common dir is commonDir — what `gt state` answers, for one SQLite
// query and one for-each-ref instead of the six seconds gt spends walking
// every ref in a large repository.
//
// A branch is tracked only when its row is valid and both it and its parent
// still have a live ref: gt's metadata outlives the branches it describes.
func Read(ctx context.Context, commonDir string) (State, error) {
	trunk, err := readTrunk(commonDir)
	if err != nil {
		return nil, err
	}
	heads, err := readHeads(ctx, commonDir)
	if err != nil {
		return nil, err
	}
	rows, err := readRows(ctx, filepath.Join(commonDir, metadataDB))
	if err != nil {
		return nil, err
	}

	state := State{}
	for _, row := range rows {
		if _, live := heads[row.branch]; !live {
			continue
		}
		if row.branch == trunk {
			state[row.branch] = BranchState{Trunk: true}
			continue
		}
		parentHead, parentLive := heads[row.parent]
		if row.validation != validationValid || !parentLive {
			continue
		}
		state[row.branch] = BranchState{
			NeedsRestack: row.parentRevision != parentHead,
			Parents:      []Ref{{Ref: row.parent, SHA: row.parentRevision}},
		}
	}
	return state, nil
}

type branchRow struct {
	branch         string
	parent         string
	parentRevision string
	validation     string
}

// readRows opens the database read-only so a concurrent gt is never blocked,
// and waits out a writer's lock rather than failing on it.
func readRows(ctx context.Context, path string) ([]branchRow, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("gtmeta: open %q: %w", path, err)
	}
	defer func() { _ = db.Close() }()

	cursor, err := db.QueryContext(ctx, `
		SELECT branch_name,
		       COALESCE(parent_branch_name, ''),
		       COALESCE(parent_branch_revision, ''),
		       COALESCE(validation_result, '')
		FROM branch_metadata`)
	if err != nil {
		return nil, fmt.Errorf("gtmeta: query %q: %w", path, err)
	}
	defer func() { _ = cursor.Close() }()

	var rows []branchRow
	for cursor.Next() {
		var row branchRow
		if err := cursor.Scan(&row.branch, &row.parent, &row.parentRevision, &row.validation); err != nil {
			return nil, fmt.Errorf("gtmeta: scan %q: %w", path, err)
		}
		rows = append(rows, row)
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("gtmeta: read %q: %w", path, err)
	}
	return rows, nil
}

func dsn(path string) string {
	return "file:" + (&url.URL{Path: path}).String() + "?mode=ro&_pragma=busy_timeout(5000)"
}

func readTrunk(commonDir string) (string, error) {
	path := filepath.Join(commonDir, repoConfig)
	payload, err := os.ReadFile(path) //nolint:gosec // path is the caller's own git common dir
	if err != nil {
		return "", fmt.Errorf("gtmeta: read %q: %w", path, err)
	}
	var config struct {
		Trunk string `json:"trunk"`
	}
	if err := json.Unmarshal(payload, &config); err != nil {
		return "", fmt.Errorf("gtmeta: parse %q: %w", path, err)
	}
	if config.Trunk == "" {
		return "", fmt.Errorf("gtmeta: %q names no trunk", path)
	}
	return config.Trunk, nil
}

// readHeads reads every local branch's head in one pass. commonDir can be a
// bare admin dir rather than a working copy, so git is pointed at it by --git-dir.
func readHeads(ctx context.Context, commonDir string) (map[string]string, error) {
	argv := []string{"--git-dir=" + commonDir, "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads/"}
	out, err := render.RunCLI(ctx, render.Dir(commonDir), "git", argv)
	if err != nil {
		return nil, fmt.Errorf("gtmeta: list branches in %q: %w", commonDir, err)
	}
	heads := map[string]string{}
	for line := range strings.Lines(out) {
		name, sha, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		heads[name] = sha
	}
	return heads, nil
}

// validationTrunk is what gt records for the trunk row, whose parent is null by
// construction.
const validationTrunk = "TRUNK"

// Row is one branch gt tracks and what a prune may do with it: a row whose
// branch no longer has a ref is stale, and a live branch gt could not resolve
// is diverged, whose remedy is gt track or gt untrack rather than deletion.
type Row struct {
	Branch   string
	Stale    bool
	Diverged bool
}

// Rows reports every branch gt tracks, including the ones Read drops.
func Rows(ctx context.Context, commonDir string) ([]Row, error) {
	heads, err := readHeads(ctx, commonDir)
	if err != nil {
		return nil, err
	}
	rows, err := readRows(ctx, filepath.Join(commonDir, metadataDB))
	if err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(rows))
	for _, row := range rows {
		_, live := heads[row.branch]
		resolved := row.validation == validationValid || row.validation == validationTrunk
		out = append(out, Row{Branch: row.branch, Stale: !live, Diverged: live && !resolved})
	}
	return out, nil
}

// Forget deletes branches' rows, which is what gt untrack does to one branch at
// a time for a whole gt startup each.
func Forget(ctx context.Context, commonDir string, branches []string) error {
	if len(branches) == 0 {
		return nil
	}
	path := filepath.Join(commonDir, metadataDB)
	db, err := sql.Open("sqlite", writableDSN(path))
	if err != nil {
		return fmt.Errorf("gtmeta: open %q: %w", path, err)
	}
	defer func() { _ = db.Close() }()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("gtmeta: begin on %q: %w", path, err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, branch := range branches {
		if _, err := tx.ExecContext(ctx, `DELETE FROM branch_metadata WHERE branch_name = ?`, branch); err != nil {
			return fmt.Errorf("gtmeta: forget %q in %q: %w", branch, path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("gtmeta: commit to %q: %w", path, err)
	}
	return nil
}

func writableDSN(path string) string {
	return "file:" + (&url.URL{Path: path}).String() + "?_pragma=busy_timeout(5000)"
}
