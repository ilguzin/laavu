//go:build !linux

package link

import "net"

// peerUID: the helper runs only on a Linux server. Elsewhere (laavu on the owner's
// laptop) nobody is trusted.
func peerUID(*net.UnixConn) (int, bool) { return 0, false }
