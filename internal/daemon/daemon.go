// Package daemon is laavud: sign-in, access, events, serving the client, the view
// origin and the dev-server proxy. It never touches agent files or tmux itself,
// only through their workers' sockets.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ilguzin/laavu/internal/auth"
	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/push"
	"github.com/ilguzin/laavu/internal/store"
	"github.com/ilguzin/laavu/internal/wire"
	"github.com/ilguzin/laavu/internal/worker"
)

// Daemon is a single laavud process.
type Daemon struct {
	Cfg    config.Daemon
	Store  *store.Store
	Auth   *auth.Auth
	Push   *push.Sender
	Static fs.FS
	Log    *slog.Logger

	agents map[string]*config.Agent
	byUID  map[int]*config.Agent
	order  []string

	origin     string // app origin that the Origin header is checked against
	viewOrigin string

	hub    *hub
	status *statuses
	views  *viewTokens
	ports  *portMap

	watchMu sync.Mutex
	watches map[string]*folderWatch

	hookSlots slots
}

// New builds the daemon.
func New(cfg config.Daemon, agents []config.Agent, st *store.Store, static fs.FS, log *slog.Logger) (*Daemon, error) {
	if log == nil {
		log = slog.Default()
	}
	d := &Daemon{
		Cfg: cfg, Store: st, Static: static, Log: log,
		agents:  map[string]*config.Agent{},
		byUID:   map[int]*config.Agent{},
		watches: map[string]*folderWatch{},
	}
	for i := range agents {
		a := &agents[i]
		d.agents[a.Name] = a
		d.byUID[a.UID] = a
		d.order = append(d.order, a.Name)
	}
	sort.Strings(d.order)
	o, err := originOf(cfg.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("public_url: %w", err)
	}
	d.origin = o
	if d.viewOrigin, err = originOf(cfg.ViewURL); err != nil {
		return nil, fmt.Errorf("view_url: %w", err)
	}
	if !cfg.NoAuth {
		if d.Auth, err = auth.New(st, cfg.PublicURL); err != nil {
			return nil, err
		}
	}
	if st != nil {
		if d.Push, err = push.New(st, cfg.PublicURL); err != nil {
			return nil, err
		}
	}
	d.hub = newHub(d)
	d.status = newStatuses(d)
	d.views = newViewTokens()
	d.ports = newPortMap(cfg)
	return d, nil
}

func originOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("need a URL like https://host[:port]")
	}
	return u.Scheme + "://" + u.Host, nil
}

// Listen opens "unix:/path" or "host:port".
func Listen(addr string, mode os.FileMode, group int) (net.Listener, error) {
	if p, ok := strings.CutPrefix(addr, "unix:"); ok {
		os.Remove(p)
		os.MkdirAll(filepath.Dir(p), 0o755)
		l, err := net.Listen("unix", p)
		if err != nil {
			return nil, err
		}
		if group >= 0 {
			os.Chown(p, -1, group)
		}
		os.Chmod(p, mode)
		return l, nil
	}
	return net.Listen("tcp", addr)
}

// Serve starts all listeners and blocks until the context is cancelled.
func (d *Daemon) Serve(ctx context.Context, app net.Listener, aux []net.Listener, hooks net.Listener) error {
	srv := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	auxSrv := &http.Server{Handler: d.AuxHandler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2+len(aux))
	go func() { errc <- srv.Serve(app) }()
	for _, l := range aux {
		go func() { errc <- auxSrv.Serve(l) }()
	}
	go func() { errc <- d.serveHooks(hooks) }()
	go d.status.loop(ctx)
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if d.Store != nil {
					d.Store.CleanupExpired()
				}
			}
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			d.Log.Error("listener failed", "err", err)
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	auxSrv.Shutdown(sctx)
	hooks.Close()
	return nil
}

// Agent returns an agent by name.
func (d *Daemon) Agent(name string) (*config.Agent, bool) {
	a, ok := d.agents[name]
	return a, ok
}

// call sends a request to the agent worker.
func (d *Daemon) call(a *config.Agent, op string, args, out any) error {
	return d.callFor(a, op, args, out, 30*time.Second)
}

// callFor is call with its own deadline, for operations that may take long (clone).
func (d *Daemon) callFor(a *config.Agent, op string, args, out any, timeout time.Duration) error {
	c, err := wire.Dial(a.WorkerSocket)
	if err != nil {
		return fmt.Errorf("agent %s worker unavailable: %w", a.Name, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	return c.Send(op, args, out)
}

// stream sends a streaming request: the connection stays open after the reply.
func (d *Daemon) stream(a *config.Agent, op string, args, out any) (*wire.Conn, error) {
	c, err := wire.Dial(a.WorkerSocket)
	if err != nil {
		return nil, fmt.Errorf("agent %s worker unavailable: %w", a.Name, err)
	}
	// Reads and uploads must not hang forever (FIFO, stuck worker);
	// the terminal and folder watches live as long as the channel is open.
	if op == "read" || op == "upload" {
		c.SetDeadline(time.Now().Add(10 * time.Minute))
	}
	if err := c.Send(op, args, out); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// canAccess reports user access to an agent: an admin sees all of them.
func (d *Daemon) canAccess(u *store.User, agent string) bool {
	if _, ok := d.agents[agent]; !ok || u == nil {
		return false
	}
	if u.Admin() {
		return true
	}
	ok, err := d.Store.HasAccess(u.ID, agent)
	return err == nil && ok
}

// usersWithAccess returns who receives the agent's events.
func (d *Daemon) usersWithAccess(agent string) map[int64]bool {
	out := map[int64]bool{}
	if d.Cfg.NoAuth || d.Store == nil {
		out[0] = true
		return out
	}
	ids, _ := d.Store.UsersWithAccess(agent)
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// profileFor returns the session author's profile: --append-system-prompt and --settings.
func (d *Daemon) profileFor(author string) worker.Profile {
	var p worker.Profile
	if author == "" || strings.ContainsAny(author, "/\\") || strings.HasPrefix(author, ".") {
		return p
	}
	dir := filepath.Join(d.Cfg.StateDir, "profiles")
	if b, err := os.ReadFile(filepath.Join(dir, author+".json")); err == nil {
		var f struct {
			AppendPrompt string `json:"append_prompt"`
		}
		if json.Unmarshal(b, &f) == nil {
			p.AppendPrompt = f.AppendPrompt
		}
	}
	if s := filepath.Join(dir, author+".settings.json"); fileExists(s) {
		p.Settings = s
	}
	return p
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
