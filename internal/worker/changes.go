package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ilguzin/laavu/internal/safepath"
)

// Changes describes what changed in the folder since Since.
type Changes struct {
	Git         bool         `json:"git"`
	Commits     []Commit     `json:"commits,omitempty"`
	Uncommitted []FileChange `json:"uncommitted,omitempty"`
	Files       []FileChange `json:"files,omitempty"` // without git: by modification time
	Truncated   bool         `json:"truncated,omitempty"`
}

type Commit struct {
	Hash    string       `json:"hash"`
	Author  string       `json:"author"`
	Time    int64        `json:"time"`
	Subject string       `json:"subject"`
	Files   []FileChange `json:"files"`
}

type FileChange struct {
	Status string `json:"status,omitempty"`
	Path   string `json:"path"`
	MTime  int64  `json:"mtime,omitempty"`
}

type changesArgs struct {
	Folder string `json:"folder"`
	Since  int64  `json:"since"`
}

const maxChangeFiles = 500

func (w *Worker) git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.quotepath=off"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "HOME="+w.Home)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

func (w *Worker) opChanges(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[changesArgs](args)
	if err != nil {
		return nil, err
	}
	r, err := w.openRoot(a.Folder)
	if err != nil {
		return nil, err
	}
	root := r.Path()
	r.Close()
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		return w.gitChanges(root, a.Since)
	}
	return mtimeChanges(root, a.Since), nil
}

func (w *Worker) gitChanges(root string, since int64) (*Changes, error) {
	ch := &Changes{Git: true}
	out, err := w.git(root, "log", "--since=@"+strconv.FormatInt(since, 10), "--no-merges",
		"--format=%x1e%H%x1f%an%x1f%at%x1f%s", "--name-status", "-n", "200")
	if err == nil {
		for _, rec := range strings.Split(out, "\x1e") {
			rec = strings.TrimSpace(rec)
			if rec == "" {
				continue
			}
			lines := strings.Split(rec, "\n")
			f := strings.Split(lines[0], "\x1f")
			if len(f) != 4 {
				continue
			}
			// An empty commit still gets [], not null: the client iterates over files.
			c := Commit{Hash: f[0], Author: f[1], Subject: f[3], Files: []FileChange{}}
			c.Time, _ = strconv.ParseInt(f[2], 10, 64)
			for _, l := range lines[1:] {
				p := strings.Split(strings.TrimSpace(l), "\t")
				if len(p) >= 2 {
					c.Files = append(c.Files, FileChange{Status: p[0][:1], Path: p[len(p)-1]})
				}
			}
			ch.Commits = append(ch.Commits, c)
		}
	}
	out, err = w.git(root, "status", "--porcelain=v1", "-uall")
	if err == nil {
		for _, l := range strings.Split(out, "\n") {
			if len(l) < 4 {
				continue
			}
			p := l[3:]
			if i := strings.Index(p, " -> "); i >= 0 {
				p = p[i+4:]
			}
			ch.Uncommitted = append(ch.Uncommitted, FileChange{Status: strings.TrimSpace(l[:2]), Path: p})
			if len(ch.Uncommitted) >= maxChangeFiles {
				ch.Truncated = true
				break
			}
		}
	}
	return ch, nil
}

func mtimeChanges(root string, since int64) *Changes {
	ch := &Changes{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if p != root && (strings.HasPrefix(name, ".") || skipWatch[name] || name == "node_modules") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Unix() < since {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		ch.Files = append(ch.Files, FileChange{Path: filepath.ToSlash(rel), MTime: info.ModTime().Unix()})
		if len(ch.Files) >= maxChangeFiles {
			ch.Truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	sort.Slice(ch.Files, func(i, j int) bool { return ch.Files[i].MTime > ch.Files[j].MTime })
	return ch
}

type diffArgs struct {
	Folder string `json:"folder"`
	Path   string `json:"path"`
	Since  int64  `json:"since"`
}

const maxDiff = 1 << 20

// opDiff returns the file diff from the last commit before Since up to and including the working tree.
func (w *Worker) opDiff(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[diffArgs](args)
	if err != nil {
		return nil, err
	}
	r, err := w.openRoot(a.Folder)
	if err != nil {
		return nil, err
	}
	root := r.Path()
	r.Close()
	p, err := safepath.Clean(a.Path)
	if err != nil {
		return nil, err
	}
	base, _ := w.git(root, "rev-list", "-1", "--before=@"+strconv.FormatInt(a.Since, 10), "HEAD")
	base = strings.TrimSpace(base)
	var out string
	if base == "" {
		// The repository is younger than the mark: diff against the empty tree.
		out, _ = w.git(root, "diff", "--no-color", "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "--", p)
		if out == "" {
			out, _ = w.git(root, "diff", "--no-color", "--no-index", "--", "/dev/null", p)
		}
	} else {
		out, _ = w.git(root, "diff", "--no-color", base, "--", p)
	}
	if out == "" {
		// Untracked file.
		out, _ = w.git(root, "diff", "--no-color", "--no-index", "--", "/dev/null", p)
	}
	trunc := false
	if len(out) > maxDiff {
		out, trunc = out[:maxDiff], true
	}
	return map[string]any{"diff": out, "truncated": trunc}, nil
}
