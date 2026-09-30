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
		cred    *unix.Ucred
		readErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, readErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) //nolint:gosec // an open descriptor fits an int
	}); err != nil {
		return 0, fmt.Errorf("reach the socket: %w", err)
	}
	if readErr != nil {
		return 0, fmt.Errorf("read SO_PEERCRED: %w", readErr)
	}
	return int(cred.Pid), nil
}
