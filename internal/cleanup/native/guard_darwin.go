package native

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

// Guard reports the live processes of the caller's uid that hold worktree, as
// a *cleanup.ActiveError carrying the first evidence found for each: a working
// directory at or below the tree, the process's own or one a thread of it took
// for itself; then an open file or directory there; then an argument naming an
// absolute path there, taken whole or after its last "=". Directories and open
// files are placed only by a path proven to name them, so letter case, a
// symlinked parent, and a path longer than the kernel's fixed buffer cannot
// hide a holder. Arguments are compared as written, against both the kernel's
// spelling of the tree and the one the caller passed.
//
// Six things are discounted and nothing else, ancestry otherwise included: the
// calling process's own descriptors and arguments, though not its working
// directory; its direct children; the arguments, alone, of the requester ctx
// names; the trailing arguments, alone, of each launcher above the invoker,
// the requester or else the calling process, where they repeat the invoker's
// own past its program: the unbroken chain of parents, such as timeout, whose
// arguments end that way, though not the arguments before that tail; the
// descriptors, alone, of each retiring watcher ctx names, matched on both its
// pid and the second the kernel started it, and only while that pid still
// names that process once the rest of it has been read; and each descriptor of
// an approved Apple service that the kernel refuses to place, with EPERM or
// EACCES: refused the descriptor's path and then its vnode alone, or given a
// path not proven to name the file and then refused the path of its inode.
//
// An approved service is one of the ten executables in approvedServices,
// told by the kernel's record and never by name: proc_pidpath gives exactly a
// listed path, that path is on the read-only root snapshot mounted at "/",
// csops reports a valid platform binary that is not being debugged, the uid
// and real uid are the caller's, the parent is launchd, and there is no
// controlling terminal. The check runs before the first descriptor is skipped
// and again after the last, and the process's start time must not change
// between them. Where a skipped descriptor's file sits is unknown; it is
// assumed to be outside every worktree.
//
// A worktree that is itself a symlink, or a process that has not exited and
// cannot be read in full, is an error rather than a pass. That covers a
// descriptor the kernel refuses to describe in any other process and a
// directory or file whose path cannot be proven: Guard makes one pass and
// retries nothing. A process caught replacing its image, whose arguments the
// kernel cannot produce, does not stop the pass: when the rest of the scan
// finds no holder, Guard fails with [cleanup.ErrUnprobed] naming it. It does
// not see a file that is only memory-mapped, a process of another uid, an
// argument given as a relative path, or a process that exits before the scan
// reaches it. A removed directory or file sits in
// no tree and holds nothing.
func Guard(ctx context.Context, worktree string) error {
	lib, err := loadLibSystem()
	if err != nil {
		return err
	}
	scan, err := newScan(ctx, lib, worktree)
	if err != nil {
		return err
	}
	pids, err := lib.listPIDs(procUIDOnly, os.Getuid())
	if err != nil {
		return err
	}
	var holders []cleanup.Holder
	var unread []string
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return err
		}
		holder, held, err := scan.inspect(int(pid))
		if errors.Is(err, errUnreadArguments) {
			unread = append(unread, err.Error())
			continue
		}
		if err != nil {
			return err
		}
		if held {
			holders = append(holders, holder)
		}
	}
	return verdict(worktree, holders, unread)
}

var errUnreadArguments = errors.New("read its arguments")

func verdict(worktree string, holders []cleanup.Holder, unread []string) error {
	if len(holders) > 0 || len(unread) == 0 {
		return active(worktree, holders)
	}
	return fmt.Errorf("%w: %s", cleanup.ErrUnprobed, strings.Join(unread, "; "))
}

func newScan(ctx context.Context, lib *libSystem, worktree string) (*scan, error) {
	root, err := kernelPath(lib, worktree)
	if err != nil {
		return nil, err
	}
	requester, named := cleanup.RequesterFrom(ctx)
	invoker := os.Getpid()
	if named {
		invoker = requester
	}
	tail, chain := launchers(lib, invoker)
	return &scan{
		lib: lib, root: root, given: filepath.Clean(worktree), self: os.Getpid(),
		requester: requester, named: named, retiring: cleanup.RetiringFrom(ctx),
		tail: tail, launchers: chain,
	}, nil
}

// launchers is pid's own arguments past its program, and the unbroken chain
// of parents above pid each started with them as its trailing ones: a wrapper
// such as timeout that runs the invoker and waits on it. The chain ends at the
// first parent whose arguments cannot be read or do not end that way.
func launchers(lib *libSystem, pid int) ([]string, []cleanup.ProcessID) {
	own, err := processArguments(pid)
	if err != nil || len(own) < 2 {
		return nil, nil
	}
	tail := own[1:]
	var chain []cleanup.ProcessID
	for {
		bsd, err := pidInfo[procBSDInfo](lib, pid, flavorBSDInfo)
		if err != nil || bsd.ppid <= 1 {
			return tail, chain
		}
		parent := int(bsd.ppid)
		above, err := pidInfo[procBSDInfo](lib, parent, flavorBSDInfo)
		if err != nil {
			return tail, chain
		}
		args, err := processArguments(parent)
		if err != nil || !endsWith(args, tail) {
			return tail, chain
		}
		chain = append(chain, cleanup.ProcessID{PID: parent, Start: int64(above.start)}) //nolint:gosec // seconds since the epoch fit int64
		pid = parent
	}
}

func endsWith(args, tail []string) bool {
	return len(args) > len(tail) && slices.Equal(args[len(args)-len(tail):], tail)
}

func active(worktree string, holders []cleanup.Holder) error {
	if len(holders) == 0 {
		return nil
	}
	sort.Slice(holders, func(a, b int) bool { return holders[a].PID < holders[b].PID })
	return &cleanup.ActiveError{Worktree: worktree, Holders: holders}
}

func kernelPath(lib *libSystem, dir string) (string, error) {
	file, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("native: %w", err)
	}
	defer func() { _ = file.Close() }()
	node, err := lib.vnodeOfFD(os.Getpid(), int32(file.Fd())) //nolint:gosec // a descriptor fits int32
	if err != nil {
		return "", fmt.Errorf("native: read the kernel path of %s: %w", dir, err)
	}
	path, linked, err := lib.locate(node)
	if err != nil {
		return "", fmt.Errorf("native: locate %s: %w", dir, err)
	}
	if !linked {
		return "", fmt.Errorf("native: %s was removed while being inspected: %w", dir, unix.ENOENT)
	}
	return path, nil
}

func (lib *libSystem) locate(node *vnodeInfoPath) (path string, linked bool, err error) {
	if node.kind == vnodeNone {
		return "", false, nil
	}
	path = unix.ByteSliceToString(node.path[:])
	var named unix.Stat_t
	if err := unix.Lstat(path, &named); err == nil && named.Dev == node.dev && named.Ino == node.ino {
		return path, true, nil
	}
	path, err = lib.pathByID(node.fsid, node.ino)
	if errors.Is(err, unix.ENOENT) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve inode %d of filesystem %d, which the kernel path %q does not name: %w", node.ino, node.fsid[0], unix.ByteSliceToString(node.path[:]), err)
	}
	return path, true, nil
}

const argumentAlignment = 8

type scan struct {
	lib       *libSystem
	root      string
	given     string
	self      int
	requester int
	named     bool
	retiring  []cleanup.ProcessID
	tail      []string
	launchers []cleanup.ProcessID
}

func (s *scan) inspect(pid int) (cleanup.Holder, bool, error) {
	bsd, err := pidInfo[procBSDInfo](s.lib, pid, flavorBSDInfo)
	if errors.Is(err, unix.ESRCH) || err != nil && s.exited(pid) {
		return cleanup.Holder{}, false, nil
	}
	if err != nil {
		return cleanup.Holder{}, false, fmt.Errorf("native: identify pid %d: %w", pid, err)
	}
	if int(bsd.ppid) == s.self {
		return cleanup.Holder{}, false, nil
	}
	name := unix.ByteSliceToString(bsd.name[:])
	if name == "" {
		name = unix.ByteSliceToString(bsd.comm[:])
	}
	evidence, path, err := s.evidence(cleanup.ProcessID{PID: pid, Start: int64(bsd.start)}, bsd.flags&flagThreadCwd != 0) //nolint:gosec // seconds since the epoch fit int64
	if err != nil && s.exited(pid) {
		return cleanup.Holder{}, false, nil
	}
	if err != nil {
		return cleanup.Holder{}, false, fmt.Errorf("native: inspect pid %d (%s): %w", pid, name, err)
	}
	if path == "" {
		return cleanup.Holder{}, false, nil
	}
	return cleanup.Holder{PID: pid, Name: name, TTY: bsd.tdev != noDevice, Evidence: evidence, Path: path}, true, nil
}

func (s *scan) exited(pid int) bool {
	bsd, err := pidInfo[procBSDInfo](s.lib, pid, flavorBSDInfo)
	if err != nil {
		return errors.Is(err, unix.ESRCH)
	}
	return bsd.status == statusZombie || bsd.flags&flagInExit != 0
}

func (s *scan) evidence(process cleanup.ProcessID, threaded bool) (kind, path string, err error) {
	pid := process.PID
	if path, err = s.cwd(pid, threaded); err != nil || path != "" {
		return cleanup.EvidenceCwd, path, err
	}
	if pid == s.self {
		return "", "", nil
	}
	retiring := slices.Contains(s.retiring, process)
	if !retiring {
		if path, err = s.descriptor(process); err != nil || path != "" {
			return cleanup.EvidenceFD, path, err
		}
	}
	if !s.named || pid != s.requester {
		if path, err = s.argument(pid, slices.Contains(s.launchers, process)); err != nil || path != "" {
			return cleanup.EvidenceArgv, path, err
		}
	}
	if retiring {
		return "", "", s.unchanged(process)
	}
	return "", "", nil
}

func (s *scan) unchanged(process cleanup.ProcessID) error {
	bsd, err := pidInfo[procBSDInfo](s.lib, process.PID, flavorBSDInfo)
	if err != nil {
		return fmt.Errorf("identify it again: %w", err)
	}
	if start := int64(bsd.start); start != process.Start { //nolint:gosec // seconds since the epoch fit int64
		return fmt.Errorf("its pid passed to a process started at %d while the descriptors of the watcher started at %d went unread", start, process.Start)
	}
	return nil
}

func (s *scan) held(node *vnodeInfoPath) (string, error) {
	path, linked, err := s.lib.locate(node)
	if err != nil || !linked || !within(s.root, path) {
		return "", err
	}
	return path, nil
}

func (s *scan) cwd(pid int, threaded bool) (string, error) {
	vnodes, err := pidInfo[procVnodePathInfo](s.lib, pid, flavorVnodePathInfo)
	if err != nil {
		return "", fmt.Errorf("read its working directory: %w", err)
	}
	path, err := s.held(&vnodes.cdir)
	if err != nil {
		return "", fmt.Errorf("locate its working directory: %w", err)
	}
	if path != "" || !threaded {
		return path, nil
	}
	return s.threadCwd(pid)
}

func (s *scan) threadCwd(pid int) (string, error) {
	threads, err := s.lib.listThreads(pid)
	if err != nil {
		return "", fmt.Errorf("list its threads: %w", err)
	}
	for _, thread := range threads {
		paths, err := pidInfoOf[procThreadPathInfo](s.lib, pid, flavorThreadPathInfo, thread)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read the working directory of its thread %#x: %w", thread, err)
		}
		path, err := s.held(&paths.cdir)
		if err != nil {
			return "", fmt.Errorf("locate the working directory of its thread %#x: %w", thread, err)
		}
		if path != "" {
			return path, nil
		}
	}
	return "", nil
}

type descriptorPass struct {
	*scan
	process  cleanup.ProcessID
	approval *approval
}

func (s *scan) descriptor(process cleanup.ProcessID) (string, error) {
	fds, err := s.lib.listFDs(process.PID)
	if err != nil {
		return "", fmt.Errorf("list its descriptors: %w", err)
	}
	pass := &descriptorPass{scan: s, process: process}
	for _, fd := range fds {
		if fd.kind != fdTypeVnode {
			continue
		}
		path, err := pass.place(fd.fd)
		if err != nil || path != "" {
			return path, err
		}
	}
	return "", pass.confirm()
}

func (p *descriptorPass) place(fd int32) (string, error) {
	node, err := p.lib.vnodeOfFD(p.process.PID, fd)
	if errors.Is(err, unix.EBADF) || errors.Is(err, unix.ENOENT) {
		return "", nil
	}
	if denied(err) {
		return p.refused(fd, fmt.Errorf("read its descriptor %d: %w", fd, err))
	}
	if err != nil {
		return "", fmt.Errorf("read its descriptor %d: %w", fd, err)
	}
	path, err := p.held(node)
	if denied(err) {
		return p.refused(fd, fmt.Errorf("locate its descriptor %d: %w", fd, err))
	}
	if err != nil {
		return "", fmt.Errorf("locate its descriptor %d: %w", fd, err)
	}
	return path, nil
}

func (s *scan) argument(pid int, launcher bool) (string, error) {
	args, err := processArguments(pid)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errUnreadArguments, err)
	}
	if launcher && endsWith(args, s.tail) {
		args = args[:len(args)-len(s.tail)]
	}
	for _, arg := range args {
		if path, names := s.names(arg); names {
			return path, nil
		}
	}
	return "", nil
}

func processArguments(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return arguments(raw)
}

func arguments(raw []byte) ([]string, error) {
	if len(raw) < 4 {
		return nil, fmt.Errorf("%d bytes hold no argument count", len(raw))
	}
	count := int(int32(binary.NativeEndian.Uint32(raw))) //nolint:gosec // argc is a C int
	area := raw[4:]
	program := bytes.IndexByte(area, 0)
	if program < 0 {
		return nil, errors.New("the program path is not terminated")
	}
	rest := area[min(len(area), (program/argumentAlignment+1)*argumentAlignment):]
	var args []string
	for len(args) < count {
		arg, after, terminated := bytes.Cut(rest, []byte{0})
		if !terminated {
			return nil, fmt.Errorf("%d of %d arguments are present", len(args), count)
		}
		args = append(args, string(arg))
		rest = after
	}
	return args, nil
}

func (s *scan) names(arg string) (string, bool) {
	for _, candidate := range []string{arg, arg[strings.LastIndexByte(arg, '=')+1:]} {
		if !filepath.IsAbs(candidate) {
			continue
		}
		if path := filepath.Clean(candidate); within(s.root, path) || within(s.given, path) {
			return path, true
		}
	}
	return "", false
}

func within(root, path string) bool {
	if root == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == root || strings.HasPrefix(path, root+"/")
}
