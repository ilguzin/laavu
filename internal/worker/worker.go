// Package worker — воркер агента: всё, что приложение делает от имени агента.
//
// Воркер работает от системного пользователя агента и принимает соединения
// только от laavud (права на сокет задаёт systemd). Через него идут рабочие
// папки, файлы, inotify, tmux-сессии и подключение к ним.
package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/names"
	"github.com/ilguzin/laavu/internal/tmuxctl"
	"github.com/ilguzin/laavu/internal/wire"
)

// Worker — воркер одного агента.
type Worker struct {
	Agent    config.Agent
	Home     string
	Reserved *names.Reserved
	Tmux     *tmuxctl.Tmux
	// Shell — shell агента для сессий-терминалов.
	Shell string
	// StateDir — где лежит снимок сессий (~/.local/state/laavu).
	StateDir string
	// UploadLimit — предел размера одной загрузки в байтах.
	UploadLimit int64

	snapMu sync.Mutex
	log    *slog.Logger

	// wheelRest — остаток строк свайпа, не дотянувший до события колеса.
	wheelMu   sync.Mutex
	wheelRest map[string]int
}

// Options — то, что нужно воркеру при старте.
type Options struct {
	Agent      config.Agent
	Reserved   *names.Reserved
	TmuxConf   string
	HookSocket string // передаётся в окружение tmux, чтобы хуки знали, куда слать
	LaavudBin  string
	Scope      bool
	Log        *slog.Logger
}

// New собирает воркер для текущего пользователя.
func New(o Options) (*Worker, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	home := o.Agent.Home
	if home == "" {
		home = u.HomeDir
	}
	shell := loginShell(u.Username)
	uid := os.Getuid()
	runtimeDir := fmt.Sprintf("/run/user/%d", uid)
	env := []string{
		"HOME=" + home,
		"USER=" + u.Username,
		"LOGNAME=" + u.Username,
		"SHELL=" + shell,
		"PATH=" + filepath.Join(home, ".local/bin") + ":/usr/local/bin:/usr/bin:/bin",
		"LANG=C.UTF-8",
		"TMPDIR=" + filepath.Join(home, "tmp"),
		"LAAVU_AGENT=" + o.Agent.Name,
		"LAAVU_PORTS=" + o.Agent.PortsEnv(),
	}
	if o.HookSocket != "" {
		env = append(env, "LAAVU_HOOK_SOCK="+o.HookSocket)
	}
	if o.LaavudBin != "" {
		env = append(env, "LAAVUD_BIN="+o.LaavudBin)
	}
	if _, err := os.Stat(runtimeDir); err == nil {
		env = append(env, "XDG_RUNTIME_DIR="+runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path="+runtimeDir+"/bus")
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	w := &Worker{
		Agent:       o.Agent,
		Home:        home,
		Reserved:    o.Reserved,
		Shell:       shell,
		StateDir:    filepath.Join(home, ".local/state/laavu"),
		UploadLimit: 200 << 20,
		Tmux: &tmuxctl.Tmux{
			Socket: o.Agent.TmuxSocket,
			Conf:   o.TmuxConf,
			Env:    env,
			Scope:  o.Scope,
		},
		log: log.With("agent", o.Agent.Name),
	}
	if w.Reserved == nil {
		w.Reserved = names.NewReserved()
	}
	os.MkdirAll(filepath.Join(home, "tmp"), 0o700)
	os.MkdirAll(w.StateDir, 0o700)
	return w, nil
}

func loginShell(username string) string {
	b, err := os.ReadFile("/etc/passwd")
	if err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Split(line, ":")
			if len(f) == 7 && f[0] == username && f[6] != "" && !strings.HasSuffix(f[6], "nologin") {
				return f[6]
			}
		}
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/bash"
}

// Listener возвращает сокет из systemd (LISTEN_FDS) или слушает path.
func Listener(path string) (net.Listener, error) {
	if os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) && os.Getenv("LISTEN_FDS") == "1" {
		f := os.NewFile(3, "systemd-socket")
		l, err := net.FileListener(f)
		f.Close()
		return l, err
	}
	if path == "" {
		return nil, errors.New("нет сокета: ни LISTEN_FDS, ни пути")
	}
	os.Remove(path)
	old := umask(0o077)
	l, err := net.Listen("unix", path)
	umask(old)
	return l, err
}

// systemdEnv — переменные юнита воркера, которым нечего делать в сессиях агента.
var systemdEnv = []string{"LISTEN_FDS", "LISTEN_PID", "LISTEN_FDNAMES", "NOTIFY_SOCKET", "INVOCATION_ID",
	"JOURNAL_STREAM", "MEMORY_PRESSURE_WATCH", "MEMORY_PRESSURE_WRITE", "SYSTEMD_EXEC_PID", "MANAGERPID"}

// Serve обслуживает соединения, пока слушатель открыт.
func (w *Worker) Serve(l net.Listener) error {
	for _, k := range systemdEnv {
		os.Unsetenv(k)
	}
	// При старте приводим снимок к tmux: после перезагрузки сессии станут «спит».
	if err := w.reconcile(); err != nil {
		w.log.Warn("сверка снимка", "err", err)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go w.handle(wire.NewConn(c))
	}
}

type handler func(w *Worker, c *wire.Conn, args json.RawMessage) (any, error)

// stream-обработчики сами отвечают и ведут кадры; они возвращают errStreamed.
var errStreamed = errors.New("streamed")

var handlers = map[string]handler{
	"ping": func(w *Worker, _ *wire.Conn, _ json.RawMessage) (any, error) {
		return map[string]string{"agent": w.Agent.Name}, nil
	},
	"workdirs": (*Worker).opWorkdirs,
	"mkdir":    (*Worker).opMkdir,
	"sessions": (*Worker).opSessions,
	"new":      (*Worker).opNew,
	"ensure":   (*Worker).opEnsure,
	"kill":     (*Worker).opKill,
	"rename":   (*Worker).opRename,
	"respawn":  (*Worker).opRespawn,
	"meta":     (*Worker).opMeta,
	"sync":     (*Worker).opSync,
	"input":    (*Worker).opInput,
	"keys":     (*Worker).opKeys,
	"scroll":   (*Worker).opScroll,
	"history":  (*Worker).opHistory,
	"context":  (*Worker).opContext,
	"attach":   (*Worker).opAttach,
	"tree":     (*Worker).opTree,
	"read":     (*Worker).opRead,
	"upload":   (*Worker).opUpload,
	"watch":    (*Worker).opWatch,
	"changes":  (*Worker).opChanges,
	"diff":     (*Worker).opDiff,
	"write":    (*Worker).opWrite,
}

func (w *Worker) handle(c *wire.Conn) {
	defer c.Close()
	var req wire.Request
	if err := c.ReadLine(&req); err != nil {
		return
	}
	h, ok := handlers[req.Op]
	if !ok {
		c.Reply(nil, fmt.Errorf("неизвестная операция %q", req.Op))
		return
	}
	data, err := h(w, c, req.Args)
	if errors.Is(err, errStreamed) {
		return
	}
	if err != nil {
		w.log.Debug("операция", "op", req.Op, "err", err)
	}
	c.Reply(data, err)
}

func decode[T any](args json.RawMessage) (T, error) {
	var v T
	if len(args) == 0 {
		return v, nil
	}
	err := json.Unmarshal(args, &v)
	return v, err
}
