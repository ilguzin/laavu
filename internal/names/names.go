// Package names validates agent, folder and session names.
package names

import (
	"bufio"
	"errors"
	"os"
	"regexp"
	"strings"
)

var (
	agentRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
	workdirRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)
	sessionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Builtin lists names that are always reserved, even without a file in /etc/laavu.
var Builtin = []string{"tmp", "bin", "go", "snap", "node_modules"}

var (
	ErrBadName  = errors.New("invalid name: use Latin letters, digits, dot, hyphen, underscore")
	ErrReserved = errors.New("name is reserved")
)

// Agent validates an agent name: it becomes the system user lv-<name>.
func Agent(name string) error {
	if !agentRe.MatchString(name) {
		return errors.New("agent name: lowercase Latin letters, digits and hyphen, up to 24 characters, starting with a letter")
	}
	return nil
}

// Session validates a tmux session name: tmux dislikes dots and colons.
func Session(name string) error {
	if !sessionRe.MatchString(name) {
		return errors.New("session name: Latin letters, digits, hyphen and underscore")
	}
	return nil
}

// Reserved is the list of reserved folder names.
type Reserved struct {
	set map[string]bool
}

// NewReserved builds the list from the built-in names and extra ones.
func NewReserved(extra ...string) *Reserved {
	r := &Reserved{set: map[string]bool{}}
	for _, n := range Builtin {
		r.set[n] = true
	}
	for _, n := range extra {
		r.set[n] = true
	}
	return r
}

// LoadReserved reads a file: one name per line, # starts a comment.
// A missing file is not an error: the built-in names remain.
func LoadReserved(path string) (*Reserved, error) {
	r := NewReserved()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line != "" {
			r.set[line] = true
		}
	}
	return r, sc.Err()
}

// IsReserved reports whether a name is reserved: anything starting with a dot, and names from the list.
func (r *Reserved) IsReserved(name string) bool {
	return strings.HasPrefix(name, ".") || r.set[name]
}

// Workdir validates a folder name.
func (r *Reserved) Workdir(name string) error {
	if r.IsReserved(name) {
		return ErrReserved
	}
	if !workdirRe.MatchString(name) {
		return ErrBadName
	}
	return nil
}
