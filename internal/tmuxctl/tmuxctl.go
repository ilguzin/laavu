// Package tmuxctl — обёртка над сервером tmux агента (tmux -L laavu).
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

// Пользовательские опции сессии, в которых живёт состояние Laavu.
const (
	OptKind       = "@laavu-kind"       // claude | shell; пусто у сессий, заведённых мимо клиента
	OptAuthor     = "@laavu-author"     // кто начал сессию
	OptClaude     = "@laavu-claude"     // UUID текущего разговора Claude Code
	OptStatus     = "@laavu-status"     // статус из хуков
	OptTranscript = "@laavu-transcript" // путь к транскрипту текущего разговора
)

// Tmux — один сервер tmux.
type Tmux struct {
	Bin    string   // путь к tmux, по умолчанию "tmux"
	Socket string   // имя для -L
	Conf   string   // общий конфиг, подаётся через -f при старте сервера
	Env    []string // окружение процессов (дополняет os.Environ)
	// Scope запускает сервер в systemd-run --user --scope, чтобы он не попал
	// в cgroup воркера и пережил его остановку.
	Scope bool
}

// Session — сессия tmux с опциями Laavu.
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

// Server — идентичность запущенного сервера tmux: pid и время старта.
// По ней снимок отличает сессии прошлого запуска от закрытых в этом.
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

// ErrNoServer — сервер tmux не запущен.
var ErrNoServer = errors.New("сервер tmux не запущен")

func (t *Tmux) bin() string {
	if t.Bin != "" {
		return t.Bin
	}
	return "tmux"
}

func (t *Tmux) base() []string { return []string{"-L", t.Socket} }

// Command собирает команду tmux с сокетом агента.
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

func isNoServer(msg string) bool {
	return strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "error connecting to") ||
		strings.Contains(msg, "No such file or directory")
}

// List возвращает сессии и идентичность сервера. Без сервера — пустой список и Server{}.
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

// Has — есть ли сессия с точно таким именем.
func (t *Tmux) Has(name string) bool {
	_, err := t.run(nil, "has-session", "-t", "="+name)
	return err == nil
}

// NewOpts — параметры новой сессии.
type NewOpts struct {
	Name    string
	Dir     string
	Command []string // пусто — shell по умолчанию
	Env     []string // KEY=VALUE в окружение сессии
	Options map[string]string
	Width   int
	Height  int
}

// New заводит сессию. Если сервер ещё не запущен, он стартует в отдельном scope.
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
	// Опции ставятся в той же цепочке команд, что и сама сессия: иначе быстрый
	// хук SessionStart или session-created успел бы прочитать или затереть их.
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

// SetGlobalEnv задаёт переменные глобального окружения сервера: их получают и сессии,
// которые агент заводит сам.
func (t *Tmux) SetGlobalEnv(env map[string]string) error {
	for k, v := range env {
		if _, err := t.run(nil, "set-environment", "-g", k, v); err != nil {
			return err
		}
	}
	return nil
}

// SetOption ставит пользовательскую опцию сессии.
func (t *Tmux) SetOption(name, key, value string) error {
	if value == "" {
		_, err := t.run(nil, "set-option", "-t", "="+name+":", "-u", key)
		return err
	}
	_, err := t.run(nil, "set-option", "-t", "="+name+":", key, value)
	return err
}

// Kill закрывает сессию.
func (t *Tmux) Kill(name string) error {
	_, err := t.run(nil, "kill-session", "-t", "="+name)
	return err
}

// Rename переименовывает сессию.
func (t *Tmux) Rename(from, to string) error {
	_, err := t.run(nil, "rename-session", "-t", "="+from, to)
	return err
}

// Respawn перезапускает умершую панель сессии командой в той же папке.
func (t *Tmux) Respawn(name, dir string, command []string) error {
	args := []string{"respawn-pane", "-k", "-t", "=" + name + ":", "-c", dir}
	args = append(args, command...)
	_, err := t.run(nil, args...)
	return err
}

// Paste вставляет текст в bracketed-режиме, чтобы многострочный текст ушёл одним
// сообщением, и, если надо, нажимает Enter.
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
		// TUI нужно время разобрать вставку: Enter сразу после неё иногда
		// попадает внутрь вставленного текста.
		if text != "" {
			time.Sleep(150 * time.Millisecond)
		}
		return t.Keys(name, "Enter")
	}
	return nil
}

// Keys отправляет клавиши по именам tmux (Escape, Enter, C-c, BTab, Up…).
func (t *Tmux) Keys(name string, keys ...string) error {
	args := append([]string{"send-keys", "-t", "=" + name + ":"}, keys...)
	_, err := t.run(nil, args...)
	return err
}

// CancelMode выводит панель из режима просмотра истории (copy-mode), куда её
// переводит прокрутка колесом. Если панель не в нём — ничего не делает.
// Цель указывается явно в обоих вызовах: сервер tmux у агента один на все сессии.
func (t *Tmux) CancelMode(name string) {
	out, err := t.run(nil, "display-message", "-p", "-t", "="+name+":", "#{pane_in_mode}")
	if err == nil && strings.TrimSpace(out) == "1" {
		t.run(nil, "send-keys", "-t", "="+name+":", "-X", "cancel")
	}
}

// MouseApp — программа в панели сама принимает мышь (Claude Code на
// альтернативном экране): истории в tmux у неё нет, прокручивает она себя сама
// по колесу. Второе значение — кодировка SGR.
func (t *Tmux) MouseApp(name string) (mouse, sgr bool) {
	out, err := t.run(nil, "display-message", "-p", "-t", "="+name+":", "#{pane_in_mode} #{mouse_any_flag} #{mouse_sgr_flag}")
	f := strings.Fields(out)
	if err != nil || len(f) != 3 || f[0] == "1" {
		return false, false
	}
	return f[1] == "1", f[2] == "1"
}

// Wheel отправляет программе в панели n событий колеса: больше нуля — вверх.
// Указатель — в середине панели, как если бы колесо крутили над ней.
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
	btn := 64 // колесо вверх
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

// Scroll прокручивает историю панели на lines строк: больше нуля — вверх (панель
// входит в просмотр истории), меньше — вниз; дойдя до конца, панель из него выходит.
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

// WindowSize — текущий размер окна сессии. Он один на всех подключённых клиентов.
func (t *Tmux) WindowSize(name string) (cols, rows int, err error) {
	out, err := t.run(nil, "display-message", "-p", "-t", "="+name+":", "#{window_width} #{window_height}")
	if err != nil {
		return 0, 0, err
	}
	_, err = fmt.Sscan(out, &cols, &rows)
	return cols, rows, err
}

// Capture возвращает историю панели с цветами.
func (t *Tmux) Capture(name string, lines int) (string, error) {
	return t.run(nil, "capture-pane", "-p", "-e", "-J", "-S", "-"+strconv.Itoa(lines), "-t", "="+name+":")
}

// AttachCommand — команда клиента tmux для PTY.
func (t *Tmux) AttachCommand(name string) *exec.Cmd {
	cmd := t.Command("attach-session", "-t", "="+name)
	cmd.Env = append(cmd.Env, "TERM=xterm-256color", "COLORTERM=truecolor")
	return cmd
}
