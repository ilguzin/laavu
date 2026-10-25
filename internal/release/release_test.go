package release

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSignVerifyFetch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "laavud-linux-amd64"), []byte("бинарник"), 0o755)
	os.WriteFile(filepath.Join(dir, "laavu-linux-amd64"), []byte("утилита"), 0o755)
	key := filepath.Join(t.TempDir(), "release.key")
	pubS, err := GenerateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Build(dir, "0.2.0")
	if err != nil || len(m.Files) != 2 {
		t.Fatal(m, err)
	}
	WriteManifest(dir, m)
	if err := Sign(dir, key); err != nil {
		t.Fatal(err)
	}
	pub, _ := ParsePublic(pubS)
	v, err := Verify(Source(dir), pub)
	if err != nil || v.Manifest.Version != "0.2.0" {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "laavud")
	if err := v.Fetch("laavud-linux-amd64", dst); err != nil {
		t.Fatal(err)
	}
	// Подменённый бинарник не проходит по хешу.
	os.WriteFile(filepath.Join(dir, "laavud-linux-amd64"), []byte("подмена"), 0o755)
	if v.Fetch("laavud-linux-amd64", dst) == nil {
		t.Fatal("подменённый файл принят")
	}
	// Подменённый манифест не проходит по подписи.
	m.Version = "9.9.9"
	WriteManifest(dir, m)
	if _, err := Verify(Source(dir), pub); err == nil {
		t.Fatal("подменённый манифест принят")
	}
	// Чужой ключ.
	other, _ := GenerateKey(filepath.Join(t.TempDir(), "k"))
	op, _ := ParsePublic(other)
	Sign(dir, key)
	if _, err := Verify(Source(dir), op); err == nil {
		t.Fatal("подпись чужим ключом принята")
	}
}

func TestNewer(t *testing.T) {
	if !Newer("0.10.0", "0.9.9") || Newer("v1.0.0", "1.0.0") || !Newer("1.0.1", "v1.0.0") || Newer("dev", "0.1.0") {
		t.Fatal("сравнение версий")
	}
}
