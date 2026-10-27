package worker

import (
	"encoding/json"
	"io"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/ilguzin/laavu/internal/wire"
)

// AttachArgs describes attaching to a session.
type AttachArgs struct {
	Name    string  `json:"name"`
	Cols    int     `json:"cols"`
	Rows    int     `json:"rows"`
	Profile Profile `json:"profile"`
}

// opAttach holds a PTY with a tmux attach client and moves bytes into frames.
// The terminal module is isolated: if plain attach is replaced by control mode,
// only this file changes.
func (w *Worker) opAttach(c *Conn, args json.RawMessage) (any, error) {
	a, err := decode[AttachArgs](args)
	if err != nil {
		return nil, err
	}
	if _, err := w.Ensure(EnsureArgs{Name: a.Name, Profile: a.Profile, Cols: a.Cols, Rows: a.Rows}); err != nil {
		return nil, err
	}
	if a.Cols <= 0 || a.Rows <= 0 {
		a.Cols, a.Rows = 80, 24
	}
	cmd := w.Tmux.AttachCommand(a.Name)
	cmd.Dir = w.Home
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(a.Cols), Rows: uint16(a.Rows)})
	if err != nil {
		return nil, err
	}
	if err := c.Reply(nil, nil); err != nil {
		f.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return nil, errStreamed
	}
	var wmu sync.Mutex
	write := func(t byte, p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return c.WriteFrame(t, p)
	}
	done := make(chan struct{})
	go w.reportWindowSize(a.Name, done, write)
	go func() {
		defer close(done)
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if werr := write(wire.FrameData, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			t, p, err := c.ReadFrame()
			if err != nil || t == wire.FrameEnd {
				break
			}
			switch t {
			case wire.FrameData:
				if _, err := f.Write(p); err != nil && err != io.EOF {
					return
				}
			case wire.FrameResize:
				// The tmux window size is shared by all session clients and, with window-size latest,
				// follows the most recently active one. The kernel sends SIGWINCH only if the size
				// changed, so we send it ourselves: a resize to the same size is how the
				// client "takes back" the window when the user returns from another device.
				if cols, rows, ok := wire.ParseResize(p); ok && cols > 0 && rows > 0 {
					pty.Setsize(f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
					cmd.Process.Signal(syscall.SIGWINCH)
				}
			}
		}
		// The daemon closed the channel: kill only our own tmux client, the session lives on.
		cmd.Process.Kill()
	}()
	<-done
	cmd.Wait()
	f.Close()
	write(wire.FrameEnd, nil)
	return nil, errStreamed
}

// WinSize is the tmux window size, a FrameJSON frame in the attach stream.
type WinSize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// reportWindowSize tells the client the tmux window size when it changes. The window
// is shared by all clients: if the session was opened on another device, the client sees
// that the window no longer has its size and offers to take it back.
func (w *Worker) reportWindowSize(name string, done <-chan struct{}, write func(byte, []byte) error) {
	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()
	var last WinSize
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		cols, rows, err := w.Tmux.WindowSize(name)
		if err != nil || (cols == last.Cols && rows == last.Rows) {
			continue
		}
		last = WinSize{cols, rows}
		b, _ := json.Marshal(last)
		if write(wire.FrameJSON, b) != nil {
			return
		}
	}
}
