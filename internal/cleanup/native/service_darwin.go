package native

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	launchd      = 1
	sealedVolume = unix.MNT_RDONLY | unix.MNT_ROOTFS | unix.MNT_SNAPSHOT
	systemMount  = "/"
)

var approvedServices = []string{
	"/System/Library/Frameworks/QuickLookThumbnailing.framework/Support/com.apple.quicklook.ThumbnailsAgent",
	"/System/Library/Frameworks/ClassKit.framework/Versions/A/progressd",
	"/System/Library/PrivateFrameworks/ScreenTimeCore.framework/Versions/A/ScreenTimeAgent",
	"/usr/libexec/dmd",
	"/usr/libexec/routined",
	"/usr/libexec/nsurlsessiond",
	"/usr/libexec/UserEventAgent",
	"/usr/libexec/lsd",
	"/usr/libexec/knowledge-agent",
	"/System/Library/PrivateFrameworks/CoreDuetContext.framework/Versions/A/Resources/ContextStoreAgent",
}

type birth struct {
	seconds uint64
	micros  uint64
}

type approval struct {
	fd      int32
	refusal error
	born    birth
}

func (s *scan) service(process cleanup.ProcessID) (birth, error) {
	pid := process.PID
	path, err := s.lib.pidPath(pid)
	if err != nil {
		return birth{}, fmt.Errorf("read its executable path: %w", err)
	}
	if !slices.Contains(approvedServices, path) {
		return birth{}, fmt.Errorf("its executable is %s", path)
	}
	var volume unix.Statfs_t
	if err := s.lib.statfs(path, &volume); err != nil {
		return birth{}, fmt.Errorf("read the volume of its executable %s: %w", path, err)
	}
	if mount := unix.ByteSliceToString(volume.Mntonname[:]); mount != systemMount || volume.Flags&sealedVolume != sealedVolume {
		return birth{}, fmt.Errorf("its executable %s is on the volume mounted at %s with flags %#x, not the sealed system volume", path, mount, volume.Flags)
	}
	status, err := s.lib.codeSigningStatus(pid)
	if err != nil {
		return birth{}, fmt.Errorf("read its code signing status: %w", err)
	}
	if status&(csValid|csPlatformBinary|csDebugged) != csValid|csPlatformBinary {
		return birth{}, fmt.Errorf("its code signing status %#x is not that of a valid platform binary left undebugged", status)
	}
	bsd, err := pidInfo[procBSDInfo](s.lib, pid, flavorBSDInfo)
	if err != nil {
		return birth{}, fmt.Errorf("identify it again: %w", err)
	}
	if uid := os.Getuid(); int(bsd.uid) != uid || int(bsd.ruid) != uid {
		return birth{}, fmt.Errorf("it runs as uid %d with real uid %d, not as uid %d", bsd.uid, bsd.ruid, uid)
	}
	if bsd.ppid != launchd {
		return birth{}, fmt.Errorf("its parent is pid %d, not launchd", bsd.ppid)
	}
	if bsd.tdev != noDevice {
		return birth{}, fmt.Errorf("it has the controlling terminal %#x", bsd.tdev)
	}
	if start := int64(bsd.start); start != process.Start { //nolint:gosec // seconds since the epoch fit int64
		return birth{}, fmt.Errorf("its pid names a process started at %d, not the one started at %d whose evidence is being read", start, process.Start)
	}
	return birth{seconds: bsd.start, micros: bsd.startMicros}, nil
}

func denied(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
}

func (p *descriptorPass) refused(fd int32, refusal error) (string, error) {
	if p.approval == nil {
		born, err := p.service(p.process)
		if err != nil {
			return "", fmt.Errorf("%w; its owner is not an approved Apple service: %w", refusal, err)
		}
		p.approval = &approval{fd: fd, refusal: refusal, born: born}
	}
	node, err := p.lib.vnodeIdentityOfFD(p.process.PID, fd)
	if denied(err) || errors.Is(err, unix.EBADF) || errors.Is(err, unix.ENOENT) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("identify its descriptor %d: %w", fd, err)
	}
	path, err := p.heldByID(node)
	if err != nil {
		return "", fmt.Errorf("locate its descriptor %d: %w", fd, err)
	}
	return path, nil
}

func (p *descriptorPass) confirm() error {
	if p.approval == nil {
		return nil
	}
	first := p.approval
	born, err := p.service(p.process)
	if err != nil {
		return fmt.Errorf("%w; its owner no longer passes for an approved Apple service: %w", first.refusal, err)
	}
	if born != first.born {
		return fmt.Errorf("%w; its pid passed from the service started at %d.%06d to a process started at %d.%06d while its descriptors were read", first.refusal, first.born.seconds, first.born.micros, born.seconds, born.micros)
	}
	return nil
}

func (s *scan) heldByID(node *vnodeInfo) (string, error) {
	if node.kind == vnodeNone {
		return "", nil
	}
	path, err := s.lib.pathByID(node.fsid, node.ino)
	if denied(err) || errors.Is(err, unix.ENOENT) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve inode %d of filesystem %d: %w", node.ino, node.fsid[0], err)
	}
	if !within(s.root, path) {
		return "", nil
	}
	return path, nil
}
