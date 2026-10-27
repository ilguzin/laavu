// laavu is the server owner's utility: installation and maintenance.
package main

import (
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ilguzin/laavu/internal/install"
	"github.com/ilguzin/laavu/internal/linkcmd"
	"github.com/ilguzin/laavu/internal/priv"
	"github.com/ilguzin/laavu/internal/release"
)

// Version is set at build time.
var Version = "dev"

const usage = `laavu installs and maintains a Laavu server.

  laavu install --domain DOMAIN [--email EMAIL] | --sslip [--public-ip IP]
                | --tailscale --tailscale-authkey KEY --tailscale-hostname NAME | --host NAME
                [--agent NAME] [--install-claude] [--owner-key "ssh-ed25519 ..."] [--owner-user USER]
                [--harden-ssh] [--skip-apt] [--release-key KEY]
                                    install Laavu on this server (as root)
  laavu install --ssh USER@IP[:PORT] [--release DIR|URL] [--identity KEY|none] [--owner-pubkey FILE]
                [address and agent flags as above]
                                    install from a laptop onto a remote server
  laavu agent create NAME [--memory 2G] [--tasks 1024] [--cpu-weight 100] [--install-claude]
  laavu agent delete NAME [--keep-home]
  laavu agent list
  laavu agent limits NAME [--memory 2G] [--tasks 1024] [--cpu-weight 100]
  laavu check [--json]              server status
  laavu check-isolation [A B] [--json]
                                    check agent isolation
  laavu firewall status|allow PORT|deny PORT
  laavu tailscale enable [--mode extra|only] [--authkey KEY] [--hostname NAME] | disable | status
                                    optional Tailscale
  laavu update laavud|templates|os|claude
  laavu restart laavud
  laavu cleanup                     remove old logs and temporary files
  laavu diag                        collect diagnostics
  laavu link [--code CODE] [--console wss://...] [--console-key KEY]
                                    connect this server to a management console
  laavu link status|rights [RIGHT=on|off ...]
  laavu unlink                      take control back from the console
  laavu harden-ssh --owner-key "ssh-ed25519 ..." [--owner-user USER]
                                    disable password and root login
  laavu release keygen|manifest|sign|verify
                                    sign releases with the project key (outside CI)
  laavu version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "install":
		err = installCmd(args)
	case "agent":
		err = agentCmd(args)
	case "harden-ssh":
		err = hardenCmd(args)
	case "release":
		err = releaseCmd(args)
	case "check-isolation":
		err = priv.Main(append([]string{"check-isolation"}, args...))
	case "check":
		err = install.Check(args, os.Stdout)
	case "firewall":
		err = install.Firewall(args, os.Stdout)
	case "tailscale":
		err = install.Tailscale(args, os.Stdout)
	case "update":
		err = install.Update(args, os.Stdout)
	case "link":
		err = linkcmd.Main(args, Version, os.Stdout)
	case "unlink":
		err = linkcmd.Unlink(os.Stdout)
	case "restart":
		err = restart(args)
	case "cleanup":
		err = install.Cleanup(os.Stdout)
	case "diag":
		err = install.Diag(os.Stdout)
	case "version":
		fmt.Println(Version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "laavu:", err)
		os.Exit(1)
	}
}

func installCmd(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	domain := fs.String("domain", "", "server domain (DNS: the domain and view.DOMAIN point at the server)")
	sslip := fs.Bool("sslip", false, "no domain: use A-B-C-D.sslip.io after the server's public IPv4")
	publicIP := fs.String("public-ip", "", "the server's public IPv4 for --sslip, when it is behind NAT")
	tailscale := fs.Bool("tailscale", false, "no domain: reachable only from your tailnet, at https://NAME.<tailnet>.ts.net")
	tsKey := fs.String("tailscale-authkey", "", "Tailscale auth key for --tailscale; without it, a login link is shown")
	tsHost := fs.String("tailscale-hostname", "", "the server's name in the tailnet for --tailscale")
	host := fs.String("host", "", "test VM: server name (e.g. localhost), ports 8443/8444/8450-8459, Caddy's own CA")
	email := fs.String("email", "", "email for Let's Encrypt")
	agent := fs.String("agent", "", "name of the first agent")
	claude := fs.Bool("install-claude", false, "install Claude Code for the first agent")
	ownerKey := fs.String("owner-key", "", "owner's ssh public key")
	harden := fs.Bool("harden-ssh", false, "disable password and root login")
	sshTarget := fs.String("ssh", "", "install on a remote server: user@IP[:port]")
	identity := fs.String("identity", "", "ssh private key (default ~/.ssh/id_ed25519 if present; none means password only)")
	skipApt := fs.Bool("skip-apt", false, "do not install packages")
	yes := fs.Bool("yes", false, "do not ask to confirm the host fingerprint (tests only)")
	rel := fs.String("release", "", "directory or https URL of a signed release (for --ssh)")
	ownerPub := fs.String("owner-pubkey", "", "owner's public key file (for --ssh)")
	ownerUser := fs.String("owner-user", "", "owner's user on the server")
	releaseKey := fs.String("release-key", "", "project public key for verifying updates")
	noLink := fs.Bool("no-link", false, "do not print the registration link (used by --ssh)")
	fs.Parse(args)
	o := install.Options{
		Domain: *domain, Host: *host, Email: *email, Agent: *agent, InstallClaude: *claude,
		Sslip: *sslip, PublicIP: *publicIP, Tailscale: *tailscale, TailscaleAuthKey: *tsKey, TailscaleHostname: *tsHost, NoLink: *noLink,
		OwnerKey: *ownerKey, OwnerUser: *ownerUser, HardenSSH: *harden, SkipApt: *skipApt,
		ReleaseKey: *releaseKey, Out: os.Stdout,
	}
	if *sshTarget != "" {
		// Only the install flags themselves are sent to the server. The tailnet switch
		// comes after ssh is hardened (RemoteInstall), so the server installs on localhost first.
		var pass []string
		h := *host
		if *tailscale {
			h = "localhost"
			pass = append(pass, "--no-link")
		}
		for _, f := range []struct {
			name, val string
		}{{"domain", *domain}, {"host", h}, {"email", *email}, {"agent", *agent}, {"public-ip", *publicIP}} {
			if f.val != "" {
				pass = append(pass, "--"+f.name, f.val)
			}
		}
		for _, f := range []struct {
			name string
			on   bool
		}{{"install-claude", *claude}, {"sslip", *sslip}} {
			if f.on {
				pass = append(pass, "--"+f.name)
			}
		}
		return install.RemoteInstall(install.RemoteOptions{
			Target: *sshTarget, Identity: *identity, Yes: *yes, Release: *rel, OwnerPub: *ownerPub,
			Sslip: *sslip && *publicIP == "", Tailscale: *tailscale, TailscaleAuthKey: *tsKey, TailscaleHostname: *tsHost,
		}, o, pass)
	}
	return install.Run(o)
}

func agentCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: laavu agent create|delete|list|limits")
	}
	op := map[string]string{"create": "create-agent", "delete": "delete-agent", "list": "list", "limits": "limits"}[args[0]]
	if op == "" {
		return fmt.Errorf("unknown agent command %s", args[0])
	}
	return priv.Main(append([]string{op}, args[1:]...))
}

func restart(args []string) error {
	if len(args) != 1 || args[0] != "laavud" {
		return errors.New("usage: laavu restart laavud")
	}
	out, err := exec.Command("systemctl", "restart", "laavud.service").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func hardenCmd(args []string) error {
	fs := flag.NewFlagSet("harden-ssh", flag.ExitOnError)
	ownerKey := fs.String("owner-key", "", "owner's public key")
	ownerUser := fs.String("owner-user", "", "owner's user")
	fs.Parse(args)
	return install.HardenSSH(install.Options{OwnerKey: *ownerKey, OwnerUser: *ownerUser})
}

func releaseCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: laavu release keygen --out FILE | manifest --dir DIR --version V | sign --dir DIR --key FILE | verify --dir DIR [--key BASE64] [--file NAME=PATH]")
	}
	fs := flag.NewFlagSet("release", flag.ExitOnError)
	out := fs.String("out", "", "private key file")
	dir := fs.String("dir", "", "release directory")
	version := fs.String("version", "", "version")
	key := fs.String("key", "", "key: private key file (sign) or base64 public key (verify)")
	var files multiFlag
	fs.Var(&files, "file", "NAME=PATH: check a file against the manifest (verify)")
	fs.Parse(args[1:])
	switch args[0] {
	case "keygen":
		if *out == "" {
			return errors.New("--out required")
		}
		pub, err := release.GenerateKey(*out)
		if err != nil {
			return err
		}
		fmt.Println("Project public key (embed at build time and pin at install time):")
		fmt.Println(pub)
		return nil
	case "manifest":
		m, err := release.Build(*dir, *version)
		if err != nil {
			return err
		}
		return release.WriteManifest(*dir, m)
	case "sign":
		return release.Sign(*dir, *key)
	case "verify":
		var pub ed25519.PublicKey
		var err error
		if *key != "" {
			pub, err = release.ParsePublic(*key)
		} else {
			pub, err = release.PinnedKey()
		}
		if err != nil {
			return err
		}
		v, err := release.Verify(release.Source(*dir), pub)
		if err != nil {
			return err
		}
		for _, f := range files {
			name, path, _ := strings.Cut(f, "=")
			if err := v.VerifyFile(name, path); err != nil {
				return err
			}
		}
		fmt.Printf("Release %s: signature and files match.\n", v.Manifest.Version)
		return nil
	}
	return fmt.Errorf("unknown release command %s", args[0])
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
