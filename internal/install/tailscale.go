package install

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ilguzin/laavu/deploy"
	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/priv"
)

// Режимы Tailscale из server.md.
const (
	TSExtra = "extra" // «вдобавок»: из интернета и из tailnet, ssh — только из tailnet
	TSOnly  = "only"  // «только tailnet»: из интернета ничего
)

// tsState — что было до включения, чтобы disable вернул как было.
type tsState struct {
	Mode       string `json:"mode"`
	PublicURL  string `json:"public_url"`
	ViewURL    string `json:"view_url"`
	ChangedURL bool   `json:"changed_url"`
}

func tsStatePath() string { return filepath.Join(config.Dir, "tailscale.json") }

// FirewallRules — правила UFW для режима. Возвращает команды ufw по порядку.
// appPorts — порты приложения, просмотра и dev-серверов.
func FirewallRules(mode string, sshPort int, appPorts []string) [][]string {
	ssh := strconv.Itoa(sshPort) + "/tcp"
	var rules [][]string
	switch mode {
	case TSExtra:
		rules = append(rules, []string{"delete", "allow", ssh})
		rules = append(rules, []string{"allow", "in", "on", "tailscale0", "to", "any", "port", strconv.Itoa(sshPort), "proto", "tcp"})
	case TSOnly:
		rules = append(rules, []string{"delete", "allow", ssh})
		rules = append(rules, []string{"allow", "in", "on", "tailscale0", "to", "any", "port", strconv.Itoa(sshPort), "proto", "tcp"})
		for _, p := range appPorts {
			rules = append(rules, []string{"delete", "allow", p})
			port, proto, _ := strings.Cut(p, "/")
			rules = append(rules, []string{"allow", "in", "on", "tailscale0", "to", "any", "port", port, "proto", proto})
		}
	default: // выключение: всё как после install
		rules = append(rules, []string{"allow", ssh})
		rules = append(rules, []string{"delete", "allow", "in", "on", "tailscale0", "to", "any", "port", strconv.Itoa(sshPort), "proto", "tcp"})
		for _, p := range appPorts {
			rules = append(rules, []string{"allow", p})
			port, proto, _ := strings.Cut(p, "/")
			rules = append(rules, []string{"delete", "allow", "in", "on", "tailscale0", "to", "any", "port", port, "proto", proto})
		}
	}
	return rules
}

func appPorts(cfg config.Daemon) []string {
	if portsMode(cfg) {
		return []string{fmt.Sprintf("%d:%d/tcp", vmApp, vmView), fmt.Sprintf("%d:%d/tcp", vmDevFrom, vmDevTo)}
	}
	return []string{"80/tcp", "443/tcp", fmt.Sprintf("%d:%d/tcp", vmDevFrom, vmDevTo)}
}

// Tailscale — laavu tailscale enable|disable|status.
func Tailscale(args []string, w io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("нужен root")
	}
	if len(args) == 0 {
		return errors.New("использование: laavu tailscale enable [--mode extra|only] [--authkey КЛЮЧ] | disable | status")
	}
	switch args[0] {
	case "status":
		out, err := exec.Command("tailscale", "status").CombinedOutput()
		w.Write(out)
		return err
	case "enable":
		return tsEnable(args[1:], w)
	case "disable":
		return tsDisable(w)
	}
	return fmt.Errorf("неизвестная команда tailscale %s", args[0])
}

func tsEnable(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("tailscale enable", flag.ExitOnError)
	mode := fs.String("mode", TSExtra, "extra — вдобавок к интернету, only — только tailnet")
	authkey := fs.String("authkey", "", "ключ авторизации tailnet; без него — ссылка входа")
	sshPort := fs.Int("ssh-port", 22, "порт ssh")
	fs.Parse(args)
	if *mode != TSExtra && *mode != TSOnly {
		return errors.New("режим: extra или only")
	}
	o := &Options{Out: w}
	if _, err := exec.LookPath("tailscale"); err != nil {
		fmt.Fprintln(w, "Ставлю Tailscale…")
		if err := run(o, "sh", "-c", "curl -fsSL https://tailscale.com/install.sh | sh"); err != nil {
			return err
		}
	}
	up := []string{"up", "--ssh=false"}
	if *authkey != "" {
		up = append(up, "--authkey="+*authkey)
	}
	cmd := exec.Command("tailscale", up...)
	// Без ключа tailscale up печатает ссылку входа — показываем её владельцу.
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return err
	}
	cfgPath := filepath.Join(config.Dir, "laavud.toml")
	cfg, err := config.LoadDaemon(cfgPath)
	if err != nil {
		return err
	}
	st := tsState{Mode: *mode, PublicURL: cfg.PublicURL, ViewURL: cfg.ViewURL}
	// «Только tailnet» без своего домена: адрес <сервер>.<tailnet>.ts.net с
	// сертификатом Tailscale. Это другой origin, passkey придётся зарегистрировать заново.
	if *mode == TSOnly && portsMode(cfg) {
		name, err := tsDNSName()
		if err != nil {
			return err
		}
		cfg.PublicURL = "https://" + name
		cfg.ViewURL = fmt.Sprintf("https://%s:%d", name, vmView)
		st.ChangedURL = true
		if err := config.WriteDaemon(cfgPath, cfg); err != nil {
			return err
		}
		if err := writeTSCaddy(o, name); err != nil {
			return err
		}
	}
	for _, r := range FirewallRules(*mode, *sshPort, appPorts(cfg)) {
		run(o, "ufw", r...)
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(tsStatePath(), b, 0o644)
	priv.Log("tailscale enable mode=%s", *mode)
	if st.ChangedURL {
		run(o, "systemctl", "restart", "laavud.service")
		fmt.Fprintf(w, "Сервер теперь доступен только из tailnet по адресу %s.\nАдрес сменился — зарегистрируйте passkey заново: laavud reset-auth\n", cfg.PublicURL)
	} else {
		fmt.Fprintf(w, "Tailscale включён (%s). ssh теперь только из tailnet.\n", *mode)
	}
	return nil
}

func tsDNSName() (string, error) {
	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return "", err
	}
	var s struct {
		Self struct{ DNSName string }
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return "", err
	}
	name := strings.TrimSuffix(s.Self.DNSName, ".")
	if name == "" {
		return "", errors.New("Tailscale не сообщил имя сервера: включите MagicDNS и HTTPS в tailnet")
	}
	return name, nil
}

func writeTSCaddy(o *Options, name string) error {
	var aux []string
	aux = append(aux, fmt.Sprintf("https://%s:%d", name, vmView))
	for p := vmDevFrom; p <= vmDevTo; p++ {
		aux = append(aux, fmt.Sprintf("https://%s:%d", name, p))
	}
	b, err := deploy.Render("Caddyfile.tmpl", CaddyData{AppSite: "https://" + name, AuxSites: strings.Join(aux, ", ")})
	if err != nil {
		return err
	}
	// Caddy получает сертификаты ts.net у локального tailscaled.
	run(o, "tailscale", "set", "--operator=caddy")
	if _, err := writeIfChanged("/etc/caddy/Caddyfile", b, 0o644); err != nil {
		return err
	}
	return run(o, "systemctl", "restart", "caddy")
}

func tsDisable(w io.Writer) error {
	o := &Options{Out: w}
	var st tsState
	if b, err := os.ReadFile(tsStatePath()); err == nil {
		json.Unmarshal(b, &st)
	}
	cfgPath := filepath.Join(config.Dir, "laavud.toml")
	cfg, err := config.LoadDaemon(cfgPath)
	if err != nil {
		return err
	}
	if st.ChangedURL {
		cfg.PublicURL, cfg.ViewURL = st.PublicURL, st.ViewURL
		if err := config.WriteDaemon(cfgPath, cfg); err != nil {
			return err
		}
		b, err := deploy.Render("Caddyfile.tmpl", caddyData(&Options{}))
		if err != nil {
			return err
		}
		writeIfChanged("/etc/caddy/Caddyfile", b, 0o644)
		run(o, "systemctl", "restart", "caddy")
		run(o, "systemctl", "restart", "laavud.service")
	}
	for _, r := range FirewallRules("", 22, appPorts(cfg)) {
		run(o, "ufw", r...)
	}
	run(o, "tailscale", "down")
	os.Remove(tsStatePath())
	priv.Log("tailscale disable")
	fmt.Fprintln(w, "Tailscale выключен, доступ из интернета восстановлен.")
	if st.ChangedURL {
		fmt.Fprintln(w, "Адрес вернулся к прежнему — зарегистрируйте passkey заново: laavud reset-auth")
	}
	return nil
}
