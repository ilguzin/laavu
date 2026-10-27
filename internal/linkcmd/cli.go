package linkcmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ilguzin/laavu/deploy"
	"github.com/ilguzin/laavu/internal/install"
	"github.com/ilguzin/laavu/internal/priv"
	"github.com/ilguzin/laavu/pkg/link"
)

const usage = `laavu link [--code CODE] [--console wss://...] [--console-key KEY]
                         bind this server to a console (as root)
laavu link status        binding, rights, connection
laavu link rights [RIGHT=on|off ...]
                         show or change what the console may do
laavu unlink             take control back from the console`

// Main parses `laavu link ...`. Run, bind and helper are for the systemd units.
func Main(args []string, version string, w io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return status(w)
		case "rights":
			return rightsCmd(args[1:], w)
		case "run":
			return runCmd(version)
		case "bind":
			return bindCmd(args[1:], version)
		case "helper":
			return helperCmd()
		case "-h", "--help", "help":
			fmt.Fprintln(w, usage)
			return nil
		}
	}
	return linkCmd(args, version, w)
}

func laavuBin() string {
	if _, err := os.Stat("/usr/local/bin/laavu"); err == nil {
		return "/usr/local/bin/laavu"
	}
	exe, _ := os.Executable()
	return exe
}

func installedVersion(name string) string {
	out, _ := exec.Command("/usr/local/bin/"+name, "version").Output()
	return strings.TrimSpace(string(out))
}

// linkCmd binds the server: as root it prepares the user, key and units, and
// the network part runs as laavu-link in a child process.
func linkCmd(args []string, version string, w io.Writer) error {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	console := fs.String("console", link.DefaultConsole, "console link URL, wss://...")
	code := fs.String("code", "", "binding code from the console, if it gave you one")
	consoleKey := fs.String("console-key", "", "expected console public key (base64), if you have it")
	fs.Parse(args)
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	if *console == "" {
		return errors.New("usage: " + usage)
	}
	if err := link.CheckURL(*console); err != nil {
		return err
	}
	if cfg, err := link.LoadConfig(); err == nil && !cfg.Unlinking {
		return fmt.Errorf("already linked to %s; run laavu unlink first", cfg.Console)
	}
	u, err := ensureUser()
	if err != nil {
		return err
	}
	pub, err := ensureKey(u)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Server key fingerprint: %s\n", link.Fingerprint(pub))
	child := []string{"-u", link.User, "--", laavuBin(), "link", "bind", "--console", *console}
	if *code != "" {
		child = append(child, "--code", *code)
	}
	if *consoleKey != "" {
		child = append(child, "--console-key", *consoleKey)
	}
	cmd := exec.Command("runuser", child...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("binding failed: %w", err)
	}
	var res struct {
		ConsoleKey string `json:"console_key"`
		Account    string `json:"account"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return fmt.Errorf("binding: unexpected output %q", out.String())
	}
	ck, err := link.ParseKey(res.ConsoleKey)
	if err != nil {
		return err
	}
	cfg := &link.Config{Console: *console, ConsoleKey: link.EncodeKey(ck), ServerKey: link.EncodeKey(pub), Bound: time.Now().Unix(),
		Account: res.Account, Rights: link.DefaultRights()}
	if err := cfg.Save(u.gid); err != nil {
		return err
	}
	os.Remove(link.SeenFile)
	os.Remove(link.StatusFile)
	if err := installUnits(); err != nil {
		return err
	}
	priv.Log("link bound to %s, console key %s", *console, link.Fingerprint(ck))
	fmt.Fprintf(w, "Linked. Console key fingerprint: %s\n", link.Fingerprint(ck))
	if res.Account != "" {
		fmt.Fprintf(w, "Console account: %s\nIf that is not your account, someone else typed in the code: run `sudo laavu unlink` now.\n", res.Account)
	}
	printRights(w, cfg)
	fmt.Fprintln(w, "Change them with `laavu link rights NAME=on|off`. `laavu unlink` takes control back.")
	return nil
}

type linkUser struct{ uid, gid int }

func ensureUser() (linkUser, error) {
	if _, err := user.Lookup(link.User); err != nil {
		out, err := exec.Command("useradd", "--system", "--home-dir", "/var/lib/"+link.User, "--no-create-home",
			"--shell", "/usr/sbin/nologin", "--user-group", link.User).CombinedOutput()
		if err != nil {
			return linkUser{}, fmt.Errorf("useradd %s: %s", link.User, strings.TrimSpace(string(out)))
		}
	}
	u, err := user.Lookup(link.User)
	if err != nil {
		return linkUser{}, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return linkUser{uid, gid}, nil
}

// ensureKey creates the server key on the first link and keeps it after: the
// console knows the server by it.
func ensureKey(u linkUser) (ed25519.PublicKey, error) {
	dir := filepath.Dir(link.KeyFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	os.Chown(dir, u.uid, u.gid)
	if k, err := link.ReadKey(link.KeyFile); err == nil {
		return k.Public().(ed25519.PublicKey), nil
	}
	pub, err := link.GenerateKey(link.KeyFile)
	if err != nil {
		return nil, err
	}
	return pub, os.Chown(link.KeyFile, u.uid, u.gid)
}

var units = []string{"laavu-link.service", "laavu-link-helper.socket", "laavu-link-helper.service"}

func installUnits() error {
	for _, name := range units {
		b, err := deploy.Render("systemd/"+name, map[string]string{"Laavu": laavuBin(), "User": link.User})
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(priv.SystemdDir, name), b, 0o644); err != nil {
			return err
		}
	}
	for _, c := range [][]string{
		{"daemon-reload"},
		{"enable", "--now", "laavu-link-helper.socket"},
		{"enable", "laavu-link.service"},
		{"restart", "laavu-link.service"},
	} {
		if out, err := exec.Command("systemctl", c...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %s", strings.Join(c, " "), strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func printRights(w io.Writer, cfg *link.Config) {
	fmt.Fprintln(w, "Console rights:")
	for _, r := range link.RightNamesList() {
		state := "off"
		if cfg.Allowed(r) {
			state = "on"
		}
		fmt.Fprintf(w, "  %-12s %s\n", r, state)
	}
}

func status(w io.Writer) error {
	cfg, err := link.LoadConfig()
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(w, "Not linked to a console.")
		return nil
	}
	if err != nil {
		return err
	}
	ck, _ := link.ParseKey(cfg.ConsoleKey)
	sk, _ := link.ParseKey(cfg.ServerKey)
	fmt.Fprintf(w, "Console: %s (key %s), since %s\n", cfg.Console, link.Fingerprint(ck), time.Unix(cfg.Bound, 0).Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "Server key: %s\n", link.Fingerprint(sk))
	if cfg.Account != "" {
		fmt.Fprintf(w, "Console account: %s\n", cfg.Account)
	}
	if b, err := os.ReadFile(link.StatusFile); err == nil {
		var s link.Status
		if json.Unmarshal(b, &s) == nil {
			at := time.Unix(s.Since, 0).Format("2006-01-02 15:04:05")
			switch {
			case s.Connected:
				fmt.Fprintf(w, "Connected since %s\n", at)
			case s.Error != "":
				fmt.Fprintf(w, "Not connected (%s): %s\n", at, s.Error)
			}
		}
	}
	printRights(w, cfg)
	return nil
}

func rightsCmd(args []string, w io.Writer) error {
	cfg, err := link.LoadConfig()
	if err != nil {
		return errors.New("not linked to a console")
	}
	if len(args) == 0 {
		printRights(w, cfg)
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	known := link.DefaultRights()
	for _, a := range args {
		name, val, ok := strings.Cut(a, "=")
		if _, exists := known[name]; !ok || !exists || (val != "on" && val != "off") {
			return fmt.Errorf("%s: expected RIGHT=on|off, rights: %s", a, strings.Join(link.RightNamesList(), ", "))
		}
		cfg.Rights[name] = val == "on"
	}
	u, err := ensureUser()
	if err != nil {
		return err
	}
	if err := cfg.Save(u.gid); err != nil {
		return err
	}
	priv.Log("link rights %s", strings.Join(args, " "))
	printRights(w, cfg)
	return nil
}

// Unlink implements laavu unlink: tell the console, then remove the binding.
func Unlink(w io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	cfg, err := link.LoadConfig()
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(w, "Not linked to a console.")
		return nil
	}
	if err != nil {
		return err
	}
	u, _ := ensureUser()
	os.Remove(link.StatusFile) // a stale "unlinked" from an earlier binding must not count
	cfg.Unlinking = true
	if err := cfg.Save(u.gid); err != nil {
		return err
	}
	told := false
	for i := 0; i < 40; i++ {
		if b, err := os.ReadFile(link.StatusFile); err == nil {
			var s link.Status
			if json.Unmarshal(b, &s) == nil && s.Unlinked {
				told = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := removeBinding(false); err != nil {
		return err
	}
	if told {
		fmt.Fprintln(w, "Unlinked. The console was told and can no longer run anything here.")
	} else {
		fmt.Fprintln(w, "Unlinked. The console could not be reached; it will see the server as gone.")
	}
	return nil
}

// removeBinding deletes the binding and the key and stops the link. From the
// helper (console unlink) it cannot stop the units right away: the link process
// is still waiting for the result, so a transient timer stops them a bit later.
func removeBinding(fromHelper bool) error {
	os.Remove(link.ConfigFile)
	os.Remove(link.KeyFile)
	os.Remove(link.SeenFile)
	priv.Log("link removed")
	cmds := [][]string{{"systemctl", "disable", "--now", "laavu-link.service", "laavu-link-helper.socket"}}
	if fromHelper {
		cmds = [][]string{
			{"systemctl", "disable", "laavu-link.service", "laavu-link-helper.socket"},
			// If the owner links again within these seconds, the new binding stays.
			{"systemd-run", "--on-active=10s", "--unit=laavu-link-stop", "--collect", "/bin/sh", "-c",
				"test -e " + link.ConfigFile + " || systemctl stop laavu-link.service laavu-link-helper.socket"},
		}
	}
	for _, c := range cmds {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %s", c[0], strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// runCmd is laavu-link.service.
func runCmd(version string) error {
	cfg, err := link.LoadConfig()
	if errors.Is(err, os.ErrNotExist) {
		return nil // unlinked: exit cleanly so systemd does not restart us
	}
	if err != nil {
		return err
	}
	if cfg.Unlinking {
		return nil
	}
	key, err := link.ReadKey(link.KeyFile)
	if err != nil {
		return err
	}
	ck, err := link.ParseKey(cfg.ConsoleKey)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	c := &link.Client{
		URL: cfg.Console, Key: key, ConsoleKey: ck,
		Laavud: installedVersion("laavud"), Laavu: version,
		Backend: &link.SocketBackend{Path: link.HelperSocket}, Config: link.LoadConfig,
		ReportEvery: time.Hour, PollEvery: 5 * time.Second, OnStatus: link.WriteStatus,
	}
	err = c.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// bindCmd runs as laavu-link from linkCmd and prints the console key as JSON.
func bindCmd(args []string, version string) error {
	fs := flag.NewFlagSet("link bind", flag.ExitOnError)
	console := fs.String("console", "", "console URL")
	code := fs.String("code", "", "binding code")
	consoleKey := fs.String("console-key", "", "expected console key")
	fs.Parse(args)
	key, err := link.ReadKey(link.KeyFile)
	if err != nil {
		return err
	}
	c := &link.Client{URL: *console, Key: key, Code: *code, Laavud: installedVersion("laavud"), Laavu: version, Config: link.LoadConfig}
	if *consoleKey != "" {
		if c.ConsoleKey, err = link.ParseKey(*consoleKey); err != nil {
			return err
		}
	}
	c.OnPending = func(p link.Pending) {
		fmt.Fprintf(os.Stderr, "\nIn the console, add a server with this code: %s\n", p.Code)
		fmt.Fprintf(os.Stderr, "Check that the console shows the same server key fingerprint as above.\n")
		fmt.Fprintf(os.Stderr, "The code is valid until %s. Waiting…\n", time.Unix(p.Expires, 0).Format("15:04"))
	}
	ctx, cancel := signalContext()
	defer cancel()
	ctx, cancel2 := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel2()
	var account string
	c.OnBound = func(b link.Bound) { account = b.Account }
	ck, err := c.Bind(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"console_key": link.EncodeKey(ck), "account": account})
}

// helperCmd is laavu-link-helper.service, socket-activated, as root.
func helperCmd() error {
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	l, err := link.ActivatedListener()
	if err != nil {
		return err
	}
	if l == nil {
		return errors.New("laavu link helper is started by laavu-link-helper.socket")
	}
	u, err := user.Lookup(link.User)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	h := &link.Helper{
		Peer:   uid,
		Exec:   link.ExecLaavu(laavuBin(), time.Hour),
		Report: install.BuildReport,
		Agents: func() []string {
			agents, _ := priv.Agents()
			var names []string
			for _, a := range agents {
				names = append(names, a.Name)
			}
			return names
		},
		Unlink: func() error { return removeBinding(true) },
		Log:    priv.Log,
		Now:    time.Now,
	}
	return h.Serve(l, time.Minute)
}
