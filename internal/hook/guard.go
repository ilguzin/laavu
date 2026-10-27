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

// Guard is `laavud hook guard`: it "notices uncommitted changes" and "watches the context".
//
// PostToolUse remembers the repositories the session touched. Stop at the end of a turn
// checks their git status and how full the context is and, if there is something to say,
// returns decision: block with a reason, so the agent appends a suggestion for the user.
type Guard struct {
	StateDir string
	// Every is the minimum number of turns between two identical suggestions.
	Every int
	// Threshold is the context fill threshold, in percent.
	Threshold int
	Now       func() time.Time
}

type guardState struct {
	Repos []string       `json:"repos"`
	Turns int            `json:"turns"`
	Last  map[string]int `json:"last"`
}

// NewGuard returns the default settings.
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

// repoOf returns the root of the git repository containing the path, or "".
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

// Run handles one hook call and writes the response to out.
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

// contextPercent returns the context fill: exact from the status line, or estimated from the transcript.
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
	// A second Stop right after our block means the agent has already replied; let it go.
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
				"Repositories changed in this session have uncommitted changes: %s. Briefly offer the user to save them (commit and push according to the folder's rules). Do nothing without the user's consent.",
				strings.Join(dirtyRepos, ", ")))
		}
	}
	if due("context") {
		if p := g.contextPercent(in); p >= g.Threshold {
			st.Last["context"] = st.Turns
			reasons = append(reasons, fmt.Sprintf(
				"The session context is about %d%% full. Briefly offer the user to compact the session (/compact) or start over (/clear), and say what would be lost.", p))
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

// Statusline is `laavud hook statusline`: Claude Code passes the status line the exact
// window size and fill percentage. We write them to a file for the indicator and the guard,
// and print a short line in the TUI.
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
	fmt.Fprintf(out, "%s · context %d%%", name, st.Percent)
}
