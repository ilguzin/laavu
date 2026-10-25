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

// serveHooks принимает события хуков. Агента демон узнаёт не из сообщения, а по
// uid отправителя (SO_PEERCRED): статус сессий чужого агента не подделать. От
// сокета принимаются только события сессий.
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
	// Агент не должен положить общий для всех демон: не больше 16 соединений
	// хуков одновременно и не больше 8 КБ на событие.
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

// slots — предел одновременных соединений хуков на агента.
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
