package install

import (
	"strings"
	"testing"
)

func TestFirewallRules(t *testing.T) {
	ports := []string{"80/tcp", "443/tcp", "8450:8459/tcp"}
	join := func(rs [][]string) string {
		var out []string
		for _, r := range rs {
			out = append(out, strings.Join(r, " "))
		}
		return strings.Join(out, "\n")
	}
	extra := join(FirewallRules(TSExtra, 22, ports))
	if !strings.Contains(extra, "delete allow 22/tcp") || !strings.Contains(extra, "allow in on tailscale0 to any port 22 proto tcp") || strings.Contains(extra, "443") {
		t.Fatalf("вдобавок: ssh только из tailnet, 443 открыт:\n%s", extra)
	}
	only := join(FirewallRules(TSOnly, 22, ports))
	for _, want := range []string{"delete allow 443/tcp", "allow in on tailscale0 to any port 443 proto tcp", "allow in on tailscale0 to any port 8450:8459 proto tcp"} {
		if !strings.Contains(only, want) {
			t.Fatalf("только tailnet, нет %q:\n%s", want, only)
		}
	}
	off := join(FirewallRules("", 22, ports))
	if !strings.Contains(off, "allow 22/tcp") || !strings.Contains(off, "allow 443/tcp") {
		t.Fatalf("выключение возвращает правила:\n%s", off)
	}
}

func TestCaddyData(t *testing.T) {
	vm := caddyData(&Options{})
	if vm.AppSite != "https://localhost:8443" || !vm.Internal || !strings.Contains(vm.AuxSites, "https://localhost:8459") {
		t.Fatalf("ВМ: %+v", vm)
	}
	d := caddyData(&Options{Domain: "laavu.example.org"})
	if d.AppSite != "laavu.example.org" || d.Internal || !strings.HasPrefix(d.AuxSites, "view.laavu.example.org") {
		t.Fatalf("домен: %+v", d)
	}
}

func TestHostMode(t *testing.T) {
	o := &Options{Host: "host.tail0000.ts.net"}
	d := daemonConfig(o)
	if d.PublicURL != "https://host.tail0000.ts.net:8443" || d.ViewURL != "https://host.tail0000.ts.net:8444" || !portsMode(d) {
		t.Fatalf("%+v", d)
	}
	c := caddyData(o)
	if c.AppSite != "https://host.tail0000.ts.net:8443" || !c.Internal || !strings.Contains(c.AuxSites, "https://host.tail0000.ts.net:8459") {
		t.Fatalf("%+v", c)
	}
	if portsMode(daemonConfig(&Options{Domain: "x.org"})) {
		t.Fatal("домен — не режим портов")
	}
}
