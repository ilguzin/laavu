// laavu — утилита владельца сервера: установка и обслуживание.
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
	"github.com/ilguzin/laavu/internal/priv"
	"github.com/ilguzin/laavu/internal/release"
)

// Version подставляется при сборке.
var Version = "dev"

const usage = `laavu — установка и обслуживание сервера Laavu.

  laavu install [--domain ДОМЕН --email ПОЧТА | --host ИМЯ] [--agent ИМЯ] [--install-claude]
                [--owner-key "ssh-ed25519 …"] [--harden-ssh]
                                    поставить Laavu на этот сервер (от root)
  laavu install --ssh ПОЛЬЗОВАТЕЛЬ@IP [те же флаги]
                                    поставить с ноутбука на удалённый сервер
  laavu agent create ИМЯ [--memory 2G] [--install-claude]
  laavu agent delete ИМЯ [--keep-home]
  laavu agent list
  laavu agent limits ИМЯ [--memory 2G] [--tasks 1024] [--cpu-weight 100]
  laavu check [--json]              состояние сервера
  laavu check-isolation [A B]       проверить изоляцию агентов
  laavu firewall status|allow ПОРТ|deny ПОРТ
  laavu tailscale enable|disable    Tailscale как опция
  laavu update laavud|templates|os|claude
  laavu restart laavud
  laavu cleanup                     убрать старые журналы и временные файлы
  laavu diag                        собрать диагностику
  laavu harden-ssh --owner-key "ssh-ed25519 …"
                                    запретить вход по паролю и под root
  laavu release keygen|manifest|sign|verify
                                    подпись релизов ключом проекта (вне CI)
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
	domain := fs.String("domain", "", "домен сервера; без него — тестовая ВМ на портах 8443/8444/8450-8459")
	host := fs.String("host", "", "имя сервера в режиме ВМ (по умолчанию localhost), например имя хоста в tailnet")
	email := fs.String("email", "", "почта для Let's Encrypt")
	agent := fs.String("agent", "", "имя первого агента")
	claude := fs.Bool("install-claude", false, "поставить Claude Code первому агенту")
	ownerKey := fs.String("owner-key", "", "открытый ключ владельца для ssh")
	harden := fs.Bool("harden-ssh", false, "запретить вход по паролю и под root")
	sshTarget := fs.String("ssh", "", "поставить на удалённый сервер: пользователь@IP[:порт]")
	identity := fs.String("identity", "", "закрытый ключ для ssh (по умолчанию ~/.ssh/id_ed25519, если есть; none — только пароль)")
	skipApt := fs.Bool("skip-apt", false, "не ставить пакеты")
	yes := fs.Bool("yes", false, "не спрашивать подтверждение отпечатка хоста (только для тестов)")
	rel := fs.String("release", "", "каталог или https-адрес подписанного релиза (для --ssh)")
	ownerPub := fs.String("owner-pubkey", "", "файл открытого ключа владельца (для --ssh)")
	ownerUser := fs.String("owner-user", "", "пользователь владельца на сервере")
	releaseKey := fs.String("release-key", "", "открытый ключ проекта для проверки обновлений")
	fs.Parse(args)
	o := install.Options{
		Domain: *domain, Host: *host, Email: *email, Agent: *agent, InstallClaude: *claude,
		OwnerKey: *ownerKey, OwnerUser: *ownerUser, HardenSSH: *harden, SkipApt: *skipApt,
		ReleaseKey: *releaseKey, Out: os.Stdout,
	}
	if *sshTarget != "" {
		// На сервер уходят только флаги самой установки.
		var pass []string
		for _, f := range []struct {
			name, val string
		}{{"domain", *domain}, {"host", *host}, {"email", *email}, {"agent", *agent}} {
			if f.val != "" {
				pass = append(pass, "--"+f.name, f.val)
			}
		}
		if *claude {
			pass = append(pass, "--install-claude")
		}
		return install.RemoteInstall(install.RemoteOptions{
			Target: *sshTarget, Identity: *identity, Yes: *yes, Release: *rel, OwnerPub: *ownerPub,
		}, o, pass)
	}
	return install.Run(o)
}

func agentCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("использование: laavu agent create|delete|list|limits")
	}
	op := map[string]string{"create": "create-agent", "delete": "delete-agent", "list": "list", "limits": "limits"}[args[0]]
	if op == "" {
		return fmt.Errorf("неизвестная команда agent %s", args[0])
	}
	return priv.Main(append([]string{op}, args[1:]...))
}

func restart(args []string) error {
	if len(args) != 1 || args[0] != "laavud" {
		return errors.New("использование: laavu restart laavud")
	}
	out, err := exec.Command("systemctl", "restart", "laavud.service").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func hardenCmd(args []string) error {
	fs := flag.NewFlagSet("harden-ssh", flag.ExitOnError)
	ownerKey := fs.String("owner-key", "", "открытый ключ владельца")
	ownerUser := fs.String("owner-user", "", "пользователь владельца")
	fs.Parse(args)
	return install.HardenSSH(install.Options{OwnerKey: *ownerKey, OwnerUser: *ownerUser})
}

func releaseCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("использование: laavu release keygen --out ФАЙЛ | manifest --dir КАТАЛОГ --version В | sign --dir КАТАЛОГ --key ФАЙЛ | verify --dir КАТАЛОГ [--key BASE64] [--file ИМЯ=ПУТЬ]")
	}
	fs := flag.NewFlagSet("release", flag.ExitOnError)
	out := fs.String("out", "", "файл закрытого ключа")
	dir := fs.String("dir", "", "каталог релиза")
	version := fs.String("version", "", "версия")
	key := fs.String("key", "", "ключ: файл закрытого (sign) или открытый base64 (verify)")
	var files multiFlag
	fs.Var(&files, "file", "ИМЯ=ПУТЬ — сверить файл с манифестом (verify)")
	fs.Parse(args[1:])
	switch args[0] {
	case "keygen":
		if *out == "" {
			return errors.New("нужен --out")
		}
		pub, err := release.GenerateKey(*out)
		if err != nil {
			return err
		}
		fmt.Println("Открытый ключ проекта (вшить при сборке и закреплять при установке):")
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
		fmt.Printf("Релиз %s: подпись и файлы сходятся.\n", v.Manifest.Version)
		return nil
	}
	return fmt.Errorf("неизвестная команда release %s", args[0])
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
