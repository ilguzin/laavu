package daemon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/ilguzin/laavu/internal/store"
	"github.com/ilguzin/laavu/internal/wire"
	"github.com/ilguzin/laavu/internal/worker"
)

// One WebSocket per browser tab. Inside are terminal channels (binary frames:
// 2 bytes of channel number plus terminal bytes) and events (JSON). Access is
// checked when each channel is opened.

type hub struct {
	d       *Daemon
	mu      sync.Mutex
	clients map[*wsClient]bool
}

func newHub(d *Daemon) *hub { return &hub{d: d, clients: map[*wsClient]bool{}} }

type wsClient struct {
	d      *Daemon
	user   *store.User
	uid    int64
	token  string
	out    chan wsOut
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	chans   map[uint16]*termChan
	sizes   map[uint16][2]int // last resize that arrived while the channel was still attaching
	agents  map[string]bool   // agent event subscriptions
	focus   map[string]bool   // sessions on screen
	visible bool
	watches map[string]bool // agent/folder
}

type wsOut struct {
	typ  websocket.MessageType
	data []byte
}

type termChan struct {
	conn   *wire.Conn
	wmu    sync.Mutex
	agent  string
	name   string
	closed bool
}

func (h *hub) add(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
}

func (h *hub) remove(c *wsClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *hub) snapshot() []*wsClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, c)
	}
	return out
}

// broadcastAgent sends an event to all connected users with access to the agent.
func (h *hub) broadcastAgent(agent string, msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	for _, c := range h.snapshot() {
		c.mu.Lock()
		sub := c.agents[agent]
		c.mu.Unlock()
		if sub && h.d.canAccessUID(c, agent) {
			c.send(websocket.MessageText, b)
		}
	}
}

func (h *hub) hasAgentSubscribers(agent string) bool {
	for _, c := range h.snapshot() {
		c.mu.Lock()
		sub := c.agents[agent]
		c.mu.Unlock()
		if sub {
			return true
		}
	}
	return false
}

// isWatching reports whether the user is looking at the session right now on any device.
func (h *hub) isWatching(uid int64, agent, session string) bool {
	k := key(agent, session)
	for _, c := range h.snapshot() {
		if c.uid != uid {
			continue
		}
		c.mu.Lock()
		w := c.visible && c.focus[k]
		c.mu.Unlock()
		if w {
			return true
		}
	}
	return false
}

// dropLogin closes the connections of one sign-in session: the user signed out on that device.
func (h *hub) dropLogin(token string) {
	for _, c := range h.snapshot() {
		if token != "" && c.token == token {
			c.cancel()
		}
	}
}

func (d *Daemon) canAccessUID(c *wsClient, agent string) bool {
	if d.Cfg.NoAuth {
		_, ok := d.agents[agent]
		return ok
	}
	return d.canAccess(c.user, agent)
}

func (c *wsClient) send(typ websocket.MessageType, b []byte) {
	select {
	case c.out <- wsOut{typ, b}:
	case <-c.ctx.Done():
	}
}

func (c *wsClient) sendJSON(v any) {
	b, err := json.Marshal(v)
	if err == nil {
		c.send(websocket.MessageText, b)
	}
}

// wsMsg is a control message from the client.
type wsMsg struct {
	T       string   `json:"t"`
	Ch      uint16   `json:"ch"`
	Agent   string   `json:"agent"`
	Session string   `json:"session"`
	Folder  string   `json:"folder"`
	Cols    int      `json:"cols"`
	Rows    int      `json:"rows"`
	Agents  []string `json:"agents"`
	Focus   []string `json:"focus"` // agent/session
	Visible bool     `json:"visible"`
}

func (d *Daemon) handleWS(w http.ResponseWriter, r *http.Request, u *store.User, token string) {
	// Origin is already checked in middleware; the library needs no check of its own.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	c := &wsClient{
		d: d, user: u, token: token, out: make(chan wsOut, 256), ctx: ctx, cancel: cancel,
		chans: map[uint16]*termChan{}, sizes: map[uint16][2]int{}, agents: map[string]bool{}, focus: map[string]bool{}, watches: map[string]bool{},
		visible: true,
	}
	if u != nil {
		c.uid = u.ID
	}
	d.hub.add(c)
	defer func() {
		d.hub.remove(c)
		cancel()
		c.closeAll()
		conn.CloseNow()
	}()
	// A single writer keeps frame order.
	go func() {
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-c.out:
				wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
				err := conn.Write(wctx, m.typ, m.data)
				wcancel()
				if err != nil {
					cancel()
					return
				}
			case <-ping.C:
				pctx, pcancel := context.WithTimeout(ctx, 20*time.Second)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	// The sign-in session may end while the socket is open: check once a minute.
	if !d.Cfg.NoAuth {
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if _, err := d.Store.LoginUser(token); err != nil {
						cancel()
						return
					}
				}
			}
		}()
	}
	c.sendJSON(map[string]any{"t": "hello"})
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			if len(data) < 2 {
				continue
			}
			ch := binary.BigEndian.Uint16(data)
			c.mu.Lock()
			tc := c.chans[ch]
			c.mu.Unlock()
			if tc != nil {
				tc.write(wire.FrameData, data[2:])
			}
			continue
		}
		var m wsMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		c.handle(m)
	}
}

func (tc *termChan) write(t byte, p []byte) {
	tc.wmu.Lock()
	defer tc.wmu.Unlock()
	if !tc.closed {
		tc.conn.WriteFrame(t, p)
	}
}

func (tc *termChan) close() {
	tc.wmu.Lock()
	if !tc.closed {
		tc.closed = true
		tc.conn.WriteFrame(wire.FrameEnd, nil)
	}
	tc.wmu.Unlock()
	// Closing the worker connection kills only its own tmux attach client.
	time.AfterFunc(2*time.Second, func() { tc.conn.Close() })
}

func (c *wsClient) closeAll() {
	c.mu.Lock()
	chans := c.chans
	c.chans = map[uint16]*termChan{}
	c.sizes = map[uint16][2]int{}
	watches := c.watches
	c.watches = map[string]bool{}
	c.mu.Unlock()
	for _, tc := range chans {
		tc.close()
	}
	for k := range watches {
		c.d.unwatch(k, c)
	}
}

func (c *wsClient) handle(m wsMsg) {
	d := c.d
	switch m.T {
	case "subscribe":
		c.mu.Lock()
		c.agents = map[string]bool{}
		for _, a := range m.Agents {
			if d.canAccessUID(c, a) {
				c.agents[a] = true
			}
		}
		c.mu.Unlock()
		for _, a := range m.Agents {
			if d.canAccessUID(c, a) {
				if list := d.status.cached(a); list != nil {
					c.sendJSON(map[string]any{"t": "sessions", "agent": a, "list": list})
				}
				go d.status.refresh(a, false)
			}
		}
	case "focus":
		c.mu.Lock()
		c.focus = map[string]bool{}
		for _, f := range m.Focus {
			c.focus[f] = true
		}
		c.visible = m.Visible
		c.mu.Unlock()
	case "open":
		c.mu.Lock()
		delete(c.sizes, m.Ch)
		c.mu.Unlock()
		go c.openTerm(m)
	case "resize":
		if m.Cols <= 0 || m.Rows <= 0 {
			return
		}
		// Attaching takes a while; a resize sent right after open (the client fits
		// its terminal) must not be lost, or the window keeps the size from open.
		c.mu.Lock()
		tc := c.chans[m.Ch]
		if tc == nil {
			c.sizes[m.Ch] = [2]int{m.Cols, m.Rows}
		}
		c.mu.Unlock()
		if tc != nil {
			tc.write(wire.FrameResize, wire.ResizePayload(m.Cols, m.Rows))
		}
	case "close":
		c.mu.Lock()
		tc := c.chans[m.Ch]
		delete(c.chans, m.Ch)
		delete(c.sizes, m.Ch)
		c.mu.Unlock()
		if tc != nil {
			tc.close()
		}
	case "watch":
		if !d.canAccessUID(c, m.Agent) {
			return
		}
		k := key(m.Agent, m.Folder)
		c.mu.Lock()
		already := c.watches[k]
		c.watches[k] = true
		c.mu.Unlock()
		if !already {
			d.watch(m.Agent, m.Folder, c)
		}
	case "unwatch":
		k := key(m.Agent, m.Folder)
		c.mu.Lock()
		had := c.watches[k]
		delete(c.watches, k)
		c.mu.Unlock()
		if had {
			d.unwatch(k, c)
		}
	}
}

func (c *wsClient) openTerm(m wsMsg) {
	d := c.d
	fail := func(msg string) { c.sendJSON(map[string]any{"t": "closed", "ch": m.Ch, "error": msg}) }
	if !d.canAccessUID(c, m.Agent) {
		fail("access denied")
		return
	}
	a, _ := d.Agent(m.Agent)
	author := d.sessionAuthor(m.Agent, m.Session)
	prof := d.profileFor(author)
	var er worker.EnsureResult
	since := time.Now()
	if err := d.call(a, "ensure", worker.EnsureArgs{Name: m.Session, Profile: prof, Cols: m.Cols, Rows: m.Rows}, &er); err != nil {
		fail(err.Error())
		return
	}
	if er.Started {
		d.status.starting(m.Agent, m.Session, since)
		go d.status.refresh(m.Agent, true)
	}
	conn, err := d.stream(a, "attach", worker.AttachArgs{Name: m.Session, Cols: m.Cols, Rows: m.Rows, Profile: prof}, nil)
	if err != nil {
		fail(err.Error())
		return
	}
	tc := &termChan{conn: conn, agent: m.Agent, name: m.Session}
	c.mu.Lock()
	if old := c.chans[m.Ch]; old != nil {
		old.close()
	}
	c.chans[m.Ch] = tc
	size, resized := c.sizes[m.Ch]
	delete(c.sizes, m.Ch)
	c.mu.Unlock()
	if resized && (size[0] != m.Cols || size[1] != m.Rows) {
		tc.write(wire.FrameResize, wire.ResizePayload(size[0], size[1]))
	}
	c.sendJSON(map[string]any{"t": "opened", "ch": m.Ch, "agent": m.Agent, "session": m.Session})
	hdr := make([]byte, 2)
	binary.BigEndian.PutUint16(hdr, m.Ch)
	for {
		typ, p, err := conn.ReadFrame()
		if err != nil || typ == wire.FrameEnd {
			break
		}
		switch typ {
		case wire.FrameData:
			buf := make([]byte, 2+len(p))
			copy(buf, hdr)
			copy(buf[2:], p)
			c.send(websocket.MessageBinary, buf)
		case wire.FrameJSON:
			var ws worker.WinSize
			if json.Unmarshal(p, &ws) == nil && ws.Cols > 0 && ws.Rows > 0 {
				c.sendJSON(map[string]any{"t": "winsize", "ch": m.Ch, "cols": ws.Cols, "rows": ws.Rows})
			}
		}
	}
	c.mu.Lock()
	mine := c.chans[m.Ch] == tc
	if mine {
		delete(c.chans, m.Ch)
	}
	c.mu.Unlock()
	conn.Close()
	if mine {
		c.sendJSON(map[string]any{"t": "closed", "ch": m.Ch})
	}
}

// sessionAuthor returns the session author from the last list.
func (d *Daemon) sessionAuthor(agent, session string) string {
	for _, s := range d.status.cached(agent) {
		if s.Name == session {
			return s.Author
		}
	}
	return ""
}

// folderWatch is one worker folder watch shared by all subscribers.
type folderWatch struct {
	conn *wire.Conn
	subs map[*wsClient]bool
}

func (d *Daemon) watch(agent, folder string, c *wsClient) {
	k := key(agent, folder)
	d.watchMu.Lock()
	fw := d.watches[k]
	if fw != nil {
		fw.subs[c] = true
		d.watchMu.Unlock()
		return
	}
	fw = &folderWatch{subs: map[*wsClient]bool{c: true}}
	d.watches[k] = fw
	d.watchMu.Unlock()
	a, _ := d.Agent(agent)
	conn, err := d.stream(a, "watch", map[string]string{"folder": folder}, nil)
	if err != nil {
		d.watchMu.Lock()
		delete(d.watches, k)
		d.watchMu.Unlock()
		return
	}
	d.watchMu.Lock()
	fw.conn = conn
	d.watchMu.Unlock()
	go func() {
		defer conn.Close()
		for {
			typ, p, err := conn.ReadFrame()
			if err != nil {
				break
			}
			if typ != wire.FrameJSON {
				continue
			}
			var ev worker.WatchEvent
			if json.Unmarshal(p, &ev) != nil {
				continue
			}
			b, _ := json.Marshal(map[string]any{"t": "fs", "agent": agent, "folder": folder, "dirs": ev.Dirs, "files": ev.Files})
			d.watchMu.Lock()
			var subs []*wsClient
			for s := range fw.subs {
				subs = append(subs, s)
			}
			d.watchMu.Unlock()
			for _, s := range subs {
				if d.canAccessUID(s, agent) {
					s.send(websocket.MessageText, b)
				}
			}
		}
		d.watchMu.Lock()
		if d.watches[k] == fw {
			delete(d.watches, k)
		}
		d.watchMu.Unlock()
	}()
}

func (d *Daemon) unwatch(k string, c *wsClient) {
	d.watchMu.Lock()
	defer d.watchMu.Unlock()
	fw := d.watches[k]
	if fw == nil {
		return
	}
	delete(fw.subs, c)
	if len(fw.subs) == 0 {
		delete(d.watches, k)
		if fw.conn != nil {
			fw.conn.WriteFrame(wire.FrameEnd, nil)
			fw.conn.Close()
		}
	}
}
