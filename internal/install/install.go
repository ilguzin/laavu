// Package install implements laavu install on the server: hardening, users, Caddy,
// laavud, isolation measures. Running it again breaks nothing.
package install

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ilguzin/laavu/deploy"
	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/priv"
	"github.com/ilguzin/laavu/internal/release"
)

// Options are the installation parameters.
type Options struct {
	// Domain is the server domain. Empty means test VM mode: ports 8443/8444/8450-8459
	// and a certificate from Caddy's internal CA.
	Domain string
	// Host is the server name in test VM mode (default localhost), e.g.
	// the VirtualBox host name in the tailnet. Passkeys are bound to the name: changing the name
	// means registering the passkey again.
	Host  string
	Email string
	// Sslip names the server A-B-C-D.sslip.io after its public IPv4 (PublicIP, or the
	// route source address) and then installs as with a domain.
	Sslip    bool
	PublicIP string
	// Tailscale makes the server reachable only from the tailnet, at
	// https://TailscaleHostname.<tailnet>.ts.net, after the rest is installed.
	Tailscale         bool
	TailscaleAuthKey  string
	TailscaleHostname string
	// NoLink skips the registration link: laavu install --ssh prints it after the tailnet switch.
	NoLink bool
	// Agent is the name of the first agent; empty means no agent.
	Agent         string
	InstallClaude bool
	// OwnerKey is the owner's ssh public key; installed before password login is disabled.
	OwnerKey string
	// OwnerUser is the owner's user on the server (for the key and the procview group).
	OwnerUser string
	// HardenSSH disables password and root login (only if the owner key is in place).
	HardenSSH bool
	SSHPort   int
	// SkipApt skips package installation (tests, offline).
	SkipApt bool
	BinDir  string
	// ReleaseKey is the project public key for verifying updates, pinned in /etc/laavu/release.pub.
	ReleaseKey string
	Out        io.Writer

	explicit bool     // the address mode was given by flags, not taken from laavud.toml
	ts       *tsState // the server is already in the tailnet (repeated install)
	tsName   string   // its ts.net name
}

// VM mode: ports are the same inside the VM and on the host.
const (
	vmApp     = 8443
	vmView    = 8444
	vmDevFrom = 8450
	vmDevTo   = 8459
)

type step struct {
	name string
	fn   func(*Options) error
}

// Run performs the installation.
func Run(o Options) error {
	if os.Geteuid() != 0 {
		return errors.New("laavu install must run as root (or via sudo)")
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.BinDir == "" {
		o.BinDir = "/usr/local/bin"
	}
	if o.SSHPort == 0 {
		o.SSHPort = 22
	}
	if o.OwnerUser == "" {
		o.OwnerUser = os.Getenv("SUDO_USER")
	}
	if err := checkOS(); err != nil {
		return err
	}
	if err := resolveAddress(&o); err != nil {
		return err
	}
	// The tailnet switch closes public ssh; passwords have to be off before that,
	// either already or by --harden-ssh with the owner's key in this run.
	if o.Tailscale && o.ts == nil && !(o.HardenSSH && o.OwnerKey != "") {
		if on, err := passwordLogin(); err != nil || on {
			return errTailnetPasswords
		}
	}
	steps := []step{
		{"packages", stepPackages},
		{"binaries", stepBinaries},
		{"users and groups", stepUsers},
		{"hide other users' processes (hidepid)", stepHidepid},
		{"ssh", stepSSH},
		{"configuration /etc/laavu", stepConfig},
		{"nftables rules between agents", stepNftables},
		{"firewall", stepFirewall},
		{"Caddy", stepCaddy},
		{"laavud", stepDaemon},
	}
	for i, s := range steps {
		fmt.Fprintf(o.Out, "[%d/%d] %s\n", i+1, len(steps)+1, s.name)
		if err := s.fn(&o); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	fmt.Fprintf(o.Out, "[%d/%d] first agent\n", len(steps)+1, len(steps)+1)
	if o.Agent != "" {
		if _, err := user.Lookup("lv-" + o.Agent); err == nil {
			fmt.Fprintf(o.Out, "  agent %s already exists\n", o.Agent)
		} else {
			if _, err := priv.CreateAgent(priv.CreateOptions{Name: o.Agent, InstallClaude: o.InstallClaude, Laavud: filepath.Join(o.BinDir, "laavud"), Out: o.Out}); err != nil {
				return fmt.Errorf("agent: %w", err)
			}
		}
	}
	if o.Tailscale && o.ts == nil {
		if on, err := passwordLogin(); err != nil || on {
			return fmt.Errorf("ssh still accepts passwords, so the server was not moved into the tailnet. Turn them off (sudo laavu harden-ssh --owner-key \"ssh-ed25519 AAAA...\"), then: sudo laavu tailscale enable --mode only --authkey KEY --hostname NAME")
		}
		fmt.Fprintln(o.Out, "Moving the server into the tailnet")
		if _, err := tsSwitch(o.Out, TSOnly, o.TailscaleAuthKey, o.TailscaleHostname, o.SSHPort); err != nil {
			return fmt.Errorf("tailnet: %w (Laavu is installed; retry with: sudo laavu tailscale enable --mode only)", err)
		}
	}
	priv.Log("install domain=%q agent=%q tailscale=%v", o.Domain, o.Agent, o.Tailscale)
	if o.NoLink {
		return nil
	}
	return printLink(&o)
}

func checkOS() error {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return err
	}
	s := string(b)
	if !strings.Contains(s, "ID=ubuntu") {
		return errors.New("only Ubuntu 24.04 and newer is supported")
	}
	for _, line := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(line, "VERSION_ID="); ok {
			v = strings.Trim(v, `"`)
			if major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0]); major < 24 {
				return fmt.Errorf("Ubuntu version %s is too old, 24.04 or newer is required", v)
			}
		}
	}
	return nil
}

func run(o *Options, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), lastLines(buf.String(), 8))
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// writeIfChanged writes the file if its content differs; reports whether it changed.
func writeIfChanged(path string, data []byte, mode os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".laavu-new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

func stepPackages(o *Options) error {
	if o.SkipApt {
		return nil
	}
	pkgs := []string{"tmux", "git", "caddy", "nftables", "ufw", "curl", "ca-certificates"}
	missing := []string{}
	for _, p := range pkgs {
		if exec.Command("dpkg-query", "-W", "-f=${Status}", p).Run() != nil {
			missing = append(missing, p)
			continue
		}
		out, _ := exec.Command("dpkg-query", "-W", "-f=${Status}", p).Output()
		if !strings.Contains(string(out), "install ok installed") {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	fmt.Fprintf(o.Out, "  installing: %s\n", strings.Join(missing, " "))
	if err := run(o, "apt-get", "update", "-q"); err != nil {
		return err
	}
	return run(o, "apt-get", append([]string{"install", "-y", "-q", "--no-install-recommends"}, missing...)...)
}

// stepBinaries puts laavu and laavud into /usr/local/bin. The source is the directory
// of the running laavu.
func stepBinaries(o *Options) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src := filepath.Dir(self)
	for _, name := range []string{"laavu", "laavud"} {
		from := filepath.Join(src, name)
		to := filepath.Join(o.BinDir, name)
		if from == to {
			continue
		}
		b, err := os.ReadFile(from)
		if err != nil {
			return fmt.Errorf("no %s next to laavu: %w", name, err)
		}
		if _, err := writeIfChanged(to, b, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func ensureGroup(o *Options, name string) error {
	if _, err := user.LookupGroup(name); err == nil {
		return nil
	}
	return run(o, "groupadd", "--system", name)
}

func stepUsers(o *Options) error {
	for _, g := range []string{"laavu-agents", "procview"} {
		if err := ensureGroup(o, g); err != nil {
			return err
		}
	}
	if _, err := user.Lookup("laavud"); err != nil {
		if err := run(o, "useradd", "--system", "--home-dir", "/var/lib/laavu", "--no-create-home",
			"--shell", "/usr/sbin/nologin", "--user-group", "laavud"); err != nil {
			return err
		}
	}
	// The server owner sees all processes.
	if o.OwnerUser != "" && o.OwnerUser != "root" {
		run(o, "usermod", "-aG", "procview", o.OwnerUser)
	}
	os.MkdirAll("/srv/laavu/agents", 0o755)
	os.MkdirAll("/var/log/laavu", 0o750)
	return nil
}

func stepHidepid(o *Options) error {
	g, err := user.LookupGroup("procview")
	if err != nil {
		return err
	}
	b, err := os.ReadFile("/etc/fstab")
	if err != nil {
		return err
	}
	if !strings.Contains(string(b), "hidepid=") {
		line := "proc /proc proc defaults,hidepid=invisible,gid=" + g.Gid + " 0 0"
		s := strings.TrimRight(string(b), "\n") + "\n# Laavu: users see only their own processes\n" + line + "\n"
		if _, err := writeIfChanged("/etc/fstab", []byte(s), 0o644); err != nil {
			return err
		}
	}
	return run(o, "mount", "-o", "remount,hidepid=invisible,gid="+g.Gid, "/proc")
}

// HardenSSH runs only the ssh step: owner key and password ban (laavu harden-ssh).
func HardenSSH(o Options) error {
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	o.HardenSSH = true
	priv.Log("harden-ssh")
	return stepSSH(&o)
}

func stepSSH(o *Options) error {
	if o.OwnerKey != "" {
		if err := addOwnerKey(o); err != nil {
			return err
		}
	}
	conf := "# Laavu: agents do not log in over ssh.\nDenyGroups laavu-agents\n"
	if o.HardenSSH {
		if !ownerHasKey(o) {
			return errors.New("disabling passwords without the owner key would lock you out of the server; pass --owner-key")
		}
		conf += "PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin prohibit-password\n"
	}
	// In sshd_config.d the first value wins, and files are read alphabetically:
	// our file must come before 50-cloud-init.conf, where hosters enable passwords.
	const path = "/etc/ssh/sshd_config.d/01-laavu.conf"
	os.Remove("/etc/ssh/sshd_config.d/50-laavu.conf")
	changed, err := writeIfChanged(path, []byte(conf), 0o644)
	if err != nil {
		return err
	}
	if changed {
		if err := run(o, "sshd", "-t"); err != nil {
			os.Remove(path)
			return err
		}
		run(o, "systemctl", "reload", "ssh")
	}
	if o.HardenSSH {
		out, _ := exec.Command("sshd", "-T").Output()
		eff := string(out)
		if !strings.Contains(eff, "\npasswordauthentication no\n") || !strings.Contains(eff, "\npermitrootlogin without-password\n") && !strings.Contains(eff, "\npermitrootlogin prohibit-password\n") {
			return errors.New("sshd still allows password login: check /etc/ssh/sshd_config and sshd_config.d")
		}
	}
	if !strings.Contains(outputOf("sshd", "-T"), "\ndenygroups laavu-agents\n") {
		return errors.New("DenyGroups laavu-agents was not applied: check sshd_config.d")
	}
	return nil
}

func outputOf(name string, args ...string) string {
	out, _ := exec.Command(name, args...).Output()
	return string(out)
}

func ownerHome(o *Options) (string, int, int, error) {
	name := o.OwnerUser
	if name == "" {
		name = "root"
	}
	u, err := user.Lookup(name)
	if err != nil {
		return "", 0, 0, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return u.HomeDir, uid, gid, nil
}

func addOwnerKey(o *Options) error {
	home, uid, gid, err := ownerHome(o)
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".ssh")
	os.MkdirAll(dir, 0o700)
	os.Chown(dir, uid, gid)
	ak := filepath.Join(dir, "authorized_keys")
	cur, _ := os.ReadFile(ak)
	key := strings.TrimSpace(o.OwnerKey)
	if strings.Contains(string(cur), key) {
		return nil
	}
	s := strings.TrimRight(string(cur), "\n")
	if s != "" {
		s += "\n"
	}
	s += key + "\n"
	if err := os.WriteFile(ak, []byte(s), 0o600); err != nil {
		return err
	}
	return os.Chown(ak, uid, gid)
}

func ownerHasKey(o *Options) bool {
	home, _, _, err := ownerHome(o)
	if err != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	return err == nil && strings.Contains(string(b), "ssh-")
}

func daemonConfig(o *Options) config.Daemon {
	d := config.Defaults()
	if o.Domain != "" {
		d.PublicURL = "https://" + o.Domain
		d.ViewURL = "https://view." + o.Domain
		// Without a wildcard certificate, dev servers are opened on the domain's ports.
		d.DevMode = "ports"
		d.DevPorts = [2]int{vmDevFrom, vmDevTo}
	} else {
		h := vmHost(o)
		d.PublicURL = fmt.Sprintf("https://%s:%d", h, vmApp)
		d.ViewURL = fmt.Sprintf("https://%s:%d", h, vmView)
	}
	return d
}

func vmHost(o *Options) string {
	if o.Host != "" {
		return o.Host
	}
	return "localhost"
}

// portsMode reports whether the server is in test VM mode: the app on port 8443.
func portsMode(cfg config.Daemon) bool {
	return strings.HasSuffix(hostOfURL(cfg.PublicURL), fmt.Sprintf(":%d", vmApp))
}

func hostOfURL(raw string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	h, _, _ := strings.Cut(s, "/")
	return h
}

func stepConfig(o *Options) error {
	os.MkdirAll(filepath.Join(config.Dir, "agents"), 0o755)
	os.MkdirAll(filepath.Join(config.Dir, "agent-template"), 0o755)
	if o.ReleaseKey != "" {
		if _, err := release.ParsePublic(o.ReleaseKey); err != nil {
			return err
		}
		if _, err := os.Stat(release.PinnedKeyFile); err != nil {
			os.WriteFile(release.PinnedKeyFile, []byte(o.ReleaseKey+"\n"), 0o644)
		}
	} else if release.ProjectKey != "" {
		if _, err := os.Stat(release.PinnedKeyFile); err != nil {
			os.WriteFile(release.PinnedKeyFile, []byte(release.ProjectKey+"\n"), 0o644)
		}
	}
	path := filepath.Join(config.Dir, "laavud.toml")
	if cur, err := config.LoadDaemon(path); err == nil && o.explicit && o.ts == nil {
		// A new address (another --host, a domain instead of sslip.io): the addresses are rewritten, the rest stays.
		want := daemonConfig(o)
		if cur.PublicURL != want.PublicURL {
			cur.PublicURL, cur.ViewURL = want.PublicURL, want.ViewURL
			cur.DevMode, cur.DevPorts = want.DevMode, want.DevPorts
			if err := config.WriteDaemon(path, cur); err != nil {
				return err
			}
			fmt.Fprintf(o.Out, "  server address is now %s: the passkey must be registered again (laavud reset-auth)\n", cur.PublicURL)
		}
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := config.WriteDaemon(path, daemonConfig(o)); err != nil {
			return err
		}
	}
	laavud := filepath.Join(o.BinDir, "laavud")
	files := map[string]string{
		"reserved":      "reserved",
		"tmux.conf":     "tmux.conf.tmpl",
		"tmpfiles.conf": "tmpfiles.conf",
	}
	for dst, src := range files {
		b, err := deploy.Render(src, map[string]string{"Laavud": laavud})
		if err != nil {
			return err
		}
		target := filepath.Join(config.Dir, dst)
		if dst == "tmpfiles.conf" {
			target = "/etc/tmpfiles.d/laavu.conf"
		}
		if dst == "reserved" {
			if _, err := os.Stat(target); err == nil {
				continue // the owner extends the list
			}
		}
		if _, err := writeIfChanged(target, b, 0o644); err != nil {
			return err
		}
	}
	// The agent rules template is for the owner to edit; priv uses it instead of the embedded one.
	for _, f := range []string{"CLAUDE.md.tmpl", "settings.json.tmpl"} {
		dst := filepath.Join(config.Dir, "agent-template", f)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		b, _ := deploy.Files.ReadFile("agent/" + f)
		os.WriteFile(dst, b, 0o644)
	}
	return run(o, "systemd-tmpfiles", "--create", "/etc/tmpfiles.d/laavu.conf")
}

func stepNftables(o *Options) error {
	b, _ := deploy.Files.ReadFile("systemd/laavu-nftables.service")
	if _, err := writeIfChanged(filepath.Join("/etc/systemd/system", "laavu-nftables.service"), b, 0o644); err != nil {
		return err
	}
	run(o, "systemctl", "daemon-reload")
	if err := priv.SyncNftables(); err != nil {
		return err
	}
	return run(o, "systemctl", "enable", "--now", "laavu-nftables.service")
}

func stepFirewall(o *Options) error {
	if err := run(o, "ufw", "allow", strconv.Itoa(o.SSHPort)+"/tcp"); err != nil {
		return err
	}
	if o.Domain != "" {
		run(o, "ufw", "allow", "80/tcp")
		run(o, "ufw", "allow", "443/tcp")
		run(o, "ufw", "allow", fmt.Sprintf("%d:%d/tcp", vmDevFrom, vmDevTo))
	} else {
		run(o, "ufw", "allow", fmt.Sprintf("%d:%d/tcp", vmApp, vmView))
		run(o, "ufw", "allow", fmt.Sprintf("%d:%d/tcp", vmDevFrom, vmDevTo))
	}
	// A repeated install keeps the tailnet rules from laavu tailscale enable.
	if st, ok := readTSState(); ok {
		if cfg, err := config.LoadDaemon(filepath.Join(config.Dir, "laavud.toml")); err == nil {
			for _, r := range FirewallRules(st.Mode, o.SSHPort, appPorts(cfg)) {
				run(o, "ufw", r...)
			}
		}
	}
	return run(o, "ufw", "--force", "enable")
}

// CaddyData is the Caddyfile template data.
type CaddyData struct {
	Email    string
	AppSite  string
	AuxSites string
	Internal bool
}

func caddyData(o *Options) CaddyData {
	var aux []string
	if o.tsName != "" {
		return tsCaddyData(o.tsName)
	}
	if o.Domain == "" {
		h := vmHost(o)
		for p := vmDevFrom; p <= vmDevTo; p++ {
			aux = append(aux, fmt.Sprintf("https://%s:%d", h, p))
		}
		return CaddyData{
			AppSite:  fmt.Sprintf("https://%s:%d", h, vmApp),
			AuxSites: strings.Join(append([]string{fmt.Sprintf("https://%s:%d", h, vmView)}, aux...), ", "),
			Internal: true,
		}
	}
	for p := vmDevFrom; p <= vmDevTo; p++ {
		aux = append(aux, fmt.Sprintf("https://%s:%d", o.Domain, p))
	}
	return CaddyData{
		Email:    o.Email,
		AppSite:  o.Domain,
		AuxSites: strings.Join(append([]string{"view." + o.Domain}, aux...), ", "),
	}
}

func stepCaddy(o *Options) error {
	b, err := deploy.Render("Caddyfile.tmpl", caddyData(o))
	if err != nil {
		return err
	}
	changed, err := writeIfChanged("/etc/caddy/Caddyfile", b, 0o644)
	if err != nil {
		return err
	}
	if err := run(o, "caddy", "validate", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"); err != nil {
		return err
	}
	if changed {
		// With admin off, the config cannot be reloaded without a restart.
		return run(o, "systemctl", "restart", "caddy")
	}
	return run(o, "systemctl", "enable", "--now", "caddy")
}

func stepDaemon(o *Options) error {
	laavud := filepath.Join(o.BinDir, "laavud")
	for _, u := range []string{"laavud.service", "laavud-worker@.socket", "laavud-worker@.service"} {
		b, err := deploy.Render("systemd/"+u, map[string]string{"Laavud": laavud})
		if err != nil {
			return err
		}
		if _, err := writeIfChanged(filepath.Join("/etc/systemd/system", u), b, 0o644); err != nil {
			return err
		}
	}
	if err := run(o, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := run(o, "systemctl", "enable", "laavud.service"); err != nil {
		return err
	}
	if err := run(o, "systemctl", "restart", "laavud.service"); err != nil {
		return err
	}
	// Restart the workers if the binary was updated.
	agents, _ := priv.Agents()
	for _, a := range agents {
		run(o, "systemctl", "try-restart", "laavud-worker@"+a.User+".service")
	}
	for i := 0; i < 50; i++ {
		if _, err := os.Stat("/run/laavu/http/app.sock"); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	out, _ := exec.Command("journalctl", "-u", "laavud", "-n", "20", "--no-pager").CombinedOutput()
	return fmt.Errorf("laavud did not start:\n%s", out)
}

// printLink prints the registration link for the first passkey if there is no admin yet.
func printLink(o *Options) error {
	laavud := filepath.Join(o.BinDir, "laavud")
	out, err := exec.Command("runuser", "-u", "laavud", "--", laavud, "invite", "--admin", "-q", "--if-no-admin").Output()
	if err != nil {
		return fmt.Errorf("registration link: %w", err)
	}
	fmt.Fprintln(o.Out)
	if strings.TrimSpace(string(out)) == "" {
		fmt.Fprintln(o.Out, "Done. The admin is already registered; `laavud reset-auth` gives a new link.")
		return nil
	}
	fmt.Fprintln(o.Out, "Done. Open the link and register the admin passkey (the link is single-use and valid for 24 hours):")
	fmt.Fprintln(o.Out, "  "+strings.TrimSpace(string(out)))
	if cfg, err := config.LoadDaemon(filepath.Join(config.Dir, "laavud.toml")); err == nil && portsMode(cfg) {
		fmt.Fprintln(o.Out, "Test VM: the certificate was issued by Caddy's internal CA (/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt).")
	}
	return nil
}
