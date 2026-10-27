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
	"time"

	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/priv"
	"github.com/ilguzin/laavu/pkg/link"
)

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

// BuildReport builds a report on the server state.
func BuildReport() *link.Report {
	r := &link.Report{Schema: link.ReportSchema, Time: time.Now().Unix(), Versions: map[string]string{}, Services: map[string]string{}, Problems: []string{}}
	r.Versions["laavud"] = output("/usr/local/bin/laavud", "version")
	r.Versions["laavu"] = output("/usr/local/bin/laavu", "version")
	r.Versions["tmux"] = output("tmux", "-V")
	r.Versions["caddy"] = strings.Fields(output("caddy", "version") + " ")[0]
	r.Versions["kernel"] = output("uname", "-r")
	for _, u := range []string{"laavud", "caddy", "laavu-nftables", "ufw", "ssh"} {
		r.Services[u] = serviceState(u + ".service")
		if r.Services[u] != "active" {
			r.Problems = append(r.Problems, fmt.Sprintf("service %s: %s", u, r.Services[u]))
		}
	}
	if total, free, ok := diskGB("/"); ok {
		r.Disk = link.DiskReport{TotalGB: round1(total), FreeGB: round1(free)}
		if total > 0 {
			r.Disk.UsedPct = int((total - free) * 100 / total)
		}
		if r.Disk.UsedPct > 90 {
			r.Problems = append(r.Problems, fmt.Sprintf("disk is %d%% full", r.Disk.UsedPct))
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
			r.Problems = append(r.Problems, fmt.Sprintf("certificate %s expires in %d days", c.Name, c.DaysLeft))
		}
	}
	agents, _ := priv.Agents()
	for _, a := range agents {
		ar := link.AgentReport{Name: a.Name, Ports: a.PortsEnv(), Worker: serviceState("laavud-worker@" + a.User + ".service")}
		if v, err := AsAgent(a.User, a.UID, filepath.Join(a.Home, ".local/bin/claude"), "--version"); err == nil {
			ar.Claude = strings.TrimSpace(v)
		}
		if ar.Worker != "active" {
			r.Problems = append(r.Problems, fmt.Sprintf("agent %s worker: %s", a.Name, ar.Worker))
		}
		r.Agents = append(r.Agents, ar)
	}
	if b, err := os.ReadFile(priv.LastCheckFile()); err == nil {
		var lc priv.LastCheck
		if json.Unmarshal(b, &lc) == nil {
			r.Isolation = &link.IsolationReport{Time: lc.Time, Pairs: lc.Pairs, Failed: lc.Failed}
			if lc.Failed > 0 {
				r.Problems = append(r.Problems, "the last isolation check found open probes")
			}
		}
	}
	if len(agents) > 1 && r.Isolation == nil {
		r.Problems = append(r.Problems, "agent isolation has not been checked yet: laavu check-isolation")
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
		r.Problems = append(r.Problems, "a reboot is required after updates")
	}
	if b, err := os.ReadFile(tsStatePath()); err == nil {
		var st tsState
		if json.Unmarshal(b, &st) == nil {
			r.Tailscale = st.Mode
		}
	}
	if cfg, err := config.LoadDaemon(filepath.Join(config.Dir, "laavud.toml")); err == nil {
		r.URL = cfg.PublicURL
	}
	if !appResponds() {
		r.Problems = append(r.Problems, "laavud does not respond on the app socket")
	}
	return r
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

func caddyCerts() []link.CertReport {
	var out []link.CertReport
	root := "/var/lib/caddy/.local/share/caddy/certificates"
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		// Certificates from Caddy's internal CA live for a day and renew themselves.
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
		out = append(out, link.CertReport{Name: strings.TrimSuffix(filepath.Base(p), ".crt"), NotAfter: c.NotAfter.Unix(),
			DaysLeft: int(time.Until(c.NotAfter).Hours() / 24)})
		return nil
	})
	return out
}

// appResponds reports whether laavud responds on its socket.
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

// Check implements laavu check.
func Check(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "report as JSON")
	fs.Parse(args)
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	r := BuildReport()
	if *asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	fmt.Fprintf(w, "laavud %s, laavu %s, %s, caddy %s, kernel %s\n", r.Versions["laavud"], r.Versions["laavu"], r.Versions["tmux"], r.Versions["caddy"], r.Versions["kernel"])
	fmt.Fprintf(w, "Disk: %.1f of %.1f GB free (%d%% used). Memory: %d of %d MB available\n", r.Disk.FreeGB, r.Disk.TotalGB, r.Disk.UsedPct, r.MemoryMB.Available, r.MemoryMB.Total)
	for _, c := range r.Certs {
		fmt.Fprintf(w, "Certificate %s: %d days left\n", c.Name, c.DaysLeft)
	}
	for _, a := range r.Agents {
		fmt.Fprintf(w, "Agent %s: worker %s, ports %s, claude %s\n", a.Name, a.Worker, a.Ports, orDash(a.Claude))
	}
	if r.Isolation != nil {
		fmt.Fprintf(w, "Isolation checked %s: %d pairs, %d open\n", time.Unix(r.Isolation.Time, 0).Format("2006-01-02 15:04"), r.Isolation.Pairs, r.Isolation.Failed)
	}
	fmt.Fprintf(w, "OS updates: %d\n", r.Updates)
	if len(r.Problems) == 0 {
		fmt.Fprintln(w, "No problems.")
		return nil
	}
	fmt.Fprintln(w, "Problems:")
	for _, p := range r.Problems {
		fmt.Fprintln(w, "  -", p)
	}
	return fmt.Errorf("problems: %d", len(r.Problems))
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// Firewall implements laavu firewall status|allow|deny.
func Firewall(args []string, w io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	if len(args) == 0 || args[0] == "status" {
		out, err := exec.Command("ufw", "status", "verbose").CombinedOutput()
		w.Write(out)
		return err
	}
	if len(args) != 2 || (args[0] != "allow" && args[0] != "deny") {
		return errors.New("usage: laavu firewall status|allow PORT[/tcp]|deny PORT[/tcp]")
	}
	port := args[1]
	if _, err := strconv.Atoi(strings.SplitN(port, "/", 2)[0]); err != nil {
		return errors.New("port must be a number, e.g. 8080 or 8080/tcp")
	}
	priv.Log("firewall %s %s", args[0], port)
	out, err := exec.Command("ufw", args[0], port).CombinedOutput()
	w.Write(out)
	return err
}

// Cleanup removes old logs, the apt cache and agent temporary files older than a week.
func Cleanup(w io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("root required")
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
			fmt.Fprintln(w, "  error:", err)
		}
	}
	// The agent home belongs to the agent: root does not walk it (a symlink ~/tmp -> /
	// would turn cleanup into deleting the system). The agent cleans it itself, in its own slice.
	agents, _ := priv.Agents()
	for _, a := range agents {
		out, err := AsAgent(a.User, a.UID, "find", "-P", filepath.Join(a.Home, "tmp"), "-xdev", "-mindepth", "1", "-maxdepth", "1",
			"-mtime", "+7", "-print", "-exec", "rm", "-rf", "--", "{}", "+")
		n := 0
		if s := strings.TrimSpace(out); s != "" {
			n = len(strings.Split(s, "\n"))
		}
		fmt.Fprintf(w, "agent %s: removed from ~/tmp: %d\n", a.Name, n)
		if err != nil {
			fmt.Fprintln(w, "  error:", err)
		}
	}
	return nil
}

// AsAgent runs a command as the agent in its slice, without root's terminal.
func AsAgent(user string, uid int, name string, args ...string) (string, error) {
	full := append([]string{"--quiet", "--pipe", "--wait", "--collect", "--uid=" + user,
		fmt.Sprintf("--slice=user-%d.slice", uid), "-p", "UMask=0077", "--", name}, args...)
	cmd := exec.Command("systemd-run", full...)
	cmd.Stdin = nil
	out, err := cmd.Output()
	return string(out), err
}

// Diag collects diagnostics for troubleshooting, without the contents of work folders.
func Diag(w io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("root required")
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
