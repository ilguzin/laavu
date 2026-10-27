// Package worker is the agent worker: everything the application does on behalf of the agent.
//
// The worker runs as the agent's system user and accepts connections
// only from laavud (systemd sets the socket permissions). Folders, files,
// inotify, tmux sessions and attaching to them all go through it.
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

// Worker is the worker of a single agent.
type Worker struct {
	Agent    config.Agent
	Home     string
	Reserved *names.Reserved
	Tmux     *tmuxctl.Tmux
	// Shell is the agent's shell for terminal sessions.
	Shell string
	// StateDir is where the session snapshot lives (~/.local/state/laavu).
	StateDir string
	// UploadLimit is the size limit of a single upload, in bytes.
	UploadLimit int64
	// LocalClone lets clone take local paths and file:// (dev mode and tests).
	LocalClone bool

	snapMu sync.Mutex
	log    *slog.Logger

	// wheelRest holds leftover swipe lines that did not add up to a wheel event.
	wheelMu   sync.Mutex
	wheelRest map[string]int
}

// Options is what the worker needs at startup.
type Options struct {
	Agent      config.Agent
	Reserved   *names.Reserved
	TmuxConf   string
	HookSocket string // passed into the tmux environment so hooks know where to send
	LaavudBin  string
	Scope      bool
	Log        *slog.Logger
}

// New builds a worker for the current user.
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

// Listener returns the socket from systemd (LISTEN_FDS) or listens on path.
func Listener(path string) (net.Listener, error) {
	if os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) && os.Getenv("LISTEN_FDS") == "1" {
		f := os.NewFile(3, "systemd-socket")
		l, err := net.FileListener(f)
		f.Close()
		return l, err
	}
	if path == "" {
		return nil, errors.New("no socket: neither LISTEN_FDS nor a path")
	}
	os.Remove(path)
	old := umask(0o077)
	l, err := net.Listen("unix", path)
	umask(old)
	return l, err
}

// systemdEnv lists worker unit variables that have no business in agent sessions.
var systemdEnv = []string{"LISTEN_FDS", "LISTEN_PID", "LISTEN_FDNAMES", "NOTIFY_SOCKET", "INVOCATION_ID",
	"JOURNAL_STREAM", "MEMORY_PRESSURE_WATCH", "MEMORY_PRESSURE_WRITE", "SYSTEMD_EXEC_PID", "MANAGERPID"}

// Serve serves connections while the listener is open.
func (w *Worker) Serve(l net.Listener) error {
	for _, k := range systemdEnv {
		os.Unsetenv(k)
	}
	// At startup, reconcile the snapshot with tmux: after a reboot sessions become "sleeping".
	if err := w.reconcile(); err != nil {
		w.log.Warn("snapshot reconcile", "err", err)
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

// stream handlers reply and send frames themselves; they return errStreamed.
var errStreamed = errors.New("streamed")

var handlers = map[string]handler{
	"ping": func(w *Worker, _ *wire.Conn, _ json.RawMessage) (any, error) {
		return map[string]string{"agent": w.Agent.Name}, nil
	},
	"workdirs":   (*Worker).opWorkdirs,
	"mkdir":      (*Worker).opMkdir,
	"sessions":   (*Worker).opSessions,
	"new":        (*Worker).opNew,
	"ensure":     (*Worker).opEnsure,
	"kill":       (*Worker).opKill,
	"rename":     (*Worker).opRename,
	"respawn":    (*Worker).opRespawn,
	"meta":       (*Worker).opMeta,
	"sync":       (*Worker).opSync,
	"input":      (*Worker).opInput,
	"keys":       (*Worker).opKeys,
	"scroll":     (*Worker).opScroll,
	"history":    (*Worker).opHistory,
	"context":    (*Worker).opContext,
	"attach":     (*Worker).opAttach,
	"tree":       (*Worker).opTree,
	"read":       (*Worker).opRead,
	"upload":     (*Worker).opUpload,
	"watch":      (*Worker).opWatch,
	"changes":    (*Worker).opChanges,
	"diff":       (*Worker).opDiff,
	"write":      (*Worker).opWrite,
	"clone":      (*Worker).opClone,
	"sshkey":     (*Worker).opSSHKey,
	"sshkey-new": (*Worker).opSSHKeyNew,
}

func (w *Worker) handle(c *wire.Conn) {
	defer c.Close()
	var req wire.Request
	if err := c.ReadLine(&req); err != nil {
		return
	}
	h, ok := handlers[req.Op]
	if !ok {
		c.Reply(nil, fmt.Errorf("unknown operation %q", req.Op))
		return
	}
	data, err := h(w, c, req.Args)
	if errors.Is(err, errStreamed) {
		return
	}
	if err != nil {
		w.log.Debug("operation", "op", req.Op, "err", err)
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
