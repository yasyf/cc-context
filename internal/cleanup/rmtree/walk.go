package rmtree

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

var errSwapped = errors.New("rmtree: entry changed between stat and use")

type holder struct {
	fd    int
	mode  uint32
	spent bool
}

type frame struct {
	holder
	dir     *os.File
	name    string
	id      cleanup.FileID
	pending []string
	missed  map[string]struct{}
	fresh   bool
	refused bool
	mark    uint64
}

type deletion struct {
	dir      string
	base     holder
	home     cleanup.FileID
	dev      uint64
	stack    []*frame
	progress uint64
	closed   bool
}

func newFrame(fd int, name string, st *unix.Stat_t, progress uint64) *frame {
	return &frame{
		holder: holder{fd: fd, mode: uint32(st.Mode) & 0o7777},
		dir:    os.NewFile(uintptr(fd), name), //nolint:gosec // an open descriptor is never negative
		name:   name,
		id:     idOf(st),
		fresh:  true,
		mark:   progress,
	}
}

func (h *holder) do(op func() error) error {
	err := retryEINTR(op)
	if !errors.Is(err, unix.EACCES) || h.spent {
		return err
	}
	h.spent = true
	if cerr := unix.Fchmod(h.fd, h.mode|0o700); cerr != nil {
		return fmt.Errorf("%w (fchmod: %w)", err, cerr)
	}
	return retryEINTR(op)
}

func (d *deletion) Step(limit int, budget time.Duration) (int, bool, error) {
	if d.closed {
		return 0, false, &Error{Op: "step", Rel: ".", Err: fs.ErrClosed}
	}
	start := time.Now()
	if err := d.bind(); err != nil {
		return 0, false, errors.Join(err, d.Close())
	}
	removed := 0
	for removed < limit {
		n, done, err := d.advance()
		removed += n
		if err != nil {
			return removed, false, errors.Join(err, d.Close())
		}
		if done {
			return removed, true, d.Close()
		}
		if time.Since(start) >= budget {
			break
		}
	}
	return removed, false, nil
}

func (d *deletion) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	errs := make([]error, 0, len(d.stack)+1)
	for _, f := range d.stack {
		errs = append(errs, f.dir.Close())
	}
	d.stack = nil
	errs = append(errs, unix.Close(d.base.fd))
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("rmtree: close: %w", err)
	}
	return nil
}

func (d *deletion) bind() error {
	if err := d.bindBase(); err != nil {
		return err
	}
	got, err := d.occupant(0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return d.rootGone("bind")
	case err != nil:
		return &Error{Op: "bind", Rel: ".", Err: fmt.Errorf("%w: the payload name cannot be read: %w", cleanup.ErrIdentity, err)}
	case got != d.stack[0].id:
		return d.rootReplaced("bind", got)
	}
	for level := 1; level < len(d.stack); level++ {
		got, err := d.occupant(level)
		switch {
		case errors.Is(err, unix.ENOENT):
			d.miss(d.stack[level-1], d.stack[level].name)
			return d.drop(level)
		case err != nil:
			return &Error{Op: "bind", Rel: relOf(d.stack[:level+1], ""), Err: err}
		case got != d.stack[level].id:
			d.progress++
			return d.drop(level)
		}
	}
	return nil
}

func (d *deletion) bindBase() error {
	var st unix.Stat_t
	err := retryEINTR(func() error { return unix.Lstat(d.dir, &st) })
	got := idOf(&st)
	switch {
	case errors.Is(err, unix.ENOENT):
		return &Error{Op: "bind", Rel: "..", Err: fmt.Errorf("%w: nothing is at %s, yet the directory holding the payload is still held open: another process renamed or removed it", cleanup.ErrIdentity, d.dir)}
	case err != nil:
		return &Error{Op: "bind", Rel: "..", Err: fmt.Errorf("%w: %s cannot be read: %w", cleanup.ErrIdentity, d.dir, err)}
	case got != d.home:
		return &Error{Op: "bind", Rel: "..", Err: fmt.Errorf("%w: %s now holds dev %d ino %d, not dev %d ino %d", cleanup.ErrIdentity, d.dir, got.Dev, got.Ino, d.home.Dev, d.home.Ino)}
	}
	return nil
}

func (d *deletion) miss(in *frame, name string) {
	if _, again := in.missed[name]; again {
		return
	}
	if in.missed == nil {
		in.missed = map[string]struct{}{}
	}
	in.missed[name] = struct{}{}
	d.progress++
}

func (d *deletion) above(level int) *holder {
	if level == 0 {
		return &d.base
	}
	return &d.stack[level-1].holder
}

func (d *deletion) occupant(level int) (cleanup.FileID, error) {
	up, name := d.above(level), d.stack[level].name
	var st unix.Stat_t
	err := up.do(func() error { return unix.Fstatat(up.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) })
	return idOf(&st), err
}

func (d *deletion) drop(level int) error {
	rel := relOf(d.stack[:level+1], "")
	dropped := d.stack[level:]
	d.stack = d.stack[:level]
	errs := make([]error, 0, len(dropped))
	for _, f := range dropped {
		errs = append(errs, f.dir.Close())
	}
	if err := errors.Join(errs...); err != nil {
		return &Error{Op: "close", Rel: rel, Err: err}
	}
	return nil
}

func (d *deletion) rootGone(op string) error {
	want := d.stack[0].id
	return &Error{Op: op, Rel: ".", Err: fmt.Errorf("%w: the payload name is empty, yet this deletion never removed dev %d ino %d: another process renamed or removed it", cleanup.ErrIdentity, want.Dev, want.Ino)}
}

func (d *deletion) rootReplaced(op string, got cleanup.FileID) error {
	want := d.stack[0].id
	return &Error{Op: op, Rel: ".", Err: fmt.Errorf("%w: the payload name now holds dev %d ino %d, not dev %d ino %d", cleanup.ErrIdentity, got.Dev, got.Ino, want.Dev, want.Ino)}
}

func (d *deletion) advance() (int, bool, error) {
	top := d.stack[len(d.stack)-1]
	name, ok, err := d.next(top)
	switch {
	case errors.Is(err, unix.ENOENT):
		// Linux fails getdents on a removed directory with ENOENT; Darwin lists it as empty.
		return d.unlisted(top, err)
	case err != nil:
		return 0, false, err
	case !ok:
		return d.finish(top)
	}
	n, err := d.visit(top, name)
	if errors.Is(err, errSwapped) {
		n, err = d.visit(top, name)
	}
	if errors.Is(err, errSwapped) {
		return 0, false, nil
	}
	return n, false, err
}

func (d *deletion) next(f *frame) (string, bool, error) {
	stalled := false
	for len(f.pending) == 0 {
		names, err := f.dir.Readdirnames(readBatch)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false, d.fail("read", "", err)
		}
		if len(names) > 0 && stalled {
			return "", false, d.fail("read", "", ErrStuck)
		}
		if len(names) > 0 {
			f.pending, f.fresh, f.refused = names, false, false
			break
		}
		if f.fresh {
			return "", false, nil
		}
		stalled = d.progress == f.mark
		if err := d.rewind(f); err != nil {
			return "", false, err
		}
	}
	name := f.pending[0]
	f.pending = f.pending[1:]
	return name, true, nil
}

func (d *deletion) rewind(f *frame) error {
	if _, err := f.dir.Seek(0, io.SeekStart); err != nil {
		return d.fail("rewind", "", err)
	}
	f.pending, f.fresh, f.mark = nil, true, d.progress
	return nil
}

func (d *deletion) visit(top *frame, name string) (int, error) {
	var st unix.Stat_t
	err := top.do(func() error { return unix.Fstatat(top.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) })
	switch {
	case errors.Is(err, unix.ENOENT):
		d.miss(top, name)
		return 0, nil
	case err != nil:
		return 0, d.fail("lstat", name, err)
	case isDir(&st):
		return 0, d.descend(top, name, &st)
	}
	return d.unlink(top, name)
}

func (d *deletion) unlink(top *frame, name string) (int, error) {
	err := top.do(func() error { return unix.Unlinkat(top.fd, name, 0) })
	switch {
	case err == nil:
		d.progress++
		return 1, nil
	case errors.Is(err, unix.ENOENT):
		d.miss(top, name)
		return 0, nil
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EISDIR):
		var st unix.Stat_t
		if unix.Fstatat(top.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && isDir(&st) {
			return 0, errSwapped
		}
	}
	return 0, d.fail("unlink", name, err)
}

func (d *deletion) descend(top *frame, name string, listed *unix.Stat_t) error {
	if err := refuseMount(d.dev, idOf(listed).Dev, d.rel(name)); err != nil {
		return err
	}
	if len(d.stack) == MaxDepth {
		return d.fail("descend", name, ErrDepth)
	}
	op := "open"
	fd, st, err := openDir(top.fd, name)
	if errors.Is(err, unix.EACCES) {
		op = "chmod"
		err = retryEINTR(func() error { return unix.Fchmodat(top.fd, name, 0o700, unix.AT_SYMLINK_NOFOLLOW) })
		if err == nil {
			op = "open"
			fd, st, err = openDir(top.fd, name)
		}
	}
	switch {
	case errors.Is(err, unix.ENOENT):
		d.miss(top, name)
		return nil
	case errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ELOOP):
		return errSwapped
	case err != nil:
		return d.fail(op, name, err)
	}
	if idOf(&st) != idOf(listed) {
		if err := unix.Close(fd); err != nil {
			return d.fail("close", name, err)
		}
		return errSwapped
	}
	d.stack = append(d.stack, newFrame(fd, name, &st, d.progress))
	return nil
}

func (d *deletion) unlisted(top *frame, cause error) (int, bool, error) {
	got, err := d.occupant(len(d.stack) - 1)
	if err == nil && got == top.id {
		return 0, false, cause
	}
	return d.finish(top)
}

func (d *deletion) finish(top *frame) (int, bool, error) {
	level := len(d.stack) - 1
	got, err := d.occupant(level)
	switch {
	case errors.Is(err, unix.ENOENT):
		return d.vanished(top)
	case err != nil:
		return 0, false, d.fail("lstat", "", err)
	case got != top.id && level == 0:
		return 0, false, d.rootReplaced("rmdir", got)
	case got != top.id:
		d.progress++
		return d.pop(top, 0)
	}
	up := d.above(level)
	err = up.do(func() error { return unix.Unlinkat(up.fd, top.name, unix.AT_REMOVEDIR) })
	switch {
	case err == nil:
		d.progress++
		return d.pop(top, 1)
	case errors.Is(err, unix.ENOENT):
		return d.vanished(top)
	case errors.Is(err, unix.ENOTEMPTY), errors.Is(err, unix.EEXIST):
		if top.refused {
			return 0, false, d.fail("rmdir", "", fmt.Errorf("%w: %w", ErrStuck, err))
		}
		if err := d.rewind(top); err != nil {
			return 0, false, err
		}
		top.refused = true
		return 0, false, nil
	}
	return 0, false, d.fail("rmdir", "", err)
}

func (d *deletion) vanished(top *frame) (int, bool, error) {
	if len(d.stack) == 1 {
		return 0, false, d.rootGone("rmdir")
	}
	d.miss(d.stack[len(d.stack)-2], top.name)
	return d.pop(top, 0)
}

func (d *deletion) pop(top *frame, removed int) (int, bool, error) {
	rel := d.rel("")
	d.stack = d.stack[:len(d.stack)-1]
	if err := top.dir.Close(); err != nil {
		return removed, false, &Error{Op: "close", Rel: rel, Err: err}
	}
	if len(d.stack) > 0 {
		return removed, false, nil
	}
	if err := unix.Fsync(d.base.fd); err != nil {
		return removed, false, &Error{Op: "fsync", Rel: "..", Err: err}
	}
	return removed, true, nil
}

func (d *deletion) fail(op, name string, err error) error {
	return &Error{Op: op, Rel: d.rel(name), Err: err}
}

func (d *deletion) rel(name string) string { return relOf(d.stack, name) }

func relOf(stack []*frame, name string) string {
	parts := make([]string, 0, len(stack))
	for _, f := range stack[1:] {
		parts = append(parts, f.name)
	}
	if name != "" {
		parts = append(parts, name)
	}
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}
