package install

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ilguzin/laavu/deploy"
	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/priv"
	"github.com/ilguzin/laavu/internal/release"
)

// DefaultReleaseURL — откуда laavu update берёт релизы, если не указано иное.
var DefaultReleaseURL = "https://github.com/ilguzin/laavu/releases/latest/download"

// Update — laavu update laavud|templates|os|claude.
func Update(args []string, w io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("нужен root")
	}
	if len(args) == 0 {
		return errors.New("использование: laavu update laavud [--from КАТАЛОГ|URL] | templates | os | claude")
	}
	switch args[0] {
	case "laavud":
		return updateLaavud(args[1:], w)
	case "templates":
		return updateTemplates(w)
	case "os":
		return updateOS(w)
	case "claude":
		return updateClaude(w)
	}
	return fmt.Errorf("неизвестная цель %s", args[0])
}

func currentVersion(bin string) string {
	out, _ := exec.Command(bin, "version").Output()
	return strings.TrimSpace(string(out))
}

// updateLaavud ставит подписанный релиз и откатывается, если laavud после
// обновления не отвечает.
func updateLaavud(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("update laavud", flag.ExitOnError)
	from := fs.String("from", DefaultReleaseURL, "каталог или https-адрес релиза")
	force := fs.Bool("force", false, "ставить, даже если версия не новее")
	timeout := fs.Duration("timeout", 20*time.Second, "сколько ждать ответа laavud после обновления")
	fs.Parse(args)
	pub, err := release.PinnedKey()
	if err != nil {
		return err
	}
	v, err := release.Verify(release.Source(*from), pub)
	if err != nil {
		return err
	}
	bin := "/usr/local/bin"
	cur := currentVersion(filepath.Join(bin, "laavud"))
	if !*force && !release.Newer(v.Manifest.Version, cur) {
		fmt.Fprintf(w, "Стоит %s, в релизе %s — обновлять нечего.\n", cur, v.Manifest.Version)
		return nil
	}
	fmt.Fprintf(w, "Обновляю %s → %s (подпись проверена)\n", cur, v.Manifest.Version)
	suffix := "-linux-" + runtime.GOARCH
	staged := map[string]string{}
	for _, name := range []string{"laavud", "laavu"} {
		dst := filepath.Join(bin, "."+name+".staged")
		if err := v.Fetch(name+suffix, dst); err != nil {
			return err
		}
		staged[name] = dst
	}
	// Старые версии рядом — для отката.
	for name := range staged {
		p := filepath.Join(bin, name)
		if b, err := os.ReadFile(p); err == nil {
			os.WriteFile(p+".prev", b, 0o755)
		}
	}
	for name, src := range staged {
		if err := os.Rename(src, filepath.Join(bin, name)); err != nil {
			return err
		}
	}
	priv.Log("update laavud %s -> %s", cur, v.Manifest.Version)
	if err := restartAll(); err == nil && waitApp(*timeout) {
		fmt.Fprintf(w, "laavud %s отвечает.\n", v.Manifest.Version)
		return nil
	}
	fmt.Fprintln(w, "laavud после обновления не отвечает — откатываю.")
	for name := range staged {
		p := filepath.Join(bin, name)
		if b, err := os.ReadFile(p + ".prev"); err == nil {
			os.WriteFile(p+".rollback", b, 0o755)
			os.Rename(p+".rollback", p)
		}
	}
	priv.Log("update laavud rollback to %s", cur)
	restartAll()
	if !waitApp(*timeout) {
		return errors.New("откат сделан, но laavud всё равно не отвечает: laavu diag")
	}
	return fmt.Errorf("обновление до %s откатено, работает %s", v.Manifest.Version, cur)
}

func restartAll() error {
	if out, err := exec.Command("systemctl", "restart", "laavud.service").CombinedOutput(); err != nil {
		return fmt.Errorf("%s", out)
	}
	agents, _ := priv.Agents()
	for _, a := range agents {
		exec.Command("systemctl", "try-restart", "laavud-worker@"+a.User+".service").Run()
	}
	return nil
}

func waitApp(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if appResponds() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// updateTemplates обновляет шаблоны в /etc/laavu. Правила уже созданных агентов
// не трогает: они принадлежат агентам.
func updateTemplates(w io.Writer) error {
	laavud := "/usr/local/bin/laavud"
	b, err := deploy.Render("tmux.conf.tmpl", map[string]string{"Laavud": laavud})
	if err != nil {
		return err
	}
	if _, err := writeIfChanged(filepath.Join(config.Dir, "tmux.conf"), b, 0o644); err != nil {
		return err
	}
	for _, f := range []string{"CLAUDE.md.tmpl", "settings.json.tmpl"} {
		src, _ := deploy.Files.ReadFile("agent/" + f)
		dst := filepath.Join(config.Dir, "agent-template", f)
		if old, err := os.ReadFile(dst); err == nil && string(old) != string(src) {
			os.WriteFile(dst+".bak", old, 0o644)
		}
		if _, err := writeIfChanged(dst, src, 0o644); err != nil {
			return err
		}
	}
	priv.Log("update templates")
	fmt.Fprintln(w, "Шаблоны обновлены. Новый tmux.conf действует для новых серверов tmux агентов; правила существующих агентов не менялись.")
	return nil
}

func updateOS(w io.Writer) error {
	o := &Options{Out: w}
	priv.Log("update os")
	if err := run(o, "apt-get", "update", "-q"); err != nil {
		return err
	}
	if err := run(o, "apt-get", "-y", "-q", "-o", "Dpkg::Options::=--force-confold", "upgrade"); err != nil {
		return err
	}
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		fmt.Fprintln(w, "Обновления поставлены, нужна перезагрузка. Сессии агентов после неё поднимутся при открытии.")
	} else {
		fmt.Fprintln(w, "Обновления поставлены.")
	}
	return nil
}

func updateClaude(w io.Writer) error {
	agents, err := priv.Agents()
	if err != nil {
		return err
	}
	priv.Log("update claude")
	for _, a := range agents {
		bin := filepath.Join(a.Home, ".local/bin/claude")
		if _, err := os.Lstat(bin); err != nil {
			fmt.Fprintf(w, "%s: Claude Code не стоит\n", a.Name)
			continue
		}
		out, err := AsAgent(a.User, a.UID, bin, "update")
		fmt.Fprintf(w, "%s: %s\n", a.Name, lastLines(out, 2))
		if err != nil {
			fmt.Fprintln(w, "  ошибка:", err)
		}
	}
	return nil
}
