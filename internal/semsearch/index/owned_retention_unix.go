//go:build !windows

package index

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/yasyf/cc-context/internal/cache"
)

func ownedAnswerHistory(dir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "generations.json")) // #nosec G304 G703 -- fixed manifest in the configured cache's SHA-256 namespace
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var generations []string
	err = json.Unmarshal(data, &generations)
	return generations, err
}

func mapRetainedOwnedAnswers(ctx context.Context, path, identity, generation string, documents, dims int) (*OwnedAnswerIndex, bool, error) {
	var idx *OwnedAnswerIndex
	var found bool
	dir := filepath.Dir(path)
	err := cache.WithLock(ctx, dir, "publication", func() error {
		generations, err := ownedAnswerHistory(dir)
		if err != nil || !slices.Contains(generations, generation) {
			return err
		}
		idx, found, err = mapOwnedAnswers(ctx, path, identity, generation, documents, dims)
		return err
	})
	return idx, found, err
}

func publishRetainedOwnedAnswers(ctx context.Context, path, identity, generation string, documents, dims int, data []byte) (*OwnedAnswerIndex, bool, error) {
	var idx *OwnedAnswerIndex
	var found bool
	dir := filepath.Dir(path)
	err := cache.WithLock(ctx, dir, "publication", func() error {
		history, err := ownedAnswerHistory(dir)
		if err != nil {
			return err
		}
		retained := []string{generation}
		for _, previous := range history {
			if previous != generation {
				retained = append(retained, previous)
				break
			}
		}
		if err := cache.Store(path, data, 0o600); err != nil {
			return err
		}
		idx, found, err = mapOwnedAnswers(ctx, path, identity, generation, documents, dims)
		if err != nil || !found {
			return err
		}
		if err := retireOwnedAnswers(ctx, dir, retained, history); err != nil {
			closeErr := idx.Close()
			idx = nil
			found = false
			return errors.Join(err, closeErr)
		}
		manifest, _ := json.Marshal(retained)
		if err := cache.Store(filepath.Join(dir, "generations.json"), manifest, 0o600); err != nil {
			closeErr := idx.Close()
			idx = nil
			found = false
			return errors.Join(err, closeErr)
		}
		return nil
	})
	return idx, found, err
}

func retireOwnedAnswers(ctx context.Context, dir string, retained, history []string) error {
	db, err := ownedAnswerDB(filepath.Join(dir, "index.sqlite"), "rw")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, "SELECT generation FROM cut ORDER BY generation")
	if err != nil {
		return err
	}
	retired := slices.Clone(history)
	for rows.Next() {
		var generation string
		if err := rows.Scan(&generation); err != nil {
			_ = rows.Close()
			return err
		}
		retired = append(retired, generation)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	slices.Sort(retired)
	retired = slices.Compact(retired)
	for _, generation := range retired {
		if slices.Contains(retained, generation) {
			continue
		}
		path := filepath.Join(dir, contentHash(generation)+".matrix")
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G703 -- explicit SQL/manifest generation key hashed beneath the owned cache namespace
			return err
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	previous := retained[0]
	if len(retained) == 2 {
		previous = retained[1]
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM cut_member WHERE generation NOT IN (?,?)", retained[0], previous); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM cut WHERE generation NOT IN (?,?)", retained[0], previous); err != nil {
		return err
	}
	return tx.Commit()
}
