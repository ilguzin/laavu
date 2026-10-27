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

// Session statuses.
const (
	StStarting = "starting" // starting up
	StWorking  = "working"  // working
	StWaiting  = "waiting"  // waiting for you
	StSleeping = "sleeping" // in the snapshot, not in tmux
	StExited   = "exited"   // the pane process died
	StError    = "error"
	StActive   = "active" // a session without hooks is printing output
	StQuiet    = "quiet"  // a session without hooks is silent
)

// push_delay sets how long to wait for a new event after Stop before pushing.
// If "working" hangs with no hook events and no output, Stop was lost
// (an API error produces no Stop), so the status is reset to "waiting".
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

// statuses holds session statuses in memory, for tabs and push. After a restart
// the daemon takes them from tmux options.
type statuses struct {
	d  *Daemon
	mu sync.Mutex
	m  map[string]*sessState // agent/session
	// last is the last broadcast session list per agent.
	last map[string][]worker.SessionInfo
	now  func() time.Time
}

func newStatuses(d *Daemon) *statuses {
	return &statuses{d: d, m: map[string]*sessState{}, last: map[string][]worker.SessionInfo{}, now: time.Now}
}

func key(agent, session string) string { return agent + "/" + session }

// setAction records the agent's last action in a session.
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

// fromHook updates the status from a Claude Code hook event.
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

// pushIfStill sends the "waiting" push if no new event arrived within the delay.
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

// merge overlays in-memory statuses on the worker's list.
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
			// After a daemon restart, the status comes from the tmux option.
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

// starting marks a session as just started. If a hook arrived after since
// (claude starts faster than the worker replies), its status wins.
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

// refresh queries the worker, overlays statuses and broadcasts the list if it
// changed.
func (s *statuses) refresh(agent string, force bool) ([]worker.SessionInfo, error) {
	a, ok := s.d.Agent(agent)
	if !ok {
		return nil, fmt.Errorf("no agent %s", agent)
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

// sameSessions compares lists ignoring activity time, which changes constantly.
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

// cached returns the agent's last session list.
func (s *statuses) cached(agent string) []worker.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[agent]
}

// loop polls tmux of agents someone is connected to: this catches sessions
// created outside the client, activity and stuck statuses.
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

// sessionExists reports whether the agent has such a session. The agent itself
// reports the name in a hook, so made-up names (a flood of pushes with arbitrary text) are dropped.
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

// pushGap allows at most one push per session within this time.
var pushGap = 30 * time.Second

// notifyWaiting pushes to users with access to the agent who do not have the session on screen.
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
			Title: "Laavu", Body: fmt.Sprintf("Session %s is waiting for you", session),
			Agent: agent, Session: session, Tag: agent + "/" + session,
		})
	}
}

// hookEvent is an event from the hook socket.
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

// hookAction is the agent's last action for the session list: tool and path
// from PreToolUse. /clear erases the old action.
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

// clip returns an agent string of at most n bytes, without control characters.
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

// sendContext broadcasts context usage after a turn.
func (d *Daemon) sendContext(a *config.Agent, session string) {
	// The transcript is written slightly after the hook.
	time.Sleep(300 * time.Millisecond)
	var cu worker.ContextUsage
	if err := d.call(a, "context", map[string]string{"name": session}, &cu); err != nil {
		return
	}
	d.hub.broadcastAgent(a.Name, map[string]any{"t": "context", "agent": a.Name, "session": session, "context": cu})
}
