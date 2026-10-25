package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ilguzin/laavu/internal/auth"
	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/names"
	"github.com/ilguzin/laavu/internal/push"
	"github.com/ilguzin/laavu/internal/store"
	"github.com/ilguzin/laavu/internal/wire"
	"github.com/ilguzin/laavu/internal/worker"
)

type ctxKey int

const (
	userKey ctxKey = iota
	tokenKey
)

func userOf(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

// devUser — единственный пользователь режима без входа.
var devUser = &store.User{ID: 0, Name: "dev", Role: "admin"}

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func errf(code int, format string, a ...any) error {
	return &apiError{code, fmt.Sprintf(format, a...)}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	var ae *apiError
	var re *wire.RemoteError
	switch {
	case errors.As(err, &ae):
		code = ae.code
	case errors.As(err, &re):
		code = http.StatusUnprocessableEntity
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// api оборачивает обработчик, который возвращает данные или ошибку.
func api(f func(w http.ResponseWriter, r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := f(w, r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if v == nil {
			v = map[string]bool{"ok": true}
		}
		writeJSON(w, http.StatusOK, v)
	}
}

func readJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// checkOrigin — защита от cross-site запросов. Без неё любая открытая вкладка
// с чужим сайтом могла бы подключиться к шеллу через cookie пользователя.
func (d *Daemon) checkOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		// Браузеры шлют Origin у всех изменяющих запросов и у WebSocket; без него — не браузер с нашей страницы.
		return false
	}
	return o == d.origin
}

// Handler — HTTP приложения.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	// Вход.
	mux.HandleFunc("POST /api/auth/register/check", api(d.apiRegisterCheck))
	mux.HandleFunc("POST /api/auth/register/begin", api(d.apiRegisterBegin))
	mux.HandleFunc("POST /api/auth/register/finish", api(d.apiRegisterFinish))
	mux.HandleFunc("POST /api/auth/login/begin", api(d.apiLoginBegin))
	mux.HandleFunc("POST /api/auth/login/finish", api(d.apiLoginFinish))
	// Дальше — только с входом.
	authed := http.NewServeMux()
	authed.HandleFunc("GET /api/me", api(d.apiMe))
	authed.HandleFunc("POST /api/auth/logout", api(d.apiLogout))
	authed.HandleFunc("POST /api/auth/device-link", api(d.apiDeviceLink))
	authed.HandleFunc("GET /api/agents", api(d.apiAgents))
	authed.HandleFunc("GET /api/agents/{agent}/workdirs", api(d.apiWorkdirs))
	authed.HandleFunc("POST /api/agents/{agent}/workdirs", api(d.apiMkdir))
	authed.HandleFunc("GET /api/agents/{agent}/sessions", api(d.apiSessions))
	authed.HandleFunc("POST /api/agents/{agent}/sessions", api(d.apiNewSession))
	authed.HandleFunc("POST /api/agents/{agent}/sessions/{name}/input", api(d.apiInput))
	authed.HandleFunc("POST /api/agents/{agent}/sessions/{name}/keys", api(d.apiKeys))
	authed.HandleFunc("POST /api/agents/{agent}/sessions/{name}/scroll", api(d.apiScroll))
	authed.HandleFunc("POST /api/agents/{agent}/sessions/{name}/kill", api(d.apiKill))
	authed.HandleFunc("POST /api/agents/{agent}/sessions/{name}/rename", api(d.apiRename))
	authed.HandleFunc("POST /api/agents/{agent}/sessions/{name}/respawn", api(d.apiRespawn))
	authed.HandleFunc("GET /api/agents/{agent}/sessions/{name}/history", api(d.apiHistory))
	authed.HandleFunc("GET /api/agents/{agent}/sessions/{name}/context", api(d.apiContext))
	authed.HandleFunc("GET /api/agents/{agent}/files/{folder}/tree", api(d.apiTree))
	authed.HandleFunc("GET /api/agents/{agent}/files/{folder}/text", api(d.apiText))
	authed.HandleFunc("GET /api/agents/{agent}/files/{folder}/download", d.apiDownload)
	authed.HandleFunc("PUT /api/agents/{agent}/files/{folder}/file", d.apiWrite)
	authed.HandleFunc("POST /api/agents/{agent}/files/{folder}/upload", api(d.apiUpload))
	authed.HandleFunc("GET /api/agents/{agent}/files/{folder}/changes", api(d.apiChanges))
	authed.HandleFunc("GET /api/agents/{agent}/files/{folder}/diff", api(d.apiDiff))
	authed.HandleFunc("POST /api/agents/{agent}/files/{folder}/view", api(d.apiViewToken))
	authed.HandleFunc("POST /api/agents/{agent}/ports/{port}/open", api(d.apiOpenPort))
	authed.HandleFunc("GET /api/push/key", api(d.apiPushKey))
	authed.HandleFunc("POST /api/push/subscribe", api(d.apiPushSubscribe))
	authed.HandleFunc("POST /api/push/unsubscribe", api(d.apiPushUnsubscribe))
	authed.HandleFunc("GET /api/ws", func(w http.ResponseWriter, r *http.Request) {
		tok, _ := r.Context().Value(tokenKey).(string)
		d.handleWS(w, r, userOf(r), tok)
	})
	authed.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, errf(http.StatusNotFound, "нет такого вызова"))
	})
	mux.Handle("/api/", d.requireAuth(authed))
	mux.HandleFunc("/", d.serveStatic)
	return d.securityHeaders(d.originGuard(mux))
}

// originGuard проверяет Origin у всех изменяющих запросов и у WebSocket.
func (d *Daemon) originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || ws {
			if !d.checkOrigin(r) {
				writeErr(w, errf(http.StatusForbidden, "запрос не с адреса приложения"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (d *Daemon) securityHeaders(next http.Handler) http.Handler {
	csp := strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'wasm-unsafe-eval'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob: " + d.viewOrigin,
		"media-src 'self' " + d.viewOrigin,
		"font-src 'self' data:",
		"connect-src 'self' " + d.viewOrigin,
		"frame-src " + d.viewOrigin,
		"worker-src 'self' blob:",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (d *Daemon) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.Cfg.NoAuth {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, devUser)))
			return
		}
		u, tok, err := d.Auth.UserFromRequest(r)
		if err != nil {
			writeErr(w, errf(http.StatusUnauthorized, "нужен вход"))
			return
		}
		ctx := context.WithValue(r.Context(), userKey, u)
		ctx = context.WithValue(ctx, tokenKey, tok)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (d *Daemon) serveStatic(w http.ResponseWriter, r *http.Request) {
	if d.Static == nil {
		http.Error(w, "клиент не собран", http.StatusNotFound)
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	f, err := d.Static.Open(p)
	if err != nil {
		// Маршруты клиента (/register, /a/...) отдают index.html.
		p = "index.html"
		f, err = d.Static.Open(p)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(filepath.Ext(p)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	switch {
	case strings.HasPrefix(p, "assets/"):
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		w.Header().Set("Cache-Control", "no-cache")
	}
	if p == "sw.js" {
		w.Header().Set("Service-Worker-Allowed", "/")
	}
	http.ServeContent(w, r, p, time.Time{}, f.(io.ReadSeeker))
}

// --- Вход ---

func (d *Daemon) noAuthErr() error {
	if d.Cfg.NoAuth {
		return errf(http.StatusNotFound, "вход отключён")
	}
	return nil
}

func (d *Daemon) apiRegisterCheck(w http.ResponseWriter, r *http.Request) (any, error) {
	if err := d.noAuthErr(); err != nil {
		return nil, err
	}
	var in struct{ Token string }
	if err := readJSON(r, &in); err != nil {
		return nil, err
	}
	return d.Auth.CheckInvite(in.Token)
}

func (d *Daemon) apiRegisterBegin(w http.ResponseWriter, r *http.Request) (any, error) {
	if err := d.noAuthErr(); err != nil {
		return nil, err
	}
	var in struct{ Token, Name string }
	if err := readJSON(r, &in); err != nil {
		return nil, err
	}
	opts, cid, err := d.Auth.BeginRegister(in.Token, in.Name)
	if err != nil {
		return nil, err
	}
	return map[string]any{"options": opts, "cid": cid}, nil
}

func (d *Daemon) apiRegisterFinish(w http.ResponseWriter, r *http.Request) (any, error) {
	if err := d.noAuthErr(); err != nil {
		return nil, err
	}
	label := r.URL.Query().Get("label")
	if len(label) > 64 {
		label = label[:64]
	}
	u, err := d.Auth.FinishRegister(r.URL.Query().Get("cid"), label, r)
	if err != nil {
		return nil, err
	}
	return d.login(w, r, u)
}

func (d *Daemon) login(w http.ResponseWriter, r *http.Request, u *store.User) (any, error) {
	tok, err := d.Store.CreateLogin(u.ID, auth.LoginTTL, r.UserAgent())
	if err != nil {
		return nil, err
	}
	auth.SetCookie(w, tok)
	return map[string]any{"name": u.Name, "role": u.Role}, nil
}

func (d *Daemon) apiLoginBegin(w http.ResponseWriter, r *http.Request) (any, error) {
	if err := d.noAuthErr(); err != nil {
		return nil, err
	}
	opts, cid, err := d.Auth.BeginLogin()
	if err != nil {
		return nil, err
	}
	return map[string]any{"options": opts, "cid": cid}, nil
}

func (d *Daemon) apiLoginFinish(w http.ResponseWriter, r *http.Request) (any, error) {
	if err := d.noAuthErr(); err != nil {
		return nil, err
	}
	u, err := d.Auth.FinishLogin(r.URL.Query().Get("cid"), r)
	if err != nil {
		return nil, errf(http.StatusUnauthorized, "%s", err.Error())
	}
	return d.login(w, r, u)
}

func (d *Daemon) apiLogout(w http.ResponseWriter, r *http.Request) (any, error) {
	if d.Cfg.NoAuth {
		return nil, nil
	}
	tok, _ := r.Context().Value(tokenKey).(string)
	d.Store.DeleteLogin(tok)
	d.hub.dropLogin(tok)
	auth.ClearCookie(w)
	return nil, nil
}

// apiDeviceLink — ссылка для добавления passkey с другого устройства.
func (d *Daemon) apiDeviceLink(w http.ResponseWriter, r *http.Request) (any, error) {
	if err := d.noAuthErr(); err != nil {
		return nil, err
	}
	u := userOf(r)
	tok, err := d.Store.CreateInvite(store.Invite{UserID: &u.ID, Role: u.Role}, 15*time.Minute)
	if err != nil {
		return nil, err
	}
	return map[string]string{"url": d.Auth.RegisterURL(tok)}, nil
}

func (d *Daemon) apiMe(w http.ResponseWriter, r *http.Request) (any, error) {
	u := userOf(r)
	me := map[string]any{
		"name": u.Name, "role": u.Role, "no_auth": d.Cfg.NoAuth,
		"view_url": d.Cfg.ViewURL, "push": d.Push != nil,
	}
	if u.Admin() && !d.Cfg.NoAuth {
		// Файл пишет laavud priv check-isolation от root.
		var lc map[string]any
		if b, err := os.ReadFile(filepath.Join(d.Cfg.StateDir, "isolation-check.json")); err == nil && json.Unmarshal(b, &lc) == nil {
			me["isolation"] = lc
		} else {
			me["isolation"] = nil
		}
	}
	return me, nil
}

// --- Агенты, папки, сессии ---

func (d *Daemon) agentFor(r *http.Request) (*config.Agent, error) {
	name := r.PathValue("agent")
	if !d.canAccess(userOf(r), name) && !(d.Cfg.NoAuth && d.agents[name] != nil) {
		return nil, errf(http.StatusNotFound, "нет такого агента")
	}
	return d.agents[name], nil
}

func (d *Daemon) apiAgents(w http.ResponseWriter, r *http.Request) (any, error) {
	type ag struct {
		Name  string `json:"name"`
		Ports string `json:"ports"`
	}
	out := []ag{}
	for _, n := range d.order {
		if d.Cfg.NoAuth || d.canAccess(userOf(r), n) {
			out = append(out, ag{Name: n, Ports: d.agents[n].PortsEnv()})
		}
	}
	return out, nil
}

func (d *Daemon) apiWorkdirs(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	var out []worker.Workdir
	return out, d.call(a, "workdirs", nil, &out)
}

func (d *Daemon) apiMkdir(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	var in struct{ Name string }
	if err := readJSON(r, &in); err != nil {
		return nil, err
	}
	var out worker.Workdir
	return out, d.call(a, "mkdir", in, &out)
}

func (d *Daemon) apiSessions(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	return d.status.refresh(a.Name, false)
}

func (d *Daemon) apiNewSession(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		Name, Folder, Kind string
		Cols, Rows         int
		Create             bool // завести папку, если её нет
	}
	if err := readJSON(r, &in); err != nil {
		return nil, err
	}
	if in.Create {
		var wd worker.Workdir
		if err := d.call(a, "mkdir", map[string]string{"name": in.Folder}, &wd); err != nil && !strings.Contains(err.Error(), "уже есть") {
			return nil, err
		}
	}
	u := userOf(r)
	var si worker.SessionInfo
	since := time.Now()
	err = d.call(a, "new", worker.NewArgs{
		Name: in.Name, Folder: in.Folder, Kind: in.Kind, Author: u.Name,
		Profile: d.profileFor(u.Name), Cols: in.Cols, Rows: in.Rows,
	}, &si)
	if err != nil {
		return nil, err
	}
	if si.Kind == "claude" {
		d.status.starting(a.Name, si.Name, since)
	}
	go d.status.refresh(a.Name, false)
	return si, nil
}

func (d *Daemon) sessionName(r *http.Request) (string, error) {
	n := r.PathValue("name")
	if err := names.Session(n); err != nil {
		return "", err
	}
	return n, nil
}

func (d *Daemon) apiInput(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	n, err := d.sessionName(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		Text  string
		Enter *bool
	}
	if err := readJSON(r, &in); err != nil {
		return nil, err
	}
	enter := in.Enter == nil || *in.Enter
	return nil, d.call(a, "input", worker.InputArgs{Name: n, Text: in.Text, Enter: enter, Profile: d.profileFor(d.sessionAuthor(a.Name, n))}, nil)
}

func (d *Daemon) apiScroll(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	n, err := d.sessionName(r)
	if err != nil {
		return nil, err
	}
	var in struct{ Lines int }
	if err := readJSON(r, &in); err != nil {
		return nil, err
	}
	return nil, d.call(a, "scroll", map[string]any{"name": n, "lines": in.Lines}, nil)
}

func (d *Daemon) apiKeys(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	n, err := d.sessionName(r)
	if err != nil {
		return nil, err
	}
	var in struct{ Keys []string }
	if err := readJSON(r, &in); err != nil {
		return nil, err
	}
	return nil, d.call(a, "keys", map[string]any{"name": n, "keys": in.Keys}, nil)
}

func (d *Daemon) apiKill(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	n, err := d.sessionName(r)
	if err != nil {
		return nil, err
	}
	err = d.call(a, "kill", map[string]string{"name": n}, nil)
	go d.status.refresh(a.Name, false)
	return nil, err
}

func (d *Daemon) apiRename(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	n, err := d.sessionName(r)
	if err != nil {
		return nil, err
	}
	var in struct{ To string }
	if err := readJSON(r, &in); err != nil {
		return nil, err
	}
	err = d.call(a, "rename", map[string]string{"name": n, "to": in.To}, nil)
	go d.status.refresh(a.Name, false)
	return nil, err
}

func (d *Daemon) apiRespawn(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	n, err := d.sessionName(r)
	if err != nil {
		return nil, err
	}
	var er worker.EnsureResult
	since := time.Now()
	if err := d.call(a, "respawn", worker.EnsureArgs{Name: n, Profile: d.profileFor(d.sessionAuthor(a.Name, n))}, &er); err != nil {
		return nil, err
	}
	d.status.starting(a.Name, n, since)
	go d.status.refresh(a.Name, true)
	return er, nil
}

func (d *Daemon) apiHistory(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	n, err := d.sessionName(r)
	if err != nil {
		return nil, err
	}
	var out map[string]string
	return out, d.call(a, "history", map[string]string{"name": n}, &out)
}

func (d *Daemon) apiContext(w http.ResponseWriter, r *http.Request) (any, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, err
	}
	n, err := d.sessionName(r)
	if err != nil {
		return nil, err
	}
	var out worker.ContextUsage
	if err := d.call(a, "context", map[string]string{"name": n}, &out); err != nil {
		// До первого ответа модели счётчиков нет — это не ошибка.
		var re *wire.RemoteError
		if errors.As(err, &re) {
			return worker.ContextUsage{}, nil
		}
		return nil, err
	}
	return out, nil
}

// --- Файлы ---

func (d *Daemon) folderFor(r *http.Request) (*config.Agent, string, error) {
	a, err := d.agentFor(r)
	if err != nil {
		return nil, "", err
	}
	f := r.PathValue("folder")
	if names.NewReserved().IsReserved(f) {
		return nil, "", errf(http.StatusNotFound, "нет такой папки")
	}
	return a, f, nil
}

func (d *Daemon) apiTree(w http.ResponseWriter, r *http.Request) (any, error) {
	a, f, err := d.folderFor(r)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	var out []worker.Entry
	return out, d.call(a, "tree", map[string]any{"folder": f, "path": q.Get("path"), "hidden": q.Get("hidden") == "1"}, &out)
}

// maxText — сколько исходника отдаётся в просмотрщик; дальше — «скачать».
const maxText = 2 << 20

func (d *Daemon) apiText(w http.ResponseWriter, r *http.Request) (any, error) {
	a, f, err := d.folderFor(r)
	if err != nil {
		return nil, err
	}
	var meta worker.FileMeta
	c, err := d.stream(a, "read", map[string]any{"folder": f, "path": r.URL.Query().Get("path"), "max": maxText}, &meta)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if meta.Dir {
		return map[string]any{"meta": meta}, nil
	}
	var sb strings.Builder
	for {
		t, p, err := c.ReadFrame()
		if err != nil {
			return nil, err
		}
		if t == wire.FrameEnd {
			break
		}
		sb.Write(p)
	}
	res := map[string]any{"meta": meta, "truncated": meta.Size > maxText}
	if meta.Kind != "binary" {
		res["content"] = sb.String()
		// Версия — для правки: сохранение пройдёт, только если файл с тех пор не менялся.
		if meta.Size <= worker.MaxWrite {
			res["version"] = worker.Version([]byte(sb.String()))
		}
	}
	return res, nil
}

// apiWrite сохраняет файл целиком. Тело — {"content", "base"}: base — версия,
// от которой правили (пусто — новый файл). Если на диске уже другая версия,
// ответ 409 с текущей версией, файл не тронут.
func (d *Daemon) apiWrite(w http.ResponseWriter, r *http.Request) {
	a, f, err := d.folderFor(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Content string `json:"content"`
		Base    string `json:"base"`
	}
	// JSON с экранированием длиннее содержимого: запас вдвое.
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2*worker.MaxWrite+4096))
	if err != nil {
		writeErr(w, errf(http.StatusRequestEntityTooLarge, "файл больше %d МБ", worker.MaxWrite>>20))
		return
	}
	if err := json.Unmarshal(b, &in); err != nil {
		writeErr(w, err)
		return
	}
	var res worker.WriteResult
	if err := d.call(a, "write", map[string]any{"folder": f, "path": r.URL.Query().Get("path"), "content": in.Content, "base": in.Base}, &res); err != nil {
		writeErr(w, err)
		return
	}
	if res.Conflict {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "файл изменился на диске", "version": res.Version})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (d *Daemon) apiDownload(w http.ResponseWriter, r *http.Request) {
	a, f, err := d.folderFor(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var meta worker.FileMeta
	c, err := d.stream(a, "read", map[string]any{"folder": f, "path": r.URL.Query().Get("path")}, &meta)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer c.Close()
	if meta.Dir {
		writeErr(w, errf(http.StatusBadRequest, "это папка"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(meta.Path)}))
	h.Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "no-store")
	for {
		t, p, err := c.ReadFrame()
		if err != nil || t == wire.FrameEnd {
			return
		}
		if _, err := w.Write(p); err != nil {
			return
		}
	}
}

func (d *Daemon) apiUpload(w http.ResponseWriter, r *http.Request) (any, error) {
	a, f, err := d.folderFor(r)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	limit := d.Cfg.UploadLimit << 20
	if r.ContentLength > limit {
		return nil, errf(http.StatusRequestEntityTooLarge, "файл больше %d МБ", d.Cfg.UploadLimit)
	}
	c, err := d.stream(a, "upload", map[string]any{"folder": f, "dir": q.Get("dir"), "name": q.Get("name"), "size": r.ContentLength}, nil)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	buf := make([]byte, 256<<10)
	body := http.MaxBytesReader(w, r.Body, limit)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if err := c.WriteFrame(wire.FrameData, buf[:n]); err != nil {
				return nil, err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			c.Close()
			return nil, rerr
		}
	}
	c.WriteFrame(wire.FrameEnd, nil)
	_, p, err := c.ReadFrame()
	if err != nil {
		return nil, err
	}
	var res struct {
		worker.UploadResult
		Error string `json:"error"`
	}
	json.Unmarshal(p, &res)
	if res.Error != "" {
		return nil, errors.New(res.Error)
	}
	return res.UploadResult, nil
}

func (d *Daemon) apiChanges(w http.ResponseWriter, r *http.Request) (any, error) {
	a, f, err := d.folderFor(r)
	if err != nil {
		return nil, err
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	var out worker.Changes
	return out, d.call(a, "changes", map[string]any{"folder": f, "since": since}, &out)
}

func (d *Daemon) apiDiff(w http.ResponseWriter, r *http.Request) (any, error) {
	a, f, err := d.folderFor(r)
	if err != nil {
		return nil, err
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	var out map[string]any
	return out, d.call(a, "diff", map[string]any{"folder": f, "path": r.URL.Query().Get("path"), "since": since}, &out)
}

// --- Пуш ---

func (d *Daemon) apiPushKey(w http.ResponseWriter, r *http.Request) (any, error) {
	if d.Push == nil {
		return nil, errf(http.StatusNotFound, "пуш выключен")
	}
	return map[string]string{"key": d.Push.PublicKey()}, nil
}

func (d *Daemon) apiPushSubscribe(w http.ResponseWriter, r *http.Request) (any, error) {
	if d.Push == nil || d.Cfg.NoAuth {
		return nil, errf(http.StatusNotFound, "пуш выключен")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 8192))
	if err != nil {
		return nil, err
	}
	sub, err := push.ParseSubscription(b)
	if err != nil {
		return nil, err
	}
	if !push.AllowedEndpoint(sub.Endpoint) {
		return nil, errors.New("адрес подписки не похож на push-сервис браузера")
	}
	return nil, d.Store.AddPushSub(userOf(r).ID, sub.Endpoint, string(b))
}

func (d *Daemon) apiPushUnsubscribe(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct{ Endpoint string }
	if err := readJSON(r, &in); err != nil {
		return nil, err
	}
	if d.Store != nil && !d.Cfg.NoAuth {
		d.Store.DeleteUserPushSub(userOf(r).ID, in.Endpoint)
	}
	return nil, nil
}
