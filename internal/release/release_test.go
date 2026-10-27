package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestSignVerifyFetch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "laavud-linux-amd64"), []byte("binary"), 0o755)
	os.WriteFile(filepath.Join(dir, "laavu-linux-amd64"), []byte("utility"), 0o755)
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
	// A tampered binary fails the hash check.
	os.WriteFile(filepath.Join(dir, "laavud-linux-amd64"), []byte("tampered"), 0o755)
	if v.Fetch("laavud-linux-amd64", dst) == nil {
		t.Fatal("tampered file accepted")
	}
	// A tampered manifest fails the signature check.
	m.Version = "9.9.9"
	WriteManifest(dir, m)
	if _, err := Verify(Source(dir), pub); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	// Foreign key.
	other, _ := GenerateKey(filepath.Join(t.TempDir(), "k"))
	op, _ := ParsePublic(other)
	Sign(dir, key)
	if _, err := Verify(Source(dir), op); err == nil {
		t.Fatal("signature by a foreign key accepted")
	}
}

// A key from `openssl genpkey -algorithm ed25519` (PKCS#8 PEM) signs as well.
func TestSignWithPEMKey(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "laavud-linux-amd64"), []byte("binary"), 0o755)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	key := filepath.Join(t.TempDir(), "release.pem")
	os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	m, _ := Build(dir, "0.2.0")
	WriteManifest(dir, m)
	if err := Sign(dir, key); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(Source(dir), pub); err != nil {
		t.Fatal(err)
	}
}

func TestNewer(t *testing.T) {
	if !Newer("0.10.0", "0.9.9") || Newer("v1.0.0", "1.0.0") || !Newer("1.0.1", "v1.0.0") || Newer("dev", "0.1.0") {
		t.Fatal("version comparison")
	}
}
