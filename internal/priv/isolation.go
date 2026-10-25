package priv

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ilguzin/laavu/internal/config"
)

// ProbeResult — одна попытка агента A дотянуться до агента B.
type ProbeResult struct {
	Name string `json:"name"`
	// Blocked — попытка провалилась, как и должна.
	Blocked bool   `json:"blocked"`
	Detail  string `json:"detail,omitempty"`
	Skipped bool   `json:"skipped,omitempty"`
}

// ProbeArgs — что пробовать от имени A.
type ProbeArgs struct {
	TargetHome   string `json:"target_home"`
	TargetUID    int    `json:"target_uid"`
	TargetPort   int    `json:"target_port"`
	TargetTmp    string `json:"target_tmp"`
	TargetWorker string `json:"target_worker"`
	DB           string `json:"db"`
}

// CheckIsolation от имени агента a пытается дотянуться до агента b: прочитать
// его дом, увидеть его процессы, подключиться к его порту, прочитать его файл в
// /tmp, подключиться к его воркеру, прочитать базу демона, получить root.
// Каждая попытка должна провалиться.
func CheckIsolation(a, b config.Agent) ([]ProbeResult, error) {
	// Слушатель в диапазоне B. Правило nftables смотрит на того, кто подключается,
	// поэтому слушать может и root.
	var l net.Listener
	port := 0
	for p := b.Ports[0]; p <= b.Ports[1]; p++ {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			l, port = ln, p
			break
		}
	}
	if l != nil {
		defer l.Close()
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Write([]byte("laavu-probe\n"))
				c.Close()
			}
		}()
	}
	// Файл B в /tmp — через полный стек PAM (как у пользовательского экземпляра
	// systemd агента), чтобы сработал umask из pam_umask. runuser common-session
	// не подключает и дал бы umask 022.
	tmp := fmt.Sprintf("/tmp/laavu-probe-%s-%d", b.Name, time.Now().UnixNano())
	if err := run("sudo", "-u", b.User, "--", "sh", "-c", "echo secret > "+tmp); err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	// Процесс B, который A не должен видеть.
	sleeper := exec.Command("runuser", "-u", b.User, "--", "sleep", "60")
	sleeper.Start()
	defer func() { sleeper.Process.Kill(); sleeper.Wait() }()
	time.Sleep(200 * time.Millisecond)

	args := ProbeArgs{TargetHome: b.Home, TargetUID: b.UID, TargetPort: port, TargetTmp: tmp,
		TargetWorker: b.WorkerSocket, DB: filepath.Join(StateDir, "laavud.db")}
	payload, _ := json.Marshal(args)
	self := LaavudBin()
	// Проба идёт в слайсе A, как его сессии: так проверяются и лимиты слайса.
	cmd := exec.Command("systemd-run", "--quiet", "--pipe", "--wait", "--collect",
		"--uid="+a.User, fmt.Sprintf("--slice=user-%d.slice", a.UID), "-p", "UMask=0077",
		self, "priv", "probe", string(payload))
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("проба не запустилась: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var res []ProbeResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("ответ пробы: %w: %s", err, out)
	}
	if port == 0 {
		res = append(res, ProbeResult{Name: "порт агента", Skipped: true, Detail: "весь диапазон занят"})
	}
	return res, nil
}

// probeMain выполняется от имени агента A.
func probeMain(args []string) error {
	if len(args) != 1 {
		return errors.New("probe: нужен JSON")
	}
	var a ProbeArgs
	if err := json.Unmarshal([]byte(args[0]), &a); err != nil {
		return err
	}
	var res []ProbeResult
	add := func(name string, blocked bool, detail string) {
		res = append(res, ProbeResult{Name: name, Blocked: blocked, Detail: detail})
	}
	// Дом.
	_, err := os.ReadDir(a.TargetHome)
	add("чтение дома", err != nil, errString(err))
	_, err = os.Stat(filepath.Join(a.TargetHome, ".claude", "settings.json"))
	add("файл в доме", err != nil, errString(err))
	// Процессы.
	seen := 0
	if ents, err := os.ReadDir("/proc"); err == nil {
		for _, e := range ents {
			if _, err := strconv.Atoi(e.Name()); err != nil {
				continue
			}
			if st, err := os.Stat("/proc/" + e.Name()); err == nil {
				if uid, ok := statUID(st); ok && uid == a.TargetUID {
					seen++
				}
			}
		}
	}
	add("процессы", seen == 0, fmt.Sprintf("видно процессов: %d", seen))
	// Порт.
	if a.TargetPort > 0 {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(a.TargetPort), 2*time.Second)
		blocked := err != nil
		if c != nil {
			buf := make([]byte, 32)
			c.SetReadDeadline(time.Now().Add(time.Second))
			n, _ := c.Read(buf)
			blocked = !strings.Contains(string(buf[:n]), "laavu-probe")
			c.Close()
		}
		add("порт агента", blocked, errString(err))
	}
	// Файл в /tmp.
	_, err = os.ReadFile(a.TargetTmp)
	add("файл в /tmp", err != nil, errString(err))
	// Воркер другого агента.
	if a.TargetWorker != "" {
		c, err := net.DialTimeout("unix", a.TargetWorker, time.Second)
		if c != nil {
			c.Close()
		}
		add("сокет воркера", err != nil, errString(err))
	}
	// База демона.
	_, err = os.ReadFile(a.DB)
	add("база laavud", err != nil, errString(err))
	// root.
	err = exec.Command("sudo", "-n", "true").Run()
	add("sudo", err != nil, errString(err))
	return json.NewEncoder(os.Stdout).Encode(res)
}

// LaavudBin — путь к laavud: рядом с запущенным бинарником или в /usr/local/bin.
func LaavudBin() string {
	self, _ := os.Executable()
	if filepath.Base(self) == "laavud" {
		return self
	}
	if p := filepath.Join(filepath.Dir(self), "laavud"); fileExists(p) {
		return p
	}
	return "/usr/local/bin/laavud"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func checkIsolationCmd(args []string) error {
	fs := flag.NewFlagSet("check-isolation", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "вывод в JSON")
	fs.Parse(reorder(args))
	agents, err := Agents()
	if err != nil {
		return err
	}
	byName := map[string]config.Agent{}
	for _, a := range agents {
		byName[a.Name] = a
	}
	var pairs [][2]config.Agent
	switch fs.NArg() {
	case 0:
		for _, a := range agents {
			for _, b := range agents {
				if a.Name != b.Name {
					pairs = append(pairs, [2]config.Agent{a, b})
				}
			}
		}
	case 2:
		a, ok1 := byName[fs.Arg(0)]
		b, ok2 := byName[fs.Arg(1)]
		if !ok1 || !ok2 {
			return errors.New("нет такого агента")
		}
		pairs = [][2]config.Agent{{a, b}}
	default:
		return errors.New("использование: check-isolation [A B] [--json]")
	}
	failed := 0
	report := map[string][]ProbeResult{}
	for _, p := range pairs {
		res, err := CheckIsolation(p[0], p[1])
		if err != nil {
			return err
		}
		for _, r := range res {
			if !r.Blocked && !r.Skipped {
				failed++
			}
		}
		report[p[0].Name+"→"+p[1].Name] = res
		if !*asJSON {
			printResults(os.Stdout, p[0].Name, p[1].Name, res)
		}
	}
	saveLastCheck(len(pairs), failed)
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(report)
	}
	Log("check-isolation pairs=%d failed=%d", len(pairs), failed)
	if failed > 0 {
		return fmt.Errorf("изоляция нарушена: %d проб прошли", failed)
	}
	if len(pairs) == 0 && !*asJSON {
		fmt.Println("Агентов меньше двух — проверять нечего.")
	}
	return nil
}

func printResults(w io.Writer, a, b string, res []ProbeResult) {
	fmt.Fprintf(w, "%s → %s\n", a, b)
	for _, r := range res {
		mark := "закрыто"
		if r.Skipped {
			mark = "пропуск"
		} else if !r.Blocked {
			mark = "ОТКРЫТО"
		}
		fmt.Fprintf(w, "  %-16s %s", r.Name, mark)
		if !r.Blocked || r.Skipped {
			fmt.Fprintf(w, "  (%s)", r.Detail)
		}
		fmt.Fprintln(w)
	}
}

// LastCheck — когда проверялась изоляция; показывается в клиенте.
type LastCheck struct {
	Time   int64 `json:"time"`
	Pairs  int   `json:"pairs"`
	Failed int   `json:"failed"`
}

// LastCheckFile — файл с результатом последней проверки.
func LastCheckFile() string { return filepath.Join(StateDir, "isolation-check.json") }

func saveLastCheck(pairs, failed int) {
	b, _ := json.Marshal(LastCheck{Time: time.Now().Unix(), Pairs: pairs, Failed: failed})
	p := LastCheckFile()
	if os.WriteFile(p, b, 0o644) == nil {
		if st, err := os.Stat(StateDir); err == nil {
			if uid, ok := statUID(st); ok {
				os.Chown(p, uid, -1)
			}
		}
	}
}
