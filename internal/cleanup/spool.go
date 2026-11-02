package cleanup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/yasyf/daemonkit/durable"
)

// SpoolName is the folder, at the mount point of a volume other than the
// layout root's, that holds the job folders of trees on that volume: a tree
// moves into its job with a rename, which cannot cross volumes.
const SpoolName = ".ccx-cleanup"

// Spool is the parent of the job folders kept on the volume mounted at mount.
func Spool(mount string) string { return filepath.Join(mount, SpoolName, "jobs") }

// Area is the registered and payload paths of job id under the jobs folder
// jobs.
func Area(jobs, id string) (registered, payload string) {
	dir := filepath.Join(jobs, id)
	return filepath.Join(dir, registeredName), filepath.Join(dir, PayloadName)
}

// JobsFor is the parent of the job folder for a tree at tree on device dev:
// JobsDir on the layout root's volume, else the spool on tree's own volume
// when this user can keep one there. A tree whose volume can hold no spool
// gets JobsDir, and the volume check refuses its move.
func (l Layout) JobsFor(tree string, dev uint64) string {
	home, err := nearestDev(l.JobsDir())
	if err != nil || home == dev {
		return l.JobsDir()
	}
	mount, err := MountOf(tree)
	if err != nil || spoolUsable(Spool(mount)) != nil {
		return l.JobsDir()
	}
	return Spool(mount)
}

// MountOf is the mount point of the volume holding path: the highest
// ancestor of path's nearest existing directory that shares its device.
func MountOf(path string) (string, error) {
	path, dev, err := nearest(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	for {
		parent := filepath.Dir(path)
		if parent == path {
			return path, nil
		}
		info, err := os.Stat(parent)
		if err != nil {
			return "", fmt.Errorf("cleanup: inspect %s: %w", parent, err)
		}
		if IDOf(info).Dev != dev {
			return path, nil
		}
		path = parent
	}
}

func (l Layout) holds(jobs string) bool {
	return jobs == l.JobsDir() || filepath.Base(jobs) == "jobs" && filepath.Base(filepath.Dir(jobs)) == SpoolName
}

func ensureSpool(jobs string) error {
	if err := spoolUsable(jobs); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Dir(jobs), jobs} {
		if err := durable.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("cleanup: create %s: %w", dir, err)
		}
		if err := private(dir); err != nil {
			return err
		}
	}
	return nil
}

func spoolUsable(jobs string) error {
	root := filepath.Dir(jobs)
	switch _, err := os.Lstat(root); {
	case err == nil:
		if err := private(root); err != nil {
			return err
		}
		if _, err := os.Lstat(jobs); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return private(jobs)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("cleanup: inspect %s: %w", root, err)
	}
	mount := filepath.Dir(root)
	info, err := os.Lstat(mount)
	if err != nil {
		return fmt.Errorf("cleanup: inspect %s: %w", mount, err)
	}
	st := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.IsDir():
		return fmt.Errorf("cleanup: %s is not a directory", mount)
	case int64(st.Uid) == int64(os.Getuid()):
		return nil
	case info.Mode()&fs.ModeSticky != 0 && info.Mode().Perm()&0o002 != 0:
		return nil
	}
	return fmt.Errorf("cleanup: %s belongs to uid %d, so this user cannot keep a cleanup spool in it", mount, st.Uid)
}

func private(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("cleanup: inspect %s: %w", dir, err)
	}
	st := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.IsDir():
		return fmt.Errorf("cleanup: %s is not a directory", dir)
	case int64(st.Uid) != int64(os.Getuid()):
		return fmt.Errorf("cleanup: %s belongs to uid %d, not this user", dir, st.Uid)
	case info.Mode().Perm() != 0o700:
		return fmt.Errorf("cleanup: %s is mode %o, not private", dir, info.Mode().Perm())
	}
	return nil
}

func nearestDev(path string) (uint64, error) {
	_, dev, err := nearest(path)
	return dev, err
}

func nearest(path string) (string, uint64, error) {
	for {
		info, err := os.Stat(path)
		if err == nil {
			return path, IDOf(info).Dev, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", 0, fmt.Errorf("cleanup: inspect %s: %w", path, err)
		}
		path = filepath.Dir(path)
	}
}
