// Package release — манифест релиза и его подпись ключом проекта.
//
// Ключ подписи хранится не в CI, а у владельца проекта. CI собирает бинарники и
// неподписанный манифест; подпись ставится руками (laavu release sign) и
// проверяется открытым ключом, закреплённым на сервере при установке.
package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

// ProjectKey — открытый ключ проекта (base64), вшивается при сборке релиза:
// -ldflags "-X github.com/ilguzin/laavu/internal/release.ProjectKey=…".
var ProjectKey = ""

// PinnedKeyFile — ключ, закреплённый на сервере при установке.
const PinnedKeyFile = "/etc/laavu/release.pub"

// Manifest — состав релиза.
type Manifest struct {
	Version string `json:"version"`
	Created string `json:"created"`
	Files   []File `json:"files"`
}

// File — один артефакт.
type File struct {
	Name   string `json:"name"` // laavud-linux-amd64
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

const (
	ManifestName  = "manifest.json"
	SignatureName = "manifest.json.sig"
)

// GenerateKey создаёт пару ключей; закрытый — в файл 0600, открытый — строкой.
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

func readKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("файл не похож на закрытый ключ релизов")
	}
	return ed25519.PrivateKey(raw), nil
}

// ParsePublic разбирает открытый ключ в base64.
func ParsePublic(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("неверный открытый ключ релизов")
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

// Build собирает манифест по файлам каталога (кроме самого манифеста и подписи).
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

// WriteManifest пишет manifest.json.
func WriteManifest(dir string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestName), append(b, '\n'), 0o644)
}

// Sign подписывает manifest.json в каталоге: подпись — ровно его байты.
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

// Source — откуда берётся релиз: каталог или https-адрес.
type Source string

// ReadFile читает файл релиза.
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

// Verified — проверенный манифест и источник.
type Verified struct {
	Manifest *Manifest
	src      Source
}

// Verify читает манифест и проверяет подпись ключом pub.
func Verify(src Source, pub ed25519.PublicKey) (*Verified, error) {
	b, err := src.read(ManifestName)
	if err != nil {
		return nil, err
	}
	sigB64, err := src.read(SignatureName)
	if err != nil {
		return nil, fmt.Errorf("нет подписи релиза: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil || !ed25519.Verify(pub, b, sig) {
		return nil, errors.New("подпись релиза не сходится с ключом проекта")
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &Verified{Manifest: &m, src: src}, nil
}

// Fetch достаёт файл релиза и сверяет его хеш с подписанным манифестом.
func (v *Verified) Fetch(name, dst string) error {
	var want *File
	for i := range v.Manifest.Files {
		if v.Manifest.Files[i].Name == name {
			want = &v.Manifest.Files[i]
		}
	}
	if want == nil {
		return fmt.Errorf("в релизе нет %s", name)
	}
	b, err := v.src.read(name)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != want.SHA256 {
		return fmt.Errorf("%s: хеш не сходится с манифестом", name)
	}
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// PinnedKey — ключ для проверки: закреплённый на сервере, иначе вшитый.
func PinnedKey() (ed25519.PublicKey, error) {
	if b, err := os.ReadFile(PinnedKeyFile); err == nil {
		return ParsePublic(string(b))
	}
	if ProjectKey != "" {
		return ParsePublic(ProjectKey)
	}
	return nil, errors.New("ключ проекта не закреплён: нет " + PinnedKeyFile)
}

// PinnedKeyString — то же, строкой base64.
func PinnedKeyString() (string, error) {
	k, err := PinnedKey()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k), nil
}

// VerifyFile сверяет локальный файл с записью манифеста.
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
			return fmt.Errorf("%s: хеш не сходится с манифестом", name)
		}
		return nil
	}
	return fmt.Errorf("в релизе нет %s", name)
}

// Newer сравнивает версии вида 1.2.3 (с необязательной v впереди).
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
