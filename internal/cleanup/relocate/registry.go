package relocate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/yasyf/cc-context/internal/cleanup"
)

type entry struct {
	admin string
	names string
}

func (e entry) claim(job *cleanup.Job) string {
	for _, payload := range spellings(job.Payload) {
		if e.names == filepath.Join(payload, ".git") {
			return "payload is registered as " + e.admin
		}
	}
	return fmt.Sprintf("%s registers %s", e.admin, e.names)
}

func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func enrolled(repo string) ([]entry, error) {
	registry := filepath.Join(repo, "worktrees")
	admins, err := os.ReadDir(registry)
	if errors.Is(err, fs.ErrNotExist) {
		if _, err := os.Stat(repo); err != nil {
			return nil, fmt.Errorf("read worktree registry of %s: %w", repo, err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read worktree registry of %s: %w", repo, err)
	}
	var entries []entry
	for _, admin := range admins {
		adminDir := filepath.Join(registry, admin.Name())
		content, err := readLinkage(filepath.Join(adminDir, "gitdir"))
		if gone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read worktree registry of %s: %w", repo, err)
		}
		named := strings.TrimRight(content, " \t\r\n")
		if named == "" {
			continue
		}
		if !filepath.IsAbs(named) {
			named = filepath.Join(adminDir, named)
		}
		entries = append(entries, entry{admin: adminDir, names: filepath.Clean(named)})
	}
	return entries, nil
}

func registered(repo string, dirs ...string) ([]entry, error) {
	entries, err := enrolled(repo)
	if err != nil {
		return nil, err
	}
	return reaching(entries, dirs...)
}

func reaching(entries []entry, dirs ...string) ([]entry, error) {
	var (
		spelled []string
		ids     []cleanup.FileID
	)
	for _, dir := range dirs {
		spelled = append(spelled, spellings(dir)...)
		info, err := os.Stat(dir)
		if gone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", dir, err)
		}
		ids = append(ids, cleanup.IDOf(info))
	}
	var found []entry
	for _, e := range entries {
		reached, err := reaches(e.names, spelled, ids)
		if err != nil {
			return nil, fmt.Errorf("resolve %s, which %s registers: %w", e.names, e.admin, err)
		}
		if reached {
			found = append(found, e)
		}
	}
	return found, nil
}

func reaches(path string, spelled []string, ids []cleanup.FileID) (bool, error) {
	for _, dir := range spelled {
		if within(path, dir) {
			return true, nil
		}
	}
	for prefix := path; ; prefix = filepath.Dir(prefix) {
		resolved, err := filepath.EvalSymlinks(prefix)
		if gone(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		below, err := descends(resolved, ids)
		if err != nil || below || resolved == prefix {
			return below, err
		}
	}
}

func descends(path string, ids []cleanup.FileID) (bool, error) {
	for {
		info, err := os.Stat(path)
		if err != nil {
			return false, err
		}
		if slices.Contains(ids, cleanup.IDOf(info)) {
			return true, nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false, nil
		}
		path = parent
	}
}

func origins(job *cleanup.Job) []string {
	named, ok := linkTarget(job.Links.AdminGitdir, job.AdminDir)
	if !ok || filepath.Dir(named) == job.Original {
		return []string{job.Original}
	}
	return []string{job.Original, filepath.Dir(named)}
}

func carried(job *cleanup.Job, entries []entry, tree string) (string, error) {
	for _, e := range entries {
		if e.admin == job.AdminDir {
			continue
		}
		for _, origin := range origins(job) {
			if e.names == filepath.Join(origin, ".git") || !inside(e.names, origin) {
				continue
			}
			twin := tree + strings.TrimPrefix(e.names, origin)
			_, err := os.Lstat(twin)
			if gone(err) {
				continue
			}
			if err != nil {
				return "", fmt.Errorf("inspect %s: %w", twin, err)
			}
			return fmt.Sprintf("%s registers %s, which moved to %s with the tree", e.admin, e.names, twin), nil
		}
	}
	return "", nil
}

func intruder(job *cleanup.Job, expected cleanup.Links) (string, error) {
	entries, err := enrolled(job.Repo)
	if err != nil {
		return "", err
	}
	claims, err := reaching(entries, filepath.Dir(job.Registered))
	if err != nil {
		return "", err
	}
	for _, e := range claims {
		if e != parkedEntry(job, expected) {
			return e.claim(job), nil
		}
	}
	return carried(job, entries, job.Registered)
}

func claimed(job *cleanup.Job) (string, error) {
	dirs := []string{filepath.Dir(job.Payload)}
	if job.Adopted {
		dirs = append(dirs, job.Source)
	}
	entries, err := enrolled(job.Repo)
	if err != nil {
		return "", err
	}
	claims, err := reaching(entries, dirs...)
	if err != nil {
		return "", err
	}
	if len(claims) > 0 {
		return claims[0].claim(job), nil
	}
	stowaway, err := carried(job, entries, job.Payload)
	if err != nil || stowaway != "" {
		return stowaway, err
	}
	adminDir, err := liveLink(job.Payload)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if adminDir != "" {
		return "payload is registered as " + adminDir, nil
	}
	return "", nil
}
