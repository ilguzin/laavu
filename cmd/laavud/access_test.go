package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/store"
)

// testConf creates a configuration directory with the given agents and a state dir.
func testConf(t *testing.T, agents ...string) string {
	dir := t.TempDir()
	d := config.Defaults()
	d.StateDir = t.TempDir()
	if err := config.WriteDaemon(filepath.Join(dir, "laavud.toml"), d); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(dir, "agents"), 0o755)
	for _, a := range agents {
		os.WriteFile(filepath.Join(dir, "agents", a+".toml"), []byte("name = \""+a+"\"\n"), 0o644)
	}
	return dir
}

func withStore(t *testing.T, dir string, f func(*store.Store)) {
	st, _, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f(st)
}

func TestAccessCommand(t *testing.T) {
	dir := testConf(t, "demo", "research")
	var uid int64
	withStore(t, dir, func(st *store.Store) {
		u, _ := st.CreateUser("sasha", "member")
		uid = u.ID
	})
	if err := access([]string{"grant", "--config-dir", dir, "sasha", "demo", "research"}); err != nil {
		t.Fatal(err)
	}
	if access([]string{"grant", "--config-dir", dir, "sasha", "nope"}) == nil {
		t.Fatal("unknown agent must be rejected")
	}
	if access([]string{"grant", "--config-dir", dir, "nobody", "demo"}) == nil {
		t.Fatal("unknown user must be rejected")
	}
	if err := access([]string{"revoke", "--config-dir", dir, "sasha", "research"}); err != nil {
		t.Fatal(err)
	}
	if err := access([]string{"grant", "--config-dir", dir, "sasha", "research"}); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "agents", "research.toml"))
	if err := access([]string{"drop", "--config-dir", dir, "research"}); err != nil {
		t.Fatal(err)
	}
	if err := access([]string{"list", "--config-dir", dir}); err != nil {
		t.Fatal(err)
	}
	withStore(t, dir, func(st *store.Store) {
		if got, _ := st.AgentsOf(uid); !reflect.DeepEqual(got, []string{"demo"}) {
			t.Fatal("access after grant and revoke:", got)
		}
	})
}

func TestInviteAgents(t *testing.T) {
	dir := testConf(t, "demo", "research")
	if invite([]string{"--config-dir", dir, "-q", "--agent", "nope"}) == nil {
		t.Fatal("unknown agent must be rejected")
	}
	if invite([]string{"--config-dir", dir, "-q", "--admin", "--agent", "demo"}) == nil {
		t.Fatal("--agent with --admin must be rejected")
	}
	if err := invite([]string{"--config-dir", dir, "-q", "--agent", "demo,research"}); err != nil {
		t.Fatal(err)
	}

	one := testConf(t, "demo")
	got, err := pickAgents(one)
	if err != nil || !reflect.DeepEqual(got, []string{"demo"}) {
		t.Fatal("a single agent is the default:", got, err)
	}
}

func TestParsePick(t *testing.T) {
	all := []string{"demo", "research", "site"}
	for answer, want := range map[string][]string{
		"":           nil,
		"all":        all,
		"2":          {"research"},
		"1, site, 1": {"demo", "site"},
		"research,3": {"research", "site"},
	} {
		got, err := parsePick(answer, all)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v, %v; want %v", answer, got, err, want)
		}
	}
	for _, bad := range []string{"4", "0", "nope"} {
		if _, err := parsePick(bad, all); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
