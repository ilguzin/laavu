package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite(t *testing.T) {
	e := start(t)
	dir := filepath.Join(e.home, "docs")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("старое\n"), 0o640)

	// Правка от той версии, что на диске, — проходит; права файла сохраняются.
	var res WriteResult
	e.must(t, "write", writeArgs{Folder: "docs", Path: "a.md", Content: "новое\n", Base: Version([]byte("старое\n"))}, &res)
	if res.Conflict || res.Version != Version([]byte("новое\n")) {
		t.Fatalf("запись: %+v", res)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.md"))
	st, _ := os.Stat(filepath.Join(dir, "a.md"))
	if string(b) != "новое\n" || st.Mode().Perm() != 0o640 {
		t.Fatalf("на диске %q, права %v", b, st.Mode().Perm())
	}

	// Агент поменял файл — правка от старой версии не перезаписывает его.
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("агент\n"), 0o640)
	seen := res.Version
	res = WriteResult{}
	e.must(t, "write", writeArgs{Folder: "docs", Path: "a.md", Content: "моё\n", Base: seen}, &res)
	if !res.Conflict || res.Version != Version([]byte("агент\n")) {
		t.Fatalf("конфликт: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.md")); string(b) != "агент\n" {
		t.Fatalf("файл агента перезаписан: %q", b)
	}

	// Новый файл — с пустой версией; второй раз с пустой — конфликт.
	res = WriteResult{}
	e.must(t, "write", writeArgs{Folder: "docs", Path: "sub/new.txt", Content: "x"}, &res)
	if res.Conflict {
		t.Fatalf("новый файл: %+v", res)
	}
	res = WriteResult{}
	e.must(t, "write", writeArgs{Folder: "docs", Path: "sub/new.txt", Content: "y"}, &res)
	if !res.Conflict {
		t.Fatal("существующий файл перезаписан как новый")
	}

	// Симлинк на месте файла, выход за папку, симлинк-каталог наружу — отказ.
	os.WriteFile(filepath.Join(e.home, "secret"), []byte("s"), 0o600)
	os.Symlink(filepath.Join(e.home, "secret"), filepath.Join(dir, "link"))
	if e.call(t, "write", writeArgs{Folder: "docs", Path: "link", Content: "z", Base: Version([]byte("s"))}, nil) == nil {
		t.Fatal("запись через симлинк")
	}
	if e.call(t, "write", writeArgs{Folder: "docs", Path: "../secret", Content: "z", Base: Version([]byte("s"))}, nil) == nil {
		t.Fatal("выход за папку")
	}
	os.Symlink(e.home, filepath.Join(dir, "up"))
	if e.call(t, "write", writeArgs{Folder: "docs", Path: "up/secret", Content: "z", Base: Version([]byte("s"))}, nil) == nil {
		t.Fatal("запись через каталог-симлинк наружу")
	}
	if b, _ := os.ReadFile(filepath.Join(e.home, "secret")); string(b) != "s" {
		t.Fatalf("файл вне папки изменён: %q", b)
	}

	// Временных файлов не остаётся.
	ents, _ := os.ReadDir(dir)
	for _, en := range ents {
		if filepath.Ext(en.Name()) == ".laavu-save" {
			t.Fatalf("остался временный файл %s", en.Name())
		}
	}
}
