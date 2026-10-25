// Package priv — привилегированные операции от root: создать и удалить агента
// со всеми мерами изоляции, проверить изоляцию. Каждая операция пишется в журнал.
package priv

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ilguzin/laavu/deploy"
	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/names"
)

// Пути на сервере. Переменные — чтобы тесты могли подменить корень.
var (
	ConfDir    = config.Dir
	AgentsHome = "/srv/laavu/agents"
	LogFile    = "/var/log/laavu/priv.log"
	SystemdDir = "/etc/systemd/system"
	StateDir   = "/var/lib/laavu"
	// FirstPort и PortBlock — диапазоны портов агентов: 20000–20999, 21000–21999…
	FirstPort = 20000
	PortBlock = 1000
	MaxPort   = 59999
)

// Main разбирает `laavud priv ...` и `laavu agent ...`.
func Main(args []string) error {
	if len(args) == 0 {
		return errors.New("использование: laavud priv create-agent|delete-agent|list|limits|check-isolation|probe")
	}
	op, rest := args[0], args[1:]
	if op == "probe" {
		// Проба запускается от имени агента, root ей не нужен.
		return probeMain(rest)
	}
	if os.Geteuid() != 0 {
		return errors.New("нужен root")
	}
	switch op {
	case "create-agent":
		return createAgentCmd(rest)
	case "delete-agent":
		return deleteAgentCmd(rest)
	case "list":
		return listAgents(os.Stdout)
	case "limits":
		return limitsCmd(rest)
	case "check-isolation":
		return checkIsolationCmd(rest)
	case "nft-sync":
		return SyncNftables()
	}
	return fmt.Errorf("неизвестная операция %q", op)
}

// Log пишет строку в журнал операций.
func Log(format string, a ...any) {
	os.MkdirAll(filepath.Dir(LogFile), 0o750)
	f, err := os.OpenFile(LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return
	}
	defer f.Close()
	who := os.Getenv("SUDO_USER")
	if who == "" {
		who = "root"
	}
	fmt.Fprintf(f, "%s %s %s\n", time.Now().Format(time.RFC3339), who, fmt.Sprintf(format, a...))
}

// run выполняет команду и возвращает ошибку с её выводом.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

func agentsDir() string { return filepath.Join(ConfDir, "agents") }

// Agents — все агенты сервера.
func Agents() ([]config.Agent, error) { return config.LoadAgents(agentsDir()) }

// CreateOptions — параметры нового агента.
type CreateOptions struct {
	Name          string
	Memory        string // MemoryMax слайса, например 2G
	Tasks         int    // TasksMax
	CPUWeight     int
	InstallClaude bool
	Laavud        string // путь к laavud для хуков
	Out           io.Writer
}

func createAgentCmd(args []string) error {
	fs := flag.NewFlagSet("create-agent", flag.ExitOnError)
	mem := fs.String("memory", "", "MemoryMax агента (по умолчанию половина памяти сервера)")
	tasks := fs.Int("tasks", 1024, "TasksMax агента")
	cpu := fs.Int("cpu-weight", 100, "CPUWeight агента")
	claude := fs.Bool("install-claude", false, "поставить Claude Code агенту (нужен интернет)")
	fs.Parse(reorder(args))
	if fs.NArg() != 1 {
		return errors.New("использование: create-agent ИМЯ")
	}
	// В правила агента идёт установленный путь, а не тот, откуда запустили.
	laavud := "/usr/local/bin/laavud"
	if !fileExists(laavud) {
		laavud = LaavudBin()
	}
	_, err := CreateAgent(CreateOptions{Name: fs.Arg(0), Memory: *mem, Tasks: *tasks, CPUWeight: *cpu, InstallClaude: *claude, Laavud: laavud, Out: os.Stdout})
	return err
}

// reorder переносит флаги вперёд позиционных аргументов: `create-agent demo --memory 1G`.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !isBoolFlag(a) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func isBoolFlag(a string) bool {
	switch strings.TrimLeft(a, "-") {
	case "install-claude", "keep-home", "json", "no-claude", "yes", "y":
		return true
	}
	return false
}

// nextPorts — первый свободный блок портов.
func nextPorts(agents []config.Agent) [2]int {
	used := map[int]bool{}
	for _, a := range agents {
		used[a.Ports[0]] = true
	}
	for p := FirstPort; p+PortBlock-1 <= MaxPort; p += PortBlock {
		if !used[p] {
			return [2]int{p, p + PortBlock - 1}
		}
	}
	return [2]int{}
}

func defaultMemory() string {
	b, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
				kb, _ := strconv.Atoi(f[1])
				if kb > 0 {
					return fmt.Sprintf("%dM", kb/1024*3/4)
				}
			}
		}
	}
	return "1G"
}

// CreateAgent заводит агента со всеми мерами изоляции.
func CreateAgent(o CreateOptions) (*config.Agent, error) {
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	if err := names.Agent(o.Name); err != nil {
		return nil, err
	}
	username := "lv-" + o.Name
	if _, err := user.Lookup(username); err == nil {
		return nil, fmt.Errorf("пользователь %s уже есть", username)
	}
	agents, err := Agents()
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		if a.Name == o.Name {
			return nil, fmt.Errorf("агент %s уже есть", o.Name)
		}
	}
	ports := nextPorts(agents)
	if ports[0] == 0 {
		return nil, errors.New("кончились диапазоны портов")
	}
	if o.Memory == "" {
		o.Memory = defaultMemory()
	}
	if o.Tasks == 0 {
		o.Tasks = 1024
	}
	if o.CPUWeight == 0 {
		o.CPUWeight = 100
	}
	home := filepath.Join(AgentsHome, o.Name)
	fmt.Fprintf(out, "Агент %s: пользователь %s, дом %s, порты %d-%d\n", o.Name, username, home, ports[0], ports[1])

	// Пользователь: дом 0700, umask 077 через pam_umask (поле GECOS), группа агентов
	// (сокет хуков и DenyGroups в sshd), пароль заблокирован.
	os.MkdirAll(AgentsHome, 0o755)
	if err := run("useradd", "--create-home", "--home-dir", home, "--shell", "/bin/bash",
		"--groups", "laavu-agents", "--comment", "umask=077", "--key", "UMASK=077", username); err != nil {
		return nil, err
	}
	Log("create-agent %s ports=%d-%d", o.Name, ports[0], ports[1])
	u, err := user.Lookup(username)
	if err != nil {
		return nil, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	os.Chmod(home, 0o700)
	run("passwd", "--lock", username)

	ag := config.Agent{Name: o.Name, User: username, Home: home, Ports: ports, UID: uid}
	ag.Fill()

	// Каталоги и шаблон правил: дальше они принадлежат агенту.
	for _, d := range []string{"tmp", ".claude", ".local/state/laavu", ".ssh"} {
		p := filepath.Join(home, d)
		os.MkdirAll(p, 0o700)
	}
	data := map[string]string{"Name": o.Name, "User": username, "Ports": ag.PortsEnv(), "Laavud": o.Laavud}
	for _, f := range []struct{ tmpl, dst string }{
		{"agent/CLAUDE.md.tmpl", ".claude/CLAUDE.md"},
		{"agent/settings.json.tmpl", ".claude/settings.json"},
	} {
		b, err := renderTemplate(f.tmpl, data)
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(home, f.dst)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return nil, err
		}
	}
	if err := chownR(home, uid, gid); err != nil {
		return nil, err
	}

	// Ресурсы: слайс пользователя агента.
	slice := filepath.Join(SystemdDir, fmt.Sprintf("user-%d.slice.d", uid))
	os.MkdirAll(slice, 0o755)
	limits := fmt.Sprintf("[Slice]\nMemoryMax=%s\nTasksMax=%d\nCPUWeight=%d\n", o.Memory, o.Tasks, o.CPUWeight)
	if err := os.WriteFile(filepath.Join(slice, "laavu.conf"), []byte(limits), 0o644); err != nil {
		return nil, err
	}

	if err := config.WriteAgent(agentsDir(), ag); err != nil {
		return nil, err
	}
	if err := SyncNftables(); err != nil {
		return nil, err
	}
	// tmux агента живёт в его пользовательском экземпляре systemd: нужен linger.
	if err := run("loginctl", "enable-linger", username); err != nil {
		return nil, err
	}
	run("systemctl", "daemon-reload")
	waitUserManager(uid)
	if err := run("systemctl", "enable", "--now", "laavud-worker@"+username+".socket", "laavud-worker@"+username+".service"); err != nil {
		return nil, err
	}
	if o.InstallClaude {
		fmt.Fprintln(out, "Ставлю Claude Code агенту…")
		if err := InstallClaude(username); err != nil {
			fmt.Fprintln(out, "Claude Code не поставился:", err)
		}
	}
	// Демон перечитывает список агентов при старте.
	run("systemctl", "try-restart", "laavud.service")

	others := []config.Agent{}
	for _, a := range agents {
		if a.Name != o.Name {
			others = append(others, a)
		}
	}
	for _, b := range others {
		for _, pair := range [][2]config.Agent{{ag, b}, {b, ag}} {
			res, err := CheckIsolation(pair[0], pair[1])
			if err != nil {
				fmt.Fprintf(out, "check-isolation %s → %s: %v\n", pair[0].Name, pair[1].Name, err)
				continue
			}
			printResults(out, pair[0].Name, pair[1].Name, res)
		}
	}
	return &ag, nil
}

func renderTemplate(name string, data any) ([]byte, error) {
	// Шаблон, поправленный владельцем в /etc/laavu/agent-template, важнее вшитого.
	local := filepath.Join(ConfDir, "agent-template", filepath.Base(name))
	if b, err := os.ReadFile(local); err == nil {
		return deploy.RenderBytes(name, b, data)
	}
	return deploy.Render(name, data)
}

func chownR(root string, uid, gid int) error {
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

func waitUserManager(uid int) {
	bus := fmt.Sprintf("/run/user/%d/bus", uid)
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(bus); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// InstallClaude ставит Claude Code в ~/.local/bin агента официальным установщиком.
func InstallClaude(username string) error {
	return run("runuser", "-l", username, "-c", "curl -fsSL https://claude.ai/install.sh | bash")
}

func deleteAgentCmd(args []string) error {
	fs := flag.NewFlagSet("delete-agent", flag.ExitOnError)
	keep := fs.Bool("keep-home", false, "сохранить дом агента в /srv/laavu/deleted")
	fs.Parse(reorder(args))
	if fs.NArg() != 1 {
		return errors.New("использование: delete-agent ИМЯ [--keep-home]")
	}
	return DeleteAgent(fs.Arg(0), *keep, os.Stdout)
}

// DeleteAgent удаляет агента: сессии, воркер, пользователя, правила.
func DeleteAgent(name string, keepHome bool, out io.Writer) error {
	ag, err := config.LoadAgent(filepath.Join(agentsDir(), name+".toml"))
	if err != nil {
		return err
	}
	Log("delete-agent %s keep-home=%v", name, keepHome)
	run("systemctl", "disable", "--now", "laavud-worker@"+ag.User+".service", "laavud-worker@"+ag.User+".socket")
	run("loginctl", "disable-linger", ag.User)
	run("loginctl", "terminate-user", ag.User)
	run("pkill", "-KILL", "-u", ag.User)
	time.Sleep(500 * time.Millisecond)
	if keepHome {
		dst := filepath.Join(filepath.Dir(AgentsHome), "deleted", fmt.Sprintf("%s-%s", name, time.Now().Format("20060102-150405")))
		os.MkdirAll(filepath.Dir(dst), 0o700)
		if err := os.Rename(ag.Home, dst); err != nil {
			return err
		}
		fmt.Fprintln(out, "Дом агента сохранён в", dst)
	}
	if err := run("userdel", "--remove", ag.User); err != nil && !keepHome {
		fmt.Fprintln(out, err)
	}
	if !keepHome {
		os.RemoveAll(ag.Home)
	}
	os.RemoveAll(filepath.Join(SystemdDir, fmt.Sprintf("user-%d.slice.d", ag.UID)))
	os.Remove(filepath.Join(agentsDir(), name+".toml"))
	run("systemctl", "daemon-reload")
	if err := SyncNftables(); err != nil {
		return err
	}
	run("systemctl", "try-restart", "laavud.service")
	fmt.Fprintf(out, "Агент %s удалён\n", name)
	return nil
}

func listAgents(out io.Writer) error {
	agents, err := Agents()
	if err != nil {
		return err
	}
	for _, a := range agents {
		fmt.Fprintf(out, "%-16s %-20s uid=%-6d порты %s  %s\n", a.Name, a.User, a.UID, a.PortsEnv(), a.Home)
	}
	return nil
}

func limitsCmd(args []string) error {
	fs := flag.NewFlagSet("limits", flag.ExitOnError)
	mem := fs.String("memory", "", "MemoryMax")
	tasks := fs.Int("tasks", 0, "TasksMax")
	cpu := fs.Int("cpu-weight", 0, "CPUWeight")
	fs.Parse(reorder(args))
	if fs.NArg() != 1 {
		return errors.New("использование: limits ИМЯ [--memory 2G] [--tasks 1024] [--cpu-weight 100]")
	}
	ag, err := config.LoadAgent(filepath.Join(agentsDir(), fs.Arg(0)+".toml"))
	if err != nil {
		return err
	}
	var props []string
	if *mem != "" {
		props = append(props, "MemoryMax="+*mem)
	}
	if *tasks > 0 {
		props = append(props, "TasksMax="+strconv.Itoa(*tasks))
	}
	if *cpu > 0 {
		props = append(props, "CPUWeight="+strconv.Itoa(*cpu))
	}
	slice := fmt.Sprintf("user-%d.slice", ag.UID)
	if len(props) == 0 {
		out, _ := exec.Command("systemctl", "show", slice, "-p", "MemoryMax,TasksMax,CPUWeight").Output()
		fmt.Print(string(out))
		return nil
	}
	Log("limits %s %s", ag.Name, strings.Join(props, " "))
	// Сохраняем в drop-in, чтобы пережило перезагрузку.
	path := filepath.Join(SystemdDir, slice+".d", "laavu.conf")
	cur, _ := os.ReadFile(path)
	lines := map[string]string{}
	for _, l := range strings.Split(string(cur), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			lines[k] = v
		}
	}
	for _, p := range props {
		k, v, _ := strings.Cut(p, "=")
		lines[k] = v
	}
	keys := make([]string, 0, len(lines))
	for k := range lines {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("[Slice]\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, lines[k])
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		return err
	}
	run("systemctl", "daemon-reload")
	return run("systemctl", append([]string{"set-property", "--runtime", slice}, props...)...)
}
