package safepath

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func setup(t *testing.T) (string, *Root) {
	t.Helper()
	home := t.TempDir()
	wd := filepath.Join(home, "Demo")
	os.MkdirAll(filepath.Join(wd, "docs"), 0o700)
	os.MkdirAll(filepath.Join(wd, ".git"), 0o700)
	os.WriteFile(filepath.Join(wd, "docs", "a.md"), []byte("# a"), 0o600)
	os.WriteFile(filepath.Join(wd, ".env"), []byte("SECRET=1"), 0o600)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id"), []byte("key"), 0o600)
	os.Symlink(filepath.Join(home, ".ssh", "id"), filepath.Join(wd, "escape"))
	os.Symlink("../.ssh/id", filepath.Join(wd, "escape-rel"))
	os.Symlink(".env", filepath.Join(wd, "env-link"))
	os.Symlink("docs/a.md", filepath.Join(wd, "ok-link"))
	r, err := Open(home, "Demo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return home, r
}

func read(t *testing.T, f *os.File, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return string(b)
}

func TestOpenInside(t *testing.T) {
	_, r := setup(t)
	f, err := r.Open("docs/a.md")
	if read(t, f, err) != "# a" {
		t.Fatal("содержимое")
	}
	f, err = r.Open("/docs/../docs/a.md")
	if read(t, f, err) != "# a" {
		t.Fatal("нормализация пути")
	}
	f, err = r.Open("ok-link")
	if read(t, f, err) != "# a" {
		t.Fatal("симлинк внутри папки должен открываться")
	}
}

func TestEscapes(t *testing.T) {
	_, r := setup(t)
	for _, p := range []string{"../.ssh/id", "escape", "escape-rel", "docs/../../.ssh/id"} {
		if f, err := r.Open(p); err == nil {
			f.Close()
			t.Errorf("%s: выход за папку должен быть запрещён", p)
		}
	}
	if _, err := Open(filepath.Dir(r.Path()), "../etc"); err == nil {
		t.Error("имя рабочей папки с ..")
	}
}

func TestWorkdirSymlinkRejected(t *testing.T) {
	home := t.TempDir()
	os.Symlink("/etc", filepath.Join(home, "etc-link"))
	if _, err := Open(home, "etc-link"); err == nil {
		t.Fatal("рабочая папка-симлинк должна отклоняться")
	}
}

func TestVisible(t *testing.T) {
	_, r := setup(t)
	for _, p := range []string{".env", "env-link", ".git/config", "docs/../.env"} {
		if f, err := r.OpenVisible(p); err == nil {
			f.Close()
			t.Errorf("%s: скрытое должно быть закрыто", p)
		}
	}
	f, err := r.OpenVisible("ok-link")
	if read(t, f, err) != "# a" {
		t.Fatal("видимый симлинк")
	}
}

func TestCreateExclusiveAndMkdir(t *testing.T) {
	home, r := setup(t)
	if err := r.MkdirAll("inbox/photos"); err != nil {
		t.Fatal(err)
	}
	f, rel, err := r.CreateExclusive("inbox/photos", "a.jpg")
	if err != nil || rel != "inbox/photos/a.jpg" {
		t.Fatal(rel, err)
	}
	f.Close()
	f, rel, err = r.CreateExclusive("inbox/photos", "a.jpg")
	if err != nil || rel != "inbox/photos/a-1.jpg" {
		t.Fatal("занятое имя должно получить номер:", rel, err)
	}
	f.Close()
	os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(r.Path(), "sshdir"))
	if err := r.MkdirAll("sshdir/x"); err == nil {
		t.Fatal("mkdir через симлинк наружу")
	}
	if _, _, err := r.CreateExclusive("sshdir", "evil"); err == nil {
		t.Fatal("создание через симлинк наружу")
	}
	if _, _, err := r.CreateExclusive(".", "../x"); err == nil {
		t.Fatal("имя со слэшем")
	}
	// Симлинк на месте имени не перезаписывает цель.
	os.Symlink(filepath.Join(home, ".ssh", "id"), filepath.Join(r.Path(), "inbox", "trap"))
	f, rel, err = r.CreateExclusive("inbox", "trap")
	if err != nil || rel != "inbox/trap-1" {
		t.Fatal(rel, err)
	}
	f.Close()
	if b, _ := os.ReadFile(filepath.Join(home, ".ssh", "id")); string(b) != "key" {
		t.Fatal("цель симлинка испорчена")
	}
}

func TestClean(t *testing.T) {
	if _, err := Clean("../a"); !errors.Is(err, ErrOutside) {
		t.Error("..")
	}
	if c, _ := Clean("/a//b/./c"); c != "a/b/c" {
		t.Error(c)
	}
}
