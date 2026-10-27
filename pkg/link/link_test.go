package link_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilguzin/laavu/pkg/link"
	"github.com/ilguzin/laavu/pkg/link/linktest"
)

type stand struct {
	g      *linktest.Gateway
	url    string
	key    ed25519.PrivateKey
	helper *link.Helper
	mu     sync.Mutex
	execs  [][]string
	unlink int
}

func newStand(t *testing.T) *stand {
	t.Helper()
	dir := t.TempDir()
	link.ConfigFile = filepath.Join(dir, "link.toml")
	link.KeyFile = filepath.Join(dir, "link.key")
	link.SeenFile = filepath.Join(dir, "seen", "link-seen.json")
	link.HelperSocket = filepath.Join(dir, "helper.sock")
	st := &stand{g: linktest.New()}
	srv := httptest.NewServer(st.g)
	t.Cleanup(srv.Close)
	st.url = "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1"
	pub, err := link.GenerateKey(link.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	st.key, _ = link.ReadKey(link.KeyFile)
	_ = pub
	st.helper = &link.Helper{
		Peer: os.Getuid(),
		Exec: func(args []string) (string, int) {
			st.mu.Lock()
			st.execs = append(st.execs, args)
			st.mu.Unlock()
			return "ran " + strings.Join(args, " ") + " for lv-secret in /srv/laavu/agents/secret\nsecret: updated\nagent secret: removed 3 files", 0
		},
		Report: func() *link.Report {
			return &link.Report{Schema: link.ReportSchema, Agents: []link.AgentReport{{Name: "secret", Ports: "20000-20999", Worker: "failed"}},
				Certs:    []link.CertReport{{Name: "p3000-secret.view.example.org"}},
				Problems: []string{"agent secret worker: failed"}}
		},
		Agents: func() []string { return []string{"secret"} },
		Unlink: func() error { st.unlink++; return nil },
		Log:    func(string, ...any) {},
		Now:    time.Now,
	}
	l, err := net.Listen("unix", link.HelperSocket)
	if err != nil {
		t.Fatal(err)
	}
	go st.helper.Serve(l, time.Hour)
	t.Cleanup(func() { l.Close() })
	return st
}

func (st *stand) serverKey() string { return link.EncodeKey(st.key.Public().(ed25519.PublicKey)) }

func (st *stand) client(consoleKey ed25519.PublicKey) *link.Client {
	return &link.Client{URL: st.url, Key: st.key, ConsoleKey: consoleKey, Laavu: "test", Laavud: "test",
		Backend: &link.SocketBackend{Path: link.HelperSocket}, Config: link.LoadConfig,
		ReportEvery: time.Hour, PollEvery: 50 * time.Millisecond}
}

// run starts the link and stops it before the next test swaps the paths.
func (st *stand) run(t *testing.T, ck ed25519.PublicKey) chan error {
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		err := st.client(ck).Run(ctx)
		runErr <- err
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return runErr
}

func (st *stand) bindConfig(t *testing.T) {
	t.Helper()
	cfg := &link.Config{Console: st.url, ConsoleKey: st.g.PublicKey(), ServerKey: st.serverKey(), Bound: 1, Rights: link.DefaultRights()}
	if err := cfg.Save(-1); err != nil {
		t.Fatal(err)
	}
	st.g.Bound(st.serverKey())
}

func TestBindWithPendingCode(t *testing.T) {
	st := newStand(t)
	c := st.client(nil)
	codes := make(chan string, 1)
	c.OnPending = func(p link.Pending) { codes <- p.Code }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan ed25519.PublicKey, 1)
	errc := make(chan error, 1)
	go func() {
		k, err := c.Bind(ctx)
		errc <- err
		done <- k
	}()
	code := <-codes
	if err := st.g.Confirm(code); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if got := link.EncodeKey(<-done); got != st.g.PublicKey() {
		t.Fatalf("console key %s, want %s", got, st.g.PublicKey())
	}
}

func TestBindWithIssuedCode(t *testing.T) {
	st := newStand(t)
	c := st.client(nil)
	c.Code = st.g.Code()
	c.OnPending = func(link.Pending) { t.Error("issued code must not need a pending round") }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Bind(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedConsoleKey(t *testing.T) {
	st := newStand(t)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := st.client(other).Bind(ctx)
	if err == nil || !strings.Contains(err.Error(), "differs from the pinned") {
		t.Fatalf("want pinned key error, got %v", err)
	}
}

func TestJobs(t *testing.T) {
	st := newStand(t)
	st.bindConfig(t)
	ck, _ := link.ParseKey(st.g.PublicKey())
	runErr := st.run(t, ck)
	s, err := st.g.Server(st.serverKey(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// The first report comes right after connecting, without agent names.
	select {
	case rep := <-s.Reports:
		if strings.Contains(string(rep), "secret") || strings.Contains(string(rep), "20000") {
			t.Fatalf("report leaks agent names or ports: %s", rep)
		}
		if !strings.Contains(string(rep), "agent agent-1 worker") {
			t.Fatalf("problems are not redacted the expected way: %s", rep)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no report")
	}

	run := func(j link.Job) link.Result {
		t.Helper()
		r, err := s.Run(j, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	j := s.NewJob("firewall.allow", map[string]any{"port": 8080, "proto": "tcp"})
	if r := run(j); r.Status != link.StatusDone || strings.Contains(r.Output, "secret") {
		t.Fatalf("firewall.allow: %+v", r)
	}
	st.mu.Lock()
	got := st.execs[len(st.execs)-1]
	st.mu.Unlock()
	if !reflect.DeepEqual(got, []string{"firewall", "allow", "8080/tcp"}) {
		t.Fatalf("args %v", got)
	}

	if r := run(j); r.Status != link.StatusRefused || !strings.Contains(r.Output, "already ran") {
		t.Fatalf("replay: %+v", r)
	}
	if r := run(s.NewJob("agent.create", map[string]any{"name": "x"})); r.Status != link.StatusRefused {
		t.Fatalf("agents right is off by default: %+v", r)
	}
	if r := run(s.NewJob("firewall.allow", map[string]any{"port": "22; rm -rf /"})); r.Status != link.StatusRefused {
		t.Fatalf("bad params: %+v", r)
	}
	if r := run(s.NewJob("shell", map[string]any{"cmd": "id"})); r.Status != link.StatusRefused {
		t.Fatalf("unknown op: %+v", r)
	}
	other := s.NewJob("cleanup", nil)
	other.ServerKey = st.g.PublicKey()
	if r := run(other); r.Status != link.StatusRefused || !strings.Contains(r.Output, "another server") {
		t.Fatalf("wrong server: %+v", r)
	}
	old := s.NewJob("cleanup", nil)
	old.NotBefore, old.Expires = time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
	if r := run(old); r.Status != link.StatusExpired {
		t.Fatalf("expired: %+v", r)
	}

	// A job signed by someone else, e.g. a compromised gateway.
	_, forger, _ := ed25519.GenerateKey(rand.Reader)
	fj := s.NewJob("cleanup", nil)
	fm, _ := link.Sign(forger, link.TypeJob, fj)
	r, err := s.RunSigned(fj.ID, fm, 10*time.Second)
	if err != nil || r.Status != link.StatusRefused || !strings.Contains(r.Output, "signature") {
		t.Fatalf("forged job: %+v %v", r, err)
	}
	// A welcome signature replayed as a job.
	wj := s.NewJob("cleanup", nil)
	wm, _ := link.Sign(st.g.Key, link.TypeWelcome, wj)
	wm.Type = link.TypeJob
	if r, _ := s.RunSigned(wj.ID, wm, 10*time.Second); r.Status != link.StatusRefused {
		t.Fatalf("cross-type signature: %+v", r)
	}

	if r := run(s.NewJob("check", nil)); r.Status != link.StatusDone || strings.Contains(r.Output, "secret") {
		t.Fatalf("check: %+v", r)
	}

	// Rights change on the server reaches the console.
	cfg, _ := link.LoadConfig()
	cfg.Rights[link.RightNames] = true
	cfg.Save(-1)
	select {
	case r := <-s.Rights:
		if !r[link.RightNames] {
			t.Fatalf("rights %v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no rights message")
	}
	if r := run(s.NewJob("check", nil)); !strings.Contains(r.Output, "secret") {
		t.Fatalf("names right on, names expected: %+v", r)
	}

	// The owner runs laavu unlink: the console hears about it and Run ends.
	cfg.Unlinking = true
	cfg.Save(-1)
	select {
	case <-s.Unlink:
	case <-time.After(10 * time.Second):
		t.Fatal("no unlink message")
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after unlink")
	}
}

func TestConsoleUnlink(t *testing.T) {
	st := newStand(t)
	st.bindConfig(t)
	ck, _ := link.ParseKey(st.g.PublicKey())
	runErr := st.run(t, ck)
	s, err := st.g.Server(st.serverKey(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Run(s.NewJob(link.OpUnlink, nil), 10*time.Second)
	if err != nil || r.Status != link.StatusDone || st.unlink != 1 {
		t.Fatalf("unlink: %+v %v", r, err)
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestOpsArgs(t *testing.T) {
	cases := []struct {
		op     string
		params string
		want   []string
	}{
		{"firewall.deny", `{"port":443}`, []string{"firewall", "deny", "443"}},
		{"agent.create", `{"name":"research","memory":"2G"}`, []string{"agent", "create", "research", "--memory", "2G"}},
		{"agent.delete", `{"name":"research","keep_home":true}`, []string{"agent", "delete", "research", "--keep-home"}},
		{"agent.limits", `{"name":"research","tasks":512}`, []string{"agent", "limits", "research", "--tasks", "512"}},
		{"update.os", ``, []string{"update", "os"}},
	}
	for _, c := range cases {
		got, err := link.Ops[c.op].Args(json.RawMessage(c.params))
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %s: %v %v", c.op, c.params, got, err)
		}
	}
	bad := []struct{ op, params string }{
		{"firewall.allow", `{"port":0}`},
		{"firewall.allow", `{"port":22,"proto":"icmp"}`},
		{"firewall.allow", `{"port":22,"extra":1}`},
		{"agent.create", `{"name":"--help"}`},
		{"agent.create", `{"name":"ok","memory":"2G; reboot"}`},
		{"update.os", `{"force":true}`},
	}
	for _, c := range bad {
		if got, err := link.Ops[c.op].Args(json.RawMessage(c.params)); err == nil {
			t.Errorf("%s %s accepted: %v", c.op, c.params, got)
		}
	}
	for name, op := range link.Ops {
		if _, ok := link.DefaultRights()[op.Right]; !ok {
			t.Errorf("%s needs unknown right %s", name, op.Right)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for u, ok := range map[string]bool{
		"wss://link.example.com/v1": true,
		"ws://127.0.0.1:7800/v1":    true,
		"ws://localhost/v1":         true,
		"ws://link.example.com/v1":  false,
		"https://link.example.com":  false,
	} {
		if err := link.CheckURL(u); (err == nil) != ok {
			t.Errorf("%s: %v", u, err)
		}
	}
}
