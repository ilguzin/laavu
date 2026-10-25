// Package hook — то, что вызывают хуки Claude Code и tmux от имени агента.
//
// Главное правило: хук статуса ничего не печатает в stdout (stdout SessionStart
// попадает в контекст модели), не ждёт дольше секунды и всегда завершается с кодом 0.
package hook

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultSocket — сокет демона для событий хуков.
const DefaultSocket = "/run/laavu/hooks/hook.sock"

// Event — то, что хук отправляет демону. Содержимого разговора здесь нет:
// ни текста промпта, ни ответов — только имя события и идентификаторы.
type Event struct {
	Kind         string `json:"kind"` // claude | tmux
	Event        string `json:"event"`
	Session      string `json:"session,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Source       string `json:"source,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Transcript   string `json:"transcript,omitempty"`
	Notification string `json:"notification,omitempty"`
	// Tool и Target — инструмент и путь из PreToolUse, без содержимого.
	Tool   string `json:"tool,omitempty"`
	Target string `json:"target,omitempty"`
}

// Input — поля входа хуков Claude Code, которые нам нужны.
type Input struct {
	HookEventName    string          `json:"hook_event_name"`
	SessionID        string          `json:"session_id"`
	TranscriptPath   string          `json:"transcript_path"`
	Cwd              string          `json:"cwd"`
	Source           string          `json:"source"`
	Reason           string          `json:"reason"`
	NotificationType string          `json:"notification_type"`
	StopHookActive   bool            `json:"stop_hook_active"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
}

func socketPath() string {
	if p := os.Getenv("LAAVU_HOOK_SOCK"); p != "" {
		return p
	}
	return DefaultSocket
}

// Send отправляет событие демону. Ошибки молча глотаются: если демона нет,
// агент должен работать как обычно.
func Send(ev Event) error {
	c, err := net.DialTimeout("unix", socketPath(), 500*time.Millisecond)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	b, _ := json.Marshal(ev)
	_, err = c.Write(append(b, '\n'))
	return err
}

// SessionName узнаёт имя tmux-сессии у самого tmux по TMUX_PANE.
func SessionName() string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" || os.Getenv("TMUX") == "" {
		return ""
	}
	cmd := exec.Command("tmux", "display-message", "-p", "-t", pane, "#S")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ReadInput читает вход хука со stdin.
func ReadInput(r io.Reader) (Input, error) {
	var in Input
	b, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return in, err
	}
	err = json.Unmarshal(b, &in)
	return in, err
}

// Claude — `laavud hook claude`: статус сессии в демон.
func Claude(stdin io.Reader) {
	in, err := ReadInput(stdin)
	if err != nil {
		return
	}
	name := SessionName()
	if name == "" {
		return
	}
	ev := Event{
		Kind: "claude", Event: in.HookEventName, Session: name, SessionID: in.SessionID,
		Source: in.Source, Reason: in.Reason, Transcript: in.TranscriptPath, Notification: in.NotificationType,
	}
	if in.HookEventName == "PreToolUse" {
		ev.Tool, ev.Target = in.ToolName, ToolTarget(in)
	}
	Send(ev)
}

// ToolTarget — путь, с которым работает инструмент, относительно рабочей папки
// сессии. Команды Bash, шаблоны поиска, адреса и тексты не берутся: в списке
// сессий видно, что агент делает, но не что пишет.
func ToolTarget(in Input) string {
	var ti struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Path         string `json:"path"`
	}
	json.Unmarshal(in.ToolInput, &ti)
	p := ti.FilePath
	if p == "" {
		p = ti.NotebookPath
	}
	if p == "" {
		p = ti.Path
	}
	if p == "" {
		return ""
	}
	if in.Cwd != "" && filepath.IsAbs(p) {
		if rel, err := filepath.Rel(in.Cwd, p); err == nil && !strings.HasPrefix(rel, "..") {
			p = rel
		}
	}
	if len(p) > 200 {
		i := len(p) - 200
		for i < len(p) && !utf8.RuneStart(p[i]) {
			i++
		}
		p = "…" + p[i:]
	}
	return p
}

// Tmux — `laavud hook tmux <событие>`: список сессий агента изменился.
func Tmux(event string) {
	Send(Event{Kind: "tmux", Event: event})
}
