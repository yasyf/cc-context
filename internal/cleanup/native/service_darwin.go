package native

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	launchd      = 1
	sealedVolume = unix.MNT_RDONLY | unix.MNT_ROOTFS | unix.MNT_SNAPSHOT
	systemMount  = "/"
)

var systemDirectories = []string{"/System/Library/", "/usr/libexec/"}

type birth struct {
	seconds uint64
	micros  uint64
}

type approval struct {
	refusal error
	born    birth
	reason  string
}

func (s *scan) bystander(process cleanup.ProcessID) (birth, string, error) {
	pid := process.PID
	path, err := s.lib.pidPath(pid)
	if err != nil {
		return birth{}, "", fmt.Errorf("read its executable path: %w", err)
	}
	daemon := s.daemon(pid, path)
	bsd, err := pidInfo[procBSDInfo](s.lib, pid, flavorBSDInfo)
	if err != nil {
		return birth{}, "", fmt.Errorf("identify it again: %w", err)
	}
	if start := int64(bsd.start); start != process.Start { //nolint:gosec // seconds since the epoch fit int64
		return birth{}, "", fmt.Errorf("its pid names a process started at %d, not the one started at %d whose evidence is being read", start, process.Start)
	}
	born := birth{seconds: bsd.start, micros: bsd.startMicros}
	uid := os.Getuid()
	switch {
	case int(bsd.ruid) != uid:
		return born, fmt.Sprintf("started by uid %d", bsd.ruid), nil
	case daemon != nil:
		return birth{}, "", daemon
	case int(bsd.uid) != uid:
		return birth{}, "", fmt.Errorf("it runs as uid %d, not as uid %d", bsd.uid, uid)
	case bsd.ppid != launchd:
		return birth{}, "", fmt.Errorf("its parent is pid %d, not launchd", bsd.ppid)
	case bsd.tdev != noDevice:
		return birth{}, "", fmt.Errorf("it has the controlling terminal %#x", bsd.tdev)
	}
	return born, "a system daemon at " + path, nil
}

func (s *scan) daemon(pid int, path string) error {
	if !slices.ContainsFunc(systemDirectories, func(dir string) bool { return strings.HasPrefix(path, dir) }) {
		return fmt.Errorf("its executable %s is outside %s", path, strings.Join(systemDirectories, " and "))
	}
	var volume unix.Statfs_t
	if err := s.lib.statfs(path, &volume); err != nil {
		return fmt.Errorf("read the volume of its executable %s: %w", path, err)
	}
	if mount := unix.ByteSliceToString(volume.Mntonname[:]); mount != systemMount || volume.Flags&sealedVolume != sealedVolume {
		return fmt.Errorf("its executable %s is on the volume mounted at %s with flags %#x, not the sealed system volume", path, mount, volume.Flags)
	}
	status, err := s.lib.codeSigningStatus(pid)
	if err != nil {
		return fmt.Errorf("read its code signing status: %w", err)
	}
	if status&(csValid|csPlatformBinary|csDebugged) != csValid|csPlatformBinary {
		return fmt.Errorf("its code signing status %#x is not that of a valid platform binary left undebugged", status)
	}
	return nil
}

func denied(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
}

func (p *descriptorPass) refused(refusal error, observation *descriptorObservation) (string, error) {
	if p.approval == nil {
		born, reason, err := p.bystander(p.process)
		if err != nil {
			return "", fmt.Errorf("%w; its owner may be a process this user started: %w", refusal, err)
		}
		p.approval = &approval{refusal: refusal, born: born, reason: reason}
	}
	path, err := observation.identify()
	if err != nil {
		return "", err
	}
	if !within(p.root, path) {
		return "", nil
	}
	return path, nil
}

func (p *descriptorPass) confirm() error {
	if p.approval == nil {
		return nil
	}
	first := p.approval
	born, _, err := p.bystander(p.process)
	if err != nil {
		return fmt.Errorf("%w; its owner no longer passes for a process this user did not start: %w", first.refusal, err)
	}
	if born != first.born {
		return fmt.Errorf("%w; its pid passed from the process started at %d.%06d to one started at %d.%06d while its descriptors were read", first.refusal, first.born.seconds, first.born.micros, born.seconds, born.micros)
	}
	p.ignored = append(p.ignored, cleanup.Ignored{PID: p.process.PID, Name: p.name, Reason: first.reason})
	return nil
}
