package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"golang.org/x/sys/unix"

	"github.com/ilguzin/laavu/internal/safepath"
)

// MaxWrite is the largest file that can be edited from the client. The client
// also gets files up to this size in full in the viewer (the daemon's maxText).
const MaxWrite = 2 << 20

// Version is the file content version: an edit from the client is saved only if
// the version on disk is the one the client saw. The agent may have changed the file meanwhile.
func Version(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:16])
}

type writeArgs struct {
	Folder  string `json:"folder"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Base    string `json:"base"` // version the edit is based on; empty means a new file
}

// WriteResult is the write outcome. On Conflict the file is untouched and Version is what is on disk
// (empty if the file does not exist).
type WriteResult struct {
	Conflict bool   `json:"conflict,omitempty"`
	Version  string `json:"version"`
	Size     int64  `json:"size"`
	MTime    int64  `json:"mtime"`
}

// current reads the file and its version. A symlink in place of the file is refused: replacing via
// rename would turn the link into a regular file, and the edit would land somewhere unexpected.
func current(r *safepath.Root, rel string) ([]byte, os.FileMode, bool, error) {
	f, err := r.OpenFile(rel, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, 0, false, nil
	}
	if errors.Is(err, unix.ELOOP) {
		return nil, 0, false, errors.New("this is a symbolic link: open the file it points to")
	}
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	if !st.Mode().IsRegular() {
		return nil, 0, false, errors.New("not a regular file")
	}
	if st.Size() > MaxWrite {
		return nil, 0, false, fmt.Errorf("file larger than %d MB: editing from the client is not supported", MaxWrite>>20)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxWrite+1))
	return b, st.Mode().Perm(), true, err
}

// opWrite writes the whole file: a temp file next to it, fsync, rename over it.
// A reader sees either the old content or the new one, never half of it.
func (w *Worker) opWrite(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[writeArgs](args)
	if err != nil {
		return nil, err
	}
	if len(a.Content) > MaxWrite {
		return nil, fmt.Errorf("file larger than %d MB", MaxWrite>>20)
	}
	rel, err := safepath.Clean(a.Path)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return nil, errors.New("no file specified")
	}
	r, err := w.openRoot(a.Folder)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	old, mode, exists, err := current(r, rel)
	if err != nil {
		return nil, err
	}
	if v := versionOf(old, exists); v != a.Base {
		return WriteResult{Conflict: true, Version: v}, nil
	}
	if !exists {
		mode = 0o600
	}

	dir, name := path.Split(rel)
	if dir == "" {
		dir = "."
	}
	if err := r.MkdirAll(dir); err != nil {
		return nil, err
	}
	tmp, tmpRel, err := r.CreateExclusive(dir, "."+name+".laavu-save")
	if err != nil {
		return nil, err
	}
	fail := func(e error) (any, error) {
		tmp.Close()
		r.Remove(tmpRel)
		return nil, e
	}
	if _, err := io.WriteString(tmp, a.Content); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	st, err := tmp.Stat()
	if err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		r.Remove(tmpRel)
		return nil, err
	}

	// Check the version once more before replacing: the agent may have written the file while we were writing.
	again, _, existsNow, err := current(r, rel)
	if err != nil {
		r.Remove(tmpRel)
		return nil, err
	}
	if v := versionOf(again, existsNow); v != a.Base {
		r.Remove(tmpRel)
		return WriteResult{Conflict: true, Version: v}, nil
	}
	d, err := r.OpenFile(dir, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		r.Remove(tmpRel)
		return nil, err
	}
	err = unix.Renameat(int(d.Fd()), path.Base(tmpRel), int(d.Fd()), name)
	d.Close()
	if err != nil {
		r.Remove(tmpRel)
		return nil, &os.PathError{Op: "rename", Path: rel, Err: err}
	}
	return WriteResult{Version: Version([]byte(a.Content)), Size: st.Size(), MTime: st.ModTime().Unix()}, nil
}

func versionOf(b []byte, exists bool) string {
	if !exists {
		return ""
	}
	return Version(b)
}
