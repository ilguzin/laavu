// laavud is the Laavu daemon, agent worker, hooks and privileged operations.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
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
	"github.com/ilguzin/laavu/internal/auth"
	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/daemon"
	"github.com/ilguzin/laavu/internal/hook"
	"github.com/ilguzin/laavu/internal/names"
	"github.com/ilguzin/laavu/internal/priv"
	"github.com/ilguzin/laavu/internal/store"
	"github.com/ilguzin/laavu/internal/webui"
	"github.com/ilguzin/laavu/internal/wire"
	"github.com/ilguzin/laavu/internal/worker"
	"golang.org/x/term"
)

// Version is set at build time.
var Version = "dev"

const usage = `laavud - the Laavu daemon.

  laavud serve                  daemon (systemd unit laavud.service)
  laavud worker                 agent worker (laavud-worker@<user>.service)
  laavud hook claude|guard|statusline
                                Claude Code hooks from the agent settings
  laavud hook tmux <event>      tmux hooks from /etc/laavu/tmux.conf
  laavud reset-auth             erase all passkeys and print an admin registration link
  laavud invite [--admin] [--name NAME] [--agent AGENT]...
                                registration link for a new user; a member gets
                                access to the given agents (asked if not given)
  laavud admin add NAME         make a user an admin
  laavud access grant|revoke USER AGENT...
                                give or take a member's access to agents
  laavud access list [USER]     who has access to which agents
  laavud access drop AGENT      remove everyone's access (on agent deletion)
  laavud priv ...               root operations: create-agent, delete-agent, check-isolation
  laavud call AGENT OP [JSON]
                                request to an agent worker (diagnostics; as laavud or root)
  laavud dev --home DIR         daemon and worker in one process, for development
  laavud version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "worker":
		err = runWorker(args)
	case "hook":
		runHook(args)
		return
	case "reset-auth":
		err = resetAuth(args)
	case "invite":
		err = invite(args)
	case "admin":
		err = admin(args)
	case "access":
		err = access(args)
	case "priv":
		err = priv.Main(args)
	case "dev":
		err = dev(args)
	case "call":
		err = call(args)
	case "version":
		fmt.Println(Version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "laavud:", err)
		os.Exit(1)
	}
}

func logger() *slog.Logger {
	lvl := slog.LevelInfo
	if os.Getenv("LAAVU_DEBUG") != "" {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	confDir := fs.String("config-dir", config.Dir, "configuration directory")
	fs.Parse(args)
	cfg, err := config.LoadDaemon(filepath.Join(*confDir, "laavud.toml"))
	if err != nil {
		return err
	}
	if cfg.NoAuth {
		return errors.New("no_auth is only allowed in laavud dev on localhost")
	}
	agents, err := config.LoadAgents(filepath.Join(*confDir, "agents"))
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.StateDir, "laavud.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	log := logger()
	d, err := daemon.New(cfg, agents, st, webui.FS(), log)
	if err != nil {
		return err
	}
	app, err := daemon.Listen(cfg.Listen, 0o666, -1)
	if err != nil {
		return err
	}
	aux, err := daemon.Listen(cfg.AuxListen, 0o666, -1)
	if err != nil {
		return err
	}
	hooks, err := daemon.Listen("unix:"+cfg.HookSocket(), 0o666, -1)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	log.Info("laavud started", "version", Version, "agents", len(agents), "url", cfg.PublicURL)
	return d.Serve(ctx, app, []net.Listener{aux}, hooks)
}

func runWorker(args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	confDir := fs.String("config-dir", config.Dir, "configuration directory")
	fs.Parse(args)
	me, err := user.Current()
	if err != nil {
		return err
	}
	agents, err := config.LoadAgents(filepath.Join(*confDir, "agents"))
	if err != nil {
		return err
	}
	var ag *config.Agent
	for i := range agents {
		if agents[i].User == me.Username {
			ag = &agents[i]
		}
	}
	if ag == nil {
		return fmt.Errorf("user %s is not a Laavu agent", me.Username)
	}
	cfg, err := config.LoadDaemon(filepath.Join(*confDir, "laavud.toml"))
	if err != nil {
		return err
	}
	reserved, err := names.LoadReserved(filepath.Join(*confDir, "reserved"))
	if err != nil {
		return err
	}
	self, _ := os.Executable()
	w, err := worker.New(worker.Options{
		Agent: *ag, Reserved: reserved, TmuxConf: filepath.Join(*confDir, "tmux.conf"),
		HookSocket: cfg.HookSocket(), LaavudBin: self, Scope: true, Log: logger(),
	})
	if err != nil {
		return err
	}
	w.UploadLimit = cfg.UploadLimit << 20
	l, err := worker.Listener(ag.WorkerSocket)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	go func() { <-ctx.Done(); l.Close() }()
	return w.Serve(l)
}

func runHook(args []string) {
	// A hook never breaks the agent: all errors are silent, exit code 0.
	defer func() { recover() }()
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case "claude":
		hook.Claude(os.Stdin)
	case "guard":
		hook.NewGuard().Run(os.Stdin, os.Stdout)
	case "statusline":
		home, _ := os.UserHomeDir()
		hook.Statusline(os.Stdin, os.Stdout, filepath.Join(home, ".local/state/laavu"))
	case "tmux":
		if len(args) > 1 {
			hook.Tmux(args[1])
		}
	}
}

// asLaavud re-runs the command as the laavud user if root invoked it: the
// database belongs to laavud, and SQLite journal files owned by root would break the daemon.
func asLaavud() (bool, error) {
	if os.Geteuid() != 0 {
		return false, nil
	}
	if _, err := user.Lookup("laavud"); err != nil {
		return false, nil
	}
	self, _ := os.Executable()
	cmd := exec.Command("runuser", append([]string{"-u", "laavud", "--", self}, os.Args[1:]...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return true, cmd.Run()
}

func openStore(confDir string) (*store.Store, config.Daemon, error) {
	cfg, err := config.LoadDaemon(filepath.Join(confDir, "laavud.toml"))
	if err != nil {
		return nil, cfg, err
	}
	st, err := store.Open(filepath.Join(cfg.StateDir, "laavud.db"))
	return st, cfg, err
}

func resetAuth(args []string) error {
	if done, err := asLaavud(); done {
		return err
	}
	fs := flag.NewFlagSet("reset-auth", flag.ExitOnError)
	confDir := fs.String("config-dir", config.Dir, "configuration directory")
	fs.Parse(args)
	st, cfg, err := openStore(*confDir)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.DeleteAllAuth(); err != nil {
		return err
	}
	users, _ := st.Users()
	var inv store.Invite
	for _, u := range users {
		if u.Admin() {
			id := u.ID
			inv = store.Invite{UserID: &id, Role: "admin"}
			break
		}
	}
	if inv.UserID == nil {
		inv = store.Invite{Role: "admin"}
	}
	tok, err := st.CreateInvite(inv, auth.InviteTTL)
	if err != nil {
		return err
	}
	fmt.Println("All passkeys and sign-ins removed. Admin registration link (valid for 24 hours):")
	fmt.Println(strings.TrimRight(cfg.PublicURL, "/") + "/register#" + tok)
	return nil
}

func invite(args []string) error {
	if done, err := asLaavud(); done {
		return err
	}
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	confDir := fs.String("config-dir", config.Dir, "configuration directory")
	isAdmin := fs.Bool("admin", false, "admin role")
	name := fs.String("name", "", "user name (by default the user picks it)")
	quiet := fs.Bool("q", false, "print only the link")
	ifNoAdmin := fs.Bool("if-no-admin", false, "do nothing if an admin already exists")
	var agents agentList
	fs.Var(&agents, "agent", "agent the member gets access to (repeatable, or comma-separated)")
	fs.Parse(args)
	if *isAdmin && len(agents) > 0 {
		return errors.New("--agent is for members: an admin sees all agents")
	}
	st, cfg, err := openStore(*confDir)
	if err != nil {
		return err
	}
	defer st.Close()
	if *ifNoAdmin {
		users, _ := st.Users()
		for _, u := range users {
			if u.Admin() {
				return nil
			}
		}
	}
	role := "member"
	if *isAdmin {
		role = "admin"
	} else {
		for _, a := range agents {
			if err := agentExists(*confDir, a); err != nil {
				return err
			}
		}
		if len(agents) == 0 {
			if agents, err = pickAgents(*confDir); err != nil {
				return err
			}
		}
	}
	tok, err := st.CreateInvite(store.Invite{Role: role, Name: *name, Agents: agents}, auth.InviteTTL)
	if err != nil {
		return err
	}
	link := strings.TrimRight(cfg.PublicURL, "/") + "/register#" + tok
	if !*quiet {
		fmt.Println("Registration link (single use, valid for 24 hours):")
	}
	fmt.Println(link)
	return nil
}

func admin(args []string) error {
	if done, err := asLaavud(); done {
		return err
	}
	if len(args) != 2 || args[0] != "add" {
		return errors.New("usage: laavud admin add NAME")
	}
	st, _, err := openStore(config.Dir)
	if err != nil {
		return err
	}
	defer st.Close()
	u, err := st.UserByName(args[1])
	if err != nil {
		return fmt.Errorf("user %s: %w", args[1], err)
	}
	return st.SetRole(u.ID, "admin")
}

// agentList is a repeatable flag; each value may also be comma-separated.
type agentList []string

func (l *agentList) String() string { return strings.Join(*l, ",") }

func (l *agentList) Set(v string) error {
	for _, a := range strings.Split(v, ",") {
		if a = strings.TrimSpace(a); a != "" {
			*l = append(*l, a)
		}
	}
	return nil
}

// agentNames lists the agents from the configuration by file name. Unlike
// config.LoadAgents it does not resolve system users, which laavud does not need here.
func agentNames(confDir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(confDir, "agents", "*.toml"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		out = append(out, strings.TrimSuffix(filepath.Base(f), ".toml"))
	}
	return out, nil
}

func agentExists(confDir, name string) error {
	if err := names.Agent(name); err != nil {
		return fmt.Errorf("agent %s: %w", name, err)
	}
	if _, err := os.Stat(filepath.Join(confDir, "agents", name+".toml")); err != nil {
		return fmt.Errorf("agent %s: not found", name)
	}
	return nil
}

// pickAgents chooses access for an invited member when --agent is not given.
// A single agent is granted by default. With several, the administrator is
// asked on a terminal; without one the member gets no access and a warning.
// Prompts go to stderr so that -q still prints only the link.
func pickAgents(confDir string) ([]string, error) {
	all, err := agentNames(confDir)
	if err != nil {
		return nil, err
	}
	switch {
	case len(all) == 0:
		fmt.Fprintln(os.Stderr, "No agents yet. Give access later: laavud access grant USER AGENT")
		return nil, nil
	case len(all) == 1:
		fmt.Fprintf(os.Stderr, "Access to: %s (the only agent)\n", all[0])
		return all, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "No access to agents: there are several (%s) and no --agent.\n"+
			"Give access later: laavud access grant USER AGENT\n", strings.Join(all, ", "))
		return nil, nil
	}
	fmt.Fprintln(os.Stderr, "Agents:")
	for i, a := range all {
		fmt.Fprintf(os.Stderr, "  %d) %s\n", i+1, a)
	}
	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Fprint(os.Stderr, "Access to (numbers or names, comma-separated; \"all\"; empty for none): ")
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return nil, err
		}
		got, perr := parsePick(strings.TrimSpace(line), all)
		if perr == nil {
			if len(got) == 0 {
				fmt.Fprintln(os.Stderr, "No access to agents. Give it later: laavud access grant USER AGENT")
			} else {
				fmt.Fprintf(os.Stderr, "Access to: %s\n", strings.Join(got, ", "))
			}
			return got, nil
		}
		fmt.Fprintln(os.Stderr, perr)
	}
}

// parsePick turns an answer like "1, research" into agent names.
func parsePick(answer string, all []string) ([]string, error) {
	if answer == "all" {
		return all, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Split(answer, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		name := ""
		if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= len(all) {
			name = all[n-1]
		} else {
			for _, a := range all {
				if a == f {
					name = a
				}
			}
		}
		if name == "" {
			return nil, fmt.Errorf("no such agent: %s", f)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

// access manages access entries: a member sees only the agents granted here.
// The daemon checks them on every request, so changes apply without a restart.
func access(args []string) error {
	if done, err := asLaavud(); done {
		return err
	}
	fs := flag.NewFlagSet("access", flag.ExitOnError)
	confDir := fs.String("config-dir", config.Dir, "configuration directory")
	const use = "usage: laavud access grant|revoke USER AGENT... | laavud access list [USER] | laavud access drop AGENT"
	if len(args) == 0 {
		return errors.New(use)
	}
	op := args[0]
	fs.Parse(args[1:])
	rest := fs.Args()
	st, _, err := openStore(*confDir)
	if err != nil {
		return err
	}
	defer st.Close()
	switch op {
	case "grant", "revoke":
		if len(rest) < 2 {
			return errors.New(use)
		}
		u, err := st.UserByName(rest[0])
		if err != nil {
			return fmt.Errorf("user %s: %w", rest[0], err)
		}
		for _, a := range rest[1:] {
			if op == "revoke" {
				err = st.RevokeAccess(u.ID, a)
			} else if err = agentExists(*confDir, a); err == nil {
				err = st.GrantAccess(u.ID, a)
			}
			if err != nil {
				return err
			}
		}
		if u.Admin() {
			fmt.Printf("%s is an admin and sees all agents regardless of access entries\n", u.Name)
		}
		return nil
	case "drop":
		// The agent's file is usually gone by now, so the name is not checked against it.
		if len(rest) != 1 {
			return errors.New(use)
		}
		return st.DropAgent(rest[0])
	case "list":
		if len(rest) > 1 {
			return errors.New(use)
		}
		users, err := st.Users()
		if err != nil {
			return err
		}
		for _, u := range users {
			if len(rest) == 1 && u.Name != rest[0] {
				continue
			}
			agents := "(admin: all)"
			if !u.Admin() {
				list, err := st.AgentsOf(u.ID)
				if err != nil {
					return err
				}
				agents = strings.Join(list, " ")
				if agents == "" {
					agents = "(none)"
				}
			}
			fmt.Printf("%s\t%s\n", u.Name, agents)
		}
		return nil
	}
	return errors.New(use)
}

// dev runs the daemon and worker in one process as the current user, without
// Caddy and without sign-in. Only for development on localhost.
func dev(args []string) error {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	home := fs.String("home", "", "agent home (required, not your real home)")
	listen := fs.Int("port", 7700, "app port; the next one is the view origin, +10..+19 are dev servers")
	agentName := fs.String("agent", "dev", "agent name")
	claude := fs.String("claude", "claude", "agent command")
	withAuth := fs.Bool("auth", false, "enable passkey sign-in")
	fs.Parse(args)
	if *home == "" {
		return errors.New("--home is required")
	}
	abs, err := filepath.Abs(*home)
	if err != nil {
		return err
	}
	os.MkdirAll(abs, 0o700)
	me, _ := user.Current()
	uid, _ := strconv.Atoi(me.Uid)
	runDir, err := os.MkdirTemp("", "laavu-dev-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(runDir)
	self, _ := os.Executable()
	conf, err := deploy.Render("tmux.conf.tmpl", map[string]string{"Laavud": self})
	if err != nil {
		return err
	}
	tmuxConf := filepath.Join(runDir, "tmux.conf")
	os.WriteFile(tmuxConf, conf, 0o644)

	ag := config.Agent{Name: *agentName, User: me.Username, Home: abs, Ports: [2]int{20000, 20999},
		Claude: *claude, TmuxSocket: "laavu-dev", WorkerSocket: filepath.Join(runDir, "worker.sock"), UID: uid}
	cfg := config.Defaults()
	cfg.PublicURL = fmt.Sprintf("http://localhost:%d", *listen)
	cfg.ViewURL = fmt.Sprintf("http://localhost:%d", *listen+1)
	cfg.DevPorts = [2]int{*listen + 10, *listen + 19}
	cfg.RunDir = runDir
	cfg.StateDir = filepath.Join(abs, ".local/state/laavu-dev-daemon")
	cfg.PushDelay.Duration = 3 * time.Second
	cfg.NoAuth = !*withAuth
	os.MkdirAll(filepath.Join(runDir, "hooks"), 0o700)
	os.MkdirAll(cfg.StateDir, 0o700)

	log := logger()
	w, err := worker.New(worker.Options{Agent: ag, TmuxConf: tmuxConf, HookSocket: cfg.HookSocket(), LaavudBin: self, Log: log})
	if err != nil {
		return err
	}
	w.LocalClone = true
	wl, err := worker.Listener(ag.WorkerSocket)
	if err != nil {
		return err
	}
	go w.Serve(wl)

	st, err := store.Open(filepath.Join(cfg.StateDir, "laavud.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	d, err := daemon.New(cfg, []config.Agent{ag}, st, webui.FS(), log)
	if err != nil {
		return err
	}
	app, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *listen))
	if err != nil {
		return err
	}
	var aux []net.Listener
	for _, p := range append([]int{*listen + 1}, rangeInts(cfg.DevPorts[0], cfg.DevPorts[1])...) {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			return err
		}
		aux = append(aux, l)
	}
	hooks, err := daemon.Listen("unix:"+cfg.HookSocket(), 0o600, -1)
	if err != nil {
		return err
	}
	if *withAuth {
		tok, _ := st.CreateInvite(store.Invite{Role: "admin"}, auth.InviteTTL)
		fmt.Println("Registration link:", cfg.PublicURL+"/register#"+tok)
	}
	fmt.Println("Laavu dev:", cfg.PublicURL)
	ctx, cancel := signalContext()
	defer cancel()
	go func() { <-ctx.Done(); wl.Close() }()
	return d.Serve(ctx, app, aux, hooks)
}

func rangeInts(a, b int) []int {
	var out []int
	for i := a; i <= b; i++ {
		out = append(out, i)
	}
	return out
}

// call sends a direct request to an agent worker. Only laavud and root can
// connect to the worker socket, so the command grants no new rights.
func call(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: laavud call AGENT OP [JSON]")
	}
	ag, err := config.LoadAgent(filepath.Join(config.Dir, "agents", args[0]+".toml"))
	if err != nil {
		return err
	}
	c, err := wire.Dial(ag.WorkerSocket)
	if err != nil {
		return err
	}
	defer c.Close()
	var payload any
	if len(args) > 2 && strings.TrimSpace(args[2]) != "" {
		payload = json.RawMessage(args[2])
	}
	var out json.RawMessage
	if err := c.Send(args[1], payload, &out); err != nil {
		return err
	}
	if len(out) == 0 {
		out = json.RawMessage(`{"ok":true}`)
	}
	var pretty bytes.Buffer
	json.Indent(&pretty, out, "", "  ")
	fmt.Println(pretty.String())
	return nil
}
