// Package install — laavu install на сервере: харденинг, пользователи, Caddy,
// laavud, меры изоляции. Повторный запуск ничего не ломает.
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

// Options — параметры установки.
type Options struct {
	// Domain — домен сервера. Пусто — режим тестовой ВМ: порты 8443/8444/8450-8459
	// и сертификат внутреннего CA Caddy.
	Domain string
	// Host — имя сервера в режиме тестовой ВМ (по умолчанию localhost), например
	// имя хоста с VirtualBox в tailnet. Passkey привязаны к имени: смена имени —
	// это новая регистрация passkey.
	Host  string
	Email string
	// Agent — имя первого агента; пусто — без агента.
	Agent         string
	InstallClaude bool
	// OwnerKey — открытый ключ владельца для ssh; кладётся до запрета входа по паролю.
	OwnerKey string
	// OwnerUser — пользователь владельца на сервере (для ключа и группы procview).
	OwnerUser string
	// HardenSSH — запретить вход по паролю и под root (только если ключ владельца на месте).
	HardenSSH bool
	SSHPort   int
	// SkipApt — не ставить пакеты (тесты, офлайн).
	SkipApt bool
	BinDir  string
	// ReleaseKey — открытый ключ проекта для проверки обновлений, закрепляется в /etc/laavu/release.pub.
	ReleaseKey string
	Out        io.Writer
}

// VM-режим: порты одинаковые внутри ВМ и на хосте.
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

// Run выполняет установку.
func Run(o Options) error {
	if os.Geteuid() != 0 {
		return errors.New("laavu install запускается от root (или через sudo)")
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
	steps := []step{
		{"пакеты", stepPackages},
		{"бинарники", stepBinaries},
		{"пользователи и группы", stepUsers},
		{"процессы других пользователей скрыты (hidepid)", stepHidepid},
		{"ssh", stepSSH},
		{"конфигурация /etc/laavu", stepConfig},
		{"правила nftables между агентами", stepNftables},
		{"брандмауэр", stepFirewall},
		{"Caddy", stepCaddy},
		{"laavud", stepDaemon},
	}
	for i, s := range steps {
		fmt.Fprintf(o.Out, "[%d/%d] %s\n", i+1, len(steps)+1, s.name)
		if err := s.fn(&o); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	fmt.Fprintf(o.Out, "[%d/%d] первый агент\n", len(steps)+1, len(steps)+1)
	if o.Agent != "" {
		if _, err := user.Lookup("lv-" + o.Agent); err == nil {
			fmt.Fprintf(o.Out, "  агент %s уже есть\n", o.Agent)
		} else {
			if _, err := priv.CreateAgent(priv.CreateOptions{Name: o.Agent, InstallClaude: o.InstallClaude, Laavud: filepath.Join(o.BinDir, "laavud"), Out: o.Out}); err != nil {
				return fmt.Errorf("агент: %w", err)
			}
		}
	}
	priv.Log("install domain=%q agent=%q", o.Domain, o.Agent)
	return printLink(&o)
}

func checkOS() error {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return err
	}
	s := string(b)
	if !strings.Contains(s, "ID=ubuntu") {
		return errors.New("поддерживается только Ubuntu 24.04 и новее")
	}
	for _, line := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(line, "VERSION_ID="); ok {
			v = strings.Trim(v, `"`)
			if major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0]); major < 24 {
				return fmt.Errorf("версия Ubuntu %s слишком старая, нужна 24.04 и новее", v)
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

// writeIfChanged пишет файл, если содержимое отличается; возвращает, изменился ли он.
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
	fmt.Fprintf(o.Out, "  ставлю: %s\n", strings.Join(missing, " "))
	if err := run(o, "apt-get", "update", "-q"); err != nil {
		return err
	}
	return run(o, "apt-get", append([]string{"install", "-y", "-q", "--no-install-recommends"}, missing...)...)
}

// stepBinaries кладёт laavu и laavud в /usr/local/bin. Источник — каталог
// запущенного laavu.
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
			return fmt.Errorf("нет %s рядом с laavu: %w", name, err)
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
	// Владелец сервера видит все процессы.
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
		s := strings.TrimRight(string(b), "\n") + "\n# Laavu: пользователи видят только свои процессы\n" + line + "\n"
		if _, err := writeIfChanged("/etc/fstab", []byte(s), 0o644); err != nil {
			return err
		}
	}
	return run(o, "mount", "-o", "remount,hidepid=invisible,gid="+g.Gid, "/proc")
}

// HardenSSH — только шаг ssh: ключ владельца и запрет пароля (laavu harden-ssh).
func HardenSSH(o Options) error {
	if os.Geteuid() != 0 {
		return errors.New("нужен root")
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
	conf := "# Laavu: агенты по ssh не входят.\nDenyGroups laavu-agents\n"
	if o.HardenSSH {
		if !ownerHasKey(o) {
			return errors.New("запрет пароля без ключа владельца закрыл бы доступ к серверу; передайте --owner-key")
		}
		conf += "PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin prohibit-password\n"
	}
	// В sshd_config.d действует первое значение, а файлы читаются по алфавиту:
	// наш файл должен идти раньше 50-cloud-init.conf, где хостеры включают пароль.
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
			return errors.New("sshd всё ещё разрешает вход по паролю: проверьте /etc/ssh/sshd_config и sshd_config.d")
		}
	}
	if !strings.Contains(outputOf("sshd", "-T"), "\ndenygroups laavu-agents\n") {
		return errors.New("DenyGroups laavu-agents не применился: проверьте sshd_config.d")
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
		// Без wildcard-сертификата dev-серверы открываются на портах домена.
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

// portsMode — сервер в режиме тестовой ВМ: приложение на порту 8443.
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
	if cur, err := config.LoadDaemon(path); err == nil && o.Domain == "" && o.Host != "" {
		// Смена имени сервера в режиме ВМ: адреса переписываются, остальное остаётся.
		want := daemonConfig(o)
		if cur.PublicURL != want.PublicURL {
			cur.PublicURL, cur.ViewURL = want.PublicURL, want.ViewURL
			if err := config.WriteDaemon(path, cur); err != nil {
				return err
			}
			fmt.Fprintf(o.Out, "  адрес сервера теперь %s — passkey нужно зарегистрировать заново (laavud reset-auth)\n", cur.PublicURL)
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
				continue // список дополняет владелец
			}
		}
		if _, err := writeIfChanged(target, b, 0o644); err != nil {
			return err
		}
	}
	// Шаблон правил агента — для правки владельцем; priv берёт его вместо вшитого.
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
	return run(o, "ufw", "--force", "enable")
}

// CaddyData — данные шаблона Caddyfile.
type CaddyData struct {
	Email    string
	AppSite  string
	AuxSites string
	Internal bool
}

func caddyData(o *Options) CaddyData {
	var aux []string
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
		// С admin off перезагрузка конфига без рестарта невозможна.
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
	// Перезапуск воркеров, если бинарник обновился.
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
	return fmt.Errorf("laavud не запустился:\n%s", out)
}

// printLink печатает ссылку регистрации первого passkey, если администратора ещё нет.
func printLink(o *Options) error {
	laavud := filepath.Join(o.BinDir, "laavud")
	out, err := exec.Command("runuser", "-u", "laavud", "--", laavud, "invite", "--admin", "-q", "--if-no-admin").Output()
	if err != nil {
		return fmt.Errorf("ссылка регистрации: %w", err)
	}
	fmt.Fprintln(o.Out)
	if strings.TrimSpace(string(out)) == "" {
		fmt.Fprintln(o.Out, "Готово. Администратор уже зарегистрирован; новую ссылку даёт `laavud reset-auth`.")
		return nil
	}
	fmt.Fprintln(o.Out, "Готово. Откройте ссылку и зарегистрируйте passkey администратора (ссылка одноразовая, действует сутки):")
	fmt.Fprintln(o.Out, "  "+strings.TrimSpace(string(out)))
	if o.Domain == "" {
		fmt.Fprintln(o.Out, "Тестовая ВМ: сертификат выпущен внутренним CA Caddy (/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt).")
	}
	return nil
}
