package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ilguzin/laavu/internal/config"
)

// sslipSuffix: A-B-C-D.sslip.io and any name under it resolve to A.B.C.D, so a server
// without its own domain still gets a public name and a Let's Encrypt certificate.
const sslipSuffix = ".sslip.io"

// ErrNoAddress is returned for a first install without a way to reach the server.
var ErrNoAddress = errors.New(`how will you open Laavu? Passkeys need a host name with HTTPS, not a bare IP. Pass one of:
  --domain NAME --email EMAIL   your own domain (DNS: NAME and view.NAME point at the server)
  --sslip                       a free name from sslip.io for the server's public IPv4
  --tailscale --tailscale-authkey KEY --tailscale-hostname NAME
                                only from your tailnet, at https://NAME.<tailnet>.ts.net
  --host NAME                   test VM: ports 8443/8444, certificate from Caddy's own CA`)

// resolveAddress decides how the server is reached. Exactly one of the modes is chosen
// on the first install; a repeated install without flags keeps what laavud.toml says.
func resolveAddress(o *Options) error {
	n := 0
	for _, set := range []bool{o.Domain != "", o.Sslip, o.Tailscale, o.Host != ""} {
		if set {
			n++
		}
	}
	if n > 1 {
		return errors.New("--domain, --sslip, --tailscale and --host exclude each other")
	}
	o.explicit = n > 0
	if o.Tailscale && o.Domain == "" && o.Host == "" {
		// The tailnet address comes later from tailscale enable; until then, port mode on localhost.
		o.Host = "localhost"
	}
	if o.Sslip {
		ip, err := publicIPv4(o.PublicIP)
		if err != nil {
			return err
		}
		o.Domain = sslipName(ip)
		fmt.Fprintf(o.Out, "  server name %s (sslip.io)\n", o.Domain)
	}
	path := filepath.Join(config.Dir, "laavud.toml")
	cur, err := config.LoadDaemon(path)
	if err != nil {
		if n == 0 {
			return ErrNoAddress
		}
		return nil
	}
	// Already in the tailnet: keep its address, Caddy config and firewall rules.
	if st, ok := readTSState(); ok && st.ChangedURL && o.Domain == "" {
		o.ts = &st
		o.tsName = hostOfURL(cur.PublicURL)
		return nil
	}
	if n == 0 {
		h := hostOfURL(cur.PublicURL)
		if portsMode(cur) {
			o.Host, _, _ = strings.Cut(h, ":")
		} else {
			o.Domain = h
		}
	}
	return nil
}

func readTSState() (tsState, bool) {
	var st tsState
	b, err := os.ReadFile(tsStatePath())
	if err != nil || json.Unmarshal(b, &st) != nil {
		return st, false
	}
	return st, true
}

func sslipName(ip net.IP) string {
	return strings.ReplaceAll(ip.To4().String(), ".", "-") + sslipSuffix
}

// publicIPv4 returns the given address or the source address of the route to the internet.
// Nothing is sent: a UDP "connection" only picks the route.
func publicIPv4(given string) (net.IP, error) {
	var ip net.IP
	if given != "" {
		ip = net.ParseIP(given)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("--public-ip %s is not an IPv4 address", given)
		}
	} else {
		c, err := net.Dial("udp4", "1.1.1.1:53")
		if err != nil {
			return nil, fmt.Errorf("no IPv4 route to the internet (%v): use --domain or --tailscale", err)
		}
		ip = c.LocalAddr().(*net.UDPAddr).IP
		c.Close()
	}
	if !isPublicIPv4(ip) {
		return nil, fmt.Errorf("the server's address %s is not public (behind NAT?): pass its public IPv4 as --public-ip, or use --domain or --tailscale", ip)
	}
	return ip.To4(), nil
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func isPublicIPv4(ip net.IP) bool {
	return ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !cgnat.Contains(ip)
}

// errTailnetPasswords stops a tailnet install while ssh still takes passwords.
var errTailnetPasswords = errors.New(`ssh still accepts passwords, so the server is not moved into the tailnet. Nothing was installed.
Put your public key on the server and turn passwords off in the same run:
  sudo laavu install --owner-key "ssh-ed25519 AAAA..." --harden-ssh --tailscale ...
or install from your laptop with --ssh USER@IP, which sets up key login and turns passwords off itself.`)

// passwordLogin reports whether sshd accepts passwords (or keyboard-interactive, which asks for one).
func passwordLogin() (bool, error) {
	out, err := exec.Command("sshd", "-T").Output()
	if err != nil {
		return false, fmt.Errorf("sshd -T: %w", err)
	}
	return sshdTakesPasswords(string(out)), nil
}

func sshdTakesPasswords(conf string) bool {
	for _, l := range strings.Split(conf, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), " ")
		if (k == "passwordauthentication" || k == "kbdinteractiveauthentication") && v == "yes" {
			return true
		}
	}
	return false
}
