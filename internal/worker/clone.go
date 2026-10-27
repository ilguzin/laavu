package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CloneArgs is a git clone into a new folder.
type CloneArgs struct {
	URL    string `json:"url"`
	Folder string `json:"folder"`
}

// CloneTimeout is how long a clone may take; the daemon waits a little longer.
const CloneTimeout = 10 * time.Minute

// scpLike is user@host:path, the form GitHub and GitLab show for ssh.
var scpLike = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.-]*:[^\s]+$`)

// checkCloneURL lets through https, ssh and user@host:path. Other git transports
// (ext::, file, local paths) would reach things the clone has no business with.
func (w *Worker) checkCloneURL(u string) error {
	switch {
	case strings.ContainsAny(u, " \t\r\n"):
	case strings.HasPrefix(u, "https://") && len(u) > len("https://"):
		return nil
	case strings.HasPrefix(u, "ssh://") && len(u) > len("ssh://"):
		return nil
	case scpLike.MatchString(u):
		return nil
	case w.LocalClone && (strings.HasPrefix(u, "file://") || filepath.IsAbs(u)):
		return nil
	}
	return errors.New("repository address: https://…, ssh://… or git@host:path")
}

func (w *Worker) opClone(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[CloneArgs](args)
	if err != nil {
		return nil, err
	}
	if err := w.checkCloneURL(a.URL); err != nil {
		return nil, err
	}
	if err := w.Reserved.Workdir(a.Folder); err != nil {
		return nil, err
	}
	dir := filepath.Join(w.Home, a.Folder)
	if _, err := os.Lstat(dir); err == nil {
		return nil, errors.New("workspace already exists")
	}
	ctx, cancel := context.WithTimeout(context.Background(), CloneTimeout)
	defer cancel()
	argv := []string{"git", "clone", "--", a.URL, dir}
	if w.Tmux.Scope {
		// Like tmux, the clone runs in the agent's own systemd scope: a big repository
		// needs more memory than the worker's cgroup allows.
		argv = append([]string{"systemd-run", "--user", "--scope", "--quiet", "--collect", "--"}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = w.Home
	// Nobody can answer a prompt: a password or host question would hang the clone.
	// A new host key is accepted and remembered, as on the first ssh by hand.
	cmd.Env = append(append(os.Environ(), w.Tmux.Env...), "GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			// git cleans up after itself on an error, but not when it is killed.
			os.RemoveAll(dir)
			return nil, errors.New("git clone took longer than 10 minutes")
		}
		return nil, errors.New(cloneError(stderr.String()))
	}
	w.log.Info("cloned", "folder", a.Folder)
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	wd := Workdir{Name: a.Folder, MTime: info.ModTime().Unix(), Git: true}
	wd.Branch, wd.Changes = w.gitStatus(dir)
	return wd, nil
}

// cloneError keeps the lines of git's stderr that say what went wrong, without the "Cloning into" chatter.
func cloneError(s string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Cloning into") || strings.HasPrefix(line, "Warning: Permanently added") {
			continue
		}
		keep = append(keep, line)
	}
	if len(keep) > 6 {
		keep = keep[len(keep)-6:]
	}
	if len(keep) == 0 {
		return "git clone failed"
	}
	return strings.Join(keep, "\n")
}
