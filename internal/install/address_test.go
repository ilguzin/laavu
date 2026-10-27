package install

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ilguzin/laavu/internal/config"
)

func TestSslip(t *testing.T) {
	if n := sslipName(net.ParseIP("158.255.4.142")); n != "158-255-4-142.sslip.io" {
		t.Fatal(n)
	}
	for ip, public := range map[string]bool{
		"158.255.4.142": true, "10.0.0.5": false, "192.168.1.2": false, "172.16.3.4": false,
		"100.101.102.103": false, "127.0.0.1": false, "169.254.1.1": false, "2001:db8::1": false,
	} {
		if isPublicIPv4(net.ParseIP(ip)) != public {
			t.Errorf("%s: public should be %v", ip, public)
		}
	}
	if _, err := publicIPv4("10.1.2.3"); err == nil || !strings.Contains(err.Error(), "--public-ip") {
		t.Fatalf("private address: %v", err)
	}
	if ip, err := publicIPv4("158.255.4.142"); err != nil || ip.String() != "158.255.4.142" {
		t.Fatal(ip, err)
	}
}

func TestResolveAddress(t *testing.T) {
	if _, err := os.Stat(filepath.Join(config.Dir, "laavud.toml")); err == nil {
		t.Skip("a Laavu server is installed on this machine")
	}
	if err := resolveAddress(&Options{Out: io.Discard}); !errors.Is(err, ErrNoAddress) {
		t.Fatalf("first install without a mode: %v", err)
	}
	if err := resolveAddress(&Options{Out: io.Discard, Domain: "x.org", Sslip: true}); err == nil {
		t.Fatal("two modes at once")
	}
	o := &Options{Out: io.Discard, Sslip: true, PublicIP: "158.255.4.142"}
	if err := resolveAddress(o); err != nil || o.Domain != "158-255-4-142.sslip.io" {
		t.Fatal(o.Domain, err)
	}
	if c := caddyData(o); c.AppSite != o.Domain || c.Internal || !strings.HasPrefix(c.AuxSites, "view."+o.Domain) {
		t.Fatalf("sslip installs as a domain: %+v", c)
	}
	o = &Options{Out: io.Discard, Tailscale: true}
	if err := resolveAddress(o); err != nil || o.Host != "localhost" || o.Domain != "" {
		t.Fatalf("tailnet starts on localhost: %+v %v", o, err)
	}
}

func TestTailnetName(t *testing.T) {
	name, err := parseTSName([]byte(`{"Self":{"DNSName":"laavu.tail1234.ts.net."}}`))
	if err != nil || name != "laavu.tail1234.ts.net" {
		t.Fatal(name, err)
	}
	if _, err := parseTSName([]byte(`{"Self":{"DNSName":""}}`)); err == nil || !strings.Contains(err.Error(), "MagicDNS") {
		t.Fatalf("no name: %v", err)
	}
	c := caddyData(&Options{tsName: "laavu.tail1234.ts.net"})
	if c.AppSite != "https://laavu.tail1234.ts.net" || c.Internal || !strings.HasPrefix(c.AuxSites, "https://laavu.tail1234.ts.net:8444") {
		t.Fatalf("repeated install in the tailnet keeps its Caddy config: %+v", c)
	}
}

func TestSshdPasswords(t *testing.T) {
	on := "port 22\npasswordauthentication yes\nkbdinteractiveauthentication no\n"
	off := "port 22\npasswordauthentication no\nkbdinteractiveauthentication no\n"
	kbd := "passwordauthentication no\nkbdinteractiveauthentication yes\n"
	if !sshdTakesPasswords(on) || sshdTakesPasswords(off) || !sshdTakesPasswords(kbd) {
		t.Fatal("sshd -T parsing")
	}
}
