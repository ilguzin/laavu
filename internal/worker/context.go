package worker

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ilguzin/laavu/internal/tmuxctl"
)

// ContextUsage is how full the session context is.
type ContextUsage struct {
	Used    int64  `json:"used"`
	Window  int64  `json:"window"`
	Percent int    `json:"percent"`
	Model   string `json:"model,omitempty"`
	// Source tells where the window size came from: statusline (exact) or table (model table).
	Source string `json:"source"`
}

// StatuslineState is what `laavud hook statusline` writes: Claude Code passes it
// the exact window size and percentage.
type StatuslineState struct {
	SessionID string `json:"session_id"`
	Window    int64  `json:"window"`
	Percent   int    `json:"percent"`
	Used      int64  `json:"used"`
	Model     string `json:"model"`
}

// StatuslinePath returns the status line state file for a conversation.
func StatuslinePath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, "ctx", sessionID+".json")
}

// WindowFor returns the window size by model name, when no exact value is known.
func WindowFor(model string, used int64) int64 {
	w := int64(200_000)
	m := strings.ToLower(model)
	if strings.Contains(m, "[1m]") || strings.Contains(m, "-5-") || strings.Contains(m, "-5.") {
		w = 1_000_000
	}
	if used > w {
		w = 1_000_000
	}
	return w
}

// LastUsage reads the counters of the last main-branch reply from the end of the transcript.
func LastUsage(path string) (used int64, model string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	// A tail of a couple of megabytes is enough: model replies are frequent.
	const tail = 2 << 20
	if st.Size() > tail {
		f.Seek(st.Size()-tail, io.SeekStart)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	found := false
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line[:min(len(line), 200)]), `"assistant"`) && !strings.Contains(string(line), `"usage"`) {
			continue
		}
		var rec struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				Model string `json:"model"`
				Usage *struct {
					Input       int64 `json:"input_tokens"`
					CacheCreate int64 `json:"cache_creation_input_tokens"`
					CacheRead   int64 `json:"cache_read_input_tokens"`
					Output      int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.IsSidechain || rec.Message.Usage == nil {
			continue
		}
		u := rec.Message.Usage
		used = u.Input + u.CacheCreate + u.CacheRead + u.Output
		model = rec.Message.Model
		found = true
	}
	if !found {
		return 0, "", errors.New("no model replies in the transcript")
	}
	return used, model, nil
}

func (w *Worker) opContext(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[nameArgs](args)
	if err != nil {
		return nil, err
	}
	live, _, err := w.Tmux.List()
	if err != nil {
		return nil, err
	}
	var s *tmuxctl.Session
	for i := range live {
		if live[i].Name == a.Name {
			s = &live[i]
		}
	}
	if s == nil || s.Claude == "" {
		return nil, errors.New("session has no Claude Code conversation")
	}
	if b, err := os.ReadFile(StatuslinePath(w.StateDir, s.Claude)); err == nil {
		var st StatuslineState
		if json.Unmarshal(b, &st) == nil && st.Window > 0 {
			return ContextUsage{Used: st.Used, Window: st.Window, Percent: st.Percent, Model: st.Model, Source: "statusline"}, nil
		}
	}
	if s.Transcript == "" {
		return nil, errors.New("transcript path is unknown")
	}
	used, model, err := LastUsage(s.Transcript)
	if err != nil {
		return nil, err
	}
	win := WindowFor(model, used)
	return ContextUsage{Used: used, Window: win, Percent: int(used * 100 / win), Model: model, Source: "table"}, nil
}
