package link

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// HelperRequest is one line from the link process to the root helper.
type HelperRequest struct {
	Op  string `json:"op"` // job or report
	Job *Msg   `json:"job,omitempty"`
}

// HelperResponse is one line back.
type HelperResponse struct {
	Result *Result `json:"result,omitempty"`
	Report *Report `json:"report,omitempty"`
	Error  string  `json:"error,omitempty"`
}

// Helper is the root side. It trusts nothing the link process says: every job
// is checked against the console key that root pinned in ConfigFile.
type Helper struct {
	// Peer is the uid allowed to connect besides root.
	Peer int
	// Exec runs laavu with the given arguments and returns its output and exit code.
	Exec func(args []string) (string, int)
	// Report builds the server report.
	Report func() *Report
	// Agents lists agent names in the order the report lists them, so job output
	// gets the same agent-N aliases as the report when the names right is off.
	Agents func() []string
	// Unlink removes the binding when the console asks for it.
	Unlink func() error
	// Log records each job in the operations log.
	Log func(format string, a ...any)
	Now func() time.Time

	mu sync.Mutex // one job at a time: updates must not overlap
}

// HandleJob checks and runs one signed job.
func (h *Helper) HandleJob(m Msg) Result {
	// The id of a job that fails the checks is unverified; it only lets the
	// console match the refusal to what it sent.
	var unverified Job
	Peek(m, &unverified)
	refuse := func(id, why string) Result { return Result{ID: id, Status: StatusRefused, Code: -1, Output: why} }
	if m.Type != TypeJob {
		return refuse(unverified.ID, "not a job")
	}
	cfg, err := LoadConfig()
	if err != nil {
		return refuse(unverified.ID, "server is not linked")
	}
	if cfg.Unlinking {
		return refuse(unverified.ID, "server is unlinking")
	}
	ck, err := ParseKey(cfg.ConsoleKey)
	if err != nil {
		return refuse(unverified.ID, "bad console key in "+ConfigFile)
	}
	var j Job
	if err := Open(ck, m, &j); err != nil {
		return refuse(unverified.ID, "job: "+err.Error())
	}
	if j.ServerKey != cfg.ServerKey {
		return refuse(j.ID, "job is for another server")
	}
	if j.ID == "" || len(j.ID) > 64 {
		return refuse(j.ID, "bad job id")
	}
	now := h.Now()
	if j.Expires-j.NotBefore > int64(MaxJobLife/time.Second) || j.Expires < j.NotBefore {
		return refuse(j.ID, "job window is too long")
	}
	if now.Unix() > j.Expires {
		return Result{ID: j.ID, Status: StatusExpired, Code: -1}
	}
	if now.Add(MaxSkew).Unix() < j.NotBefore {
		return refuse(j.ID, "job is not due yet")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := markSeen(j.ID, j.Expires, now); err != nil {
		return refuse(j.ID, err.Error())
	}
	h.Log("link job %s %s", j.ID, j.Op)
	if j.Op == OpUnlink {
		if err := h.Unlink(); err != nil {
			return Result{ID: j.ID, Status: StatusFailed, Code: 1, Output: err.Error()}
		}
		return Result{ID: j.ID, Status: StatusDone}
	}
	op, ok := Ops[j.Op]
	if !ok {
		return refuse(j.ID, "unknown operation "+j.Op)
	}
	if !cfg.Allowed(op.Right) {
		return refuse(j.ID, "right "+op.Right+" is off on this server")
	}
	args, err := op.Args(j.Params)
	if err != nil {
		return refuse(j.ID, err.Error())
	}
	if j.Op == "check" {
		b, _ := json.Marshal(Redact(h.Report(), cfg.Allowed(RightNames)))
		return Result{ID: j.ID, Status: StatusDone, Output: string(b)}
	}
	out, code := h.Exec(args)
	if !cfg.Allowed(RightNames) {
		var names []string
		if h.Agents != nil {
			names = h.Agents()
		}
		out = redactText(redactNames(out, names))
	}
	res := Result{ID: j.ID, Status: StatusDone, Code: code, Output: truncate(out)}
	if code != 0 {
		res.Status = StatusFailed
	}
	return res
}

func truncate(s string) string {
	if len(s) <= MaxOutput {
		return s
	}
	return "…\n" + s[len(s)-MaxOutput:]
}

// markSeen refuses a job id that already ran and remembers it until it expires.
func markSeen(id string, expires int64, now time.Time) error {
	seen := map[string]int64{}
	if b, err := os.ReadFile(SeenFile); err == nil {
		json.Unmarshal(b, &seen)
	}
	for k, exp := range seen {
		if exp < now.Unix() {
			delete(seen, k)
		}
	}
	if _, dup := seen[id]; dup {
		return errors.New("job already ran")
	}
	seen[id] = expires
	b, _ := json.Marshal(seen)
	// Not in laavud's or laavu-link's state directory: their owners could delete the list.
	os.MkdirAll(filepath.Dir(SeenFile), 0o700)
	tmp := SeenFile + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("replay list: %w", err)
	}
	return os.Rename(tmp, SeenFile)
}

// Redact hides agent names unless the names right is on. The console sees how
// many agents there are and how they are doing, not what they are called.
func Redact(r *Report, names bool) *Report {
	if names {
		return r
	}
	list := make([]string, len(r.Agents))
	for i := range r.Agents {
		list[i] = r.Agents[i].Name
		r.Agents[i].Name = fmt.Sprintf("agent-%d", i+1)
		r.Agents[i].Ports = ""
	}
	for i := range r.Problems {
		r.Problems[i] = redactNames(r.Problems[i], list)
	}
	// Dev-server subdomains (p3000-name.view...) carry agent names into certificates.
	for i := range r.Certs {
		r.Certs[i].Name = redactNames(r.Certs[i].Name, list)
	}
	return r
}

// redactNames replaces each agent name with agent-N, N being its place in names.
// A name counts wherever it is not glued to other letters or digits, so the
// p3000-name.view subdomain is caught too. Longer names go first, and the
// aliases go in last, so a name like "agent" cannot hit an alias.
func redactNames(s string, names []string) string {
	order := make([]int, len(names))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return len(names[order[a]]) > len(names[order[b]]) })
	for _, i := range order {
		if names[i] == "" {
			continue
		}
		re := regexp.MustCompile(`(^|[^a-z0-9])` + regexp.QuoteMeta(names[i]) + `($|[^a-z0-9])`)
		mark := fmt.Sprintf("\x00%d\x00", i)
		// Twice: a match takes the character after it, so "a a" needs a second pass.
		for range 2 {
			s = re.ReplaceAllString(s, "${1}"+mark+"${2}")
		}
	}
	for i := range names {
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00%d\x00", i), fmt.Sprintf("agent-%d", i+1))
	}
	return s
}

// agentUserRe matches agent users (lv-name) and agent homes in command output.
var agentUserRe = regexp.MustCompile(`lv-[a-z][a-z0-9-]*|/srv/laavu/agents/[a-z][a-z0-9-]*`)

func redactText(s string) string {
	return agentUserRe.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "lv-") {
			return "lv-…"
		}
		return "/srv/laavu/agents/…"
	})
}

// Serve accepts connections until no request arrives for idle. The socket unit
// starts the helper again on the next connection, so after laavu update the next
// job already runs the new binary.
func (h *Helper) Serve(l net.Listener, idle time.Duration) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	active := make(chan struct{}, 1)
	go func() {
		t := time.NewTimer(idle)
		for {
			select {
			case <-active:
				t.Reset(idle)
			case <-t.C:
				h.mu.Lock() // do not exit in the middle of a job
				l.Close()
				h.mu.Unlock()
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case active <- struct{}{}:
		default:
		}
		go h.serveConn(c, active)
	}
}

func (h *Helper) serveConn(c net.Conn, active chan struct{}) {
	defer c.Close()
	if uc, ok := c.(*net.UnixConn); ok && !h.peerOK(uc) {
		return
	}
	r := bufio.NewReader(c)
	enc := json.NewEncoder(c)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		select {
		case active <- struct{}{}:
		default:
		}
		var req HelperRequest
		var resp HelperResponse
		if err := json.Unmarshal(line, &req); err != nil {
			resp.Error = "bad request"
		} else {
			resp = h.handle(req)
		}
		if enc.Encode(resp) != nil {
			return
		}
	}
}

func (h *Helper) handle(req HelperRequest) HelperResponse {
	switch req.Op {
	case "job":
		if req.Job == nil {
			return HelperResponse{Error: "no job"}
		}
		res := h.HandleJob(*req.Job)
		return HelperResponse{Result: &res}
	case "report":
		cfg, err := LoadConfig()
		if err != nil {
			return HelperResponse{Error: "server is not linked"}
		}
		if !cfg.Allowed(RightMonitoring) {
			return HelperResponse{}
		}
		return HelperResponse{Report: Redact(h.Report(), cfg.Allowed(RightNames))}
	}
	return HelperResponse{Error: "unknown op"}
}

func (h *Helper) peerOK(c *net.UnixConn) bool {
	uid, ok := peerUID(c)
	return ok && (uid == 0 || uid == h.Peer)
}

// ExecLaavu runs the installed laavu. The arguments come from Ops, never from the console as text.
func ExecLaavu(bin string, timeout time.Duration) func([]string) (string, int) {
	return func(args []string) (string, int) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			code = 1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
			if ctx.Err() != nil {
				out = append(out, []byte("\ntimed out")...)
			}
		}
		return string(out), code
	}
}

// ActivatedListener returns the socket passed by systemd, or nil.
func ActivatedListener() (net.Listener, error) {
	if os.Getenv("LISTEN_PID") != fmt.Sprint(os.Getpid()) || os.Getenv("LISTEN_FDS") != "1" {
		return nil, nil
	}
	f := os.NewFile(3, "link-helper.sock")
	defer f.Close()
	return net.FileListener(f)
}
