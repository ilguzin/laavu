package link

import (
	"net"
	"syscall"
)

// peerUID returns the uid of the process on the other end of a unix socket.
func peerUID(c *net.UnixConn) (int, bool) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *syscall.Ucred
	raw.Control(func(fd uintptr) {
		cred, _ = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if cred == nil {
		return 0, false
	}
	return int(cred.Uid), true
}
