package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// serveHooks accepts hook events. The daemon identifies the agent by the sender's
// uid (SO_PEERCRED), not by the message: another agent's session status cannot be
// forged. Only session events are accepted from the socket.
func (d *Daemon) serveHooks(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go d.handleHookConn(c)
	}
}

func peerUID(c net.Conn) (int, bool) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *unix.Ucred
	var cerr error
	raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if cerr != nil || cred == nil {
		return 0, false
	}
	return int(cred.Uid), true
}

func (d *Daemon) handleHookConn(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	uid, ok := peerUID(c)
	if !ok {
		return
	}
	a, ok := d.byUID[uid]
	if !ok {
		return
	}
	// An agent must not bring down the shared daemon: at most 16 concurrent hook
	// connections and at most 8 KB per event.
	if !d.hookSlots.acquire(uid) {
		return
	}
	defer d.hookSlots.release(uid)
	line, err := bufio.NewReaderSize(io.LimitReader(c, 8192), 8192).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var ev hookEvent
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	d.handleHook(a.Name, ev)
}

// slots limits concurrent hook connections per agent.
type slots struct {
	mu sync.Mutex
	n  map[int]int
}

const maxHookConns = 16

func (s *slots) acquire(uid int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == nil {
		s.n = map[int]int{}
	}
	if s.n[uid] >= maxHookConns {
		return false
	}
	s.n[uid]++
	return true
}

func (s *slots) release(uid int) {
	s.mu.Lock()
	s.n[uid]--
	s.mu.Unlock()
}
