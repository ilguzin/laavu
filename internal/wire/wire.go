// Package wire is the protocol between the daemon and an agent worker over a unix socket.
//
// A connection starts with a request: one JSON line {"op": ..., "args": {...}}.
// The reply is one JSON line {"ok": true, "data": ...} or {"ok": false, "error": "..."}.
// For streaming operations (attach, read, upload, watch), after a successful reply
// the connection switches to frames: [type 1 byte][length 4 bytes BE][data].
package wire

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Frame types.
const (
	FrameData   byte = 'd' // terminal or file bytes
	FrameResize byte = 'r' // 4 bytes: cols, rows (uint16 BE)
	FrameJSON   byte = 'j' // event or final reply
	FrameEnd    byte = 'e' // end of stream from this side
)

// MaxFrame is the frame size limit.
const MaxFrame = 1 << 20

type Request struct {
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

type Response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Conn is a connection with buffered reads.
type Conn struct {
	net.Conn
	r *bufio.Reader
}

func NewConn(c net.Conn) *Conn { return &Conn{Conn: c, r: bufio.NewReaderSize(c, 64<<10)} }

// Dial opens a connection to a worker.
func Dial(path string) (*Conn, error) {
	c, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return nil, err
	}
	return NewConn(c), nil
}

func (c *Conn) WriteLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.Write(append(b, '\n'))
	return err
}

func (c *Conn) ReadLine(v any) error {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// Send sends a request and reads the reply; data is decoded into out if it is not nil.
func (c *Conn) Send(op string, args, out any) error {
	req := Request{Op: op}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		req.Args = b
	}
	if err := c.WriteLine(req); err != nil {
		return err
	}
	var resp Response
	if err := c.ReadLine(&resp); err != nil {
		return err
	}
	if !resp.OK {
		return &RemoteError{resp.Error}
	}
	if out != nil && len(resp.Data) > 0 {
		return json.Unmarshal(resp.Data, out)
	}
	return nil
}

// RemoteError is an error returned by the worker.
type RemoteError struct{ Msg string }

func (e *RemoteError) Error() string { return e.Msg }

// Reply answers a request.
func (c *Conn) Reply(data any, err error) error {
	if err != nil {
		return c.WriteLine(Response{OK: false, Error: err.Error()})
	}
	resp := Response{OK: true}
	if data != nil {
		b, e := json.Marshal(data)
		if e != nil {
			return e
		}
		resp.Data = b
	}
	return c.WriteLine(resp)
}

// WriteFrame writes a frame. The caller must serialize concurrent calls.
func (c *Conn) WriteFrame(typ byte, p []byte) error { return WriteFrame(c.Conn, typ, p) }

// ReadFrame reads a frame.
func (c *Conn) ReadFrame() (byte, []byte, error) { return ReadFrame(c.r) }

func WriteFrame(w io.Writer, typ byte, p []byte) error {
	if len(p) > MaxFrame {
		return errors.New("frame too large")
	}
	hdr := make([]byte, 5, 5+len(p))
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
	_, err := w.Write(append(hdr, p...))
	return err
}

func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds the limit", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return hdr[0], p, nil
}

// ResizePayload encodes the terminal size.
func ResizePayload(cols, rows int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b, uint16(cols))
	binary.BigEndian.PutUint16(b[2:], uint16(rows))
	return b
}

// ParseResize decodes the terminal size.
func ParseResize(b []byte) (cols, rows int, ok bool) {
	if len(b) != 4 {
		return 0, 0, false
	}
	return int(binary.BigEndian.Uint16(b)), int(binary.BigEndian.Uint16(b[2:])), true
}
