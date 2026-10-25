package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(g *Guard, in map[string]any) map[string]string {
	b, _ := json.Marshal(in)
	var out bytes.Buffer
	g.Run(bytes.NewReader(b), &out)
	if out.Len() == 0 {
		return nil
	}
	var res map[string]string
	json.Unmarshal(out.Bytes(), &res)
	return res
}

func TestGuardUnsaved(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("нет git")
	}
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init", "-q").Run()
	g := &Guard{StateDir: t.TempDir(), Every: 3, Threshold: 65, Now: time.Now}
	sid := "s1"
	stop := func(active bool) map[string]string {
		return run(g, map[string]any{"hook_event_name": "Stop", "session_id": sid, "stop_hook_active": active, "transcript_path": "/нет"})
	}
	if r := stop(false); r != nil {
		t.Fatalf("без правок не должно быть block: %v", r)
	}
	f := filepath.Join(repo, "a.txt")
	os.WriteFile(f, []byte("x"), 0o600)
	run(g, map[string]any{"hook_event_name": "PostToolUse", "session_id": sid, "tool_name": "Write", "tool_input": map[string]string{"file_path": f}})
	r := stop(false)
	if r["decision"] != "block" || !strings.Contains(r["reason"], repo) {
		t.Fatalf("ожидался block с репозиторием: %v", r)
	}
	if stop(true) != nil {
		t.Fatal("при stop_hook_active хук молчит")
	}
	// Не чаще раза в Every ходов.
	for i := 0; i < 2; i++ {
		if r := stop(false); r != nil {
			t.Fatalf("ход %d: повтор слишком рано: %v", i, r)
		}
	}
	if r := stop(false); r == nil {
		t.Fatal("через Every ходов напоминание повторяется")
	}
}

func TestGuardContext(t *testing.T) {
	g := &Guard{StateDir: t.TempDir(), Every: 5, Threshold: 65, Now: time.Now}
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(tr, []byte(fmt.Sprintf(`{"type":"assistant","message":{"model":"claude-sonnet-4","usage":{"input_tokens":1,"cache_read_input_tokens":%d}}}`+"\n", 150000)), 0o600)
	r := run(g, map[string]any{"hook_event_name": "Stop", "session_id": "s", "transcript_path": tr})
	if r["decision"] != "block" || !strings.Contains(r["reason"], "/compact") {
		t.Fatalf("75%% контекста: %v", r)
	}
	// Точное значение строки статуса важнее таблицы.
	var out bytes.Buffer
	Statusline(strings.NewReader(`{"session_id":"s2","model":{"id":"m","display_name":"Opus"},"context_window":{"context_window_size":1000000,"used_percentage":12.4,"current_usage":{"input_tokens":124000}}}`), &out, g.StateDir)
	if out.String() != "Opus · контекст 12%" {
		t.Fatalf("строка статуса: %q", out.String())
	}
	os.WriteFile(tr, []byte(`{"type":"assistant","message":{"model":"claude-sonnet-4","usage":{"input_tokens":190000}}}`+"\n"), 0o600)
	if r := run(g, map[string]any{"hook_event_name": "Stop", "session_id": "s2", "transcript_path": tr}); r != nil {
		t.Fatalf("по строке статуса 12%% — без block: %v", r)
	}
}
