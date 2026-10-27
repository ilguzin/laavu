// Package tmuxctl wraps the agent's tmux server (tmux -L laavu).
package tmuxctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// User session options that hold Laavu state.
const (
	OptKind       = "@laavu-kind"       // claude | shell; empty for sessions created outside the client
	OptAuthor     = "@laavu-author"     // who started the session
	OptClaude     = "@laavu-claude"     // UUID of the current Claude Code conversation
	OptStatus     = "@laavu-status"     // status from hooks
	OptTranscript = "@laavu-transcript" // path to the current conversation's transcript
)

// Tmux is a single tmux server.
type Tmux struct {
	Bin    string   // path to tmux, "tmux" by default
	Socket string   // name for -L
	Conf   string   // shared config, passed via -f when the server starts
	Env    []string // process environment (added to os.Environ)
	// Scope starts the server under systemd-run --user --scope so that it stays
	// out of the worker's cgroup and survives the worker stopping.
	Scope bool
}

// Session is a tmux session with Laavu options.
type Session struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Created    int64  `json:"created"`
	Attached   int    `json:"attached"`
	Kind       string `json:"kind"`
	Author     string `json:"author,omitempty"`
	Claude     string `json:"claude,omitempty"`
	Status     string `json:"status,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	PaneDead   bool   `json:"pane_dead"`
	DeadStatus int    `json:"dead_status,omitempty"`
	StartCmd   string `json:"start_cmd,omitempty"`
	CurrentCmd string `json:"current_cmd,omitempty"`
	Activity   int64  `json:"activity"`
}

// Server identifies a running tmux server: pid and start time.
// The snapshot uses it to tell sessions of a previous run from ones closed in this run.
type Server struct {
	PID   int   `json:"pid"`
	Start int64 `json:"start"`
}

var listFields = []string{
	"#{session_name}", "#{session_path}", "#{session_created}", "#{session_attached}",
	"#{" + OptKind + "}", "#{" + OptAuthor + "}", "#{" + OptClaude + "}", "#{" + OptStatus + "}", "#{" + OptTranscript + "}",
	"#{pane_dead}", "#{pane_dead_status}", "#{pane_start_command}", "#{pane_current_command}",
	"#{window_activity}", "#{pid}", "#{start_time}",
}

// ErrNoServer means the tmux server is not running.
var ErrNoServer = errors.New("tmux server is not running")

func (t *Tmux) bin() string {
	if t.Bin != "" {
		return t.Bin
	}
	return "tmux"
}

func (t *Tmux) base() []string { return []string{"-L", t.Socket} }

// Command builds a tmux command with the agent's socket.
func (t *Tmux) Command(args ...string) *exec.Cmd {
	cmd := exec.Command(t.bin(), append(t.base(), args...)...)
	cmd.Env = append(os.Environ(), t.Env...)
	return cmd
}

func (t *Tmux) run(stdin []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.bin(), append(t.base(), args...)...)
	cmd.Env = append(os.Environ(), t.Env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if isNoServer(msg) {
			return "", ErrNoServer
		}
		return "", fmt.Errorf("tmux %s: %s", args[0], msg)
	}
	return out.String(), nil
}

// isNoServer: the server is not running, or it was exiting when we connected
// ("server exited unexpectedly", e.g. right after kill-server or a crash).
func isNoServer(msg string) bool {
	return strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "server exited unexpectedly") ||
		strings.Contains(msg, "error connecting to") ||
		strings.Contains(msg, "No such file or directory")
}

// List returns the sessions and the server identity. Without a server: an empty list and Server{}.
func (t *Tmux) List() ([]Session, Server, error) {
	out, err := t.run(nil, "list-sessions", "-F", strings.Join(listFields, "\t"))
	if errors.Is(err, ErrNoServer) {
		return nil, Server{}, nil
	}
	if err != nil {
		return nil, Server{}, err
	}
	var res []Session
	var srv Server
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != len(listFields) {
			continue
		}
		s := Session{
			Name: f[0], Path: f[1], Kind: f[4], Author: f[5], Claude: f[6], Status: f[7], Transcript: f[8],
			PaneDead: f[9] == "1", StartCmd: f[11], CurrentCmd: f[12],
		}
		s.Created, _ = strconv.ParseInt(f[2], 10, 64)
		s.Attached, _ = strconv.Atoi(f[3])
		s.DeadStatus, _ = strconv.Atoi(f[10])
		s.Activity, _ = strconv.ParseInt(f[13], 10, 64)
		srv.PID, _ = strconv.Atoi(f[14])
		srv.Start, _ = strconv.ParseInt(f[15], 10, 64)
		res = append(res, s)
	}
	return res, srv, nil
}

// Has reports whether a session with exactly this name exists.
func (t *Tmux) Has(name string) bool {
	_, err := t.run(nil, "has-session", "-t", "="+name)
	return err == nil
}

// NewOpts are the parameters of a new session.
type NewOpts struct {
	Name    string
	Dir     string
	Command []string // empty means the default shell
	Env     []string // KEY=VALUE for the session environment
	Options map[string]string
	Width   int
	Height  int
}

// New creates a session. If the server is not running yet, it starts in a separate scope.
func (t *Tmux) New(o NewOpts) error {
	args := []string{}
	if t.Conf != "" {
		args = append(args, "-f", t.Conf)
	}
	args = append(args, "new-session", "-d", "-s", o.Name, "-c", o.Dir)
	if o.Width > 0 && o.Height > 0 {
		args = append(args, "-x", strconv.Itoa(o.Width), "-y", strconv.Itoa(o.Height))
	}
	for _, e := range o.Env {
		args = append(args, "-e", e)
	}
	if len(o.Command) > 0 {
		args = append(args, "--")
		args = append(args, o.Command...)
	}
	// Options are set in the same command chain as the session itself: otherwise a fast
	// SessionStart or session-created hook could read or overwrite them first.
	keys := make([]string, 0, len(o.Options))
	for k := range o.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if o.Options[k] == "" {
			continue
		}
		args = append(args, ";", "set-option", "-t", "="+o.Name+":", k, o.Options[k])
	}
	full := append(t.base(), args...)
	var cmd *exec.Cmd
	if t.Scope && !t.serverRunning() {
		sargs := append([]string{"--user", "--scope", "--quiet", "--collect", "--", t.bin()}, full...)
		cmd = exec.Command("systemd-run", sargs...)
	} else {
		cmd = exec.Command(t.bin(), full...)
	}
	cmd.Env = append(os.Environ(), t.Env...)
	cmd.Dir = o.Dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (t *Tmux) serverRunning() bool {
	_, err := t.run(nil, "list-sessions", "-F", "#{pid}")
	return err == nil
}

// SetGlobalEnv sets the server's global environment variables: sessions the agent
// creates on its own get them too.
func (t *Tmux) SetGlobalEnv(env map[string]string) error {
	for k, v := range env {
		if _, err := t.run(nil, "set-environment", "-g", k, v); err != nil {
			return err
		}
	}
	return nil
}

// SetOption sets a user session option.
func (t *Tmux) SetOption(name, key, value string) error {
	if value == "" {
		_, err := t.run(nil, "set-option", "-t", "="+name+":", "-u", key)
		return err
	}
	_, err := t.run(nil, "set-option", "-t", "="+name+":", key, value)
	return err
}

// Kill closes a session.
func (t *Tmux) Kill(name string) error {
	_, err := t.run(nil, "kill-session", "-t", "="+name)
	return err
}

// Rename renames a session.
func (t *Tmux) Rename(from, to string) error {
	_, err := t.run(nil, "rename-session", "-t", "="+from, to)
	return err
}

// Respawn restarts a dead session pane with a command in the same folder.
func (t *Tmux) Respawn(name, dir string, command []string) error {
	args := []string{"respawn-pane", "-k", "-t", "=" + name + ":", "-c", dir}
	args = append(args, command...)
	_, err := t.run(nil, args...)
	return err
}

// Paste pastes text in bracketed mode so multi-line text goes as a single
// message, and presses Enter if requested.
func (t *Tmux) Paste(name, text string, enter bool) error {
	buf := fmt.Sprintf("laavu-%d", time.Now().UnixNano())
	if text != "" {
		if _, err := t.run([]byte(text), "load-buffer", "-b", buf, "-"); err != nil {
			return err
		}
		if _, err := t.run(nil, "paste-buffer", "-p", "-d", "-b", buf, "-t", "="+name+":"); err != nil {
			return err
		}
	}
	if enter {
		// The TUI needs time to process the paste: an Enter sent right after it
		// sometimes lands inside the pasted text.
		if text != "" {
			time.Sleep(150 * time.Millisecond)
		}
		return t.Keys(name, "Enter")
	}
	return nil
}

// Keys sends keys by their tmux names (Escape, Enter, C-c, BTab, Up...).
func (t *Tmux) Keys(name string, keys ...string) error {
	args := append([]string{"send-keys", "-t", "=" + name + ":"}, keys...)
	_, err := t.run(nil, args...)
	return err
}

// CancelMode takes the pane out of history view (copy-mode), which wheel scrolling
// puts it into. If the pane is not in that mode, it does nothing.
// The target is explicit in both calls: the agent has one tmux server for all sessions.
func (t *Tmux) CancelMode(name string) {
	out, err := t.run(nil, "display-message", "-p", "-t", "="+name+":", "#{pane_in_mode}")
	if err == nil && strings.TrimSpace(out) == "1" {
		t.run(nil, "send-keys", "-t", "="+name+":", "-X", "cancel")
	}
}

// MouseApp reports whether the program in the pane handles the mouse itself (Claude Code
// on the alternate screen): it has no tmux history and scrolls itself on wheel
// events. The second value reports SGR encoding.
func (t *Tmux) MouseApp(name string) (mouse, sgr bool) {
	out, err := t.run(nil, "display-message", "-p", "-t", "="+name+":", "#{pane_in_mode} #{mouse_any_flag} #{mouse_sgr_flag}")
	f := strings.Fields(out)
	if err != nil || len(f) != 3 || f[0] == "1" {
		return false, false
	}
	return f[1] == "1", f[2] == "1"
}

// Wheel sends n wheel events to the program in the pane: positive means up.
// The pointer is in the middle of the pane, as if the wheel were turned over it.
func (t *Tmux) Wheel(name string, n int, sgr bool) error {
	if n == 0 {
		return nil
	}
	target := "=" + name + ":"
	out, err := t.run(nil, "display-message", "-p", "-t", target, "#{pane_width} #{pane_height}")
	if err != nil {
		return err
	}
	var w, h int
	if _, err := fmt.Sscan(out, &w, &h); err != nil {
		return err
	}
	btn := 64 // wheel up
	if n < 0 {
		btn, n = 65, -n
	}
	x, y := w/2+1, h/2+1
	var ev []byte
	if sgr {
		ev = []byte(fmt.Sprintf("\x1b[<%d;%d;%dM", btn, x, y))
	} else {
		ev = []byte{0x1b, '[', 'M', byte(32 + btn), byte(32 + min(x, 223)), byte(32 + min(y, 223))}
	}
	args := []string{"send-keys", "-t", target, "-H"}
	for i := 0; i < n; i++ {
		for _, b := range ev {
			args = append(args, fmt.Sprintf("%02x", b))
		}
	}
	_, err = t.run(nil, args...)
	return err
}

// Scroll scrolls the pane history by lines: positive means up (the pane enters
// history view), negative means down; on reaching the bottom the pane leaves it.
func (t *Tmux) Scroll(name string, lines int) error {
	target := "=" + name + ":"
	if lines > 0 {
		if _, err := t.run(nil, "copy-mode", "-e", "-t", target); err != nil {
			return err
		}
		_, err := t.run(nil, "send-keys", "-t", target, "-X", "-N", strconv.Itoa(lines), "scroll-up")
		return err
	}
	out, err := t.run(nil, "display-message", "-p", "-t", target, "#{pane_in_mode}")
	if err != nil || strings.TrimSpace(out) != "1" || lines == 0 {
		return err
	}
	_, err = t.run(nil, "send-keys", "-t", target, "-X", "-N", strconv.Itoa(-lines), "scroll-down")
	return err
}

// WindowSize returns the current session window size. It is shared by all attached clients.
func (t *Tmux) WindowSize(name string) (cols, rows int, err error) {
	out, err := t.run(nil, "display-message", "-p", "-t", "="+name+":", "#{window_width} #{window_height}")
	if err != nil {
		return 0, 0, err
	}
	_, err = fmt.Sscan(out, &cols, &rows)
	return cols, rows, err
}

// Capture returns the pane history with colors.
func (t *Tmux) Capture(name string, lines int) (string, error) {
	return t.run(nil, "capture-pane", "-p", "-e", "-J", "-S", "-"+strconv.Itoa(lines), "-t", "="+name+":")
}

// AttachCommand returns the tmux client command for a PTY.
func (t *Tmux) AttachCommand(name string) *exec.Cmd {
	cmd := t.Command("attach-session", "-t", "="+name)
	cmd.Env = append(cmd.Env, "TERM=xterm-256color", "COLORTERM=truecolor")
	return cmd
}
