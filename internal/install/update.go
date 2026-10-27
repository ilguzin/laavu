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

// DefaultReleaseURL is where laavu update gets releases unless told otherwise.
var DefaultReleaseURL = "https://github.com/ilguzin/laavu/releases/latest/download"

// Update implements laavu update laavud|templates|os|claude.
func Update(args []string, w io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	if len(args) == 0 {
		return errors.New("usage: laavu update laavud [--from DIR|URL] | templates | os | claude")
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
	return fmt.Errorf("unknown target %s", args[0])
}

func currentVersion(bin string) string {
	out, _ := exec.Command(bin, "version").Output()
	return strings.TrimSpace(string(out))
}

// updateLaavud installs a signed release and rolls back if laavud does not
// respond after the update.
func updateLaavud(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("update laavud", flag.ExitOnError)
	from := fs.String("from", DefaultReleaseURL, "release directory or https URL")
	force := fs.Bool("force", false, "install even if the version is not newer")
	timeout := fs.Duration("timeout", 20*time.Second, "how long to wait for laavud to respond after the update")
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
		fmt.Fprintf(w, "Installed %s, release has %s: nothing to update.\n", cur, v.Manifest.Version)
		return nil
	}
	fmt.Fprintf(w, "Updating %s → %s (signature verified)\n", cur, v.Manifest.Version)
	suffix := "-linux-" + runtime.GOARCH
	staged := map[string]string{}
	for _, name := range []string{"laavud", "laavu"} {
		dst := filepath.Join(bin, "."+name+".staged")
		if err := v.Fetch(name+suffix, dst); err != nil {
			return err
		}
		staged[name] = dst
	}
	// Keep the old versions alongside for rollback.
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
		fmt.Fprintf(w, "laavud %s is responding.\n", v.Manifest.Version)
		return nil
	}
	fmt.Fprintln(w, "laavud is not responding after the update, rolling back.")
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
		return errors.New("rolled back, but laavud still does not respond: laavu diag")
	}
	return fmt.Errorf("update to %s rolled back, running %s", v.Manifest.Version, cur)
}

func restartAll() error {
	if out, err := exec.Command("systemctl", "restart", "laavud.service").CombinedOutput(); err != nil {
		return fmt.Errorf("%s", out)
	}
	agents, _ := priv.Agents()
	for _, a := range agents {
		exec.Command("systemctl", "try-restart", "laavud-worker@"+a.User+".service").Run()
	}
	// The console link picks up the new binary too. --no-block: this update may be
	// running as a console job, and the link process waits for its result.
	exec.Command("systemctl", "--no-block", "try-restart", "laavu-link.service").Run()
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

// updateTemplates updates the templates in /etc/laavu. It does not touch the rules
// of existing agents: they belong to the agents.
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
	fmt.Fprintln(w, "Templates updated. The new tmux.conf applies to new agent tmux servers; existing agent rules were not changed.")
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
		fmt.Fprintln(w, "Updates installed, a reboot is required. Agent sessions will come back up when opened after it.")
	} else {
		fmt.Fprintln(w, "Updates installed.")
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
			fmt.Fprintf(w, "%s: Claude Code is not installed\n", a.Name)
			continue
		}
		out, err := AsAgent(a.User, a.UID, bin, "update")
		fmt.Fprintf(w, "%s: %s\n", a.Name, lastLines(out, 2))
		if err != nil {
			fmt.Fprintln(w, "  error:", err)
		}
	}
	return nil
}
