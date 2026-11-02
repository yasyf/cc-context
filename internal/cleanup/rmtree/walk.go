package rmtree

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

var errSwapped = errors.New("rmtree: entry changed between stat and use")

type holder struct {
	fd     int
	mode   uint32
	sealed bool
	once   sync.Once
	chmod  error
}

func (h *holder) do(op func() error) error {
	err := retryEINTR(op)
	if !errors.Is(err, unix.EACCES) || h.sealed {
		return err
	}
	h.once.Do(func() { h.chmod = unix.Fchmod(h.fd, h.mode|0o700) })
	if h.chmod != nil {
		return fmt.Errorf("%w (fchmod: %w)", err, h.chmod)
	}
	return retryEINTR(op)
}

type node struct {
	holder
	dir    *os.File
	name   string
	id     cleanup.FileID
	parent *node
	depth  int

	busy     int
	passes   int
	progress uint64
	mark     uint64
	empty    bool
	refused  bool
	missed   map[string]struct{}
	closed   bool
}

type op uint8

const (
	opList op = iota
	opUnlink
	opRemove
)

type task struct {
	op   op
	node *node
	name string
}

func (t task) removes() bool { return t.op != opList }

type deletion struct {
	dir  string
	base holder
	home cleanup.FileID
	dev  uint64
	root *node

	mu       sync.Mutex
	wake     *sync.Cond
	queue    []task
	live     map[*node]struct{}
	tokens   int
	removed  int
	running  int
	deadline time.Time
	err      error
	done     bool
	closed   bool
}

func newDeletion(dir string, base int, home cleanup.FileID, dev uint64, root *node) *deletion {
	d := &deletion{dir: dir, base: holder{fd: base, sealed: true}, home: home, dev: dev, root: root, live: map[*node]struct{}{root: {}}}
	d.wake = sync.NewCond(&d.mu)
	d.push(task{op: opList, node: root})
	return d
}

func newNode(fd int, name string, st *unix.Stat_t, parent *node) *node {
	depth := 1
	if parent != nil {
		depth = parent.depth + 1
	}
	return &node{
		holder: holder{fd: fd, mode: uint32(st.Mode) & 0o7777},
		dir:    os.NewFile(uintptr(fd), name), //nolint:gosec // an open descriptor is never negative
		name:   name,
		id:     idOf(st),
		parent: parent,
		depth:  depth,
	}
}

func (d *deletion) Step(limit int, budget time.Duration, workers int) (int, bool, error) {
	if d.closed {
		return 0, false, &Error{Op: "step", Rel: ".", Err: fs.ErrClosed}
	}
	if err := d.bind(); err != nil {
		return 0, false, errors.Join(err, d.Close())
	}
	removed, err := d.run(limit, budget, workers)
	switch {
	case err != nil:
		return removed, false, errors.Join(err, d.Close())
	case d.done:
		return removed, true, d.Close()
	}
	return removed, false, nil
}

func (d *deletion) run(limit int, budget time.Duration, workers int) (int, error) {
	d.tokens, d.removed, d.deadline = limit, 0, time.Now().Add(budget)
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Go(d.work)
	}
	wg.Wait()
	return d.removed, d.err
}

func (d *deletion) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	errs := make([]error, 0, len(d.live)+1)
	for n := range d.live {
		errs = append(errs, n.dir.Close())
	}
	d.live, d.queue = nil, nil
	errs = append(errs, unix.Close(d.base.fd))
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("rmtree: close: %w", err)
	}
	return nil
}

func (d *deletion) work() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		t, ok := d.take()
		if !ok {
			return
		}
		d.running++
		removed, err := d.perform(t)
		d.running--
		d.removed += removed
		if t.removes() && removed == 0 {
			d.tokens++
		}
		if err != nil && d.err == nil {
			d.err = err
		}
		d.wake.Broadcast()
	}
}

func (d *deletion) take() (task, bool) {
	for {
		switch {
		case d.err != nil, d.done:
			return task{}, false
		case len(d.queue) == 0 && d.running == 0:
			return task{}, false
		case len(d.queue) == 0:
			d.wake.Wait()
			continue
		}
		t := d.queue[len(d.queue)-1]
		if t.removes() && d.tokens <= 0 || d.removed > 0 && !time.Now().Before(d.deadline) {
			return task{}, false
		}
		d.queue = d.queue[:len(d.queue)-1]
		if t.removes() {
			d.tokens--
		}
		return t, true
	}
}

func (d *deletion) push(t task) {
	d.queue = append(d.queue, t)
	if t.op != opRemove {
		t.node.busy++
	}
}

func (d *deletion) perform(t task) (int, error) {
	switch t.op {
	case opList:
		err := d.list(t.node)
		d.release(t.node)
		return 0, err
	case opUnlink:
		removed, err := d.unlink(t.node, t.name)
		d.release(t.node)
		return removed, err
	}
	return d.finish(t.node)
}

func (d *deletion) release(n *node) {
	n.busy--
	d.settle(n)
}

func (d *deletion) settle(n *node) {
	switch {
	case n.closed || n.busy > 0:
	case n.empty:
		d.push(task{op: opRemove, node: n})
	default:
		d.push(task{op: opList, node: n})
	}
}

func (d *deletion) unlocked(fn func()) {
	d.mu.Unlock()
	defer d.mu.Lock()
	fn()
}

func (d *deletion) bind() error {
	if err := d.bindBase(); err != nil {
		return err
	}
	got, err := d.occupant(d.root)
	switch {
	case errors.Is(err, unix.ENOENT):
		return d.rootGone("bind")
	case err != nil:
		return &Error{Op: "bind", Rel: ".", Err: fmt.Errorf("%w: the payload name cannot be read: %w", cleanup.ErrIdentity, err)}
	case got != d.root.id:
		return d.rootReplaced("bind", got)
	}
	held := slices.SortedFunc(maps.Keys(d.live), func(a, b *node) int { return cmp.Compare(a.depth, b.depth) })
	for _, n := range held {
		if n == d.root || n.closed {
			continue
		}
		got, err := d.occupant(n)
		switch {
		case errors.Is(err, unix.ENOENT):
			d.miss(n.parent, n.name)
		case err != nil:
			return &Error{Op: "bind", Rel: relOf(n, ""), Err: err}
		case got != n.id:
			n.parent.progress++
		default:
			continue
		}
		if err := d.drop(n); err != nil {
			return err
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

func (d *deletion) above(n *node) *holder {
	if n.parent == nil {
		return &d.base
	}
	return &n.parent.holder
}

func (d *deletion) occupant(n *node) (cleanup.FileID, error) {
	up := d.above(n)
	var st unix.Stat_t
	err := up.do(func() error { return unix.Fstatat(up.fd, n.name, &st, unix.AT_SYMLINK_NOFOLLOW) })
	return idOf(&st), err
}

func (d *deletion) miss(in *node, name string) {
	if _, again := in.missed[name]; again {
		return
	}
	if in.missed == nil {
		in.missed = map[string]struct{}{}
	}
	in.missed[name] = struct{}{}
	in.progress++
}

func (d *deletion) drop(n *node) error {
	rel := relOf(n, "")
	var errs []error
	for held := range d.live {
		if held.within(n) {
			held.closed = true
			delete(d.live, held)
			errs = append(errs, held.dir.Close())
		}
	}
	d.queue = slices.DeleteFunc(d.queue, func(t task) bool { return t.node.closed })
	n.parent.busy--
	d.settle(n.parent)
	if err := errors.Join(errs...); err != nil {
		return &Error{Op: "close", Rel: rel, Err: err}
	}
	return nil
}

func (n *node) within(ancestor *node) bool {
	for at := n; at != nil; at = at.parent {
		if at == ancestor {
			return true
		}
	}
	return false
}

func (d *deletion) rootGone(op string) error {
	want := d.root.id
	return &Error{Op: op, Rel: ".", Err: fmt.Errorf("%w: the payload name is empty, yet this deletion never removed dev %d ino %d: another process renamed or removed it", cleanup.ErrIdentity, want.Dev, want.Ino)}
}

func (d *deletion) rootReplaced(op string, got cleanup.FileID) error {
	want := d.root.id
	return &Error{Op: op, Rel: ".", Err: fmt.Errorf("%w: the payload name now holds dev %d ino %d, not dev %d ino %d", cleanup.ErrIdentity, got.Dev, got.Ino, want.Dev, want.Ino)}
}

func (d *deletion) list(n *node) error {
	stalled := n.passes > 0 && n.progress == n.mark
	n.mark, n.passes, n.empty = n.progress, n.passes+1, true
	for {
		var (
			names []string
			err   error
		)
		d.unlocked(func() { names, err = n.dir.Readdirnames(readBatch) })
		if len(names) > 0 {
			if stalled {
				return fail("read", n, "", ErrStuck)
			}
			n.empty, n.refused = false, false
			for _, name := range names {
				d.push(task{op: opUnlink, node: n, name: name})
			}
			d.wake.Broadcast()
		}
		switch {
		case errors.Is(err, io.EOF):
			return d.rewind(n)
		case errors.Is(err, unix.ENOENT):
			// Linux fails getdents on a removed directory with ENOENT; Darwin lists it as empty.
			return d.unlisted(n, err)
		case err != nil:
			return fail("read", n, "", err)
		}
	}
}

func (d *deletion) rewind(n *node) error {
	var err error
	d.unlocked(func() { _, err = n.dir.Seek(0, io.SeekStart) })
	if err != nil {
		return fail("rewind", n, "", err)
	}
	return nil
}

func (d *deletion) unlisted(n *node, cause error) error {
	var (
		got cleanup.FileID
		err error
	)
	d.unlocked(func() { got, err = d.occupant(n) })
	if err == nil && got == n.id {
		return fail("read", n, "", cause)
	}
	n.empty = true
	return nil
}

func (d *deletion) unlink(n *node, name string) (int, error) {
	for range 2 {
		var err error
		d.unlocked(func() { err = n.do(func() error { return unix.Unlinkat(n.fd, name, 0) }) })
		switch {
		case err == nil:
			n.progress++
			return 1, nil
		case errors.Is(err, unix.ENOENT):
			d.miss(n, name)
			return 0, nil
		case !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EISDIR):
			return 0, fail("unlink", n, name, err)
		}
		var (
			st      unix.Stat_t
			statErr error
		)
		d.unlocked(func() { statErr = unix.Fstatat(n.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) })
		switch {
		case errors.Is(statErr, unix.ENOENT):
			d.miss(n, name)
			return 0, nil
		case statErr != nil:
			return 0, fail("lstat", n, name, statErr)
		case !isDir(&st):
			return 0, fail("unlink", n, name, err)
		}
		if err := d.descend(n, name, &st); !errors.Is(err, errSwapped) {
			return 0, err
		}
	}
	return 0, nil
}

func (d *deletion) descend(n *node, name string, listed *unix.Stat_t) error {
	if err := refuseMount(d.dev, idOf(listed).Dev, relOf(n, name)); err != nil {
		return err
	}
	if n.depth == MaxDepth {
		return fail("descend", n, name, ErrDepth)
	}
	var (
		fd  int
		st  unix.Stat_t
		err error
		op  = "open"
	)
	d.unlocked(func() {
		fd, st, err = openDir(n.fd, name)
		if errors.Is(err, unix.EACCES) {
			op = "chmod"
			err = retryEINTR(func() error { return unix.Fchmodat(n.fd, name, 0o700, unix.AT_SYMLINK_NOFOLLOW) })
			if err == nil {
				op = "open"
				fd, st, err = openDir(n.fd, name)
			}
		}
	})
	switch {
	case errors.Is(err, unix.ENOENT):
		d.miss(n, name)
		return nil
	case errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ELOOP):
		return errSwapped
	case err != nil:
		return fail(op, n, name, err)
	}
	if idOf(&st) != idOf(listed) {
		if err := unix.Close(fd); err != nil {
			return fail("close", n, name, err)
		}
		return errSwapped
	}
	child := newNode(fd, name, &st, n)
	d.live[child] = struct{}{}
	n.busy++
	d.push(task{op: opList, node: child})
	return nil
}

func (d *deletion) finish(n *node) (int, error) {
	var (
		got cleanup.FileID
		err error
	)
	d.unlocked(func() { got, err = d.occupant(n) })
	switch {
	case errors.Is(err, unix.ENOENT):
		return 0, d.vanished(n)
	case err != nil:
		return 0, fail("lstat", n, "", err)
	case got != n.id && n == d.root:
		return 0, d.rootReplaced("rmdir", got)
	case got != n.id:
		n.parent.progress++
		return 0, d.drop(n)
	}
	up := d.above(n)
	d.unlocked(func() { err = up.do(func() error { return unix.Unlinkat(up.fd, n.name, unix.AT_REMOVEDIR) }) })
	switch {
	case err == nil:
		return 1, d.removedDir(n)
	case errors.Is(err, unix.ENOENT):
		return 0, d.vanished(n)
	case errors.Is(err, unix.ENOTEMPTY), errors.Is(err, unix.EEXIST):
		if n.refused {
			return 0, fail("rmdir", n, "", fmt.Errorf("%w: %w", ErrStuck, err))
		}
		n.refused, n.empty, n.passes = true, false, 0
		d.push(task{op: opList, node: n})
		return 0, nil
	}
	return 0, fail("rmdir", n, "", err)
}

func (d *deletion) vanished(n *node) error {
	if n == d.root {
		return d.rootGone("rmdir")
	}
	d.miss(n.parent, n.name)
	return d.drop(n)
}

func (d *deletion) removedDir(n *node) error {
	n.closed = true
	delete(d.live, n)
	if err := n.dir.Close(); err != nil {
		return &Error{Op: "close", Rel: relOf(n, ""), Err: err}
	}
	if n == d.root {
		d.done = true
		if err := unix.Fsync(d.base.fd); err != nil {
			return &Error{Op: "fsync", Rel: "..", Err: err}
		}
		return nil
	}
	n.parent.progress++
	n.parent.busy--
	d.settle(n.parent)
	return nil
}

func fail(op string, n *node, name string, err error) error {
	return &Error{Op: op, Rel: relOf(n, name), Err: err}
}

func relOf(n *node, name string) string {
	var parts []string
	if name != "" {
		parts = append(parts, name)
	}
	for at := n; at != nil && at.parent != nil; at = at.parent {
		parts = append(parts, at.name)
	}
	if len(parts) == 0 {
		return "."
	}
	slices.Reverse(parts)
	return strings.Join(parts, "/")
}
