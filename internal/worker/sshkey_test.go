package worker

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestSSHKey(t *testing.T) {
	e := start(t)
	var k SSHKey
	if err := e.call(t, "sshkey", nil, &k); err != nil || k.Public != "" {
		t.Fatalf("no key yet: %+v %v", k, err)
	}
	if err := e.call(t, "sshkey-new", nil, &k); err != nil || !strings.HasPrefix(k.Public, "ssh-ed25519 ") || !strings.HasPrefix(k.Fingerprint, "SHA256:") {
		t.Fatalf("new key: %+v %v", k, err)
	}
	priv := filepath.Join(e.home, ".ssh", "id_ed25519")
	b, err := os.ReadFile(priv)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		t.Fatal(err)
	}
	if ssh.FingerprintSHA256(signer.PublicKey()) != k.Fingerprint {
		t.Fatal("public and private keys differ")
	}
	for path, mode := range map[string]os.FileMode{priv: 0o600, filepath.Dir(priv): 0o700} {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != mode {
			t.Errorf("%s: %v", path, fi.Mode().Perm())
		}
	}
	var again SSHKey
	if err := e.call(t, "sshkey-new", nil, &again); err == nil {
		t.Fatal("a second key overwrote the first")
	}
	if b2, _ := os.ReadFile(priv); string(b2) != string(b) {
		t.Fatal("the key changed")
	}
	if err := e.call(t, "sshkey", nil, &again); err != nil || again != k {
		t.Fatalf("reading the key back: %+v %v", again, err)
	}
}

func TestCloneURL(t *testing.T) {
	w := &Worker{}
	for u, ok := range map[string]bool{
		"https://github.com/ilguzin/laavu.git": true,
		"ssh://git@github.com/ilguzin/laavu":   true,
		"git@github.com:ilguzin/laavu.git":     true,
		"git@gitlab.example.org:/srv/repo.git": true,
		"ext::sh -c touch% /tmp/x":             false,
		"file:///etc":                          false,
		"/srv/repo":                            false,
		"-oProxyCommand=x@host:repo":           false,
		"git@-oProxyCommand=x:repo":            false,
		"https://":                             false,
		"git@github.com:a b":                   false,
		"http://example.org/repo.git":          false,
	} {
		if err := w.checkCloneURL(u); (err == nil) != ok {
			t.Errorf("%q: allowed should be %v", u, ok)
		}
	}
	w.LocalClone = true
	if w.checkCloneURL("/srv/repo") != nil || w.checkCloneURL("ext::sh") == nil {
		t.Fatal("dev mode: local paths only")
	}
}

func TestClone(t *testing.T) {
	e := start(t)
	e.w.LocalClone = true
	src := filepath.Join(t.TempDir(), "src")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", src},
		{"-C", src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "first"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
	}
	var wd Workdir
	if err := e.call(t, "clone", CloneArgs{URL: src, Folder: "app"}, &wd); err != nil || wd.Name != "app" || !wd.Git || wd.Branch != "main" {
		t.Fatalf("clone: %+v %v", wd, err)
	}
	// The empty commit comes with "files": [], not null: the client iterates over it.
	var ch json.RawMessage
	if err := e.call(t, "changes", map[string]any{"folder": "app", "since": 0}, &ch); err != nil || !strings.Contains(string(ch), `"files":[]`) {
		t.Fatalf("changes of an empty commit: %s %v", ch, err)
	}
	if err := e.call(t, "clone", CloneArgs{URL: src, Folder: "app"}, &wd); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("into an existing folder: %v", err)
	}
	if err := e.call(t, "clone", CloneArgs{URL: src, Folder: ".ssh"}, &wd); err == nil {
		t.Fatal("into a reserved folder")
	}
	err := e.call(t, "clone", CloneArgs{URL: filepath.Join(t.TempDir(), "missing"), Folder: "gone"}, &wd)
	if err == nil || strings.Contains(err.Error(), "Cloning into") {
		t.Fatalf("missing repository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.home, "gone")); err == nil {
		t.Fatal("a failed clone left its folder")
	}
}
