// Package link is the contract between a Laavu server and the management console
// (docs/link.md): messages, signatures, operations and rights.
//
// The server side has two processes. `laavu link run` runs as the unprivileged
// laavu-link user and holds the WebSocket to the console. It never executes jobs:
// it hands each signed job to the root helper (`laavu link helper`) over a unix
// socket, and the helper checks the console signature, the target server, the
// time window, replay and the right before running laavu.
package link

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Contract is the contract version this code speaks.
const Contract = 1

// MaxMessage limits one WebSocket message.
const MaxMessage = 256 << 10

// Message types.
const (
	TypeHello   = "hello"   // server → console, first message
	TypeWelcome = "welcome" // console → server, signed by the console
	TypeAuth    = "auth"    // server → console, signed by the server
	TypePending = "pending" // console → server: binding code to show the owner
	TypeBound   = "bound"   // console → server, signed: the owner confirmed the binding
	TypeJob     = "job"     // console → server, signed by the console
	TypeResult  = "result"  // server → console
	TypeReport  = "report"  // server → console
	TypeRights  = "rights"  // server → console
	TypeUnlink  = "unlink"  // server → console: the owner took control back
	TypeError   = "error"   // either side, before closing
	TypeSetIP   = "dns.set_ip"
)

// Msg is one message on the wire. Signed messages carry Body (base64 of the
// JSON payload) and Sig; plain ones carry their fields in Data.
type Msg struct {
	Type string          `json:"type"`
	Body string          `json:"body,omitempty"`
	Sig  string          `json:"sig,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Hello is the server's first message.
type Hello struct {
	Contracts []int  `json:"contracts"`
	ServerKey string `json:"server_key"`
	Nonce     string `json:"nonce"`
	Laavu     string `json:"laavu"`
	Bind      bool   `json:"bind,omitempty"` // not bound yet: the console answers with pending or bound
	Code      string `json:"code,omitempty"` // a binding code the console issued to the owner
}

// Welcome is signed by the console.
type Welcome struct {
	Contract    int    `json:"contract"`
	ServerKey   string `json:"server_key"`
	ServerNonce string `json:"server_nonce"`
	ConsoleKey  string `json:"console_key"`
	Nonce       string `json:"nonce"`
	Time        int64  `json:"time"`
}

// Auth is signed by the server.
type Auth struct {
	ConsoleKey   string          `json:"console_key"`
	ConsoleNonce string          `json:"console_nonce"`
	Laavud       string          `json:"laavud"`
	Laavu        string          `json:"laavu"`
	Rights       map[string]bool `json:"rights,omitempty"`
}

// Pending carries the binding code the owner enters in the console.
type Pending struct {
	Code    string `json:"code"`
	Expires int64  `json:"expires"`
}

// Bound is signed by the console.
type Bound struct {
	ServerKey string `json:"server_key"`
	Time      int64  `json:"time"`
	// Account names the console account the server is now bound to, so the
	// owner sees it on the server: a code typed in by someone else shows up here.
	Account string `json:"account,omitempty"`
}

// Job is signed by the console.
type Job struct {
	ID        string          `json:"id"`
	ServerKey string          `json:"server_key"`
	Op        string          `json:"op"`
	Params    json.RawMessage `json:"params,omitempty"`
	NotBefore int64           `json:"not_before"`
	Expires   int64           `json:"expires"`
}

// Result statuses.
const (
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusRefused = "refused" // bad signature, wrong server, right is off, unknown operation
	StatusExpired = "expired"
)

// Result is the outcome of a job.
type Result struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Code   int    `json:"code"`
	Output string `json:"output,omitempty"`
}

// MaxOutput limits the output sent back with a result.
const MaxOutput = 16 << 10

// SetIP is signed by the server: the only way to move its name to a new address.
type SetIP struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
	Time int64  `json:"time"`
}

// MaxSkew is how far a signed time may be from the server clock.
const MaxSkew = 5 * time.Minute

// MaxJobLife is the longest window between not_before and expires the server accepts.
const MaxJobLife = 7 * 24 * time.Hour

func signedBytes(typ string, body []byte) []byte {
	return append([]byte("laavu-link/1 "+typ+"\n"), body...)
}

// Sign builds a signed message. The type is part of what is signed, so a
// signature for one message type cannot be replayed as another.
func Sign(key ed25519.PrivateKey, typ string, v any) (Msg, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return Msg{}, err
	}
	sig := ed25519.Sign(key, signedBytes(typ, body))
	return Msg{Type: typ, Body: base64.StdEncoding.EncodeToString(body), Sig: base64.StdEncoding.EncodeToString(sig)}, nil
}

// Open checks the signature of a signed message and decodes its body into v.
func Open(pub ed25519.PublicKey, m Msg, v any) error {
	body, err := base64.StdEncoding.DecodeString(m.Body)
	if err != nil {
		return errors.New("body is not base64")
	}
	sig, err := base64.StdEncoding.DecodeString(m.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("bad signature encoding")
	}
	if !ed25519.Verify(pub, signedBytes(m.Type, body), sig) {
		return errors.New("signature does not match")
	}
	return json.Unmarshal(body, v)
}

// Peek decodes the body of a signed message without checking the signature,
// for when the key to check it with is inside the body (welcome while binding).
func Peek(m Msg, v any) error {
	body, err := base64.StdEncoding.DecodeString(m.Body)
	if err != nil {
		return errors.New("body is not base64")
	}
	return json.Unmarshal(body, v)
}

// Plain builds an unsigned message.
func Plain(typ string, v any) (Msg, error) {
	b, err := json.Marshal(v)
	return Msg{Type: typ, Data: b}, err
}

// EncodeKey and ParseKey use standard base64, like the release key.
func EncodeKey(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

func ParseKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("invalid ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

// Fingerprint is what the owner compares between the server and the console.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	h := hex.EncodeToString(sum[:8])
	return fmt.Sprintf("%s-%s-%s-%s", h[0:4], h[4:8], h[8:12], h[12:16])
}

// Nonce returns 16 random bytes in base64.
func Nonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// CheckTime reports whether a signed time is close enough to now.
func CheckTime(t int64, now time.Time) error {
	d := now.Sub(time.Unix(t, 0))
	if d > MaxSkew || d < -MaxSkew {
		return fmt.Errorf("time differs from the server clock by %s", d.Round(time.Second))
	}
	return nil
}
