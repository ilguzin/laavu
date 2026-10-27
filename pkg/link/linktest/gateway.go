// Package linktest is a console gateway stub for tests: it speaks the server
// side of docs/link.md and signs jobs with its own key. It is not the console.
package linktest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/ilguzin/laavu/pkg/link"
)

// Gateway accepts link connections.
type Gateway struct {
	Key ed25519.PrivateKey
	// AutoBind confirms every binding right after the pending code.
	AutoBind bool
	Log      func(format string, a ...any)

	mu      sync.Mutex
	servers map[string]*Server
	pending map[string]*Server // binding code → waiting server
	bound   map[string]bool    // server keys bound to this console
	codes   map[string]bool    // codes issued by Code()
}

// Server is one connected server as the gateway sees it.
type Server struct {
	Key     string
	Hello   link.Hello
	Auth    link.Auth
	Reports chan json.RawMessage
	Rights  chan map[string]bool
	Unlink  chan struct{}

	g       *Gateway
	ws      *websocket.Conn
	wmu     sync.Mutex
	rmu     sync.Mutex
	results map[string]chan link.Result
	last    json.RawMessage
	rights  map[string]bool
	bindNow chan struct{}
	done    chan struct{}
}

// New makes a gateway with a fresh key.
func New() *Gateway {
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	return &Gateway{Key: k, servers: map[string]*Server{}, pending: map[string]*Server{},
		bound: map[string]bool{}, codes: map[string]bool{}, Log: func(string, ...any) {}}
}

// PublicKey is the console key servers pin.
func (g *Gateway) PublicKey() string { return link.EncodeKey(g.Key.Public().(ed25519.PublicKey)) }

// Code issues a binding code the owner can pass to laavu link --code.
func (g *Gateway) Code() string {
	c := newCode()
	g.mu.Lock()
	g.codes[c] = true
	g.mu.Unlock()
	return c
}

func newCode() string {
	const abc = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = abc[int(b[i])%len(abc)]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

// ServeHTTP is the /v1 endpoint.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(link.MaxMessage)
	if err := g.serve(r.Context(), ws); err != nil {
		g.Log("link: %v", err)
	}
}

func (g *Gateway) serve(ctx context.Context, ws *websocket.Conn) error {
	var m link.Msg
	if err := wsjson.Read(ctx, ws, &m); err != nil {
		return err
	}
	if m.Type != link.TypeHello {
		return errors.New("expected hello")
	}
	s := &Server{g: g, ws: ws, Reports: make(chan json.RawMessage, 16), Rights: make(chan map[string]bool, 16),
		Unlink: make(chan struct{}, 1), results: map[string]chan link.Result{}, bindNow: make(chan struct{}, 1), done: make(chan struct{})}
	defer close(s.done)
	if err := json.Unmarshal(m.Data, &s.Hello); err != nil {
		return err
	}
	s.Key = s.Hello.ServerKey
	sk, err := link.ParseKey(s.Key)
	if err != nil {
		return err
	}
	nonce := link.Nonce()
	wm, _ := link.Sign(g.Key, link.TypeWelcome, link.Welcome{Contract: link.Contract, ServerKey: s.Key,
		ServerNonce: s.Hello.Nonce, ConsoleKey: g.PublicKey(), Nonce: nonce, Time: time.Now().Unix()})
	if err := s.send(ctx, wm); err != nil {
		return err
	}
	if err := wsjson.Read(ctx, ws, &m); err != nil {
		return err
	}
	if err := link.Open(sk, m, &s.Auth); err != nil || m.Type != link.TypeAuth {
		return fmt.Errorf("auth: %v", err)
	}
	if s.Auth.ConsoleNonce != nonce || s.Auth.ConsoleKey != g.PublicKey() {
		return errors.New("auth is not for this connection")
	}
	if s.Hello.Bind {
		return g.bind(ctx, s)
	}
	g.mu.Lock()
	if !g.bound[s.Key] {
		g.mu.Unlock()
		return errors.New("unknown server")
	}
	g.servers[s.Key] = s
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		if g.servers[s.Key] == s {
			delete(g.servers, s.Key)
		}
		g.mu.Unlock()
	}()
	for {
		var m link.Msg
		if err := wsjson.Read(ctx, ws, &m); err != nil {
			return err
		}
		switch m.Type {
		case link.TypeReport:
			s.rmu.Lock()
			s.last = m.Data
			s.rmu.Unlock()
			offer(s.Reports, m.Data)
		case link.TypeRights:
			var r map[string]bool
			json.Unmarshal(m.Data, &r)
			s.rmu.Lock()
			s.rights = r
			s.rmu.Unlock()
			offer(s.Rights, r)
		case link.TypeResult:
			var r link.Result
			json.Unmarshal(m.Data, &r)
			s.rmu.Lock()
			if c := s.results[r.ID]; c != nil {
				c <- r
			}
			s.rmu.Unlock()
		case link.TypeUnlink:
			g.mu.Lock()
			delete(g.bound, s.Key)
			g.mu.Unlock()
			g.Log("server %s unlinked", link.Fingerprint(mustKey(s.Key)))
			offer(s.Unlink, struct{}{})
		}
	}
}

// offer never blocks the reader: nobody has to drain the channels.
func offer[T any](c chan T, v T) {
	select {
	case c <- v:
	default:
	}
}

// Last returns the latest report and rights.
func (s *Server) Last() (json.RawMessage, map[string]bool) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.rights == nil {
		return s.last, s.Auth.Rights
	}
	return s.last, s.rights
}

func (g *Gateway) bind(ctx context.Context, s *Server) error {
	g.mu.Lock()
	issued := s.Hello.Code != "" && g.codes[s.Hello.Code]
	if issued {
		delete(g.codes, s.Hello.Code)
	}
	code := newCode()
	if !issued {
		g.pending[code] = s
	}
	g.mu.Unlock()
	if !issued {
		pm, _ := link.Plain(link.TypePending, link.Pending{Code: code, Expires: time.Now().Add(10 * time.Minute).Unix()})
		if err := s.send(ctx, pm); err != nil {
			return err
		}
		if !g.AutoBind {
			select {
			case <-s.bindNow:
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Minute):
				return errors.New("binding code expired")
			}
		}
	}
	bm, _ := link.Sign(g.Key, link.TypeBound, link.Bound{ServerKey: s.Key, Time: time.Now().Unix()})
	g.mu.Lock()
	g.bound[s.Key] = true
	delete(g.pending, code)
	g.mu.Unlock()
	if err := s.send(ctx, bm); err != nil {
		return err
	}
	g.Log("bound server %s", link.Fingerprint(mustKey(s.Key)))
	// Let the server read bound before the connection goes away.
	wsjson.Read(ctx, s.ws, &link.Msg{})
	return nil
}

func mustKey(s string) ed25519.PublicKey {
	k, _ := link.ParseKey(s)
	return k
}

// Confirm is the owner entering a pending code in the console.
func (g *Gateway) Confirm(code string) error {
	g.mu.Lock()
	s := g.pending[code]
	g.mu.Unlock()
	if s == nil {
		return errors.New("no such code")
	}
	s.bindNow <- struct{}{}
	return nil
}

// Bound marks a server key as bound without a binding round, for tests.
func (g *Gateway) Bound(serverKey string) {
	g.mu.Lock()
	g.bound[serverKey] = true
	g.mu.Unlock()
}

// Server returns a connected server, waiting up to timeout.
func (g *Gateway) Server(key string, timeout time.Duration) (*Server, error) {
	deadline := time.Now().Add(timeout)
	for {
		g.mu.Lock()
		s := g.servers[key]
		g.mu.Unlock()
		if s != nil {
			return s, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("server is not connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Servers lists connected server keys.
func (g *Gateway) Servers() []*Server {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []*Server
	for _, s := range g.servers {
		out = append(out, s)
	}
	return out
}

func (s *Server) send(ctx context.Context, m link.Msg) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return wsjson.Write(ctx, s.ws, m)
}

// NewJob fills a job for this server, valid for an hour.
func (s *Server) NewJob(op string, params any) link.Job {
	var raw json.RawMessage
	if params != nil {
		raw, _ = json.Marshal(params)
	}
	b := make([]byte, 8)
	rand.Read(b)
	now := time.Now().Unix()
	return link.Job{ID: fmt.Sprintf("%x", b), ServerKey: s.Key, Op: op, Params: raw, NotBefore: now, Expires: now + 3600}
}

// Run signs a job with the gateway key, sends it and waits for the result.
func (s *Server) Run(j link.Job, timeout time.Duration) (link.Result, error) {
	m, err := link.Sign(s.g.Key, link.TypeJob, j)
	if err != nil {
		return link.Result{}, err
	}
	return s.RunSigned(j.ID, m, timeout)
}

// RunSigned sends an already built message, for tests with bad signatures.
func (s *Server) RunSigned(id string, m link.Msg, timeout time.Duration) (link.Result, error) {
	c := make(chan link.Result, 1)
	s.rmu.Lock()
	s.results[id] = c
	s.rmu.Unlock()
	defer func() {
		s.rmu.Lock()
		delete(s.results, id)
		s.rmu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := s.send(ctx, m); err != nil {
		return link.Result{}, err
	}
	select {
	case r := <-c:
		return r, nil
	case <-s.done:
		return link.Result{}, errors.New("server disconnected")
	case <-ctx.Done():
		return link.Result{}, errors.New("no result")
	}
}
