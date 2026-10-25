// Package safepath открывает файлы только внутри рабочей папки.
//
// Пути проверяет ядро: openat2 с RESOLVE_BENEATH от дескриптора рабочей папки.
// Симлинк, который ведёт наружу, в /etc или в ~/.ssh, открыть не даст.
package safepath

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var (
	ErrOutside = errors.New("путь вне рабочей папки")
	ErrHidden  = errors.New("скрытый файл")
)

// Root — открытая рабочая папка.
type Root struct {
	fd   int
	path string
}

// Open открывает рабочую папку name внутри дома. Сама папка не может быть симлинком.
func Open(home, name string) (*Root, error) {
	if name == "" || strings.ContainsRune(name, '/') || name == "." || name == ".." {
		return nil, ErrOutside
	}
	hfd, err := unix.Open(home, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: home, Err: err}
	}
	defer unix.Close(hfd)
	fd, err := unix.Openat2(hfd, name, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return &Root{fd: fd, path: filepath.Join(home, name)}, nil
}

func (r *Root) Close() error { return unix.Close(r.fd) }

// Path — абсолютный путь рабочей папки.
func (r *Root) Path() string { return r.path }

// Clean приводит относительный путь к виду без "..", "." и ведущего слэша.
// Выход за корень — ошибка, а не тихое обрезание.
func Clean(rel string) (string, error) {
	if rel == "" || rel == "/" || rel == "." {
		return ".", nil
	}
	rel = strings.TrimPrefix(rel, "/")
	c := path.Clean(rel)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", ErrOutside
	}
	return c, nil
}

// HasHidden — есть ли в пути компонент, начинающийся с точки.
func HasHidden(rel string) bool {
	for _, p := range strings.Split(rel, "/") {
		if strings.HasPrefix(p, ".") && p != "." {
			return true
		}
	}
	return false
}

// OpenFile открывает файл или каталог внутри рабочей папки.
func (r *Root) OpenFile(rel string, flags int, mode uint32) (*os.File, error) {
	c, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat2(r.fd, c, &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC),
		Mode:    uint64(mode),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		if errors.Is(err, unix.EXDEV) {
			return nil, ErrOutside
		}
		return nil, &os.PathError{Op: "open", Path: c, Err: err}
	}
	return os.NewFile(uintptr(fd), filepath.Join(r.path, c)), nil
}

// Open открывает на чтение. O_NONBLOCK — чтобы FIFO, который положил агент,
// не повесил воркер на открытии; читать воркер станет только обычные файлы.
func (r *Root) Open(rel string) (*os.File, error) {
	return r.OpenFile(rel, unix.O_RDONLY|unix.O_NONBLOCK, 0)
}

// OpenVisible открывает на чтение и отказывает, если путь или то, куда ведут
// симлинки, проходит через скрытый компонент. Так origin просмотра не отдаёт
// ни .env, ни симлинк на него.
func (r *Root) OpenVisible(rel string) (*os.File, error) {
	c, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	if HasHidden(c) {
		return nil, ErrHidden
	}
	f, err := r.Open(c)
	if err != nil {
		return nil, err
	}
	real, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil {
		f.Close()
		return nil, err
	}
	relReal, err := filepath.Rel(r.path, real)
	if err != nil || relReal == ".." || strings.HasPrefix(relReal, "../") {
		f.Close()
		return nil, ErrOutside
	}
	if HasHidden(filepath.ToSlash(relReal)) {
		f.Close()
		return nil, ErrHidden
	}
	return f, nil
}

// MkdirAll создаёт цепочку каталогов внутри рабочей папки.
func (r *Root) MkdirAll(rel string) error {
	c, err := Clean(rel)
	if err != nil {
		return err
	}
	if c == "." {
		return nil
	}
	cur := "."
	for _, p := range strings.Split(c, "/") {
		next := path.Join(cur, p)
		d, err := r.OpenFile(cur, unix.O_PATH|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		err = unix.Mkdirat(int(d.Fd()), p, 0o700)
		d.Close()
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return &os.PathError{Op: "mkdir", Path: next, Err: err}
		}
		// Проверка, что получившийся путь — каталог внутри папки, а не симлинк наружу.
		chk, err := r.OpenFile(next, unix.O_PATH|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		chk.Close()
		cur = next
	}
	return nil
}

// CreateExclusive создаёт новый файл в каталоге dir. Если имя занято, к нему
// добавляется номер: «фото.jpg» → «фото-1.jpg». Существующие файлы не перезаписываются.
func (r *Root) CreateExclusive(dir, name string) (*os.File, string, error) {
	if name == "" || strings.ContainsAny(name, "/\x00") || name == "." || name == ".." {
		return nil, "", errors.New("недопустимое имя файла")
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		n := name
		if i > 0 {
			n = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		rel := path.Join(dir, n)
		f, err := r.OpenFile(rel, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
		if err == nil {
			return f, rel, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("не удалось подобрать свободное имя")
}

// Remove удаляет файл внутри рабочей папки.
func (r *Root) Remove(rel string) error {
	c, err := Clean(rel)
	if err != nil {
		return err
	}
	dir, base := path.Split(c)
	if dir == "" {
		dir = "."
	}
	d, err := r.OpenFile(dir, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer d.Close()
	return unix.Unlinkat(int(d.Fd()), base, 0)
}
