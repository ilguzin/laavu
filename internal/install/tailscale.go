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

// Tailscale modes: in addition to the internet, or tailnet only.
const (
	TSExtra = "extra" // "extra": from the internet and from the tailnet, ssh only from the tailnet
	TSOnly  = "only"  // "tailnet only": nothing from the internet
)

// tsState records what was there before enabling, so disable can restore it.
type tsState struct {
	Mode       string `json:"mode"`
	PublicURL  string `json:"public_url"`
	ViewURL    string `json:"view_url"`
	ChangedURL bool   `json:"changed_url"`
}

func tsStatePath() string { return filepath.Join(config.Dir, "tailscale.json") }

// FirewallRules returns the UFW rules for a mode, as ufw commands in order.
// appPorts are the app, view and dev server ports.
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
	default: // off: everything as after install
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

// Tailscale implements laavu tailscale enable|disable|status.
func Tailscale(args []string, w io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	if len(args) == 0 {
		return errors.New("usage: laavu tailscale enable [--mode extra|only] [--authkey KEY] [--hostname NAME] | disable | status")
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
	return fmt.Errorf("unknown tailscale command %s", args[0])
}

func tsEnable(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("tailscale enable", flag.ExitOnError)
	mode := fs.String("mode", TSExtra, "extra: in addition to the internet, only: tailnet only")
	authkey := fs.String("authkey", "", "tailnet auth key; without it, a login link is shown")
	hostname := fs.String("hostname", "", "the server's name in the tailnet (default: the machine name)")
	sshPort := fs.Int("ssh-port", 22, "ssh port")
	fs.Parse(args)
	url, err := tsSwitch(w, *mode, *authkey, *hostname, *sshPort)
	if err != nil {
		return err
	}
	if url == "" {
		fmt.Fprintf(w, "Tailscale enabled (%s). ssh is now tailnet only.\n", *mode)
		return nil
	}
	fmt.Fprintf(w, "The server is now reachable only from the tailnet at %s, ssh too.\n", url)
	out, err := exec.Command("runuser", "-u", "laavud", "--", "/usr/local/bin/laavud", "invite", "--admin", "-q", "--if-no-admin").Output()
	if link := strings.TrimSpace(string(out)); err == nil && link != "" {
		fmt.Fprintln(w, "Open the link and register the admin passkey (the link is single-use and valid for 24 hours):")
		fmt.Fprintln(w, "  "+link)
	} else {
		fmt.Fprintln(w, "The address changed, register the passkey again: laavud reset-auth")
	}
	return nil
}

// tsSwitch joins the tailnet and applies a mode. It returns the new app address when
// the server moved to its ts.net name (tailnet only, no own domain), otherwise "".
func tsSwitch(w io.Writer, mode, authkey, hostname string, sshPort int) (string, error) {
	if mode != TSExtra && mode != TSOnly {
		return "", errors.New("mode: extra or only")
	}
	o := &Options{Out: w}
	if _, err := exec.LookPath("tailscale"); err != nil {
		fmt.Fprintln(w, "Installing Tailscale...")
		if err := run(o, "sh", "-c", "curl -fsSL https://tailscale.com/install.sh | sh"); err != nil {
			return "", err
		}
	}
	up := []string{"up", "--ssh=false"}
	if authkey != "" {
		up = append(up, "--authkey="+authkey)
	}
	if hostname != "" {
		up = append(up, "--hostname="+hostname)
	}
	cmd := exec.Command("tailscale", up...)
	// Without a key, tailscale up prints a login link; we show it to the owner.
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return "", err
	}
	cfgPath := filepath.Join(config.Dir, "laavud.toml")
	cfg, err := config.LoadDaemon(cfgPath)
	if err != nil {
		return "", err
	}
	st := tsState{Mode: mode, PublicURL: cfg.PublicURL, ViewURL: cfg.ViewURL}
	// "Tailnet only" without an own domain: the address is <server>.<tailnet>.ts.net with
	// a Tailscale certificate. That is a different origin, so the passkey must be registered again.
	if mode == TSOnly && portsMode(cfg) {
		name, err := tsDNSName()
		if err != nil {
			return "", err
		}
		cfg.PublicURL = "https://" + name
		cfg.ViewURL = fmt.Sprintf("https://%s:%d", name, vmView)
		st.ChangedURL = true
		if err := config.WriteDaemon(cfgPath, cfg); err != nil {
			return "", err
		}
		if err := writeTSCaddy(o, name); err != nil {
			return "", err
		}
	}
	for _, r := range FirewallRules(mode, sshPort, appPorts(cfg)) {
		run(o, "ufw", r...)
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(tsStatePath(), b, 0o644)
	priv.Log("tailscale enable mode=%s", mode)
	if !st.ChangedURL {
		return "", nil
	}
	run(o, "systemctl", "restart", "laavud.service")
	return cfg.PublicURL, nil
}

func tsDNSName() (string, error) {
	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return "", err
	}
	return parseTSName(out)
}

// parseTSName takes the server's ts.net name from `tailscale status --json`.
func parseTSName(status []byte) (string, error) {
	var s struct {
		Self struct{ DNSName string }
	}
	if err := json.Unmarshal(status, &s); err != nil {
		return "", err
	}
	name := strings.TrimSuffix(s.Self.DNSName, ".")
	if name == "" {
		return "", errors.New("Tailscale did not report the server name: enable MagicDNS and HTTPS in the tailnet")
	}
	return name, nil
}

func tsCaddyData(name string) CaddyData {
	aux := []string{fmt.Sprintf("https://%s:%d", name, vmView)}
	for p := vmDevFrom; p <= vmDevTo; p++ {
		aux = append(aux, fmt.Sprintf("https://%s:%d", name, p))
	}
	return CaddyData{AppSite: "https://" + name, AuxSites: strings.Join(aux, ", ")}
}

func writeTSCaddy(o *Options, name string) error {
	b, err := deploy.Render("Caddyfile.tmpl", tsCaddyData(name))
	if err != nil {
		return err
	}
	// Caddy gets ts.net certificates from the local tailscaled.
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
	fmt.Fprintln(w, "Tailscale disabled, internet access restored.")
	if st.ChangedURL {
		fmt.Fprintln(w, "The address is back to the previous one, register the passkey again: laavud reset-auth")
	}
	return nil
}
