package link

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Paths on the server. They are variables so tests can swap them.
// DefaultConsole is the project's console, used when laavu link gets no --console.
const DefaultConsole = "wss://link.getlaavu.com/v1"

var (
	ConfigFile   = "/etc/laavu/link.toml"                 // root:laavu-link 0640, written only by root
	KeyFile      = "/var/lib/laavu-link/link.key"         // laavu-link 0600
	SeenFile     = "/var/lib/laavu-helper/link-seen.json" // root 0600 in a root 0700 dir: job ids already run
	HelperSocket = "/run/laavu/link-helper.sock"          // root:laavu-link 0660
	StatusFile   = "/run/laavu-link/status.json"          // written by laavu link run
	User         = "laavu-link"
)

// Config is the binding, kept by root. The link process can read it but not change it,
// so a compromised link process cannot swap the console key or turn rights on.
type Config struct {
	Console    string          `toml:"console"`
	ConsoleKey string          `toml:"console_key"`
	ServerKey  string          `toml:"server_key"`
	Bound      int64           `toml:"bound"`
	Account    string          `toml:"account,omitempty"`   // the console account, as the console named it
	Unlinking  bool            `toml:"unlinking,omitempty"` // the owner ran laavu unlink: tell the console and stop
	Rights     map[string]bool `toml:"rights"`
}

// LoadConfig reads the binding. A missing file means the server is not linked.
func LoadConfig() (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(ConfigFile, &c); err != nil {
		return nil, err
	}
	if c.Rights == nil {
		c.Rights = map[string]bool{}
	}
	return &c, nil
}

// Allowed reports whether a right is on. Unknown rights are off.
func (c *Config) Allowed(right string) bool { return c.Rights[right] }

// Save writes the binding atomically.
func (c *Config) Save(gid int) error {
	var b bytes.Buffer
	b.WriteString("# Link to the Laavu console. Change rights with laavu link rights, remove with laavu unlink.\n")
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return err
	}
	tmp := ConfigFile + ".new"
	if err := os.WriteFile(tmp, b.Bytes(), 0o640); err != nil {
		return err
	}
	if gid >= 0 {
		if err := os.Chown(tmp, 0, gid); err != nil {
			return err
		}
	}
	return os.Rename(tmp, ConfigFile)
}

// GenerateKey writes a new server key and returns its public part.
func GenerateKey(path string) (ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return pub, nil
}

// ReadKey reads the server key.
func ReadKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New(path + " does not look like a link key")
	}
	return ed25519.PrivateKey(raw), nil
}

// CheckURL accepts wss:// anywhere and ws:// only on loopback (the test stub).
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		h := u.Hostname()
		if ip := net.ParseIP(h); h == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
		return fmt.Errorf("%s: ws:// is only allowed on loopback, use wss://", raw)
	}
	return fmt.Errorf("%s: console URL must start with wss://", raw)
}
