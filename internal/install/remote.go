package install

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ilguzin/laavu/internal/release"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// Install from a laptop: laavu install --ssh user@IP. Built-in SSH client,
// host fingerprint on first connection, password kept only in process memory.

// RemoteOptions are the settings used only for installing over SSH.
type RemoteOptions struct {
	Target   string // user@host[:port]
	Identity string // private key for login; "none" means password only
	Yes      bool   // accept the host fingerprint without asking (tests)
	// Release is a directory or https URL of a signed release. Empty means the binaries
	// next to the running laavu (developer build, unsigned).
	Release string
	// OwnerPub is the owner's public key; empty means ~/.ssh/id_ed25519.pub or a new key.
	OwnerPub string
	// Sslip: the server is named after the target address if it is a public IPv4.
	Sslip bool
	// Tailscale: after ssh is hardened, the server moves into the tailnet (tailscale enable --mode only).
	Tailscale         bool
	TailscaleAuthKey  string
	TailscaleHostname string
	Stdin             io.Reader
	Out               io.Writer
}

func knownHostsPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "laavu", "known_hosts")
}

func splitTarget(t string) (user, addr string, err error) {
	user, host, ok := strings.Cut(t, "@")
	if !ok || user == "" || host == "" {
		return "", "", errors.New("address must look like user@IP[:port]")
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "22")
	}
	return user, host, nil
}

// hostKeyCallback shows the fingerprint on first connection and remembers it.
// If the host key changes later, it refuses: that is either a reinstall or an attack.
func hostKeyCallback(ro *RemoteOptions, in *bufio.Reader) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		line := hostname + " " + fp
		path := knownHostsPath()
		if b, err := os.ReadFile(path); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				h, f, ok := strings.Cut(l, " ")
				if !ok || h != hostname {
					continue
				}
				if f == fp {
					return nil
				}
				return fmt.Errorf("host key for %s changed (was %s, now %s). If the server was reinstalled, remove the line from %s", hostname, f, fp, path)
			}
		}
		fmt.Fprintf(ro.Out, "First connection to %s.\nServer key fingerprint: %s\nCompare it with your hosting provider's panel.\n", hostname, fp)
		if !ro.Yes {
			fmt.Fprint(ro.Out, "Continue? [y/N] ")
			ans, _ := in.ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
				return errors.New("connection cancelled")
			}
		}
		os.MkdirAll(filepath.Dir(path), 0o700)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintln(f, line)
		return err
	}
}

func readPassword(ro *RemoteOptions, in *bufio.Reader, prompt string) (string, error) {
	fmt.Fprint(ro.Out, prompt)
	if f, ok := ro.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(ro.Out)
		return string(b), err
	}
	// Not a terminal: the password is one line from stdin (for automated tests).
	s, err := in.ReadString('\n')
	fmt.Fprintln(ro.Out)
	return strings.TrimRight(s, "\r\n"), err
}

func authMethods(ro *RemoteOptions, in *bufio.Reader, user, addr string) []ssh.AuthMethod {
	var methods []ssh.AuthMethod
	keyPath := ro.Identity
	if keyPath == "" {
		home, _ := os.UserHomeDir()
		keyPath = filepath.Join(home, ".ssh", "id_ed25519")
	}
	if keyPath != "none" {
		if b, err := os.ReadFile(keyPath); err == nil {
			if signer, err := ssh.ParsePrivateKey(b); err == nil {
				methods = append(methods, ssh.PublicKeys(signer))
			}
		}
	}
	var pw string
	asked := false
	ask := func() (string, error) {
		if !asked {
			asked = true
			var err error
			pw, err = readPassword(ro, in, fmt.Sprintf("Password for %s@%s: ", user, addr))
			if err != nil {
				return "", err
			}
		}
		return pw, nil
	}
	methods = append(methods, ssh.PasswordCallback(ask))
	methods = append(methods, ssh.KeyboardInteractive(func(name, instr string, qs []string, echos []bool) ([]string, error) {
		ans := make([]string, len(qs))
		for i := range qs {
			p, err := ask()
			if err != nil {
				return nil, err
			}
			ans[i] = p
		}
		return ans, nil
	}))
	return methods
}

// Dial connects to the server.
func Dial(ro *RemoteOptions, in *bufio.Reader) (*ssh.Client, string, error) {
	user, addr, err := splitTarget(ro.Target)
	if err != nil {
		return nil, "", err
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods(ro, in, user, addr),
		HostKeyCallback: hostKeyCallback(ro, in),
		Timeout:         15 * time.Second,
	}
	c, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, user, fmt.Errorf("ssh %s: %w", addr, err)
	}
	return c, user, nil
}

// lockedWriter is a shared writer for the session's stdout and stderr. Without it two
// copiers would write into one bytes.Buffer at the same time (via its ReadFrom)
// and lose output.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// remoteRun runs a command and copies its output to out.
func remoteRun(c *ssh.Client, cmd string, stdin io.Reader, out io.Writer) error {
	s, err := c.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	s.Stdin = stdin
	lw := &lockedWriter{w: out}
	s.Stdout = lw
	s.Stderr = lw
	return s.Run(cmd)
}

func remoteOutput(c *ssh.Client, cmd string) (string, error) {
	var b bytes.Buffer
	err := remoteRun(c, cmd, nil, &b)
	return strings.TrimSpace(b.String()), err
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ownerPublicKey returns the owner's public key: the given one, ~/.ssh/id_ed25519.pub or a new one.
func ownerPublicKey(ro *RemoteOptions, host string) (string, string, error) {
	if ro.OwnerPub != "" {
		b, err := os.ReadFile(ro.OwnerPub)
		if err != nil {
			return "", "", err
		}
		return strings.TrimSpace(string(b)), strings.TrimSuffix(ro.OwnerPub, ".pub"), nil
	}
	home, _ := os.UserHomeDir()
	def := filepath.Join(home, ".ssh", "id_ed25519")
	if b, err := os.ReadFile(def + ".pub"); err == nil {
		return strings.TrimSpace(string(b)), def, nil
	}
	// No key: create a dedicated one for this server.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "laavu "+host)
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(home, ".ssh", "laavu_"+strings.NewReplacer(":", "_", ".", "_").Replace(host))
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		return "", "", err
	}
	sp, _ := ssh.NewPublicKey(pub)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " laavu"
	os.WriteFile(path+".pub", []byte(line+"\n"), 0o644)
	return line, path, nil
}

var archMap = map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}

// RemoteInstall implements laavu install --ssh.
func RemoteInstall(ro RemoteOptions, o Options, passArgs []string) error {
	if ro.Out == nil {
		ro.Out = os.Stdout
	}
	if ro.Stdin == nil {
		ro.Stdin = os.Stdin
	}
	in := bufio.NewReader(ro.Stdin)
	out := ro.Out

	// 1. The release signature is verified on the laptop before copying.
	var ver *release.Verified
	if ro.Release != "" {
		pub, err := release.PinnedKey()
		if err != nil {
			return err
		}
		if ver, err = release.Verify(release.Source(ro.Release), pub); err != nil {
			return err
		}
		fmt.Fprintf(out, "Release %s: signature verified.\n", ver.Manifest.Version)
	}

	c, user, err := Dial(&ro, in)
	if err != nil {
		return err
	}
	defer c.Close()
	_, addr, _ := splitTarget(ro.Target)
	host, _, _ := net.SplitHostPort(addr)
	fmt.Fprintf(out, "Connected to %s.\n", addr)

	sudo := ""
	if user != "root" {
		if _, err := remoteOutput(c, "sudo -n true"); err != nil {
			return errors.New("root or a user with passwordless sudo required")
		}
		sudo = "sudo -n "
	}
	arch, err := remoteOutput(c, "uname -m")
	if err != nil {
		return err
	}
	goarch := archMap[arch]
	if goarch == "" {
		return fmt.Errorf("server architecture %s is not supported", arch)
	}

	// 2. Binaries: from the signed release or next to the running laavu.
	local := map[string]string{}
	tmpDir, err := os.MkdirTemp("", "laavu-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	for _, name := range []string{"laavu", "laavud"} {
		if ver != nil {
			p := filepath.Join(tmpDir, name)
			if err := ver.Fetch(name+"-linux-"+goarch, p); err != nil {
				return err
			}
			local[name] = p
			continue
		}
		if runtime.GOOS != "linux" || runtime.GOARCH != goarch {
			return fmt.Errorf("this laavu build is for %s/%s, but the server is linux/%s: pass --release", runtime.GOOS, runtime.GOARCH, goarch)
		}
		self, _ := os.Executable()
		local[name] = filepath.Join(filepath.Dir(self), name)
	}
	if ver == nil {
		fmt.Fprintln(out, "Warning: installing a local build without a release signature.")
	}
	remoteDir, err := remoteOutput(c, "mktemp -d /tmp/laavu-install.XXXXXX")
	if err != nil {
		return err
	}
	defer remoteRun(c, sudo+"rm -rf "+shellQuote(remoteDir), nil, io.Discard)
	for name, p := range local {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		err = remoteRun(c, "cat > "+shellQuote(remoteDir+"/"+name)+" && chmod 755 "+shellQuote(remoteDir+"/"+name), f, out)
		f.Close()
		if err != nil {
			return fmt.Errorf("copying %s: %w", name, err)
		}
	}
	// 3. The second signature check runs on the server, with its own laavu.
	if ver != nil {
		for _, f := range []string{release.ManifestName, release.SignatureName} {
			b, err := release.Source(ro.Release).ReadFile(f)
			if err != nil {
				return err
			}
			if err := remoteRun(c, "cat > "+shellQuote(remoteDir+"/"+f), bytes.NewReader(b), out); err != nil {
				return err
			}
		}
		key, _ := release.PinnedKeyString()
		cmd := fmt.Sprintf("%s/laavu release verify --dir %s --key %s --file laavud-linux-%s=%s/laavud --file laavu-linux-%s=%s/laavu",
			remoteDir, remoteDir, shellQuote(key), goarch, remoteDir, goarch, remoteDir)
		if err := remoteRun(c, cmd, nil, out); err != nil {
			return fmt.Errorf("signature check on the server: %w", err)
		}
	}

	// 4. Installation; the owner key goes in before passwords are disabled.
	ownerPub, ownerKeyPath, err := ownerPublicKey(&ro, host)
	if err != nil {
		return err
	}
	args := append([]string{}, passArgs...)
	if ip := net.ParseIP(host); ro.Sslip && ip != nil && isPublicIPv4(ip) {
		// The server may only see a private address (NAT), the laptop knows the public one.
		args = append(args, "--public-ip", host)
	}
	args = append(args, "--owner-key", ownerPub)
	if user != "root" {
		args = append(args, "--owner-user", user)
	}
	if k, err := release.PinnedKeyString(); err == nil {
		args = append(args, "--release-key", k)
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	fmt.Fprintln(out, "Installing Laavu on the server...")
	var logBuf bytes.Buffer
	if err := remoteRun(c, sudo+remoteDir+"/laavu install "+strings.Join(quoted, " "), nil, io.MultiWriter(out, &logBuf)); err != nil {
		return fmt.Errorf("installation on the server: %w", err)
	}

	// 5. Key login is checked over a new connection; only then are passwords disabled.
	signerB, err := os.ReadFile(ownerKeyPath)
	if err != nil {
		return fmt.Errorf("owner private key %s: %w", ownerKeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(signerB)
	if err != nil {
		fmt.Fprintf(out, "Key %s is passphrase-protected, so key login cannot be checked; password login is left enabled.\n", ownerKeyPath)
		return noTailnetWithPasswords(&ro, ownerPub)
	}
	kc, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback(&RemoteOptions{Yes: true, Out: io.Discard}, in), Timeout: 15 * time.Second})
	if err != nil {
		fmt.Fprintf(out, "Key login failed (%v); password login is left enabled.\n", err)
		return noTailnetWithPasswords(&ro, ownerPub)
	}
	defer kc.Close()
	hargs := []string{"--owner-key", shellQuote(ownerPub)}
	if user != "root" {
		hargs = append(hargs, "--owner-user", shellQuote(user))
	}
	err = remoteRun(kc, sudo+"/usr/local/bin/laavu harden-ssh "+strings.Join(hargs, " "), nil, io.Discard)
	if err != nil {
		return fmt.Errorf("disabling password login: %w", err)
	}
	login := fmt.Sprintf("ssh -i %s %s@", ownerKeyPath, user)
	if ro.Tailscale {
		return remoteTailnet(&ro, kc, sudo, out, login)
	}
	fmt.Fprintf(out, "\nServer login is now by key only: %s%s\n", login, host)
	return nil
}

// noTailnetWithPasswords stops a tailnet install when passwords could not be turned off:
// the server stays installed on localhost, and the owner finishes the switch by hand.
func noTailnetWithPasswords(ro *RemoteOptions, ownerPub string) error {
	if !ro.Tailscale {
		return nil
	}
	return fmt.Errorf(`ssh still accepts passwords, so the server was not moved into the tailnet.
Laavu is installed but not reachable yet. Make key login work (a key without a passphrase,
or one loaded into ssh-agent), then on the server:
  sudo laavu harden-ssh --owner-key %s
  sudo laavu tailscale enable --mode only --authkey KEY --hostname NAME`, shellQuote(ownerPub))
}

// remoteTailnet moves the installed server into the tailnet over an open connection:
// once the firewall closes public ssh, only this connection still works.
func remoteTailnet(ro *RemoteOptions, c *ssh.Client, sudo string, out io.Writer, login string) error {
	if !ro.Tailscale {
		return nil
	}
	fmt.Fprintln(out, "Moving the server into the tailnet...")
	cmd := sudo + "/usr/local/bin/laavu tailscale enable --mode only"
	if ro.TailscaleAuthKey != "" {
		cmd += " --authkey " + shellQuote(ro.TailscaleAuthKey)
	}
	if ro.TailscaleHostname != "" {
		cmd += " --hostname " + shellQuote(ro.TailscaleHostname)
	}
	if err := remoteRun(c, cmd, nil, out); err != nil {
		return fmt.Errorf("tailnet: %w (Laavu is installed; retry on the server: sudo laavu tailscale enable --mode only)", err)
	}
	if st, err := remoteOutput(c, sudo+"tailscale status --json"); err == nil {
		if name, err := parseTSName([]byte(st)); err == nil {
			fmt.Fprintf(out, "\nThe server is now reachable only from the tailnet. Login: %s%s\n", login, name)
		}
	}
	return nil
}
