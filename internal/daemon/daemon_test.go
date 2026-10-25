package daemon

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ilguzin/laavu/deploy"
	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/push"
	"github.com/ilguzin/laavu/internal/store"
	"github.com/ilguzin/laavu/internal/worker"
)

var binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "laavu-bin-")
	if err != nil {
		panic(err)
	}
	binDir = dir
	for _, p := range []string{"cmd/laavud", "test/fakeclaude"} {
		out, err := exec.Command("go", "build", "-o", filepath.Join(dir, filepath.Base(p)), "../../"+p).CombinedOutput()
		if err != nil {
			panic(string(out))
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t      *testing.T
	d      *Daemon
	srv    *httptest.Server
	aux    *httptest.Server
	home   string
	agent  config.Agent
	cancel context.CancelFunc
}

func setup(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	run := t.TempDir()
	laavud := filepath.Join(binDir, "laavud")
	conf, _ := deploy.Render("tmux.conf.tmpl", map[string]string{"Laavud": laavud})
	os.WriteFile(filepath.Join(run, "tmux.conf"), conf, 0o644)
	settings, _ := deploy.Render("agent/settings.json.tmpl", map[string]string{"Laavud": laavud})
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), settings, 0o600)

	me, _ := user.Current()
	uid, _ := strconv.Atoi(me.Uid)
	ag := config.Agent{Name: "demo", User: me.Username, Home: home, Ports: [2]int{20000, 20999},
		Claude: filepath.Join(binDir, "fakeclaude"), TmuxSocket: fmt.Sprintf("laavu-dt-%d", time.Now().UnixNano()),
		WorkerSocket: filepath.Join(run, "w.sock"), UID: uid}
	cfg := config.Defaults()
	cfg.RunDir = run
	cfg.StateDir = t.TempDir()
	cfg.NoAuth = true
	cfg.PushDelay.Duration = 200 * time.Millisecond
	os.MkdirAll(filepath.Join(run, "hooks"), 0o700)

	w, err := worker.New(worker.Options{Agent: ag, TmuxConf: filepath.Join(run, "tmux.conf"), HookSocket: cfg.HookSocket(), LaavudBin: laavud})
	if err != nil {
		t.Fatal(err)
	}
	// Хуки claude и tmux должны видеть HOME агента и сокет хуков.
	w.Tmux.Env = append(w.Tmux.Env, "HOME="+home)
	wl, err := worker.Listener(ag.WorkerSocket)
	if err != nil {
		t.Fatal(err)
	}
	go w.Serve(wl)

	srv := httptest.NewUnstartedServer(nil)
	aux := httptest.NewUnstartedServer(nil)
	cfg.PublicURL = "http://" + srv.Listener.Addr().String()
	cfg.ViewURL = "http://" + aux.Listener.Addr().String()
	d, err := New(cfg, []config.Agent{ag}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = d.Handler()
	aux.Config.Handler = d.AuxHandler()
	srv.Start()
	aux.Start()
	hl, err := Listen("unix:"+cfg.HookSocket(), 0o600, -1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go d.serveHooks(hl)
	go d.status.loop(ctx)
	t.Cleanup(func() {
		cancel()
		hl.Close()
		srv.Close()
		aux.Close()
		wl.Close()
		exec.Command("tmux", "-L", ag.TmuxSocket, "kill-server").Run()
	})
	return &env{t: t, d: d, srv: srv, aux: aux, home: home, agent: ag, cancel: cancel}
}

func (e *env) req(method, path string, body any, out any) int {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, e.srv.URL+path, rd)
	r.Header.Set("Origin", e.d.origin)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		json.Unmarshal(b, out)
	}
	if resp.StatusCode != 200 && out == nil {
		e.t.Logf("%s %s → %d %s", method, path, resp.StatusCode, b)
	}
	return resp.StatusCode
}

func (e *env) sessions() map[string]worker.SessionInfo {
	var list []worker.SessionInfo
	e.req("GET", "/api/agents/demo/sessions", nil, &list)
	m := map[string]worker.SessionInfo{}
	for _, s := range list {
		m[s.Name] = s
	}
	return m
}

func (e *env) history(name string) string {
	var h map[string]string
	e.req("GET", "/api/agents/demo/sessions/"+name+"/history", nil, &h)
	return h["text"]
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("не дождались: %s", what)
}

func TestClaudeSessionLifecycle(t *testing.T) {
	e := setup(t)
	var si worker.SessionInfo
	if c := e.req("POST", "/api/agents/demo/sessions", map[string]any{"folder": "Demo", "kind": "claude", "create": true}, &si); c != 200 {
		t.Fatal("новая сессия", c)
	}
	if si.Name != "Demo-1" {
		t.Fatalf("имя: %+v", si)
	}
	// SessionStart → «ждёт ответа» и UUID разговора в опции tmux.
	var first string
	eventually(t, "SessionStart", func() bool {
		s := e.sessions()["Demo-1"]
		first = s.Claude
		return s.Status == StWaiting && s.Claude != ""
	})
	if s := e.sessions()["Demo-1"]; s.Author != "dev" || s.Folder != "Demo" || s.Kind != "claude" {
		t.Fatalf("сессия: %+v", s)
	}
	// Сообщение из поля ввода, многострочное.
	if c := e.req("POST", "/api/agents/demo/sessions/Demo-1/input", map[string]any{"text": "первая строка\nвторая строка"}, nil); c != 200 {
		t.Fatal("ввод", c)
	}
	eventually(t, "ответ", func() bool {
		return strings.Contains(e.history("Demo-1"), "echo: первая строка / вторая строка")
	})
	var cu worker.ContextUsage
	eventually(t, "контекст", func() bool {
		return e.req("GET", "/api/agents/demo/sessions/Demo-1/context", nil, &cu) == 200 && cu.Used > 20000
	})
	// /clear меняет идентификатор разговора.
	e.req("POST", "/api/agents/demo/sessions/Demo-1/input", map[string]any{"text": "/clear"}, nil)
	eventually(t, "новый UUID после /clear", func() bool {
		s := e.sessions()["Demo-1"]
		return s.Claude != "" && s.Claude != first
	})
	second := e.sessions()["Demo-1"].Claude

	// Перезагрузка: tmux умер, сессия спит; открытие канала поднимает её с --resume.
	exec.Command("tmux", "-L", e.agent.TmuxSocket, "kill-server").Run()
	eventually(t, "спит", func() bool { return e.sessions()["Demo-1"].Status == StSleeping })
	ws := e.dialWS()
	defer ws.CloseNow()
	ws.Write(context.Background(), websocket.MessageText, []byte(`{"t":"open","ch":1,"agent":"demo","session":"Demo-1","cols":100,"rows":30}`))
	got := e.readTerm(ws, 1, "(resume)")
	if !strings.Contains(got, second) {
		t.Fatalf("поднялся не тот разговор: %q", got)
	}
	// Ввод прямо в терминал.
	frame := append([]byte{0, 1}, []byte("из терминала\r")...)
	ws.Write(context.Background(), websocket.MessageBinary, frame)
	e.readTerm(ws, 1, "echo: из терминала")
}

func (e *env) dialWS() *websocket.Conn {
	e.t.Helper()
	u := strings.Replace(e.srv.URL, "http", "ws", 1) + "/api/ws"
	c, _, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {e.d.origin}}})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) readTerm(c *websocket.Conn, ch uint16, want string) string {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var b strings.Builder
	for !strings.Contains(b.String(), want) {
		typ, data, err := c.Read(ctx)
		if err != nil {
			e.t.Fatalf("ждали %q, получили %q: %v", want, b.String(), err)
		}
		if typ == websocket.MessageBinary && len(data) >= 2 && binary.BigEndian.Uint16(data) == ch {
			b.Write(data[2:])
		}
	}
	return b.String()
}

func TestGuardInSession(t *testing.T) {
	e := setup(t)
	repo := filepath.Join(e.home, "site")
	os.MkdirAll(repo, 0o700)
	exec.Command("git", "-C", repo, "init", "-q").Run()
	e.req("POST", "/api/agents/demo/sessions", map[string]any{"folder": "site", "kind": "claude", "name": "s"}, nil)
	eventually(t, "старт", func() bool { return e.sessions()["s"].Status == StWaiting })
	e.req("POST", "/api/agents/demo/sessions/s/input", map[string]any{"text": "write:page.md"}, nil)
	eventually(t, "напоминание о несохранённом", func() bool {
		return strings.Contains(e.history("s"), "незакоммиченные изменения")
	})
}

func TestShellAndExternalSessions(t *testing.T) {
	e := setup(t)
	os.MkdirAll(filepath.Join(e.home, "w"), 0o700)
	e.req("POST", "/api/agents/demo/sessions", map[string]any{"folder": "w", "kind": "shell", "name": "term"}, nil)
	exec.Command("tmux", "-L", e.agent.TmuxSocket, "new", "-d", "-s", "dev-site", "-c", filepath.Join(e.home, "w"), "sleep 600").Run()
	eventually(t, "обе сессии", func() bool {
		m := e.sessions()
		return m["term"].Kind == "shell" && m["dev-site"].Kind == "other" && m["dev-site"].Folder == "w"
	})
	// Rename и kill.
	if c := e.req("POST", "/api/agents/demo/sessions/term/rename", map[string]any{"to": "term2"}, nil); c != 200 {
		t.Fatal("rename", c)
	}
	e.req("POST", "/api/agents/demo/sessions/dev-site/kill", nil, nil)
	m := e.sessions()
	if _, ok := m["term2"]; !ok || len(m) != 1 {
		t.Fatalf("после rename/kill: %+v", m)
	}
}

func TestOriginAndAccess(t *testing.T) {
	e := setup(t)
	r, _ := http.NewRequest("POST", e.srv.URL+"/api/agents/demo/workdirs", strings.NewReader(`{"name":"x"}`))
	r.Header.Set("Origin", "https://evil.example")
	resp, _ := http.DefaultClient.Do(r)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal("чужой Origin должен отклоняться:", resp.StatusCode)
	}
	r, _ = http.NewRequest("POST", e.srv.URL+"/api/agents/demo/workdirs", strings.NewReader(`{"name":"x"}`))
	resp, _ = http.DefaultClient.Do(r)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal("без Origin изменяющий запрос отклоняется:", resp.StatusCode)
	}
	u := strings.Replace(e.srv.URL, "http", "ws", 1) + "/api/ws"
	if _, _, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}}); err == nil {
		t.Fatal("WebSocket с чужим Origin")
	}
	if c := e.req("GET", "/api/agents/nope/workdirs", nil, nil); c != 404 {
		t.Fatal("чужой агент:", c)
	}
	if c := e.req("GET", "/api/agents/demo/files/.ssh/tree", nil, nil); c != 404 {
		t.Fatal("зарезервированная папка:", c)
	}
}

func TestFilesAndView(t *testing.T) {
	e := setup(t)
	wd := filepath.Join(e.home, "docs")
	os.MkdirAll(wd, 0o700)
	os.WriteFile(filepath.Join(wd, "page.html"), []byte("<script>1</script>"), 0o600)
	os.WriteFile(filepath.Join(wd, ".env"), []byte("SECRET"), 0o600)
	os.WriteFile(filepath.Join(wd, "note.md"), []byte("# Привет"), 0o600)

	var txt struct {
		Meta    worker.FileMeta
		Content string
	}
	e.req("GET", "/api/agents/demo/files/docs/text?path=note.md", nil, &txt)
	if txt.Content != "# Привет" || txt.Meta.Kind != "markdown" {
		t.Fatalf("текст: %+v", txt)
	}
	// Правка: с версией из чтения — сохраняется, со старой — 409 и файл не тронут.
	var txt2 struct {
		Version string
	}
	e.req("GET", "/api/agents/demo/files/docs/text?path=note.md", nil, &txt2)
	var wr worker.WriteResult
	if c := e.req("PUT", "/api/agents/demo/files/docs/file?path=note.md", map[string]string{"content": "# Новое", "base": txt2.Version}, &wr); c != 200 || wr.Version == "" {
		t.Fatalf("сохранение: %d %+v", c, wr)
	}
	var cf map[string]string
	if c := e.req("PUT", "/api/agents/demo/files/docs/file?path=note.md", map[string]string{"content": "# Чужое", "base": txt2.Version}, &cf); c != 409 || cf["version"] != wr.Version {
		t.Fatalf("конфликт: %d %v", c, cf)
	}
	if b, _ := os.ReadFile(filepath.Join(wd, "note.md")); string(b) != "# Новое" {
		t.Fatalf("на диске: %q", b)
	}
	if c := e.req("PUT", "/api/agents/demo/files/.ssh/file?path=x", map[string]string{"content": "k"}, nil); c != 404 {
		t.Fatal("запись в зарезервированную папку:", c)
	}
	// Загрузка.
	r, _ := http.NewRequest("POST", e.srv.URL+"/api/agents/demo/files/docs/upload?name=photo.jpg", strings.NewReader("JPEGDATA"))
	r.Header.Set("Origin", e.d.origin)
	resp, err := http.DefaultClient.Do(r)
	if err != nil || resp.StatusCode != 200 {
		t.Fatal("загрузка", err, resp.StatusCode)
	}
	var up worker.UploadResult
	json.NewDecoder(resp.Body).Decode(&up)
	if up.Path != "photo.jpg" || up.Size != 8 {
		t.Fatalf("загрузка: %+v", up)
	}
	// Origin просмотра: токен на папку, без скрытых.
	var v map[string]string
	e.req("POST", "/api/agents/demo/files/docs/view", nil, &v)
	get := func(p string) (int, string, http.Header) {
		resp, err := http.Get(v["base"] + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	code, body, h := get("page.html")
	if code != 200 || body != "<script>1</script>" || !strings.HasPrefix(h.Get("Content-Type"), "text/html") || h.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("html: %d %q %v", code, body, h)
	}
	if code, _, _ := get(".env"); code == 200 {
		t.Fatal("скрытый файл через просмотр")
	}
	if code, _, _ := get("../.claude/settings.json"); code == 200 {
		t.Fatal("выход за папку через просмотр")
	}
	resp, _ = http.Get(e.aux.URL + "/t/неттакого/page.html")
	if resp.StatusCode != 403 {
		t.Fatal("чужой токен")
	}
	// Скачивание — attachment с sandbox.
	r, _ = http.NewRequest("GET", e.srv.URL+"/api/agents/demo/files/docs/download?path=page.html", nil)
	resp, _ = http.DefaultClient.Do(r)
	if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") || resp.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("скачивание: %v", resp.Header)
	}
}

func TestDevPortProxy(t *testing.T) {
	e := setup(t)
	// Dev-сервер агента на порту из его диапазона.
	var l net.Listener
	var port int
	for p := 20000; p < 20999; p++ {
		if ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p)); err == nil {
			l, port = ln, p
			break
		}
	}
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__Host-laavu", Value: "forged", Path: "/", Secure: true})
		http.SetCookie(w, &http.Cookie{Name: "app-session", Value: "ok"})
		fmt.Fprintf(w, "dev %s host=%s cookie=%q", r.URL.Path, r.Host, r.Header.Get("Cookie"))
	}))
	defer l.Close()
	if c := e.req("POST", "/api/agents/demo/ports/30000/open", nil, nil); c != 400 {
		t.Fatal("порт вне диапазона:", c)
	}
	var res map[string]string
	if c := e.req("POST", fmt.Sprintf("/api/agents/demo/ports/%d/open", port), nil, &res); c != 200 {
		t.Fatal("открыть порт:", c, res)
	}
	link := res["url"]
	// В тесте внешний адрес порта не слушается — идём в aux с нужным Host.
	host := strings.TrimPrefix(strings.SplitN(link, "/__laavu", 2)[0], "http://")
	jar := map[string]string{}
	do := func(path string) (int, string) {
		r, _ := http.NewRequest("GET", e.aux.URL+path, nil)
		r.Host = host
		for k, v := range jar {
			r.AddCookie(&http.Cookie{Name: k, Value: v})
		}
		r.AddCookie(&http.Cookie{Name: "app", Value: "1"})
		r.AddCookie(&http.Cookie{Name: "__Host-laavu", Value: "VICTIM"})
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		for _, c := range resp.Cookies() {
			jar[c.Name] = c.Value
		}
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if c, _ := do("/"); c != 403 {
		t.Fatal("без cookie порта:", c)
	}
	authPath := link[strings.Index(link, "/__laavu"):]
	if c, _ := do(authPath); c != 302 {
		t.Fatal("одноразовая ссылка:", c)
	}
	if c, _ := do(authPath); c != 403 {
		t.Fatal("повторное использование ссылки")
	}
	c, body := do("/assets/app.js")
	if c != 200 || !strings.Contains(body, "dev /assets/app.js") || strings.Contains(body, portCookie) || !strings.Contains(body, "host="+host) {
		t.Fatalf("прокси: %d %s", c, body)
	}
	// Cookie входа приложения dev-сервер агента не видит и поставить не может.
	if strings.Contains(body, "VICTIM") || !strings.Contains(body, "app=1") {
		t.Fatalf("cookie приложения ушла dev-серверу: %s", body)
	}
	if jar["__Host-laavu"] == "forged" || jar["app-session"] != "ok" {
		t.Fatalf("Set-Cookie от dev-сервера: %v", jar)
	}
}

func TestPortOwner(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tcp")
	os.WriteFile(f, []byte(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:4E21 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:4E22 0100007F:9C40 01 00000000:00000000 00:00000000 00000000  1002        0 2 1 0000000000000000 100 0 0 10 0
`), 0o644)
	old := procNetFiles
	procNetFiles = []string{f}
	defer func() { procNetFiles = old }()
	if err := checkPortOwner(20001, 1001); err != nil {
		t.Fatal(err)
	}
	if checkPortOwner(20001, 1002) == nil {
		t.Fatal("чужой владелец")
	}
	if checkPortOwner(20002, 1002) == nil {
		t.Fatal("не LISTEN")
	}
}

// Пуш «ждёт ответа»: уходит после тишины, не уходит, если сессия на экране.
func TestPushAfterStop(t *testing.T) {
	e := setup(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	os.MkdirAll(filepath.Join(e.home, "w"), 0o700)
	e.req("POST", "/api/agents/demo/sessions", map[string]any{"folder": "w", "kind": "shell", "name": "s"}, nil)
	e.d.Store = st
	e.d.Cfg.NoAuth = false
	u, _ := st.CreateUser("alice", "admin")
	var mu sync.Mutex
	hits := 0
	rcv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(201)
	}))
	defer rcv.Close()
	p, _ := push.New(st, e.d.Cfg.PublicURL)
	p.Client = rcv.Client()
	e.d.Push = p
	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	sub, _ := json.Marshal(map[string]any{"endpoint": rcv.URL + "/s", "keys": map[string]string{
		"p256dh": base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()), "auth": base64.RawURLEncoding.EncodeToString(make([]byte, 16))}})
	st.AddPushSub(u.ID, rcv.URL+"/s", string(sub))
	count := func() int { mu.Lock(); defer mu.Unlock(); return hits }
	old := pushGap
	pushGap = 0
	defer func() { pushGap = old }()

	// Выдуманная сессия — события игнорируются, пуша нет.
	e.d.handleHook("demo", hookEvent{Kind: "claude", Event: "Stop", Session: "Срочно-войдите"})
	time.Sleep(400 * time.Millisecond)
	if count() != 0 {
		t.Fatal("пуш о несуществующей сессии")
	}

	// Stop, а за ним быстро новое событие — пуша нет.
	e.d.handleHook("demo", hookEvent{Kind: "claude", Event: "Stop", Session: "s"})
	e.d.handleHook("demo", hookEvent{Kind: "claude", Event: "PreToolUse", Session: "s", Tool: "Edit", Target: "src/a\x1b[2J.go"})
	time.Sleep(400 * time.Millisecond)
	if count() != 0 {
		t.Fatal("пуш после Stop, за которым продолжился ход")
	}
	// Последнее действие — в списке сессий, без управляющих символов.
	list := e.d.status.merge("demo", []worker.SessionInfo{{Name: "s", Kind: "claude"}})
	if a := list[0].Action; a == nil || a.Tool != "Edit" || a.Target != "src/a[2J.go" {
		t.Fatalf("действие: %+v", a)
	}
	// Stop и тишина — пуш.
	e.d.handleHook("demo", hookEvent{Kind: "claude", Event: "Stop", Session: "s"})
	eventually(t, "пуш", func() bool { return count() == 1 })
	// Сессия на экране у пользователя — пуша нет.
	c := &wsClient{uid: u.ID, visible: true, focus: map[string]bool{"demo/s": true}}
	e.d.hub.add(c)
	e.d.handleHook("demo", hookEvent{Kind: "claude", Event: "Notification", Session: "s"})
	time.Sleep(400 * time.Millisecond)
	if count() != 1 {
		t.Fatal("пуш о сессии, которая на экране")
	}
	// Вкладка свёрнута — пуш есть.
	c.visible = false
	e.d.handleHook("demo", hookEvent{Kind: "claude", Event: "Stop", Session: "s"})
	eventually(t, "пуш при свёрнутой вкладке", func() bool { return count() == 2 })
}
