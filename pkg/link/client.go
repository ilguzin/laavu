package link

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Backend is what the link process asks of the root helper.
type Backend interface {
	Job(m Msg) (Result, error)
	Report() (json.RawMessage, error)
}

// Client holds the connection to the console. It runs as laavu-link.
type Client struct {
	URL        string
	Key        ed25519.PrivateKey
	ConsoleKey ed25519.PublicKey // nil while binding: taken from the console over TLS
	Code       string            // binding code issued by the console, optional
	Laavud     string
	Laavu      string
	Backend    Backend
	// Config returns the current binding; the link process polls it for rights and unlink.
	Config      func() (*Config, error)
	ReportEvery time.Duration
	PollEvery   time.Duration
	// OnPending shows the binding code to the owner.
	OnPending func(Pending)
	// OnBound gets the confirmed binding, with the console account it went to.
	OnBound func(Bound)
	// OnStatus records the connection state.
	OnStatus func(Status)
	Now      func() time.Time
}

// Status is written to StatusFile for laavu link status.
type Status struct {
	Connected bool   `json:"connected"`
	Since     int64  `json:"since"`
	Error     string `json:"error,omitempty"`
	Unlinked  bool   `json:"unlinked,omitempty"`
}

// ErrUnlinked ends Run after the console was told the owner took control back.
var ErrUnlinked = errors.New("unlinked")

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) status(s Status) {
	if c.OnStatus != nil {
		s.Since = c.now().Unix()
		c.OnStatus(s)
	}
}

// Run keeps the connection up until ctx ends or the server is unlinked.
func (c *Client) Run(ctx context.Context) error {
	pause := 2 * time.Second
	for {
		start := time.Now()
		err := c.session(ctx, false)
		if errors.Is(err, ErrUnlinked) {
			c.status(Status{Unlinked: true})
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.status(Status{Error: errString(err)})
		if time.Since(start) > time.Minute {
			pause = 2 * time.Second
		}
		// Jitter keeps a fleet of servers from reconnecting in step after a console restart.
		d := pause + time.Duration(rand.Int64N(int64(pause/5)+1))
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
		pause = min(pause*2, 5*time.Minute)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Bind connects once in binding mode and returns the console key after the
// owner confirmed the binding in the console.
func (c *Client) Bind(ctx context.Context) (ed25519.PublicKey, error) {
	err := c.session(ctx, true)
	var b boundErr
	if errors.As(err, &b) {
		return b.key, nil
	}
	if err == nil {
		err = errors.New("console closed the connection before the binding was confirmed")
	}
	return nil, err
}

type boundErr struct{ key ed25519.PublicKey }

func (boundErr) Error() string { return "bound" }

type conn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (k *conn) send(ctx context.Context, m Msg) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return wsjson.Write(wctx, k.ws, m)
}

func (c *Client) session(ctx context.Context, binding bool) error {
	if err := CheckURL(c.URL); err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ws, _, err := websocket.Dial(dctx, c.URL, nil)
	cancel()
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(MaxMessage)
	k := &conn{ws: ws}
	pub := c.Key.Public().(ed25519.PublicKey)
	nonce := Nonce()
	hello, _ := Plain(TypeHello, Hello{Contracts: []int{Contract}, ServerKey: EncodeKey(pub), Nonce: nonce,
		Laavu: c.Laavu, Bind: binding, Code: c.Code})
	if err := k.send(ctx, hello); err != nil {
		return err
	}
	read := func(rctx context.Context) (Msg, error) {
		var m Msg
		err := wsjson.Read(rctx, ws, &m)
		if err == nil && m.Type == TypeError {
			var e struct {
				Message string `json:"message"`
			}
			json.Unmarshal(m.Data, &e)
			return m, fmt.Errorf("console: %s", e.Message)
		}
		return m, err
	}
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	m, err := read(hctx)
	cancel()
	if err != nil {
		return err
	}
	ck, err := c.checkWelcome(m, pub, nonce)
	if err != nil {
		k.send(ctx, errMsg(err.Error()))
		ws.Close(websocket.StatusPolicyViolation, "bad welcome")
		return err
	}
	var w Welcome
	Peek(m, &w)
	auth := Auth{ConsoleKey: EncodeKey(ck), ConsoleNonce: w.Nonce, Laavud: c.Laavud, Laavu: c.Laavu}
	if !binding {
		if cfg, err := c.Config(); err == nil {
			auth.Rights = cfg.Rights
		}
	}
	am, _ := Sign(c.Key, TypeAuth, auth)
	if err := k.send(ctx, am); err != nil {
		return err
	}
	if binding {
		return c.waitBound(ctx, read, ck, pub)
	}
	c.status(Status{Connected: true})
	return c.serve(ctx, k, read, auth.Rights)
}

func errMsg(s string) Msg {
	m, _ := Plain(TypeError, map[string]string{"message": s})
	return m
}

func (c *Client) checkWelcome(m Msg, pub ed25519.PublicKey, nonce string) (ed25519.PublicKey, error) {
	if m.Type != TypeWelcome {
		return nil, fmt.Errorf("expected welcome, got %q", m.Type)
	}
	var w Welcome
	if err := Peek(m, &w); err != nil {
		return nil, err
	}
	ck, err := ParseKey(w.ConsoleKey)
	if err != nil {
		return nil, err
	}
	if c.ConsoleKey != nil && !ck.Equal(c.ConsoleKey) {
		return nil, errors.New("console key differs from the pinned one")
	}
	if err := Open(ck, m, &w); err != nil {
		return nil, fmt.Errorf("welcome: %w", err)
	}
	if w.Contract != Contract {
		return nil, fmt.Errorf("console offers contract %d, this server speaks %d", w.Contract, Contract)
	}
	if w.ServerKey != EncodeKey(pub) || w.ServerNonce != nonce {
		return nil, errors.New("welcome is not for this connection")
	}
	if err := CheckTime(w.Time, c.now()); err != nil {
		return nil, err
	}
	return ck, nil
}

func (c *Client) waitBound(ctx context.Context, read func(context.Context) (Msg, error), ck, pub ed25519.PublicKey) error {
	for {
		m, err := read(ctx)
		if err != nil {
			return err
		}
		switch m.Type {
		case TypePending:
			var p Pending
			if json.Unmarshal(m.Data, &p) == nil && c.OnPending != nil {
				c.OnPending(p)
			}
		case TypeBound:
			var b Bound
			if err := Open(ck, m, &b); err != nil {
				return fmt.Errorf("bound: %w", err)
			}
			if b.ServerKey != EncodeKey(pub) {
				return errors.New("bound is not for this server")
			}
			if err := CheckTime(b.Time, c.now()); err != nil {
				return err
			}
			if c.OnBound != nil {
				c.OnBound(b)
			}
			return boundErr{ck}
		}
	}
}

func (c *Client) serve(ctx context.Context, k *conn, read func(context.Context) (Msg, error), rights map[string]bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan Msg, 64)
	errc := make(chan error, 2)
	go func() {
		for {
			m, err := read(ctx)
			if err != nil {
				errc <- err
				return
			}
			if m.Type == TypeJob {
				select {
				case jobs <- m:
				default:
					// The queue is full; the console sees no result and may retry with a new id.
				}
			}
		}
	}()
	// Jobs run one by one, in the order they came; the helper serialises them anyway.
	go func() {
		for {
			select {
			case m := <-jobs:
				res, err := c.Backend.Job(m)
				if err != nil {
					res = Result{Status: StatusFailed, Code: -1, Output: "helper: " + err.Error()}
					var j Job
					if Peek(m, &j) == nil {
						res.ID = j.ID
					}
				}
				rm, _ := Plain(TypeResult, res)
				if err := k.send(ctx, rm); err != nil {
					errc <- err
					return
				}
				if res.Status == StatusDone && isUnlink(m) {
					errc <- ErrUnlinked
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	sendReport := func() error {
		rep, err := c.Backend.Report()
		if err != nil || rep == nil {
			return nil // the helper may be restarting, or monitoring is off
		}
		return k.send(ctx, Msg{Type: TypeReport, Data: rep})
	}
	if err := sendReport(); err != nil {
		return err
	}
	report := time.NewTicker(c.ReportEvery)
	defer report.Stop()
	poll := time.NewTicker(c.PollEvery)
	defer poll.Stop()
	ping := time.NewTicker(time.Minute)
	defer ping.Stop()
	for {
		select {
		case err := <-errc:
			if errors.Is(err, ErrUnlinked) {
				k.ws.Close(websocket.StatusNormalClosure, "unlinked")
			}
			return err
		case <-report.C:
			if err := sendReport(); err != nil {
				return err
			}
		case <-poll.C:
			cfg, err := c.Config()
			if err != nil {
				continue
			}
			if cfg.Unlinking {
				um, _ := Plain(TypeUnlink, struct{}{})
				k.send(ctx, um)
				k.ws.Close(websocket.StatusNormalClosure, "unlinked")
				return ErrUnlinked
			}
			if !reflect.DeepEqual(cfg.Rights, rights) {
				rights = cfg.Rights
				rm, _ := Plain(TypeRights, rights)
				if err := k.send(ctx, rm); err != nil {
					return err
				}
			}
		case <-ping.C:
			pctx, pc := context.WithTimeout(ctx, 30*time.Second)
			err := k.ws.Ping(pctx)
			pc()
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func isUnlink(m Msg) bool {
	var j Job
	return Peek(m, &j) == nil && j.Op == OpUnlink
}

// SocketBackend talks to the root helper over its unix socket.
type SocketBackend struct {
	Path string
}

func (s *SocketBackend) call(req HelperRequest, timeout time.Duration) (*HelperResponse, error) {
	c, err := net.DialTimeout("unix", s.Path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReaderSize(c, MaxMessage).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp HelperResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return &resp, nil
}

func (s *SocketBackend) Job(m Msg) (Result, error) {
	// apt upgrade on a slow VPS can take a while.
	resp, err := s.call(HelperRequest{Op: "job", Job: &m}, 2*time.Hour)
	if err != nil {
		return Result{}, err
	}
	if resp.Result == nil {
		return Result{}, errors.New("empty result")
	}
	return *resp.Result, nil
}

func (s *SocketBackend) Report() (json.RawMessage, error) {
	resp, err := s.call(HelperRequest{Op: "report"}, 2*time.Minute)
	if err != nil || resp.Report == nil {
		return nil, err
	}
	return json.Marshal(resp.Report)
}

// WriteStatus is the default OnStatus for the service.
func WriteStatus(s Status) {
	b, _ := json.Marshal(s)
	tmp := StatusFile + ".new"
	os.MkdirAll(filepath.Dir(StatusFile), 0o755)
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, StatusFile)
	}
}
