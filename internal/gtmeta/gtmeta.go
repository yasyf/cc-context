// Package gtmeta reads a Graphite repository's tracked branch state straight
// from the SQLite database gt keeps in the git common dir.
package gtmeta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/render"

	// modernc.org/sqlite registers the "sqlite" driver, and is the pure-Go one:
	// a cgo driver would break ccx's cross-compiled release build.
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

// BranchState is one branch's tracked state. Head is the branch's own ref, so
// a caller submitting the stack need not re-ask git per branch. State is the
// hold gt has put the branch under — "frozen" for gt freeze, "merging" for a
// branch mid-merge — and empty for a branch under no hold at all, which is
// every branch most of the time.
type BranchState struct {
	Trunk        bool
	NeedsRestack bool
	Head         string
	State        string
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
		head, live := heads[row.branch]
		if !live {
			continue
		}
		if row.branch == trunk {
			state[row.branch] = BranchState{Trunk: true, Head: head}
			continue
		}
		parentHead, parentLive := heads[row.parent]
		if row.validation != validationValid || !parentLive {
			continue
		}
		state[row.branch] = BranchState{
			NeedsRestack: row.parentRevision != parentHead,
			Head:         head,
			State:        row.state,
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
	state          string
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
		       COALESCE(validation_result, ''),
		       COALESCE(state, '')
		FROM branch_metadata`)
	if err != nil {
		return nil, fmt.Errorf("gtmeta: query %q: %w", path, err)
	}
	defer func() { _ = cursor.Close() }()

	var rows []branchRow
	for cursor.Next() {
		var row branchRow
		if err := cursor.Scan(&row.branch, &row.parent, &row.parentRevision, &row.validation, &row.state); err != nil {
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
// Parent is the branch gt recorded this one as sitting on, empty on the trunk
// row, so a caller forgetting rows can see which survivors it would strand.
type Row struct {
	Branch   string
	Parent   string
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
		out = append(out, Row{Branch: row.branch, Parent: row.parent, Stale: !live, Diverged: live && !resolved})
	}
	return out, nil
}

// Forget deletes branches' rows, which is what gt untrack does to one branch at
// a time for a whole gt startup each. The delete is two-sided like Reparent's
// move: a name left in the parent's children column is one gt still walks into
// and finds no row for, so each branch is disowned by its parent first.
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
		var parent string
		row := tx.QueryRowContext(ctx, `SELECT COALESCE(parent_branch_name, '') FROM branch_metadata WHERE branch_name = ?`, branch)
		switch err := row.Scan(&parent); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return fmt.Errorf("gtmeta: read parent of %q in %q: %w", branch, path, err)
		}
		if _, err := editChildren(ctx, tx, path, parent, func(children []string) []string {
			return slices.DeleteFunc(children, func(child string) bool { return child == branch })
		}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM branch_metadata WHERE branch_name = ?`, branch); err != nil {
			return fmt.Errorf("gtmeta: forget %q in %q: %w", branch, path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("gtmeta: commit to %q: %w", path, err)
	}
	return nil
}

// Reparent moves each branch onto a new parent, which gt has no verb for. The
// move is two-sided: gt walks its tree through the children column, so a branch
// missing from its new parent's children vanishes from gt log even though gt
// state reports the move. The parent revision stays put, so a moved branch
// reads as needing the restack it does need.
func Reparent(ctx context.Context, commonDir string, moves map[string]string) error {
	if len(moves) == 0 {
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

	for _, branch := range slices.Sorted(maps.Keys(moves)) {
		if err := reparentOne(ctx, tx, path, branch, moves[branch]); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("gtmeta: commit to %q: %w", path, err)
	}
	return nil
}

func reparentOne(ctx context.Context, tx *sql.Tx, path, branch, parent string) error {
	previous, err := parentOf(ctx, tx, path, branch)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE branch_metadata SET parent_branch_name = ? WHERE branch_name = ?`, parent, branch); err != nil {
		return fmt.Errorf("gtmeta: reparent %q onto %q in %q: %w", branch, parent, path, err)
	}
	adopted, err := editChildren(ctx, tx, path, parent, func(children []string) []string {
		if slices.Contains(children, branch) {
			return children
		}
		return append(children, branch)
	})
	if err != nil {
		return err
	}
	if !adopted {
		return fmt.Errorf("gtmeta: %q has no branch_metadata row in %q", parent, path)
	}
	_, err = editChildren(ctx, tx, path, previous, func(children []string) []string {
		return slices.DeleteFunc(children, func(child string) bool { return child == branch })
	})
	return err
}

func parentOf(ctx context.Context, tx *sql.Tx, path, branch string) (string, error) {
	var parent string
	row := tx.QueryRowContext(ctx, `SELECT COALESCE(parent_branch_name, '') FROM branch_metadata WHERE branch_name = ?`, branch)
	switch err := row.Scan(&parent); {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("gtmeta: %q has no branch_metadata row in %q", branch, path)
	case err != nil:
		return "", fmt.Errorf("gtmeta: read parent of %q in %q: %w", branch, path, err)
	}
	return parent, nil
}

// editChildren rewrites parent's children through edit, reporting whether the
// row was there to edit: the parent a moved branch leaves is often the row the
// same prune is about to forget, and disowning a row that is already gone is
// the move landing, not a failure.
func editChildren(ctx context.Context, tx *sql.Tx, path, parent string, edit func([]string) []string) (bool, error) {
	var payload string
	row := tx.QueryRowContext(ctx, `SELECT COALESCE(children, '[]') FROM branch_metadata WHERE branch_name = ?`, parent)
	switch err := row.Scan(&payload); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("gtmeta: read children of %q in %q: %w", parent, path, err)
	}
	var children []string
	if err := json.Unmarshal([]byte(payload), &children); err != nil {
		return false, fmt.Errorf("gtmeta: parse children of %q in %q: %w", parent, path, err)
	}
	encoded, err := json.Marshal(edit(children))
	if err != nil {
		return false, fmt.Errorf("gtmeta: encode children of %q: %w", parent, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE branch_metadata SET children = ? WHERE branch_name = ?`, string(encoded), parent); err != nil {
		return false, fmt.Errorf("gtmeta: record children of %q in %q: %w", parent, path, err)
	}
	return true, nil
}

func writableDSN(path string) string {
	return "file:" + (&url.URL{Path: path}).String() + "?_pragma=busy_timeout(5000)"
}

// Version is the branch_metadata.last_submitted_version blob: the shas gt
// recorded for a branch's last submitted PR version, matching what gt itself
// writes on submit.
type Version struct {
	HeadSha  string `json:"headSha"`
	BaseSha  string `json:"baseSha"`
	BaseName string `json:"baseName"`
}

// LastSubmitted reads every branch's last submitted version out of the
// metadata database, skipping branches never submitted.
func LastSubmitted(ctx context.Context, commonDir string) (map[string]Version, error) {
	path := filepath.Join(commonDir, metadataDB)
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("gtmeta: open %q: %w", path, err)
	}
	defer func() { _ = db.Close() }()

	cursor, err := db.QueryContext(ctx, `
		SELECT branch_name, last_submitted_version
		FROM branch_metadata
		WHERE last_submitted_version IS NOT NULL AND last_submitted_version != ''`)
	if err != nil {
		return nil, fmt.Errorf("gtmeta: query %q: %w", path, err)
	}
	defer func() { _ = cursor.Close() }()

	versions := map[string]Version{}
	for cursor.Next() {
		var branch, payload string
		if err := cursor.Scan(&branch, &payload); err != nil {
			return nil, fmt.Errorf("gtmeta: scan %q: %w", path, err)
		}
		var v Version
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			return nil, fmt.Errorf("gtmeta: parse last_submitted_version of %q in %q: %w", branch, path, err)
		}
		versions[branch] = v
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("gtmeta: read %q: %w", path, err)
	}
	return versions, nil
}

// RecordSubmitted writes each branch's last_submitted_version, which is what
// gt reads to decide the branch was ever submitted and which remote head its
// next force-with-lease may replace. One transaction, matching the atomic push
// it records. A branch without a row is an error: only a tracked branch can
// have been submitted.
func RecordSubmitted(ctx context.Context, commonDir string, versions map[string]Version) error {
	if len(versions) == 0 {
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

	for _, branch := range slices.Sorted(maps.Keys(versions)) {
		payload, err := json.Marshal(versions[branch])
		if err != nil {
			return fmt.Errorf("gtmeta: encode last_submitted_version of %q: %w", branch, err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE branch_metadata SET last_submitted_version = ? WHERE branch_name = ?`, string(payload), branch)
		if err != nil {
			return fmt.Errorf("gtmeta: record %q in %q: %w", branch, path, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("gtmeta: record %q in %q: %w", branch, path, err)
		}
		if affected != 1 {
			return fmt.Errorf("gtmeta: %q has no branch_metadata row in %q", branch, path)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("gtmeta: commit to %q: %w", path, err)
	}
	return nil
}

// RecordRestacked writes each branch's parent_branch_revision, the column gt
// compares against its parent's live head to decide the branch needs a restack.
// Nothing else about a restacked branch changes: its parent's name is the same,
// and its row stays valid. A branch without a row is an error, since only a
// tracked branch can have been restacked.
func RecordRestacked(ctx context.Context, commonDir string, revisions map[string]string) error {
	if len(revisions) == 0 {
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

	for _, branch := range slices.Sorted(maps.Keys(revisions)) {
		result, err := tx.ExecContext(ctx, `UPDATE branch_metadata SET parent_branch_revision = ? WHERE branch_name = ?`, revisions[branch], branch)
		if err != nil {
			return fmt.Errorf("gtmeta: record %q in %q: %w", branch, path, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("gtmeta: record %q in %q: %w", branch, path, err)
		}
		if affected != 1 {
			return fmt.Errorf("gtmeta: %q has no branch_metadata row in %q", branch, path)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("gtmeta: commit to %q: %w", path, err)
	}
	return nil
}
