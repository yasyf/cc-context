package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

type rootWalk struct {
	path    string
	scan    *scan
	holders []cleanup.Holder
	failure error
	current rootEvidence
}

type rootEvidence struct {
	err  error
	kind string
	path string
	done bool
	pass *descriptorPass
}

// GuardRoots checks every requested root in one fresh process walk while keeping
// each root's watcher allowances and classification independent. It returns the
// first blocking root in input order and its existing activity or inspection error.
func GuardRoots(ctx context.Context, first cleanup.GuardRoot, additional ...cleanup.GuardRoot) (string, error) {
	lib, err := loadLibSystem()
	if err != nil {
		return first.Path, err
	}
	return guardRoots(ctx, lib, first, additional...)
}

func guardRoots(ctx context.Context, lib *libSystem, first cleanup.GuardRoot, additional ...cleanup.GuardRoot) (string, error) {
	if err := ctx.Err(); err != nil {
		return first.Path, err
	}
	base, err := newScan(ctx, lib, first.Path)
	if err != nil {
		return first.Path, err
	}
	base.retiring = first.Retiring
	roots := make([]rootWalk, 1, len(additional)+1)
	roots[0].scan, roots[0].path = base, first.Path
	for _, target := range additional {
		root, err := kernelPath(lib, target.Path)
		copy := *base
		copy.root, copy.given, copy.retiring, copy.ignored = root, filepath.Clean(target.Path), target.Retiring, nil
		roots = append(roots, rootWalk{path: target.Path, scan: &copy, failure: err})
	}
	pids, err := lib.listPIDs(procUIDOnly, os.Getuid())
	if err != nil {
		return first.Path, err
	}
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return first.Path, err
		}
		walkProcess(neededRoots(roots), int(pid))
		if roots[0].failure != nil {
			return first.Path, roots[0].failure
		}
	}
	if err := ctx.Err(); err != nil {
		return first.Path, err
	}
	return guardOutcome(ctx, roots)
}

func guardOutcome(ctx context.Context, roots []rootWalk) (string, error) {
	for i := range roots {
		root := &roots[i]
		if root.failure != nil {
			return root.path, root.failure
		}
		cleanup.NoteIgnored(ctx, root.scan.ignored...)
		if err := active(root.path, root.holders); err != nil {
			return root.path, err
		}
	}
	return "", nil
}

func walkProcess(roots []rootWalk, pid int) {
	lib := roots[0].scan.lib
	bsd, err := pidInfo[procBSDInfo](lib, pid, flavorBSDInfo)
	for i := range roots {
		root := &roots[i]
		root.current = rootEvidence{done: root.failure != nil}
		if root.current.done {
			continue
		}
		if errors.Is(err, unix.ESRCH) || err != nil && root.scan.exited(pid) {
			root.current.done = true
			continue
		}
		if err != nil {
			root.failure = fmt.Errorf("native: identify pid %d: %w", pid, err)
			root.current.done = true
			continue
		}
		if int(bsd.ppid) == root.scan.self {
			root.current.done = true
		}
	}
	if err != nil || !pendingRoots(roots) {
		return
	}
	name := unix.ByteSliceToString(bsd.name[:])
	if name == "" {
		name = unix.ByteSliceToString(bsd.comm[:])
	}
	process := cleanup.ProcessID{PID: pid, Start: int64(bsd.start)} //nolint:gosec // seconds since the epoch fit int64
	walkCwds(roots, process, name, bsd.flags&flagThreadCwd != 0)
	roots = neededRoots(roots)
	if roots[0].failure != nil {
		return
	}
	if pid == roots[0].scan.self {
		finishProcess(roots, process, name, bsd.tdev != noDevice)
		return
	}
	walkDescriptors(roots, process, name)
	roots = neededRoots(roots)
	if roots[0].failure != nil {
		return
	}
	walkArguments(roots, process)
	for i := range roots {
		root := &roots[i]
		if !root.current.done && slices.Contains(root.scan.retiring, process) {
			root.fail(process.PID, name, root.scan.unchanged(process))
		}
	}
	finishProcess(roots, process, name, bsd.tdev != noDevice)
}

func neededRoots(roots []rootWalk) []rootWalk {
	for i := range roots {
		root := &roots[i]
		if root.failure != nil || len(root.holders) != 0 || root.current.path != "" {
			return roots[:i+1]
		}
	}
	return roots
}

func pendingRoots(roots []rootWalk) bool {
	return slices.ContainsFunc(roots, func(root rootWalk) bool { return !root.current.done })
}

func (r *rootWalk) fail(pid int, name string, err error) {
	if err == nil {
		return
	}
	r.current.done = true
	if !r.scan.exited(pid) {
		r.failure = fmt.Errorf("native: inspect pid %d (%s): %w", pid, name, err)
	}
}

func (r *rootWalk) hold(kind, path string) {
	r.current.kind, r.current.path, r.current.done = kind, path, true
}

func failPending(roots []rootWalk, process cleanup.ProcessID, name string, err error) {
	for i := range roots {
		if !roots[i].current.done {
			roots[i].fail(process.PID, name, err)
		}
	}
}

func holdCwd(roots []rootWalk, process cleanup.ProcessID, name string, node *vnodeInfoPath, detail string) {
	path, linked, err := roots[0].scan.lib.locate(node)
	if err != nil {
		failPending(roots, process, name, fmt.Errorf("%s: %w", detail, err))
		return
	}
	if !linked {
		return
	}
	for i := range roots {
		root := &roots[i]
		if !root.current.done && within(root.scan.root, path) {
			root.hold(cleanup.EvidenceCwd, path)
		}
	}
}

func walkCwds(roots []rootWalk, process cleanup.ProcessID, name string, threaded bool) {
	lib := roots[0].scan.lib
	vnodes, err := pidInfo[procVnodePathInfo](lib, process.PID, flavorVnodePathInfo)
	if err != nil {
		failPending(roots, process, name, fmt.Errorf("read its working directory: %w", err))
		return
	}
	holdCwd(roots, process, name, &vnodes.cdir, "locate its working directory")
	roots = neededRoots(roots)
	if !threaded || !pendingRoots(roots) {
		return
	}
	threads, err := lib.listThreads(process.PID)
	if err != nil {
		failPending(roots, process, name, fmt.Errorf("list its threads: %w", err))
		return
	}
	for _, thread := range threads {
		paths, err := pidInfoOf[procThreadPathInfo](lib, process.PID, flavorThreadPathInfo, thread)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			failPending(roots, process, name, fmt.Errorf("read the working directory of its thread %#x: %w", thread, err))
			return
		}
		holdCwd(roots, process, name, &paths.cdir, fmt.Sprintf("locate the working directory of its thread %#x", thread))
		roots = neededRoots(roots)
		if !pendingRoots(roots) {
			return
		}
	}
}

func finishProcess(roots []rootWalk, process cleanup.ProcessID, name string, tty bool) {
	for i := range roots {
		root := &roots[i]
		if root.failure == nil && root.current.path != "" {
			root.holders = append(root.holders, cleanup.Holder{PID: process.PID, Name: name, TTY: tty, Evidence: root.current.kind, Path: root.current.path})
		}
	}
}

func walkArguments(roots []rootWalk, process cleanup.ProcessID) {
	needed := false
	for i := range roots {
		root := &roots[i]
		needed = needed || !root.current.done && (!root.scan.named || process != root.scan.requester)
	}
	if !needed {
		return
	}
	args, err := processArguments(process.PID)
	if err != nil {
		return
	}
	for i := range roots {
		root := &roots[i]
		if root.current.done || root.scan.named && process == root.scan.requester {
			continue
		}
		if path := root.scan.argument(args, slices.Contains(root.scan.launchers, process)); path != "" {
			root.hold(cleanup.EvidenceArgv, path)
		}
	}
}

func walkDescriptors(roots []rootWalk, process cleanup.ProcessID, name string) {
	descriptorEvidence(roots, process, name)
	for i := range roots {
		if roots[i].current.err != nil {
			roots[i].fail(process.PID, name, roots[i].current.err)
		}
	}
}

func (r *rootWalk) descriptorError(err error) {
	if err != nil {
		r.current.err, r.current.done = err, true
	}
}

func descriptorEvidence(roots []rootWalk, process cleanup.ProcessID, name string) {
	needed := false
	for i := range roots {
		root := &roots[i]
		if !root.current.done && !slices.Contains(root.scan.retiring, process) {
			root.current.pass = &descriptorPass{scan: root.scan, process: process, name: name}
			needed = true
		}
	}
	if !needed {
		return
	}
	lib := roots[0].scan.lib
	fds, err := lib.listFDs(process.PID)
	if err != nil {
		for i := range roots {
			root := &roots[i]
			if root.current.pass != nil {
				root.descriptorError(fmt.Errorf("list its descriptors: %w", err))
			}
		}
		return
	}
	for _, fd := range fds {
		if fd.kind != fdTypeVnode {
			continue
		}
		observation := descriptorObservation{lib: lib, process: process, fd: fd.fd}
		for i := range roots {
			root := &roots[i]
			if root.current.done || root.current.pass == nil {
				continue
			}
			path, err := root.current.pass.placeObserved(&observation)
			if err != nil {
				root.descriptorError(err)
			} else if path != "" {
				root.hold(cleanup.EvidenceFD, path)
				roots = roots[:i+1]
				break
			}
		}
		if roots[0].current.err != nil || !pendingRoots(roots) {
			return
		}
	}
	for i := range roots {
		root := &roots[i]
		if !root.current.done && root.current.pass != nil {
			root.descriptorError(root.current.pass.confirm())
		}
	}
}

type descriptorObservation struct {
	lib     *libSystem
	process cleanup.ProcessID
	fd      int32

	nodeRead bool
	node     *vnodeInfoPath
	nodeErr  error

	located     bool
	path        string
	linked      bool
	locationErr error

	identified   bool
	identityPath string
	identityErr  error
}

func (o *descriptorObservation) readNode() (*vnodeInfoPath, error) {
	if !o.nodeRead {
		o.node, o.nodeErr = o.lib.vnodeOfFD(o.process.PID, o.fd)
		o.nodeRead = true
	}
	return o.node, o.nodeErr
}

func (o *descriptorObservation) locate() (string, bool, error) {
	if !o.located {
		o.path, o.linked, o.locationErr = o.lib.locate(o.node)
		o.located = true
	}
	return o.path, o.linked, o.locationErr
}

func (o *descriptorObservation) identify() (string, error) {
	if o.identified {
		return o.identityPath, o.identityErr
	}
	o.identified = true
	node, err := o.lib.vnodeIdentityOfFD(o.process.PID, o.fd)
	switch {
	case denied(err), errors.Is(err, unix.EBADF), errors.Is(err, unix.ENOENT):
		return "", nil
	case err != nil:
		o.identityErr = fmt.Errorf("identify its descriptor %d: %w", o.fd, err)
	case node.kind == vnodeNone:
		return "", nil
	default:
		path, err := o.lib.pathByID(node.fsid, node.ino)
		if denied(err) || errors.Is(err, unix.ENOENT) {
			return "", nil
		}
		if err != nil {
			o.identityErr = fmt.Errorf("locate its descriptor %d: resolve inode %d of filesystem %d: %w", o.fd, node.ino, node.fsid[0], err)
		} else {
			o.identityPath = path
		}
	}
	return o.identityPath, o.identityErr
}

func (p *descriptorPass) placeObserved(o *descriptorObservation) (string, error) {
	_, err := o.readNode()
	switch {
	case errors.Is(err, unix.EBADF), errors.Is(err, unix.ENOENT):
		return "", nil
	case denied(err):
		return p.refused(fmt.Errorf("read its descriptor %d: %w", o.fd, err), o)
	case err != nil:
		return "", fmt.Errorf("read its descriptor %d: %w", o.fd, err)
	}
	path, linked, err := o.locate()
	if denied(err) {
		return p.refused(fmt.Errorf("locate its descriptor %d: %w", o.fd, err), o)
	}
	if err != nil {
		return "", fmt.Errorf("locate its descriptor %d: %w", o.fd, err)
	}
	if !linked || !within(p.root, path) {
		return "", nil
	}
	return path, nil
}
