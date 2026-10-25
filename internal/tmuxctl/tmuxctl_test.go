package tmuxctl

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func newTest(t *testing.T) *Tmux {
	t.Helper()
	sock := fmt.Sprintf("laavu-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	conf := t.TempDir() + "/tmux.conf"
	os.WriteFile(conf, []byte("set -g remain-on-exit on\nset -g status off\nset -g history-limit 5000\n"), 0o644)
	tm := &Tmux{Socket: sock, Conf: conf}
	t.Cleanup(func() { tm.run(nil, "kill-server") })
	return tm
}

func TestLifecycle(t *testing.T) {
	tm := newTest(t)
	if ss, srv, err := tm.List(); err != nil || len(ss) != 0 || srv.PID != 0 {
		t.Fatalf("без сервера: %v %v %v", ss, srv, err)
	}
	dir := t.TempDir()
	err := tm.New(NewOpts{Name: "work-1", Dir: dir, Command: []string{"sh", "-c", "cat"}, Options: map[string]string{OptKind: "shell", OptAuthor: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	ss, srv, err := tm.List()
	if err != nil || len(ss) != 1 {
		t.Fatalf("список: %v %v", ss, err)
	}
	s := ss[0]
	if s.Name != "work-1" || s.Path != dir || s.Kind != "shell" || s.Author != "alice" || srv.PID == 0 || srv.Start == 0 {
		t.Fatalf("поля сессии: %+v %+v", s, srv)
	}
	if !tm.Has("work-1") || tm.Has("work") {
		t.Fatal("has-session должен искать точное имя")
	}
	if err := tm.Paste("work-1", "строка один\nстрока два", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		out, _ := tm.Capture("work-1", 100)
		return strings.Contains(out, "строка два")
	})
	if err := tm.Rename("work-1", "work-2"); err != nil {
		t.Fatal(err)
	}
	ss, _, _ = tm.List()
	if ss[0].Name != "work-2" || ss[0].Author != "alice" {
		t.Fatalf("опции должны переехать с переименованием: %+v", ss[0])
	}
	// Смерть процесса: панель остаётся с remain-on-exit.
	tm.Keys("work-2", "C-d")
	waitFor(t, func() bool {
		ss, _, _ := tm.List()
		return len(ss) == 1 && ss[0].PaneDead
	})
	if err := tm.Respawn("work-2", dir, []string{"sh", "-c", "cat"}); err != nil {
		t.Fatal(err)
	}
	ss, _, _ = tm.List()
	if ss[0].PaneDead {
		t.Fatal("после respawn панель должна ожить")
	}
	if err := tm.Kill("work-2"); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("не дождались")
}
