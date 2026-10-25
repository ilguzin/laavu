package worker

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ilguzin/laavu/internal/wire"
)

// Каталоги, за которыми не следим: шумные и тяжёлые.
var skipWatch = map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true, "target": true, ".next": true}

// maxWatches — предел числа каталогов под наблюдением на одну папку.
const maxWatches = 4000

// WatchEvent — изменившиеся каталоги рабочей папки (относительные пути).
type WatchEvent struct {
	Dirs  []string `json:"dirs"`
	Files []string `json:"files"`
}

// opWatch: ответ, затем кадры JSON с событиями, склеенными за 300 мс, пока демон
// не закроет соединение.
func (w *Worker) opWatch(c *Conn, args json.RawMessage) (any, error) {
	a, err := decode[struct {
		Folder string `json:"folder"`
	}](args)
	if err != nil {
		return nil, err
	}
	r, err := w.openRoot(a.Folder)
	if err != nil {
		return nil, err
	}
	root := r.Path()
	r.Close()
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	defer fw.Close()
	count := 0
	addTree := func(dir string) {
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if p != root && skipWatch[d.Name()] {
				return filepath.SkipDir
			}
			if count >= maxWatches {
				return filepath.SkipAll
			}
			if fw.Add(p) == nil {
				count++
			}
			return nil
		})
	}
	addTree(root)
	if err := c.Reply(nil, nil); err != nil {
		return nil, errStreamed
	}
	closed := make(chan struct{})
	go func() {
		// Любой кадр или обрыв со стороны демона — конец наблюдения.
		c.ReadFrame()
		close(closed)
	}()
	rel := func(p string) string {
		x, err := filepath.Rel(root, p)
		if err != nil {
			return "."
		}
		return filepath.ToSlash(x)
	}
	dirs, files := map[string]bool{}, map[string]bool{}
	var timer <-chan time.Time
	for {
		select {
		case <-closed:
			return nil, errStreamed
		case ev, ok := <-fw.Events:
			if !ok {
				return nil, errStreamed
			}
			if ev.Op&fsnotify.Create != 0 {
				if st, err := os.Lstat(ev.Name); err == nil && st.IsDir() {
					addTree(ev.Name)
				}
			}
			r := rel(ev.Name)
			if strings.HasPrefix(r, "..") {
				continue
			}
			dirs[filepath.ToSlash(filepath.Dir(r))] = true
			files[r] = true
			if timer == nil {
				timer = time.After(300 * time.Millisecond)
			}
		case <-fw.Errors:
		case <-timer:
			timer = nil
			ev := WatchEvent{Dirs: keys(dirs), Files: keys(files)}
			dirs, files = map[string]bool{}, map[string]bool{}
			b, _ := json.Marshal(ev)
			if err := c.WriteFrame(wire.FrameJSON, b); err != nil {
				return nil, errStreamed
			}
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
