package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ilguzin/laavu/internal/auth"
	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/store"
	"github.com/ilguzin/laavu/internal/wire"
	"github.com/ilguzin/laavu/internal/worker"
)

// The view origin and dev-server origins. Everything here is served without the
// app cookie: HTML written by an agent must not run next to the API.

const viewTTL = time.Hour

type viewToken struct {
	agent, folder string
	uid           int64
	expires       time.Time
}

type viewTokens struct {
	mu sync.Mutex
	m  map[string]*viewToken
}

func newViewTokens() *viewTokens { return &viewTokens{m: map[string]*viewToken{}} }

// issue grants a read-only token for one folder. A repeated request for the
// same folder by the same user extends the existing token, so that links
// inside an open page keep working.
func (v *viewTokens) issue(agent, folder string, uid int64) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	for tok, t := range v.m {
		if t.expires.Before(now) {
			delete(v.m, tok)
			continue
		}
		if t.agent == agent && t.folder == folder && t.uid == uid {
			t.expires = now.Add(viewTTL)
			return tok
		}
	}
	tok := store.RandomToken()
	v.m[tok] = &viewToken{agent: agent, folder: folder, uid: uid, expires: now.Add(viewTTL)}
	return tok
}

func (v *viewTokens) get(tok string) (*viewToken, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	t, ok := v.m[tok]
	if !ok || t.expires.Before(time.Now()) {
		return nil, false
	}
	return t, true
}

func (d *Daemon) apiViewToken(w http.ResponseWriter, r *http.Request) (any, error) {
	a, f, err := d.folderFor(r)
	if err != nil {
		return nil, err
	}
	tok := d.views.issue(a.Name, f, userOf(r).ID)
	return map[string]string{"base": strings.TrimRight(d.Cfg.ViewURL, "/") + "/t/" + tok + "/"}, nil
}

// AuxHandler tells origins apart by the Host header: view or dev server.
func (d *Daemon) AuxHandler() http.Handler {
	viewHost := hostOf(d.Cfg.ViewURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Host, viewHost) {
			d.serveView(w, r)
			return
		}
		if m := d.ports.byHost(r.Host); m != nil {
			d.serveDevPort(w, r, m)
			return
		}
		http.Error(w, "unknown address", http.StatusNotFound)
	})
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

func (d *Daemon) serveView(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	// HTML in the iframe runs with an opaque origin (no allow-same-origin), so its
	// fetches of sibling files arrive with Origin: null. Access is granted by the token in the path anyway.
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "private, no-cache")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "read only", http.StatusMethodNotAllowed)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/t/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	tok, rel, _ := strings.Cut(rest, "/")
	t, ok := d.views.get(tok)
	if !ok {
		http.Error(w, "view link expired, open the file again", http.StatusForbidden)
		return
	}
	// The user may have lost access to the agent during the token's lifetime.
	if !d.Cfg.NoAuth {
		u, err := d.Store.UserByID(t.uid)
		if err != nil || !d.canAccess(u, t.agent) {
			http.Error(w, "access denied", http.StatusForbidden)
			return
		}
	}
	a, _ := d.Agent(t.agent)
	if rel == "" {
		rel = "index.html"
	}
	var meta worker.FileMeta
	c, err := d.stream(a, "read", map[string]any{"folder": t.folder, "path": rel, "visible": true}, &meta)
	if err != nil {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	defer c.Close()
	if meta.Dir {
		http.Error(w, "this is a folder", http.StatusNotFound)
		return
	}
	ct := mime.TypeByExtension(strings.ToLower(path.Ext(meta.Path)))
	switch {
	case ct != "":
	case meta.Kind == "binary":
		ct = "application/octet-stream"
	default:
		ct = "text/plain; charset=utf-8"
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	if r.Method == http.MethodHead {
		return
	}
	for {
		typ, p, err := c.ReadFrame()
		if err != nil || typ == wire.FrameEnd {
			return
		}
		if _, err := w.Write(p); err != nil {
			return
		}
	}
}

// --- Dev servers ---

// portLink is an opened agent port: its own origin, a cookie for the link's lifetime.
type portLink struct {
	agent    string
	port     int
	uid      int64
	host     string
	external int // external port in "ports" mode
	oneTime  string
	secret   string
	expires  time.Time
}

type portMap struct {
	cfg config.Daemon
	mu  sync.Mutex
	m   map[string]*portLink // host -> link
}

// portLinkTTL is how long an opened port lives.
const portLinkTTL = 12 * time.Hour

func newPortMap(cfg config.Daemon) *portMap { return &portMap{cfg: cfg, m: map[string]*portLink{}} }

func (p *portMap) byHost(host string) *portLink {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := p.m[strings.ToLower(host)]
	if l == nil || l.expires.Before(time.Now()) {
		return nil
	}
	return l
}

// open allocates an origin for an agent port.
func (p *portMap) open(agent string, port int, uid int64) (*portLink, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for h, l := range p.m {
		if l.expires.Before(now) {
			delete(p.m, h)
		}
	}
	l := &portLink{agent: agent, port: port, uid: uid, oneTime: store.RandomToken(), secret: store.RandomToken(), expires: now.Add(portLinkTTL)}
	switch p.cfg.DevMode {
	case "subdomain":
		l.host = fmt.Sprintf("p%d-%s.%s", port, agent, p.cfg.DevDomain)
	default:
		base := hostOf(p.cfg.PublicURL)
		hostname := base
		if h, _, err := net.SplitHostPort(base); err == nil {
			hostname = h
		}
		// Same agent port for the same user gets the same external port.
		for _, old := range p.m {
			if old.agent == agent && old.port == port && old.uid == uid {
				old.oneTime, old.expires = l.oneTime, l.expires
				return old, nil
			}
		}
		for ext := p.cfg.DevPorts[0]; ext <= p.cfg.DevPorts[1] && ext > 0; ext++ {
			h := net.JoinHostPort(hostname, strconv.Itoa(ext))
			if _, used := p.m[h]; !used {
				l.host, l.external = h, ext
				break
			}
		}
		if l.host == "" {
			return nil, errors.New("all external dev-server ports are in use, close old ones")
		}
	}
	p.m[strings.ToLower(l.host)] = l
	return l, nil
}

func (d *Daemon) apiOpenPort(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < a.Ports[0] || port > a.Ports[1] {
		return nil, errf(http.StatusBadRequest, "port outside the agent range %s", a.PortsEnv())
	}
	if err := checkPortOwner(port, a.UID); err != nil {
		return nil, err
	}
	l, err := d.ports.open(a.Name, port, userOf(r).ID)
	if err != nil {
		return nil, err
	}
	scheme := "https"
	if u, err := url.Parse(d.Cfg.PublicURL); err == nil {
		scheme = u.Scheme
	}
	return map[string]string{"url": scheme + "://" + l.host + "/__laavu/auth?t=" + l.oneTime}, nil
}

const portCookie = "laavu-port"

func (d *Daemon) serveDevPort(w http.ResponseWriter, r *http.Request, l *portLink) {
	if r.URL.Path == "/__laavu/auth" {
		d.ports.mu.Lock()
		ok := l.oneTime != "" && r.URL.Query().Get("t") == l.oneTime
		if ok {
			l.oneTime = ""
		}
		d.ports.mu.Unlock()
		if !ok {
			http.Error(w, "link already used or expired, open the port from the client again", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: portCookie, Value: l.secret, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(d.Cfg.PublicURL, "https"),
			SameSite: http.SameSiteLaxMode, Expires: l.expires,
		})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	c, err := r.Cookie(portCookie)
	if err != nil || c.Value != l.secret {
		http.Error(w, "open the port with the button in the Laavu client", http.StatusForbidden)
		return
	}
	if !d.Cfg.NoAuth {
		u, err := d.Store.UserByID(l.uid)
		if err != nil || !d.canAccess(u, l.agent) {
			http.Error(w, "access denied", http.StatusForbidden)
			return
		}
	}
	a, _ := d.Agent(l.agent)
	// A port in the agent range may be taken by a neighbouring agent: connect only
	// if the agent itself is listening.
	if err := checkPortOwner(l.port, a.UID); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(l.port)}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			// The port is part of the origin but not of the cookie scope: the browser
			// sends app cookies to dev ports on the same host too. The agent's app must
			// see neither the sign-in cookie nor the proxy cookie, or it could steal the session.
			removeCookie(pr.Out, portCookie)
			removeCookie(pr.Out, auth.CookieName)
		},
		ModifyResponse: func(resp *http.Response) error {
			// Nor can it set or overwrite them.
			var keep []string
			for _, sc := range resp.Header.Values("Set-Cookie") {
				name, _, _ := strings.Cut(strings.TrimSpace(sc), "=")
				if name == portCookie || strings.HasPrefix(name, "__Host-") || strings.HasPrefix(name, "__Secure-laavu") {
					continue
				}
				keep = append(keep, sc)
			}
			resp.Header.Del("Set-Cookie")
			for _, sc := range keep {
				resp.Header.Add("Set-Cookie", sc)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "agent dev server is not responding on port "+strconv.Itoa(l.port), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

func removeCookie(r *http.Request, name string) {
	cs := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cs {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
}

// procNetFiles is where the socket table is read from; replaced in tests.
var procNetFiles = []string{"/proc/net/tcp", "/proc/net/tcp6"}

// checkPortOwner checks via /proc/net/tcp{,6} that the port is listened on by user uid.
// These tables are visible to any user even with hidepid.
func checkPortOwner(port, uid int) error {
	found := false
	for _, f := range procNetFiles {
		owners, err := listenOwners(f, port)
		if err != nil {
			continue
		}
		for _, o := range owners {
			found = true
			if o != uid {
				return fmt.Errorf("port %d is listened on by another user", port)
			}
		}
	}
	if !found {
		return fmt.Errorf("nothing is listening on port %d", port)
	}
	return nil
}

func listenOwners(file string, port int) ([]int, error) {
	fh, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var out []int
	sc := bufio.NewScanner(fh)
	sc.Scan()
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[3] != "0A" { // 0A — LISTEN
			continue
		}
		_, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		p, err := strconv.ParseInt(hexPort, 16, 32)
		if err != nil || int(p) != port {
			continue
		}
		u, err := strconv.Atoi(f[7])
		if err == nil {
			out = append(out, u)
		}
	}
	return out, sc.Err()
}
