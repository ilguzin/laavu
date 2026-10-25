// laavud — демон Laavu, воркер агента, хуки и привилегированные операции.
package main

import (
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
)

// Version подставляется при сборке.
var Version = "dev"

const usage = `laavud — демон Laavu.

  laavud serve                  демон (systemd-юнит laavud.service)
  laavud worker                 воркер агента (laavud-worker@<пользователь>.service)
  laavud hook claude|guard|statusline
                                хуки Claude Code из настроек агента
  laavud hook tmux <событие>    хуки tmux из /etc/laavu/tmux.conf
  laavud reset-auth             стереть все passkey и напечатать ссылку регистрации администратора
  laavud invite [--admin] [--name ИМЯ]
                                ссылка регистрации нового пользователя
  laavud admin add ИМЯ          сделать пользователя администратором
  laavud priv ...               операции от root: create-agent, delete-agent, check-isolation
  laavud call АГЕНТ ОПЕРАЦИЯ [JSON]
                                запрос к воркеру агента (диагностика; от laavud или root)
  laavud dev --home DIR         демон и воркер в одном процессе для разработки
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
	confDir := fs.String("config-dir", config.Dir, "каталог конфигурации")
	fs.Parse(args)
	cfg, err := config.LoadDaemon(filepath.Join(*confDir, "laavud.toml"))
	if err != nil {
		return err
	}
	if cfg.NoAuth {
		return errors.New("no_auth допустим только в laavud dev на localhost")
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
	log.Info("laavud запущен", "version", Version, "agents", len(agents), "url", cfg.PublicURL)
	return d.Serve(ctx, app, []net.Listener{aux}, hooks)
}

func runWorker(args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	confDir := fs.String("config-dir", config.Dir, "каталог конфигурации")
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
		return fmt.Errorf("пользователь %s не агент Laavu", me.Username)
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
	// Хук никогда не валит агента: любые ошибки молча, код выхода 0.
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

// asLaavud перезапускает команду от пользователя laavud, если её вызвал root:
// база принадлежит laavud, и файлы журнала SQLite от root сломали бы демон.
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
	confDir := fs.String("config-dir", config.Dir, "каталог конфигурации")
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
	fmt.Println("Все passkey и входы удалены. Ссылка регистрации администратора (действует сутки):")
	fmt.Println(strings.TrimRight(cfg.PublicURL, "/") + "/register#" + tok)
	return nil
}

func invite(args []string) error {
	if done, err := asLaavud(); done {
		return err
	}
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	confDir := fs.String("config-dir", config.Dir, "каталог конфигурации")
	isAdmin := fs.Bool("admin", false, "роль администратора")
	name := fs.String("name", "", "имя пользователя (по умолчанию его выбирает он сам)")
	quiet := fs.Bool("q", false, "печатать только ссылку")
	ifNoAdmin := fs.Bool("if-no-admin", false, "ничего не делать, если администратор уже есть")
	fs.Parse(args)
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
	}
	tok, err := st.CreateInvite(store.Invite{Role: role, Name: *name}, auth.InviteTTL)
	if err != nil {
		return err
	}
	link := strings.TrimRight(cfg.PublicURL, "/") + "/register#" + tok
	if !*quiet {
		fmt.Println("Ссылка регистрации (одноразовая, действует сутки):")
	}
	fmt.Println(link)
	return nil
}

func admin(args []string) error {
	if done, err := asLaavud(); done {
		return err
	}
	if len(args) != 2 || args[0] != "add" {
		return errors.New("использование: laavud admin add ИМЯ")
	}
	st, _, err := openStore(config.Dir)
	if err != nil {
		return err
	}
	defer st.Close()
	u, err := st.UserByName(args[1])
	if err != nil {
		return fmt.Errorf("пользователь %s: %w", args[1], err)
	}
	return st.SetRole(u.ID, "admin")
}

// dev — демон и воркер в одном процессе от текущего пользователя, без Caddy и
// без входа. Только для разработки на localhost.
func dev(args []string) error {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	home := fs.String("home", "", "дом агента (обязательно, не ваш настоящий дом)")
	listen := fs.Int("port", 7700, "порт приложения; следующий — origin просмотра, +10…+19 — dev-серверы")
	agentName := fs.String("agent", "dev", "имя агента")
	claude := fs.String("claude", "claude", "команда агента")
	withAuth := fs.Bool("auth", false, "включить вход по passkey")
	fs.Parse(args)
	if *home == "" {
		return errors.New("нужен --home")
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
		fmt.Println("Ссылка регистрации:", cfg.PublicURL+"/register#"+tok)
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

// call — прямой запрос к воркеру агента. Подключиться к сокету воркера могут
// только laavud и root, так что новых прав команда не даёт.
func call(args []string) error {
	if len(args) < 2 {
		return errors.New("использование: laavud call АГЕНТ ОПЕРАЦИЯ [JSON]")
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
