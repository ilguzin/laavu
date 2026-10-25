package daemon

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ilguzin/laavu/internal/config"
	"github.com/ilguzin/laavu/internal/names"
	"github.com/ilguzin/laavu/internal/push"
	"github.com/ilguzin/laavu/internal/worker"
)

// Статусы сессий.
const (
	StStarting = "starting" // запускается
	StWorking  = "working"  // работает
	StWaiting  = "waiting"  // ждёт ответа
	StSleeping = "sleeping" // есть в снимке, нет в tmux
	StExited   = "exited"   // процесс в панели умер
	StError    = "error"
	StActive   = "active" // сессия без хуков что-то выводит
	StQuiet    = "quiet"  // сессия без хуков молчит
)

// Сколько ждать нового события после Stop, прежде чем слать пуш, задаёт push_delay.
// Если «работает» висит без событий хуков и без вывода, значит Stop потерялся
// (ошибка API не даёт Stop) — статус сбрасывается в «ждёт ответа».
const (
	staleWorking  = 3 * time.Minute
	staleActivity = 2 * time.Minute
	activeWindow  = 10 * time.Second
	pollInterval  = 3 * time.Second
)

type sessState struct {
	status    string
	lastEvent time.Time
	seq       uint64
	timer     *time.Timer
	lastPush  time.Time
	action    *worker.Action
}

// statuses — статусы сессий в памяти: для вкладок и пуша. После перезапуска
// демон берёт их из опций tmux.
type statuses struct {
	d  *Daemon
	mu sync.Mutex
	m  map[string]*sessState // агент/сессия
	// last — последний разосланный список сессий агента.
	last map[string][]worker.SessionInfo
	now  func() time.Time
}

func newStatuses(d *Daemon) *statuses {
	return &statuses{d: d, m: map[string]*sessState{}, last: map[string][]worker.SessionInfo{}, now: time.Now}
}

func key(agent, session string) string { return agent + "/" + session }

// setAction запоминает последнее действие агента в сессии.
func (s *statuses) setAction(agent, session string, a *worker.Action) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.m[key(agent, session)]
	if st == nil {
		st = &sessState{}
		s.m[key(agent, session)] = st
	}
	st.action = a
}

// fromHook — событие хука Claude Code меняет статус.
func (s *statuses) fromHook(agent, session, event string) (status string, changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(agent, session)
	st := s.m[k]
	if st == nil {
		st = &sessState{}
		s.m[k] = st
	}
	st.lastEvent = s.now()
	st.seq++
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	prev := st.status
	switch event {
	case "SessionStart", "Stop", "Notification":
		st.status = StWaiting
	case "UserPromptSubmit", "PreToolUse", "PostToolUse", "PreCompact", "SubagentStop":
		st.status = StWorking
	default:
		return st.status, false
	}
	if event == "Stop" || event == "Notification" {
		seq := st.seq
		st.timer = time.AfterFunc(s.d.Cfg.PushDelay.Duration, func() { s.pushIfStill(agent, session, seq) })
	}
	return st.status, prev != st.status
}

// pushIfStill — пуш «ждёт ответа», если за задержку не пришло нового события.
func (s *statuses) pushIfStill(agent, session string, seq uint64) {
	s.mu.Lock()
	st := s.m[key(agent, session)]
	ok := st != nil && st.seq == seq && st.status == StWaiting
	s.mu.Unlock()
	if !ok {
		return
	}
	s.d.notifyWaiting(agent, session)
}

// merge накладывает статусы из памяти на список воркера.
func (s *statuses) merge(agent string, list []worker.SessionInfo) []worker.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for i := range list {
		si := &list[i]
		if si.Sleeping || si.Dead {
			delete(s.m, key(agent, si.Name))
			continue
		}
		if si.Kind != "claude" {
			if now.Sub(time.Unix(si.Activity, 0)) < activeWindow {
				si.Status = StActive
			} else {
				si.Status = StQuiet
			}
			continue
		}
		st := s.m[key(agent, si.Name)]
		if st == nil {
			// После перезапуска демона — статус из опции tmux.
			if si.Status == "" {
				si.Status = StStarting
			}
			st = &sessState{status: si.Status, lastEvent: now}
			s.m[key(agent, si.Name)] = st
		}
		if st.status == StWorking && now.Sub(st.lastEvent) > staleWorking && now.Sub(time.Unix(si.Activity, 0)) > staleActivity {
			st.status = StWaiting
		}
		si.Status = st.status
		si.Action = st.action
	}
	return list
}

// starting — сессию только что подняли. Если хук успел прийти после since
// (claude стартует быстрее, чем отвечает воркер), его статус важнее.
func (s *statuses) starting(agent, session string, since time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.m[key(agent, session)]
	if st == nil {
		st = &sessState{}
		s.m[key(agent, session)] = st
	}
	if st.lastEvent.After(since) {
		return
	}
	st.status = StStarting
	st.lastEvent = s.now()
}

// refresh спрашивает воркер, накладывает статусы и рассылает список, если он
// изменился.
func (s *statuses) refresh(agent string, force bool) ([]worker.SessionInfo, error) {
	a, ok := s.d.Agent(agent)
	if !ok {
		return nil, fmt.Errorf("нет агента %s", agent)
	}
	var list []worker.SessionInfo
	if err := s.d.call(a, "sessions", nil, &list); err != nil {
		return nil, err
	}
	list = s.merge(agent, list)
	s.mu.Lock()
	alive := map[string]bool{}
	for _, si := range list {
		alive[key(agent, si.Name)] = true
	}
	for k, st := range s.m {
		if strings.HasPrefix(k, agent+"/") && !alive[k] {
			if st.timer != nil {
				st.timer.Stop()
			}
			delete(s.m, k)
		}
	}
	prev := s.last[agent]
	changed := force || !sameSessions(prev, list)
	s.last[agent] = list
	s.mu.Unlock()
	if changed {
		s.d.hub.broadcastAgent(agent, map[string]any{"t": "sessions", "agent": agent, "list": list})
	}
	return list, nil
}

// sameSessions сравнивает списки без времени активности: оно меняется постоянно.
func sameSessions(a, b []worker.SessionInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		x.Activity, y.Activity = 0, 0
		x.Command, y.Command = "", ""
		if !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}

// cached — последний список сессий агента.
func (s *statuses) cached(agent string) []worker.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[agent]
}

// loop опрашивает tmux агентов, к которым кто-то подключён: так видны сессии,
// заведённые мимо клиента, активность и зависшие статусы.
func (s *statuses) loop(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for _, a := range s.d.order {
		s.refresh(a, false)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, a := range s.d.order {
				if s.d.hub.hasAgentSubscribers(a) {
					s.refresh(a, false)
				}
			}
		}
	}
}

// sessionExists — есть ли такая сессия у агента. Имя в хуке сообщает сам агент,
// поэтому выдуманные имена (поток пушей с произвольным текстом) отбрасываются.
func (d *Daemon) sessionExists(agent, session string) bool {
	for _, s := range d.status.cached(agent) {
		if s.Name == session {
			return true
		}
	}
	list, err := d.status.refresh(agent, false)
	if err != nil {
		return false
	}
	for _, s := range list {
		if s.Name == session && !s.Sleeping {
			return true
		}
	}
	return false
}

// pushGap — не чаще одного пуша на сессию за это время.
var pushGap = 30 * time.Second

// notifyWaiting — пуш тем, у кого есть доступ к агенту и у кого сессия не на экране.
func (d *Daemon) notifyWaiting(agent, session string) {
	if d.Push == nil {
		return
	}
	d.status.mu.Lock()
	st := d.status.m[key(agent, session)]
	if st == nil || time.Since(st.lastPush) < pushGap {
		d.status.mu.Unlock()
		return
	}
	st.lastPush = time.Now()
	d.status.mu.Unlock()
	for uid := range d.usersWithAccess(agent) {
		if d.hub.isWatching(uid, agent, session) {
			continue
		}
		d.Push.SendUser(uid, push.Message{
			Title: "Laavu", Body: fmt.Sprintf("Сессия %s ждёт ответа", session),
			Agent: agent, Session: session, Tag: agent + "/" + session,
		})
	}
}

// hookEvent — событие из сокета хуков.
type hookEvent struct {
	Kind         string `json:"kind"`
	Event        string `json:"event"`
	Session      string `json:"session"`
	SessionID    string `json:"session_id"`
	Source       string `json:"source"`
	Reason       string `json:"reason"`
	Transcript   string `json:"transcript"`
	Notification string `json:"notification"`
	Tool         string `json:"tool"`
	Target       string `json:"target"`
}

func (d *Daemon) handleHook(agent string, ev hookEvent) {
	a, ok := d.Agent(agent)
	if !ok {
		return
	}
	switch ev.Kind {
	case "tmux":
		d.status.refresh(agent, false)
	case "claude":
		if names.Session(ev.Session) != nil || !d.sessionExists(agent, ev.Session) {
			return
		}
		status, changed := d.status.fromHook(agent, ev.Session, ev.Event)
		meta := worker.MetaArgs{Name: ev.Session}
		if changed {
			meta.Status = &status
		}
		if ev.Event == "SessionStart" && ev.SessionID != "" {
			meta.Claude = &ev.SessionID
			if ev.Transcript != "" {
				meta.Transcript = &ev.Transcript
			}
		}
		if meta.Status != nil || meta.Claude != nil {
			go d.call(a, "meta", meta, nil)
		}
		if changed {
			d.hub.broadcastAgent(agent, map[string]any{"t": "status", "agent": agent, "session": ev.Session, "status": status})
		}
		d.hookAction(agent, ev)
		if ev.Event == "Stop" || ev.Event == "SessionStart" {
			go d.sendContext(a, ev.Session)
		}
	}
}

// hookAction — последнее действие агента для списка сессий: инструмент и путь
// из PreToolUse. После /clear старое действие стирается.
func (d *Daemon) hookAction(agent string, ev hookEvent) {
	var act *worker.Action
	switch {
	case ev.Event == "PreToolUse" && ev.Tool != "":
		act = &worker.Action{Tool: clip(ev.Tool, 64), Target: clip(ev.Target, 240), At: d.status.now().Unix()}
	case ev.Event == "SessionStart" && ev.Source == "clear":
	default:
		return
	}
	d.status.setAction(agent, ev.Session, act)
	d.hub.broadcastAgent(agent, map[string]any{"t": "action", "agent": agent, "session": ev.Session, "action": act})
}

// clip — строка от агента не длиннее n байт, без управляющих символов.
func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	if len(s) > n {
		i := n
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		s = s[:i]
	}
	return s
}

// sendContext рассылает заполненность контекста после хода.
func (d *Daemon) sendContext(a *config.Agent, session string) {
	// Транскрипт дописывается чуть позже хука.
	time.Sleep(300 * time.Millisecond)
	var cu worker.ContextUsage
	if err := d.call(a, "context", map[string]string{"name": session}, &cu); err != nil {
		return
	}
	d.hub.broadcastAgent(a.Name, map[string]any{"t": "context", "agent": a.Name, "session": session, "context": cu})
}
