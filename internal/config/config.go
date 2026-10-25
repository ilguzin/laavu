// Package config читает /etc/laavu: настройки демона и список агентов.
package config

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ilguzin/laavu/internal/names"
)

// Dir — каталог конфигурации на сервере.
const Dir = "/etc/laavu"

// Daemon — /etc/laavu/laavud.toml.
type Daemon struct {
	// PublicURL — адрес приложения, по нему проверяется Origin и строится RP ID passkey.
	PublicURL string `toml:"public_url"`
	// ViewURL — origin просмотра HTML и ресурсов.
	ViewURL string `toml:"view_url"`
	// DevMode — как открываются dev-серверы агентов: "ports" (пул внешних портов)
	// или "subdomain" (p<порт>-<агент>.<DevDomain>).
	DevMode   string `toml:"dev_mode"`
	DevPorts  [2]int `toml:"dev_ports"`
	DevDomain string `toml:"dev_domain"`

	Listen    string `toml:"listen"`     // unix:/run/laavu/http/app.sock или 127.0.0.1:7700
	AuxListen string `toml:"aux_listen"` // origin просмотра и dev-серверов
	StateDir  string `toml:"state_dir"`
	RunDir    string `toml:"run_dir"`

	PushDelay   Duration `toml:"push_delay"`
	UploadLimit int64    `toml:"upload_limit_mb"`
	// NoAuth отключает вход. Только для режима разработки на localhost.
	NoAuth bool `toml:"no_auth"`
}

// Agent — /etc/laavu/agents/<имя>.toml.
type Agent struct {
	Name  string `toml:"name"`
	User  string `toml:"user"`
	Home  string `toml:"home"`
	Ports [2]int `toml:"ports"`
	// Claude — команда запуска агентской системы, по умолчанию "claude".
	Claude string `toml:"claude"`
	// TmuxSocket — имя сокета для tmux -L, по умолчанию "laavu".
	TmuxSocket string `toml:"tmux_socket"`
	// WorkerSocket — путь к сокету воркера; по умолчанию /run/laavu/workers/<user>.sock.
	WorkerSocket string `toml:"worker_socket"`

	UID int `toml:"-"`
}

// Duration — time.Duration в TOML строкой ("5s").
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Defaults — настройки по умолчанию для тестовой ВМ.
func Defaults() Daemon {
	return Daemon{
		PublicURL:   "https://localhost:8443",
		ViewURL:     "https://localhost:8444",
		DevMode:     "ports",
		DevPorts:    [2]int{8450, 8459},
		Listen:      "unix:/run/laavu/http/app.sock",
		AuxListen:   "unix:/run/laavu/http/aux.sock",
		StateDir:    "/var/lib/laavu",
		RunDir:      "/run/laavu",
		PushDelay:   Duration{5 * time.Second},
		UploadLimit: 200,
	}
}

// LoadDaemon читает настройки демона поверх значений по умолчанию.
func LoadDaemon(path string) (Daemon, error) {
	d := Defaults()
	if _, err := toml.DecodeFile(path, &d); err != nil {
		return d, fmt.Errorf("%s: %w", path, err)
	}
	return d, nil
}

// HookSocket — сокет, в который хуки шлют события.
func (d Daemon) HookSocket() string { return filepath.Join(d.RunDir, "hooks", "hook.sock") }

// LoadAgents читает все агенты из каталога и находит их uid.
func LoadAgents(dir string) ([]Agent, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Agent
	for _, f := range files {
		a, err := LoadAgent(f)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// LoadAgent читает одного агента.
func LoadAgent(path string) (Agent, error) {
	var a Agent
	if _, err := toml.DecodeFile(path, &a); err != nil {
		return a, fmt.Errorf("%s: %w", path, err)
	}
	if a.Name == "" {
		a.Name = strings.TrimSuffix(filepath.Base(path), ".toml")
	}
	if err := names.Agent(a.Name); err != nil {
		return a, fmt.Errorf("%s: %w", path, err)
	}
	a.fill()
	u, err := user.Lookup(a.User)
	if err != nil {
		return a, fmt.Errorf("агент %s: %w", a.Name, err)
	}
	a.UID, _ = strconv.Atoi(u.Uid)
	if a.Home == "" {
		a.Home = u.HomeDir
	}
	return a, nil
}

func (a *Agent) fill() {
	if a.User == "" {
		a.User = "lv-" + a.Name
	}
	if a.Claude == "" {
		a.Claude = "claude"
	}
	if a.TmuxSocket == "" {
		a.TmuxSocket = "laavu"
	}
	if a.WorkerSocket == "" {
		a.WorkerSocket = filepath.Join("/run/laavu/workers", a.User+".sock")
	}
}

// Fill дозаполняет значения по умолчанию (для агентов, собранных в коде).
func (a *Agent) Fill() { a.fill() }

// PortsEnv — диапазон портов агента в виде "20000-20999".
func (a Agent) PortsEnv() string { return fmt.Sprintf("%d-%d", a.Ports[0], a.Ports[1]) }

// WriteAgent сохраняет агента в каталог.
func WriteAgent(dir string, a Agent) error {
	f, err := os.CreateTemp(dir, ".agent-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := toml.NewEncoder(f).Encode(a); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, a.Name+".toml"))
}

// WriteDaemon сохраняет настройки демона.
func WriteDaemon(path string, d Daemon) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(d); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
