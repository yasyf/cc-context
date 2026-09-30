// Package rmtree deletes a relocated payload one entry at a time through
// directory descriptors it holds open, so no removal resolves a path, follows a
// symlink, or crosses onto another volume. Every step first proves that the
// directory holding the payload is still the one its path names and that the
// payload still sits at its name inside it, and lets go of any held directory
// that has left its place, so a tree moved away is never deleted through a
// descriptor.
package rmtree

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

// MaxDepth is the deepest nesting a deletion descends, the payload root
// counting as one level. Every level keeps a descriptor open.
const MaxDepth = 1024

const (
	dirFlags  = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	readBatch = 256
)

var (
	// ErrMount reports a directory on another device than the payload root. A
	// nested mount is refused: never crossed, never unmounted.
	ErrMount = errors.New("rmtree: nested mount")
	// ErrStuck reports a directory that still lists entries after a full pass
	// over it removed none of them.
	ErrStuck = errors.New("rmtree: entries remain that cannot be removed")
	// ErrDepth reports a tree nested deeper than MaxDepth.
	ErrDepth = errors.New("rmtree: tree is nested too deep")
)

// Error is one failed operation of a deletion. Rel is the entry's path
// relative to the payload root, "." being the root itself.
type Error struct {
	Op, Rel string
	Err     error
}

func (e *Error) Error() string { return fmt.Sprintf("rmtree: %s %s: %v", e.Op, e.Rel, e.Err) }

func (e *Error) Unwrap() error { return e.Err }

// Deleter is the cleanup.Deleter that deletes through held descriptors.
type Deleter struct{}

var _ cleanup.Deleter = Deleter{}

// Open holds dir and the payload name inside it open without removing
// anything. name must be a single path component, and the final component of
// dir must not be a symlink. Every Step resolves dir again and fails with
// cleanup.ErrIdentity once it no longer names the directory held.
func (Deleter) Open(dir, name string, want cleanup.FileID) (cleanup.Deletion, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return nil, fmt.Errorf("rmtree: payload name %q is not a single path component: %w", name, fs.ErrInvalid)
	}
	base, err := unix.Open(dir, dirFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("rmtree: open %s: %w", dir, err)
	}
	var held unix.Stat_t
	if err := unix.Fstat(base, &held); err != nil {
		return nil, fmt.Errorf("rmtree: stat %s: %w", dir, errors.Join(err, unix.Close(base)))
	}
	fd, st, err := openDir(base, name)
	if err != nil {
		err = errors.Join(rootError(err), unix.Close(base))
		return nil, &Error{Op: "open", Rel: ".", Err: err}
	}
	if got := idOf(&st); got != want {
		err := fmt.Errorf("%w: found dev %d ino %d, want dev %d ino %d", cleanup.ErrIdentity, got.Dev, got.Ino, want.Dev, want.Ino)
		return nil, &Error{Op: "open", Rel: ".", Err: errors.Join(err, unix.Close(fd), unix.Close(base))}
	}
	return &deletion{
		dir:   dir,
		base:  holder{fd: base, spent: true},
		home:  idOf(&held),
		dev:   want.Dev,
		stack: []*frame{newFrame(fd, name, &st, 0)},
	}, nil
}

func rootError(err error) error {
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("%w: not a directory: %w", cleanup.ErrIdentity, err)
	}
	return err
}

func openDir(at int, name string) (int, unix.Stat_t, error) {
	var (
		fd  int
		st  unix.Stat_t
		err error
	)
	err = retryEINTR(func() error {
		fd, err = unix.Openat(at, name, dirFlags, 0)
		return err
	})
	if err != nil {
		return -1, st, err
	}
	if err := unix.Fstat(fd, &st); err != nil {
		return -1, st, errors.Join(err, unix.Close(fd))
	}
	return fd, st, nil
}

func retryEINTR(op func() error) error {
	for {
		if err := op(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func idOf(st *unix.Stat_t) cleanup.FileID {
	return cleanup.FileID{Dev: uint64(st.Dev), Ino: st.Ino} //nolint:gosec // mirrors cleanup.IDOf: Dev is int32 on darwin, and the widening is an identity key, never arithmetic
}

func isDir(st *unix.Stat_t) bool { return st.Mode&unix.S_IFMT == unix.S_IFDIR }

func refuseMount(root, child uint64, rel string) error {
	if child == root {
		return nil
	}
	return &Error{Op: "descend", Rel: rel, Err: fmt.Errorf("%w: device %d under a payload on device %d", ErrMount, child, root)}
}
