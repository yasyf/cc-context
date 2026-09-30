package daemon

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
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
