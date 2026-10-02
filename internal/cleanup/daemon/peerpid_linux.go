package daemon

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func peerPID(conn *net.UnixConn) (int, error) {
	peer, err := peerCred(conn)
	return peer.PID, err
}

func peerCred(conn *net.UnixConn) (cleanup.Peer, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return cleanup.Peer{}, fmt.Errorf("reach the socket: %w", err)
	}
	var (
		cred    *unix.Ucred
		readErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, readErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) //nolint:gosec // an open descriptor fits an int
	}); err != nil {
		return cleanup.Peer{}, fmt.Errorf("reach the socket: %w", err)
	}
	if readErr != nil {
		return cleanup.Peer{}, fmt.Errorf("read SO_PEERCRED: %w", readErr)
	}
	return cleanup.Peer{PID: int(cred.Pid), UID: int(cred.Uid)}, nil
}
