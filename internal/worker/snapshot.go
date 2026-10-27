package worker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/ilguzin/laavu/internal/tmuxctl"
	"github.com/ilguzin/laavu/internal/wire"
)

// Conn is a connection to the daemon.
type Conn = wire.Conn

// SnapEntry is a snapshot entry: everything needed to bring a session back after a reboot.
type SnapEntry struct {
	Name     string         `json:"name"`
	Path     string         `json:"path"`
	Kind     string         `json:"kind"`
	Author   string         `json:"author,omitempty"`
	Claude   string         `json:"claude,omitempty"`
	StartCmd string         `json:"start_cmd,omitempty"`
	Server   tmuxctl.Server `json:"server"`
}

// Snapshot — ~/.local/state/laavu/sessions.json.
type Snapshot struct {
	Sessions []SnapEntry `json:"sessions"`
}

func (s *Snapshot) find(name string) int {
	for i, e := range s.Sessions {
		if e.Name == name {
			return i
		}
	}
	return -1
}

func (w *Worker) snapPath() string { return filepath.Join(w.StateDir, "sessions.json") }

func (w *Worker) loadSnapshot() (*Snapshot, error) {
	b, err := os.ReadFile(w.snapPath())
	if errors.Is(err, os.ErrNotExist) {
		return &Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		// A corrupted snapshot is not a disaster: conversations remain in the Claude Code history.
		w.log.Warn("snapshot corrupted, starting over", "err", err)
		return &Snapshot{}, nil
	}
	return &s, nil
}

func (w *Worker) saveSnapshot(s *Snapshot) error {
	if err := os.MkdirAll(w.StateDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(w.StateDir, ".sessions-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), w.snapPath())
}

// Reconcile brings the snapshot in line with tmux. Live sessions are recorded from their options.
// Entries from the same tmux server run that no longer exist are removed: the session
// was closed or renamed. Entries from a previous run stay: they are sleeping
// sessions after a reboot.
func Reconcile(old *Snapshot, live []tmuxctl.Session, srv tmuxctl.Server) *Snapshot {
	out := &Snapshot{Sessions: []SnapEntry{}}
	seen := map[string]bool{}
	for _, s := range live {
		seen[s.Name] = true
		e := SnapEntry{Name: s.Name, Path: s.Path, Kind: s.Kind, Author: s.Author, Claude: s.Claude, StartCmd: s.StartCmd, Server: srv}
		if e.Kind == "" {
			e.Kind = "other"
		}
		out.Sessions = append(out.Sessions, e)
	}
	for _, e := range old.Sessions {
		if seen[e.Name] {
			continue
		}
		if srv.PID != 0 && e.Server == srv {
			continue
		}
		out.Sessions = append(out.Sessions, e)
	}
	return out
}

func (w *Worker) reconcile() error {
	w.snapMu.Lock()
	defer w.snapMu.Unlock()
	live, srv, err := w.Tmux.List()
	if err != nil {
		return err
	}
	old, err := w.loadSnapshot()
	if err != nil {
		return err
	}
	return w.saveSnapshot(Reconcile(old, live, srv))
}

func (w *Worker) dropFromSnapshot(name string) error {
	w.snapMu.Lock()
	defer w.snapMu.Unlock()
	s, err := w.loadSnapshot()
	if err != nil {
		return err
	}
	if i := s.find(name); i >= 0 {
		s.Sessions = append(s.Sessions[:i], s.Sessions[i+1:]...)
		return w.saveSnapshot(s)
	}
	return nil
}
