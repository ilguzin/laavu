// Package names проверяет имена агентов, рабочих папок и сессий.
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

// Builtin — имена, которые зарезервированы всегда, даже без файла в /etc/laavu.
var Builtin = []string{"tmp", "bin", "go", "snap", "node_modules"}

var (
	ErrBadName  = errors.New("недопустимое имя: латиница, цифры, точка, дефис, подчёркивание")
	ErrReserved = errors.New("имя зарезервировано")
)

// Agent проверяет имя агента: из него получается системный пользователь lv-<имя>.
func Agent(name string) error {
	if !agentRe.MatchString(name) {
		return errors.New("имя агента: строчная латиница, цифры и дефис, до 24 символов, с буквы")
	}
	return nil
}

// Session проверяет имя tmux-сессии: точку и двоеточие tmux не любит.
func Session(name string) error {
	if !sessionRe.MatchString(name) {
		return errors.New("имя сессии: латиница, цифры, дефис и подчёркивание")
	}
	return nil
}

// Reserved — список зарезервированных имён рабочих папок.
type Reserved struct {
	set map[string]bool
}

// NewReserved собирает список из встроенных имён и дополнительных.
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

// LoadReserved читает файл: одно имя на строку, # — комментарий.
// Отсутствующий файл не ошибка: остаются встроенные имена.
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

// IsReserved говорит, зарезервировано ли имя: всё, что с точки, и имена из списка.
func (r *Reserved) IsReserved(name string) bool {
	return strings.HasPrefix(name, ".") || r.set[name]
}

// Workdir проверяет имя рабочей папки.
func (r *Reserved) Workdir(name string) error {
	if r.IsReserved(name) {
		return ErrReserved
	}
	if !workdirRe.MatchString(name) {
		return ErrBadName
	}
	return nil
}
