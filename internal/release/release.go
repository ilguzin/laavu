// Package release holds the release manifest and its signature by the project key.
//
// The signing key lives with the project owner, not in CI. CI builds the binaries and
// an unsigned manifest; the signature is added by hand (laavu release sign) and
// verified with the public key pinned on the server at install time.
package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProjectKey is the project public key (base64), embedded when building a release:
// -ldflags "-X github.com/ilguzin/laavu/internal/release.ProjectKey=...".
var ProjectKey = ""

// PinnedKeyFile is the key pinned on the server at install time.
const PinnedKeyFile = "/etc/laavu/release.pub"

// Manifest is the release contents.
type Manifest struct {
	Version string `json:"version"`
	Created string `json:"created"`
	Files   []File `json:"files"`
}

// File is a single artifact.
type File struct {
	Name   string `json:"name"` // laavud-linux-amd64
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

const (
	ManifestName  = "manifest.json"
	SignatureName = "manifest.json.sig"
)

// GenerateKey creates a key pair; the private key goes to a 0600 file, the public one is returned as a string.
func GenerateKey(privPath string) (string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(privPath, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// readKey reads a private key written by GenerateKey (base64) or by
// `openssl genpkey -algorithm ed25519` (PKCS#8 PEM).
func readKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if blk, _ := pem.Decode(b); blk != nil {
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if priv, ok := k.(ed25519.PrivateKey); err == nil && ok {
			return priv, nil
		}
		return nil, errors.New("file does not look like an ed25519 private key")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("file does not look like a release private key")
	}
	return ed25519.PrivateKey(raw), nil
}

// ParsePublic parses a base64 public key.
func ParsePublic(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("invalid release public key")
	}
	return ed25519.PublicKey(raw), nil
}

func fileHash(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// Build assembles the manifest from the directory's files (except the manifest and signature themselves).
func Build(dir, version string) (*Manifest, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Version: version, Created: time.Now().UTC().Format(time.RFC3339)}
	for _, e := range ents {
		if e.IsDir() || e.Name() == ManifestName || e.Name() == SignatureName {
			continue
		}
		sum, size, err := fileHash(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		m.Files = append(m.Files, File{Name: e.Name(), SHA256: sum, Size: size})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Name < m.Files[j].Name })
	return m, nil
}

// WriteManifest writes manifest.json.
func WriteManifest(dir string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestName), append(b, '\n'), 0o644)
}

// Sign signs manifest.json in the directory: the signature covers exactly its bytes.
func Sign(dir, keyPath string) error {
	key, err := readKey(keyPath)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return err
	}
	sig := ed25519.Sign(key, b)
	return os.WriteFile(filepath.Join(dir, SignatureName), []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644)
}

// Source is where a release comes from: a directory or an https URL.
type Source string

// ReadFile reads a release file.
func (s Source) ReadFile(name string) ([]byte, error) { return s.read(name) }

func (s Source) read(name string) ([]byte, error) {
	if strings.HasPrefix(string(s), "https://") {
		c := &http.Client{Timeout: 2 * time.Minute}
		resp, err := c.Get(strings.TrimRight(string(s), "/") + "/" + name)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s: HTTP %d", name, resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	}
	return os.ReadFile(filepath.Join(string(s), name))
}

// Verified is a verified manifest plus its source.
type Verified struct {
	Manifest *Manifest
	src      Source
}

// Verify reads the manifest and checks its signature with key pub.
func Verify(src Source, pub ed25519.PublicKey) (*Verified, error) {
	b, err := src.read(ManifestName)
	if err != nil {
		return nil, err
	}
	sigB64, err := src.read(SignatureName)
	if err != nil {
		return nil, fmt.Errorf("release signature missing: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil || !ed25519.Verify(pub, b, sig) {
		return nil, errors.New("release signature does not match the project key")
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &Verified{Manifest: &m, src: src}, nil
}

// Fetch retrieves a release file and checks its hash against the signed manifest.
func (v *Verified) Fetch(name, dst string) error {
	var want *File
	for i := range v.Manifest.Files {
		if v.Manifest.Files[i].Name == name {
			want = &v.Manifest.Files[i]
		}
	}
	if want == nil {
		return fmt.Errorf("release has no %s", name)
	}
	b, err := v.src.read(name)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != want.SHA256 {
		return fmt.Errorf("%s: hash does not match the manifest", name)
	}
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// PinnedKey is the key used for verification: the one pinned on the server, otherwise the embedded one.
func PinnedKey() (ed25519.PublicKey, error) {
	if b, err := os.ReadFile(PinnedKeyFile); err == nil {
		return ParsePublic(string(b))
	}
	if ProjectKey != "" {
		return ParsePublic(ProjectKey)
	}
	return nil, errors.New("project key not pinned: missing " + PinnedKeyFile)
}

// PinnedKeyString is the same, as a base64 string.
func PinnedKeyString() (string, error) {
	k, err := PinnedKey()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k), nil
}

// VerifyFile checks a local file against its manifest entry.
func (v *Verified) VerifyFile(name, path string) error {
	for _, f := range v.Manifest.Files {
		if f.Name != name {
			continue
		}
		sum, _, err := fileHash(path)
		if err != nil {
			return err
		}
		if sum != f.SHA256 {
			return fmt.Errorf("%s: hash does not match the manifest", name)
		}
		return nil
	}
	return fmt.Errorf("release has no %s", name)
}

// Newer compares versions like 1.2.3 (with an optional leading v).
func Newer(a, b string) bool {
	pa, pb := parts(a), parts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parts(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "-")
	for i, p := range strings.SplitN(v, ".", 3) {
		fmt.Sscanf(p, "%d", &out[i])
	}
	return out
}
