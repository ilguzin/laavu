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

// Origin просмотра и origin dev-серверов. Всё здесь отдаётся без cookie
// приложения: HTML, который написал агент, не должен выполняться рядом с API.

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

// issue выдаёт токен на одну рабочую папку и только на чтение. Повторный запрос
// той же папки тем же пользователем продлевает существующий токен, чтобы
// ссылки внутри открытой страницы не ломались.
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

// AuxHandler различает origin по заголовку Host: просмотр или dev-сервер.
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
		http.Error(w, "неизвестный адрес", http.StatusNotFound)
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
	// HTML в iframe работает с opaque origin (без allow-same-origin), его fetch
	// к соседним файлам приходит с Origin: null. Доступ всё равно даёт токен в пути.
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "private, no-cache")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "только чтение", http.StatusMethodNotAllowed)
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
		http.Error(w, "ссылка просмотра устарела, откройте файл заново", http.StatusForbidden)
		return
	}
	// Пользователь мог потерять доступ к агенту за время жизни токена.
	if !d.Cfg.NoAuth {
		u, err := d.Store.UserByID(t.uid)
		if err != nil || !d.canAccess(u, t.agent) {
			http.Error(w, "нет доступа", http.StatusForbidden)
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
		http.Error(w, "нет такого файла", http.StatusNotFound)
		return
	}
	defer c.Close()
	if meta.Dir {
		http.Error(w, "это папка", http.StatusNotFound)
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

// --- Dev-серверы ---

// portLink — открытый порт агента: свой origin, cookie на время ссылки.
type portLink struct {
	agent    string
	port     int
	uid      int64
	host     string
	external int // внешний порт в режиме "ports"
	oneTime  string
	secret   string
	expires  time.Time
}

type portMap struct {
	cfg config.Daemon
	mu  sync.Mutex
	m   map[string]*portLink // host → ссылка
}

// portLinkTTL — сколько живёт открытый порт.
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

// open выделяет origin для порта агента.
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
		// Тот же порт агента тому же пользователю — тот же внешний порт.
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
			return nil, errors.New("все внешние порты для dev-серверов заняты, закройте старые")
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
		return nil, errf(http.StatusBadRequest, "порт вне диапазона агента %s", a.PortsEnv())
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
			http.Error(w, "ссылка уже использована или устарела — откройте порт из клиента заново", http.StatusForbidden)
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
		http.Error(w, "откройте порт кнопкой в клиенте Laavu", http.StatusForbidden)
		return
	}
	if !d.Cfg.NoAuth {
		u, err := d.Store.UserByID(l.uid)
		if err != nil || !d.canAccess(u, l.agent) {
			http.Error(w, "нет доступа", http.StatusForbidden)
			return
		}
	}
	a, _ := d.Agent(l.agent)
	// Порт из диапазона агента мог занять соседний агент: подключаемся, только
	// если слушает сам агент.
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
			// Порт входит в origin, но не в область cookie: браузер шлёт cookie
			// приложения и на dev-порты того же хоста. Ни cookie входа, ни cookie
			// прокси приложение агента видеть не должно — иначе агент украдёт сессию.
			removeCookie(pr.Out, portCookie)
			removeCookie(pr.Out, auth.CookieName)
		},
		ModifyResponse: func(resp *http.Response) error {
			// И не может их поставить или затереть.
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
			http.Error(w, "dev-сервер агента не отвечает на порту "+strconv.Itoa(l.port), http.StatusBadGateway)
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

// procNetFiles — откуда читается таблица сокетов; в тестах подменяется.
var procNetFiles = []string{"/proc/net/tcp", "/proc/net/tcp6"}

// checkPortOwner проверяет по /proc/net/tcp{,6}, что порт слушает пользователь uid.
// Эти таблицы видны любому пользователю и при hidepid.
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
				return fmt.Errorf("порт %d слушает чужой пользователь", port)
			}
		}
	}
	if !found {
		return fmt.Errorf("на порту %d никто не слушает", port)
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
