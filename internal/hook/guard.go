package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ilguzin/laavu/internal/worker"
)

// Guard — `laavud hook guard`: «замечает несохранённое» и «следит за контекстом».
//
// PostToolUse запоминает репозитории, которых коснулась сессия. Stop в конце хода
// проверяет их git status и заполненность контекста и, если есть что сказать,
// возвращает decision: block с причиной — агент дописывает пользователю предложение.
type Guard struct {
	StateDir string
	// Every — одно и то же предлагается не чаще раза в столько ходов.
	Every int
	// Threshold — порог заполненности контекста, проценты.
	Threshold int
	Now       func() time.Time
}

type guardState struct {
	Repos []string       `json:"repos"`
	Turns int            `json:"turns"`
	Last  map[string]int `json:"last"`
}

// NewGuard — настройки по умолчанию.
func NewGuard() *Guard {
	home, _ := os.UserHomeDir()
	return &Guard{StateDir: filepath.Join(home, ".local/state/laavu"), Every: 5, Threshold: 65, Now: time.Now}
}

func (g *Guard) path(sid string) string {
	return filepath.Join(g.StateDir, "guard", filepath.Base(sid)+".json")
}

func (g *Guard) load(sid string) *guardState {
	st := &guardState{Last: map[string]int{}}
	if b, err := os.ReadFile(g.path(sid)); err == nil {
		json.Unmarshal(b, st)
	}
	if st.Last == nil {
		st.Last = map[string]int{}
	}
	return st
}

func (g *Guard) save(sid string, st *guardState) {
	os.MkdirAll(filepath.Dir(g.path(sid)), 0o700)
	b, _ := json.Marshal(st)
	os.WriteFile(g.path(sid), b, 0o600)
}

// repoOf — корень git-репозитория, в котором лежит путь, или пусто.
func repoOf(p string) string {
	if p == "" {
		return ""
	}
	dir := p
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		dir = filepath.Dir(p)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// Run обрабатывает один вызов хука и пишет ответ в out.
func (g *Guard) Run(stdin io.Reader, out io.Writer) {
	in, err := ReadInput(stdin)
	if err != nil || in.SessionID == "" {
		return
	}
	switch in.HookEventName {
	case "PostToolUse":
		g.postToolUse(in)
	case "Stop":
		g.stop(in, out)
	case "SessionStart":
		if in.Source == "clear" {
			os.Remove(g.path(in.SessionID))
		}
	}
}

func (g *Guard) postToolUse(in Input) {
	var repo string
	if editTools[in.ToolName] {
		var ti struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		}
		json.Unmarshal(in.ToolInput, &ti)
		p := ti.FilePath
		if p == "" {
			p = ti.NotebookPath
		}
		repo = repoOf(p)
	} else if in.ToolName == "Bash" {
		repo = repoOf(in.Cwd)
	}
	if repo == "" {
		return
	}
	st := g.load(in.SessionID)
	for _, r := range st.Repos {
		if r == repo {
			return
		}
	}
	st.Repos = append(st.Repos, repo)
	sort.Strings(st.Repos)
	g.save(in.SessionID, st)
}

func dirty(repo string) bool {
	cmd := exec.Command("git", "-C", repo, "status", "--porcelain")
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	b, err := cmd.Output()
	return err == nil && len(strings.TrimSpace(string(b))) > 0
}

// contextPercent — заполненность: точная из строки статуса или по транскрипту.
func (g *Guard) contextPercent(in Input) int {
	if b, err := os.ReadFile(worker.StatuslinePath(g.StateDir, in.SessionID)); err == nil {
		var st worker.StatuslineState
		if json.Unmarshal(b, &st) == nil && st.Window > 0 {
			return st.Percent
		}
	}
	used, model, err := worker.LastUsage(in.TranscriptPath)
	if err != nil {
		return 0
	}
	return int(used * 100 / worker.WindowFor(model, used))
}

func (g *Guard) stop(in Input, out io.Writer) {
	// Второй Stop подряд после нашего block — агент уже ответил, отпускаем.
	if in.StopHookActive {
		return
	}
	st := g.load(in.SessionID)
	st.Turns++
	due := func(kind string) bool {
		last, ok := st.Last[kind]
		return !ok || st.Turns-last >= g.Every
	}
	var reasons []string
	if due("unsaved") {
		var dirtyRepos []string
		for _, r := range st.Repos {
			if dirty(r) {
				dirtyRepos = append(dirtyRepos, r)
			}
		}
		if len(dirtyRepos) > 0 {
			st.Last["unsaved"] = st.Turns
			reasons = append(reasons, fmt.Sprintf(
				"В репозиториях, которые менялись в этой сессии, есть незакоммиченные изменения: %s. Коротко предложи пользователю сохранить их (закоммитить и запушить по правилам папки). Без его согласия ничего не делай.",
				strings.Join(dirtyRepos, ", ")))
		}
	}
	if due("context") {
		if p := g.contextPercent(in); p >= g.Threshold {
			st.Last["context"] = st.Turns
			reasons = append(reasons, fmt.Sprintf(
				"Контекст сессии заполнен примерно на %d%%. Коротко предложи пользователю сжать сессию (/compact) или начать заново (/clear) и скажи, что при этом потеряется.", p))
		}
	}
	g.save(in.SessionID, st)
	if len(reasons) == 0 {
		return
	}
	json.NewEncoder(out).Encode(map[string]string{
		"decision": "block",
		"reason":   "Laavu: " + strings.Join(reasons, " "),
	})
}

// Statusline — `laavud hook statusline`: Claude Code передаёт строке статуса точный
// размер окна и процент заполненности. Пишем их в файл для индикатора и guard,
// а в TUI печатаем короткую строку.
func Statusline(stdin io.Reader, out io.Writer, stateDir string) {
	var in struct {
		SessionID string `json:"session_id"`
		Model     struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"model"`
		ContextWindow struct {
			Size         int64    `json:"context_window_size"`
			UsedPercent  *float64 `json:"used_percentage"`
			CurrentUsage *struct {
				Input       int64 `json:"input_tokens"`
				CacheCreate int64 `json:"cache_creation_input_tokens"`
				CacheRead   int64 `json:"cache_read_input_tokens"`
				Output      int64 `json:"output_tokens"`
			} `json:"current_usage"`
		} `json:"context_window"`
	}
	b, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if json.Unmarshal(b, &in) != nil || in.SessionID == "" {
		return
	}
	st := worker.StatuslineState{SessionID: in.SessionID, Window: in.ContextWindow.Size, Model: in.Model.ID}
	if u := in.ContextWindow.CurrentUsage; u != nil {
		st.Used = u.Input + u.CacheCreate + u.CacheRead + u.Output
	}
	if in.ContextWindow.UsedPercent != nil {
		st.Percent = int(*in.ContextWindow.UsedPercent + 0.5)
	} else if st.Window > 0 {
		st.Percent = int(st.Used * 100 / st.Window)
	}
	p := worker.StatuslinePath(stateDir, filepath.Base(in.SessionID))
	os.MkdirAll(filepath.Dir(p), 0o700)
	if data, err := json.Marshal(st); err == nil {
		os.WriteFile(p, data, 0o600)
	}
	name := in.Model.DisplayName
	if name == "" {
		name = in.Model.ID
	}
	fmt.Fprintf(out, "%s · контекст %d%%", name, st.Percent)
}
