package worker

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/tmuxctl"
	"github.com/ilguzin/laavu/internal/wire"
	"strconv"
	"syscall"
)

type env struct {
	w    *Worker
	sock string
	home string
}

func start(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	conf := filepath.Join(t.TempDir(), "tmux.conf")
	os.WriteFile(conf, []byte("set -g remain-on-exit on\nset -g status off\n"), 0o644)
	ag := config.Agent{Name: "test", Home: home, Ports: [2]int{20000, 20999},
		TmuxSocket: fmt.Sprintf("laavu-wt-%d-%d", os.Getpid(), time.Now().UnixNano())}
	ag.Fill()
	w, err := New(Options{Agent: ag, TmuxConf: conf})
	if err != nil {
		t.Fatal(err)
	}
	w.Shell = "/bin/sh"
	sock := filepath.Join(t.TempDir(), "w.sock")
	l, err := Listener(sock)
	if err != nil {
		t.Fatal(err)
	}
	go w.Serve(l)
	t.Cleanup(func() {
		l.Close()
		killTmux(ag.TmuxSocket)
	})
	return &env{w: w, sock: sock, home: home}
}

func (e *env) call(t *testing.T, op string, args, out any) error {
	t.Helper()
	c, err := wire.Dial(e.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.Send(op, args, out)
}

func (e *env) must(t *testing.T, op string, args, out any) {
	t.Helper()
	if err := e.call(t, op, args, out); err != nil {
		t.Fatalf("%s: %v", op, err)
	}
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 60; i++ {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

func TestWorkdirsAndSessions(t *testing.T) {
	e := start(t)
	os.MkdirAll(filepath.Join(e.home, ".claude"), 0o700)
	os.MkdirAll(filepath.Join(e.home, "tmp"), 0o700)
	e.must(t, "mkdir", map[string]string{"name": "Demo"}, nil)
	if e.call(t, "mkdir", map[string]string{"name": ".ssh"}, nil) == nil {
		t.Fatal("reserved name")
	}
	if e.call(t, "mkdir", map[string]string{"name": "Demo"}, nil) == nil {
		t.Fatal("repeated creation")
	}
	var wds []Workdir
	e.must(t, "workdirs", nil, &wds)
	if len(wds) != 1 || wds[0].Name != "Demo" || wds[0].Git {
		t.Fatalf("workdirs: %+v", wds)
	}
	// A folder with git: branch and number of uncommitted changes.
	repo := filepath.Join(e.home, "Repo")
	if out, err := exec.Command("git", "init", "-q", "-b", "trunk", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o600)
	os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b"), 0o600)
	e.must(t, "workdirs", nil, &wds)
	if len(wds) != 2 || wds[1].Name != "Repo" || !wds[1].Git || wds[1].Branch != "trunk" || wds[1].Changes != 2 {
		t.Fatalf("workdirs with git: %+v", wds)
	}
	os.RemoveAll(repo)

	var si SessionInfo
	e.must(t, "new", NewArgs{Folder: "Demo", Kind: "shell", Author: "alice"}, &si)
	if si.Name != "Demo-1" {
		t.Fatalf("default name: %q", si.Name)
	}
	e.must(t, "new", NewArgs{Folder: "Demo", Kind: "shell"}, &si)
	if si.Name != "Demo-2" {
		t.Fatalf("second name: %q", si.Name)
	}
	if e.call(t, "new", NewArgs{Folder: "tmp", Kind: "shell"}, nil) == nil {
		t.Fatal("session in a reserved folder")
	}
	// A session created bypassing the worker, the way the agent creates it.
	exec.Command("tmux", "-L", e.w.Agent.TmuxSocket, "new", "-d", "-s", "dev-site", "-c", e.home, "sleep 300").Run()

	var ss []SessionInfo
	ss = nil
	e.must(t, "sessions", nil, &ss)
	if len(ss) != 3 {
		t.Fatalf("sessions: %+v", ss)
	}
	byName := map[string]SessionInfo{}
	for _, s := range ss {
		byName[s.Name] = s
	}
	if byName["Demo-1"].Folder != "Demo" || byName["Demo-1"].Author != "alice" || byName["Demo-1"].Kind != "shell" {
		t.Fatalf("Demo-1: %+v", byName["Demo-1"])
	}
	if byName["dev-site"].Folder != "" || byName["dev-site"].Kind != "other" {
		t.Fatalf("dev-site: %+v", byName["dev-site"])
	}

	// Input field: multi-line text in a single paste.
	e.must(t, "input", InputArgs{Name: "Demo-1", Text: "echo laavu-$((40+2))", Enter: true}, nil)
	eventually(t, "command output", func() bool {
		var h map[string]string
		e.call(t, "history", map[string]string{"name": "Demo-1"}, &h)
		return strings.Contains(h["text"], "laavu-42")
	})

	// Renaming moves the snapshot entry.
	e.must(t, "rename", map[string]string{"name": "Demo-2", "to": "review"}, nil)
	snap, _ := e.w.loadSnapshot()
	if snap.find("Demo-2") >= 0 || snap.find("review") < 0 {
		t.Fatalf("snapshot after rename: %+v", snap)
	}

	// "Reboot": the tmux server died, sessions become sleeping.
	exec.Command("tmux", "-L", e.w.Agent.TmuxSocket, "kill-server").Run()
	ss = nil
	e.must(t, "sessions", nil, &ss)
	if len(ss) != 3 {
		t.Fatalf("sleeping sessions must remain after reboot: %+v", ss)
	}
	for _, s := range ss {
		if !s.Sleeping || s.Status != "sleeping" {
			t.Fatalf("not sleeping: %+v", s)
		}
	}
	// Bringing up one session does not erase the other sleeping ones.
	var er EnsureResult
	e.must(t, "ensure", EnsureArgs{Name: "review"}, &er)
	if !er.Started {
		t.Fatal("ensure should have brought the session up")
	}
	ss = nil
	e.must(t, "sessions", nil, &ss)
	sleeping := 0
	for _, s := range ss {
		if s.Sleeping {
			sleeping++
		}
		if s.Name == "review" && (s.Sleeping || s.Folder != "Demo") {
			t.Fatalf("review: %+v", s)
		}
	}
	if len(ss) != 3 || sleeping != 2 {
		t.Fatalf("after bring-up: %+v", ss)
	}
	// Closing a sleeping session removes it from the snapshot.
	e.must(t, "kill", map[string]string{"name": "dev-site"}, nil)
	e.must(t, "kill", map[string]string{"name": "review"}, nil)
	ss = nil
	e.must(t, "sessions", nil, &ss)
	if len(ss) != 1 || ss[0].Name != "Demo-1" {
		t.Fatalf("after close: %+v", ss)
	}
}

func TestDeadPaneAndRespawn(t *testing.T) {
	e := start(t)
	os.MkdirAll(filepath.Join(e.home, "w"), 0o700)
	e.must(t, "new", NewArgs{Name: "s", Folder: "w", Kind: "shell"}, nil)
	e.must(t, "keys", map[string]any{"name": "s", "keys": []string{"C-d"}}, nil)
	var ss []SessionInfo
	eventually(t, "pane died", func() bool {
		ss = nil
		e.call(t, "sessions", nil, &ss)
		return len(ss) == 1 && ss[0].Dead && ss[0].Status == "exited"
	})
	e.must(t, "respawn", EnsureArgs{Name: "s"}, nil)
	ss = nil
	e.must(t, "sessions", nil, &ss)
	if ss[0].Dead {
		t.Fatal("after respawn")
	}
	if e.call(t, "keys", map[string]any{"name": "s", "keys": []string{"q"}}, nil) == nil {
		t.Fatal("arbitrary keys must be rejected")
	}
	// Leaving history view (wheel scrolling) before input.
	inMode := func() string {
		out, _ := e.w.Tmux.Command("display-message", "-p", "-t", "=s:", "#{pane_in_mode}").Output()
		return strings.TrimSpace(string(out))
	}
	// The second session is newer than the first: leaving the mode must target the right one.
	e.must(t, "new", NewArgs{Name: "other", Folder: "w", Kind: "shell"}, nil)
	e.must(t, "scroll", map[string]any{"name": "s", "lines": 3}, nil)
	if inMode() != "1" {
		t.Fatal("scrolling up did not enter history view")
	}
	e.must(t, "keys", map[string]any{"name": "s", "keys": []string{"CancelMode"}}, nil)
	if inMode() != "0" {
		t.Fatal("CancelMode did not leave history view")
	}
	// Outside the mode it breaks nothing.
	e.must(t, "keys", map[string]any{"name": "s", "keys": []string{"CancelMode", "Enter"}}, nil)

	// A mouse-aware program on the alternate screen (like Claude Code): a swipe
	// reaches it as SGR wheel events, one per 3 lines, not as copy-mode.
	got := filepath.Join(e.home, "mouse.txt")
	script := `printf '\033[?1049h\033[?1003h\033[?1006h'; stty raw -echo; exec cat -v > ` + got
	if out, err := e.w.Tmux.Command("new-session", "-d", "-s", "m", "-x", "80", "-y", "24", "sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("tmux new: %v %s", err, out)
	}
	eventually(t, "program enabled the mouse", func() bool {
		mouse, sgr := e.w.Tmux.MouseApp("m")
		return mouse && sgr
	})
	e.must(t, "scroll", map[string]any{"name": "m", "lines": 4}, nil)
	e.must(t, "scroll", map[string]any{"name": "m", "lines": 2}, nil)
	e.must(t, "scroll", map[string]any{"name": "m", "lines": -3}, nil)
	eventually(t, "wheel events", func() bool {
		b, _ := os.ReadFile(got)
		return string(b) == "^[[<64;41;13M^[[<64;41;13M^[[<65;41;13M"
	})
	out, _ := e.w.Tmux.Command("display-message", "-p", "-t", "=m:", "#{pane_in_mode}").Output()
	if strings.TrimSpace(string(out)) != "0" {
		t.Fatal("copy-mode was entered for a mouse-aware program")
	}
}

func TestAttach(t *testing.T) {
	e := start(t)
	os.MkdirAll(filepath.Join(e.home, "w"), 0o700)
	e.must(t, "new", NewArgs{Name: "s", Folder: "w", Kind: "shell"}, nil)
	c, err := wire.Dial(e.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Send("attach", AttachArgs{Name: "s", Cols: 100, Rows: 30}, nil); err != nil {
		t.Fatal(err)
	}
	c.WriteFrame(wire.FrameData, []byte("echo attach-$((6*7))\r"))
	got := ""
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(got, "attach-42") && time.Now().Before(deadline) {
		c.SetReadDeadline(deadline)
		typ, p, err := c.ReadFrame()
		if err != nil {
			t.Fatal(err, got)
		}
		if typ == wire.FrameData {
			got += string(p)
		}
	}
	if !strings.Contains(got, "attach-42") {
		t.Fatal("no output in the terminal")
	}
	c.WriteFrame(wire.FrameResize, wire.ResizePayload(120, 40))
	c.WriteFrame(wire.FrameEnd, nil)
	// The session lives on after the client closes.
	time.Sleep(200 * time.Millisecond)
	if !e.w.Tmux.Has("s") {
		t.Fatal("session died together with the client")
	}
}

func readAll(t *testing.T, c *wire.Conn) string {
	var b strings.Builder
	for {
		typ, p, err := c.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if typ == wire.FrameEnd {
			return b.String()
		}
		b.Write(p)
	}
}

func TestFiles(t *testing.T) {
	e := start(t)
	wd := filepath.Join(e.home, "docs")
	os.MkdirAll(filepath.Join(wd, "sub"), 0o700)
	os.WriteFile(filepath.Join(wd, "a.md"), []byte("# Title\n"), 0o600)
	os.WriteFile(filepath.Join(wd, ".env"), []byte("X=1"), 0o600)
	os.WriteFile(filepath.Join(wd, "blob"), []byte{1, 2, 0, 3}, 0o600)
	os.WriteFile(filepath.Join(wd, ".laavu-uploads"), []byte("inbox\n"), 0o600)

	var ents []Entry
	e.must(t, "tree", treeArgs{Folder: "docs"}, &ents)
	if len(ents) != 3 || ents[0].Name != "sub" || ents[0].Type != "dir" {
		t.Fatalf("tree: %+v", ents)
	}
	e.must(t, "tree", treeArgs{Folder: "docs", Hidden: true}, &ents)
	if len(ents) != 5 {
		t.Fatalf("tree with hidden entries: %+v", ents)
	}

	c, _ := wire.Dial(e.sock)
	var meta FileMeta
	if err := c.Send("read", readArgs{Folder: "docs", Path: "a.md"}, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Kind != "markdown" || readAll(t, c) != "# Title\n" {
		t.Fatalf("read: %+v", meta)
	}
	c.Close()
	c, _ = wire.Dial(e.sock)
	c.Send("read", readArgs{Folder: "docs", Path: "blob", MetaOnly: true}, &meta)
	c.Close()
	if meta.Kind != "binary" {
		t.Fatalf("binary: %+v", meta)
	}
	if e.call(t, "read", readArgs{Folder: "docs", Path: ".env", Visible: true}, nil) == nil {
		t.Fatal("hidden file in preview mode")
	}
	if e.call(t, "read", readArgs{Folder: "docs", Path: "../.bashrc"}, nil) == nil {
		t.Fatal("escaping the folder")
	}

	// An upload without a chosen directory follows .laavu-uploads.
	up := func(dir, name, body string) UploadResult {
		c, _ := wire.Dial(e.sock)
		defer c.Close()
		if err := c.Send("upload", uploadArgs{Folder: "docs", Dir: dir, Name: name, Size: int64(len(body))}, nil); err != nil {
			t.Fatal(err)
		}
		c.WriteFrame(wire.FrameData, []byte(body))
		c.WriteFrame(wire.FrameEnd, nil)
		_, p, err := c.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		var r UploadResult
		json.Unmarshal(p, &r)
		return r
	}
	if r := up("", "photo.jpg", "jpeg"); r.Path != "inbox/photo.jpg" || r.Size != 4 {
		t.Fatalf("upload: %+v", r)
	}
	if r := up("", "photo.jpg", "jpeg2"); r.Path != "inbox/photo-1.jpg" {
		t.Fatalf("repeated upload: %+v", r)
	}
	if r := up("sub", "../../evil", "x"); r.Path != "sub/evil" {
		t.Fatalf("name with a path: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(wd, "inbox", "photo-1.jpg")); string(b) != "jpeg2" {
		t.Fatal("upload content")
	}

	// Changes without git go by time.
	var ch Changes
	e.must(t, "changes", changesArgs{Folder: "docs", Since: time.Now().Add(-time.Minute).Unix()}, &ch)
	if ch.Git || len(ch.Files) < 3 {
		t.Fatalf("changes: %+v", ch)
	}
	for _, f := range ch.Files {
		if strings.HasPrefix(f.Path, ".") {
			t.Fatalf("hidden file in changes: %+v", f)
		}
	}
}

func TestWatch(t *testing.T) {
	e := start(t)
	wd := filepath.Join(e.home, "w")
	os.MkdirAll(wd, 0o700)
	c, _ := wire.Dial(e.sock)
	defer c.Close()
	if err := c.Send("watch", map[string]string{"folder": "w"}, nil); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(wd, "new"), 0o700)
	time.Sleep(50 * time.Millisecond)
	os.WriteFile(filepath.Join(wd, "new", "f.txt"), []byte("x"), 0o600)
	got := map[string]bool{}
	deadline := time.Now().Add(3 * time.Second)
	for !got["new/f.txt"] && time.Now().Before(deadline) {
		c.SetReadDeadline(deadline)
		_, p, err := c.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		var ev WatchEvent
		json.Unmarshal(p, &ev)
		for _, f := range ev.Files {
			got[f] = true
		}
	}
	if !got["new/f.txt"] {
		t.Fatalf("events: %v", got)
	}
}

func TestGitChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	e := start(t)
	wd := filepath.Join(e.home, "repo")
	os.MkdirAll(wd, 0o700)
	g := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", wd, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(string(out))
		}
	}
	g("init", "-q")
	os.WriteFile(filepath.Join(wd, "a.txt"), []byte("1\n"), 0o600)
	g("add", ".")
	g("commit", "-qm", "first")
	since := time.Now().Unix() - 1
	os.WriteFile(filepath.Join(wd, "a.txt"), []byte("2\n"), 0o600)
	os.WriteFile(filepath.Join(wd, "b.txt"), []byte("new\n"), 0o600)
	var ch Changes
	e.must(t, "changes", changesArgs{Folder: "repo", Since: since}, &ch)
	if !ch.Git || len(ch.Uncommitted) != 2 {
		t.Fatalf("git: %+v", ch)
	}
	var d map[string]any
	e.must(t, "diff", diffArgs{Folder: "repo", Path: "b.txt", Since: since}, &d)
	if !strings.Contains(d["diff"].(string), "+new") {
		t.Fatalf("untracked file diff: %v", d)
	}
}

func TestReconcile(t *testing.T) {
	old := &Snapshot{Sessions: []SnapEntry{
		{Name: "a", Server: tmuxctl.Server{PID: 1, Start: 10}},
		{Name: "b", Server: tmuxctl.Server{PID: 2, Start: 20}},
		{Name: "c", Server: tmuxctl.Server{PID: 2, Start: 20}},
	}}
	srv := tmuxctl.Server{PID: 2, Start: 20}
	live := []tmuxctl.Session{{Name: "c", Kind: "claude"}, {Name: "d"}}
	got := Reconcile(old, live, srv)
	names := []string{}
	for _, e := range got.Sessions {
		names = append(names, e.Name+":"+e.Kind)
	}
	// a: previous run (sleeping), b: closed in this run, c: alive, d: new, created outside the client.
	if strings.Join(names, ",") != "c:claude,d:other,a:" {
		t.Fatalf("reconcile: %v", names)
	}
	// Without a server nothing is removed.
	if got := Reconcile(old, nil, tmuxctl.Server{}); len(got.Sessions) != 3 {
		t.Fatal("entries must remain without a server")
	}
}

func TestLastUsage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	lines := []string{
		`{"type":"user","message":{"content":"hi"}}`,
		`{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":2,"cache_creation_input_tokens":3042,"cache_read_input_tokens":67425,"output_tokens":10}}}`,
		`{"type":"assistant","isSidechain":true,"message":{"model":"x","usage":{"input_tokens":999999}}}`,
	}
	os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	used, model, err := LastUsage(p)
	if err != nil || used != 70479 || model != "claude-opus-5-5" {
		t.Fatal(used, model, err)
	}
	if WindowFor("claude-opus-5-5", used) != 1_000_000 || WindowFor("claude-sonnet-4", 1000) != 200_000 {
		t.Fatal("window")
	}
	_ = io.EOF
}

// An agent session starts through a login shell: variables from ~/.bashrc
// (for example, the Claude Code token) are visible to the agent.
func TestClaudeSeesBashrc(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	e := start(t)
	e.w.Shell = "/bin/bash"
	os.WriteFile(filepath.Join(e.home, ".profile"), []byte(`[ -f "$HOME/.bashrc" ] && . "$HOME/.bashrc"`+"\n"), 0o600)
	os.WriteFile(filepath.Join(e.home, ".bashrc"), []byte("case $- in *i*) ;; *) return;; esac\nexport LAAVU_TEST_TOKEN=from-bashrc\n"), 0o600)
	fake := filepath.Join(e.home, "fake-claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"token=$LAAVU_TEST_TOKEN args=$*\"\nsleep 30\n"), 0o700)
	e.w.Agent.Claude = fake
	os.MkdirAll(filepath.Join(e.home, "w"), 0o700)
	e.must(t, "new", NewArgs{Name: "c", Folder: "w", Kind: "claude"}, nil)
	eventually(t, "agent sees the variable from .bashrc", func() bool {
		var h map[string]string
		e.call(t, "history", map[string]string{"name": "c"}, &h)
		return strings.Contains(h["text"], "token=from-bashrc args=")
	})
}

// killTmux stops the test tmux server and waits for the processes in its panes:
// kill-server returns before they exit, and they may still write into a TempDir
// that the test is about to remove.
func killTmux(socket string) {
	out, _ := exec.Command("tmux", "-L", socket, "list-panes", "-a", "-F", "#{pane_pid}").Output()
	exec.Command("tmux", "-L", socket, "kill-server").Run()
	for _, f := range strings.Fields(string(out)) {
		pid, _ := strconv.Atoi(f)
		for i := 0; pid > 0 && i < 50 && syscall.Kill(pid, 0) == nil; i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
}
