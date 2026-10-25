package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ilguzin/laavu/internal/names"
	"github.com/ilguzin/laavu/internal/tmuxctl"
)

// Workdir — рабочая папка агента.
type Workdir struct {
	Name  string `json:"name"`
	MTime int64  `json:"mtime"`
	Git   bool   `json:"git"`
	// Branch и Changes — ветка и число незакоммиченных изменений (git status).
	Branch  string `json:"branch,omitempty"`
	Changes int    `json:"changes,omitempty"`
}

func (w *Worker) opWorkdirs(_ *Conn, _ json.RawMessage) (any, error) {
	ents, err := os.ReadDir(w.Home)
	if err != nil {
		return nil, err
	}
	out := []Workdir{}
	for _, e := range ents {
		if !e.IsDir() || w.Reserved.Workdir(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		wd := Workdir{Name: e.Name(), MTime: info.ModTime().Unix()}
		if _, err := os.Stat(filepath.Join(w.Home, e.Name(), ".git")); err == nil {
			wd.Git = true
			wd.Branch, wd.Changes = w.gitStatus(filepath.Join(w.Home, e.Name()))
		}
		out = append(out, wd)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// gitStatus — ветка и число строк git status. Первая строка `## ветка...апстрим`;
// в новом репозитории — `## No commits yet on ветка`.
func (w *Worker) gitStatus(dir string) (branch string, changes int) {
	out, err := w.git(dir, "status", "--porcelain", "--branch")
	if err != nil {
		return "", 0
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if b, ok := strings.CutPrefix(line, "## "); ok {
			b = strings.TrimPrefix(b, "No commits yet on ")
			b, _, _ = strings.Cut(b, "...")
			b, _, _ = strings.Cut(b, " ")
			if b != "HEAD" {
				branch = b
			}
		} else if line != "" {
			changes++
		}
	}
	return branch, changes
}

func (w *Worker) opMkdir(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[struct{ Name string }](args)
	if err != nil {
		return nil, err
	}
	if err := w.Reserved.Workdir(a.Name); err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(w.Home, a.Name), 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errors.New("папка уже есть")
		}
		return nil, err
	}
	return Workdir{Name: a.Name, MTime: time.Now().Unix()}, nil
}

// SessionInfo — сессия так, как её видит клиент.
type SessionInfo struct {
	Name     string `json:"name"`
	Folder   string `json:"folder"` // рабочая папка; пусто — «прочее»
	Path     string `json:"path"`
	Kind     string `json:"kind"` // claude | shell | other
	Author   string `json:"author,omitempty"`
	Claude   string `json:"claude,omitempty"`
	Status   string `json:"status,omitempty"`
	Sleeping bool   `json:"sleeping,omitempty"`
	Dead     bool   `json:"dead,omitempty"`
	Attached int    `json:"attached"`
	Activity int64  `json:"activity,omitempty"`
	Created  int64  `json:"created,omitempty"`
	Command  string `json:"command,omitempty"`
	// Action — последнее действие агента; заполняет демон по хукам.
	Action *Action `json:"action,omitempty"`
}

// Action — инструмент и путь из последнего PreToolUse.
type Action struct {
	Tool   string `json:"tool"`
	Target string `json:"target,omitempty"`
	At     int64  `json:"at"`
}

// folderOf — рабочая папка, в которой лежит путь, или пусто.
func (w *Worker) folderOf(p string) string {
	rel, err := filepath.Rel(w.Home, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	first := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	if w.Reserved.Workdir(first) != nil {
		return ""
	}
	return first
}

func kindOf(k string) string {
	if k == "claude" || k == "shell" {
		return k
	}
	return "other"
}

// Sessions — живые сессии tmux и спящие из снимка.
func (w *Worker) Sessions() ([]SessionInfo, error) {
	if err := w.reconcile(); err != nil {
		return nil, err
	}
	live, _, err := w.Tmux.List()
	if err != nil {
		return nil, err
	}
	snap, err := w.loadSnapshot()
	if err != nil {
		return nil, err
	}
	out := []SessionInfo{}
	seen := map[string]bool{}
	for _, s := range live {
		seen[s.Name] = true
		info := SessionInfo{
			Name: s.Name, Folder: w.folderOf(s.Path), Path: s.Path, Kind: kindOf(s.Kind),
			Author: s.Author, Claude: s.Claude, Status: s.Status, Dead: s.PaneDead,
			Attached: s.Attached, Activity: s.Activity, Created: s.Created, Command: s.CurrentCmd,
		}
		if s.PaneDead {
			info.Status = "exited"
		}
		out = append(out, info)
	}
	for _, e := range snap.Sessions {
		if seen[e.Name] {
			continue
		}
		out = append(out, SessionInfo{
			Name: e.Name, Folder: w.folderOf(e.Path), Path: e.Path, Kind: kindOf(e.Kind),
			Author: e.Author, Claude: e.Claude, Status: "sleeping", Sleeping: true, Command: e.StartCmd,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (w *Worker) opSessions(_ *Conn, _ json.RawMessage) (any, error) { return w.Sessions() }

// Profile — профиль автора сессии: стиль работы агента.
type Profile struct {
	AppendPrompt string `json:"append_prompt,omitempty"`
	Settings     string `json:"settings,omitempty"` // путь к файлу для --settings
}

func (p Profile) args() []string {
	var a []string
	if p.AppendPrompt != "" {
		a = append(a, "--append-system-prompt", p.AppendPrompt)
	}
	if p.Settings != "" {
		a = append(a, "--settings", p.Settings)
	}
	return a
}

// NewArgs — новая сессия.
type NewArgs struct {
	Name    string  `json:"name"`
	Folder  string  `json:"folder"`
	Kind    string  `json:"kind"` // claude | shell
	Author  string  `json:"author"`
	Profile Profile `json:"profile"`
	Cols    int     `json:"cols"`
	Rows    int     `json:"rows"`
}

func (w *Worker) claudeCmd(resume string, p Profile) []string {
	cmd := strings.Fields(w.Agent.Claude)
	if len(cmd) == 0 {
		cmd = []string{"claude"}
	}
	if resume != "" {
		cmd = append(cmd, "--resume", resume)
	}
	return w.viaShell(append(cmd, p.args()...))
}

// viaShell запускает команду через интерактивный login-shell агента, как если бы
// он набрал её в терминале: подхватываются ~/.profile и ~/.bashrc (PATH,
// переменные вроде CLAUDE_CODE_OAUTH_TOKEN). exec — чтобы в панели остался сам
// процесс команды, а не shell.
func (w *Worker) viaShell(cmd []string) []string {
	return append([]string{w.Shell, "-lic", `exec "$@"`, "laavu"}, cmd...)
}

func (w *Worker) shellCmd() []string { return []string{w.Shell, "-l"} }

func (w *Worker) sessionEnv() map[string]string {
	return map[string]string{
		"TMPDIR":      filepath.Join(w.Home, "tmp"),
		"LAAVU_PORTS": w.Agent.PortsEnv(),
		"LAAVU_AGENT": w.Agent.Name,
	}
}

func envList(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func (w *Worker) freeName(folder string) string {
	live, _, _ := w.Tmux.List()
	snap, _ := w.loadSnapshot()
	used := map[string]bool{}
	for _, s := range live {
		used[s.Name] = true
	}
	for _, s := range snap.Sessions {
		used[s.Name] = true
	}
	base := strings.NewReplacer(".", "-").Replace(folder)
	for i := 1; ; i++ {
		n := fmt.Sprintf("%s-%d", base, i)
		if !used[n] {
			return n
		}
	}
}

func (w *Worker) opNew(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[NewArgs](args)
	if err != nil {
		return nil, err
	}
	return w.NewSession(a)
}

// NewSession заводит сессию в рабочей папке.
func (w *Worker) NewSession(a NewArgs) (*SessionInfo, error) {
	if err := w.Reserved.Workdir(a.Folder); err != nil {
		return nil, fmt.Errorf("папка %q: %w", a.Folder, err)
	}
	dir := filepath.Join(w.Home, a.Folder)
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("папки %q нет", a.Folder)
	}
	if a.Name == "" {
		a.Name = w.freeName(a.Folder)
	}
	if err := names.Session(a.Name); err != nil {
		return nil, err
	}
	if w.Tmux.Has(a.Name) {
		return nil, errors.New("сессия с таким именем уже есть")
	}
	if snap, _ := w.loadSnapshot(); snap.find(a.Name) >= 0 {
		return nil, errors.New("сессия с таким именем спит, откройте её или закройте")
	}
	var cmd []string
	switch a.Kind {
	case "claude":
		cmd = w.claudeCmd("", a.Profile)
	case "shell", "":
		a.Kind = "shell"
		cmd = w.shellCmd()
	default:
		return nil, fmt.Errorf("неизвестный вид сессии %q", a.Kind)
	}
	opts := map[string]string{tmuxctl.OptKind: a.Kind, tmuxctl.OptAuthor: a.Author}
	if a.Kind == "claude" {
		opts[tmuxctl.OptStatus] = "starting"
	}
	err := w.Tmux.New(tmuxctl.NewOpts{
		Name: a.Name, Dir: dir, Command: cmd, Env: envList(w.sessionEnv()),
		Options: opts, Width: a.Cols, Height: a.Rows,
	})
	if err != nil {
		return nil, err
	}
	w.Tmux.SetGlobalEnv(w.sessionEnv())
	if err := w.reconcile(); err != nil {
		w.log.Warn("сверка снимка", "err", err)
	}
	return &SessionInfo{Name: a.Name, Folder: a.Folder, Path: dir, Kind: a.Kind, Author: a.Author, Status: opts[tmuxctl.OptStatus]}, nil
}

// EnsureArgs — поднять спящую или умершую сессию.
type EnsureArgs struct {
	Name    string  `json:"name"`
	Profile Profile `json:"profile"`
	// Respawn — перезапустить и умершую панель (кнопка «продолжить»).
	Respawn bool `json:"respawn"`
	Cols    int  `json:"cols"`
	Rows    int  `json:"rows"`
}

// EnsureResult — что пришлось сделать.
type EnsureResult struct {
	Started bool `json:"started"`
}

func (w *Worker) opEnsure(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[EnsureArgs](args)
	if err != nil {
		return nil, err
	}
	return w.Ensure(a)
}

// Ensure — ленивый resume: сессия из снимка поднимается командой из снимка в своей папке.
func (w *Worker) Ensure(a EnsureArgs) (EnsureResult, error) {
	if w.Tmux.Has(a.Name) {
		if !a.Respawn {
			return EnsureResult{}, nil
		}
		live, _, err := w.Tmux.List()
		if err != nil {
			return EnsureResult{}, err
		}
		for _, s := range live {
			if s.Name != a.Name || !s.PaneDead {
				continue
			}
			cmd := w.restoreCmd(s.Kind, s.Claude, s.StartCmd, a.Profile)
			if s.Kind == "claude" {
				w.Tmux.SetOption(a.Name, tmuxctl.OptStatus, "starting")
			}
			return EnsureResult{Started: true}, w.Tmux.Respawn(a.Name, s.Path, cmd)
		}
		return EnsureResult{}, nil
	}
	snap, err := w.loadSnapshot()
	if err != nil {
		return EnsureResult{}, err
	}
	i := snap.find(a.Name)
	if i < 0 {
		return EnsureResult{}, errors.New("сессии нет")
	}
	e := snap.Sessions[i]
	dir := e.Path
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		dir = w.Home
	}
	opts := map[string]string{tmuxctl.OptKind: e.Kind, tmuxctl.OptAuthor: e.Author, tmuxctl.OptClaude: e.Claude}
	if e.Kind == "claude" {
		opts[tmuxctl.OptStatus] = "starting"
	}
	err = w.Tmux.New(tmuxctl.NewOpts{
		Name: e.Name, Dir: dir, Command: w.restoreCmd(e.Kind, e.Claude, e.StartCmd, a.Profile),
		Env: envList(w.sessionEnv()), Options: opts, Width: a.Cols, Height: a.Rows,
	})
	if err != nil {
		return EnsureResult{}, err
	}
	w.Tmux.SetGlobalEnv(w.sessionEnv())
	return EnsureResult{Started: true}, w.reconcile()
}

func (w *Worker) restoreCmd(kind, claude, startCmd string, p Profile) []string {
	switch kind {
	case "claude":
		return w.claudeCmd(claude, p)
	case "shell":
		return w.shellCmd()
	}
	if strings.TrimSpace(startCmd) != "" {
		return []string{w.Shell, "-lic", startCmd}
	}
	return w.shellCmd()
}

type nameArgs struct {
	Name string `json:"name"`
	To   string `json:"to,omitempty"`
}

func (w *Worker) opKill(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[nameArgs](args)
	if err != nil {
		return nil, err
	}
	if w.Tmux.Has(a.Name) {
		if err := w.Tmux.Kill(a.Name); err != nil {
			return nil, err
		}
	}
	// Спящую сессию закрыть — значит убрать её из снимка.
	return nil, w.dropFromSnapshot(a.Name)
}

func (w *Worker) opRename(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[nameArgs](args)
	if err != nil {
		return nil, err
	}
	if err := names.Session(a.To); err != nil {
		return nil, err
	}
	if w.Tmux.Has(a.To) {
		return nil, errors.New("имя занято")
	}
	if !w.Tmux.Has(a.Name) {
		return nil, errors.New("переименовать можно только запущенную сессию")
	}
	if err := w.Tmux.Rename(a.Name, a.To); err != nil {
		return nil, err
	}
	return nil, w.reconcile()
}

func (w *Worker) opRespawn(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[EnsureArgs](args)
	if err != nil {
		return nil, err
	}
	a.Respawn = true
	return w.Ensure(a)
}

// MetaArgs — опции сессии, которые выставляет демон по хукам.
type MetaArgs struct {
	Name       string  `json:"name"`
	Status     *string `json:"status,omitempty"`
	Claude     *string `json:"claude,omitempty"`
	Transcript *string `json:"transcript,omitempty"`
}

func (w *Worker) opMeta(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[MetaArgs](args)
	if err != nil {
		return nil, err
	}
	if !w.Tmux.Has(a.Name) {
		return nil, errors.New("сессии нет")
	}
	set := func(k string, v *string) error {
		if v == nil {
			return nil
		}
		return w.Tmux.SetOption(a.Name, k, *v)
	}
	if err := set(tmuxctl.OptStatus, a.Status); err != nil {
		return nil, err
	}
	if err := set(tmuxctl.OptTranscript, a.Transcript); err != nil {
		return nil, err
	}
	if err := set(tmuxctl.OptClaude, a.Claude); err != nil {
		return nil, err
	}
	if a.Claude != nil {
		return nil, w.reconcile()
	}
	return nil, nil
}

func (w *Worker) opSync(_ *Conn, _ json.RawMessage) (any, error) { return w.Sessions() }

// InputArgs — сообщение из поля ввода.
type InputArgs struct {
	Name    string  `json:"name"`
	Text    string  `json:"text"`
	Enter   bool    `json:"enter"`
	Profile Profile `json:"profile"`
}

func (w *Worker) opInput(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[InputArgs](args)
	if err != nil {
		return nil, err
	}
	res, err := w.Ensure(EnsureArgs{Name: a.Name, Profile: a.Profile})
	if err != nil {
		return nil, err
	}
	if res.Started {
		// Только что поднятой сессии нужно время, чтобы TUI был готов принять ввод.
		time.Sleep(2 * time.Second)
	} else if live, _, err := w.Tmux.List(); err == nil {
		// Сессию только что завели: shell ещё не дочитал профиль, и Enter теряется.
		for _, s := range live {
			if s.Name == a.Name {
				if age := time.Since(time.Unix(s.Created, 0)); age < 1500*time.Millisecond {
					time.Sleep(1500*time.Millisecond - age)
				}
			}
		}
	}
	return res, w.Tmux.Paste(a.Name, a.Text, a.Enter)
}

var allowedKeys = map[string]bool{
	"Escape": true, "Enter": true, "Tab": true, "BTab": true, "C-c": true, "C-d": true,
	"Up": true, "Down": true, "Left": true, "Right": true, "PageUp": true, "PageDown": true,
}

func (w *Worker) opKeys(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[struct {
		Name string   `json:"name"`
		Keys []string `json:"keys"`
	}](args)
	if err != nil {
		return nil, err
	}
	// CancelMode — не клавиша, а выход из просмотра истории: клиент ставит его
	// первым, если перед вводом прокручивал терминал вверх.
	keys := a.Keys
	if len(keys) > 0 && keys[0] == "CancelMode" {
		w.Tmux.CancelMode(a.Name)
		keys = keys[1:]
	}
	for _, k := range keys {
		if !allowedKeys[k] {
			return nil, fmt.Errorf("клавиша %q не разрешена", k)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	return nil, w.Tmux.Keys(a.Name, keys...)
}

// opScroll — прокрутка истории со свайпа на телефоне. Идёт тем же путём, что и
// выход из просмотра истории (keys с CancelMode), поэтому порядок не путается.
func (w *Worker) opScroll(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[struct {
		Name  string `json:"name"`
		Lines int    `json:"lines"`
	}](args)
	if err != nil {
		return nil, err
	}
	if a.Lines > 1000 || a.Lines < -1000 {
		return nil, errors.New("слишком большая прокрутка")
	}
	// Claude Code живёт на альтернативном экране и прокручивает себя сам по колесу
	// мыши, как на десктопе; copy-mode tmux показал бы пустую историю.
	if mouse, sgr := w.Tmux.MouseApp(a.Name); mouse {
		return nil, w.Tmux.Wheel(a.Name, w.wheelSteps(a.Name, a.Lines), sgr)
	}
	return nil, w.Tmux.Scroll(a.Name, a.Lines)
}

// wheelLines — сколько строк свайпа даёт одно событие колеса: программа сама
// сдвигает экран на несколько строк за щелчок колеса.
const wheelLines = 3

// wheelSteps переводит строки свайпа в события колеса, остаток копится по сессии.
func (w *Worker) wheelSteps(name string, lines int) int {
	w.wheelMu.Lock()
	defer w.wheelMu.Unlock()
	if w.wheelRest == nil {
		w.wheelRest = map[string]int{}
	}
	r := w.wheelRest[name]
	if (r > 0) != (lines > 0) {
		r = 0 // сменилось направление
	}
	r += lines
	n := r / wheelLines
	w.wheelRest[name] = r - n*wheelLines
	return n
}

func (w *Worker) opHistory(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[nameArgs](args)
	if err != nil {
		return nil, err
	}
	out, err := w.Tmux.Capture(a.Name, 20000)
	if err != nil {
		return nil, err
	}
	return map[string]string{"text": out}, nil
}
