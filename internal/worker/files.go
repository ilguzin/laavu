package worker

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ilguzin/laavu/internal/safepath"
	"github.com/ilguzin/laavu/internal/wire"
)

func (w *Worker) openRoot(folder string) (*safepath.Root, error) {
	if err := w.Reserved.Workdir(folder); err != nil {
		return nil, fmt.Errorf("workspace %q: %w", folder, err)
	}
	return safepath.Open(w.Home, folder)
}

// Entry is a tree element.
type Entry struct {
	Name   string `json:"name"`
	Type   string `json:"type"` // dir | file | link | other
	Size   int64  `json:"size"`
	MTime  int64  `json:"mtime"`
	Hidden bool   `json:"hidden,omitempty"`
	Kind   string `json:"kind,omitempty"` // file kind for the viewer, by extension
}

type treeArgs struct {
	Folder string `json:"folder"`
	Path   string `json:"path"`
	Hidden bool   `json:"hidden"`
}

func (w *Worker) opTree(_ *Conn, args json.RawMessage) (any, error) {
	a, err := decode[treeArgs](args)
	if err != nil {
		return nil, err
	}
	r, err := w.openRoot(a.Folder)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if !a.Hidden && safepath.HasHidden(a.Path) {
		return nil, safepath.ErrHidden
	}
	d, err := r.Open(a.Path)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, e := range ents {
		hidden := strings.HasPrefix(e.Name(), ".")
		if hidden && !a.Hidden {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		t := "other"
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			t = "link"
		case info.IsDir():
			t = "dir"
		case info.Mode().IsRegular():
			t = "file"
		}
		en := Entry{Name: e.Name(), Type: t, Size: info.Size(), MTime: info.ModTime().Unix(), Hidden: hidden}
		if t != "dir" {
			en.Kind = KindByExt(e.Name())
		}
		out = append(out, en)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == "dir") != (out[j].Type == "dir") {
			return out[i].Type == "dir"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

var extKinds = map[string]string{
	".md": "markdown", ".markdown": "markdown",
	".html": "html", ".htm": "html",
	".svg": "svg",
	".png": "image", ".jpg": "image", ".jpeg": "image", ".gif": "image", ".webp": "image", ".avif": "image", ".bmp": "image", ".ico": "image",
	".pdf": "pdf",
	".zip": "binary", ".gz": "binary", ".tgz": "binary", ".xz": "binary", ".bz2": "binary", ".7z": "binary", ".rar": "binary",
	".exe": "binary", ".so": "binary", ".o": "binary", ".a": "binary", ".bin": "binary", ".db": "binary", ".sqlite": "binary",
	".docx": "binary", ".xlsx": "binary", ".pptx": "binary", ".doc": "binary", ".xls": "binary", ".odt": "binary",
	".mp3": "binary", ".mp4": "binary", ".mov": "binary", ".wav": "binary", ".ogg": "binary", ".webm": "binary",
	".woff": "binary", ".woff2": "binary", ".ttf": "binary", ".otf": "binary", ".class": "binary", ".jar": "binary",
}

var codeExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".mjs": true, ".ts": true, ".tsx": true, ".jsx": true, ".c": true, ".h": true,
	".cpp": true, ".cc": true, ".hpp": true, ".rs": true, ".java": true, ".kt": true, ".rb": true, ".php": true, ".sh": true,
	".bash": true, ".zsh": true, ".css": true, ".scss": true, ".json": true, ".yaml": true, ".yml": true, ".toml": true,
	".xml": true, ".sql": true, ".lua": true, ".swift": true, ".cs": true, ".tex": true, ".r": true, ".jl": true, ".ini": true,
	".conf": true, ".dockerfile": true, ".makefile": true, ".vue": true, ".svelte": true,
}

// KindByExt returns the file kind by extension; empty means an unknown extension.
func KindByExt(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if k, ok := extKinds[ext]; ok {
		return k
	}
	if codeExts[ext] {
		return "code"
	}
	switch ext {
	case ".txt", ".log", ".csv", ".tsv":
		return "text"
	}
	switch strings.ToLower(name) {
	case "makefile", "dockerfile", "license", "readme", ".gitignore", ".env":
		return "text"
	}
	return ""
}

// Sniff checks the first kilobytes: zero bytes or invalid UTF-8 mean binary.
func Sniff(head []byte) string {
	for _, b := range head {
		if b == 0 {
			return "binary"
		}
	}
	// A last character cut at the boundary is not an error.
	for i := 0; i < 4 && len(head) > 0 && !utf8.Valid(head); i++ {
		head = head[:len(head)-1]
	}
	if !utf8.Valid(head) {
		return "binary"
	}
	return "text"
}

// FileMeta is file information sent before the content.
type FileMeta struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	Kind  string `json:"kind"`
	Dir   bool   `json:"dir,omitempty"`
}

type readArgs struct {
	Folder string `json:"folder"`
	Path   string `json:"path"`
	// Visible is the preview origin mode: hidden files are never served.
	Visible bool  `json:"visible"`
	Max     int64 `json:"max"` // 0 means no limit
	// MetaOnly requests information only, without content.
	MetaOnly bool `json:"meta_only"`
}

// opRead: the response is FileMeta, then data frames and an end frame.
func (w *Worker) opRead(c *Conn, args json.RawMessage) (any, error) {
	a, err := decode[readArgs](args)
	if err != nil {
		return nil, err
	}
	r, err := w.openRoot(a.Folder)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var f *os.File
	if a.Visible {
		f, err = r.OpenVisible(a.Path)
	} else {
		f, err = r.Open(a.Path)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	clean, _ := safepath.Clean(a.Path)
	meta := FileMeta{Path: clean, Size: st.Size(), MTime: st.ModTime().Unix(), Dir: st.IsDir()}
	if st.IsDir() {
		meta.Kind = "dir"
		return meta, nil
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	br := bufio.NewReaderSize(f, 64<<10)
	meta.Kind = KindByExt(st.Name())
	if meta.Kind == "" || meta.Kind == "code" || meta.Kind == "text" || meta.Kind == "markdown" {
		head, _ := br.Peek(8192)
		if Sniff(head) == "binary" {
			meta.Kind = "binary"
		} else if meta.Kind == "" {
			meta.Kind = "text"
		}
	}
	if a.MetaOnly {
		return meta, nil
	}
	if err := c.Reply(meta, nil); err != nil {
		return nil, errStreamed
	}
	var src io.Reader = br
	if a.Max > 0 {
		src = io.LimitReader(br, a.Max)
	}
	buf := make([]byte, 256<<10)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if err := c.WriteFrame(wire.FrameData, buf[:n]); err != nil {
				return nil, errStreamed
			}
		}
		if rerr != nil {
			break
		}
	}
	c.WriteFrame(wire.FrameEnd, nil)
	return nil, errStreamed
}

type uploadArgs struct {
	Folder string `json:"folder"`
	Dir    string `json:"dir"` // empty means per .laavu-uploads, or the root
	Name   string `json:"name"`
	Size   int64  `json:"size"`
}

// UploadResult tells where the file was stored.
type UploadResult struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// UploadDir reads .laavu-uploads: a single line with a relative path.
func UploadDir(r *safepath.Root) string {
	cf, err := r.Open(".laavu-uploads")
	if err != nil {
		return "."
	}
	defer cf.Close()
	line, _ := bufio.NewReader(io.LimitReader(cf, 4096)).ReadString('\n')
	line = strings.TrimSpace(line)
	c, err := safepath.Clean(line)
	if err != nil || line == "" {
		return "."
	}
	return c
}

// opUpload: responds with the final path after receiving data frames and an end frame.
// First an empty "ready to receive" response, at the end a JSON frame with the result.
func (w *Worker) opUpload(c *Conn, args json.RawMessage) (any, error) {
	a, err := decode[uploadArgs](args)
	if err != nil {
		return nil, err
	}
	if a.Size > w.UploadLimit {
		return nil, fmt.Errorf("file larger than %d MB", w.UploadLimit>>20)
	}
	r, err := w.openRoot(a.Folder)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	dir := a.Dir
	if dir == "" {
		dir = UploadDir(r)
	}
	if dir, err = safepath.Clean(dir); err != nil {
		return nil, err
	}
	name := path.Base(strings.ReplaceAll(a.Name, "\\", "/"))
	if err := r.MkdirAll(dir); err != nil {
		return nil, err
	}
	f, rel, err := r.CreateExclusive(dir, name)
	if err != nil {
		return nil, err
	}
	if err := c.Reply(nil, nil); err != nil {
		f.Close()
		r.Remove(rel)
		return nil, errStreamed
	}
	var n int64
	fail := func(e error) (any, error) {
		f.Close()
		r.Remove(rel)
		b, _ := json.Marshal(map[string]string{"error": e.Error()})
		c.WriteFrame(wire.FrameJSON, b)
		return nil, errStreamed
	}
	for {
		t, p, err := c.ReadFrame()
		if err != nil {
			return fail(err)
		}
		if t == wire.FrameEnd {
			break
		}
		if t != wire.FrameData {
			continue
		}
		n += int64(len(p))
		if n > w.UploadLimit {
			return fail(fmt.Errorf("file larger than %d MB", w.UploadLimit>>20))
		}
		if _, err := f.Write(p); err != nil {
			return fail(err)
		}
	}
	if err := f.Close(); err != nil {
		r.Remove(rel)
		return fail(err)
	}
	b, _ := json.Marshal(UploadResult{Path: rel, Size: n})
	c.WriteFrame(wire.FrameJSON, b)
	return nil, errStreamed
}
