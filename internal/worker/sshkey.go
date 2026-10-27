package worker

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SSHKey is the agent's public ssh key; empty when there is none yet.
type SSHKey struct {
	Public      string `json:"public,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// keyPath is the key git and ssh pick up by default in the agent's sessions.
func (w *Worker) keyPath() string { return filepath.Join(w.Home, ".ssh", "id_ed25519") }

func (w *Worker) opSSHKey(_ *Conn, _ json.RawMessage) (any, error) {
	b, err := os.ReadFile(w.keyPath() + ".pub")
	if errors.Is(err, os.ErrNotExist) {
		return SSHKey{}, nil
	}
	if err != nil {
		return nil, err
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(b)
	if err != nil {
		return nil, errors.New("~/.ssh/id_ed25519.pub is not a public key")
	}
	return SSHKey{Public: strings.TrimSpace(string(b)), Fingerprint: ssh.FingerprintSHA256(pub)}, nil
}

// opSSHKeyNew creates an ed25519 key. An existing key is never overwritten:
// repositories may already trust it.
func (w *Worker) opSSHKeyNew(c *Conn, _ json.RawMessage) (any, error) {
	if _, err := os.Lstat(w.keyPath()); err == nil {
		return nil, errors.New("the agent already has a key")
	}
	if err := os.MkdirAll(filepath.Dir(w.keyPath()), 0o700); err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	comment := w.Agent.Name
	if u, err := user.Current(); err == nil {
		comment = u.Username
	}
	if h, err := os.Hostname(); err == nil {
		comment += "@" + h
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(w.keyPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(pem.EncodeToMemory(block)); err != nil {
		f.Close()
		os.Remove(w.keyPath())
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " " + comment + "\n"
	if err := os.WriteFile(w.keyPath()+".pub", []byte(line), 0o644); err != nil {
		return nil, err
	}
	w.log.Info("ssh key created", "fingerprint", ssh.FingerprintSHA256(sp))
	return w.opSSHKey(c, nil)
}
