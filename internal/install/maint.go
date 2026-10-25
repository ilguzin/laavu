package install

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/priv"
)

// Report — отчёт laavu check. Схема пойдёт в контракт с консолью (К1).
type Report struct {
	Time      int64             `json:"time"`
	Versions  map[string]string `json:"versions"`
	Services  map[string]string `json:"services"`
	Disk      DiskReport        `json:"disk"`
	MemoryMB  MemReport         `json:"memory_mb"`
	Certs     []CertReport      `json:"certs,omitempty"`
	Agents    []AgentReport     `json:"agents"`
	Isolation *priv.LastCheck   `json:"isolation,omitempty"`
	Updates   int               `json:"os_updates"`
	Reboot    bool              `json:"reboot_required"`
	Problems  []string          `json:"problems"`
}

type DiskReport struct {
	TotalGB float64 `json:"total_gb"`
	FreeGB  float64 `json:"free_gb"`
	UsedPct int     `json:"used_pct"`
}

type MemReport struct {
	Total     int `json:"total"`
	Available int `json:"available"`
}

type CertReport struct {
	Name     string `json:"name"`
	NotAfter int64  `json:"not_after"`
	DaysLeft int    `json:"days_left"`
}

type AgentReport struct {
	Name   string `json:"name"`
	Worker string `json:"worker"`
	Ports  string `json:"ports"`
	Claude string `json:"claude,omitempty"`
}

func output(name string, args ...string) string {
	out, _ := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out))
}

func serviceState(unit string) string {
	s := output("systemctl", "is-active", unit)
	if s == "" {
		return "unknown"
	}
	return s
}

// BuildReport собирает отчёт о состоянии сервера.
func BuildReport() *Report {
	r := &Report{Time: time.Now().Unix(), Versions: map[string]string{}, Services: map[string]string{}, Problems: []string{}}
	r.Versions["laavud"] = output("/usr/local/bin/laavud", "version")
	r.Versions["laavu"] = output("/usr/local/bin/laavu", "version")
	r.Versions["tmux"] = output("tmux", "-V")
	r.Versions["caddy"] = strings.Fields(output("caddy", "version") + " ")[0]
	r.Versions["kernel"] = output("uname", "-r")
	for _, u := range []string{"laavud", "caddy", "laavu-nftables", "ufw", "ssh"} {
		r.Services[u] = serviceState(u + ".service")
		if r.Services[u] != "active" {
			r.Problems = append(r.Problems, fmt.Sprintf("служба %s: %s", u, r.Services[u]))
		}
	}
	var st syscall.Statfs_t
	if syscall.Statfs("/", &st) == nil {
		total := float64(st.Blocks) * float64(st.Bsize) / (1 << 30)
		free := float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
		r.Disk = DiskReport{TotalGB: round1(total), FreeGB: round1(free)}
		if total > 0 {
			r.Disk.UsedPct = int((total - free) * 100 / total)
		}
		if r.Disk.UsedPct > 90 {
			r.Problems = append(r.Problems, fmt.Sprintf("диск заполнен на %d%%", r.Disk.UsedPct))
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			kb, _ := strconv.Atoi(f[1])
			switch f[0] {
			case "MemTotal:":
				r.MemoryMB.Total = kb / 1024
			case "MemAvailable:":
				r.MemoryMB.Available = kb / 1024
			}
		}
	}
	r.Certs = caddyCerts()
	for _, c := range r.Certs {
		if c.DaysLeft < 14 {
			r.Problems = append(r.Problems, fmt.Sprintf("сертификат %s истекает через %d дн.", c.Name, c.DaysLeft))
		}
	}
	agents, _ := priv.Agents()
	for _, a := range agents {
		ar := AgentReport{Name: a.Name, Ports: a.PortsEnv(), Worker: serviceState("laavud-worker@" + a.User + ".service")}
		if v, err := AsAgent(a.User, a.UID, filepath.Join(a.Home, ".local/bin/claude"), "--version"); err == nil {
			ar.Claude = strings.TrimSpace(v)
		}
		if ar.Worker != "active" {
			r.Problems = append(r.Problems, fmt.Sprintf("воркер агента %s: %s", a.Name, ar.Worker))
		}
		r.Agents = append(r.Agents, ar)
	}
	if b, err := os.ReadFile(priv.LastCheckFile()); err == nil {
		var lc priv.LastCheck
		if json.Unmarshal(b, &lc) == nil {
			r.Isolation = &lc
			if lc.Failed > 0 {
				r.Problems = append(r.Problems, "последняя проверка изоляции нашла открытые пробы")
			}
		}
	}
	if len(agents) > 1 && r.Isolation == nil {
		r.Problems = append(r.Problems, "изоляция агентов ещё не проверялась: laavu check-isolation")
	}
	if out := output("apt-get", "-s", "-q", "upgrade"); out != "" {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "Inst ") {
				r.Updates++
			}
		}
	}
	_, err := os.Stat("/var/run/reboot-required")
	r.Reboot = err == nil
	if r.Reboot {
		r.Problems = append(r.Problems, "нужна перезагрузка после обновлений")
	}
	if !appResponds() {
		r.Problems = append(r.Problems, "laavud не отвечает на сокете приложения")
	}
	return r
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

func caddyCerts() []CertReport {
	var out []CertReport
	root := "/var/lib/caddy/.local/share/caddy/certificates"
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		// Сертификаты внутреннего CA Caddy живут сутки и продлеваются сами.
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".crt") || strings.Contains(p, "/local/") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil
		}
		out = append(out, CertReport{Name: strings.TrimSuffix(filepath.Base(p), ".crt"), NotAfter: c.NotAfter.Unix(),
			DaysLeft: int(time.Until(c.NotAfter).Hours() / 24)})
		return nil
	})
	return out
}

// appResponds — отвечает ли laavud на своём сокете.
func appResponds() bool {
	cfg, err := config.LoadDaemon(filepath.Join(config.Dir, "laavud.toml"))
	if err != nil {
		return false
	}
	path, ok := strings.CutPrefix(cfg.Listen, "unix:")
	if !ok {
		return false
	}
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) { return net.Dial("unix", path) },
	}}
	resp, err := c.Get("http://laavud/manifest.webmanifest")
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode < 500
}

// Check — laavu check.
func Check(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "отчёт в JSON")
	fs.Parse(args)
	if os.Geteuid() != 0 {
		return errors.New("нужен root")
	}
	r := BuildReport()
	if *asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	fmt.Fprintf(w, "laavud %s, laavu %s, %s, caddy %s, ядро %s\n", r.Versions["laavud"], r.Versions["laavu"], r.Versions["tmux"], r.Versions["caddy"], r.Versions["kernel"])
	fmt.Fprintf(w, "Диск: свободно %.1f из %.1f ГБ (%d%% занято). Память: доступно %d из %d МБ\n", r.Disk.FreeGB, r.Disk.TotalGB, r.Disk.UsedPct, r.MemoryMB.Available, r.MemoryMB.Total)
	for _, c := range r.Certs {
		fmt.Fprintf(w, "Сертификат %s: ещё %d дн.\n", c.Name, c.DaysLeft)
	}
	for _, a := range r.Agents {
		fmt.Fprintf(w, "Агент %s: воркер %s, порты %s, claude %s\n", a.Name, a.Worker, a.Ports, orDash(a.Claude))
	}
	if r.Isolation != nil {
		fmt.Fprintf(w, "Изоляция проверялась %s: пар %d, открыто %d\n", time.Unix(r.Isolation.Time, 0).Format("2006-01-02 15:04"), r.Isolation.Pairs, r.Isolation.Failed)
	}
	fmt.Fprintf(w, "Обновлений ОС: %d\n", r.Updates)
	if len(r.Problems) == 0 {
		fmt.Fprintln(w, "Проблем нет.")
		return nil
	}
	fmt.Fprintln(w, "Проблемы:")
	for _, p := range r.Problems {
		fmt.Fprintln(w, "  -", p)
	}
	return fmt.Errorf("проблем: %d", len(r.Problems))
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// Firewall — laavu firewall status|allow|deny.
func Firewall(args []string, w io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("нужен root")
	}
	if len(args) == 0 || args[0] == "status" {
		out, err := exec.Command("ufw", "status", "verbose").CombinedOutput()
		w.Write(out)
		return err
	}
	if len(args) != 2 || (args[0] != "allow" && args[0] != "deny") {
		return errors.New("использование: laavu firewall status|allow ПОРТ[/tcp]|deny ПОРТ[/tcp]")
	}
	port := args[1]
	if _, err := strconv.Atoi(strings.SplitN(port, "/", 2)[0]); err != nil {
		return errors.New("порт — число, например 8080 или 8080/tcp")
	}
	priv.Log("firewall %s %s", args[0], port)
	out, err := exec.Command("ufw", args[0], port).CombinedOutput()
	w.Write(out)
	return err
}

// Cleanup — старые журналы, apt-кеш, временные файлы агентов старше недели.
func Cleanup(w io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("нужен root")
	}
	priv.Log("cleanup")
	steps := [][]string{
		{"journalctl", "--vacuum-time=30d"},
		{"apt-get", "clean"},
		{"apt-get", "autoremove", "-y", "-q"},
	}
	for _, s := range steps {
		out, err := exec.Command(s[0], s[1:]...).CombinedOutput()
		fmt.Fprintf(w, "%s: %s\n", strings.Join(s, " "), lastLines(string(out), 1))
		if err != nil {
			fmt.Fprintln(w, "  ошибка:", err)
		}
	}
	// Дом агента принадлежит агенту: root по нему не ходит (симлинк ~/tmp → /
	// превратил бы чистку в удаление системы). Чистит сам агент, в своём слайсе.
	agents, _ := priv.Agents()
	for _, a := range agents {
		out, err := AsAgent(a.User, a.UID, "find", "-P", filepath.Join(a.Home, "tmp"), "-xdev", "-mindepth", "1", "-maxdepth", "1",
			"-mtime", "+7", "-print", "-exec", "rm", "-rf", "--", "{}", "+")
		n := 0
		if s := strings.TrimSpace(out); s != "" {
			n = len(strings.Split(s, "\n"))
		}
		fmt.Fprintf(w, "агент %s: удалено из ~/tmp: %d\n", a.Name, n)
		if err != nil {
			fmt.Fprintln(w, "  ошибка:", err)
		}
	}
	return nil
}

// AsAgent выполняет команду от агента в его слайсе, без терминала root.
func AsAgent(user string, uid int, name string, args ...string) (string, error) {
	full := append([]string{"--quiet", "--pipe", "--wait", "--collect", "--uid=" + user,
		fmt.Sprintf("--slice=user-%d.slice", uid), "-p", "UMask=0077", "--", name}, args...)
	cmd := exec.Command("systemd-run", full...)
	cmd.Stdin = nil
	out, err := cmd.Output()
	return string(out), err
}

// Diag — диагностика для разбора проблем: без содержимого рабочих папок.
func Diag(w io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("нужен root")
	}
	sections := [][]string{
		{"systemctl", "--no-pager", "status", "laavud", "caddy", "laavu-nftables"},
		{"journalctl", "--no-pager", "-u", "laavud", "-n", "100"},
		{"journalctl", "--no-pager", "-u", "laavud-worker@*", "-n", "100"},
		{"journalctl", "--no-pager", "-u", "caddy", "-n", "50"},
		{"nft", "list", "table", "inet", "laavu"},
		{"ufw", "status", "verbose"},
		{"ls", "-la", "/run/laavu", "/run/laavu/http", "/run/laavu/hooks", "/run/laavu/workers"},
		{"findmnt", "/proc"},
		{"cat", "/etc/laavu/laavud.toml"},
		{"tail", "-n", "50", priv.LogFile},
	}
	for _, s := range sections {
		fmt.Fprintf(w, "\n=== %s\n", strings.Join(s, " "))
		out, _ := exec.Command(s[0], s[1:]...).CombinedOutput()
		w.Write(out)
	}
	return nil
}
