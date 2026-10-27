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
		t.Fatalf("without a server: %v %v %v", ss, srv, err)
	}
	dir := t.TempDir()
	err := tm.New(NewOpts{Name: "work-1", Dir: dir, Command: []string{"sh", "-c", "cat"}, Options: map[string]string{OptKind: "shell", OptAuthor: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	ss, srv, err := tm.List()
	if err != nil || len(ss) != 1 {
		t.Fatalf("list: %v %v", ss, err)
	}
	s := ss[0]
	if s.Name != "work-1" || s.Path != dir || s.Kind != "shell" || s.Author != "alice" || srv.PID == 0 || srv.Start == 0 {
		t.Fatalf("session fields: %+v %+v", s, srv)
	}
	if !tm.Has("work-1") || tm.Has("work") {
		t.Fatal("has-session must match the exact name")
	}
	if err := tm.Paste("work-1", "line one\nline two", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		out, _ := tm.Capture("work-1", 100)
		return strings.Contains(out, "line two")
	})
	if err := tm.Rename("work-1", "work-2"); err != nil {
		t.Fatal(err)
	}
	ss, _, _ = tm.List()
	if ss[0].Name != "work-2" || ss[0].Author != "alice" {
		t.Fatalf("options must survive the rename: %+v", ss[0])
	}
	// Process death: the pane stays thanks to remain-on-exit.
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
		t.Fatal("the pane must come back to life after respawn")
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
	t.Fatal("timed out waiting")
}

func TestIsNoServer(t *testing.T) {
	for _, msg := range []string{
		"no server running on /tmp/tmux-1000/x",
		"error connecting to /tmp/tmux-1000/x (Connection refused)",
		"server exited unexpectedly",
	} {
		if !isNoServer(msg) {
			t.Errorf("%q is a missing server", msg)
		}
	}
	if isNoServer("can't find session: demo") {
		t.Error("a missing session is not a missing server")
	}
}
