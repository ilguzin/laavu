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

// AttachArgs — подключение к сессии.
type AttachArgs struct {
	Name    string  `json:"name"`
	Cols    int     `json:"cols"`
	Rows    int     `json:"rows"`
	Profile Profile `json:"profile"`
}

// opAttach держит PTY с клиентом tmux attach и перекладывает байты в кадры.
// Модуль терминала изолирован: если обычный attach заменится на control mode,
// поменяется только этот файл.
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
				// Размер окна tmux один на всех клиентов сессии и при window-size latest
				// следует за последним активным. Ядро шлёт SIGWINCH, только если размер
				// изменился, поэтому шлём его сами: resize того же размера — способ
				// клиента «забрать» окно, когда к нему вернулись с другого устройства.
				if cols, rows, ok := wire.ParseResize(p); ok && cols > 0 && rows > 0 {
					pty.Setsize(f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
					cmd.Process.Signal(syscall.SIGWINCH)
				}
			}
		}
		// Демон закрыл канал: убиваем только своего клиента tmux, сессия живёт дальше.
		cmd.Process.Kill()
	}()
	<-done
	cmd.Wait()
	f.Close()
	write(wire.FrameEnd, nil)
	return nil, errStreamed
}

// WinSize — размер окна tmux, кадр FrameJSON в потоке attach.
type WinSize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// reportWindowSize сообщает клиенту размер окна tmux, когда он меняется. Окно
// одно на всех клиентов: если сессию открыли на другом устройстве, клиент видит,
// что окно уже не его размера, и предлагает вернуть его себе.
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
