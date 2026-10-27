// Package safepath opens files only inside a folder.
//
// Paths are checked by the kernel: openat2 with RESOLVE_BENEATH from the folder descriptor.
// A symlink pointing outside, to /etc or ~/.ssh, cannot be opened.
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
	ErrOutside = errors.New("path outside the folder")
	ErrHidden  = errors.New("hidden file")
)

// Root is an open folder.
type Root struct {
	fd   int
	path string
}

// Open opens the folder name inside home. The folder itself cannot be a symlink.
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

// Path returns the absolute path of the folder.
func (r *Root) Path() string { return r.path }

// Clean normalizes a relative path to a form without "..", "." and a leading slash.
// Escaping the root is an error, not silent truncation.
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

// HasHidden reports whether the path has a component starting with a dot.
func HasHidden(rel string) bool {
	for _, p := range strings.Split(rel, "/") {
		if strings.HasPrefix(p, ".") && p != "." {
			return true
		}
	}
	return false
}

// OpenFile opens a file or directory inside the folder.
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

// Open opens for reading. O_NONBLOCK keeps a FIFO planted by the agent from
// hanging the worker on open; the worker only reads regular files anyway.
func (r *Root) Open(rel string) (*os.File, error) {
	return r.OpenFile(rel, unix.O_RDONLY|unix.O_NONBLOCK, 0)
}

// OpenVisible opens for reading and refuses if the path, or where its symlinks
// lead, passes through a hidden component. This way the preview origin serves
// neither .env nor a symlink to it.
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

// MkdirAll creates a chain of directories inside the folder.
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
		// Check that the resulting path is a directory inside the folder, not a symlink pointing outside.
		chk, err := r.OpenFile(next, unix.O_PATH|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		chk.Close()
		cur = next
	}
	return nil
}

// CreateExclusive creates a new file in directory dir. If the name is taken, a number
// is appended: "photo.jpg" -> "photo-1.jpg". Existing files are never overwritten.
func (r *Root) CreateExclusive(dir, name string) (*os.File, string, error) {
	if name == "" || strings.ContainsAny(name, "/\x00") || name == "." || name == ".." {
		return nil, "", errors.New("invalid file name")
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
	return nil, "", errors.New("could not find a free name")
}

// Remove deletes a file inside the folder.
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
