package names

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkdir(t *testing.T) {
	r := NewReserved("private")
	ok := []string{"Demo", "site.v2", "my_project", "a-b", "x"}
	bad := []string{"", ".claude", ".x", "tmp", "bin", "node_modules", "private", "with space", "кириллица", "a/b", "..", "-flag"}
	for _, n := range ok {
		if err := r.Workdir(n); err != nil {
			t.Errorf("%q: неожиданная ошибка %v", n, err)
		}
	}
	for _, n := range bad {
		if err := r.Workdir(n); err == nil {
			t.Errorf("%q: должно быть запрещено", n)
		}
	}
}

func TestSessionAndAgent(t *testing.T) {
	for _, n := range []string{"Demo-2", "dev_site", "a"} {
		if Session(n) != nil {
			t.Errorf("сессия %q должна проходить", n)
		}
	}
	for _, n := range []string{"a.b", "a:b", "", "a b"} {
		if Session(n) == nil {
			t.Errorf("сессия %q должна отклоняться", n)
		}
	}
	if Agent("demo") != nil || Agent("Demo") == nil || Agent("1x") == nil || Agent("a-very-long-agent-name-xyz") == nil {
		t.Error("проверка имени агента")
	}
}

func TestLoadReserved(t *testing.T) {
	p := filepath.Join(t.TempDir(), "reserved")
	os.WriteFile(p, []byte("# комментарий\nprivate\n  cache # хвост\n\n"), 0o644)
	r, err := LoadReserved(p)
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsReserved("private") || !r.IsReserved("cache") || !r.IsReserved("tmp") || r.IsReserved("Demo") {
		t.Error("список прочитан неверно")
	}
	if _, err := LoadReserved(filepath.Join(t.TempDir(), "нет")); err != nil {
		t.Error("отсутствующий файл не должен быть ошибкой")
	}
}
