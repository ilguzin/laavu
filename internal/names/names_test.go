package names

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkdir(t *testing.T) {
	r := NewReserved("private")
	ok := []string{"Demo", "site.v2", "my_project", "a-b", "x"}
	bad := []string{"", ".claude", ".x", "tmp", "bin", "node_modules", "private", "with space", "日本語", "a/b", "..", "-flag"}
	for _, n := range ok {
		if err := r.Workdir(n); err != nil {
			t.Errorf("%q: unexpected error %v", n, err)
		}
	}
	for _, n := range bad {
		if err := r.Workdir(n); err == nil {
			t.Errorf("%q: must be rejected", n)
		}
	}
}

func TestSessionAndAgent(t *testing.T) {
	for _, n := range []string{"Demo-2", "dev_site", "a"} {
		if Session(n) != nil {
			t.Errorf("session %q must be accepted", n)
		}
	}
	for _, n := range []string{"a.b", "a:b", "", "a b"} {
		if Session(n) == nil {
			t.Errorf("session %q must be rejected", n)
		}
	}
	if Agent("demo") != nil || Agent("Demo") == nil || Agent("1x") == nil || Agent("a-very-long-agent-name-xyz") == nil {
		t.Error("agent name validation")
	}
}

func TestLoadReserved(t *testing.T) {
	p := filepath.Join(t.TempDir(), "reserved")
	os.WriteFile(p, []byte("# comment\nprivate\n  cache # trailing\n\n"), 0o644)
	r, err := LoadReserved(p)
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsReserved("private") || !r.IsReserved("cache") || !r.IsReserved("tmp") || r.IsReserved("Demo") {
		t.Error("list read incorrectly")
	}
	if _, err := LoadReserved(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Error("a missing file must not be an error")
	}
}
