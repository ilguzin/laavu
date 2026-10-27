// fakeclaude is a Claude Code stub for tests without a model token.
//
// It calls hooks from ~/.claude/settings.json and from --settings the same way the real
// Claude Code does (input fields verified on the test stand, see docs/stand-claude-code.md),
// writes a transcript with token counters, and understands /clear, /compact and /exit.
// It "replies" to every message with an echo. A message with "write:path" writes a file via
// the "Write tool" (calling PreToolUse/PostToolUse with tool_input.file_path).
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type hookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type matcher struct {
	Matcher string    `json:"matcher"`
	Hooks   []hookCmd `json:"hooks"`
}

type settings struct {
	Hooks map[string][]matcher `json:"hooks"`
}

var (
	allHooks  = map[string][]hookCmd{}
	sessionID string
	transcr   string
	cwd       string
	tokens    int64
)

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func load(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var s settings
	if json.Unmarshal(b, &s) != nil {
		return
	}
	for ev, ms := range s.Hooks {
		for _, m := range ms {
			allHooks[ev] = append(allHooks[ev], m.Hooks...)
		}
	}
}

// run calls all hooks for an event and returns their stdout.
func run(event string, extra map[string]any) []string {
	in := map[string]any{
		"session_id": sessionID, "transcript_path": transcr, "cwd": cwd, "hook_event_name": event,
	}
	for k, v := range extra {
		in[k] = v
	}
	payload, _ := json.Marshal(in)
	var outs []string
	for _, h := range allHooks[event] {
		cmd := exec.Command("/bin/sh", "-c", h.Command)
		cmd.Stdin = bytes.NewReader(payload)
		cmd.Dir = cwd
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Run()
		if s := strings.TrimSpace(out.String()); s != "" {
			outs = append(outs, s)
		}
	}
	return outs
}

func newConversation() {
	sessionID = uuid()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".claude", "projects", strings.NewReplacer("/", "-", ".", "-").Replace(cwd))
	os.MkdirAll(dir, 0o700)
	transcr = filepath.Join(dir, sessionID+".jsonl")
	tokens = 20000
}

func appendTranscript(rec map[string]any) {
	f, err := os.OpenFile(transcr, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(rec)
	f.Write(append(b, '\n'))
}

func answer(text string) {
	tokens += int64(len(text))*50 + 5000
	appendTranscript(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	appendTranscript(map[string]any{"type": "assistant", "isSidechain": false, "message": map[string]any{
		"model": "claude-fake-5-5",
		"usage": map[string]any{"input_tokens": 2, "cache_creation_input_tokens": 1000, "cache_read_input_tokens": tokens, "output_tokens": 100},
	}})
}

func main() {
	resume := flag.String("resume", "", "")
	settingsFlag := flag.String("settings", "", "")
	appendPrompt := flag.String("append-system-prompt", "", "")
	flag.Parse()
	cwd, _ = os.Getwd()
	home, _ := os.UserHomeDir()
	load(filepath.Join(home, ".claude", "settings.json"))
	load(filepath.Join(cwd, ".claude", "settings.json"))
	if *settingsFlag != "" {
		load(*settingsFlag)
	}
	source := "startup"
	if *resume != "" {
		sessionID = *resume
		source = "resume"
		dir := filepath.Join(home, ".claude", "projects", strings.NewReplacer("/", "-", ".", "-").Replace(cwd))
		transcr = filepath.Join(dir, sessionID+".jsonl")
		tokens = 20000
	} else {
		newConversation()
	}
	// Bracketed paste, like the real TUI: tmux paste-buffer -p wraps the pasted text.
	fmt.Print("\x1b[?2004h")
	fmt.Printf("fakeclaude %s (%s)\r\n", sessionID, source)
	if *appendPrompt != "" {
		fmt.Printf("profile: %s\r\n", *appendPrompt)
	}
	run("SessionStart", map[string]any{"source": source, "model": "claude-fake-5-5"})
	fmt.Print("> ")
	r := bufio.NewReader(os.Stdin)
	var buf strings.Builder
	inPaste := false
	for {
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			run("SessionEnd", map[string]any{"reason": "other"})
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.Contains(line, "\x1b[200~") {
			inPaste = true
			line = strings.ReplaceAll(line, "\x1b[200~", "")
		}
		if strings.Contains(line, "\x1b[201~") {
			inPaste = false
			line = strings.ReplaceAll(line, "\x1b[201~", "")
		}
		buf.WriteString(line)
		if inPaste {
			buf.WriteString("\n")
			continue
		}
		msg := strings.TrimSpace(buf.String())
		buf.Reset()
		if msg == "" {
			fmt.Print("> ")
			continue
		}
		switch {
		case msg == "/exit":
			run("SessionEnd", map[string]any{"reason": "prompt_input_exit"})
			return
		case msg == "/clear":
			run("SessionEnd", map[string]any{"reason": "clear"})
			newConversation()
			run("SessionStart", map[string]any{"source": "clear"})
			fmt.Print("(cleared)\r\n> ")
			continue
		case msg == "/compact":
			run("PreCompact", map[string]any{"trigger": "manual"})
			tokens = 10000
			run("SessionStart", map[string]any{"source": "compact"})
			fmt.Print("(compacted)\r\n> ")
			continue
		}
		run("UserPromptSubmit", map[string]any{"prompt": msg})
		if p, ok := strings.CutPrefix(msg, "write:"); ok {
			p = strings.TrimSpace(p)
			if !filepath.IsAbs(p) {
				p = filepath.Join(cwd, p)
			}
			ti := map[string]any{"file_path": p, "content": "fake\n"}
			run("PreToolUse", map[string]any{"tool_name": "Write", "tool_input": ti})
			os.WriteFile(p, []byte("fake\n"), 0o644)
			run("PostToolUse", map[string]any{"tool_name": "Write", "tool_input": ti, "tool_response": map[string]any{}})
		}
		if d, ok := strings.CutPrefix(msg, "sleep:"); ok {
			if dur, err := time.ParseDuration(strings.TrimSpace(d)); err == nil {
				time.Sleep(dur)
			}
		}
		answer(msg)
		fmt.Printf("echo: %s\r\n", strings.ReplaceAll(msg, "\n", " / "))
		active := false
		for i := 0; i < 3; i++ {
			blocked := ""
			for _, out := range run("Stop", map[string]any{"stop_hook_active": active}) {
				var dec struct{ Decision, Reason string }
				if json.Unmarshal([]byte(out), &dec) == nil && dec.Decision == "block" {
					blocked = dec.Reason
				}
			}
			if blocked == "" {
				break
			}
			fmt.Printf("(hook) %s\r\n", blocked)
			answer("(continued by hook)")
			active = true
		}
		fmt.Print("> ")
	}
}
