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
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("old\n"), 0o640)

	// An edit based on the version on disk succeeds; file permissions are preserved.
	var res WriteResult
	e.must(t, "write", writeArgs{Folder: "docs", Path: "a.md", Content: "new\n", Base: Version([]byte("old\n"))}, &res)
	if res.Conflict || res.Version != Version([]byte("new\n")) {
		t.Fatalf("write: %+v", res)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.md"))
	st, _ := os.Stat(filepath.Join(dir, "a.md"))
	if string(b) != "new\n" || st.Mode().Perm() != 0o640 {
		t.Fatalf("on disk %q, permissions %v", b, st.Mode().Perm())
	}

	// The agent changed the file: an edit based on the old version does not overwrite it.
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("agent\n"), 0o640)
	seen := res.Version
	res = WriteResult{}
	e.must(t, "write", writeArgs{Folder: "docs", Path: "a.md", Content: "mine\n", Base: seen}, &res)
	if !res.Conflict || res.Version != Version([]byte("agent\n")) {
		t.Fatalf("conflict: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.md")); string(b) != "agent\n" {
		t.Fatalf("agent's file was overwritten: %q", b)
	}

	// A new file has an empty version; a second write with an empty one is a conflict.
	res = WriteResult{}
	e.must(t, "write", writeArgs{Folder: "docs", Path: "sub/new.txt", Content: "x"}, &res)
	if res.Conflict {
		t.Fatalf("new file: %+v", res)
	}
	res = WriteResult{}
	e.must(t, "write", writeArgs{Folder: "docs", Path: "sub/new.txt", Content: "y"}, &res)
	if !res.Conflict {
		t.Fatal("existing file was overwritten as new")
	}

	// A symlink in place of the file, escaping the folder, a symlinked directory pointing outside: all refused.
	os.WriteFile(filepath.Join(e.home, "secret"), []byte("s"), 0o600)
	os.Symlink(filepath.Join(e.home, "secret"), filepath.Join(dir, "link"))
	if e.call(t, "write", writeArgs{Folder: "docs", Path: "link", Content: "z", Base: Version([]byte("s"))}, nil) == nil {
		t.Fatal("write through a symlink")
	}
	if e.call(t, "write", writeArgs{Folder: "docs", Path: "../secret", Content: "z", Base: Version([]byte("s"))}, nil) == nil {
		t.Fatal("escaping the folder")
	}
	os.Symlink(e.home, filepath.Join(dir, "up"))
	if e.call(t, "write", writeArgs{Folder: "docs", Path: "up/secret", Content: "z", Base: Version([]byte("s"))}, nil) == nil {
		t.Fatal("write through a symlinked directory pointing outside")
	}
	if b, _ := os.ReadFile(filepath.Join(e.home, "secret")); string(b) != "s" {
		t.Fatalf("file outside the folder was modified: %q", b)
	}

	// No temp files are left behind.
	ents, _ := os.ReadDir(dir)
	for _, en := range ents {
		if filepath.Ext(en.Name()) == ".laavu-save" {
			t.Fatalf("temp file left behind: %s", en.Name())
		}
	}
}
