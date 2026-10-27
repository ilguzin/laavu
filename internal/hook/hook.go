// Package hook implements what Claude Code and tmux hooks call on behalf of the agent.
//
// The main rule: the status hook prints nothing to stdout (SessionStart stdout
// ends up in the model's context), never waits longer than a second and always exits with code 0.
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

// DefaultSocket is the daemon socket for hook events.
const DefaultSocket = "/run/laavu/hooks/hook.sock"

// Event is what the hook sends to the daemon. It carries no conversation content:
// no prompt text, no replies, only the event name and identifiers.
type Event struct {
	Kind         string `json:"kind"` // claude | tmux
	Event        string `json:"event"`
	Session      string `json:"session,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Source       string `json:"source,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Transcript   string `json:"transcript,omitempty"`
	Notification string `json:"notification,omitempty"`
	// Tool and Target are the tool and path from PreToolUse, without content.
	Tool   string `json:"tool,omitempty"`
	Target string `json:"target,omitempty"`
}

// Input holds the Claude Code hook input fields we need.
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

// Send sends an event to the daemon. Errors are silently swallowed: if there is no daemon,
// the agent must work as usual.
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

// SessionName asks tmux itself for the tmux session name by TMUX_PANE.
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

// ReadInput reads the hook input from stdin.
func ReadInput(r io.Reader) (Input, error) {
	var in Input
	b, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return in, err
	}
	err = json.Unmarshal(b, &in)
	return in, err
}

// Claude is `laavud hook claude`: reports the session status to the daemon.
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

// ToolTarget returns the path the tool works on, relative to the session folder.
// Bash commands, search patterns, URLs and texts are not taken: the session list
// shows what the agent is doing, but not what it writes.
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

// Tmux is `laavud hook tmux <event>`: the agent's session list has changed.
func Tmux(event string) {
	Send(Event{Kind: "tmux", Event: event})
}
