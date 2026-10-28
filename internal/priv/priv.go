// Package priv performs privileged root operations: create and delete an agent
// with all isolation measures, check isolation. Every operation is logged.
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

// Paths on the server. They are variables so tests can swap the root.
var (
	ConfDir    = config.Dir
	AgentsHome = "/srv/laavu/agents"
	LogFile    = "/var/log/laavu/priv.log"
	SystemdDir = "/etc/systemd/system"
	StateDir   = "/var/lib/laavu"
	// FirstPort and PortBlock define agent port ranges: 20000-20999, 21000-21999...
	FirstPort = 20000
	PortBlock = 1000
	MaxPort   = 59999
)

// Main parses `laavud priv ...` and `laavu agent ...`.
func Main(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: laavud priv create-agent|delete-agent|list|limits|check-isolation|probe")
	}
	op, rest := args[0], args[1:]
	if op == "probe" {
		// The probe runs as the agent and does not need root.
		return probeMain(rest)
	}
	if os.Geteuid() != 0 {
		return errors.New("root required")
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
	return fmt.Errorf("unknown operation %q", op)
}

// Log writes a line to the operations log.
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

// run runs a command and returns an error that includes its output.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

func agentsDir() string { return filepath.Join(ConfDir, "agents") }

// Agents returns all agents on the server.
func Agents() ([]config.Agent, error) { return config.LoadAgents(agentsDir()) }

// CreateOptions are the parameters of a new agent.
type CreateOptions struct {
	Name          string
	Memory        string // slice MemoryMax, e.g. 2G
	Tasks         int    // TasksMax
	CPUWeight     int
	InstallClaude bool
	Laavud        string // path to laavud for hooks
	Out           io.Writer
}

func createAgentCmd(args []string) error {
	fs := flag.NewFlagSet("create-agent", flag.ExitOnError)
	mem := fs.String("memory", "", "agent MemoryMax (default: three quarters of the server memory)")
	tasks := fs.Int("tasks", 1024, "agent TasksMax")
	cpu := fs.Int("cpu-weight", 100, "agent CPUWeight")
	claude := fs.Bool("install-claude", false, "install Claude Code for the agent (needs internet)")
	fs.Parse(reorder(args))
	if fs.NArg() != 1 {
		return errors.New("usage: create-agent NAME")
	}
	// The agent rules get the installed path, not the one we were run from.
	laavud := "/usr/local/bin/laavud"
	if !fileExists(laavud) {
		laavud = LaavudBin()
	}
	_, err := CreateAgent(CreateOptions{Name: fs.Arg(0), Memory: *mem, Tasks: *tasks, CPUWeight: *cpu, InstallClaude: *claude, Laavud: laavud, Out: os.Stdout})
	return err
}

// reorder moves flags in front of positional arguments: `create-agent demo --memory 1G`.
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

// nextPorts returns the first free port block.
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

// CreateAgent sets up an agent with all isolation measures.
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
		return nil, fmt.Errorf("user %s already exists", username)
	}
	agents, err := Agents()
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		if a.Name == o.Name {
			return nil, fmt.Errorf("agent %s already exists", o.Name)
		}
	}
	ports := nextPorts(agents)
	if ports[0] == 0 {
		return nil, errors.New("out of port ranges")
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
	fmt.Fprintf(out, "Agent %s: user %s, home %s, ports %d-%d\n", o.Name, username, home, ports[0], ports[1])

	// User: home 0700, umask 077 via pam_umask (GECOS field), agents group
	// (hook socket and DenyGroups in sshd), password locked.
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

	// Directories and the rules template: from here on they belong to the agent.
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

	// Resources: the agent user's slice.
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
	// The agent's tmux lives in its user instance of systemd: linger is required.
	if err := run("loginctl", "enable-linger", username); err != nil {
		return nil, err
	}
	run("systemctl", "daemon-reload")
	waitUserManager(uid)
	if err := run("systemctl", "enable", "--now", "laavud-worker@"+username+".socket", "laavud-worker@"+username+".service"); err != nil {
		return nil, err
	}
	if o.InstallClaude {
		fmt.Fprintln(out, "Installing Claude Code for the agent...")
		if err := InstallClaude(username); err != nil {
			fmt.Fprintln(out, "Claude Code install failed:", err)
		}
	}
	// The daemon rereads the agent list on start.
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
	// A template edited by the owner in /etc/laavu/agent-template takes precedence over the embedded one.
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

// InstallClaude installs Claude Code into the agent's ~/.local/bin with the official installer.
func InstallClaude(username string) error {
	return run("runuser", "-l", username, "-c", "curl -fsSL https://claude.ai/install.sh | bash")
}

func deleteAgentCmd(args []string) error {
	fs := flag.NewFlagSet("delete-agent", flag.ExitOnError)
	keep := fs.Bool("keep-home", false, "keep the agent home in /srv/laavu/deleted")
	fs.Parse(reorder(args))
	if fs.NArg() != 1 {
		return errors.New("usage: delete-agent NAME [--keep-home]")
	}
	return DeleteAgent(fs.Arg(0), *keep, os.Stdout)
}

// DeleteAgent removes an agent: sessions, worker, user, rules.
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
		fmt.Fprintln(out, "Agent home kept in", dst)
	}
	if err := run("userdel", "--remove", ag.User); err != nil && !keepHome {
		fmt.Fprintln(out, err)
	}
	if !keepHome {
		os.RemoveAll(ag.Home)
	}
	os.RemoveAll(filepath.Join(SystemdDir, fmt.Sprintf("user-%d.slice.d", ag.UID)))
	os.Remove(filepath.Join(agentsDir(), name+".toml"))
	// Users' access goes too: otherwise a new agent with this name would inherit it.
	if err := run("runuser", "-u", "laavud", "--", LaavudBin(), "access", "drop", "--config-dir", ConfDir, name); err != nil {
		fmt.Fprintln(out, "Access to the agent was not removed, run: laavud access drop", name, "-", err)
	}
	run("systemctl", "daemon-reload")
	if err := SyncNftables(); err != nil {
		return err
	}
	run("systemctl", "try-restart", "laavud.service")
	fmt.Fprintf(out, "Agent %s deleted\n", name)
	return nil
}

func listAgents(out io.Writer) error {
	agents, err := Agents()
	if err != nil {
		return err
	}
	for _, a := range agents {
		fmt.Fprintf(out, "%-16s %-20s uid=%-6d ports %s  %s\n", a.Name, a.User, a.UID, a.PortsEnv(), a.Home)
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
		return errors.New("usage: limits NAME [--memory 2G] [--tasks 1024] [--cpu-weight 100]")
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
	// Save it in a drop-in so it survives a reboot.
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
