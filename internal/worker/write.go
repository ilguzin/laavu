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

// MaxWrite — самый большой файл, который правится из клиента. Столько же
// клиент получает целиком в просмотрщик (maxText демона).
const MaxWrite = 2 << 20

// Version — версия содержимого файла: правка из клиента сохраняется, только если
// на диске та же версия, что клиент видел. Агент мог поменять файл за это время.
func Version(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:16])
}

type writeArgs struct {
	Folder  string `json:"folder"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Base    string `json:"base"` // версия, от которой правили; пусто — новый файл
}

// WriteResult — итог записи. При Conflict файл не тронут, Version — то, что на диске
// (пусто, если файла нет).
type WriteResult struct {
	Conflict bool   `json:"conflict,omitempty"`
	Version  string `json:"version"`
	Size     int64  `json:"size"`
	MTime    int64  `json:"mtime"`
}

// current читает файл и его версию. Симлинк на месте файла — отказ: замена через
// rename превратила бы ссылку в обычный файл, а правка ушла бы не туда, куда ждали.
func current(r *safepath.Root, rel string) ([]byte, os.FileMode, bool, error) {
	f, err := r.OpenFile(rel, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, 0, false, nil
	}
	if errors.Is(err, unix.ELOOP) {
		return nil, 0, false, errors.New("это символическая ссылка: откройте файл, на который она ведёт")
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
		return nil, 0, false, errors.New("не обычный файл")
	}
	if st.Size() > MaxWrite {
		return nil, 0, false, fmt.Errorf("файл больше %d МБ — правка из клиента не поддерживается", MaxWrite>>20)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxWrite+1))
	return b, st.Mode().Perm(), true, err
}

// opWrite записывает файл целиком: временный файл рядом, fsync, rename поверх.
// Читатель видит либо старое содержимое, либо новое, но не половину.
func (w *Worker) opWrite(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[writeArgs](args)
	if err != nil {
		return nil, err
	}
	if len(a.Content) > MaxWrite {
		return nil, fmt.Errorf("файл больше %d МБ", MaxWrite>>20)
	}
	rel, err := safepath.Clean(a.Path)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return nil, errors.New("не указан файл")
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

	// Перед заменой — ещё раз версия: агент мог записать файл, пока писали мы.
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
