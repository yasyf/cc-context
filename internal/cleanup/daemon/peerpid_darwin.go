package daemon

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func peerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("reach the socket: %w", err)
	}
	var (
		pid     int
		readErr error
	)
	if err := raw.Control(func(fd uintptr) {
		pid, readErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID) //nolint:gosec // an open descriptor fits an int
	}); err != nil {
		return 0, fmt.Errorf("reach the socket: %w", err)
	}
	if readErr != nil {
		return 0, fmt.Errorf("read LOCAL_PEERPID: %w", readErr)
	}
	return pid, nil
}

func peerCred(conn *net.UnixConn) (cleanup.Peer, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return cleanup.Peer{}, fmt.Errorf("reach the socket: %w", err)
	}
	var (
		pid     int
		cred    *unix.Xucred
		readErr error
	)
	if err := raw.Control(func(fd uintptr) {
		pid, readErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID) //nolint:gosec // an open descriptor fits an int
		if readErr != nil {
			readErr = fmt.Errorf("read LOCAL_PEERPID: %w", readErr)
			return
		}
		cred, readErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED) //nolint:gosec // an open descriptor fits an int
		if readErr != nil {
			readErr = fmt.Errorf("read LOCAL_PEERCRED: %w", readErr)
		}
	}); err != nil {
		return cleanup.Peer{}, fmt.Errorf("reach the socket: %w", err)
	}
	if readErr != nil {
		return cleanup.Peer{}, readErr
	}
	return cleanup.Peer{PID: pid, UID: int(cred.Uid)}, nil
}
